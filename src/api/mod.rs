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
    Extension, Json, Router,
};
use serde_json::json;
use tower_http::request_id::{MakeRequestUuid, PropagateRequestIdLayer, SetRequestIdLayer};
use tower_http::trace::TraceLayer;

use crate::auth::{AuthIdentity, JwtSigner, LdapProvider, LoginGuard, OidcProvider};
use crate::config::Config;
use crate::format::{ArtifactService, DockerRegistry, FormatRegistry};
use crate::meta::MetaStore;
use crate::proxy::HttpUpstream;
use crate::storage::BlobBackend;

mod acl;
mod alerts;
mod analytics;
mod anomaly_ban;
mod artifacts;
mod audit;
mod auth_routes;
mod browse;
mod cargo_routes;
mod cc_challenge;
mod docker_routes;
mod format_routes;
mod go_routes;
mod groups;
mod identity;
mod metrics;
mod migrate;
mod migration_jobs;
mod npm_routes;
mod nuget_routes;
mod oidc_routes;
mod protection;
mod protection_config;
mod protection_state;
mod pypi_routes;
mod rate_limit;
mod repo_access;
mod repositories;
mod search;
mod slowloris;
mod tokens;
mod upload_routes;
mod usage;
mod users;
mod waf;

pub use alerts::{
    channel as alert_channel, spawn_alert_pruner, spawn_alert_writer, AlertEngine, AlertSink,
    ProtectionDimension,
};
pub use anomaly_ban::{BanRegistry, IpMatcher};
pub use audit::{
    channel as audit_channel, spawn_audit_retention, spawn_audit_writer, AuditResult, AuditSink,
};
pub use cc_challenge::CcChallenger;
pub use identity::resolve_identity;
pub use metrics::{install_recorder, MetricsHandle};
pub use migration_jobs::MigrationJobs;
pub use oidc_routes::OidcFlowStore;
pub use protection_state::{ProtectionSnapshot, ProtectionState};
pub use rate_limit::RateLimiter;
pub use usage::{channel as usage_channel, spawn_usage_pruner, spawn_usage_writer, UsageSink};
pub use waf::WafRuleSet;

/// 应用内具体化的通用制品机理服务类型（运行期可选 blob 后端 + reqwest 上游）。
pub type AppArtifactService = ArtifactService<BlobBackend, HttpUpstream>;

/// 应用内具体化的 Docker Registry v2 存储服务类型（运行期可选 blob 后端）。
pub type AppDockerRegistry = DockerRegistry<BlobBackend>;

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
    /// blob 存储（运行期按配置选定 fs / s3 后端）。
    pub store: BlobBackend,
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
    /// 审计事件投递端（FR-31，ADR-0015）：主路径非阻塞 enqueue，后台任务批量落库。
    pub audit: AuditSink,
    /// 使用分析采集投递端（FR-57，ADR-0009）：主路径非阻塞采集访问 / 下载，后台任务聚合落库。
    pub usage: UsageSink,
    /// Prometheus 指标注册表句柄（FR-32，ADR-0015）：`Some` 表示已安装进程内 recorder，
    /// `/metrics` 抓取时渲染；`None` 表示指标端点未启用（端点返回 404）。
    pub metrics: Option<MetricsHandle>,
    /// 基础速率限制器（FR-33，ADR-0008）：进程内按 IP / 身份维度固定窗计数，超阈值返回 429。
    pub rate_limiter: Arc<rate_limit::RateLimiter>,
    /// OIDC 认证 provider（FR-34，ADR-0016）：`Some` 表示已配置 `[auth.oidc]`，
    /// OIDC 登录端点据此发起授权码流；`None` 时相关端点返回 404（未配置即不存在）。
    pub oidc: Option<Arc<OidcProvider>>,
    /// OIDC 登录流程的进程内短期存储（按 state 一次性绑定 PKCE / nonce，防 CSRF / 重放）。
    pub oidc_flows: Arc<oidc_routes::OidcFlowStore>,
    /// LDAP 认证 provider（FR-35，ADR-0016）：`Some` 表示已配置 `[auth.ldap]`，
    /// 口令型登录（Web 表单 / Basic Auth）本地校验失败后委托其做 bind 校验；`None` 时不参与登录。
    pub ldap: Option<Arc<LdapProvider>>,
    /// 运行时防护配置热替换槽（FR-79，扩展 ADR-0008）：收拢当前生效的 `ProtectionConfig` 及其派生态
    /// （IP 名单匹配器、WAF 规则集），经 `RwLock<Arc<..>>` 原子替换。防护中间件经 `snapshot()` 读当前
    /// 配置与派生态（读锁极短、锁外判定）；管理端 PATCH 经 `replace()` 重建派生态并即时生效、无须重启。
    /// **防护配置真源为本槽**：启动后中间件不再读 `config.protection`，`config.protection` 仅初始装载来源。
    pub protection: Arc<protection_state::ProtectionState>,
    /// 访问异常检测与自动封禁登记表（FR-53，ADR-0008）：进程内内存维护各 IP 的窗内异常信号计数
    /// 与封禁到期时刻，窗内异常达阈值即自动封禁一个时间窗、到期自动解封；重启即清，不落 DB。
    pub ban_registry: Arc<anomaly_ban::BanRegistry>,
    /// CC 挑战签名器（FR-54，ADR-0008）：以 HMAC（密钥经会话 JWT 真源密钥域分隔派生，不暴露其本体、
    /// 不与会话 JWT 串味）无状态签发 / 校验工作量证明（PoW）挑战令牌，不存挑战态。默认关闭、默认豁免
    /// 已认证客户端，仅对匿名可疑流量要求 PoW 证明；按连接级来源 IP 绑定，不采信 XFF。
    pub cc_challenger: Arc<cc_challenge::CcChallenger>,
    /// 防护告警投递端（FR-56，ADR-0017）：主路径非阻塞 enqueue，后台任务批量落库。
    pub alerts: alerts::AlertSink,
    /// 进程内防护告警评估器（FR-56，ADR-0017）：各防护命中点累加窗内计数、达阈值即告警；
    /// 状态端点据其窗内计数快照展示防护健康。计数 / 去抖状态进程内内存维护，重启即清。
    pub alert_engine: Arc<alerts::AlertEngine>,
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

    /// 采集用主体名：已认证取用户名，匿名取 `anonymous`（绝不含凭据）。
    pub fn actor_name(&self) -> &str {
        self.0
            .user()
            .map(|u| u.username.as_str())
            .unwrap_or("anonymous")
    }
}

/// 构建 axum 路由：挂健康检查、认证、用户、Token 端点与请求 ID、追踪、身份解析中间件。
pub fn build_router(state: AppState) -> Router {
    // 进程内迁移任务注册表（FR-83，ADR-0019）：随路由创建一份，经 Extension 注入迁移端点共享
    let migration_jobs = Arc::new(MigrationJobs::default());

    // 管理 API 子路由，统一挂在 /api/v1 前缀下
    let api_v1 = Router::new()
        .route("/auth/login", post(auth_routes::login))
        .route("/auth/logout", post(auth_routes::logout))
        .route("/auth/refresh", post(auth_routes::refresh))
        .route("/auth/oidc/login", get(oidc_routes::oidc_login))
        .route("/auth/oidc/callback", get(oidc_routes::oidc_callback))
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
        .route(
            "/repositories/{id}/upload",
            post(upload_routes::upload_artifact),
        )
        .route("/search", get(search::search))
        .route("/audit", get(audit::list_audit))
        .route("/analytics/usage", get(analytics::usage_analytics))
        .route("/protection/status", get(protection::protection_status))
        .route("/protection/alerts", get(protection::list_alerts))
        .route(
            "/protection/config",
            get(protection_config::get_protection_config)
                .patch(protection_config::patch_protection_config),
        )
        .route(
            "/migrate/nexus/preview",
            post(migrate::preview_nexus_repositories),
        )
        .route(
            "/migrate/nexus/offline/preview",
            post(migrate::preview_nexus_offline),
        )
        .route(
            "/migrate/nexus/proxy/migrate",
            post(migrate::migrate_nexus_proxy),
        )
        .route(
            "/migrate/nexus/hosted/migrate",
            post(migrate::migrate_nexus_hosted),
        )
        .route(
            "/migrate/nexus/online/migrate",
            post(migrate::migrate_nexus_online),
        )
        .route("/migrate/jobs", get(migrate::migrate_nexus_jobs))
        .route("/migrate/jobs/{id}", get(migrate::migrate_nexus_job))
        .route(
            "/repositories/{id}/acl",
            get(acl::list_acl).post(acl::create_acl),
        )
        .route(
            "/repositories/{id}/acl/{acl_id}",
            axum::routing::delete(acl::delete_acl),
        )
        .route(
            "/groups",
            get(groups::list_groups).post(groups::create_group),
        )
        .route(
            "/groups/{id}",
            get(groups::get_group).delete(groups::delete_group),
        )
        .route(
            "/groups/{id}/members",
            get(groups::list_members).post(groups::add_member),
        )
        .route(
            "/groups/{id}/members/{user_id}",
            axum::routing::delete(groups::remove_member),
        )
        .route(
            "/repositories/{id}/group-acl",
            get(groups::list_group_acl).post(groups::create_group_acl),
        )
        .route(
            "/repositories/{id}/group-acl/{acl_id}",
            axum::routing::delete(groups::delete_group_acl),
        );

    // 格式 API：按原生协议挂载，路径含仓库名（如 Raw 的 /{repo}/{path..}）。
    // 用 catch-all 段匹配仓库内任意路径；axum 优先匹配 /health 与 /api/v1 等字面前缀。
    // PyPI twine 上传目标为 `POST /{repo}/`（空路径，catch-all 不匹配），故单列其路由；
    // `POST /{repo}/{*path}` 兜底 PyPI 的 `legacy/` 等带路径上传形态。
    let format_api = Router::new()
        .route(
            "/{repo}/",
            get(format_routes::get_repo_root).post(format_routes::post_artifact_root),
        )
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
        .route("/metrics", get(metrics::metrics_endpoint))
        .nest("/api/v1", api_v1)
        .merge(docker_api)
        .merge(format_api)
        .merge(spa)
        // 未匹配任何路由的请求回退到 SPA 入口（前端客户端路由 + 未构建时的 503 占位）
        .fallback(crate::web::spa_fallback)
        .with_state(state.clone())
        // 迁移任务注册表注入（FR-83，ADR-0019）：异步在线拉取端点与进度查询端点共享之
        .layer(Extension(migration_jobs))
        // 指标采集中间件（FR-32，ADR-0015）：置于最内层（最贴近 handler），观测真实 HTTP 维度
        // 与延迟；只做无锁原子观测，渲染仅在 /metrics 被抓取时发生。
        .layer(middleware::from_fn_with_state(
            state.clone(),
            metrics::metrics_layer,
        ))
        // 审计中间件：置于身份解析之内（更靠近 handler），运行 handler 后按方法+路径+状态
        // 归类精选写/管理/授权拒绝事件并非阻塞投递；读到的身份由下方身份解析中间件先行注入。
        .layer(middleware::from_fn_with_state(
            state.clone(),
            audit::audit_layer,
        ))
        // 速率限制中间件（FR-33，ADR-0008）：置于身份解析之后、业务之前，按 IP / 身份维度计数，
        // 超阈值在进入业务前返回 429；未启用时直接放行。须读身份，故置于身份解析之内（更靠近 handler）。
        .layer(middleware::from_fn_with_state(
            state.clone(),
            rate_limit::rate_limit_layer,
        ))
        // CC 挑战中间件（FR-54，ADR-0008）：置于身份解析之内（须读已注入身份以豁免已认证客户端）。
        // 启用时仅对匿名可疑流量要求工作量证明（PoW）：无 / 错误证明返回 429 + 挑战参数，带合法证明
        // 放行；已认证默认豁免。无状态 HMAC 签发 / 校验、绑定连接级来源 IP（不采信 XFF）；未启用时直接放行。
        .layer(middleware::from_fn_with_state(
            state.clone(),
            cc_challenge::cc_challenge_layer,
        ))
        // 身份解析中间件：先于业务 handler 解析 Bearer/Basic/匿名 注入扩展
        .layer(middleware::from_fn_with_state(
            state.clone(),
            identity::identity_layer,
        ))
        // 异常检测与自动封禁 + IP 黑/白名单中间件（FR-53，ADR-0008）：置于热路径前端（外于身份解析
        // 与限流），白名单豁免、黑名单 / 封禁中直接拒；放行后按响应状态（含限流产生的 429）统计异常
        // 信号、触阈即自动封禁。仅应用层（L7）；按连接级来源 IP 判定，不采信 XFF。
        .layer(middleware::from_fn_with_state(
            state.clone(),
            anomaly_ban::anomaly_ban_layer,
        ))
        // 可配置 WAF 规则引擎中间件（FR-55，ADR-0008）：置于热路径前端（与其他 L7 防护同层），
        // 在进入业务前按有序规则匹配请求 method/path/query/header，首个命中 block 即拒 403、
        // 命中 allow 即放行。未启用 / 空规则集走零开销快路径。仅应用层（L7）；不依赖来源 IP、不采信 XFF。
        .layer(middleware::from_fn_with_state(
            state.clone(),
            waf::waf_layer,
        ))
        // 慢速攻击防护中间件（FR-52，ADR-0008）：置于身份解析之外（更靠近连接侧），在读取请求体前
        // 介入，把请求体包成带「首块等待超时 + 块间空闲超时 + 通用字节上限」的数据流，慢速 drip 超时
        // 即断、超大体（带 Content-Length 时进入业务前）拒 413；未启用时直接放行。仅应用层（L7）。
        .layer(middleware::from_fn_with_state(
            state,
            slowloris::slowloris_layer,
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
        let store = BlobBackend::Fs(
            crate::storage::LocalFsStore::new(dir.path().join("blobs"))
                .await
                .unwrap(),
        );
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
        // 测试用审计：创建 sink 并启动写入任务，使路由真实走审计采集链路
        let (audit, audit_rx) = audit::channel();
        audit::spawn_audit_writer(meta.clone(), audit_rx);
        // 测试用使用分析：创建 sink 并启动写入任务（关明细），使路由真实走采集链路
        let (usage, usage_rx) = usage::channel();
        usage::spawn_usage_writer(meta.clone(), usage_rx, false);
        // 测试用防护告警：创建 sink 并启动写入任务，使路由真实走告警落库链路
        let (alerts, alert_rx) = alerts::channel();
        alerts::spawn_alert_writer(meta.clone(), alert_rx);
        let alert_engine = Arc::new(alerts::AlertEngine::new(alerts.clone()));
        let state = AppState {
            config: Arc::new(Config::default()),
            meta,
            store,
            jwt,
            login_guard: Arc::new(LoginGuard::new(5, 900)),
            artifacts,
            formats: Arc::new(FormatRegistry::with_builtin()),
            docker: Some(docker),
            audit,
            usage,
            // 默认测试状态不安装 recorder；需要指标端点的测试自行用 install_recorder 注入
            metrics: None,
            rate_limiter: Arc::new(rate_limit::RateLimiter::new()),
            // 默认测试状态不配置 OIDC；需要 OIDC 的测试自行注入 provider
            oidc: None,
            oidc_flows: Arc::new(oidc_routes::OidcFlowStore::new()),
            // 默认测试状态不配置 LDAP；需要 LDAP 的测试自行注入 provider
            ldap: None,
            // 默认测试状态：防护热替换槽以默认配置装载（名单空、WAF 空、各防护默认关闭）；
            // 需要的测试经 protection.replace 或直接构造定制配置
            protection: Arc::new(protection_state::ProtectionState::new(
                crate::config::ProtectionConfig::default(),
            )),
            ban_registry: Arc::new(anomaly_ban::BanRegistry::new()),
            // 默认测试状态 CC 挑战关闭；需要的测试自行定制配置与挑战器（密钥与 jwt 同源）
            cc_challenger: Arc::new(cc_challenge::CcChallenger::new(
                b"test-secret-32-bytes-xxxxxxxxxxxx",
            )),
            // 默认测试状态：告警引擎就绪（默认配置告警关闭，record 直接返回）
            alerts,
            alert_engine,
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

    // ===== FR-32 指标端点测试 =====

    use crate::auth::hash_password;
    use crate::config::MetricsConfig;
    use crate::meta::Role;
    use std::sync::OnceLock;

    /// 进程内全局 recorder 仅可安装一次：测试间共享同一句柄，避免重复安装报错。
    fn 共享指标句柄() -> MetricsHandle {
        static HANDLE: OnceLock<MetricsHandle> = OnceLock::new();
        HANDLE
            .get_or_init(|| install_recorder().expect("安装测试 recorder"))
            .clone()
    }

    /// 以给定指标配置 + 是否安装句柄构造状态：覆盖 enabled / allow_anonymous 各组合。
    async fn 指标状态(
        metrics_cfg: MetricsConfig,
        装句柄: bool,
    ) -> (AppState, tempfile::TempDir) {
        let (mut state, dir) = 测试用状态().await;
        let mut cfg = (*state.config).clone();
        cfg.observability.metrics = metrics_cfg;
        state.config = Arc::new(cfg);
        if 装句柄 {
            state.metrics = Some(共享指标句柄());
        }
        (state, dir)
    }

    /// 在状态库内建一个 Admin 用户并签发其会话 JWT，用于带认证抓取 /metrics。
    async fn 签发管理员令牌(state: &AppState) -> String {
        let uid = state
            .meta
            .create_user("metrics-admin", &hash_password("pw").unwrap(), Role::Admin)
            .await
            .unwrap();
        state.jwt.issue(&uid, "metrics-admin", Role::Admin).unwrap()
    }

    /// 在状态库内建一个普通 User 并签发其会话 JWT，用于验证非管理员被拒。
    async fn 签发普通用户令牌(state: &AppState) -> String {
        let uid = state
            .meta
            .create_user("metrics-user", &hash_password("pw").unwrap(), Role::User)
            .await
            .unwrap();
        state.jwt.issue(&uid, "metrics-user", Role::User).unwrap()
    }

    async fn 抓取(app: Router, 令牌: Option<&str>) -> Response {
        let mut builder = Request::builder().uri("/metrics");
        if let Some(t) = 令牌 {
            builder = builder.header("Authorization", format!("Bearer {t}"));
        }
        app.oneshot(builder.body(Body::empty()).unwrap())
            .await
            .unwrap()
    }

    #[tokio::test]
    async fn metrics_默认匿名抓取被拒_401() {
        let (state, _dir) = 指标状态(MetricsConfig::default(), true).await;
        let app = build_router(state);
        let resp = 抓取(app, None).await;
        assert_eq!(resp.status(), StatusCode::UNAUTHORIZED);
    }

    #[tokio::test]
    async fn metrics_普通用户抓取被拒_403() {
        let (state, _dir) = 指标状态(MetricsConfig::default(), true).await;
        let token = 签发普通用户令牌(&state).await;
        let app = build_router(state);
        let resp = 抓取(app, Some(&token)).await;
        assert_eq!(resp.status(), StatusCode::FORBIDDEN);
    }

    #[tokio::test]
    async fn metrics_管理员抓取成功并返回_prometheus_文本() {
        let (state, _dir) = 指标状态(MetricsConfig::default(), true).await;
        let token = 签发管理员令牌(&state).await;
        let app = build_router(state);
        let resp = 抓取(app, Some(&token)).await;
        assert_eq!(resp.status(), StatusCode::OK);
        let ct = resp
            .headers()
            .get(axum::http::header::CONTENT_TYPE)
            .and_then(|v| v.to_str().ok())
            .unwrap_or("")
            .to_string();
        assert!(ct.starts_with("text/plain"));
    }

    #[tokio::test]
    async fn metrics_允许匿名时免认证抓取成功() {
        let cfg = MetricsConfig {
            enabled: true,
            allow_anonymous: true,
        };
        let (state, _dir) = 指标状态(cfg, true).await;
        let app = build_router(state);
        let resp = 抓取(app, None).await;
        assert_eq!(resp.status(), StatusCode::OK);
    }

    #[tokio::test]
    async fn metrics_未启用时端点返回_404() {
        // enabled=false：即便误装了句柄，端点也按未启用返回 404（不泄露运行画像）
        let cfg = MetricsConfig {
            enabled: false,
            allow_anonymous: true,
        };
        let (state, _dir) = 指标状态(cfg, true).await;
        let app = build_router(state);
        let resp = 抓取(app, None).await;
        assert_eq!(resp.status(), StatusCode::NOT_FOUND);
    }

    #[tokio::test]
    async fn metrics_未安装句柄时端点返回_404() {
        // enabled=true 但 metrics=None（recorder 安装失败降级）：端点返回 404
        let (state, _dir) = 指标状态(MetricsConfig::default(), false).await;
        let token = 签发管理员令牌(&state).await;
        let app = build_router(state);
        let resp = 抓取(app, Some(&token)).await;
        assert_eq!(resp.status(), StatusCode::NOT_FOUND);
    }

    #[tokio::test]
    async fn metrics_请求计数随抓取增长且为低基数标签() {
        let cfg = MetricsConfig {
            enabled: true,
            allow_anonymous: true,
        };
        let (state, _dir) = 指标状态(cfg, true).await;
        let app = build_router(state.clone());

        // 先打一发健康检查，制造一次被中间件观测的 GET 请求
        let _ = app
            .clone()
            .oneshot(
                Request::builder()
                    .uri("/health")
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();

        let resp = 抓取(app, None).await;
        assert_eq!(resp.status(), StatusCode::OK);
        let body = resp.into_body().collect().await.unwrap().to_bytes();
        let text = String::from_utf8(body.to_vec()).unwrap();

        // 渲染文本应含本服务的请求计数指标名，且标签为有界枚举（method/status_class/format）
        assert!(
            text.contains("jianartifact_http_requests_total"),
            "渲染文本应含请求计数指标：{text}"
        );
        assert!(text.contains("method="), "应含 method 标签");
        assert!(text.contains("status_class="), "应含 status_class 标签");
        assert!(text.contains("format="), "应含 format 标签");
        // 低基数纪律：标签里不得出现仓库名 / 完整路径等无界值（如 /health 路径串）
        assert!(
            !text.contains("/health"),
            "标签不得包含原始路径等无界值：{text}"
        );
    }
}
