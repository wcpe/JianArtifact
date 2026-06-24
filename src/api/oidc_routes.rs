//! OIDC 授权码流端点（FR-34 / ADR-0016）。
//!
//! `GET /api/v1/auth/oidc/login`：生成 state + PKCE + nonce，重定向到 IdP 授权端点。
//! `GET /api/v1/auth/oidc/callback`：校验 state、换码、校验 ID Token（签名 / iss / aud / exp /
//! nonce），经「外部身份 → 本地用户」映射（守 ADR-0010）得本地用户后，**照常签发既有会话 JWT**。
//!
//! 本 handler 保持轻薄：协议适配 + 编排，OIDC 校验逻辑在 `auth::oidc`，映射逻辑在
//! `auth::resolve_external_login`。凭据脱敏：state / code / ID Token 等绝不进日志。

use std::collections::HashMap;
use std::sync::Arc;
use std::sync::Mutex;
use std::time::{Duration, Instant};

use axum::{
    extract::{Query, State},
    response::{IntoResponse, Redirect, Response},
};
use serde::Deserialize;

use crate::auth::oidc::FlowState;

use super::{ApiError, AppState, AuditResult, ClientIp};

/// 登录流程状态的存活时长：超过即视为过期（防 state 被长期重放）。
const FLOW_TTL: Duration = Duration::from_secs(600);
/// 流程表条目数硬上限：兜底防止异常请求撑爆内存（超限时清理过期项后仍满则拒新流程）。
const FLOW_MAX_ENTRIES: usize = 10_000;

/// 一条暂存的登录流程：按 `state` 一次性绑定 PKCE / nonce，回调时取出并消费。
struct StoredFlow {
    /// PKCE / nonce 等流程参数。
    flow: FlowState,
    /// 创建时刻，用于过期判定。
    created: Instant,
}

/// OIDC 登录流程的进程内短期存储：按 `state` 暂存，回调时一次性取出并删除（防重放）。
///
/// 仅内存态、单飞一次性；条目过期或被消费即移除。临界区只做表的增删查，无锁外 IO。
#[derive(Default)]
pub struct OidcFlowStore {
    inner: Mutex<HashMap<String, StoredFlow>>,
}

impl OidcFlowStore {
    /// 新建空存储。
    pub fn new() -> Self {
        Self::default()
    }

    /// 暂存一条流程（按 state 键）。表满时先清过期项，仍满则丢弃最旧逻辑由调用方承担——
    /// 这里简单拒绝（返回 false），调用方据此回错，避免无界增长。
    fn insert(&self, state: String, flow: FlowState) -> bool {
        let mut map = self.inner.lock().unwrap_or_else(|e| e.into_inner());
        // 顺带清理过期项
        let now = Instant::now();
        map.retain(|_, v| now.duration_since(v.created) < FLOW_TTL);
        if map.len() >= FLOW_MAX_ENTRIES {
            return false;
        }
        map.insert(state, StoredFlow { flow, created: now });
        true
    }

    /// 按 state 一次性取出流程（取出即删除）；不存在或已过期返回 None。
    fn take(&self, state: &str) -> Option<FlowState> {
        let mut map = self.inner.lock().unwrap_or_else(|e| e.into_inner());
        let stored = map.remove(state)?;
        if Instant::now().duration_since(stored.created) >= FLOW_TTL {
            return None;
        }
        Some(stored.flow)
    }
}

/// 回调查询参数。
#[derive(Debug, Deserialize)]
pub struct CallbackParams {
    /// 授权码。
    code: Option<String>,
    /// 防 CSRF 的 state，须与登录时下发的一致。
    state: Option<String>,
    /// IdP 返回的错误码（用户取消 / 拒绝授权等）。
    error: Option<String>,
}

/// 取 OIDC provider 与流程存储；未配置 OIDC 时返回 404（端点视为不存在）。
fn require_oidc(
    state: &AppState,
) -> Result<(Arc<crate::auth::OidcProvider>, Arc<OidcFlowStore>), ApiError> {
    match (&state.oidc, &state.oidc_flows) {
        (Some(provider), flows) => Ok((provider.clone(), flows.clone())),
        _ => Err(ApiError::NotFound),
    }
}

/// `GET /api/v1/auth/oidc/login`：生成流程参数并重定向到 IdP 授权端点。
pub async fn oidc_login(State(state): State<AppState>) -> Result<Response, ApiError> {
    let (provider, flows) = require_oidc(&state)?;
    let (auth_url, csrf_state, flow) = provider.begin_login().await.map_err(|e| {
        tracing::warn!(错误 = %e, "OIDC 发起登录失败（discovery 不可用）");
        ApiError::BadGateway
    })?;
    if !flows.insert(csrf_state, flow) {
        tracing::warn!("OIDC 登录流程表已满，拒绝新流程");
        return Err(ApiError::TooManyRequests(60));
    }
    // 302 重定向到 IdP；不记录 auth_url 内的 state / nonce 细节
    Ok(Redirect::to(&auth_url).into_response())
}

/// `GET /api/v1/auth/oidc/callback`：校验 state、换码、校验 ID Token、映射本地用户并签发会话。
pub async fn oidc_callback(
    State(state): State<AppState>,
    ClientIp(client_ip): ClientIp,
    Query(params): Query<CallbackParams>,
) -> Result<Response, ApiError> {
    let (provider, flows) = require_oidc(&state)?;
    let source_ip = Some(client_ip.as_str());

    // IdP 直接回错（用户取消 / 拒绝）：不泄露细节，统一 401
    if params.error.is_some() {
        tracing::warn!("OIDC 回调携带 error，认证未完成");
        return Err(ApiError::Unauthorized);
    }
    let code = params
        .code
        .ok_or(ApiError::BadRequest("缺少授权码".into()))?;
    let csrf_state = params
        .state
        .ok_or(ApiError::BadRequest("缺少 state".into()))?;

    // state 校验：取出对应流程（取出即消费，防重放）；不存在 / 过期即 CSRF 失败
    let flow = flows.take(&csrf_state).ok_or_else(|| {
        tracing::warn!("OIDC 回调 state 校验失败（不存在 / 已过期 / 已消费）");
        ApiError::Unauthorized
    })?;

    // 换码并校验 ID Token（签名 / iss / aud / exp / nonce）
    let subject = provider.complete_login(&code, &flow).await.map_err(|e| {
        tracing::warn!(错误 = %e, "OIDC 换码或 ID Token 校验失败");
        ApiError::Unauthorized
    })?;

    // 外部身份 → 本地用户映射（守 ADR-0010：JIT 默认关、默认角色 User）
    let user =
        match crate::auth::resolve_external_login(&state.meta, &subject, provider.auto_provision())
            .await
        {
            Ok(u) => u,
            Err(e) => {
                // 拒绝原因记审计（用建议用户名，绝不记外部凭据 / ID Token）
                state.audit.record_login(
                    &subject.preferred_username,
                    AuditResult::Denied,
                    source_ip,
                    None,
                );
                tracing::warn!(原因 = %e, "OIDC 外部身份映射本地用户失败，拒绝登录");
                return Err(ApiError::Unauthorized);
            }
        };

    // 照常签发既有会话 JWT（TTL / 刷新 / 登出与 ADR-0011 一致）
    let role = crate::meta::Role::from_db_str(&user.role);
    let token = state
        .jwt
        .issue(&user.id, &user.username, role)
        .map_err(|e| {
            tracing::error!(错误 = %e, "OIDC 登录签发 JWT 失败");
            ApiError::Internal
        })?;
    state
        .audit
        .record_login(&user.username, AuditResult::Success, source_ip, None);
    tracing::info!(用户名 = %user.username, "OIDC 登录成功，已签发会话");

    // 回跳前端，把会话令牌经 fragment 交给 SPA（fragment 不进服务端日志 / Referer）。
    // SPA 在 /login 路由解析 fragment 中的 access_token 落地会话。
    let redirect = format!("/login#access_token={token}&token_type=Bearer");
    Ok(Redirect::to(&redirect).into_response())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::auth::oidc::FlowState;

    fn 造流程() -> FlowState {
        FlowState {
            code_verifier: "verifier".into(),
            nonce: "nonce".into(),
        }
    }

    #[test]
    fn 流程暂存后可一次性取出且不可重复取() {
        let store = OidcFlowStore::new();
        assert!(store.insert("st1".into(), 造流程()));
        // 首次取出成功
        let f = store.take("st1").unwrap();
        assert_eq!(f.nonce, "nonce");
        // 再取即不存在（一次性，防重放）
        assert!(store.take("st1").is_none());
    }

    #[test]
    fn 取不存在的_state_返回_none() {
        let store = OidcFlowStore::new();
        assert!(store.take("不存在").is_none());
    }
}
