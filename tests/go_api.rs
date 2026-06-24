//! Go 模块格式（GOPROXY 协议）的 HTTP 集成测试（FR-28）。
//!
//! 覆盖：hosted 上传 .mod/.zip/.info → @v/list / .info / .mod / .zip 取回字节一致、
//! @latest 取最大版本、bang 编码模块路径往返、重复上传同版本 409（不可变）、
//! 写授权边界、private 无权 404，以及 proxy cache-miss 回源 .zip 命中缓存、@v/list 回源透传
//! （真实 mock GOPROXY 走 HttpUpstream）。
//!
//! 鉴权与制品机理复用既有层，本文件只验 Go 协议适配的正确性。

use std::io::Write as _;
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
        let dir = tempfile::tempdir().unwrap();
        let meta = MetaStore::open(&dir.path().join("test.db")).await.unwrap();
        let store = BlobBackend::Fs(LocalFsStore::new(dir.path().join("blobs")).await.unwrap());
        let jwt = JwtSigner::from_secret(b"go-secret-32-bytes-xxxxxxxxxxxxxx", 3600);
        let upstream = HttpUpstream::new(std::time::Duration::from_secs(60)).unwrap();
        let artifacts = Arc::new(ArtifactService::new(store.clone(), meta.clone(), upstream));
        let mut config = Config::default();
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

    async fn seed_go_repo(&self, name: &str, visibility: Visibility) -> String {
        self.state
            .meta
            .create_repository(NewRepository {
                name,
                format: "go",
                r#type: RepoType::Hosted,
                visibility,
                upstream_url: None,
                upstream_auth_ref: None,
            })
            .await
            .unwrap()
    }

    async fn seed_go_proxy_repo(
        &self,
        name: &str,
        visibility: Visibility,
        upstream_url: &str,
    ) -> String {
        self.state
            .meta
            .create_repository(NewRepository {
                name,
                format: "go",
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

/// 发请求并返回 (状态码, 文本体)。
async fn send_text(router: Router, req: Request<Body>) -> (StatusCode, String) {
    let (status, bytes) = send_bytes(router, req).await;
    (status, String::from_utf8(bytes).unwrap())
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

fn bytes_req(method: &str, uri: &str, auth: Option<&str>, body: Vec<u8>) -> Request<Body> {
    let mut builder = Request::builder().method(method).uri(uri);
    if let Some(a) = auth {
        builder = builder.header("authorization", a);
    }
    builder.body(Body::from(body)).unwrap()
}

fn empty_req(method: &str, uri: &str, auth: Option<&str>) -> Request<Body> {
    let mut builder = Request::builder().method(method).uri(uri);
    if let Some(a) = auth {
        builder = builder.header("authorization", a);
    }
    builder.body(Body::empty()).unwrap()
}

/// 构造一个最小模块 zip：内部布局 `{module}@{version}/go.mod`，验证字节往返与可解压。
fn build_module_zip(module: &str, version: &str, go_mod: &str) -> Vec<u8> {
    let mut buf = Vec::new();
    {
        let mut zw = zip::ZipWriter::new(std::io::Cursor::new(&mut buf));
        let opts: zip::write::FileOptions<()> =
            zip::write::FileOptions::default().compression_method(zip::CompressionMethod::Deflated);
        zw.start_file(format!("{module}@{version}/go.mod"), opts)
            .unwrap();
        zw.write_all(go_mod.as_bytes()).unwrap();
        zw.finish().unwrap();
    }
    buf
}

/// 校验 zip 内含 `{module}@{version}/go.mod` 条目（确认 proxy / hosted 字节往返无损）。
fn zip_contains_gomod(bytes: &[u8], module: &str, version: &str) -> bool {
    let reader = std::io::Cursor::new(bytes);
    let mut archive = match zip::ZipArchive::new(reader) {
        Ok(a) => a,
        Err(_) => return false,
    };
    let entry = format!("{module}@{version}/go.mod");
    (0..archive.len()).any(|i| {
        archive
            .by_index(i)
            .map(|f| f.name() == entry)
            .unwrap_or(false)
    })
}

// ---------- FR-28 hosted：上传 → list / info / mod / zip 端到端 ----------

#[tokio::test]
async fn go_上传后取_list_info_mod_zip_端到端() {
    let fx = Fixture::new().await;
    let writer = fx.seed_user("dev", "pw", Role::User).await;
    let rid = fx.seed_go_repo("go-hosted", Visibility::Public).await;
    fx.seed_acl(&rid, &writer, Permission::Write).await;
    let auth = format!("Bearer {}", fx.login_token("dev", "pw").await);

    let module = "golang.org/x/text";
    let version = "v0.3.7";
    let go_mod = "module golang.org/x/text\n\ngo 1.18\n";
    let zip_bytes = build_module_zip(module, version, go_mod);

    // 上传 .mod
    let (s, _) = send(
        fx.router(),
        bytes_req(
            "PUT",
            "/go-hosted/golang.org/x/text/@v/v0.3.7.mod",
            Some(&auth),
            go_mod.as_bytes().to_vec(),
        ),
    )
    .await;
    assert_eq!(s, StatusCode::CREATED, "上传 .mod 应 201");

    // 上传 .zip
    let (s, _) = send(
        fx.router(),
        bytes_req(
            "PUT",
            "/go-hosted/golang.org/x/text/@v/v0.3.7.zip",
            Some(&auth),
            zip_bytes.clone(),
        ),
    )
    .await;
    assert_eq!(s, StatusCode::CREATED, "上传 .zip 应 201");

    // @v/list（公开仓库匿名可读）
    let (s, list) = send_text(
        fx.router(),
        empty_req("GET", "/go-hosted/golang.org/x/text/@v/list", None),
    )
    .await;
    assert_eq!(s, StatusCode::OK);
    assert_eq!(list.trim(), "v0.3.7", "list 应含该版本");

    // .mod 取回字节一致
    let (s, mod_bytes) = send_text(
        fx.router(),
        empty_req("GET", "/go-hosted/golang.org/x/text/@v/v0.3.7.mod", None),
    )
    .await;
    assert_eq!(s, StatusCode::OK);
    assert_eq!(mod_bytes, go_mod, "go.mod 字节应一致");

    // .info 未显式上传 → 据 .mod created_at 合成，含 Version 字段
    let (s, info) = send(
        fx.router(),
        empty_req("GET", "/go-hosted/golang.org/x/text/@v/v0.3.7.info", None),
    )
    .await;
    assert_eq!(s, StatusCode::OK);
    assert_eq!(info["Version"], "v0.3.7");
    assert!(info["Time"].is_string(), "info 应含 Time 字段");

    // .zip 取回字节一致且可解压
    let (s, got_zip) = send_bytes(
        fx.router(),
        empty_req("GET", "/go-hosted/golang.org/x/text/@v/v0.3.7.zip", None),
    )
    .await;
    assert_eq!(s, StatusCode::OK);
    assert_eq!(got_zip, zip_bytes, "zip 字节应一致");
    assert!(
        zip_contains_gomod(&got_zip, module, version),
        "zip 应可解压并含 go.mod"
    );
}

#[tokio::test]
async fn go_latest_取最大版本() {
    let fx = Fixture::new().await;
    let writer = fx.seed_user("dev", "pw", Role::User).await;
    let rid = fx.seed_go_repo("go-hosted", Visibility::Public).await;
    fx.seed_acl(&rid, &writer, Permission::Write).await;
    let auth = format!("Bearer {}", fx.login_token("dev", "pw").await);

    // 上传三个版本的 .mod（乱序）
    for ver in ["v1.0.0", "v1.2.0", "v1.1.5"] {
        let go_mod = format!("module example.com/m\n\ngo 1.20\n// {ver}\n");
        let (s, _) = send(
            fx.router(),
            bytes_req(
                "PUT",
                &format!("/go-hosted/example.com/m/@v/{ver}.mod"),
                Some(&auth),
                go_mod.into_bytes(),
            ),
        )
        .await;
        assert_eq!(s, StatusCode::CREATED);
    }

    // @latest 取最大版本 v1.2.0
    let (s, latest) = send(
        fx.router(),
        empty_req("GET", "/go-hosted/example.com/m/@latest", None),
    )
    .await;
    assert_eq!(s, StatusCode::OK);
    assert_eq!(latest["Version"], "v1.2.0");

    // @v/list 含三个版本
    let (s, list) = send_text(
        fx.router(),
        empty_req("GET", "/go-hosted/example.com/m/@v/list", None),
    )
    .await;
    assert_eq!(s, StatusCode::OK);
    let mut versions: Vec<&str> = list.lines().collect();
    versions.sort();
    assert_eq!(versions, vec!["v1.0.0", "v1.1.5", "v1.2.0"]);
}

// ---------- bang 编码模块路径往返 ----------

#[tokio::test]
async fn go_bang_编码模块路径往返() {
    let fx = Fixture::new().await;
    let writer = fx.seed_user("dev", "pw", Role::User).await;
    let rid = fx.seed_go_repo("go-hosted", Visibility::Public).await;
    fx.seed_acl(&rid, &writer, Permission::Write).await;
    let auth = format!("Bearer {}", fx.login_token("dev", "pw").await);

    // 模块 github.com/Sirupsen/logrus → bang 编码 github.com/!sirupsen/logrus
    let go_mod = "module github.com/Sirupsen/logrus\n\ngo 1.17\n";
    let (s, _) = send(
        fx.router(),
        bytes_req(
            "PUT",
            "/go-hosted/github.com/!sirupsen/logrus/@v/v1.9.0.mod",
            Some(&auth),
            go_mod.as_bytes().to_vec(),
        ),
    )
    .await;
    assert_eq!(s, StatusCode::CREATED);

    // 用 bang 编码路径取回 .mod
    let (s, got) = send_text(
        fx.router(),
        empty_req(
            "GET",
            "/go-hosted/github.com/!sirupsen/logrus/@v/v1.9.0.mod",
            None,
        ),
    )
    .await;
    assert_eq!(s, StatusCode::OK);
    assert_eq!(got, go_mod);

    // @latest 的 go get 片段在制品详情中用解码后模块路径——此处至少验 list 用 bang 路径可聚合
    let (s, list) = send_text(
        fx.router(),
        empty_req(
            "GET",
            "/go-hosted/github.com/!sirupsen/logrus/@v/list",
            None,
        ),
    )
    .await;
    assert_eq!(s, StatusCode::OK);
    assert_eq!(list.trim(), "v1.9.0");
}

// ---------- FR-61 不可变：重复上传同版本 409 ----------

#[tokio::test]
async fn go_重复上传同版本返回_409() {
    let fx = Fixture::new().await;
    let writer = fx.seed_user("dev", "pw", Role::User).await;
    let rid = fx.seed_go_repo("go-hosted", Visibility::Public).await;
    fx.seed_acl(&rid, &writer, Permission::Write).await;
    let auth = format!("Bearer {}", fx.login_token("dev", "pw").await);

    let go_mod = "module example.com/m\n\ngo 1.20\n";
    let uri = "/go-hosted/example.com/m/@v/v1.0.0.mod";
    let (s1, _) = send(
        fx.router(),
        bytes_req("PUT", uri, Some(&auth), go_mod.as_bytes().to_vec()),
    )
    .await;
    assert_eq!(s1, StatusCode::CREATED);

    // 再次上传同版本 .mod → 409（不可变）
    let (s2, err) = send(
        fx.router(),
        bytes_req("PUT", uri, Some(&auth), go_mod.as_bytes().to_vec()),
    )
    .await;
    assert_eq!(s2, StatusCode::CONFLICT);
    assert_eq!(err["error"]["code"], "conflict");
}

// ---------- FR-09 写授权边界 / 检索鉴权 ----------

#[tokio::test]
async fn go_无写权限上传被拒() {
    let fx = Fixture::new().await;
    // public go 仓库：匿名可读但无写 → 上传应 403
    fx.seed_go_repo("go-pub", Visibility::Public).await;
    let (status, _) = send(
        fx.router(),
        bytes_req(
            "PUT",
            "/go-pub/example.com/m/@v/v1.0.0.mod",
            None,
            b"module example.com/m\n".to_vec(),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::FORBIDDEN);
}

#[tokio::test]
async fn go_私有仓库对无权者读_404_隐藏存在性() {
    let fx = Fixture::new().await;
    fx.seed_go_repo("go-secret", Visibility::Private).await;
    // 匿名读 private @v/list → 404（不泄露存在）
    let (status, _) = send(
        fx.router(),
        empty_req("GET", "/go-secret/example.com/m/@v/list", None),
    )
    .await;
    assert_eq!(status, StatusCode::NOT_FOUND);
}

// ---------- FR-12 proxy：cache-miss 回源 .zip 命中缓存 + @v/list 回源透传 ----------

/// 启动一个 mock GOPROXY：按 GOPROXY 端点返回 list / mod / zip，记录 zip 与 list 命中次数。
///
/// 返回 (上游基址, list 命中计数, zip 命中计数)。
async fn 启动_mock_goproxy(
    list_body: &'static str,
    mod_body: &'static str,
    zip_body: &'static [u8],
) -> (
    String,
    Arc<std::sync::atomic::AtomicUsize>,
    Arc<std::sync::atomic::AtomicUsize>,
) {
    use axum::extract::Path as AxPath;
    use axum::routing::get;
    use std::sync::atomic::{AtomicUsize, Ordering};

    let list_calls = Arc::new(AtomicUsize::new(0));
    let zip_calls = Arc::new(AtomicUsize::new(0));

    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    let base = format!("http://{addr}");

    let lc = list_calls.clone();
    let zc = zip_calls.clone();

    // 用 catch-all 匹配 GOPROXY 端点（模块路径可多段），据末段分派
    let app = Router::new().route(
        "/{*rest}",
        get(move |AxPath(rest): AxPath<String>| {
            let lc = lc.clone();
            let zc = zc.clone();
            async move {
                use axum::http::header::CONTENT_TYPE;
                use axum::response::IntoResponse;
                if rest.ends_with("/@v/list") {
                    lc.fetch_add(1, Ordering::SeqCst);
                    return ([(CONTENT_TYPE, "text/plain")], list_body.to_string()).into_response();
                }
                if rest.ends_with(".mod") {
                    return ([(CONTENT_TYPE, "text/plain")], mod_body.to_string()).into_response();
                }
                if rest.ends_with(".zip") {
                    zc.fetch_add(1, Ordering::SeqCst);
                    return ([(CONTENT_TYPE, "application/zip")], zip_body.to_vec())
                        .into_response();
                }
                StatusCode::NOT_FOUND.into_response()
            }
        }),
    );

    tokio::spawn(async move {
        axum::serve(listener, app).await.unwrap();
    });
    (base, list_calls, zip_calls)
}

#[tokio::test]
async fn go_proxy_回源_list_透传与_zip_缓存() {
    use std::sync::atomic::Ordering;

    let module = "example.com/m";
    let version = "v1.0.0";
    let zip_static: &'static [u8] = Box::leak(
        build_module_zip(module, version, "module example.com/m\n\ngo 1.20\n").into_boxed_slice(),
    );
    let (上游基址, list_calls, zip_calls) =
        启动_mock_goproxy("v1.0.0\nv1.1.0\n", "module example.com/m\n", zip_static).await;

    let fx = Fixture::new().await;
    fx.seed_go_proxy_repo("go-mirror", Visibility::Public, &上游基址)
        .await;

    // ① @v/list：proxy 回源透传
    let (s, list) = send_text(
        fx.router(),
        empty_req("GET", "/go-mirror/example.com/m/@v/list", None),
    )
    .await;
    assert_eq!(s, StatusCode::OK);
    assert!(list.contains("v1.0.0") && list.contains("v1.1.0"));
    assert_eq!(list_calls.load(Ordering::SeqCst), 1, "list 应回源一次");

    // ② cache-miss 取 .zip：回源一次，字节一致
    let (s, bytes) = send_bytes(
        fx.router(),
        empty_req("GET", "/go-mirror/example.com/m/@v/v1.0.0.zip", None),
    )
    .await;
    assert_eq!(s, StatusCode::OK);
    assert_eq!(bytes, zip_static);
    assert!(zip_contains_gomod(&bytes, module, version));
    assert_eq!(zip_calls.load(Ordering::SeqCst), 1, "首次 zip 应回源一次");

    // ③ cache-hit 再取 .zip：命中本地缓存，不再回源
    let (s, bytes) = send_bytes(
        fx.router(),
        empty_req("GET", "/go-mirror/example.com/m/@v/v1.0.0.zip", None),
    )
    .await;
    assert_eq!(s, StatusCode::OK);
    assert_eq!(bytes, zip_static);
    assert_eq!(
        zip_calls.load(Ordering::SeqCst),
        1,
        "命中缓存不应再回源 zip"
    );
}
