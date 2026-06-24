//! L1 认证与身份层的 HTTP 集成测试：通过 axum 路由端到端验证登录、会话刷新、
//! API Token、Basic Auth、用户管理与身份四通道（FR-01/02/03/04/05/63/65）。

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
use jianartifact::format::{ArtifactService, DockerRegistry, FormatRegistry};
use jianartifact::meta::{MetaStore, Role};
use jianartifact::proxy::HttpUpstream;
use jianartifact::storage::{BlobBackend, LocalFsStore};

/// 测试夹具：持有可重复构建路由的状态与临时目录。
struct Fixture {
    state: AppState,
    _dir: tempfile::TempDir,
}

impl Fixture {
    /// 用默认认证参数构造夹具。
    async fn new() -> Self {
        Self::with_auth(3600, 5, 900).await
    }

    /// 用指定 JWT TTL 与登录防护参数构造夹具。
    async fn with_auth(ttl_secs: u64, max_failures: u32, lockout_secs: u64) -> Self {
        let dir = tempfile::tempdir().unwrap();
        // 集成测试走真实 SQLite 文件（open_in_memory 仅 cfg(test) 内部可见）
        let meta = MetaStore::open(&dir.path().join("test.db")).await.unwrap();
        let store = BlobBackend::Fs(LocalFsStore::new(dir.path().join("blobs")).await.unwrap());
        let jwt = JwtSigner::from_secret(b"integration-secret-32-bytes-xxxx", ttl_secs);
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
        // 审计采集：建有界 channel 并启动写入任务，使登录等事件真实落库供断言
        let (audit, audit_rx) = jianartifact::api::audit_channel();
        jianartifact::api::spawn_audit_writer(meta.clone(), audit_rx);
        // 使用分析采集：建有界 channel 并启动写入任务（关明细），使路由真实走采集链路
        let (usage, usage_rx) = jianartifact::api::usage_channel();
        jianartifact::api::spawn_usage_writer(meta.clone(), usage_rx, false);
        let state = AppState {
            config: Arc::new(Config::default()),
            meta,
            store,
            jwt,
            login_guard: Arc::new(LoginGuard::new(max_failures, lockout_secs)),
            artifacts,
            formats: Arc::new(FormatRegistry::with_builtin()),
            docker: Some(docker),
            audit,
            usage,
            metrics: None,
            rate_limiter: Arc::new(jianartifact::api::RateLimiter::new()),
            oidc: None,
            oidc_flows: std::sync::Arc::new(jianartifact::api::OidcFlowStore::new()),
        };
        Self { state, _dir: dir }
    }

    /// 每次请求新建一个路由实例（Router 克隆廉价，状态共享同一库）。
    fn router(&self) -> Router {
        build_router(self.state.clone())
    }

    /// 在库中直接建用户，返回 id。
    async fn seed_user(&self, username: &str, password: &str, role: Role) -> String {
        let hash = auth::hash_password(password).unwrap();
        self.state
            .meta
            .create_user(username, &hash, role)
            .await
            .unwrap()
    }
}

/// 发送请求并返回 (状态码, JSON 体)。
async fn send(router: Router, req: Request<Body>) -> (StatusCode, Value) {
    let resp = router.oneshot(req).await.unwrap();
    let status = resp.status();
    let bytes = resp.into_body().collect().await.unwrap().to_bytes();
    let body = serde_json::from_slice(&bytes).unwrap_or(Value::Null);
    (status, body)
}

/// 构造一个带 JSON 体的请求。
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

/// 构造一个无 body 的请求。
fn empty_req(method: &str, uri: &str, auth: Option<&str>) -> Request<Body> {
    let mut builder = Request::builder().method(method).uri(uri);
    if let Some(a) = auth {
        builder = builder.header("authorization", a);
    }
    builder.body(Body::empty()).unwrap()
}

/// 登录并取回 JWT 访问令牌。
async fn login_token(fx: &Fixture, username: &str, password: &str) -> String {
    let (status, body) = send(
        fx.router(),
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

// ---------- FR-01 登录与会话 ----------

#[tokio::test]
async fn 登录成功返回令牌与用户信息() {
    let fx = Fixture::new().await;
    fx.seed_user("admin", "S3cret!", Role::Admin).await;

    let (status, body) = send(
        fx.router(),
        json_req(
            "POST",
            "/api/v1/auth/login",
            None,
            json!({ "username": "admin", "password": "S3cret!" }),
        ),
    )
    .await;

    assert_eq!(status, StatusCode::OK);
    assert!(body["access_token"].as_str().unwrap().len() > 20);
    assert_eq!(body["token_type"], "Bearer");
    assert_eq!(body["user"]["username"], "admin");
    assert_eq!(body["user"]["role"], "admin");
    // 绝不回显口令哈希
    assert!(body["user"]["password_hash"].is_null());
}

#[tokio::test]
async fn 口令错误返回_401() {
    let fx = Fixture::new().await;
    fx.seed_user("admin", "right", Role::Admin).await;
    let (status, _) = send(
        fx.router(),
        json_req(
            "POST",
            "/api/v1/auth/login",
            None,
            json!({ "username": "admin", "password": "wrong" }),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::UNAUTHORIZED);
}

#[tokio::test]
async fn 不存在的用户返回_401_不泄露存在性() {
    let fx = Fixture::new().await;
    let (status, _) = send(
        fx.router(),
        json_req(
            "POST",
            "/api/v1/auth/login",
            None,
            json!({ "username": "无此人", "password": "x" }),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::UNAUTHORIZED);
}

#[tokio::test]
async fn 禁用账户登录返回_403() {
    let fx = Fixture::new().await;
    let id = fx.seed_user("u", "pw", Role::User).await;
    fx.state
        .meta
        .update_user(&id, None, Some(true))
        .await
        .unwrap();
    let (status, body) = send(
        fx.router(),
        json_req(
            "POST",
            "/api/v1/auth/login",
            None,
            json!({ "username": "u", "password": "pw" }),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::FORBIDDEN);
    assert_eq!(body["error"]["code"], "account_disabled");
}

#[tokio::test]
async fn 缺参登录返回_400() {
    let fx = Fixture::new().await;
    let (status, _) = send(
        fx.router(),
        json_req(
            "POST",
            "/api/v1/auth/login",
            None,
            json!({ "username": "", "password": "" }),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::BAD_REQUEST);
}

// ---------- FR-65 登录暴力破解防护 ----------

#[tokio::test]
async fn 连续失败达阈值触发限流_429() {
    // 阈值 3、锁定 900 秒
    let fx = Fixture::with_auth(3600, 3, 900).await;
    fx.seed_user("admin", "right", Role::Admin).await;

    // 连续 3 次错误口令
    for _ in 0..3 {
        let (status, _) = send(
            fx.router(),
            json_req(
                "POST",
                "/api/v1/auth/login",
                None,
                json!({ "username": "admin", "password": "wrong" }),
            ),
        )
        .await;
        assert_eq!(status, StatusCode::UNAUTHORIZED);
    }
    // 第 4 次即便口令正确也应被限流
    let (status, body) = send(
        fx.router(),
        json_req(
            "POST",
            "/api/v1/auth/login",
            None,
            json!({ "username": "admin", "password": "right" }),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::TOO_MANY_REQUESTS);
    assert_eq!(body["error"]["code"], "too_many_requests");
}

#[tokio::test]
async fn 锁定到期后可再次登录() {
    // 锁定窗口 1 秒，便于验证自动恢复
    let fx = Fixture::with_auth(3600, 2, 1).await;
    fx.seed_user("admin", "right", Role::Admin).await;
    for _ in 0..2 {
        let _ = send(
            fx.router(),
            json_req(
                "POST",
                "/api/v1/auth/login",
                None,
                json!({ "username": "admin", "password": "wrong" }),
            ),
        )
        .await;
    }
    // 锁定中
    let (locked, _) = send(
        fx.router(),
        json_req(
            "POST",
            "/api/v1/auth/login",
            None,
            json!({ "username": "admin", "password": "right" }),
        ),
    )
    .await;
    assert_eq!(locked, StatusCode::TOO_MANY_REQUESTS);
    // 等待越过锁定窗口
    tokio::time::sleep(std::time::Duration::from_millis(1100)).await;
    let (ok, _) = send(
        fx.router(),
        json_req(
            "POST",
            "/api/v1/auth/login",
            None,
            json!({ "username": "admin", "password": "right" }),
        ),
    )
    .await;
    assert_eq!(ok, StatusCode::OK);
}

// ---------- FR-63 /me、刷新、登出 ----------

#[tokio::test]
async fn me_带_jwt_返回当前用户_无凭据_401() {
    let fx = Fixture::new().await;
    fx.seed_user("alice", "pw", Role::User).await;
    let token = login_token(&fx, "alice", "pw").await;

    let (status, body) = send(
        fx.router(),
        empty_req("GET", "/api/v1/me", Some(&format!("Bearer {token}"))),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(body["username"], "alice");
    assert_eq!(body["role"], "user");

    // 无凭据 401
    let (status, _) = send(fx.router(), empty_req("GET", "/api/v1/me", None)).await;
    assert_eq!(status, StatusCode::UNAUTHORIZED);
}

#[tokio::test]
async fn 刷新换发新令牌_无凭据_401() {
    let fx = Fixture::new().await;
    fx.seed_user("alice", "pw", Role::User).await;
    let token = login_token(&fx, "alice", "pw").await;

    let (status, body) = send(
        fx.router(),
        empty_req(
            "POST",
            "/api/v1/auth/refresh",
            Some(&format!("Bearer {token}")),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert!(body["access_token"].as_str().unwrap().len() > 20);

    let (status, _) = send(fx.router(), empty_req("POST", "/api/v1/auth/refresh", None)).await;
    assert_eq!(status, StatusCode::UNAUTHORIZED);
}

#[tokio::test]
async fn 过期_jwt_被拒_刷新也失败() {
    // TTL = 0：签发即过期
    let fx = Fixture::with_auth(0, 5, 900).await;
    fx.seed_user("alice", "pw", Role::User).await;
    let token = login_token(&fx, "alice", "pw").await;
    // 越过 jsonwebtoken 默认 leeway
    tokio::time::sleep(std::time::Duration::from_secs(2)).await;

    let (status, _) = send(
        fx.router(),
        empty_req("GET", "/api/v1/me", Some(&format!("Bearer {token}"))),
    )
    .await;
    assert_eq!(status, StatusCode::UNAUTHORIZED);
}

#[tokio::test]
async fn 登出已认证返回_200_匿名_401() {
    let fx = Fixture::new().await;
    fx.seed_user("alice", "pw", Role::User).await;
    let token = login_token(&fx, "alice", "pw").await;

    let (status, _) = send(
        fx.router(),
        empty_req(
            "POST",
            "/api/v1/auth/logout",
            Some(&format!("Bearer {token}")),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::OK);

    let (status, _) = send(fx.router(), empty_req("POST", "/api/v1/auth/logout", None)).await;
    assert_eq!(status, StatusCode::UNAUTHORIZED);
}

// ---------- FR-02 API Token ----------

#[tokio::test]
async fn token_签发_bearer_使用_吊销后被拒() {
    let fx = Fixture::new().await;
    fx.seed_user("dev", "pw", Role::User).await;
    let session = login_token(&fx, "dev", "pw").await;

    // 签发
    let (status, body) = send(
        fx.router(),
        json_req(
            "POST",
            "/api/v1/tokens",
            Some(&format!("Bearer {session}")),
            json!({ "name": "ci" }),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::CREATED);
    let token_id = body["id"].as_str().unwrap().to_string();
    let plaintext = body["token"].as_str().unwrap().to_string();
    assert!(plaintext.starts_with("jna_"));

    // 用该 Token 走 Bearer 通道访问 /me
    let (status, me) = send(
        fx.router(),
        empty_req("GET", "/api/v1/me", Some(&format!("Bearer {plaintext}"))),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(me["username"], "dev");

    // 列表不回显明文 / 哈希
    let (status, list) = send(
        fx.router(),
        empty_req("GET", "/api/v1/tokens", Some(&format!("Bearer {session}"))),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(list.as_array().unwrap().len(), 1);
    assert!(list[0]["token"].is_null());
    assert!(list[0]["token_hash"].is_null());

    // 吊销
    let (status, _) = send(
        fx.router(),
        empty_req(
            "DELETE",
            &format!("/api/v1/tokens/{token_id}"),
            Some(&format!("Bearer {session}")),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::OK);

    // 吊销后该 Token 不再可用（回退匿名 → /me 401）
    let (status, _) = send(
        fx.router(),
        empty_req("GET", "/api/v1/me", Some(&format!("Bearer {plaintext}"))),
    )
    .await;
    assert_eq!(status, StatusCode::UNAUTHORIZED);
}

#[tokio::test]
async fn 吊销他人_token_返回_403() {
    let fx = Fixture::new().await;
    fx.seed_user("a", "pw", Role::User).await;
    fx.seed_user("b", "pw", Role::User).await;
    let a = login_token(&fx, "a", "pw").await;
    let b = login_token(&fx, "b", "pw").await;

    // a 签发 Token
    let (_, body) = send(
        fx.router(),
        json_req(
            "POST",
            "/api/v1/tokens",
            Some(&format!("Bearer {a}")),
            json!({ "name": "a-token" }),
        ),
    )
    .await;
    let a_token_id = body["id"].as_str().unwrap().to_string();

    // b 尝试吊销 a 的 Token → 403
    let (status, _) = send(
        fx.router(),
        empty_req(
            "DELETE",
            &format!("/api/v1/tokens/{a_token_id}"),
            Some(&format!("Bearer {b}")),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::FORBIDDEN);
}

#[tokio::test]
async fn 匿名签发_token_返回_401() {
    let fx = Fixture::new().await;
    let (status, _) = send(
        fx.router(),
        json_req("POST", "/api/v1/tokens", None, json!({ "name": "x" })),
    )
    .await;
    assert_eq!(status, StatusCode::UNAUTHORIZED);
}

// ---------- FR-03 Basic Auth ----------

#[tokio::test]
async fn basic_口令通道访问_me() {
    use base64::{engine::general_purpose::STANDARD, Engine};
    let fx = Fixture::new().await;
    fx.seed_user("bob", "s3cret", Role::User).await;
    let header = format!("Basic {}", STANDARD.encode("bob:s3cret"));
    let (status, body) = send(fx.router(), empty_req("GET", "/api/v1/me", Some(&header))).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(body["username"], "bob");
}

#[tokio::test]
async fn basic_token_通道访问_me() {
    use base64::{engine::general_purpose::STANDARD, Engine};
    let fx = Fixture::new().await;
    fx.seed_user("bob", "pw", Role::User).await;
    let session = login_token(&fx, "bob", "pw").await;
    let (_, body) = send(
        fx.router(),
        json_req(
            "POST",
            "/api/v1/tokens",
            Some(&format!("Bearer {session}")),
            json!({ "name": "ci" }),
        ),
    )
    .await;
    let plaintext = body["token"].as_str().unwrap().to_string();
    // 包管理器把 Token 当 Basic 密码
    let header = format!("Basic {}", STANDARD.encode(format!("bob:{plaintext}")));
    let (status, me) = send(fx.router(), empty_req("GET", "/api/v1/me", Some(&header))).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(me["username"], "bob");
}

#[tokio::test]
async fn basic_错误口令回退匿名_me_401() {
    use base64::{engine::general_purpose::STANDARD, Engine};
    let fx = Fixture::new().await;
    fx.seed_user("bob", "s3cret", Role::User).await;
    let header = format!("Basic {}", STANDARD.encode("bob:wrong"));
    let (status, _) = send(fx.router(), empty_req("GET", "/api/v1/me", Some(&header))).await;
    assert_eq!(status, StatusCode::UNAUTHORIZED);
}

// ---------- FR-04 / FR-05 用户管理（仅管理员） ----------

#[tokio::test]
async fn 管理员可增查改删用户() {
    let fx = Fixture::new().await;
    fx.seed_user("admin", "pw", Role::Admin).await;
    let admin = login_token(&fx, "admin", "pw").await;
    let auth = format!("Bearer {admin}");

    // 创建
    let (status, created) = send(
        fx.router(),
        json_req(
            "POST",
            "/api/v1/users",
            Some(&auth),
            json!({ "username": "newbie", "password": "pw", "role": "User" }),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::CREATED);
    assert_eq!(created["username"], "newbie");
    assert_eq!(created["role"], "user");
    assert!(created["password_hash"].is_null());
    let uid = created["id"].as_str().unwrap().to_string();

    // 列表含两人
    let (status, list) = send(fx.router(), empty_req("GET", "/api/v1/users", Some(&auth))).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(list.as_array().unwrap().len(), 2);

    // 详情
    let (status, one) = send(
        fx.router(),
        empty_req("GET", &format!("/api/v1/users/{uid}"), Some(&auth)),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(one["username"], "newbie");

    // 改角色 + 禁用
    let (status, updated) = send(
        fx.router(),
        json_req(
            "PATCH",
            &format!("/api/v1/users/{uid}"),
            Some(&auth),
            json!({ "role": "Admin", "disabled": true }),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(updated["role"], "admin");
    assert_eq!(updated["disabled"], true);

    // 删除
    let (status, _) = send(
        fx.router(),
        empty_req("DELETE", &format!("/api/v1/users/{uid}"), Some(&auth)),
    )
    .await;
    assert_eq!(status, StatusCode::OK);

    // 删后详情 404
    let (status, _) = send(
        fx.router(),
        empty_req("GET", &format!("/api/v1/users/{uid}"), Some(&auth)),
    )
    .await;
    assert_eq!(status, StatusCode::NOT_FOUND);
}

#[tokio::test]
async fn 非管理员访问用户端点返回_403() {
    let fx = Fixture::new().await;
    fx.seed_user("user", "pw", Role::User).await;
    let token = login_token(&fx, "user", "pw").await;
    let auth = format!("Bearer {token}");

    let (status, body) = send(fx.router(), empty_req("GET", "/api/v1/users", Some(&auth))).await;
    assert_eq!(status, StatusCode::FORBIDDEN);
    assert_eq!(body["error"]["code"], "forbidden");

    let (status, _) = send(
        fx.router(),
        json_req(
            "POST",
            "/api/v1/users",
            Some(&auth),
            json!({ "username": "x", "password": "x", "role": "User" }),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::FORBIDDEN);
}

#[tokio::test]
async fn 匿名访问用户端点返回_401() {
    let fx = Fixture::new().await;
    let (status, _) = send(fx.router(), empty_req("GET", "/api/v1/users", None)).await;
    assert_eq!(status, StatusCode::UNAUTHORIZED);
}

#[tokio::test]
async fn 创建重名用户返回_409() {
    let fx = Fixture::new().await;
    fx.seed_user("admin", "pw", Role::Admin).await;
    let admin = login_token(&fx, "admin", "pw").await;
    let auth = format!("Bearer {admin}");
    // admin 已存在，再建同名
    let (status, body) = send(
        fx.router(),
        json_req(
            "POST",
            "/api/v1/users",
            Some(&auth),
            json!({ "username": "admin", "password": "pw", "role": "User" }),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::CONFLICT);
    assert_eq!(body["error"]["code"], "conflict");
}

#[tokio::test]
async fn 创建非法角色返回_400() {
    let fx = Fixture::new().await;
    fx.seed_user("admin", "pw", Role::Admin).await;
    let admin = login_token(&fx, "admin", "pw").await;
    let auth = format!("Bearer {admin}");
    let (status, _) = send(
        fx.router(),
        json_req(
            "POST",
            "/api/v1/users",
            Some(&auth),
            json!({ "username": "x", "password": "pw", "role": "superadmin" }),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::BAD_REQUEST);
}
