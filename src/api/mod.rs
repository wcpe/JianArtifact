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
use crate::format::{ArtifactService, DockerRegistry, FormatRegistry};
use crate::meta::MetaStore;
use crate::proxy::HttpUpstream;
use crate::storage::LocalFsStore;

mod acl;
mod artifacts;
mod auth_routes;
mod cargo_routes;
mod docker_routes;
mod format_routes;
mod go_routes;
mod identity;
mod npm_routes;
mod pypi_routes;
mod nuget_routes;
mod repo_access;
mod repositories;
mod search;
mod tokens;
mod users;

pub use identity::resolve_identity;

/// 应用内具体化的通用制品机理服务类型（本地 blob 存储 + reqwest 上游）。
pub type AppArtifactService = ArtifactService<LocalFsStore, HttpUpstream>;

/// 应用内具体化的 Docker Registry v2 存储服务类型（本地 blob 存储）。
pub type AppDockerRegistry = DockerRegistry<LocalFsStore>;

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
    /// 通用制品机理服务（写入 / 读取 / 删除，含 proxy 单飞缓存）。
    pub artifacts: Arc<AppArtifactService>,
    /// 格式注册表（按格式名查处理器，多态分发）。
    pub formats: Arc<FormatRegistry>,
    /// Docker Registry v2 存储服务（blob 上传状态机 + manifest 存取）。
    pub docker: Option<Arc<AppDockerRegistry>>,
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
    /// 上传体积超过配置上限（FR-64）。
    #[error("制品体积超过上限")]
    PayloadTooLarge,
    /// 上游网关错误（proxy 回源失败 / 超时）。
    #[error("上游拉取失败")]
    BadGateway,
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
            ApiError::PayloadTooLarge => StatusCode::PAYLOAD_TOO_LARGE,
            ApiError::BadGateway => StatusCode::BAD_GATEWAY,
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
            ApiError::PayloadTooLarge => "payload_too_large",
            ApiError::BadGateway => "bad_gateway",
            ApiError::Internal => "internal_error",
        }
    }
}

/// 把通用制品机理错误映射为 HTTP 错误。
impl From<crate::format::ServiceError> for ApiError {
    fn from(e: crate::format::ServiceError) -> Self {
        use crate::format::ServiceError;
        match e {
            ServiceError::NotFound => ApiError::NotFound,
            // 覆盖被拒按各格式语义对应 409（如 Maven release 不可覆盖）
            ServiceError::OverwriteForbidden => {
                ApiError::Conflict("制品已存在且不允许覆盖".to_string())
            }
            ServiceError::TooLarge => ApiError::PayloadTooLarge,
            ServiceError::Upstream => ApiError::BadGateway,
            ServiceError::InvalidOperation(msg) => ApiError::BadRequest(msg),
            ServiceError::Storage(err) => {
                tracing::error!(错误 = %err, "blob 存储访问失败");
                ApiError::Internal
            }
            ServiceError::Meta(err) => err.into(),
        }
    }
}

/// 把格式路径解析错误映射为 400（路径非法 / 穿越）。
impl From<crate::format::PathError> for ApiError {
    fn from(e: crate::format::PathError) -> Self {
        ApiError::BadRequest(e.to_string())
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
        .route(
            "/tokens",
            get(tokens::list_tokens).post(tokens::create_token),
        )
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
        )
        .route(
            "/repositories/{id}/artifacts/{*path}",
            get(artifacts::get_artifact_detail).delete(artifacts::delete_artifact),
        )
        .route("/search", get(search::search))
        .route(
            "/repositories/{id}/acl",
            get(acl::list_acl).post(acl::create_acl),
        )
        .route(
            "/repositories/{id}/acl/{acl_id}",
            axum::routing::delete(acl::delete_acl),
        );

    // 格式 API：按原生协议挂载，路径含仓库名（如 Raw 的 /{repo}/{path..}）。
    // 用 catch-all 段匹配仓库内任意路径；axum 优先匹配 /health 与 /api/v1 等字面前缀。
    // PyPI twine 上传目标为 `POST /{repo}/`（空路径，catch-all 不匹配），故单列其路由；
    // `POST /{repo}/{*path}` 兜底 PyPI 的 `legacy/` 等带路径上传形态。
    let format_api = Router::new()
        .route("/{repo}/", post(format_routes::post_artifact_root))
        .route(
            "/{repo}/{*path}",
            get(format_routes::get_artifact)
                .put(format_routes::put_artifact)
                .post(format_routes::post_artifact)
                .delete(format_routes::delete_artifact),
        );

    // Docker Registry v2 / OCI Distribution：挂载于 /v2/。
    // `/v2/` 为版本检查；`/v2/token` 为 Bearer 范围令牌端点（须置于 catch-all 之前，
    // 避免被 `/v2/{*path}` 通配吞掉）；`/v2/{*path}` 按方法分发（name 可多段，走内部解析）。
    let docker_api = Router::new()
        .route("/v2/", get(docker_routes::version_check))
        .route("/v2/token", get(docker_routes::token_endpoint))
        .route(
            "/v2/{*path}",
            get(docker_routes::dispatch_get)
                .head(docker_routes::dispatch_head)
                .post(docker_routes::dispatch_post)
                .patch(docker_routes::dispatch_patch)
                .put(docker_routes::dispatch_put),
        );

    // Web 控制台 SPA：静态资源走 /assets/{*path}，其余未匹配 GET 经 fallback 回退 index.html。
    // 必须在 API / 格式 / 健康检查路由之后接入，避免拦截 /api/v1、/v2/、/health 与格式路径。
    let spa = Router::new().route("/assets/{*path}", get(crate::web::serve_asset));

    Router::new()
        .route("/health", get(health))
        .nest("/api/v1", api_v1)
        .merge(docker_api)
        .merge(format_api)
        .merge(spa)
        // 未匹配任何路由的请求回退到 SPA 入口（前端客户端路由 + 未构建时的 503 占位）
        .fallback(crate::web::spa_fallback)
        .with_state(state.clone())
        // 身份解析中间件：先于业务 handler 解析 Bearer/Basic/匿名 注入扩展
        .layer(middleware::from_fn_with_state(
            state,
            identity::identity_layer,
        ))
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
    async fn 未知前端路由回退到_spa_入口() {
        // SPA 行为：未被 API / 格式 / 健康检查匹配的单段 GET 路径回退到前端入口。
        // 干净检出（未构建前端）时返回 503 占位页；任一情况都不应是 404，
        // 以便前端客户端路由（如 /login）刷新后仍由前端接管。
        let (state, _dir) = 测试用状态().await;
        let app = build_router(state);
        let response = app
            .oneshot(
                Request::builder()
                    .uri("/login")
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        // 未构建前端时为 503 占位，已构建时为 200 index.html；均非 404
        assert_ne!(response.status(), StatusCode::NOT_FOUND);
        assert!(
            response.status() == StatusCode::OK
                || response.status() == StatusCode::SERVICE_UNAVAILABLE
        );
    }

    #[tokio::test]
    async fn 健康检查不被_spa_回退拦截() {
        // 关键回归：SPA fallback 不得吞掉 /health，仍返回 200 健康状态。
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
        let body = 读_json(response).await;
        assert_eq!(body["status"], "ok");
    }

    #[tokio::test]
    async fn api_端点不被_spa_回退拦截() {
        // 关键回归：未认证访问受保护 API 仍走 API 逻辑返回 401，而非被 SPA 回退成 200/503。
        let (state, _dir) = 测试用状态().await;
        let app = build_router(state);
        let response = app
            .oneshot(
                Request::builder()
                    .uri("/api/v1/me")
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(response.status(), StatusCode::UNAUTHORIZED);
    }

    #[tokio::test]
    async fn 根路径返回_spa_入口或占位() {
        // GET / 应交由 SPA：未构建为 503 占位、已构建为 200 index.html。
        let (state, _dir) = 测试用状态().await;
        let app = build_router(state);
        let response = app
            .oneshot(Request::builder().uri("/").body(Body::empty()).unwrap())
            .await
            .unwrap();
        assert!(
            response.status() == StatusCode::OK
                || response.status() == StatusCode::SERVICE_UNAVAILABLE
        );
        // 内容类型应为 HTML（无论入口还是占位页）
        let content_type = response
            .headers()
            .get(axum::http::header::CONTENT_TYPE)
            .and_then(|v| v.to_str().ok())
            .unwrap_or("");
        assert!(content_type.contains("text/html"));
    }
}
