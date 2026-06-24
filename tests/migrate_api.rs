//! Nexus OSS 迁移在线入口端点的 HTTP 集成测试（FR-36，ADR-0006）。
//!
//! 重点覆盖鉴权边界（匿名 401 / 非管理员 403 / 管理员放行）与错误映射
//! （凭据引用缺失 400、连接源系统失败 502）。响应解析的正确性由 `migrate` 模块单测
//! 经 mock 穷举覆盖；本文件不依赖真实 Nexus 实例。

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

/// 构造带 JSON 体的迁移预览请求。
fn preview_req(auth: Option<&str>, body: Value) -> Request<Body> {
    let mut builder = Request::builder()
        .method("POST")
        .uri("/api/v1/migrate/nexus/preview")
        .header("content-type", "application/json");
    if let Some(a) = auth {
        builder = builder.header("authorization", a);
    }
    builder.body(Body::from(body.to_string())).unwrap()
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
