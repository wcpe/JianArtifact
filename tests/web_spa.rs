//! Web 控制台 SPA 服务集成测试（FR-18~22 嵌入与回退）。
//!
//! 验证：① SPA 回退不拦截 API / 格式 / 健康检查 / Docker 端点；
//! ② 未知前端路由回退到入口（已构建为 index.html，未构建为 503 占位）；
//! ③ `/assets/*` 命中嵌入资源、未命中 404。
//!
//! 测试对“前端是否已构建”保持健壮：嵌入集合取决于编译期 `frontend/dist` 内容，
//! 故对入口断言只校验“非 404 且为 HTML”，不强依赖具体产物。

use std::sync::Arc;

use axum::body::Body;
use axum::http::{Request, StatusCode};
use http_body_util::BodyExt;
use tower::ServiceExt;

use jianartifact::api::{build_router, AppState};
use jianartifact::auth::{JwtSigner, LoginGuard};
use jianartifact::config::Config;
use jianartifact::format::{ArtifactService, DockerRegistry, FormatRegistry};
use jianartifact::meta::MetaStore;
use jianartifact::proxy::HttpUpstream;
use jianartifact::storage::LocalFsStore;

/// 构造测试用 AppState（内存库 + 临时 blob 目录）。
async fn 测试用状态() -> (AppState, tempfile::TempDir) {
    let dir = tempfile::tempdir().unwrap();
    let meta = MetaStore::open(&dir.path().join("test.db")).await.unwrap();
    let store = LocalFsStore::new(dir.path().join("blobs")).await.unwrap();
    let jwt = JwtSigner::from_secret(b"test-secret-32-bytes-xxxxxxxxxxxx", 3600);
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
    let state = AppState {
        config: Arc::new(Config::default()),
        meta,
        store,
        jwt,
        login_guard: Arc::new(LoginGuard::new(5, 900)),
        artifacts,
        formats: Arc::new(FormatRegistry::with_builtin()),
        docker: Some(docker),
    };
    (state, dir)
}

/// 取响应的 Content-Type 字符串。
fn content_type(resp: &axum::response::Response) -> String {
    resp.headers()
        .get(axum::http::header::CONTENT_TYPE)
        .and_then(|v| v.to_str().ok())
        .unwrap_or("")
        .to_string()
}

#[tokio::test]
async fn 根路径交由_spa_返回_html() {
    let (state, _dir) = 测试用状态().await;
    let app = build_router(state);
    let resp = app
        .oneshot(Request::builder().uri("/").body(Body::empty()).unwrap())
        .await
        .unwrap();
    // 已构建为 200 index.html；未构建为 503 占位页；均为 HTML、均非 404
    assert_ne!(resp.status(), StatusCode::NOT_FOUND);
    assert!(content_type(&resp).contains("text/html"));
}

#[tokio::test]
async fn 前端深链路由回退到_spa_而非_404() {
    let (state, _dir) = 测试用状态().await;
    let app = build_router(state);
    // 单段客户端路由（如 /repositories）刷新后应由 SPA 接管，不返回 404
    let resp = app
        .oneshot(
            Request::builder()
                .uri("/repositories")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_ne!(resp.status(), StatusCode::NOT_FOUND);
    assert!(content_type(&resp).contains("text/html"));
}

#[tokio::test]
async fn 不存在的静态资源返回_404() {
    let (state, _dir) = 测试用状态().await;
    let app = build_router(state);
    let resp = app
        .oneshot(
            Request::builder()
                .uri("/assets/不存在的资源-zzz.js")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::NOT_FOUND);
}

#[tokio::test]
async fn health_不被_spa_拦截() {
    let (state, _dir) = 测试用状态().await;
    let app = build_router(state);
    let resp = app
        .oneshot(
            Request::builder()
                .uri("/health")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::OK);
    let bytes = resp.into_body().collect().await.unwrap().to_bytes();
    let body: serde_json::Value = serde_json::from_slice(&bytes).unwrap();
    assert_eq!(body["status"], "ok");
}

#[tokio::test]
async fn 管理_api_不被_spa_拦截_未认证_401() {
    let (state, _dir) = 测试用状态().await;
    let app = build_router(state);
    let resp = app
        .oneshot(
            Request::builder()
                .uri("/api/v1/users")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    // 走 API 逻辑（未认证 401），而非被 SPA 回退成 200/503
    assert_eq!(resp.status(), StatusCode::UNAUTHORIZED);
}

#[tokio::test]
async fn docker_版本检查不被_spa_拦截() {
    let (state, _dir) = 测试用状态().await;
    let app = build_router(state);
    let resp = app
        .oneshot(Request::builder().uri("/v2/").body(Body::empty()).unwrap())
        .await
        .unwrap();
    // Docker registry v2 版本检查端点由 docker 路由处理：未带凭据时返回 401 + Bearer 质询
    // （发起认证发现），证明 `/v2/` 未被 SPA 回退拦截（否则会得到 HTML 而非该质询头）。
    assert_eq!(resp.status(), StatusCode::UNAUTHORIZED);
    let www = resp
        .headers()
        .get("www-authenticate")
        .and_then(|v| v.to_str().ok())
        .unwrap_or("");
    assert!(www.starts_with("Bearer "), "应为 Bearer 质询: {www}");
}
