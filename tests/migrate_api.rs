//! Nexus OSS 迁移入口端点的 HTTP 集成测试（FR-36 / FR-37 / FR-39，ADR-0006）。
//!
//! 重点覆盖鉴权边界（匿名 401 / 非管理员 403 / 管理员放行）与错误映射
//! （在线：凭据引用缺失 400、连接源系统失败 502；离线：路径不存在 / 非法 400；
//! hosted 搬运：`offline_path` 为空 400）。响应 / 元数据解析与搬运编排（建仓 / blob 先落盘
//! 再写索引 / 覆盖语义 / 幂等）的正确性由 `migrate` 模块单测穷举覆盖；本文件不依赖真实 Nexus 实例。

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

/// 构造测试用状态（内存库 + 临时 blob 目录）。
async fn 测试用状态() -> (AppState, tempfile::TempDir) {
    let dir = tempfile::tempdir().unwrap();
    let meta = MetaStore::open(&dir.path().join("test.db")).await.unwrap();
    let store = BlobBackend::Fs(LocalFsStore::new(dir.path().join("blobs")).await.unwrap());
    let jwt = JwtSigner::from_secret(b"migrate-secret-32-bytes-xxxxxxxxx", 3600);
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
        settings: std::sync::Arc::new(
            jianartifact::config::EditableSettings::new(
                jianartifact::config::NetworkProxyConfig::default(),
                std::time::Duration::from_secs(60),
                &jianartifact::config::UpdateConfig::default(),
            )
            .unwrap(),
        ),
    };
    (state, dir)
}

/// 发送请求并返回 (状态码, JSON 体)。
async fn send(router: Router, req: Request<Body>) -> (StatusCode, Value) {
    let resp = router.oneshot(req).await.unwrap();
    let status = resp.status();
    let bytes = resp.into_body().collect().await.unwrap().to_bytes();
    let body = serde_json::from_slice(&bytes).unwrap_or(Value::Null);
    (status, body)
}

/// 构造带 JSON 体的在线迁移预览请求。
fn preview_req(auth: Option<&str>, body: Value) -> Request<Body> {
    json_post("/api/v1/migrate/nexus/preview", auth, body)
}

/// 构造带 JSON 体的离线 blob store 预览请求。
fn offline_req(auth: Option<&str>, body: Value) -> Request<Body> {
    json_post("/api/v1/migrate/nexus/offline/preview", auth, body)
}

/// 构造带 JSON 体的 hosted 仓库搬运请求。
fn hosted_migrate_req(auth: Option<&str>, body: Value) -> Request<Body> {
    json_post("/api/v1/migrate/nexus/hosted/migrate", auth, body)
}

/// 构造带 JSON 体的在线拉取迁移请求（FR-82）。
fn online_migrate_req(auth: Option<&str>, body: Value) -> Request<Body> {
    json_post("/api/v1/migrate/nexus/online/migrate", auth, body)
}

/// 构造带可选认证头的 JSON POST 请求。
fn json_post(uri: &str, auth: Option<&str>, body: Value) -> Request<Body> {
    let mut builder = Request::builder()
        .method("POST")
        .uri(uri)
        .header("content-type", "application/json");
    if let Some(a) = auth {
        builder = builder.header("authorization", a);
    }
    builder.body(Body::from(body.to_string())).unwrap()
}

/// 在临时目录下铺一个最小可用的 Nexus 文件型 blob store 布局，返回根目录临时句柄。
fn build_offline_store() -> tempfile::TempDir {
    let dir = tempfile::tempdir().unwrap();
    let chap = dir.path().join("content").join("vol-01").join("chap-01");
    std::fs::create_dir_all(&chap).unwrap();
    std::fs::write(
        chap.join("blob-1.properties"),
        "@Bucket.repo-name=maven-releases\n\
         @BlobStore.blob-name=/org/example/app/1.0/app-1.0.jar\n\
         sha1=a1b2c3\nsize=1024\ndeleted=false\n",
    )
    .unwrap();
    dir
}

/// 建用户并登录取回 JWT。
async fn seed_and_login(state: &AppState, username: &str, password: &str, role: Role) -> String {
    let hash = auth::hash_password(password).unwrap();
    state.meta.create_user(username, &hash, role).await.unwrap();
    let (status, body) = send(
        build_router(state.clone()),
        Request::builder()
            .method("POST")
            .uri("/api/v1/auth/login")
            .header("content-type", "application/json")
            .body(Body::from(
                json!({ "username": username, "password": password }).to_string(),
            ))
            .unwrap(),
    )
    .await;
    assert_eq!(status, StatusCode::OK, "登录应成功: {body}");
    body["access_token"].as_str().unwrap().to_string()
}

#[tokio::test]
async fn 匿名预览返回_401() {
    let (state, _dir) = 测试用状态().await;
    let (status, _) = send(
        build_router(state),
        preview_req(None, json!({ "base_url": "https://nexus.example" })),
    )
    .await;
    assert_eq!(status, StatusCode::UNAUTHORIZED);
}

#[tokio::test]
async fn 普通用户预览返回_403() {
    let (state, _dir) = 测试用状态().await;
    let token = seed_and_login(&state, "user", "pw", Role::User).await;
    let (status, _) = send(
        build_router(state),
        preview_req(
            Some(&format!("Bearer {token}")),
            json!({ "base_url": "https://nexus.example" }),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::FORBIDDEN);
}

#[tokio::test]
async fn 管理员带未配置凭据引用返回_400() {
    let (state, _dir) = 测试用状态().await;
    let token = seed_and_login(&state, "admin", "pw", Role::Admin).await;
    // auth_ref 指向一个不可能存在的环境变量名，应在调用源系统前以 400 短路
    let (status, body) = send(
        build_router(state),
        preview_req(
            Some(&format!("Bearer {token}")),
            json!({
                "base_url": "https://nexus.example",
                "auth_ref": "this_ref_is_not_configured_in_env_xyz"
            }),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::BAD_REQUEST, "body: {body}");
}

#[tokio::test]
async fn 管理员连接不可达源系统返回_502() {
    let (state, _dir) = 测试用状态().await;
    let token = seed_and_login(&state, "admin", "pw", Role::Admin).await;
    // 指向保留测试域名（不可达），连接失败应映射为 502 上游网关错误
    let (status, body) = send(
        build_router(state),
        preview_req(
            Some(&format!("Bearer {token}")),
            json!({ "base_url": "https://nexus.invalid.test.localhost.example" }),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::BAD_GATEWAY, "body: {body}");
}

#[tokio::test]
async fn 离线预览匿名返回_401() {
    let (state, _dir) = 测试用状态().await;
    let (status, _) = send(
        build_router(state),
        offline_req(None, json!({ "path": "/whatever" })),
    )
    .await;
    assert_eq!(status, StatusCode::UNAUTHORIZED);
}

#[tokio::test]
async fn 离线预览普通用户返回_403() {
    let (state, _dir) = 测试用状态().await;
    let token = seed_and_login(&state, "user", "pw", Role::User).await;
    let (status, _) = send(
        build_router(state),
        offline_req(
            Some(&format!("Bearer {token}")),
            json!({ "path": "/whatever" }),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::FORBIDDEN);
}

#[tokio::test]
async fn 离线预览管理员枚举有效_blob_store() {
    let (state, _dir) = 测试用状态().await;
    let token = seed_and_login(&state, "admin", "pw", Role::Admin).await;
    let store = build_offline_store();
    let (status, body) = send(
        build_router(state),
        offline_req(
            Some(&format!("Bearer {token}")),
            json!({ "path": store.path().to_str().unwrap() }),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::OK, "body: {body}");
    // 应枚举出 maven-releases 仓库及其下 1 个 blob
    assert_eq!(body[0]["repo_name"], "maven-releases");
    assert_eq!(body[0]["blob_count"], 1);
    assert_eq!(
        body[0]["blobs"][0]["blob_name"],
        "/org/example/app/1.0/app-1.0.jar"
    );
    assert_eq!(body[0]["blobs"][0]["sha1"], "a1b2c3");
    assert_eq!(body[0]["blobs"][0]["size"], 1024);
}

#[tokio::test]
async fn 离线预览路径不存在返回_400() {
    let (state, _dir) = 测试用状态().await;
    let token = seed_and_login(&state, "admin", "pw", Role::Admin).await;
    let (status, body) = send(
        build_router(state),
        offline_req(
            Some(&format!("Bearer {token}")),
            json!({ "path": "D:/__此路径必定不存在的_blob_store__/x" }),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::BAD_REQUEST, "body: {body}");
}

#[tokio::test]
async fn 离线预览空路径返回_400() {
    let (state, _dir) = 测试用状态().await;
    let token = seed_and_login(&state, "admin", "pw", Role::Admin).await;
    let (status, body) = send(
        build_router(state),
        offline_req(Some(&format!("Bearer {token}")), json!({ "path": "   " })),
    )
    .await;
    assert_eq!(status, StatusCode::BAD_REQUEST, "body: {body}");
}

// ---------- hosted 仓库搬运端点（FR-39）鉴权与校验边界 ----------

#[tokio::test]
async fn hosted_搬运匿名返回_401() {
    let (state, _dir) = 测试用状态().await;
    let (status, _) = send(
        build_router(state),
        hosted_migrate_req(
            None,
            json!({ "base_url": "https://nexus.example", "offline_path": "/whatever" }),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::UNAUTHORIZED);
}

#[tokio::test]
async fn hosted_搬运普通用户返回_403() {
    let (state, _dir) = 测试用状态().await;
    let token = seed_and_login(&state, "user", "pw", Role::User).await;
    let (status, _) = send(
        build_router(state),
        hosted_migrate_req(
            Some(&format!("Bearer {token}")),
            json!({ "base_url": "https://nexus.example", "offline_path": "/whatever" }),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::FORBIDDEN);
}

#[tokio::test]
async fn hosted_搬运管理员空_offline_path_返回_400() {
    let (state, _dir) = 测试用状态().await;
    let token = seed_and_login(&state, "admin", "pw", Role::Admin).await;
    // offline_path 为空白应在调用源系统前以 400 短路
    let (status, body) = send(
        build_router(state),
        hosted_migrate_req(
            Some(&format!("Bearer {token}")),
            json!({ "base_url": "https://nexus.example", "offline_path": "   " }),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::BAD_REQUEST, "body: {body}");
}

#[tokio::test]
async fn 匿名在线迁移返回_401() {
    let (state, _dir) = 测试用状态().await;
    let (status, _) = send(
        build_router(state),
        online_migrate_req(
            None,
            json!({ "base_url": "https://nexus.example", "repositories": [{ "source": "r3d" }] }),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::UNAUTHORIZED);
}

#[tokio::test]
async fn 普通用户在线迁移返回_403() {
    let (state, _dir) = 测试用状态().await;
    let token = seed_and_login(&state, "user", "pw", Role::User).await;
    let (status, _) = send(
        build_router(state),
        online_migrate_req(
            Some(&format!("Bearer {token}")),
            json!({ "base_url": "https://nexus.example", "repositories": [{ "source": "r3d" }] }),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::FORBIDDEN);
}

#[tokio::test]
async fn 在线迁移管理员空仓库列表返回_400() {
    let (state, _dir) = 测试用状态().await;
    let token = seed_and_login(&state, "admin", "pw", Role::Admin).await;
    // 未选择仓库应在调用源系统前以 400 短路
    let (status, body) = send(
        build_router(state),
        online_migrate_req(
            Some(&format!("Bearer {token}")),
            json!({ "base_url": "https://nexus.example", "repositories": [] }),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::BAD_REQUEST, "body: {body}");
}

/// 构造带可选认证头的 GET 请求。
fn get_req(uri: &str, auth: Option<&str>) -> Request<Body> {
    let mut builder = Request::builder().method("GET").uri(uri);
    if let Some(a) = auth {
        builder = builder.header("authorization", a);
    }
    builder.body(Body::empty()).unwrap()
}

#[tokio::test]
async fn 匿名查询迁移任务返回_401() {
    let (state, _dir) = 测试用状态().await;
    let (status, _) = send(
        build_router(state),
        get_req("/api/v1/migrate/jobs/any", None),
    )
    .await;
    assert_eq!(status, StatusCode::UNAUTHORIZED);
}

#[tokio::test]
async fn 普通用户查询迁移任务返回_403() {
    let (state, _dir) = 测试用状态().await;
    let token = seed_and_login(&state, "user", "pw", Role::User).await;
    let (status, _) = send(
        build_router(state),
        get_req("/api/v1/migrate/jobs/any", Some(&format!("Bearer {token}"))),
    )
    .await;
    assert_eq!(status, StatusCode::FORBIDDEN);
}

#[tokio::test]
async fn 管理员查询未知迁移任务返回_404() {
    let (state, _dir) = 测试用状态().await;
    let token = seed_and_login(&state, "admin", "pw", Role::Admin).await;
    let (status, _) = send(
        build_router(state),
        get_req(
            "/api/v1/migrate/jobs/不存在",
            Some(&format!("Bearer {token}")),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::NOT_FOUND);
}

#[tokio::test]
async fn 匿名列迁移任务返回_401() {
    let (state, _dir) = 测试用状态().await;
    let (status, _) = send(build_router(state), get_req("/api/v1/migrate/jobs", None)).await;
    assert_eq!(status, StatusCode::UNAUTHORIZED);
}

#[tokio::test]
async fn 管理员列迁移任务空表返回_200() {
    let (state, _dir) = 测试用状态().await;
    let token = seed_and_login(&state, "admin", "pw", Role::Admin).await;
    let (status, body) = send(
        build_router(state),
        get_req("/api/v1/migrate/jobs", Some(&format!("Bearer {token}"))),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(body, json!([]));
}
