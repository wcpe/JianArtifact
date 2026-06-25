//! npm registry 格式的 HTTP 集成测试（FR-15）。
//!
//! 覆盖：发布 → packument 获取 → tarball 下载端到端（hosted）、scoped 包 URL 编码、
//! tarball 存储路径与 dist 重写（integrity/shasum）、版本不可变（重复 publish 同版本 409）、
//! 写授权边界，以及 proxy cache-miss 回源（真实 mock 上游走 HttpUpstream）。
//!
//! 鉴权与制品机理复用既有层，本文件只验 npm 协议适配的正确性。

use std::sync::Arc;

use axum::body::Body;
use axum::http::{Request, StatusCode};
use axum::Router;
use base64::Engine;
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
        let dir = tempfile::tempdir().unwrap();
        let meta = MetaStore::open(&dir.path().join("test.db")).await.unwrap();
        let store = BlobBackend::Fs(LocalFsStore::new(dir.path().join("blobs")).await.unwrap());
        let jwt = JwtSigner::from_secret(b"npm-secret-32-bytes-xxxxxxxxxxxxx", 3600);
        let upstream = HttpUpstream::new(std::time::Duration::from_secs(60)).unwrap();
        let artifacts = Arc::new(ArtifactService::new(store.clone(), meta.clone(), upstream));
        let mut config = Config::default();
        // 固定对外地址，便于断言 dist.tarball / 使用片段
        config.server.public_base_url = Some("http://localhost:8080".to_string());
        let (audit, audit_rx) = jianartifact::api::audit_channel();
        jianartifact::api::spawn_audit_writer(meta.clone(), audit_rx);
        // 使用分析采集：建有界 channel 并启动写入任务（关明细），使路由真实走采集链路
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
            // FR-53：测试默认名单空、封禁登记表空（异常检测默认关闭）
            protection: std::sync::Arc::new(jianartifact::api::ProtectionState::new(
                jianartifact::config::ProtectionConfig::default(),
            )),
            ban_registry: std::sync::Arc::new(jianartifact::api::BanRegistry::new()),
            // FR-54：测试默认 CC 挑战关闭；挑战器用固定密钥
            cc_challenger: std::sync::Arc::new(jianartifact::api::CcChallenger::new(
                b"test-secret-32-bytes-xxxxxxxxxxxx",
            )),
            // FR-56：防护告警默认关闭，引擎与投递端就绪（关闭时 record 直接返回）
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

    /// 建一个 npm hosted 仓库，返回 id。
    async fn seed_npm_repo(&self, name: &str, visibility: Visibility) -> String {
        self.state
            .meta
            .create_repository(NewRepository {
                name,
                format: "npm",
                r#type: RepoType::Hosted,
                visibility,
                upstream_url: None,
                upstream_auth_ref: None,
            })
            .await
            .unwrap()
    }

    /// 建一个 npm proxy 仓库（指向给定上游基址），返回 id。
    async fn seed_npm_proxy_repo(
        &self,
        name: &str,
        visibility: Visibility,
        upstream_url: &str,
    ) -> String {
        self.state
            .meta
            .create_repository(NewRepository {
                name,
                format: "npm",
                r#type: RepoType::Proxy,
                visibility,
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
        let (status, body) = send(
            self.router(),
            json_req(
                "POST",
                "/api/v1/auth/login",
                None,
                json!({ "username": username, "password": password }),
            ),
        )
        .await;
        assert_eq!(status, StatusCode::OK, "登录应成功: {body}");
        body["access_token"].as_str().unwrap().to_string()
    }
}

/// 发请求并返回 (状态码, JSON 体)。
async fn send(router: Router, req: Request<Body>) -> (StatusCode, Value) {
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

/// 构造一份 npm publish 请求体（仿 `npm publish` 的 JSON：name/versions/dist-tags/_attachments）。
fn publish_body(package: &str, version: &str, tarball_name: &str, tarball: &[u8]) -> Value {
    let data = base64::engine::general_purpose::STANDARD.encode(tarball);
    json!({
        "_id": package,
        "name": package,
        "versions": {
            version: {
                "name": package,
                "version": version,
                "dist": {
                    // 客户端会带占位 tarball，服务端应据本仓库重写
                    "tarball": format!("http://placeholder/{tarball_name}")
                }
            }
        },
        "dist-tags": { "latest": version },
        "_attachments": {
            tarball_name: {
                "content_type": "application/octet-stream",
                "data": data,
                "length": tarball.len()
            }
        }
    })
}

/// 计算字节的 sha1 十六进制（与服务端 dist.shasum 对账）。
fn sha1_hex(bytes: &[u8]) -> String {
    use sha1::{Digest, Sha1};
    let mut h = Sha1::new();
    h.update(bytes);
    format!("{:x}", h.finalize())
}

/// 计算字节的 npm integrity（`sha512-<base64(原始 sha512 字节)>`），与服务端对账。
fn integrity(bytes: &[u8]) -> String {
    use sha2::{Digest, Sha512};
    let mut h = Sha512::new();
    h.update(bytes);
    let digest = h.finalize();
    format!(
        "sha512-{}",
        base64::engine::general_purpose::STANDARD.encode(digest)
    )
}

// ---------- FR-15 hosted：发布 → packument → tarball 端到端 ----------

#[tokio::test]
async fn npm_发布后取_packument_与_tarball_端到端() {
    let fx = Fixture::new().await;
    let writer = fx.seed_user("dev", "pw", Role::User).await;
    let rid = fx.seed_npm_repo("npm-hosted", Visibility::Public).await;
    fx.seed_acl(&rid, &writer, Permission::Write).await;
    let auth = format!("Bearer {}", fx.login_token("dev", "pw").await);

    let tarball = b"this-is-a-fake-tgz-payload";
    // PUT /{repo}/{package} 发布
    let (status, _) = send(
        fx.router(),
        json_req(
            "PUT",
            "/npm-hosted/lodash",
            Some(&auth),
            publish_body("lodash", "4.17.21", "lodash-4.17.21.tgz", tarball),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::CREATED, "发布应 201");

    // GET packument（公开仓库匿名可读）
    let (status, packument) = send(fx.router(), empty_req("GET", "/npm-hosted/lodash", None)).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(packument["name"], "lodash");
    assert_eq!(packument["dist-tags"]["latest"], "4.17.21");
    let dist = &packument["versions"]["4.17.21"]["dist"];
    // dist.tarball 指向本仓库（覆盖客户端占位）
    assert_eq!(
        dist["tarball"],
        "http://localhost:8080/npm-hosted/lodash/-/lodash-4.17.21.tgz"
    );
    // FR-69：integrity / shasum 用服务端算好的真实摘要
    assert_eq!(dist["shasum"], sha1_hex(tarball));
    assert_eq!(dist["integrity"], integrity(tarball));

    // GET tarball：从 dist.tarball 指向的路径下载，字节一致
    let (status, bytes) = send_bytes(
        fx.router(),
        empty_req("GET", "/npm-hosted/lodash/-/lodash-4.17.21.tgz", None),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(bytes, tarball);
}

#[tokio::test]
async fn npm_发布两个版本_packument_合并且_latest_更新() {
    let fx = Fixture::new().await;
    let writer = fx.seed_user("dev", "pw", Role::User).await;
    let rid = fx.seed_npm_repo("npm-hosted", Visibility::Public).await;
    fx.seed_acl(&rid, &writer, Permission::Write).await;
    let auth = format!("Bearer {}", fx.login_token("dev", "pw").await);

    for (ver, name) in [("1.0.0", "pkg-1.0.0.tgz"), ("1.1.0", "pkg-1.1.0.tgz")] {
        let (status, _) = send(
            fx.router(),
            json_req(
                "PUT",
                "/npm-hosted/pkg",
                Some(&auth),
                publish_body("pkg", ver, name, ver.as_bytes()),
            ),
        )
        .await;
        assert_eq!(status, StatusCode::CREATED, "版本 {ver} 发布应 201");
    }

    let (status, packument) = send(fx.router(), empty_req("GET", "/npm-hosted/pkg", None)).await;
    assert_eq!(status, StatusCode::OK);
    // 两个版本都在 packument 中
    assert!(packument["versions"]["1.0.0"].is_object());
    assert!(packument["versions"]["1.1.0"].is_object());
    // latest 指向最后发布的版本
    assert_eq!(packument["dist-tags"]["latest"], "1.1.0");
}

// ---------- scoped 包：URL 编码（@scope%2Fname）端到端 ----------

#[tokio::test]
async fn npm_scoped_包发布与安装走_url_编码() {
    let fx = Fixture::new().await;
    let writer = fx.seed_user("dev", "pw", Role::User).await;
    let rid = fx.seed_npm_repo("npm-hosted", Visibility::Public).await;
    fx.seed_acl(&rid, &writer, Permission::Write).await;
    let auth = format!("Bearer {}", fx.login_token("dev", "pw").await);

    let tarball = b"scoped-tgz";
    // npm 对 scoped 包用 %2F 编码斜杠：PUT /{repo}/@scope%2Fpkg
    let (status, _) = send(
        fx.router(),
        json_req(
            "PUT",
            "/npm-hosted/@acme%2Fwidget",
            Some(&auth),
            publish_body("@acme/widget", "2.0.0", "widget-2.0.0.tgz", tarball),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::CREATED);

    // GET packument（编码 URL）
    let (status, packument) = send(
        fx.router(),
        empty_req("GET", "/npm-hosted/@acme%2Fwidget", None),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(packument["name"], "@acme/widget");
    let dist = &packument["versions"]["2.0.0"]["dist"];
    // tarball 路径保留 scope 段
    assert_eq!(
        dist["tarball"],
        "http://localhost:8080/npm-hosted/@acme/widget/-/widget-2.0.0.tgz"
    );

    // GET scoped tarball（编码 URL）
    let (status, bytes) = send_bytes(
        fx.router(),
        empty_req("GET", "/npm-hosted/@acme%2Fwidget/-/widget-2.0.0.tgz", None),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(bytes, tarball);
}

// ---------- FR-61 版本不可变：重复 publish 同版本 409 ----------

#[tokio::test]
async fn npm_重复发布同版本返回_409() {
    let fx = Fixture::new().await;
    let writer = fx.seed_user("dev", "pw", Role::User).await;
    let rid = fx.seed_npm_repo("npm-hosted", Visibility::Public).await;
    fx.seed_acl(&rid, &writer, Permission::Write).await;
    let auth = format!("Bearer {}", fx.login_token("dev", "pw").await);

    let body = publish_body("pkg", "1.0.0", "pkg-1.0.0.tgz", b"v1");
    let (s1, _) = send(
        fx.router(),
        json_req("PUT", "/npm-hosted/pkg", Some(&auth), body.clone()),
    )
    .await;
    assert_eq!(s1, StatusCode::CREATED);

    // 再次发布同版本 → 409
    let (s2, err) = send(
        fx.router(),
        json_req("PUT", "/npm-hosted/pkg", Some(&auth), body),
    )
    .await;
    assert_eq!(s2, StatusCode::CONFLICT);
    assert_eq!(err["error"]["code"], "conflict");
}

// ---------- FR-09 写授权边界 ----------

#[tokio::test]
async fn npm_无写权限发布被拒() {
    let fx = Fixture::new().await;
    // public npm 仓库：匿名可读但无写 → 发布应 403
    fx.seed_npm_repo("npm-pub", Visibility::Public).await;
    let (status, _) = send(
        fx.router(),
        json_req(
            "PUT",
            "/npm-pub/pkg",
            None,
            publish_body("pkg", "1.0.0", "pkg-1.0.0.tgz", b"x"),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::FORBIDDEN);
}

#[tokio::test]
async fn npm_私有仓库对无权者读_404_隐藏存在性() {
    let fx = Fixture::new().await;
    fx.seed_npm_repo("npm-secret", Visibility::Private).await;
    // 匿名读 private packument → 404（不泄露存在）
    let (status, _) = send(fx.router(), empty_req("GET", "/npm-secret/pkg", None)).await;
    assert_eq!(status, StatusCode::NOT_FOUND);
}

// ---------- FR-12 proxy：cache-miss 回源 packument（重写 tarball）+ tarball 缓存 ----------

/// 启动一个 mock npm registry：按路径返回 packument 或 tarball，记录上游命中次数。
///
/// 返回 (上游基址, packument 命中计数, tarball 命中计数)。packument 的 dist.tarball
/// 指向上游自身地址，服务端代理后应重写为本仓库。
async fn 启动_mock_registry(
    package: &'static str,
    version: &'static str,
    tarball_name: &'static str,
    tarball: &'static [u8],
) -> (
    String,
    Arc<std::sync::atomic::AtomicUsize>,
    Arc<std::sync::atomic::AtomicUsize>,
) {
    use axum::extract::Path as AxPath;
    use axum::routing::get;
    use std::sync::atomic::{AtomicUsize, Ordering};

    let pack_calls = Arc::new(AtomicUsize::new(0));
    let tar_calls = Arc::new(AtomicUsize::new(0));

    // 先绑定端口，拿到地址后才能构造 packument 里的上游 tarball URL
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    let base = format!("http://{addr}");

    let upstream_tarball_url = format!("{base}/{package}/-/{tarball_name}");
    let packument = json!({
        "name": package,
        "dist-tags": { "latest": version },
        "versions": {
            version: {
                "name": package,
                "version": version,
                "dist": {
                    "tarball": upstream_tarball_url,
                    "shasum": sha1_hex(tarball),
                    "integrity": integrity(tarball)
                }
            }
        }
    })
    .to_string();

    let pc = pack_calls.clone();
    let tc = tar_calls.clone();
    let pack_doc = packument.clone();
    // /{package} → packument；/{package}/-/{file} → tarball
    let app = Router::new()
        .route(
            "/{pkg}",
            get(move |AxPath(_pkg): AxPath<String>| {
                let pc = pc.clone();
                let doc = pack_doc.clone();
                async move {
                    pc.fetch_add(1, Ordering::SeqCst);
                    (
                        [(axum::http::header::CONTENT_TYPE, "application/json")],
                        doc,
                    )
                }
            }),
        )
        .route(
            "/{pkg}/-/{file}",
            get(move |AxPath((_pkg, _file)): AxPath<(String, String)>| {
                let tc = tc.clone();
                async move {
                    tc.fetch_add(1, Ordering::SeqCst);
                    tarball
                }
            }),
        );

    tokio::spawn(async move {
        axum::serve(listener, app).await.unwrap();
    });
    (base, pack_calls, tar_calls)
}

#[tokio::test]
async fn npm_proxy_回源_packument_重写_tarball_并缓存() {
    use std::sync::atomic::Ordering;

    let tarball: &'static [u8] = b"upstream-tgz-bytes";
    let (上游基址, pack_calls, tar_calls) =
        启动_mock_registry("leftpad", "1.3.0", "leftpad-1.3.0.tgz", tarball).await;
    let fx = Fixture::new().await;
    let _rid = fx
        .seed_npm_proxy_repo("npm-mirror", Visibility::Public, &上游基址)
        .await;

    // ① 取 packument：proxy 回源 + 重写 tarball 指向本仓库
    let (status, packument) =
        send(fx.router(), empty_req("GET", "/npm-mirror/leftpad", None)).await;
    assert_eq!(status, StatusCode::OK);
    let dist = &packument["versions"]["1.3.0"]["dist"];
    assert_eq!(
        dist["tarball"],
        "http://localhost:8080/npm-mirror/leftpad/-/leftpad-1.3.0.tgz"
    );
    // integrity / shasum 保持上游原值（不改写，校验照常）
    assert_eq!(dist["shasum"], sha1_hex(tarball));
    assert_eq!(dist["integrity"], integrity(tarball));
    assert_eq!(
        pack_calls.load(Ordering::SeqCst),
        1,
        "应回源拉一次 packument"
    );

    // ② cache-miss 取 tarball：从重写后的本仓库路径下载，回源一次
    let (status, bytes) = send_bytes(
        fx.router(),
        empty_req("GET", "/npm-mirror/leftpad/-/leftpad-1.3.0.tgz", None),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(bytes, tarball);
    assert_eq!(
        tar_calls.load(Ordering::SeqCst),
        1,
        "首次 tarball 应回源一次"
    );

    // ③ cache-hit 再取 tarball：命中本地缓存，不再回源
    let (status, bytes) = send_bytes(
        fx.router(),
        empty_req("GET", "/npm-mirror/leftpad/-/leftpad-1.3.0.tgz", None),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(bytes, tarball);
    assert_eq!(
        tar_calls.load(Ordering::SeqCst),
        1,
        "命中缓存不应再回源 tarball"
    );
}
