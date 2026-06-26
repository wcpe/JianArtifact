//! 目录浏览的 HTTP 集成测试（FR-75）：Accept 驱动双形态 + 鉴权过滤。
//!
//! 重点穷举鉴权（§2.1 检索鉴权过滤 / §2.1 私有对匿名一律拒绝）：匿名 / 无权用户列举
//! private 仓库目录时，JSON 与 HTML 两形态均返回 404、不泄露资源存在性；public 匿名可浏览；
//! 有读 ACL 的用户可浏览。并验证目录请求（尾斜杠）与单文件下载（无尾斜杠）分流正确。

use std::sync::Arc;

use axum::body::Body;
use axum::http::{Request, StatusCode};
use axum::Router;
use base64::{engine::general_purpose::STANDARD, Engine};
use http_body_util::BodyExt;
use serde_json::{json, Value};
use tower::ServiceExt;

use jianartifact::api::{build_router, AppState};
use jianartifact::auth::{self, JwtSigner, LoginGuard};
use jianartifact::config::Config;
use jianartifact::format::{ArtifactService, DockerRegistry, FormatRegistry};
use jianartifact::meta::{MetaStore, NewRepository, Permission, RepoType, Role, Visibility};
use jianartifact::proxy::HttpUpstream;
use jianartifact::storage::{BlobBackend, LocalFsStore};

/// 测试夹具：持有可重复构建路由的状态与临时目录。
struct Fixture {
    state: AppState,
    _dir: tempfile::TempDir,
}

impl Fixture {
    async fn new() -> Self {
        let dir = tempfile::tempdir().unwrap();
        let meta = MetaStore::open(&dir.path().join("test.db")).await.unwrap();
        let store = BlobBackend::Fs(LocalFsStore::new(dir.path().join("blobs")).await.unwrap());
        let jwt = JwtSigner::from_secret(b"dir-browse-secret-32-bytes-xxxxxx", 3600);
        let upstream = HttpUpstream::new(std::time::Duration::from_secs(60)).unwrap();
        let artifacts = Arc::new(ArtifactService::new(store.clone(), meta.clone(), upstream));
        let docker = Arc::new(
            DockerRegistry::new(
                store.clone(),
                meta.clone(),
                dir.path().join("uploads"),
                None,
            )
            .await
            .unwrap(),
        );
        let mut config = Config::default();
        config.server.public_base_url = Some("http://localhost:8080".to_string());
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
            docker: Some(docker),
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
            restart: std::sync::Arc::new(jianartifact::update::RestartHandle::default()),
            settings: std::sync::Arc::new(
                jianartifact::config::EditableSettings::new(
                    jianartifact::config::NetworkProxyConfig::default(),
                    std::time::Duration::from_secs(60),
                    &jianartifact::config::UpdateConfig::default(),
                )
                .unwrap(),
            ),
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

    async fn seed_repo(&self, name: &str, visibility: Visibility) -> String {
        self.state
            .meta
            .create_repository(NewRepository {
                name,
                format: "raw",
                r#type: RepoType::Hosted,
                visibility,
                upstream_url: None,
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

/// 发请求返回 (状态码, JSON 体)。
async fn send(router: Router, req: Request<Body>) -> (StatusCode, Value) {
    let resp = router.oneshot(req).await.unwrap();
    let status = resp.status();
    let bytes = resp.into_body().collect().await.unwrap().to_bytes();
    let body = serde_json::from_slice(&bytes).unwrap_or(Value::Null);
    (status, body)
}

/// 发请求返回 (状态码, content-type, 文本体)。
async fn send_text(router: Router, req: Request<Body>) -> (StatusCode, String, String) {
    let resp = router.oneshot(req).await.unwrap();
    let status = resp.status();
    let ct = resp
        .headers()
        .get("content-type")
        .and_then(|v| v.to_str().ok())
        .unwrap_or("")
        .to_string();
    let bytes = resp.into_body().collect().await.unwrap().to_bytes();
    let text = String::from_utf8_lossy(&bytes).to_string();
    (status, ct, text)
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

/// 构造原始字节请求（带可选 authorization 与 accept）。
fn raw_req(
    method: &str,
    uri: &str,
    auth: Option<&str>,
    accept: Option<&str>,
    body: Vec<u8>,
) -> Request<Body> {
    let mut builder = Request::builder().method(method).uri(uri);
    if let Some(a) = auth {
        builder = builder.header("authorization", a);
    }
    if let Some(a) = accept {
        builder = builder.header("accept", a);
    }
    builder.body(Body::from(body)).unwrap()
}

/// 经 PUT 在仓库内布置一个文件（需写权限）。
async fn put_file(fx: &Fixture, repo: &str, path: &str, auth: &str, data: &[u8]) {
    let (status, _) = send(
        fx.router(),
        raw_req(
            "PUT",
            &format!("/{repo}/{path}"),
            Some(auth),
            None,
            data.to_vec(),
        ),
    )
    .await;
    assert!(
        status == StatusCode::CREATED || status == StatusCode::OK,
        "布置文件应成功，实际 {status}"
    );
}

// ---------- FR-75 JSON 形态 ----------

#[tokio::test]
async fn 目录请求_json_列出一层子目录与文件() {
    let fx = Fixture::new().await;
    fx.seed_user("admin", "pw", Role::Admin).await;
    let auth = format!("Bearer {}", fx.login_token("admin", "pw").await);
    fx.seed_repo("files", Visibility::Public).await;

    // 布置：dir/a.txt、dir/sub/b.txt、top.txt
    put_file(&fx, "files", "dir/a.txt", &auth, b"a").await;
    put_file(&fx, "files", "dir/sub/b.txt", &auth, b"b").await;
    put_file(&fx, "files", "top.txt", &auth, b"t").await;

    // 列举 dir/：应见 a.txt（file）与 sub（folder），不见更深的 sub/b.txt 扁平铺开
    let (status, body) = send(
        fx.router(),
        raw_req(
            "GET",
            "/files/dir/",
            Some(&auth),
            Some("application/json"),
            vec![],
        ),
    )
    .await;
    assert_eq!(status, StatusCode::OK, "目录 JSON 列举应 200: {body}");
    let entries = body["entries"].as_array().expect("entries 应为数组");
    let names: Vec<(&str, &str)> = entries
        .iter()
        .map(|e| (e["name"].as_str().unwrap(), e["type"].as_str().unwrap()))
        .collect();
    assert!(
        names.contains(&("a.txt", "file")),
        "应含文件 a.txt: {names:?}"
    );
    assert!(
        names.contains(&("sub", "folder")),
        "应含子目录 sub: {names:?}"
    );
    // 不把更深层 b.txt 直接列出
    assert!(
        !names.iter().any(|(n, _)| *n == "b.txt"),
        "不应扁平铺开深层文件: {names:?}"
    );
    // 文件条目带元数据
    let a = entries.iter().find(|e| e["name"] == "a.txt").unwrap();
    assert_eq!(a["size"], 1);
    assert!(a["sha256"].as_str().unwrap().len() == 64);
}

#[tokio::test]
async fn 仓库根目录请求_json_列出顶层项() {
    let fx = Fixture::new().await;
    fx.seed_user("admin", "pw", Role::Admin).await;
    let auth = format!("Bearer {}", fx.login_token("admin", "pw").await);
    fx.seed_repo("files", Visibility::Public).await;
    put_file(&fx, "files", "dir/a.txt", &auth, b"a").await;
    put_file(&fx, "files", "top.txt", &auth, b"t").await;

    // 仓库根：GET /files/
    let (status, body) = send(
        fx.router(),
        raw_req(
            "GET",
            "/files/",
            Some(&auth),
            Some("application/json"),
            vec![],
        ),
    )
    .await;
    assert_eq!(status, StatusCode::OK, "{body}");
    let names: Vec<(&str, &str)> = body["entries"]
        .as_array()
        .unwrap()
        .iter()
        .map(|e| (e["name"].as_str().unwrap(), e["type"].as_str().unwrap()))
        .collect();
    assert!(names.contains(&("dir", "folder")), "{names:?}");
    assert!(names.contains(&("top.txt", "file")), "{names:?}");
}

#[tokio::test]
async fn 前缀列举不串入兄弟前缀() {
    let fx = Fixture::new().await;
    fx.seed_user("admin", "pw", Role::Admin).await;
    let auth = format!("Bearer {}", fx.login_token("admin", "pw").await);
    fx.seed_repo("files", Visibility::Public).await;
    // docs/ 与 docsx/ 是兄弟前缀，列举 docs/ 不得带出 docsx 的内容
    put_file(&fx, "files", "docs/readme.txt", &auth, b"r").await;
    put_file(&fx, "files", "docsx/note.txt", &auth, b"n").await;

    let (status, body) = send(
        fx.router(),
        raw_req(
            "GET",
            "/files/docs/",
            Some(&auth),
            Some("application/json"),
            vec![],
        ),
    )
    .await;
    assert_eq!(status, StatusCode::OK, "{body}");
    let names: Vec<&str> = body["entries"]
        .as_array()
        .unwrap()
        .iter()
        .map(|e| e["name"].as_str().unwrap())
        .collect();
    assert_eq!(names, vec!["readme.txt"], "只应含 docs/ 下的项: {names:?}");
}

// ---------- FR-75 HTML 形态 ----------

#[tokio::test]
async fn 目录请求_html_返回索引页() {
    let fx = Fixture::new().await;
    fx.seed_user("admin", "pw", Role::Admin).await;
    let auth = format!("Bearer {}", fx.login_token("admin", "pw").await);
    fx.seed_repo("files", Visibility::Public).await;
    put_file(&fx, "files", "dir/a.txt", &auth, b"a").await;
    put_file(&fx, "files", "dir/sub/b.txt", &auth, b"b").await;

    let (status, ct, html) = send_text(
        fx.router(),
        raw_req("GET", "/files/dir/", Some(&auth), Some("text/html"), vec![]),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert!(ct.starts_with("text/html"), "content-type 应为 html: {ct}");
    assert!(html.contains("a.txt"), "索引页应含文件名: {html}");
    assert!(html.contains("sub"), "索引页应含子目录名: {html}");
}

// ---------- §2.1 鉴权：私有对匿名 / 无权一律 404，不泄露存在性 ----------

#[tokio::test]
async fn 私有仓库目录_匿名列举_404_两形态都不泄露() {
    let fx = Fixture::new().await;
    fx.seed_user("admin", "pw", Role::Admin).await;
    let auth = format!("Bearer {}", fx.login_token("admin", "pw").await);
    fx.seed_repo("secret", Visibility::Private).await;
    put_file(&fx, "secret", "dir/a.txt", &auth, b"a").await;

    // 匿名 JSON
    let (status, body) = send(
        fx.router(),
        raw_req(
            "GET",
            "/secret/dir/",
            None,
            Some("application/json"),
            vec![],
        ),
    )
    .await;
    assert_eq!(status, StatusCode::NOT_FOUND, "匿名 JSON 应 404: {body}");
    // 不泄露文件名 / 仓库存在
    assert!(!body.to_string().contains("a.txt"));

    // 匿名 HTML
    let (status, _ct, html) = send_text(
        fx.router(),
        raw_req("GET", "/secret/dir/", None, Some("text/html"), vec![]),
    )
    .await;
    assert_eq!(status, StatusCode::NOT_FOUND, "匿名 HTML 应 404");
    assert!(!html.contains("a.txt"), "HTML 404 不应泄露文件名");
}

#[tokio::test]
async fn 私有仓库目录_无_acl_登录用户_404() {
    let fx = Fixture::new().await;
    fx.seed_user("admin", "pw", Role::Admin).await;
    fx.seed_user("outsider", "pw", Role::User).await;
    let admin = format!("Bearer {}", fx.login_token("admin", "pw").await);
    fx.seed_repo("secret", Visibility::Private).await;
    put_file(&fx, "secret", "dir/a.txt", &admin, b"a").await;

    let out = format!("Bearer {}", fx.login_token("outsider", "pw").await);
    let (status, _) = send(
        fx.router(),
        raw_req(
            "GET",
            "/secret/dir/",
            Some(&out),
            Some("application/json"),
            vec![],
        ),
    )
    .await;
    assert_eq!(status, StatusCode::NOT_FOUND, "无 ACL 用户应 404");
}

#[tokio::test]
async fn 私有仓库目录_有读_acl_可列举_三身份通道一致() {
    let fx = Fixture::new().await;
    fx.seed_user("admin", "pw", Role::Admin).await;
    let reader = fx.seed_user("reader", "pw", Role::User).await;
    let admin = format!("Bearer {}", fx.login_token("admin", "pw").await);
    let rid = fx.seed_repo("secret", Visibility::Private).await;
    fx.seed_acl(&rid, &reader, Permission::Read).await;
    put_file(&fx, "secret", "dir/a.txt", &admin, b"a").await;

    // 通道一：Bearer-JWT
    let jwt = fx.login_token("reader", "pw").await;
    let (status, body) = send(
        fx.router(),
        raw_req(
            "GET",
            "/secret/dir/",
            Some(&format!("Bearer {jwt}")),
            Some("application/json"),
            vec![],
        ),
    )
    .await;
    assert_eq!(status, StatusCode::OK, "JWT 通道应 200: {body}");

    // 通道二：API Token
    let plain = jianartifact::auth::generate_api_token();
    fx.state
        .meta
        .create_token(&reader, "ci", &jianartifact::auth::hash_api_token(&plain))
        .await
        .unwrap();
    let (status, _) = send(
        fx.router(),
        raw_req(
            "GET",
            "/secret/dir/",
            Some(&format!("Bearer {plain}")),
            Some("application/json"),
            vec![],
        ),
    )
    .await;
    assert_eq!(status, StatusCode::OK, "Token 通道应 200");

    // 通道三：Basic
    let basic = format!("Basic {}", STANDARD.encode("reader:pw"));
    let (status, _) = send(
        fx.router(),
        raw_req(
            "GET",
            "/secret/dir/",
            Some(&basic),
            Some("application/json"),
            vec![],
        ),
    )
    .await;
    assert_eq!(status, StatusCode::OK, "Basic 通道应 200");
}

#[tokio::test]
async fn 公开仓库目录_匿名可列举() {
    let fx = Fixture::new().await;
    fx.seed_user("admin", "pw", Role::Admin).await;
    let auth = format!("Bearer {}", fx.login_token("admin", "pw").await);
    fx.seed_repo("pub", Visibility::Public).await;
    put_file(&fx, "pub", "dir/a.txt", &auth, b"a").await;

    let (status, body) = send(
        fx.router(),
        raw_req("GET", "/pub/dir/", None, Some("application/json"), vec![]),
    )
    .await;
    assert_eq!(status, StatusCode::OK, "{body}");
    let names: Vec<&str> = body["entries"]
        .as_array()
        .unwrap()
        .iter()
        .map(|e| e["name"].as_str().unwrap())
        .collect();
    assert!(names.contains(&"a.txt"));
}

// ---------- 回归：无尾斜杠仍走单文件下载 ----------

#[tokio::test]
async fn 无尾斜杠路径仍是单文件下载() {
    let fx = Fixture::new().await;
    fx.seed_user("admin", "pw", Role::Admin).await;
    let auth = format!("Bearer {}", fx.login_token("admin", "pw").await);
    fx.seed_repo("files", Visibility::Public).await;
    put_file(&fx, "files", "dir/a.txt", &auth, b"hello").await;

    // 无尾斜杠 → 下载文件本体
    let resp = fx
        .router()
        .oneshot(raw_req(
            "GET",
            "/files/dir/a.txt",
            Some(&auth),
            None,
            vec![],
        ))
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::OK);
    let bytes = resp.into_body().collect().await.unwrap().to_bytes();
    assert_eq!(&bytes[..], b"hello", "应返回文件内容而非目录列表");
}
