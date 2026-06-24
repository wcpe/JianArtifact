//! PyPI 格式的 HTTP 集成测试（FR-27）。
//!
//! 覆盖：twine 上传 → Simple 索引 / 项目页（含 #sha256=）→ pip 下载字节一致（hosted）、
//! PEP503 项目名规范化、重复上传同文件 409（FR-61 不可覆盖）、sha256_digest 对账失败 400、
//! PEP691 JSON 内容协商、写授权边界（无写 403 / private 无权 404）、上传上限 413，
//! 以及 proxy：Simple 项目页回源重写文件链接 + 包文件 cache-miss → hit（真实 mock 上游走 HttpUpstream）。
//!
//! 鉴权与制品机理复用既有层，本文件只验 PyPI 协议适配的正确性。

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
use jianartifact::storage::LocalFsStore;

/// PEP691 JSON 内容协商类型（Simple 页面 JSON 形态）。
const PEP691_CONTENT_TYPE: &str = "application/vnd.pypi.simple.v1+json";

/// 测试夹具：真实 SQLite 文件 + 临时 blob 目录 + 固定对外地址。
struct Fixture {
    state: AppState,
    _dir: tempfile::TempDir,
}

impl Fixture {
    async fn new() -> Self {
        let dir = tempfile::tempdir().unwrap();
        let meta = MetaStore::open(&dir.path().join("test.db")).await.unwrap();
        let store = LocalFsStore::new(dir.path().join("blobs")).await.unwrap();
        let jwt = JwtSigner::from_secret(b"pypi-secret-32-bytes-xxxxxxxxxxxx", 3600);
        let upstream = HttpUpstream::new(std::time::Duration::from_secs(60)).unwrap();
        let artifacts = Arc::new(ArtifactService::new(store.clone(), meta.clone(), upstream));
        let mut config = Config::default();
        config.server.public_base_url = Some("http://localhost:8080".to_string());
        let state = AppState {
            config: Arc::new(config),
            meta,
            store,
            jwt,
            login_guard: Arc::new(LoginGuard::new(50, 900)),
            artifacts,
            formats: Arc::new(FormatRegistry::with_builtin()),
            docker: None,
        };
        Self { state, _dir: dir }
    }

    /// 用给定上传上限重建夹具（验 413）。
    async fn with_max_size(max: u64) -> Self {
        let dir = tempfile::tempdir().unwrap();
        let meta = MetaStore::open(&dir.path().join("test.db")).await.unwrap();
        let store = LocalFsStore::new(dir.path().join("blobs")).await.unwrap();
        let jwt = JwtSigner::from_secret(b"pypi-secret-32-bytes-xxxxxxxxxxxx", 3600);
        let upstream = HttpUpstream::new(std::time::Duration::from_secs(60)).unwrap();
        let artifacts = Arc::new(ArtifactService::new(store.clone(), meta.clone(), upstream));
        let mut config = Config::default();
        config.server.public_base_url = Some("http://localhost:8080".to_string());
        config.limits.max_artifact_size = Some(max);
        let state = AppState {
            config: Arc::new(config),
            meta,
            store,
            jwt,
            login_guard: Arc::new(LoginGuard::new(50, 900)),
            artifacts,
            formats: Arc::new(FormatRegistry::with_builtin()),
            docker: None,
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

    /// 建一个 PyPI hosted 仓库，返回 id。
    async fn seed_pypi_repo(&self, name: &str, visibility: Visibility) -> String {
        self.state
            .meta
            .create_repository(NewRepository {
                name,
                format: "pypi",
                r#type: RepoType::Hosted,
                visibility,
                upstream_url: None,
                upstream_auth_ref: None,
            })
            .await
            .unwrap()
    }

    /// 建一个 PyPI proxy 仓库（指向给定上游基址），返回 id。
    async fn seed_pypi_proxy_repo(
        &self,
        name: &str,
        visibility: Visibility,
        upstream_url: &str,
    ) -> String {
        self.state
            .meta
            .create_repository(NewRepository {
                name,
                format: "pypi",
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
        let (status, body) = send_json(
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

/// 发请求并返回 (状态码, 文本体)。
async fn send_text(router: Router, req: Request<Body>) -> (StatusCode, String) {
    let (status, bytes) = send_bytes(router, req).await;
    (status, String::from_utf8_lossy(&bytes).to_string())
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

fn accept_req(method: &str, uri: &str, accept: &str) -> Request<Body> {
    Request::builder()
        .method(method)
        .uri(uri)
        .header("accept", accept)
        .body(Body::empty())
        .unwrap()
}

/// 计算字节 sha256 十六进制（与服务端 / Simple 页面对账）。
fn sha256_hex(bytes: &[u8]) -> String {
    use sha2::{Digest, Sha256};
    let mut h = Sha256::new();
    h.update(bytes);
    format!("{:x}", h.finalize())
}

/// twine 上传体的一个字段（文本或文件）。
struct Part {
    name: &'static str,
    filename: Option<&'static str>,
    value: Vec<u8>,
}

fn text_part(name: &'static str, value: &str) -> Part {
    Part {
        name,
        filename: None,
        value: value.as_bytes().to_vec(),
    }
}

fn file_part(name: &'static str, filename: &'static str, value: &[u8]) -> Part {
    Part {
        name,
        filename: Some(filename),
        value: value.to_vec(),
    }
}

/// 构造一份 twine 风格的 multipart/form-data 上传请求（含 :action=file_upload）。
///
/// 返回 (Request, boundary)；body 按 multipart 规范拼接各字段。
fn twine_upload_req(repo: &str, auth: Option<&str>, parts: &[Part]) -> Request<Body> {
    let boundary = "----JianArtifactTestBoundary";
    let mut body: Vec<u8> = Vec::new();
    for p in parts {
        body.extend_from_slice(format!("--{boundary}\r\n").as_bytes());
        match p.filename {
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
        .uri(format!("/{repo}/"))
        .header(
            "content-type",
            format!("multipart/form-data; boundary={boundary}"),
        );
    if let Some(a) = auth {
        builder = builder.header("authorization", a);
    }
    builder.body(Body::from(body)).unwrap()
}

/// 构造标准上传字段（:action / name / version / [sha256_digest] / content 文件）。
fn upload_parts<'a>(
    name: &'a str,
    version: &'a str,
    sha256: Option<&'a str>,
    filename: &'static str,
    content: &'a [u8],
) -> Vec<Part> {
    // name / version 须为 'static 生命周期的字面量在测试里直接传，故此处转拥有。
    let mut parts = vec![
        text_part(":action", "file_upload"),
        Part {
            name: "name",
            filename: None,
            value: name.as_bytes().to_vec(),
        },
        Part {
            name: "version",
            filename: None,
            value: version.as_bytes().to_vec(),
        },
    ];
    if let Some(d) = sha256 {
        parts.push(Part {
            name: "sha256_digest",
            filename: None,
            value: d.as_bytes().to_vec(),
        });
    }
    parts.push(file_part("content", filename, content));
    parts
}

// ---------- hosted：twine 上传 → Simple → pip 下载端到端 ----------

#[tokio::test]
async fn pypi_上传后_simple_列表与下载端到端() {
    let fx = Fixture::new().await;
    let writer = fx.seed_user("dev", "pw", Role::User).await;
    let rid = fx.seed_pypi_repo("pypi-hosted", Visibility::Public).await;
    fx.seed_acl(&rid, &writer, Permission::Write).await;
    let auth = format!("Bearer {}", fx.login_token("dev", "pw").await);

    let wheel = b"this-is-a-fake-wheel-payload";
    let sha = sha256_hex(wheel);

    // twine 上传（带 sha256_digest 对账）
    let parts = upload_parts(
        "Flask",
        "3.0.0",
        Some(&sha),
        "Flask-3.0.0-py3-none-any.whl",
        wheel,
    );
    let (status, _) = send_text(
        fx.router(),
        twine_upload_req("pypi-hosted", Some(&auth), &parts),
    )
    .await;
    assert_eq!(status, StatusCode::OK, "twine 上传应 200");

    // Simple 根索引（公开仓库匿名可读）：含规范化项目名 flask
    let (status, html) =
        send_text(fx.router(), empty_req("GET", "/pypi-hosted/simple/", None)).await;
    assert_eq!(status, StatusCode::OK);
    assert!(
        html.contains("<a href=\"flask/\">flask</a>"),
        "根索引应列 flask: {html}"
    );

    // Simple 项目页：含文件锚点与 #sha256=
    let (status, html) = send_text(
        fx.router(),
        empty_req("GET", "/pypi-hosted/simple/flask/", None),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert!(
        html.contains(&format!(
            "packages/flask/Flask-3.0.0-py3-none-any.whl#sha256={sha}"
        )),
        "项目页文件链接应带本仓库路径与 sha256: {html}"
    );

    // pip 下载包文件：字节一致 + 校验头 sha256 一致
    let resp = fx
        .router()
        .oneshot(empty_req(
            "GET",
            "/pypi-hosted/packages/flask/Flask-3.0.0-py3-none-any.whl",
            None,
        ))
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::OK);
    let header_sha = resp
        .headers()
        .get("x-checksum-sha256")
        .and_then(|v| v.to_str().ok())
        .unwrap_or("")
        .to_string();
    assert_eq!(header_sha, sha);
    let bytes = resp
        .into_body()
        .collect()
        .await
        .unwrap()
        .to_bytes()
        .to_vec();
    assert_eq!(bytes, wheel);
}

// ---------- PEP503 项目名规范化：上传 Holy_Grail，simple/holy-grail/ 命中 ----------

#[tokio::test]
async fn pypi_项目名规范化_上传与查询一致() {
    let fx = Fixture::new().await;
    let writer = fx.seed_user("dev", "pw", Role::User).await;
    let rid = fx.seed_pypi_repo("pypi-hosted", Visibility::Public).await;
    fx.seed_acl(&rid, &writer, Permission::Write).await;
    let auth = format!("Bearer {}", fx.login_token("dev", "pw").await);

    let sdist = b"holy-grail-sdist";
    let parts = upload_parts("Holy_Grail", "1.0", None, "Holy_Grail-1.0.tar.gz", sdist);
    let (status, _) = send_text(
        fx.router(),
        twine_upload_req("pypi-hosted", Some(&auth), &parts),
    )
    .await;
    assert_eq!(status, StatusCode::OK);

    // 用规范名 holy-grail 查询项目页应命中
    let (status, html) = send_text(
        fx.router(),
        empty_req("GET", "/pypi-hosted/simple/holy-grail/", None),
    )
    .await;
    assert_eq!(status, StatusCode::OK, "规范名查询应命中: {html}");
    assert!(html.contains("Holy_Grail-1.0.tar.gz"));
}

// ---------- FR-61 不可覆盖：重复上传同文件 409 ----------

#[tokio::test]
async fn pypi_重复上传同文件返回_409() {
    let fx = Fixture::new().await;
    let writer = fx.seed_user("dev", "pw", Role::User).await;
    let rid = fx.seed_pypi_repo("pypi-hosted", Visibility::Public).await;
    fx.seed_acl(&rid, &writer, Permission::Write).await;
    let auth = format!("Bearer {}", fx.login_token("dev", "pw").await);

    let wheel = b"immutable-wheel";
    let parts = upload_parts("pkg", "1.0", None, "pkg-1.0-py3-none-any.whl", wheel);
    let (s1, _) = send_text(
        fx.router(),
        twine_upload_req("pypi-hosted", Some(&auth), &parts),
    )
    .await;
    assert_eq!(s1, StatusCode::OK);

    // 再次上传同文件 → 409
    let parts2 = upload_parts("pkg", "1.0", None, "pkg-1.0-py3-none-any.whl", wheel);
    let (s2, body) = send_json(
        fx.router(),
        twine_upload_req("pypi-hosted", Some(&auth), &parts2),
    )
    .await;
    assert_eq!(s2, StatusCode::CONFLICT);
    assert_eq!(body["error"]["code"], "conflict");
}

// ---------- sha256_digest 对账失败 → 400 且不落盘 ----------

#[tokio::test]
async fn pypi_摘要不符返回_400_且不落盘() {
    let fx = Fixture::new().await;
    let writer = fx.seed_user("dev", "pw", Role::User).await;
    let rid = fx.seed_pypi_repo("pypi-hosted", Visibility::Public).await;
    fx.seed_acl(&rid, &writer, Permission::Write).await;
    let auth = format!("Bearer {}", fx.login_token("dev", "pw").await);

    let wheel = b"content-bytes";
    // 故意给错误摘要
    let bad = "0000000000000000000000000000000000000000000000000000000000000000";
    let parts = upload_parts("pkg", "1.0", Some(bad), "pkg-1.0-py3-none-any.whl", wheel);
    let (status, body) = send_json(
        fx.router(),
        twine_upload_req("pypi-hosted", Some(&auth), &parts),
    )
    .await;
    assert_eq!(status, StatusCode::BAD_REQUEST);
    assert_eq!(body["error"]["code"], "bad_request");

    // 不落盘：项目页应 404（无任何文件）
    let (status, _) = send_text(
        fx.router(),
        empty_req("GET", "/pypi-hosted/simple/pkg/", None),
    )
    .await;
    assert_eq!(status, StatusCode::NOT_FOUND, "摘要不符不应落盘");
}

// ---------- PEP691 JSON 内容协商 ----------

#[tokio::test]
async fn pypi_simple_json_内容协商() {
    let fx = Fixture::new().await;
    let writer = fx.seed_user("dev", "pw", Role::User).await;
    let rid = fx.seed_pypi_repo("pypi-hosted", Visibility::Public).await;
    fx.seed_acl(&rid, &writer, Permission::Write).await;
    let auth = format!("Bearer {}", fx.login_token("dev", "pw").await);

    let wheel = b"json-content-negotiation";
    let sha = sha256_hex(wheel);
    let parts = upload_parts("pkg", "1.0", None, "pkg-1.0-py3-none-any.whl", wheel);
    send_text(
        fx.router(),
        twine_upload_req("pypi-hosted", Some(&auth), &parts),
    )
    .await;

    // 根索引 JSON
    let (status, body) = send_json(
        fx.router(),
        accept_req("GET", "/pypi-hosted/simple/", PEP691_CONTENT_TYPE),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(body["meta"]["api-version"], "1.0");
    assert_eq!(body["projects"][0]["name"], "pkg");

    // 项目页 JSON：hashes.sha256 与服务端一致
    let (status, body) = send_json(
        fx.router(),
        accept_req("GET", "/pypi-hosted/simple/pkg/", PEP691_CONTENT_TYPE),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(body["files"][0]["filename"], "pkg-1.0-py3-none-any.whl");
    assert_eq!(body["files"][0]["hashes"]["sha256"], sha);
}

// ---------- 写授权边界 ----------

#[tokio::test]
async fn pypi_无写权限上传被拒_403() {
    let fx = Fixture::new().await;
    // public 仓库匿名可读但无写 → 上传 403
    fx.seed_pypi_repo("pypi-pub", Visibility::Public).await;
    let wheel = b"x";
    let parts = upload_parts("pkg", "1.0", None, "pkg-1.0-py3-none-any.whl", wheel);
    let (status, _) = send_text(fx.router(), twine_upload_req("pypi-pub", None, &parts)).await;
    assert_eq!(status, StatusCode::FORBIDDEN);
}

#[tokio::test]
async fn pypi_私有仓库对无权者读_404_隐藏存在性() {
    let fx = Fixture::new().await;
    fx.seed_pypi_repo("pypi-secret", Visibility::Private).await;
    // 匿名读 private Simple 索引 → 404（不泄露存在）
    let (status, _) = send_text(fx.router(), empty_req("GET", "/pypi-secret/simple/", None)).await;
    assert_eq!(status, StatusCode::NOT_FOUND);
}

// ---------- FR-64 上传上限 413 ----------

#[tokio::test]
async fn pypi_上传超限返回_413() {
    let fx = Fixture::with_max_size(8).await;
    let writer = fx.seed_user("dev", "pw", Role::User).await;
    let rid = fx.seed_pypi_repo("pypi-hosted", Visibility::Public).await;
    fx.seed_acl(&rid, &writer, Permission::Write).await;
    let auth = format!("Bearer {}", fx.login_token("dev", "pw").await);

    // content 超过 8 字节上限
    let wheel = b"0123456789-too-large";
    let parts = upload_parts("pkg", "1.0", None, "pkg-1.0-py3-none-any.whl", wheel);
    let (status, body) = send_json(
        fx.router(),
        twine_upload_req("pypi-hosted", Some(&auth), &parts),
    )
    .await;
    assert_eq!(status, StatusCode::PAYLOAD_TOO_LARGE);
    assert_eq!(body["error"]["code"], "payload_too_large");
}

// ---------- proxy：Simple 项目页回源重写 + 包文件 cache-miss → hit ----------

/// 启动一个 mock PyPI 上游：根 `/simple/{project}/` 返回项目页 HTML（文件链接指向上游自身），
/// `/files/{file}` 返回包文件字节，记录各自命中次数。
///
/// 返回 (上游基址, 项目页命中计数, 文件命中计数)。上游基址为主机根（约定：proxy upstream 指向 host 根）。
async fn 启动_mock_pypi(
    project: &'static str,
    filename: &'static str,
    file_bytes: &'static [u8],
) -> (
    String,
    Arc<std::sync::atomic::AtomicUsize>,
    Arc<std::sync::atomic::AtomicUsize>,
) {
    use axum::routing::get;
    use std::sync::atomic::{AtomicUsize, Ordering};

    let simple_calls = Arc::new(AtomicUsize::new(0));
    let file_calls = Arc::new(AtomicUsize::new(0));

    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    let base = format!("http://{addr}");

    // 项目页文件链接指向上游自身的 /files/{file}（跨"路径"，模拟 files.pythonhosted.org）
    let upstream_file_url = format!("{base}/files/{filename}");
    let sha = sha256_hex(file_bytes);
    let project_html = format!(
        "<!DOCTYPE html><html><body><a href=\"{upstream_file_url}#sha256={sha}\">{filename}</a></body></html>"
    );

    let sc = simple_calls.clone();
    let fc = file_calls.clone();
    let doc = project_html.clone();
    let app = Router::new()
        .route(
            "/simple/{project}/",
            get(move |_p: axum::extract::Path<String>| {
                let sc = sc.clone();
                let doc = doc.clone();
                async move {
                    sc.fetch_add(1, Ordering::SeqCst);
                    ([(axum::http::header::CONTENT_TYPE, "text/html")], doc)
                }
            }),
        )
        .route(
            "/files/{file}",
            get(move |_f: axum::extract::Path<String>| {
                let fc = fc.clone();
                async move {
                    fc.fetch_add(1, Ordering::SeqCst);
                    file_bytes
                }
            }),
        );
    let _ = project;

    tokio::spawn(async move {
        axum::serve(listener, app).await.unwrap();
    });
    (base, simple_calls, file_calls)
}

#[tokio::test]
async fn pypi_proxy_回源项目页重写_并缓存包文件() {
    use std::sync::atomic::Ordering;

    let file_bytes: &'static [u8] = b"upstream-wheel-bytes";
    let (上游基址, simple_calls, file_calls) =
        启动_mock_pypi("flask", "Flask-3.0.0-py3-none-any.whl", file_bytes).await;
    let fx = Fixture::new().await;
    fx.seed_pypi_proxy_repo("pypi-mirror", Visibility::Public, &上游基址)
        .await;
    let sha = sha256_hex(file_bytes);

    // ① Simple 项目页：proxy 回源 + 重写文件链接指向本仓库
    let (status, html) = send_text(
        fx.router(),
        empty_req("GET", "/pypi-mirror/simple/flask/", None),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert!(
        html.contains(&format!(
            "../../packages/flask/Flask-3.0.0-py3-none-any.whl#sha256={sha}"
        )),
        "项目页应重写为本仓库路径: {html}"
    );
    assert!(!html.contains("/files/"), "不应残留上游文件路径: {html}");
    assert_eq!(simple_calls.load(Ordering::SeqCst), 1, "应回源一次项目页");

    // ② cache-miss 取包文件：proxy 先取项目页解析上游 URL，再回源文件
    let (status, bytes) = send_bytes(
        fx.router(),
        empty_req(
            "GET",
            "/pypi-mirror/packages/flask/Flask-3.0.0-py3-none-any.whl",
            None,
        ),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(bytes, file_bytes);
    assert_eq!(file_calls.load(Ordering::SeqCst), 1, "首次包文件应回源一次");

    // ③ cache-hit 再取包文件：命中本地缓存，不再回源文件
    let (status, bytes) = send_bytes(
        fx.router(),
        empty_req(
            "GET",
            "/pypi-mirror/packages/flask/Flask-3.0.0-py3-none-any.whl",
            None,
        ),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(bytes, file_bytes);
    assert_eq!(
        file_calls.load(Ordering::SeqCst),
        1,
        "命中缓存不应再回源包文件"
    );
}
