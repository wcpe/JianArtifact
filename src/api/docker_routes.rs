//! Docker Registry v2 / OCI Distribution 格式 API（FR-16）：挂载于 `/v2/`。
//!
//! handler 保持薄：只做协议适配（路径解析、状态码与头组装、错误→registry v2 错误体）、
//! 认证 / 鉴权编排，存储与状态机下沉到 `format::DockerRegistry`。
//!
//! 鉴权要点（testing-and-quality §2.1）：
//! - **未认证访问受保护资源 → 401 + `WWW-Authenticate`**（docker 客户端据此带凭据重试）。
//! - **已认证但无权**：按既有 authz——无读权限的 private 返回 404 隐藏存在性；有读无写返回 403。
//! - `{name}` 形如 `{仓库}/{镜像}`：首段为 JianArtifact 仓库名，其余为镜像名（可多段）。

use axum::{
    body::Body,
    extract::{FromRequestParts, Path, Query, State},
    http::{header, request::Parts, HeaderValue, StatusCode},
    response::{IntoResponse, Response},
    Json,
};
use futures_util::TryStreamExt;
use serde::Deserialize;
use serde_json::json;
use tokio_util::io::{ReaderStream, StreamReader};

use crate::auth::{basic, AuthIdentity, DockerAccess, DOCKER_TOKEN_TTL_SECS};
use crate::authz::{authorize, Action, Decision};
use crate::format::docker;
use crate::format::DockerError;
use crate::meta::RepositoryRecord;

use super::repo_access::build_repo_view;
use super::{ApiError, AppState, Identity};

/// Docker Distribution API 版本头名。
const API_VERSION_HEADER: &str = "Docker-Distribution-Api-Version";
/// Docker Distribution API 版本值。
const API_VERSION_VALUE: &str = "registry/2.0";
/// manifest digest 响应头名。
const CONTENT_DIGEST_HEADER: &str = "Docker-Content-Digest";
/// Bearer 质询中的 service 标识（令牌端点据此区分服务）。
const TOKEN_SERVICE: &str = "jianartifact";
/// docker 范围令牌端点路径（相对 `/v2`）。
const TOKEN_PATH: &str = "/v2/token";
/// docker `pull` 动作名。
const ACTION_PULL: &str = "pull";
/// docker `push` 动作名。
const ACTION_PUSH: &str = "push";
/// registry v2 资源类型（固定为 repository）。
const RESOURCE_TYPE: &str = "repository";

/// PUT blob 完成时的 digest 查询参数。
#[derive(Debug, Deserialize)]
pub struct DigestQuery {
    /// 客户端声明的 blob digest（`sha256:{hex}`）。
    digest: Option<String>,
}

/// docker 协议错误：转为 registry v2 规范错误体并附带状态码。
///
/// 不复用管理 API 的统一 JSON 错误结构（registry v2 客户端按其自有 errors 数组解析）。
struct DockerApiError {
    /// HTTP 状态码。
    status: StatusCode,
    /// registry v2 错误码（如 `BLOB_UNKNOWN` / `MANIFEST_UNKNOWN` / `UNAUTHORIZED`）。
    code: &'static str,
    /// 面向客户端的可读说明。
    message: String,
    /// 401 时携带的 `WWW-Authenticate` 头完整值（Bearer 质询）；其余情况为 None。
    challenge: Option<String>,
}

impl DockerApiError {
    fn new(status: StatusCode, code: &'static str, message: impl Into<String>) -> Self {
        Self {
            status,
            code,
            message: message.into(),
            challenge: None,
        }
    }

    /// 未认证访问受保护资源：401 + `WWW-Authenticate: Bearer`，引导 docker 客户端到
    /// 令牌端点用 Basic 凭据换取范围令牌后重试。`challenge` 为完整头值。
    fn unauthorized(challenge: String) -> Self {
        Self {
            status: StatusCode::UNAUTHORIZED,
            code: "UNAUTHORIZED",
            message: "需要认证".to_string(),
            challenge: Some(challenge),
        }
    }

    fn not_found(code: &'static str) -> Self {
        Self::new(StatusCode::NOT_FOUND, code, "资源不存在")
    }
}

impl IntoResponse for DockerApiError {
    fn into_response(self) -> Response {
        let body = Json(json!({
            "errors": [{ "code": self.code, "message": self.message }]
        }));
        let mut resp = (self.status, body).into_response();
        resp.headers_mut().insert(
            API_VERSION_HEADER,
            HeaderValue::from_static(API_VERSION_VALUE),
        );
        if let Some(challenge) = self.challenge {
            // Bearer 令牌质询：docker / skopeo 据此到令牌端点换取范围令牌再重试
            if let Ok(v) = HeaderValue::from_str(&challenge) {
                resp.headers_mut().insert(header::WWW_AUTHENTICATE, v);
            }
        }
        resp
    }
}

/// 组装 Bearer 质询头值：`Bearer realm="{realm}",service="{service}",scope="repository:{name}:{actions}"`。
///
/// realm 取对外令牌端点地址（基于 `public_base_url`，缺省按监听地址构造，复用 `location()` 思路）；
/// `actions` 为本操作所需动作（读 = `pull`；写 = `pull,push`）。
fn bearer_challenge(state: &AppState, name: &str, actions: &str) -> String {
    let realm = format!("{}{TOKEN_PATH}", base_url(state));
    format!(
        "Bearer realm=\"{realm}\",service=\"{TOKEN_SERVICE}\",scope=\"{RESOURCE_TYPE}:{name}:{actions}\""
    )
}

/// 把 DockerRegistry 存储错误映射为 registry v2 协议错误。
fn map_docker_error(e: DockerError) -> DockerApiError {
    match e {
        DockerError::NotFound => DockerApiError::not_found("BLOB_UNKNOWN"),
        DockerError::UnknownUpload => DockerApiError::not_found("BLOB_UPLOAD_UNKNOWN"),
        DockerError::DigestMismatch => DockerApiError::new(
            StatusCode::BAD_REQUEST,
            "DIGEST_INVALID",
            "digest 与内容不匹配",
        ),
        DockerError::InvalidDigest => {
            DockerApiError::new(StatusCode::BAD_REQUEST, "DIGEST_INVALID", "digest 格式非法")
        }
        DockerError::UnsupportedMediaType => DockerApiError::new(
            StatusCode::BAD_REQUEST,
            "MANIFEST_INVALID",
            "manifest 媒体类型不受支持",
        ),
        DockerError::TooLarge => DockerApiError::new(
            StatusCode::PAYLOAD_TOO_LARGE,
            "BLOB_UPLOAD_INVALID",
            "上传体积超过上限",
        ),
        DockerError::Storage(err) => {
            tracing::error!(错误 = %err, "docker blob 存储访问失败");
            DockerApiError::new(StatusCode::INTERNAL_SERVER_ERROR, "UNKNOWN", "内部错误")
        }
        DockerError::Meta(err) => {
            tracing::error!(错误 = %err, "docker 元数据访问失败");
            DockerApiError::new(StatusCode::INTERNAL_SERVER_ERROR, "UNKNOWN", "内部错误")
        }
    }
}

/// 版本检查（`GET /v2/`）：探活并发起认证发现。
///
/// registry v2 的"挑战-应答"令牌流要求客户端能在**探活阶段**发现令牌 realm：未携带
/// `Authorization` 时返回 `401 + WWW-Authenticate: Bearer`（不带 scope），客户端据此
/// 到令牌端点换取令牌（匿名亦可换取仅含 public `pull` 的匿名令牌，故匿名拉取 public 仍成立）；
/// 携带凭据 / 令牌时返回 200 与版本头。skopeo / docker 据此建立 bearer 流程，认证推送方可用。
pub async fn version_check(state: State<AppState>, headers: axum::http::HeaderMap) -> Response {
    if headers.get(header::AUTHORIZATION).is_none() {
        // 不带 scope 的 Bearer 质询：仅用于让客户端发现令牌 realm 并建立认证流程
        return DockerApiError::unauthorized(format!(
            "Bearer realm=\"{}{TOKEN_PATH}\",service=\"{TOKEN_SERVICE}\"",
            base_url(&state)
        ))
        .into_response();
    }
    let mut resp = StatusCode::OK.into_response();
    resp.headers_mut().insert(
        API_VERSION_HEADER,
        HeaderValue::from_static(API_VERSION_VALUE),
    );
    resp
}

/// docker 范围令牌端点（`GET /v2/token`）。
///
/// 流程：① 读 `Authorization: Basic` 解析用户（口令或 API Token）——提供但无效则 401，
/// 无凭据则按匿名；② 对每个 `scope=repository:{name}:{actions}` 逐项跑授权，只把**通过**的
/// 动作放进该 scope 的授予集合（仓库不存在或全拒 → 该 scope 授予空，不报错）；③ 用同一 HS256
/// 密钥签发短期 docker 令牌返回。客户端据 401 Bearer 质询调用此端点，再用 `Bearer` 重试原请求。
pub async fn token_endpoint(
    State(state): State<AppState>,
    Query(params): Query<Vec<(String, String)>>,
    headers: axum::http::HeaderMap,
) -> Response {
    // ① 解析 Basic 凭据为身份：携带 Basic 但无效 → 401；无凭据 → 匿名
    let identity = match resolve_token_identity(&state, &headers).await {
        Ok(id) => id,
        Err(resp) => return resp,
    };
    let subject = identity
        .user()
        .map(|u| u.username.clone())
        .unwrap_or_else(|| "anonymous".to_string());

    // ② 逐个 scope 计算授予动作
    let mut access = Vec::new();
    for (k, v) in &params {
        if k != "scope" {
            continue;
        }
        for scope in v.split(' ').filter(|s| !s.is_empty()) {
            if let Some(granted) = grant_scope(&state, &identity, scope).await {
                access.push(granted);
            }
        }
    }

    // ③ 签发短期 docker 令牌
    let token = match state
        .jwt
        .issue_docker_token(&subject, access, DOCKER_TOKEN_TTL_SECS)
    {
        Ok(t) => t,
        Err(e) => {
            tracing::error!(错误 = %e, "签发 docker 范围令牌失败");
            return DockerApiError::new(StatusCode::INTERNAL_SERVER_ERROR, "UNKNOWN", "内部错误")
                .into_response();
        }
    };

    Json(json!({
        "token": token,
        "access_token": token,
        "expires_in": DOCKER_TOKEN_TTL_SECS,
        "issued_at": rfc3339_now(),
    }))
    .into_response()
}

/// 当前 UTC 时间格式化为 RFC3339 字符串（如 `2026-06-23T08:00:00Z`）。
///
/// registry v2 令牌响应的 `issued_at` 为可选信息字段，客户端主要依赖 `token` / `expires_in`。
/// 项目未引入日期库，这里据 Unix 秒按公历换算（civil-from-days 算法），避免新增依赖。
fn rfc3339_now() -> String {
    use std::time::{SystemTime, UNIX_EPOCH};
    let secs = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0) as i64;
    let days = secs.div_euclid(86_400);
    let secs_of_day = secs.rem_euclid(86_400);
    let (h, m, s) = (
        secs_of_day / 3600,
        (secs_of_day % 3600) / 60,
        secs_of_day % 60,
    );
    let (year, month, day) = civil_from_days(days);
    format!("{year:04}-{month:02}-{day:02}T{h:02}:{m:02}:{s:02}Z")
}

/// 把"自 1970-01-01 起的天数"换算为公历 (年, 月, 日)。
///
/// 采用 Howard Hinnant 的 civil-from-days 算法（对负数天数亦正确，闰年规则完备）。
fn civil_from_days(z: i64) -> (i64, u32, u32) {
    let z = z + 719_468;
    let era = z.div_euclid(146_097);
    let doe = z.rem_euclid(146_097);
    let yoe = (doe - doe / 1460 + doe / 36_524 - doe / 146_096) / 365;
    let y = yoe + era * 400;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let d = (doy - (153 * mp + 2) / 5 + 1) as u32;
    let m = if mp < 10 { mp + 3 } else { mp - 9 } as u32;
    (if m <= 2 { y + 1 } else { y }, m, d)
}

/// 解析令牌端点的 Basic 凭据为身份：无 Authorization → 匿名；
/// 有 Basic 但解析为匿名（凭据无效）→ Err(401)；其余通道（非 Basic）按匿名。
async fn resolve_token_identity(
    state: &AppState,
    headers: &axum::http::HeaderMap,
) -> Result<AuthIdentity, Response> {
    let Some(value) = headers
        .get(header::AUTHORIZATION)
        .and_then(|v| v.to_str().ok())
    else {
        return Ok(AuthIdentity::Anonymous);
    };
    // 非 Basic 通道（如直接带 Bearer）在令牌端点不采信，按匿名
    if basic::strip_scheme_prefix(value, "Basic ").is_none() {
        return Ok(AuthIdentity::Anonymous);
    }
    // Docker 令牌端点同走 Basic Auth，配置了 LDAP 时一并支持 LDAP bind 登录。
    let ldap = super::identity::LdapAuthContext::from_state(state);
    let identity = super::resolve_identity(&state.meta, &state.jwt, value, ldap.as_ref()).await;
    if identity.is_authenticated() {
        Ok(identity)
    } else {
        // 提供了 Basic 凭据却无效：明确拒绝（而非降级匿名签空令牌）
        Err(DockerApiError::unauthorized(format!(
            "Bearer realm=\"{}{TOKEN_PATH}\",service=\"{TOKEN_SERVICE}\"",
            base_url(state)
        ))
        .into_response())
    }
}

/// 解析单个 `repository:{name}:{actions}` scope，对每个请求动作跑授权，返回授予项。
///
/// 仓库不存在或全部动作被拒 → 返回授予空 actions 的项（registry v2 约定：能登录即可签发，
/// 具体放行交原请求再判）。scope 格式非法 → None（忽略该 scope）。
async fn grant_scope(
    state: &AppState,
    identity: &AuthIdentity,
    scope: &str,
) -> Option<DockerAccess> {
    let (resource_type, rest) = scope.split_once(':')?;
    if resource_type != RESOURCE_TYPE {
        return None;
    }
    // name 可含冒号？docker 资源名不含冒号，actions 在最后一段：从右侧切一次
    let (name, actions_raw) = rest.rsplit_once(':')?;
    if name.is_empty() {
        return None;
    }

    let mut granted = Vec::new();
    // 解析仓库视图一次，供该 scope 的所有动作复用（锁外 IO 已在 meta 层）
    let view = match split_name(name) {
        Some((repo_name, _)) => repo_view_for(state, identity, &repo_name).await,
        None => None,
    };

    for action_raw in actions_raw.split(',').filter(|a| !a.is_empty()) {
        let Some(action) = parse_action(action_raw) else {
            continue;
        };
        if let Some(view) = view.as_ref() {
            if authorize(identity, view, action) == Decision::Allow {
                granted.push(action_name(action).to_string());
            }
        }
    }

    Some(DockerAccess {
        r#type: RESOURCE_TYPE.to_string(),
        name: name.to_string(),
        actions: granted,
    })
}

/// 据用户身份查某仓库的授权视图；仓库不存在 / 查库失败 → None（视为无授权）。
async fn repo_view_for(
    state: &AppState,
    identity: &AuthIdentity,
    repo_name: &str,
) -> Option<crate::authz::RepoView> {
    let repo = state.meta.get_repository_by_name(repo_name).await.ok()??;
    // 复用既有视图构造：经 Identity 包装以匹配 build_repo_view 接口
    build_repo_view(state, &Identity(identity.clone()), &repo)
        .await
        .ok()
}

/// 把 docker 动作名解析为内部 Action；未知动作返回 None。
fn parse_action(action: &str) -> Option<Action> {
    match action {
        ACTION_PULL => Some(Action::Read),
        ACTION_PUSH => Some(Action::Write),
        _ => None,
    }
}

/// docker 操作的鉴权上下文：封装"先试 docker Bearer 令牌、再回退既有 Identity"。
///
/// - `token`：经 `verify_docker_token` 校验通过的范围令牌 claims（若请求带有效 docker 令牌）；
/// - `identity`：既有身份解析中间件注入的身份（预先 Basic / 会话 JWT / API Token / 匿名）。
///
/// 判定时优先用令牌的 `access`（携带 docker 令牌即视为已认证）；无令牌则回退 `identity` 走
/// 既有 authz 逻辑，确保 curl 预先 Basic 与匿名 public 读照旧可用。
pub struct DockerAuth {
    /// 既有身份（令牌缺失或无效时回退使用）。
    identity: Identity,
    /// 校验通过的 docker 范围令牌（若有）。
    token: Option<crate::auth::DockerTokenClaims>,
}

impl FromRequestParts<AppState> for DockerAuth {
    type Rejection = ApiError;

    async fn from_request_parts(
        parts: &mut Parts,
        state: &AppState,
    ) -> Result<Self, Self::Rejection> {
        let identity = Identity::from_request_parts(parts, state).await?;
        // 仅当 Authorization 为 Bearer 且能按 docker 令牌校验通过时采信；
        // 其余（Basic / 会话 JWT / API Token / 无凭据）一律回退既有 identity。
        let token = parts
            .headers
            .get(header::AUTHORIZATION)
            .and_then(|v| v.to_str().ok())
            .and_then(|h| basic::strip_scheme_prefix(h, "Bearer "))
            .and_then(|raw| state.jwt.verify_docker_token(raw.trim()).ok());
        Ok(DockerAuth { identity, token })
    }
}

impl DockerAuth {
    /// 令牌是否对 `(name, action)` 授予对应动作：命中某 access 项的 name 且 actions 含该动作。
    fn token_grants(&self, name: &str, action: Action) -> bool {
        let want = action_name(action);
        self.token.as_ref().is_some_and(|claims| {
            claims
                .access
                .iter()
                .any(|a| a.name == name && a.actions.iter().any(|act| act == want))
        })
    }

    /// 是否携带（已校验）docker 令牌——携带即视为已认证语义（隐藏存在性按 404 而非 401）。
    fn has_token(&self) -> bool {
        self.token.is_some()
    }
}

/// 操作对应的 docker 动作名。
///
/// docker registry 协议仅有 pull / push 两种动作，本端点只会以 Read / Write 调用本函数；
/// delete / admin 等变更类动作归入 push（写类），但 docker 流程实际不产生它们。
fn action_name(action: Action) -> &'static str {
    match action {
        Action::Read => ACTION_PULL,
        Action::Write | Action::Delete | Action::Admin => ACTION_PUSH,
    }
}

/// 把 docker `{name}` 拆为 `(JianArtifact 仓库名, 镜像名)`。
///
/// 首段为仓库名，其余为镜像名（docker 镜像名可多段，如 `library/alpine`）。
/// 仅一段时视为缺镜像名，返回 None。
fn split_name(name: &str) -> Option<(String, String)> {
    let (repo, image) = name.split_once('/')?;
    if repo.is_empty() || image.is_empty() {
        return None;
    }
    Some((repo.to_string(), image.to_string()))
}

/// 解析仓库并施加读授权（docker 语义）。
///
/// 鉴权来源二选一（互斥）：① 携带有效 docker 令牌 → **仅按令牌的 access 判定**（令牌的
/// `sub` 即已认证身份，授权已在签发时定）；② 否则回退既有 identity 走 authz。`name` 为完整
/// docker 名（`{仓库}/{镜像}`），用于令牌匹配与 401 质询 scope。
///
/// 拒绝映射：携令牌或已登录（已认证语义）→ 404 隐藏存在性；无令牌且匿名 → 401 Bearer 质询。
async fn load_readable_repo(
    state: &AppState,
    auth: &DockerAuth,
    name: &str,
    repo_name: &str,
) -> Result<RepositoryRecord, DockerApiError> {
    let repo = match state.meta.get_repository_by_name(repo_name).await {
        Ok(Some(r)) => r,
        Ok(None) => return Err(deny_read(state, auth, name)),
        Err(e) => return Err(map_docker_error(DockerError::Meta(e))),
    };
    if auth.has_token() {
        // 令牌通道：授予 pull → 放行；否则按已认证 404 隐藏存在性
        return if auth.token_grants(name, Action::Read) {
            Ok(repo)
        } else {
            Err(DockerApiError::not_found("NAME_UNKNOWN"))
        };
    }
    // identity 通道：走既有 authz
    let view = build_repo_view(state, &auth.identity, &repo)
        .await
        .map_err(|_| {
            DockerApiError::new(StatusCode::INTERNAL_SERVER_ERROR, "UNKNOWN", "内部错误")
        })?;
    match authorize(&auth.identity.0, &view, Action::Read) {
        Decision::Allow => Ok(repo),
        Decision::Deny => Err(deny_read(state, auth, name)),
    }
}

/// 解析仓库并施加写授权（docker 语义）。
///
/// 鉴权来源二选一（互斥）：① 携带有效 docker 令牌 → **仅按令牌的 access 判定**；② 否则回退
/// 既有 identity。已认证语义下：授予 push → 放行；仅授予 pull（有读无写）→ 403；无读 → 404。
/// 未认证（无令牌且匿名）→ 401 + Bearer 质询（scope 含 `pull,push`）。
async fn load_writable_repo(
    state: &AppState,
    auth: &DockerAuth,
    name: &str,
    repo_name: &str,
) -> Result<RepositoryRecord, DockerApiError> {
    if auth.has_token() {
        // 令牌通道：仓库须存在（不存在按已认证 404）
        let repo = match state.meta.get_repository_by_name(repo_name).await {
            Ok(Some(r)) => r,
            Ok(None) => return Err(DockerApiError::not_found("NAME_UNKNOWN")),
            Err(e) => return Err(map_docker_error(DockerError::Meta(e))),
        };
        return if auth.token_grants(name, Action::Write) {
            Ok(repo)
        } else if auth.token_grants(name, Action::Read) {
            // 有读无写 → 403
            Err(DockerApiError::new(
                StatusCode::FORBIDDEN,
                "DENIED",
                "无写权限",
            ))
        } else {
            // 既无读也无写 → 404 隐藏存在性
            Err(DockerApiError::not_found("NAME_UNKNOWN"))
        };
    }
    // identity 通道：写必须认证，匿名 → 401 引导到令牌端点（scope 含 pull,push）
    if !auth.identity.0.is_authenticated() {
        return Err(DockerApiError::unauthorized(bearer_challenge(
            state,
            name,
            &format!("{ACTION_PULL},{ACTION_PUSH}"),
        )));
    }
    let repo = match state.meta.get_repository_by_name(repo_name).await {
        Ok(Some(r)) => r,
        // 已认证但仓库不存在 → 404
        Ok(None) => return Err(DockerApiError::not_found("NAME_UNKNOWN")),
        Err(e) => return Err(map_docker_error(DockerError::Meta(e))),
    };
    let view = build_repo_view(state, &auth.identity, &repo)
        .await
        .map_err(|_| {
            DockerApiError::new(StatusCode::INTERNAL_SERVER_ERROR, "UNKNOWN", "内部错误")
        })?;
    // 先过读判定：无读权限（含登录无 ACL 访问 private）→ 404 隐藏存在性
    if authorize(&auth.identity.0, &view, Action::Read) == Decision::Deny {
        return Err(DockerApiError::not_found("NAME_UNKNOWN"));
    }
    match authorize(&auth.identity.0, &view, Action::Write) {
        Decision::Allow => Ok(repo),
        Decision::Deny => Err(DockerApiError::new(
            StatusCode::FORBIDDEN,
            "DENIED",
            "无写权限",
        )),
    }
}

/// 读拒绝的状态映射：携令牌或已登录（已认证语义）→ 404 隐藏存在性；无令牌且匿名 → 401 Bearer 质询。
fn deny_read(state: &AppState, auth: &DockerAuth, name: &str) -> DockerApiError {
    if auth.has_token() || auth.identity.0.is_authenticated() {
        DockerApiError::not_found("NAME_UNKNOWN")
    } else {
        DockerApiError::unauthorized(bearer_challenge(state, name, ACTION_PULL))
    }
}

/// 校验仓库格式为 docker，否则 404（该路由仅服务 docker 仓库）。
fn ensure_docker(repo: &RepositoryRecord) -> Result<(), DockerApiError> {
    if repo.format == "docker" {
        Ok(())
    } else {
        Err(DockerApiError::not_found("NAME_UNKNOWN"))
    }
}

/// 取 docker registry 服务句柄（启动时必装配；缺失视为内部错误）。
fn registry(state: &AppState) -> Result<&super::AppDockerRegistry, DockerApiError> {
    state.docker.as_deref().ok_or_else(|| {
        tracing::error!("docker registry 未装配");
        DockerApiError::new(StatusCode::INTERNAL_SERVER_ERROR, "UNKNOWN", "内部错误")
    })
}

/// 对外基础地址：优先取 `public_base_url`（去尾斜杠），缺省按监听地址构造 `http://{addr}:{port}`。
///
/// 令牌端点 realm 与 Location 头共用此基址，保证客户端取到可回连的地址。
fn base_url(state: &AppState) -> String {
    if let Some(u) = state.config.server.public_base_url.as_deref() {
        return u.trim_end_matches('/').to_string();
    }
    let server = &state.config.server;
    format!("http://{}:{}", server.listen_addr, server.port)
}

/// 把对外基础地址与路径拼成 Location 头值。
fn location(state: &AppState, path: &str) -> String {
    format!("{}{path}", base_url(state))
}

/// `/v2/` 之后的路径解析结果：把 `{name}/{后缀}` 归类为具体的 registry 操作。
///
/// docker 的 `{name}` 可含多段（如 `repo/library/alpine`），而后缀模式固定
/// （`/blobs/uploads/...` / `/blobs/{digest}` / `/manifests/{ref}`），故从右侧的后缀标记切分。
enum V2Route {
    /// 启动 blob 上传：`{name}/blobs/uploads/`。
    StartUpload { name: String },
    /// 续传 / 完成 blob 上传：`{name}/blobs/uploads/{uuid}`。
    Upload { name: String, uuid: String },
    /// blob 读取：`{name}/blobs/{digest}`。
    Blob { name: String, digest: String },
    /// manifest 存取：`{name}/manifests/{reference}`。
    Manifest { name: String, reference: String },
    /// tag 列表：`{name}/tags/list`。
    TagsList { name: String },
}

/// 解析 `/v2/` 之后的相对路径为 [`V2Route`]；无法识别返回 None。
fn parse_v2_route(rest: &str) -> Option<V2Route> {
    // tag 列表：以 `/tags/list` 结尾
    if let Some(name) = rest.strip_suffix("/tags/list") {
        return non_empty(name).map(|n| V2Route::TagsList { name: n });
    }
    // 启动上传：以 `/blobs/uploads/` 结尾（uuid 为空）
    if let Some(name) = rest.strip_suffix("/blobs/uploads/") {
        return non_empty(name).map(|n| V2Route::StartUpload { name: n });
    }
    // 续传 / 完成上传：含 `/blobs/uploads/{uuid}`
    if let Some((name, uuid)) = split_marker(rest, "/blobs/uploads/") {
        return Some(V2Route::Upload { name, uuid });
    }
    // blob 读取：含 `/blobs/{digest}`
    if let Some((name, digest)) = split_marker(rest, "/blobs/") {
        return Some(V2Route::Blob { name, digest });
    }
    // manifest 存取：含 `/manifests/{reference}`
    if let Some((name, reference)) = split_marker(rest, "/manifests/") {
        return Some(V2Route::Manifest { name, reference });
    }
    None
}

/// 按后缀标记切分为 `(标记前 name, 标记后 tail)`，两侧均非空才成立。
fn split_marker(rest: &str, marker: &str) -> Option<(String, String)> {
    let (name, tail) = rest.split_once(marker)?;
    if name.is_empty() || tail.is_empty() {
        return None;
    }
    Some((name.to_string(), tail.to_string()))
}

/// 非空字符串过滤工具。
fn non_empty(s: &str) -> Option<String> {
    if s.is_empty() {
        None
    } else {
        Some(s.to_string())
    }
}

// ---------------- 方法分发器（catch-all `/v2/{*path}` 按方法路由） ----------------

/// POST 分发：仅 `{name}/blobs/uploads/` 合法（启动上传）。
pub async fn dispatch_post(
    state: State<AppState>,
    auth: DockerAuth,
    Path(rest): Path<String>,
    q: Query<DigestQuery>,
    body: Body,
) -> Response {
    match parse_v2_route(&rest) {
        Some(V2Route::StartUpload { name }) => start_blob_upload(state, auth, name, q, body).await,
        _ => DockerApiError::not_found("NAME_UNKNOWN").into_response(),
    }
}

/// PATCH 分发：仅 `{name}/blobs/uploads/{uuid}` 合法（续传）。
pub async fn dispatch_patch(
    state: State<AppState>,
    auth: DockerAuth,
    Path(rest): Path<String>,
    body: Body,
) -> Response {
    match parse_v2_route(&rest) {
        Some(V2Route::Upload { name, uuid }) => {
            patch_blob_upload(state, auth, name, uuid, body).await
        }
        _ => DockerApiError::not_found("NAME_UNKNOWN").into_response(),
    }
}

/// PUT 分发：`{name}/blobs/uploads/{uuid}`（完成上传）或 `{name}/manifests/{ref}`（写 manifest）。
pub async fn dispatch_put(
    state: State<AppState>,
    auth: DockerAuth,
    Path(rest): Path<String>,
    q: Query<DigestQuery>,
    headers: axum::http::HeaderMap,
    body: axum::body::Bytes,
) -> Response {
    match parse_v2_route(&rest) {
        Some(V2Route::Upload { name, uuid }) => {
            // 完成上传需把已读 body 作为末段；这里 body 已聚合为 Bytes（manifest 与完成上传共用 PUT）
            put_blob_upload(state, auth, name, uuid, q, body).await
        }
        Some(V2Route::Manifest { name, reference }) => {
            put_manifest(state, auth, name, reference, headers, body).await
        }
        _ => DockerApiError::not_found("NAME_UNKNOWN").into_response(),
    }
}

/// GET 分发：`/v2/`（版本检查由专门路由处理）、blob 或 manifest 读取。
pub async fn dispatch_get(
    state: State<AppState>,
    auth: DockerAuth,
    Path(rest): Path<String>,
) -> Response {
    match parse_v2_route(&rest) {
        Some(V2Route::Blob { name, digest }) => {
            blob_request(state.0, auth, name, digest, true).await
        }
        Some(V2Route::Manifest { name, reference }) => {
            manifest_request(state.0, auth, name, reference, true).await
        }
        Some(V2Route::TagsList { name }) => tags_list(state.0, auth, name).await,
        _ => DockerApiError::not_found("NAME_UNKNOWN").into_response(),
    }
}

/// HEAD 分发：blob 或 manifest 存在性检查。
pub async fn dispatch_head(
    state: State<AppState>,
    auth: DockerAuth,
    Path(rest): Path<String>,
) -> Response {
    match parse_v2_route(&rest) {
        Some(V2Route::Blob { name, digest }) => {
            blob_request(state.0, auth, name, digest, false).await
        }
        Some(V2Route::Manifest { name, reference }) => {
            manifest_request(state.0, auth, name, reference, false).await
        }
        _ => DockerApiError::not_found("NAME_UNKNOWN").into_response(),
    }
}

/// tag 列表（`GET /v2/{name}/tags/list`）：返回该镜像在本仓库下的全部 tag。
///
/// 经读授权（private 对无权 → 404/401，与 manifest 读一致）；tag 取自存储中
/// `{image}/tags/{tag}` 指针索引。无任何 tag 视为名称未知，返回 404 NAME_UNKNOWN。
async fn tags_list(state: AppState, auth: DockerAuth, name: String) -> Response {
    let (repo_name, image) = match split_name(&name) {
        Some(v) => v,
        None => return DockerApiError::not_found("NAME_UNKNOWN").into_response(),
    };
    let repo = match load_readable_repo(&state, &auth, &name, &repo_name).await {
        Ok(r) => r,
        Err(e) => return e.into_response(),
    };
    if let Err(e) = ensure_docker(&repo) {
        return e.into_response();
    }
    let arts = match state.meta.list_artifacts_by_repo(&repo.id).await {
        Ok(a) => a,
        Err(e) => return map_docker_error(DockerError::Meta(e)).into_response(),
    };
    // tag 指针存储键形如 `{image}/tags/{tag}`：按前缀筛出本镜像的 tag。
    let prefix = format!("{image}/tags/");
    let mut tags: Vec<String> = arts
        .iter()
        .filter_map(|a| a.path.strip_prefix(&prefix))
        .filter(|t| !t.is_empty() && !t.contains('/'))
        .map(|t| t.to_string())
        .collect();
    tags.sort();
    tags.dedup();
    if tags.is_empty() {
        return DockerApiError::not_found("NAME_UNKNOWN").into_response();
    }
    let mut resp = Json(json!({ "name": name, "tags": tags })).into_response();
    resp.headers_mut().insert(
        API_VERSION_HEADER,
        HeaderValue::from_static(API_VERSION_VALUE),
    );
    resp
}

// ---------------- blob 上传状态机 ----------------

/// 启动 blob 上传（`POST /v2/{name}/blobs/uploads/`）：返回 202 + Location（含 uuid）。
async fn start_blob_upload(
    State(state): State<AppState>,
    auth: DockerAuth,
    name: String,
    Query(q): Query<DigestQuery>,
    body: Body,
) -> Response {
    let (repo_name, image) = match split_name(&name) {
        Some(v) => v,
        None => return DockerApiError::not_found("NAME_UNKNOWN").into_response(),
    };
    let repo = match load_writable_repo(&state, &auth, &name, &repo_name).await {
        Ok(r) => r,
        Err(e) => return e.into_response(),
    };
    if let Err(e) = ensure_docker(&repo) {
        return e.into_response();
    }
    let reg = match registry(&state) {
        Ok(r) => r,
        Err(e) => return e.into_response(),
    };

    let started = match reg.start_upload().await {
        Ok(s) => s,
        Err(e) => return map_docker_error(e).into_response(),
    };

    // 单体上传：POST 直接带 digest 与 body（POST-then-PUT 的合并形态）
    if let Some(digest) = q.digest {
        let reader = StreamReader::new(
            body.into_data_stream()
                .map_err(|e| std::io::Error::other(e.to_string())),
        );
        if let Err(e) = reg.append_upload(&started.upload_id, reader).await {
            reg.cancel_upload(&started.upload_id).await;
            return map_docker_error(e).into_response();
        }
        return finalize_blob(&state, &repo, &image, &started.upload_id, &digest).await;
    }

    // 分块上传：返回 202 + Location，客户端后续 PATCH / PUT
    let loc = location(
        &state,
        &format!(
            "/v2/{repo_name}/{image}/blobs/uploads/{}",
            started.upload_id
        ),
    );
    upload_accepted(&loc, &started.upload_id, 0)
}

/// 追加 blob 分块（`PATCH /v2/{name}/blobs/uploads/{uuid}`）：流式写入，返回 202 + Range。
async fn patch_blob_upload(
    State(state): State<AppState>,
    auth: DockerAuth,
    name: String,
    uuid: String,
    body: Body,
) -> Response {
    let (repo_name, image) = match split_name(&name) {
        Some(v) => v,
        None => return DockerApiError::not_found("NAME_UNKNOWN").into_response(),
    };
    let repo = match load_writable_repo(&state, &auth, &name, &repo_name).await {
        Ok(r) => r,
        Err(e) => return e.into_response(),
    };
    if let Err(e) = ensure_docker(&repo) {
        return e.into_response();
    }
    let reg = match registry(&state) {
        Ok(r) => r,
        Err(e) => return e.into_response(),
    };

    let reader = StreamReader::new(
        body.into_data_stream()
            .map_err(|e| std::io::Error::other(e.to_string())),
    );
    let outcome = match reg.append_upload(&uuid, reader).await {
        Ok(o) => o,
        Err(e) => {
            // 超限等错误：取消会话清理临时文件
            if matches!(e, DockerError::TooLarge) {
                reg.cancel_upload(&uuid).await;
            }
            return map_docker_error(e).into_response();
        }
    };

    let loc = location(
        &state,
        &format!("/v2/{repo_name}/{image}/blobs/uploads/{uuid}"),
    );
    upload_accepted(&loc, &uuid, outcome.written)
}

/// 完成 blob 上传（`PUT /v2/{name}/blobs/uploads/{uuid}?digest=...`）：可携末段 body。
async fn put_blob_upload(
    State(state): State<AppState>,
    auth: DockerAuth,
    name: String,
    uuid: String,
    Query(q): Query<DigestQuery>,
    body: axum::body::Bytes,
) -> Response {
    let (repo_name, image) = match split_name(&name) {
        Some(v) => v,
        None => return DockerApiError::not_found("NAME_UNKNOWN").into_response(),
    };
    let repo = match load_writable_repo(&state, &auth, &name, &repo_name).await {
        Ok(r) => r,
        Err(e) => return e.into_response(),
    };
    if let Err(e) = ensure_docker(&repo) {
        return e.into_response();
    }
    let reg = match registry(&state) {
        Ok(r) => r,
        Err(e) => return e.into_response(),
    };

    let digest = match q.digest {
        Some(d) => d,
        None => {
            return DockerApiError::new(
                StatusCode::BAD_REQUEST,
                "DIGEST_INVALID",
                "完成上传需提供 digest",
            )
            .into_response()
        }
    };

    // PUT 可能携带最后一段字节（先追加再完成）；末段通常很小或为空
    if !body.is_empty() {
        let reader = std::io::Cursor::new(body);
        if let Err(e) = reg.append_upload(&uuid, reader).await {
            if matches!(e, DockerError::TooLarge) {
                reg.cancel_upload(&uuid).await;
            }
            return map_docker_error(e).into_response();
        }
    }

    finalize_blob(&state, &repo, &image, &uuid, &digest).await
}

/// 完成上传并组装 201 响应（含 Location 与 Docker-Content-Digest）。
async fn finalize_blob(
    state: &AppState,
    repo: &RepositoryRecord,
    image: &str,
    upload_id: &str,
    digest: &str,
) -> Response {
    let reg = match registry(state) {
        Ok(r) => r,
        Err(e) => return e.into_response(),
    };
    match reg.finish_upload(repo, image, upload_id, digest).await {
        Ok(final_digest) => {
            let loc = location(
                state,
                &format!("/v2/{}/{}/blobs/{}", repo.name, image, final_digest),
            );
            let mut resp = StatusCode::CREATED.into_response();
            let h = resp.headers_mut();
            insert_api_version(h);
            if let Ok(v) = HeaderValue::from_str(&loc) {
                h.insert(header::LOCATION, v);
            }
            if let Ok(v) = HeaderValue::from_str(&final_digest) {
                h.insert(CONTENT_DIGEST_HEADER, v);
            }
            resp
        }
        Err(e) => map_docker_error(e).into_response(),
    }
}

/// 组装上传进行中的 202 响应（Location + Range + Upload-UUID）。
fn upload_accepted(location: &str, uuid: &str, written: u64) -> Response {
    let mut resp = StatusCode::ACCEPTED.into_response();
    let h = resp.headers_mut();
    insert_api_version(h);
    if let Ok(v) = HeaderValue::from_str(location) {
        h.insert(header::LOCATION, v);
    }
    if let Ok(v) = HeaderValue::from_str(uuid) {
        h.insert("Docker-Upload-UUID", v);
    }
    // Range 表示已接收字节区间（0-N），N 为最后一个已写字节的偏移
    let range = if written == 0 {
        "0-0".to_string()
    } else {
        format!("0-{}", written - 1)
    };
    if let Ok(v) = HeaderValue::from_str(&range) {
        h.insert(header::RANGE, v);
    }
    resp
}

// ---------------- blob 读取 ----------------

/// blob 读取公共流程：`with_body` 区分 GET（带体）与 HEAD（仅头）。
async fn blob_request(
    state: AppState,
    auth: DockerAuth,
    name: String,
    digest: String,
    with_body: bool,
) -> Response {
    let (repo_name, image) = match split_name(&name) {
        Some(v) => v,
        None => return DockerApiError::not_found("NAME_UNKNOWN").into_response(),
    };
    let repo = match load_readable_repo(&state, &auth, &name, &repo_name).await {
        Ok(r) => r,
        Err(e) => return e.into_response(),
    };
    if let Err(e) = ensure_docker(&repo) {
        return e.into_response();
    }
    let reg = match registry(&state) {
        Ok(r) => r,
        Err(e) => return e.into_response(),
    };

    let handle = match reg.get_blob(&repo, &image, &digest).await {
        Ok(h) => h,
        Err(e) => return map_docker_error(e).into_response(),
    };

    let body = if with_body {
        Body::from_stream(ReaderStream::new(handle.blob))
    } else {
        Body::empty()
    };
    Response::builder()
        .status(StatusCode::OK)
        .header(API_VERSION_HEADER, API_VERSION_VALUE)
        .header(CONTENT_DIGEST_HEADER, handle.digest)
        .header(header::CONTENT_TYPE, "application/octet-stream")
        .header(header::CONTENT_LENGTH, handle.size)
        .body(body)
        .unwrap_or_else(|_| StatusCode::INTERNAL_SERVER_ERROR.into_response())
}

// ---------------- manifest 存取 ----------------

/// PUT manifest（`PUT /v2/{name}/manifests/{reference}`）：写入并返回 201 + digest 头。
async fn put_manifest(
    State(state): State<AppState>,
    auth: DockerAuth,
    name: String,
    reference: String,
    headers: axum::http::HeaderMap,
    body: axum::body::Bytes,
) -> Response {
    let (repo_name, image) = match split_name(&name) {
        Some(v) => v,
        None => return DockerApiError::not_found("NAME_UNKNOWN").into_response(),
    };
    let repo = match load_writable_repo(&state, &auth, &name, &repo_name).await {
        Ok(r) => r,
        Err(e) => return e.into_response(),
    };
    if let Err(e) = ensure_docker(&repo) {
        return e.into_response();
    }
    let reg = match registry(&state) {
        Ok(r) => r,
        Err(e) => return e.into_response(),
    };

    // 媒体类型取自 Content-Type 头（docker push 必带），缺失则用默认 schema2
    let media_type = headers
        .get(header::CONTENT_TYPE)
        .and_then(|v| v.to_str().ok())
        .map(str::to_string)
        .unwrap_or_else(|| docker::MEDIA_TYPE_MANIFEST_V2.to_string());

    match reg
        .put_manifest(&repo, &image, &reference, &media_type, body.to_vec())
        .await
    {
        Ok(digest) => {
            let loc = location(
                &state,
                &format!("/v2/{repo_name}/{image}/manifests/{reference}"),
            );
            let mut resp = StatusCode::CREATED.into_response();
            let h = resp.headers_mut();
            insert_api_version(h);
            if let Ok(v) = HeaderValue::from_str(&loc) {
                h.insert(header::LOCATION, v);
            }
            if let Ok(v) = HeaderValue::from_str(&digest) {
                h.insert(CONTENT_DIGEST_HEADER, v);
            }
            resp
        }
        Err(e) => map_docker_error(e).into_response(),
    }
}

/// manifest 读取公共流程：`with_body` 区分 GET 与 HEAD。
async fn manifest_request(
    state: AppState,
    auth: DockerAuth,
    name: String,
    reference: String,
    with_body: bool,
) -> Response {
    let (repo_name, image) = match split_name(&name) {
        Some(v) => v,
        None => return DockerApiError::not_found("NAME_UNKNOWN").into_response(),
    };
    let repo = match load_readable_repo(&state, &auth, &name, &repo_name).await {
        Ok(r) => r,
        Err(e) => return e.into_response(),
    };
    if let Err(e) = ensure_docker(&repo) {
        return e.into_response();
    }
    let reg = match registry(&state) {
        Ok(r) => r,
        Err(e) => return e.into_response(),
    };

    let handle = match reg.get_manifest(&repo, &image, &reference).await {
        Ok(h) => h,
        Err(DockerError::NotFound) => {
            return DockerApiError::not_found("MANIFEST_UNKNOWN").into_response()
        }
        Err(e) => return map_docker_error(e).into_response(),
    };

    let len = handle.bytes.len();
    let body = if with_body {
        Body::from(handle.bytes)
    } else {
        Body::empty()
    };
    Response::builder()
        .status(StatusCode::OK)
        .header(API_VERSION_HEADER, API_VERSION_VALUE)
        .header(CONTENT_DIGEST_HEADER, handle.digest)
        .header(header::CONTENT_TYPE, handle.media_type)
        .header(header::CONTENT_LENGTH, len)
        .body(body)
        .unwrap_or_else(|_| StatusCode::INTERNAL_SERVER_ERROR.into_response())
}

/// 插入 API 版本头。
fn insert_api_version(headers: &mut axum::http::HeaderMap) {
    headers.insert(
        API_VERSION_HEADER,
        HeaderValue::from_static(API_VERSION_VALUE),
    );
}
