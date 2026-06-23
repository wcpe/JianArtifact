//! API 层：axum 路由与中间件。本层保持轻薄，只做协议适配与错误转换，不写业务逻辑。
//!
//! 提供健康检查、认证（登录/登出/刷新/me）、用户管理、API Token 管理端点，
//! 以及统一识别 Bearer(JWT/Token)/Basic/匿名 的身份解析中间件。

use std::sync::Arc;

use axum::{
    extract::{FromRequestParts, State},
    http::request::Parts,
    http::StatusCode,
    middleware,
    response::{IntoResponse, Response},
    routing::{get, post},
    Json, Router,
};
use serde_json::json;
use tower_http::request_id::{MakeRequestUuid, PropagateRequestIdLayer, SetRequestIdLayer};
use tower_http::trace::TraceLayer;

use crate::auth::{AuthIdentity, JwtSigner, LoginGuard};
use crate::config::Config;
use crate::meta::MetaStore;
use crate::storage::LocalFsStore;

mod auth_routes;
mod identity;
mod repositories;
mod tokens;
mod users;

pub use identity::resolve_identity;

/// 请求 ID 头名称。
const REQUEST_ID_HEADER: &str = "x-request-id";

/// 应用共享状态：配置、元数据存储、blob 存储、JWT 签名器与登录防护守卫。
///
/// 用 Arc 包裹以便在各 handler 间廉价克隆共享。
#[derive(Clone)]
pub struct AppState {
    /// 运行期配置。
    pub config: Arc<Config>,
    /// 元数据存储（内部已是连接池，克隆廉价）。
    pub meta: MetaStore,
    /// blob 存储。
    pub store: LocalFsStore,
    /// JWT 会话签名器。
    pub jwt: JwtSigner,
    /// 登录暴力破解防护守卫（进程内存计数）。
    pub login_guard: Arc<LoginGuard>,
}

/// 统一 API 错误类型，转换为 JSON 响应 `{"error":{"code","message"}}`。
#[derive(Debug, thiserror::Error)]
pub enum ApiError {
    /// 请求体或参数不合法。
    #[error("{0}")]
    BadRequest(String),
    /// 未认证，或凭据无效 / 已吊销 / 已过期。
    #[error("未认证或凭据无效")]
    Unauthorized,
    /// 已认证但无权执行该操作（角色或 ACL 不足）。
    #[error("无权执行该操作")]
    Forbidden,
    /// 资源不存在。
    #[error("资源不存在")]
    NotFound,
    /// 资源冲突（如同名用户已存在）。
    #[error("{0}")]
    Conflict(String),
    /// 登录尝试过于频繁被限流，携带建议等待秒数。
    #[error("登录尝试过于频繁，请在 {0} 秒后重试")]
    TooManyRequests(u64),
    /// 账户已被禁用。
    #[error("账户已被禁用")]
    AccountDisabled,
    /// 内部服务器错误。
    #[error("内部服务器错误")]
    Internal,
}

impl ApiError {
    /// 返回该错误对应的 HTTP 状态码。
    fn status(&self) -> StatusCode {
        match self {
            ApiError::BadRequest(_) => StatusCode::BAD_REQUEST,
            ApiError::Unauthorized => StatusCode::UNAUTHORIZED,
            ApiError::Forbidden => StatusCode::FORBIDDEN,
            ApiError::NotFound => StatusCode::NOT_FOUND,
            ApiError::Conflict(_) => StatusCode::CONFLICT,
            ApiError::TooManyRequests(_) => StatusCode::TOO_MANY_REQUESTS,
            // 账户禁用沿用 API.md 约定的 403
            ApiError::AccountDisabled => StatusCode::FORBIDDEN,
            ApiError::Internal => StatusCode::INTERNAL_SERVER_ERROR,
        }
    }

    /// 返回该错误的稳定错误码（供客户端机器识别）。
    fn code(&self) -> &'static str {
        match self {
            ApiError::BadRequest(_) => "bad_request",
            ApiError::Unauthorized => "unauthorized",
            ApiError::Forbidden => "forbidden",
            ApiError::NotFound => "not_found",
            ApiError::Conflict(_) => "conflict",
            ApiError::TooManyRequests(_) => "too_many_requests",
            ApiError::AccountDisabled => "account_disabled",
            ApiError::Internal => "internal_error",
        }
    }
}

impl IntoResponse for ApiError {
    fn into_response(self) -> Response {
        let status = self.status();
        let body = Json(json!({
            "error": {
                "code": self.code(),
                "message": self.to_string(),
            }
        }));
        (status, body).into_response()
    }
}

/// 把元数据层错误统一映射为内部错误（细节不外泄给调用方）。
impl From<crate::meta::MetaError> for ApiError {
    fn from(e: crate::meta::MetaError) -> Self {
        // 记录详情到日志，对外仅暴露通用内部错误，避免泄露 SQL / 结构信息
        tracing::error!(错误 = %e, "元数据访问失败");
        ApiError::Internal
    }
}

/// 从请求扩展中取出已解析身份的提取器。
///
/// 身份由 `resolve_identity` 中间件预先注入；中间件总会注入（至少为匿名），
/// 故缺失视为内部错误。
pub struct Identity(pub AuthIdentity);

impl<S> FromRequestParts<S> for Identity
where
    S: Send + Sync,
{
    type Rejection = ApiError;

    async fn from_request_parts(parts: &mut Parts, _state: &S) -> Result<Self, Self::Rejection> {
        parts
            .extensions
            .get::<AuthIdentity>()
            .cloned()
            .map(Identity)
            .ok_or(ApiError::Internal)
    }
}

/// 客户端来源 IP 提取器：读取 `ConnectInfo<SocketAddr>`（生产由
/// `into_make_service_with_connect_info` 注入），缺失时回退占位（如单元测试）。
///
/// 本批按连接 IP 用于登录防护计数；XFF 仅在可信前置代理时才可采信，
/// 留待 P2 七层防护增强（见 lockout 模块说明）。
pub struct ClientIp(pub String);

impl<S> FromRequestParts<S> for ClientIp
where
    S: Send + Sync,
{
    type Rejection = std::convert::Infallible;

    async fn from_request_parts(parts: &mut Parts, _state: &S) -> Result<Self, Self::Rejection> {
        let ip = parts
            .extensions
            .get::<axum::extract::ConnectInfo<std::net::SocketAddr>>()
            .map(|ci| ci.0.ip().to_string())
            .unwrap_or_else(|| "unknown".to_string());
        Ok(ClientIp(ip))
    }
}

impl Identity {
    /// 要求已认证，否则 401；返回已认证用户。
    pub fn require_authenticated(&self) -> Result<&crate::auth::AuthUser, ApiError> {
        self.0.user().ok_or(ApiError::Unauthorized)
    }

    /// 要求管理员：未认证 401，已认证非管理员 403。
    pub fn require_admin(&self) -> Result<&crate::auth::AuthUser, ApiError> {
        let user = self.require_authenticated()?;
        if user.role == crate::meta::Role::Admin {
            Ok(user)
        } else {
            Err(ApiError::Forbidden)
        }
    }
}

/// 构建 axum 路由：挂健康检查、认证、用户、Token 端点与请求 ID、追踪、身份解析中间件。
pub fn build_router(state: AppState) -> Router {
    // 管理 API 子路由，统一挂在 /api/v1 前缀下
    let api_v1 = Router::new()
        .route("/auth/login", post(auth_routes::login))
        .route("/auth/logout", post(auth_routes::logout))
        .route("/auth/refresh", post(auth_routes::refresh))
        .route("/me", get(auth_routes::me))
        .route("/users", get(users::list_users).post(users::create_user))
        .route(
            "/users/{id}",
            get(users::get_user)
                .patch(users::update_user)
                .delete(users::delete_user),
        )
        .route("/tokens", get(tokens::list_tokens).post(tokens::create_token))
        .route("/tokens/{id}", axum::routing::delete(tokens::revoke_token))
        .route(
            "/repositories",
            get(repositories::list_repositories).post(repositories::create_repository),
        )
        .route(
            "/repositories/{id}",
            get(repositories::get_repository)
                .patch(repositories::update_repository)
                .delete(repositories::delete_repository),
        )
        .route(
            "/repositories/{id}/artifacts",
            get(repositories::list_artifacts),
        );

    Router::new()
        .route("/health", get(health))
        .nest("/api/v1", api_v1)
        .with_state(state.clone())
        // 身份解析中间件：先于业务 handler 解析 Bearer/Basic/匿名 注入扩展
        .layer(middleware::from_fn_with_state(state, identity::identity_layer))
        // 中间件顺序：设置请求 ID → 追踪 → 透传请求 ID 到响应
        .layer(PropagateRequestIdLayer::x_request_id())
        .layer(TraceLayer::new_for_http())
        .layer(SetRequestIdLayer::new(
            REQUEST_ID_HEADER.parse().expect("请求 ID 头名称合法"),
            MakeRequestUuid,
        ))
}

/// 健康检查处理器：无需认证，返回 200 与简单状态 JSON。
async fn health(State(state): State<AppState>) -> impl IntoResponse {
    Json(json!({
        "status": "ok",
        "version": env!("CARGO_PKG_VERSION"),
        // 不泄露敏感信息，仅回显服务监听端口供探活区分
        "port": state.config.server.port,
    }))
}

#[cfg(test)]
mod tests {
    use super::*;
    use axum::body::Body;
    use axum::http::Request;
    use http_body_util::BodyExt;
    use tower::ServiceExt;

    /// 构造测试用 AppState（内存库 + 临时 blob 目录 + 固定 JWT 密钥）。
    pub(crate) async fn 测试用状态() -> (AppState, tempfile::TempDir) {
        let dir = tempfile::tempdir().unwrap();
        let meta = MetaStore::open_in_memory().await.unwrap();
        let store = LocalFsStore::new(dir.path().join("blobs")).await.unwrap();
        let jwt = JwtSigner::from_secret(b"test-secret-32-bytes-xxxxxxxxxxxx", 3600);
        let state = AppState {
            config: Arc::new(Config::default()),
            meta,
            store,
            jwt,
            login_guard: Arc::new(LoginGuard::new(5, 900)),
        };
        (state, dir)
    }

    /// 便捷：读响应体为 JSON。
    pub(crate) async fn 读_json(resp: Response) -> serde_json::Value {
        let bytes = resp.into_body().collect().await.unwrap().to_bytes();
        serde_json::from_slice(&bytes).unwrap_or(serde_json::Value::Null)
    }

    #[tokio::test]
    async fn health_返回_200_与_ok状态() {
        let (state, _dir) = 测试用状态().await;
        let app = build_router(state);

        let response = app
            .oneshot(
                Request::builder()
                    .uri("/health")
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();

        assert_eq!(response.status(), StatusCode::OK);
        // 响应应带回请求 ID 头
        assert!(response.headers().contains_key(REQUEST_ID_HEADER));

        let body = 读_json(response).await;
        assert_eq!(body["status"], "ok");
        assert_eq!(body["port"], 8080);
    }

    #[tokio::test]
    async fn 未知路径返回_404() {
        let (state, _dir) = 测试用状态().await;
        let app = build_router(state);
        let response = app
            .oneshot(
                Request::builder()
                    .uri("/不存在")
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(response.status(), StatusCode::NOT_FOUND);
    }
}
