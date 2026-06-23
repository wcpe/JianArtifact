//! API Token 管理端点（FR-02）：用户自助签发 / 列出 / 吊销自己的 Token。
//!
//! 明文 Token 仅在签发时返回一次，服务端只存其哈希；列表不回显明文与哈希。

use axum::{
    extract::{Path, State},
    Json,
};
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};

use crate::auth::{generate_api_token, hash_api_token};
use crate::meta::TokenRecord;

use super::{ApiError, AppState, Identity};

/// 签发 Token 请求体。
#[derive(Debug, Deserialize)]
pub struct CreateTokenRequest {
    /// Token 名称（便于辨识用途）。
    pub name: String,
}

/// 签发 Token 成功返回体：元数据 + 仅本次可见的明文 Token。
#[derive(Debug, Serialize)]
pub struct CreateTokenResponse {
    /// Token 主键。
    pub id: String,
    /// Token 名称。
    pub name: String,
    /// 创建时间。
    pub created_at: String,
    /// 明文 Token，仅本次返回，此后不可再得。
    pub token: String,
}

/// Token 元数据视图（不含明文与哈希）。
#[derive(Debug, Serialize)]
pub struct TokenView {
    /// Token 主键。
    pub id: String,
    /// Token 名称。
    pub name: String,
    /// 创建时间。
    pub created_at: String,
    /// 最近使用时间；从未使用为 null。
    pub last_used_at: Option<String>,
    /// 是否已吊销。
    pub revoked: bool,
}

impl From<TokenRecord> for TokenView {
    fn from(r: TokenRecord) -> Self {
        Self {
            id: r.id,
            name: r.name,
            created_at: r.created_at,
            last_used_at: r.last_used_at,
            revoked: r.revoked != 0,
        }
    }
}

/// 为当前用户签发一枚 API Token。
pub async fn create_token(
    State(state): State<AppState>,
    identity: Identity,
    Json(req): Json<CreateTokenRequest>,
) -> Result<(axum::http::StatusCode, Json<CreateTokenResponse>), ApiError> {
    let user = identity.require_authenticated()?;
    if req.name.is_empty() {
        return Err(ApiError::BadRequest("Token 名称不能为空".to_string()));
    }

    let plaintext = generate_api_token();
    let hash = hash_api_token(&plaintext);
    let id = state.meta.create_token(&user.user_id, &req.name, &hash).await?;

    tracing::info!(用户 = %user.username, token名 = %req.name, "已签发 API Token");
    let record = state
        .meta
        .get_token_by_id(&id)
        .await?
        .ok_or(ApiError::Internal)?;

    Ok((
        axum::http::StatusCode::CREATED,
        Json(CreateTokenResponse {
            id: record.id,
            name: record.name,
            created_at: record.created_at,
            // 明文仅此一次返回
            token: plaintext,
        }),
    ))
}

/// 列出当前用户自己的全部 Token 元数据。
pub async fn list_tokens(
    State(state): State<AppState>,
    identity: Identity,
) -> Result<Json<Vec<TokenView>>, ApiError> {
    let user = identity.require_authenticated()?;
    let tokens = state.meta.list_tokens_by_user(&user.user_id).await?;
    Ok(Json(tokens.into_iter().map(TokenView::from).collect()))
}

/// 吊销当前用户自己的 Token；非本人 Token 返回 403，不存在返回 404。
pub async fn revoke_token(
    State(state): State<AppState>,
    identity: Identity,
    Path(id): Path<String>,
) -> Result<Json<Value>, ApiError> {
    let user = identity.require_authenticated()?;
    let token = state.meta.get_token_by_id(&id).await?.ok_or(ApiError::NotFound)?;
    // 仅可吊销本人 Token，避免越权操作他人凭据
    if token.user_id != user.user_id {
        return Err(ApiError::Forbidden);
    }
    state.meta.revoke_token(&id).await?;
    tracing::info!(用户 = %user.username, token = %id, "已吊销 API Token");
    Ok(Json(json!({ "status": "ok" })))
}
