//! 仓库模型与授权层的 HTTP 集成测试（FR-06/07/08/09/10/13）。
//!
//! 重点穷举鉴权判定矩阵（#1 高风险区）：可见性 × 角色 × ACL × 操作，并三身份通道
//! （Bearer-JWT / Bearer-Token / Basic）各走一遍读浏览；验证私有对匿名 / 无权一律 404
//! 隐藏存在性、写权限边界、CRUD 与 ACL 的 Admin-only、列表按身份过滤。

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
use jianartifact::meta::{MetaStore, Permission, Role};
use jianartifact::proxy::HttpUpstream;
use jianartifact::storage::{BlobBackend, LocalFsStore};

/// 测试夹具：持有可重复构建路由的状态与临时目录。
struct Fixture {
    state: AppState,
    _dir: tempfile::TempDir,
}

impl Fixture {
    /// 构造夹具（走真实 SQLite 文件，验证迁移与外键级联）。
    async fn new() -> Self {
        let dir = tempfile::tempdir().unwrap();
        let meta = MetaStore::open(&dir.path().join("test.db")).await.unwrap();
        let store = BlobBackend::Fs(LocalFsStore::new(dir.path().join("blobs")).await.unwrap());
        let jwt = JwtSigner::from_secret(b"repo-authz-secret-32-bytes-xxxxxx", 3600);
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
            restart: std::sync::Arc::new(jianartifact::update::RestartHandle::default()),
        };
        Self { state, _dir: dir }
    }

    /// 每次请求新建一个路由实例（状态共享同一库）。
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

    /// 直接建仓库（绕过 API，便于布置矩阵前置），返回 id。
    async fn seed_repo(&self, name: &str, visibility: jianartifact::meta::Visibility) -> String {
        self.state
            .meta
            .create_repository(jianartifact::meta::NewRepository {
                name,
                format: "raw",
                r#type: jianartifact::meta::RepoType::Hosted,
                visibility,
                upstream_url: None,
                upstream_auth_ref: None,
            })
            .await
            .unwrap()
    }

    /// 直接授予 ACL。
    async fn seed_acl(&self, repo_id: &str, user_id: &str, permission: Permission) {
        self.state
            .meta
            .create_acl(repo_id, user_id, permission)
            .await
            .unwrap();
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

/// 构造带 JSON 体的请求。
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

/// 构造无 body 的请求。
fn empty_req(method: &str, uri: &str, auth: Option<&str>) -> Request<Body> {
    let mut builder = Request::builder().method(method).uri(uri);
    if let Some(a) = auth {
        builder = builder.header("authorization", a);
    }
    builder.body(Body::empty()).unwrap()
}

/// 登录取回 JWT。
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

use jianartifact::meta::Visibility;

// ---------- FR-10 仓库 CRUD（Admin-only） ----------

#[tokio::test]
async fn 管理员可创建查改删仓库() {
    let fx = Fixture::new().await;
    fx.seed_user("admin", "pw", Role::Admin).await;
    let token = login_token(&fx, "admin", "pw").await;
    let auth = format!("Bearer {token}");

    // 创建 hosted public
    let (status, created) = send(
        fx.router(),
        json_req(
            "POST",
            "/api/v1/repositories",
            Some(&auth),
            json!({ "name": "libs", "format": "Maven", "type": "hosted", "visibility": "public" }),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::CREATED);
    assert_eq!(created["name"], "libs");
    assert_eq!(created["format"], "maven");
    assert_eq!(created["type"], "hosted");
    assert_eq!(created["visibility"], "public");
    // 不回显凭据引用
    assert!(created["upstream_auth_ref"].is_null());
    let rid = created["id"].as_str().unwrap().to_string();

    // 详情
    let (status, one) = send(
        fx.router(),
        empty_req("GET", &format!("/api/v1/repositories/{rid}"), Some(&auth)),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(one["name"], "libs");

    // 改可见性
    let (status, updated) = send(
        fx.router(),
        json_req(
            "PATCH",
            &format!("/api/v1/repositories/{rid}"),
            Some(&auth),
            json!({ "visibility": "private" }),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(updated["visibility"], "private");

    // 删除
    let (status, _) = send(
        fx.router(),
        empty_req(
            "DELETE",
            &format!("/api/v1/repositories/{rid}"),
            Some(&auth),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::OK);

    // 删后详情 404
    let (status, _) = send(
        fx.router(),
        empty_req("GET", &format!("/api/v1/repositories/{rid}"), Some(&auth)),
    )
    .await;
    assert_eq!(status, StatusCode::NOT_FOUND);
}

#[tokio::test]
async fn 创建_proxy_缺上游地址_400() {
    let fx = Fixture::new().await;
    fx.seed_user("admin", "pw", Role::Admin).await;
    let auth = format!("Bearer {}", login_token(&fx, "admin", "pw").await);
    let (status, _) = send(
        fx.router(),
        json_req(
            "POST",
            "/api/v1/repositories",
            Some(&auth),
            json!({ "name": "mirror", "format": "npm", "type": "proxy", "visibility": "public" }),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::BAD_REQUEST);
}

#[tokio::test]
async fn 创建非法格式_400() {
    let fx = Fixture::new().await;
    fx.seed_user("admin", "pw", Role::Admin).await;
    let auth = format!("Bearer {}", login_token(&fx, "admin", "pw").await);
    let (status, _) = send(
        fx.router(),
        json_req(
            "POST",
            "/api/v1/repositories",
            Some(&auth),
            json!({ "name": "x", "format": "rubygems", "type": "hosted", "visibility": "public" }),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::BAD_REQUEST);
}

#[tokio::test]
async fn 创建重名仓库_409() {
    let fx = Fixture::new().await;
    fx.seed_user("admin", "pw", Role::Admin).await;
    let auth = format!("Bearer {}", login_token(&fx, "admin", "pw").await);
    fx.seed_repo("dup", Visibility::Public).await;
    let (status, body) = send(
        fx.router(),
        json_req(
            "POST",
            "/api/v1/repositories",
            Some(&auth),
            json!({ "name": "dup", "format": "raw", "type": "hosted", "visibility": "public" }),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::CONFLICT);
    assert_eq!(body["error"]["code"], "conflict");
}

#[tokio::test]
async fn 非管理员_crud_仓库_403_匿名_401() {
    let fx = Fixture::new().await;
    fx.seed_user("user", "pw", Role::User).await;
    let user = format!("Bearer {}", login_token(&fx, "user", "pw").await);
    let rid = fx.seed_repo("r", Visibility::Public).await;

    // 普通用户创建 / 改 / 删 → 403
    let (status, body) = send(
        fx.router(),
        json_req(
            "POST",
            "/api/v1/repositories",
            Some(&user),
            json!({ "name": "x", "format": "raw", "type": "hosted", "visibility": "public" }),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::FORBIDDEN);
    assert_eq!(body["error"]["code"], "forbidden");

    let (status, _) = send(
        fx.router(),
        json_req(
            "PATCH",
            &format!("/api/v1/repositories/{rid}"),
            Some(&user),
            json!({ "visibility": "private" }),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::FORBIDDEN);

    let (status, _) = send(
        fx.router(),
        empty_req(
            "DELETE",
            &format!("/api/v1/repositories/{rid}"),
            Some(&user),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::FORBIDDEN);

    // 匿名创建 → 401
    let (status, _) = send(
        fx.router(),
        json_req(
            "POST",
            "/api/v1/repositories",
            None,
            json!({ "name": "x", "format": "raw", "type": "hosted", "visibility": "public" }),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::UNAUTHORIZED);
}

// ---------- FR-08 私有仓库对匿名 / 无权一律 404（不泄露存在性） ----------

#[tokio::test]
async fn 私有仓库对匿名详情与浏览均_404() {
    let fx = Fixture::new().await;
    let rid = fx.seed_repo("secret", Visibility::Private).await;

    let (status, body) = send(
        fx.router(),
        empty_req("GET", &format!("/api/v1/repositories/{rid}"), None),
    )
    .await;
    assert_eq!(status, StatusCode::NOT_FOUND);
    assert_eq!(body["error"]["code"], "not_found");

    let (status, _) = send(
        fx.router(),
        empty_req(
            "GET",
            &format!("/api/v1/repositories/{rid}/artifacts"),
            None,
        ),
    )
    .await;
    assert_eq!(status, StatusCode::NOT_FOUND);
}

#[tokio::test]
async fn 私有仓库对无_acl_登录用户_404() {
    let fx = Fixture::new().await;
    fx.seed_user("nobody", "pw", Role::User).await;
    let auth = format!("Bearer {}", login_token(&fx, "nobody", "pw").await);
    let rid = fx.seed_repo("secret", Visibility::Private).await;

    let (status, _) = send(
        fx.router(),
        empty_req("GET", &format!("/api/v1/repositories/{rid}"), Some(&auth)),
    )
    .await;
    assert_eq!(status, StatusCode::NOT_FOUND);
}

#[tokio::test]
async fn 不存在的仓库返回_404() {
    let fx = Fixture::new().await;
    let (status, _) = send(
        fx.router(),
        empty_req("GET", "/api/v1/repositories/无此仓库", None),
    )
    .await;
    assert_eq!(status, StatusCode::NOT_FOUND);
}

// ---------- 公开仓库匿名可读 ----------

#[tokio::test]
async fn 公开仓库匿名可读详情与浏览() {
    let fx = Fixture::new().await;
    let rid = fx.seed_repo("public-libs", Visibility::Public).await;

    let (status, body) = send(
        fx.router(),
        empty_req("GET", &format!("/api/v1/repositories/{rid}"), None),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(body["name"], "public-libs");

    let (status, list) = send(
        fx.router(),
        empty_req(
            "GET",
            &format!("/api/v1/repositories/{rid}/artifacts"),
            None,
        ),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    // 当前批次无制品写入路径，索引为空数组
    assert_eq!(list.as_array().unwrap().len(), 0);
}

// ---------- FR-13 列表按身份过滤 ----------

#[tokio::test]
async fn 列表按身份过滤无权私有仓库() {
    let fx = Fixture::new().await;
    let alice = fx.seed_user("alice", "pw", Role::User).await;
    fx.seed_user("admin", "pw", Role::Admin).await;
    let _pub_id = fx.seed_repo("pub", Visibility::Public).await;
    let priv_a = fx.seed_repo("priv-a", Visibility::Private).await;
    let _priv_b = fx.seed_repo("priv-b", Visibility::Private).await;
    // alice 仅对 priv-a 有读权限
    fx.seed_acl(&priv_a, &alice, Permission::Read).await;

    // 匿名只见 public
    let (status, anon) = send(fx.router(), empty_req("GET", "/api/v1/repositories", None)).await;
    assert_eq!(status, StatusCode::OK);
    let anon_names: Vec<&str> = anon
        .as_array()
        .unwrap()
        .iter()
        .map(|r| r["name"].as_str().unwrap())
        .collect();
    assert_eq!(anon_names, vec!["pub"]);

    // alice 见 public + 自己有读权限的 priv-a，不见 priv-b
    let alice_auth = format!("Bearer {}", login_token(&fx, "alice", "pw").await);
    let (_, alice_list) = send(
        fx.router(),
        empty_req("GET", "/api/v1/repositories", Some(&alice_auth)),
    )
    .await;
    let mut alice_names: Vec<&str> = alice_list
        .as_array()
        .unwrap()
        .iter()
        .map(|r| r["name"].as_str().unwrap())
        .collect();
    alice_names.sort();
    assert_eq!(alice_names, vec!["priv-a", "pub"]);

    // 管理员见全部三个
    let admin_auth = format!("Bearer {}", login_token(&fx, "admin", "pw").await);
    let (_, admin_list) = send(
        fx.router(),
        empty_req("GET", "/api/v1/repositories", Some(&admin_auth)),
    )
    .await;
    assert_eq!(admin_list.as_array().unwrap().len(), 3);
}

#[tokio::test]
async fn 仅写_acl_用户列表可见私有仓库() {
    // 写权限蕴含读权限，列表过滤应把仅 write 的用户也算可见
    let fx = Fixture::new().await;
    let bob = fx.seed_user("bob", "pw", Role::User).await;
    let priv_id = fx.seed_repo("priv", Visibility::Private).await;
    fx.seed_acl(&priv_id, &bob, Permission::Write).await;
    let auth = format!("Bearer {}", login_token(&fx, "bob", "pw").await);
    let (_, list) = send(
        fx.router(),
        empty_req("GET", "/api/v1/repositories", Some(&auth)),
    )
    .await;
    assert_eq!(list.as_array().unwrap().len(), 1);
    assert_eq!(list[0]["name"], "priv");
}

// ---------- FR-07 ACL CRUD（Admin-only） ----------

#[tokio::test]
async fn 管理员可列增删_acl() {
    let fx = Fixture::new().await;
    fx.seed_user("admin", "pw", Role::Admin).await;
    let target = fx.seed_user("dev", "pw", Role::User).await;
    let auth = format!("Bearer {}", login_token(&fx, "admin", "pw").await);
    let rid = fx.seed_repo("r", Visibility::Private).await;

    // 新增 read
    let (status, created) = send(
        fx.router(),
        json_req(
            "POST",
            &format!("/api/v1/repositories/{rid}/acl"),
            Some(&auth),
            json!({ "user_id": target, "permission": "read" }),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::CREATED);
    assert_eq!(created["permission"], "read");
    let acl_id = created["id"].as_str().unwrap().to_string();

    // 重复授 read → 409
    let (status, body) = send(
        fx.router(),
        json_req(
            "POST",
            &format!("/api/v1/repositories/{rid}/acl"),
            Some(&auth),
            json!({ "user_id": target, "permission": "read" }),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::CONFLICT);
    assert_eq!(body["error"]["code"], "conflict");

    // 列表含一条
    let (status, list) = send(
        fx.router(),
        empty_req(
            "GET",
            &format!("/api/v1/repositories/{rid}/acl"),
            Some(&auth),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(list.as_array().unwrap().len(), 1);

    // 删除
    let (status, _) = send(
        fx.router(),
        empty_req(
            "DELETE",
            &format!("/api/v1/repositories/{rid}/acl/{acl_id}"),
            Some(&auth),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    let (_, list) = send(
        fx.router(),
        empty_req(
            "GET",
            &format!("/api/v1/repositories/{rid}/acl"),
            Some(&auth),
        ),
    )
    .await;
    assert_eq!(list.as_array().unwrap().len(), 0);
}

#[tokio::test]
async fn acl_对不存在用户或仓库_404() {
    let fx = Fixture::new().await;
    fx.seed_user("admin", "pw", Role::Admin).await;
    let auth = format!("Bearer {}", login_token(&fx, "admin", "pw").await);
    let rid = fx.seed_repo("r", Visibility::Private).await;

    // 用户不存在
    let (status, _) = send(
        fx.router(),
        json_req(
            "POST",
            &format!("/api/v1/repositories/{rid}/acl"),
            Some(&auth),
            json!({ "user_id": "无此用户", "permission": "read" }),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::NOT_FOUND);

    // 仓库不存在
    let (status, _) = send(
        fx.router(),
        empty_req("GET", "/api/v1/repositories/无此仓库/acl", Some(&auth)),
    )
    .await;
    assert_eq!(status, StatusCode::NOT_FOUND);
}

#[tokio::test]
async fn 非管理员访问_acl_端点_403_匿名_401() {
    let fx = Fixture::new().await;
    fx.seed_user("user", "pw", Role::User).await;
    let user = format!("Bearer {}", login_token(&fx, "user", "pw").await);
    let rid = fx.seed_repo("r", Visibility::Private).await;

    let (status, _) = send(
        fx.router(),
        empty_req(
            "GET",
            &format!("/api/v1/repositories/{rid}/acl"),
            Some(&user),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::FORBIDDEN);

    let (status, _) = send(
        fx.router(),
        empty_req("GET", &format!("/api/v1/repositories/{rid}/acl"), None),
    )
    .await;
    assert_eq!(status, StatusCode::UNAUTHORIZED);
}

// ---------- FR-13 浏览制品的读权限矩阵（端到端补 pure 函数） ----------

#[tokio::test]
async fn 私有仓库读_acl_用户可浏览_无权_404() {
    let fx = Fixture::new().await;
    let reader = fx.seed_user("reader", "pw", Role::User).await;
    fx.seed_user("outsider", "pw", Role::User).await;
    let rid = fx.seed_repo("priv", Visibility::Private).await;
    fx.seed_acl(&rid, &reader, Permission::Read).await;

    // 有读 ACL → 200
    let reader_auth = format!("Bearer {}", login_token(&fx, "reader", "pw").await);
    let (status, _) = send(
        fx.router(),
        empty_req(
            "GET",
            &format!("/api/v1/repositories/{rid}/artifacts"),
            Some(&reader_auth),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::OK);

    // 无 ACL → 404
    let out_auth = format!("Bearer {}", login_token(&fx, "outsider", "pw").await);
    let (status, _) = send(
        fx.router(),
        empty_req(
            "GET",
            &format!("/api/v1/repositories/{rid}/artifacts"),
            Some(&out_auth),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::NOT_FOUND);
}

// ---------- 三身份通道各走一遍私有读浏览（避免某通道绕过） ----------

#[tokio::test]
async fn 三身份通道对私有仓库读判定一致() {
    let fx = Fixture::new().await;
    let reader = fx.seed_user("reader", "pw", Role::User).await;
    let rid = fx.seed_repo("priv", Visibility::Private).await;
    fx.seed_acl(&rid, &reader, Permission::Read).await;
    let uri = format!("/api/v1/repositories/{rid}");

    // 通道一：Bearer-JWT
    let jwt = login_token(&fx, "reader", "pw").await;
    let (status, _) = send(
        fx.router(),
        empty_req("GET", &uri, Some(&format!("Bearer {jwt}"))),
    )
    .await;
    assert_eq!(status, StatusCode::OK, "JWT 通道");

    // 通道二：Bearer API Token
    let plaintext = jianartifact::auth::generate_api_token();
    fx.state
        .meta
        .create_token(
            &reader,
            "ci",
            &jianartifact::auth::hash_api_token(&plaintext),
        )
        .await
        .unwrap();
    let (status, _) = send(
        fx.router(),
        empty_req("GET", &uri, Some(&format!("Bearer {plaintext}"))),
    )
    .await;
    assert_eq!(status, StatusCode::OK, "API Token 通道");

    // 通道三：Basic（用户名 + 口令）
    let basic = format!("Basic {}", STANDARD.encode("reader:pw"));
    let (status, _) = send(fx.router(), empty_req("GET", &uri, Some(&basic))).await;
    assert_eq!(status, StatusCode::OK, "Basic 通道");

    // 同样三通道下，无权用户经各通道均 404（构造一个 outsider）
    let outsider = fx.seed_user("outsider", "pw", Role::User).await;
    let out_plain = jianartifact::auth::generate_api_token();
    fx.state
        .meta
        .create_token(
            &outsider,
            "ci",
            &jianartifact::auth::hash_api_token(&out_plain),
        )
        .await
        .unwrap();
    let out_jwt = login_token(&fx, "outsider", "pw").await;
    let out_basic = format!("Basic {}", STANDARD.encode("outsider:pw"));
    for (name, header) in [
        ("JWT", format!("Bearer {out_jwt}")),
        ("Token", format!("Bearer {out_plain}")),
        ("Basic", out_basic),
    ] {
        let (status, _) = send(fx.router(), empty_req("GET", &uri, Some(&header))).await;
        assert_eq!(status, StatusCode::NOT_FOUND, "无权 {name} 通道应 404");
    }
}
