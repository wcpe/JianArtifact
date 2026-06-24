//! 认证端点：登录、登出、刷新会话、当前用户（ADR-0003 / ADR-0011）。
//!
//! Web 会话以无状态 JWT 承载，放 `Authorization: Bearer` 头（不走 Cookie）。
//! 登出在无状态 JWT 下由客户端丢弃令牌完成，服务端返回 200（不维护服务端 denylist，
//! 该增强属可选项，本批不实现）。

use axum::{extract::State, http::HeaderMap, Json};
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};

use crate::auth::verify_password;

use super::{ApiError, AppState, AuditResult, ClientIp, Identity};

/// 请求 ID 头名称（与 api::mod 设置保持一致），登录审计据此关联请求。
const REQUEST_ID_HEADER: &str = "x-request-id";

/// 登录请求体。
#[derive(Debug, Deserialize)]
pub struct LoginRequest {
    /// 用户名。
    pub username: String,
    /// 口令。
    pub password: String,
}

/// 登录 / 刷新成功返回体：访问令牌、有效期与用户信息。
#[derive(Debug, Serialize)]
pub struct LoginResponse {
    /// 访问令牌（JWT）。
    pub access_token: String,
    /// 令牌类型，固定 `Bearer`。
    pub token_type: &'static str,
    /// 有效期（秒）。
    pub expires_in: u64,
    /// 当前用户信息。
    pub user: UserInfo,
}

/// 对外暴露的用户公开信息（不含口令哈希等敏感项）。
#[derive(Debug, Serialize)]
pub struct UserInfo {
    /// 用户主键。
    pub id: String,
    /// 用户名。
    pub username: String,
    /// 全局角色字符串（admin | user）。
    pub role: String,
}

/// 登录：校验口令，签发 JWT；含登录暴力破解防护（FR-65）。
///
/// 登录事件由本 handler 显式记审计（因需记被尝试的用户名）；脱敏：只记用户名，绝不记口令。
pub async fn login(
    State(state): State<AppState>,
    ClientIp(client_ip): ClientIp,
    headers: HeaderMap,
    Json(req): Json<LoginRequest>,
) -> Result<Json<LoginResponse>, ApiError> {
    if req.username.is_empty() || req.password.is_empty() {
        return Err(ApiError::BadRequest("用户名与口令不能为空".to_string()));
    }
    // 关联请求 ID（供审计追溯）
    let request_id = headers
        .get(REQUEST_ID_HEADER)
        .and_then(|v| v.to_str().ok())
        .map(str::to_owned);
    let request_id = request_id.as_deref();
    let source_ip = Some(client_ip.as_str());

    // 锁定检查：达阈值则限流
    if let Err(e) = state.login_guard.check(&req.username, &client_ip) {
        state
            .audit
            .record_login(&req.username, AuditResult::Denied, source_ip, request_id);
        return Err(ApiError::TooManyRequests(e.retry_after_secs));
    }

    let user = state.meta.get_user_by_username(&req.username).await?;
    let user = match user {
        Some(u) if verify_password(&req.password, &u.password_hash) => u,
        _ => {
            // 用户不存在或口令错误：均记一次失败并统一返回 401，不泄露存在性
            state.login_guard.record_failure(&req.username, &client_ip);
            state
                .audit
                .record_login(&req.username, AuditResult::Denied, source_ip, request_id);
            tracing::warn!(用户名 = %req.username, "登录失败：用户名或口令错误");
            return Err(ApiError::Unauthorized);
        }
    };

    if user.disabled != 0 {
        state
            .audit
            .record_login(&req.username, AuditResult::Denied, source_ip, request_id);
        tracing::warn!(用户名 = %req.username, "登录被拒：账户已禁用");
        return Err(ApiError::AccountDisabled);
    }

    // 登录成功：清零失败计数并签发会话
    state.login_guard.record_success(&req.username, &client_ip);
    state
        .audit
        .record_login(&user.username, AuditResult::Success, source_ip, request_id);
    let role = crate::meta::Role::from_db_str(&user.role);
    let token = state
        .jwt
        .issue(&user.id, &user.username, role)
        .map_err(|e| {
            tracing::error!(错误 = %e, "签发 JWT 失败");
            ApiError::Internal
        })?;
    tracing::info!(用户名 = %user.username, "登录成功，已签发会话");

    Ok(Json(LoginResponse {
        access_token: token,
        token_type: "Bearer",
        expires_in: state.jwt.ttl_secs(),
        user: UserInfo {
            id: user.id,
            username: user.username,
            role: user.role,
        },
    }))
}

/// 登出：无状态 JWT 下由客户端丢弃令牌，服务端返回 200。
///
/// 需已认证（避免匿名误调）；服务端不维护 denylist（可选增强，本批不做）。
pub async fn logout(identity: Identity) -> Result<Json<Value>, ApiError> {
    identity.require_authenticated()?;
    Ok(Json(json!({ "status": "ok" })))
}

/// 刷新会话：凭未过期的 JWT 会话换发新 JWT，过期 / 无效则 401。
pub async fn refresh(
    State(state): State<AppState>,
    identity: Identity,
) -> Result<Json<LoginResponse>, ApiError> {
    let user = identity.require_authenticated()?;
    let token = state
        .jwt
        .issue(&user.user_id, &user.username, user.role)
        .map_err(|e| {
            tracing::error!(错误 = %e, "刷新签发 JWT 失败");
            ApiError::Internal
        })?;
    Ok(Json(LoginResponse {
        access_token: token,
        token_type: "Bearer",
        expires_in: state.jwt.ttl_secs(),
        user: UserInfo {
            id: user.user_id.clone(),
            username: user.username.clone(),
            role: user.role.as_str().to_string(),
        },
    }))
}

/// 当前用户：返回调用方信息；未认证 401。
pub async fn me(identity: Identity) -> Result<Json<UserInfo>, ApiError> {
    let user = identity.require_authenticated()?;
    Ok(Json(UserInfo {
        id: user.user_id.clone(),
        username: user.username.clone(),
        role: user.role.as_str().to_string(),
    }))
}
