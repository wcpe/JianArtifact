//! 通用制品上传 API 的 HTTP 集成测试（FR-73）。
//!
//! 覆盖：Web 控制台统一上传端点 `POST /api/v1/repositories/{id}/upload`（multipart/form-data）——
//! Maven（表单 GAV）/ npm（表单 name+version，不解包）/ Raw（表单 path）三格式上传后经既有下载端点取回字节一致；
//! 写授权边界（无写 403 / 私有无权 404）、proxy 仓库拒绝（400）、不支持格式拒绝（400）、上传上限 413、
//! 覆盖语义（Maven release 重复 409）。
//!
//! 鉴权与制品机理复用既有层，本文件只验上传端点的协议适配与坐标定位正确性。

use std::sync::Arc;

use axum::body::Body;
use axum::http::{Request, StatusCode};
use axum::Router;
use http_body_util::BodyExt;
use serde_json::{json, Value};
use tower::ServiceExt;

use jianartifact::api::{build_router, AppState};
use jianartifact::auth::{self, JwtSigner, LoginGuard};
use jianartifact::config::Config;
use jianartifact::format::{ArtifactService, FormatRegistry};
use jianartifact::meta::{MetaStore, NewRepository, Permission, RepoType, Role, Visibility};
use jianartifact::proxy::HttpUpstream;
use jianartifact::storage::{BlobBackend, LocalFsStore};

/// 测试夹具：真实 SQLite 文件 + 临时 blob 目录 + 固定对外地址。
struct Fixture {
    state: AppState,
    _dir: tempfile::TempDir,
}

impl Fixture {
    async fn new() -> Self {
        Self::with_opt_max_size(None).await
    }

    /// 用给定上传上限重建夹具（验 413）。
    async fn with_max_size(max: u64) -> Self {
        Self::with_opt_max_size(Some(max)).await
    }

    async fn with_opt_max_size(max: Option<u64>) -> Self {
        let dir = tempfile::tempdir().unwrap();
        let meta = MetaStore::open(&dir.path().join("test.db")).await.unwrap();
        let store = BlobBackend::Fs(LocalFsStore::new(dir.path().join("blobs")).await.unwrap());
        let jwt = JwtSigner::from_secret(b"upload-secret-32-bytes-xxxxxxxxx", 3600);
        let upstream = HttpUpstream::new(std::time::Duration::from_secs(60)).unwrap();
        let artifacts = Arc::new(ArtifactService::new(store.clone(), meta.clone(), upstream));
        let mut config = Config::default();
        config.server.public_base_url = Some("http://localhost:8080".to_string());
        config.limits.max_artifact_size = max;
        let (audit, audit_rx) = jianartifact::api::audit_channel();
        jianartifact::api::spawn_audit_writer(meta.clone(), audit_rx);
        let (usage, usage_rx) = jianartifact::api::usage_channel();
        jianartifact::api::spawn_usage_writer(meta.clone(), usage_rx, false);
        let state = AppState {
            config: Arc::new(config),
            meta,
            store,
            jwt,
            login_guard: Arc::new(LoginGuard::new(50, 900)),
            artifacts,
            formats: Arc::new(FormatRegistry::with_builtin()),
            docker: None,
            audit,
            usage,
            metrics: None,
            rate_limiter: Arc::new(jianartifact::api::RateLimiter::new()),
            oidc: None,
            oidc_flows: std::sync::Arc::new(jianartifact::api::OidcFlowStore::new()),
            ldap: None,
            // 防护配置真源转为热替换槽（ADR-0018）：原 ip_matcher / waf_rules 并入 ProtectionState
            protection: std::sync::Arc::new(jianartifact::api::ProtectionState::new(
                jianartifact::config::ProtectionConfig::default(),
            )),
            ban_registry: std::sync::Arc::new(jianartifact::api::BanRegistry::new()),
            cc_challenger: std::sync::Arc::new(jianartifact::api::CcChallenger::new(
                b"test-secret-32-bytes-xxxxxxxxxxxx",
            )),
            alerts: jianartifact::api::alert_channel().0,
            alert_engine: std::sync::Arc::new(jianartifact::api::AlertEngine::new(
                jianartifact::api::alert_channel().0,
            )),
        };
        Self { state, _dir: dir }
    }

    fn router(&self) -> Router {
        build_router(self.state.clone())
    }

    async fn seed_user(&self, username: &str, password: &str, role: Role) -> String {
        let hash = auth::hash_password(password).unwrap();
        self.state
            .meta
            .create_user(username, &hash, role)
            .await
            .unwrap()
    }

    /// 建一个 hosted 仓库，返回 id。
    async fn seed_repo(&self, name: &str, format: &'static str, visibility: Visibility) -> String {
        self.state
            .meta
            .create_repository(NewRepository {
                name,
                format,
                r#type: RepoType::Hosted,
                visibility,
                upstream_url: None,
                upstream_auth_ref: None,
            })
            .await
            .unwrap()
    }

    /// 建一个 proxy 仓库，返回 id。
    async fn seed_proxy_repo(
        &self,
        name: &str,
        format: &'static str,
        upstream_url: &str,
    ) -> String {
        self.state
            .meta
            .create_repository(NewRepository {
                name,
                format,
                r#type: RepoType::Proxy,
                visibility: Visibility::Public,
                upstream_url: Some(upstream_url),
                upstream_auth_ref: None,
            })
            .await
            .unwrap()
    }

    async fn seed_acl(&self, repo_id: &str, user_id: &str, permission: Permission) {
        self.state
            .meta
            .create_acl(repo_id, user_id, permission)
            .await
            .unwrap();
    }

    async fn login_token(&self, username: &str, password: &str) -> String {
        let resp = self
            .router()
            .oneshot(json_req(
                "POST",
                "/api/v1/auth/login",
                None,
                json!({ "username": username, "password": password }),
            ))
            .await
            .unwrap();
        assert_eq!(resp.status(), StatusCode::OK, "登录应成功");
        let bytes = resp.into_body().collect().await.unwrap().to_bytes();
        let body: Value = serde_json::from_slice(&bytes).unwrap();
        body["access_token"].as_str().unwrap().to_string()
    }
}

fn json_req(method: &str, uri: &str, auth: Option<&str>, body: Value) -> Request<Body> {
    let mut builder = Request::builder()
        .method(method)
        .uri(uri)
        .header("content-type", "application/json");
    if let Some(a) = auth {
        builder = builder.header("authorization", a);
    }
    builder.body(Body::from(body.to_string())).unwrap()
}

fn empty_req(method: &str, uri: &str, auth: Option<&str>) -> Request<Body> {
    let mut builder = Request::builder().method(method).uri(uri);
    if let Some(a) = auth {
        builder = builder.header("authorization", a);
    }
    builder.body(Body::empty()).unwrap()
}

/// multipart 上传体的一个字段（文本或文件）。
struct Part {
    name: String,
    filename: Option<String>,
    value: Vec<u8>,
}

fn text_part(name: &str, value: &str) -> Part {
    Part {
        name: name.to_string(),
        filename: None,
        value: value.as_bytes().to_vec(),
    }
}

fn file_part(name: &str, filename: &str, value: &[u8]) -> Part {
    Part {
        name: name.to_string(),
        filename: Some(filename.to_string()),
        value: value.to_vec(),
    }
}

/// 构造一份 multipart/form-data 上传请求，POST 到上传端点。
fn upload_req(repo_id: &str, auth: Option<&str>, parts: &[Part]) -> Request<Body> {
    let boundary = "----JianArtifactUploadBoundary";
    let mut body: Vec<u8> = Vec::new();
    for p in parts {
        body.extend_from_slice(format!("--{boundary}\r\n").as_bytes());
        match &p.filename {
            Some(fname) => {
                body.extend_from_slice(
                    format!(
                        "Content-Disposition: form-data; name=\"{}\"; filename=\"{}\"\r\n",
                        p.name, fname
                    )
                    .as_bytes(),
                );
                body.extend_from_slice(b"Content-Type: application/octet-stream\r\n\r\n");
            }
            None => {
                body.extend_from_slice(
                    format!(
                        "Content-Disposition: form-data; name=\"{}\"\r\n\r\n",
                        p.name
                    )
                    .as_bytes(),
                );
            }
        }
        body.extend_from_slice(&p.value);
        body.extend_from_slice(b"\r\n");
    }
    body.extend_from_slice(format!("--{boundary}--\r\n").as_bytes());

    let mut builder = Request::builder()
        .method("POST")
        .uri(format!("/api/v1/repositories/{repo_id}/upload"))
        .header(
            "content-type",
            format!("multipart/form-data; boundary={boundary}"),
        );
    if let Some(a) = auth {
        builder = builder.header("authorization", a);
    }
    builder.body(Body::from(body)).unwrap()
}

/// 发请求并返回 (状态码, JSON 体)。
async fn send_json(router: Router, req: Request<Body>) -> (StatusCode, Value) {
    let resp = router.oneshot(req).await.unwrap();
    let status = resp.status();
    let bytes = resp.into_body().collect().await.unwrap().to_bytes();
    let body = serde_json::from_slice(&bytes).unwrap_or(Value::Null);
    (status, body)
}

/// 发请求并返回 (状态码, 原始字节)。
async fn send_bytes(router: Router, req: Request<Body>) -> (StatusCode, Vec<u8>) {
    let resp = router.oneshot(req).await.unwrap();
    let status = resp.status();
    let bytes = resp
        .into_body()
        .collect()
        .await
        .unwrap()
        .to_bytes()
        .to_vec();
    (status, bytes)
}

// ---------- Maven：表单 GAV 上传 → 经下载端点取回字节一致 ----------

#[tokio::test]
async fn maven_表单gav上传后可下载且字节一致() {
    let fx = Fixture::new().await;
    let writer = fx.seed_user("dev", "pw", Role::User).await;
    let rid = fx
        .seed_repo("maven-hosted", "maven", Visibility::Public)
        .await;
    fx.seed_acl(&rid, &writer, Permission::Write).await;
    let auth = format!("Bearer {}", fx.login_token("dev", "pw").await);

    let jar = b"fake-jar-bytes-content";
    let parts = vec![
        text_part("group_id", "com.example.app"),
        text_part("artifact_id", "demo"),
        text_part("version", "1.0.0"),
        file_part("file", "demo-1.0.0.jar", jar),
    ];
    let (status, _) = send_json(fx.router(), upload_req(&rid, Some(&auth), &parts)).await;
    assert_eq!(status, StatusCode::CREATED, "Maven 上传应 201");

    // 经既有下载端点取回：路径按 Maven 布局
    let (status, bytes) = send_bytes(
        fx.router(),
        empty_req(
            "GET",
            "/maven-hosted/com/example/app/demo/1.0.0/demo-1.0.0.jar",
            None,
        ),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(bytes, jar);
}

// ---------- npm：表单 name+version 上传（不解包）→ 取回字节一致 ----------

#[tokio::test]
async fn npm_表单上传后tarball可下载且字节一致() {
    let fx = Fixture::new().await;
    let writer = fx.seed_user("dev", "pw", Role::User).await;
    let rid = fx.seed_repo("npm-hosted", "npm", Visibility::Public).await;
    fx.seed_acl(&rid, &writer, Permission::Write).await;
    let auth = format!("Bearer {}", fx.login_token("dev", "pw").await);

    let tgz = b"fake-npm-tarball-gzip-bytes";
    let parts = vec![
        text_part("name", "lodash"),
        text_part("version", "4.17.21"),
        file_part("file", "lodash-4.17.21.tgz", tgz),
    ];
    let (status, _) = send_json(fx.router(), upload_req(&rid, Some(&auth), &parts)).await;
    assert_eq!(status, StatusCode::CREATED, "npm 上传应 201");

    // tarball 存于 {name}/-/{filename}，经下载端点取回
    let (status, bytes) = send_bytes(
        fx.router(),
        empty_req("GET", "/npm-hosted/lodash/-/lodash-4.17.21.tgz", None),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(bytes, tgz);
}

// ---------- Raw：表单 path 上传 → 取回字节一致 ----------

#[tokio::test]
async fn raw_表单path上传后可下载且字节一致() {
    let fx = Fixture::new().await;
    let writer = fx.seed_user("dev", "pw", Role::User).await;
    let rid = fx.seed_repo("raw-hosted", "raw", Visibility::Public).await;
    fx.seed_acl(&rid, &writer, Permission::Write).await;
    let auth = format!("Bearer {}", fx.login_token("dev", "pw").await);

    let blob = b"arbitrary-raw-file-bytes";
    let parts = vec![
        text_part("path", "dir/sub/file.bin"),
        file_part("file", "file.bin", blob),
    ];
    let (status, _) = send_json(fx.router(), upload_req(&rid, Some(&auth), &parts)).await;
    assert_eq!(status, StatusCode::CREATED, "Raw 上传应 201");

    let (status, bytes) = send_bytes(
        fx.router(),
        empty_req("GET", "/raw-hosted/dir/sub/file.bin", None),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(bytes, blob);
}

// ---------- 写授权边界：无写权限上传 403 ----------

#[tokio::test]
async fn 无写权限上传被拒_403() {
    let fx = Fixture::new().await;
    // public 仓库匿名可读但无写 → 上传 403
    let rid = fx.seed_repo("raw-pub", "raw", Visibility::Public).await;
    let parts = vec![text_part("path", "x.bin"), file_part("file", "x.bin", b"x")];
    let (status, _) = send_json(fx.router(), upload_req(&rid, None, &parts)).await;
    assert_eq!(status, StatusCode::FORBIDDEN);
}

// ---------- 私有仓库对无权者上传 404 隐藏存在性 ----------

#[tokio::test]
async fn 私有仓库无权上传_404_隐藏存在性() {
    let fx = Fixture::new().await;
    let rid = fx.seed_repo("raw-secret", "raw", Visibility::Private).await;
    let parts = vec![text_part("path", "x.bin"), file_part("file", "x.bin", b"x")];
    // 匿名对 private 仓库上传 → 404（不泄露存在）
    let (status, _) = send_json(fx.router(), upload_req(&rid, None, &parts)).await;
    assert_eq!(status, StatusCode::NOT_FOUND);
}

// ---------- proxy 仓库拒绝上传：400 ----------

#[tokio::test]
async fn 向proxy仓库上传被拒_400() {
    let fx = Fixture::new().await;
    let writer = fx.seed_user("dev", "pw", Role::User).await;
    let rid = fx
        .seed_proxy_repo("raw-proxy", "raw", "http://upstream.example")
        .await;
    fx.seed_acl(&rid, &writer, Permission::Write).await;
    let auth = format!("Bearer {}", fx.login_token("dev", "pw").await);

    let parts = vec![text_part("path", "x.bin"), file_part("file", "x.bin", b"x")];
    let (status, body) = send_json(fx.router(), upload_req(&rid, Some(&auth), &parts)).await;
    assert_eq!(status, StatusCode::BAD_REQUEST, "proxy 仓库不允许直传");
    assert_eq!(body["error"]["code"], "bad_request");
}

// ---------- 不支持的格式拒绝：400 ----------

#[tokio::test]
async fn 不支持的格式上传被拒_400() {
    let fx = Fixture::new().await;
    let writer = fx.seed_user("dev", "pw", Role::User).await;
    // docker 不在通用上传支持范围（仅 maven/npm/raw）
    let rid = fx
        .seed_repo("docker-hosted", "docker", Visibility::Public)
        .await;
    fx.seed_acl(&rid, &writer, Permission::Write).await;
    let auth = format!("Bearer {}", fx.login_token("dev", "pw").await);

    let parts = vec![text_part("path", "x.bin"), file_part("file", "x.bin", b"x")];
    let (status, body) = send_json(fx.router(), upload_req(&rid, Some(&auth), &parts)).await;
    assert_eq!(status, StatusCode::BAD_REQUEST, "不支持的格式应 400");
    assert_eq!(body["error"]["code"], "bad_request");
}

// ---------- 上传上限 413 ----------

#[tokio::test]
async fn 上传超限返回_413() {
    let fx = Fixture::with_max_size(8).await;
    let writer = fx.seed_user("dev", "pw", Role::User).await;
    let rid = fx.seed_repo("raw-hosted", "raw", Visibility::Public).await;
    fx.seed_acl(&rid, &writer, Permission::Write).await;
    let auth = format!("Bearer {}", fx.login_token("dev", "pw").await);

    let parts = vec![
        text_part("path", "big.bin"),
        file_part("file", "big.bin", b"0123456789-too-large"),
    ];
    let (status, body) = send_json(fx.router(), upload_req(&rid, Some(&auth), &parts)).await;
    assert_eq!(status, StatusCode::PAYLOAD_TOO_LARGE);
    assert_eq!(body["error"]["code"], "payload_too_large");
}

// ---------- Maven release 重复上传 409（覆盖语义） ----------

#[tokio::test]
async fn maven_release重复上传返回_409() {
    let fx = Fixture::new().await;
    let writer = fx.seed_user("dev", "pw", Role::User).await;
    let rid = fx
        .seed_repo("maven-hosted", "maven", Visibility::Public)
        .await;
    fx.seed_acl(&rid, &writer, Permission::Write).await;
    let auth = format!("Bearer {}", fx.login_token("dev", "pw").await);

    let jar = b"release-jar";
    let make_parts = || {
        vec![
            text_part("group_id", "com.example"),
            text_part("artifact_id", "lib"),
            text_part("version", "2.0.0"),
            file_part("file", "lib-2.0.0.jar", jar),
        ]
    };
    let (s1, _) = send_json(fx.router(), upload_req(&rid, Some(&auth), &make_parts())).await;
    assert_eq!(s1, StatusCode::CREATED);

    let (s2, body) = send_json(fx.router(), upload_req(&rid, Some(&auth), &make_parts())).await;
    assert_eq!(s2, StatusCode::CONFLICT, "release 正式构件不可覆盖");
    assert_eq!(body["error"]["code"], "conflict");
}

// ---------- 缺文件字段 400 ----------

#[tokio::test]
async fn 缺文件字段上传被拒_400() {
    let fx = Fixture::new().await;
    let writer = fx.seed_user("dev", "pw", Role::User).await;
    let rid = fx.seed_repo("raw-hosted", "raw", Visibility::Public).await;
    fx.seed_acl(&rid, &writer, Permission::Write).await;
    let auth = format!("Bearer {}", fx.login_token("dev", "pw").await);

    // 只有 path 文本字段，无 file 文件字段
    let parts = vec![text_part("path", "x.bin")];
    let (status, body) = send_json(fx.router(), upload_req(&rid, Some(&auth), &parts)).await;
    assert_eq!(status, StatusCode::BAD_REQUEST);
    assert_eq!(body["error"]["code"], "bad_request");
}
