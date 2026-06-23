//! 用户管理端点（FR-04 / FR-05）：仅管理员可操作。
//!
//! 创建用户口令以 argon2 哈希存储；任何响应都不回显 `password_hash`。

use axum::{
    extract::{Path, State},
    Json,
};
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};

use crate::auth::hash_password;
use crate::meta::{Role, UserRecord};

use super::{ApiError, AppState, Identity};

/// 对外用户视图（不含口令哈希）。
#[derive(Debug, Serialize)]
pub struct UserView {
    /// 用户主键。
    pub id: String,
    /// 用户名。
    pub username: String,
    /// 全局角色字符串（admin | user）。
    pub role: String,
    /// 是否被禁用。
    pub disabled: bool,
    /// 创建时间（ISO8601）。
    pub created_at: String,
}

impl From<UserRecord> for UserView {
    fn from(r: UserRecord) -> Self {
        Self {
            id: r.id,
            username: r.username,
            role: r.role,
            disabled: r.disabled != 0,
            created_at: r.created_at,
        }
    }
}

/// 创建用户请求体。
#[derive(Debug, Deserialize)]
pub struct CreateUserRequest {
    /// 用户名。
    pub username: String,
    /// 口令（明文，仅用于服务端计算 argon2 哈希，不入库）。
    pub password: String,
    /// 角色字符串（Admin | User，大小写不敏感）。
    pub role: String,
}

/// 更新用户请求体：字段可选，仅更新提供的项。
#[derive(Debug, Deserialize)]
pub struct UpdateUserRequest {
    /// 角色字符串（Admin | User，大小写不敏感）。
    #[serde(default)]
    pub role: Option<String>,
    /// 禁用 / 启用。
    #[serde(default)]
    pub disabled: Option<bool>,
}

/// 列出全部用户（仅管理员）。
pub async fn list_users(
    State(state): State<AppState>,
    identity: Identity,
) -> Result<Json<Vec<UserView>>, ApiError> {
    identity.require_admin()?;
    let users = state.meta.list_users().await?;
    Ok(Json(users.into_iter().map(UserView::from).collect()))
}

/// 创建用户（仅管理员）。
pub async fn create_user(
    State(state): State<AppState>,
    identity: Identity,
    Json(req): Json<CreateUserRequest>,
) -> Result<(axum::http::StatusCode, Json<UserView>), ApiError> {
    identity.require_admin()?;
    if req.username.is_empty() || req.password.is_empty() {
        return Err(ApiError::BadRequest("用户名与口令不能为空".to_string()));
    }
    let role = parse_role(&req.role)?;

    let hash = hash_password(&req.password).map_err(|e| {
        tracing::error!(错误 = %e, "口令哈希失败");
        ApiError::Internal
    })?;

    // 唯一约束冲突映射为 409；其余 DB 错误为内部错误
    let id = match state.meta.create_user(&req.username, &hash, role).await {
        Ok(id) => id,
        Err(crate::meta::MetaError::Database(sqlx::Error::Database(db)))
            if db.is_unique_violation() =>
        {
            return Err(ApiError::Conflict("用户名已存在".to_string()));
        }
        Err(e) => return Err(e.into()),
    };

    tracing::info!(用户名 = %req.username, "已创建用户");
    let created = state
        .meta
        .get_user_by_id(&id)
        .await?
        .ok_or(ApiError::Internal)?;
    Ok((axum::http::StatusCode::CREATED, Json(created.into())))
}

/// 获取用户详情（仅管理员）。
pub async fn get_user(
    State(state): State<AppState>,
    identity: Identity,
    Path(id): Path<String>,
) -> Result<Json<UserView>, ApiError> {
    identity.require_admin()?;
    let user = state
        .meta
        .get_user_by_id(&id)
        .await?
        .ok_or(ApiError::NotFound)?;
    Ok(Json(user.into()))
}

/// 更新用户角色 / 禁用状态（仅管理员）。
pub async fn update_user(
    State(state): State<AppState>,
    identity: Identity,
    Path(id): Path<String>,
    Json(req): Json<UpdateUserRequest>,
) -> Result<Json<UserView>, ApiError> {
    identity.require_admin()?;
    let role = match &req.role {
        Some(s) => Some(parse_role(s)?),
        None => None,
    };
    let updated = state.meta.update_user(&id, role, req.disabled).await?;
    if !updated {
        return Err(ApiError::NotFound);
    }
    tracing::info!(用户 = %id, "已更新用户");
    let user = state
        .meta
        .get_user_by_id(&id)
        .await?
        .ok_or(ApiError::NotFound)?;
    Ok(Json(user.into()))
}

/// 删除用户（仅管理员）。
pub async fn delete_user(
    State(state): State<AppState>,
    identity: Identity,
    Path(id): Path<String>,
) -> Result<Json<Value>, ApiError> {
    identity.require_admin()?;
    let deleted = state.meta.delete_user(&id).await?;
    if !deleted {
        return Err(ApiError::NotFound);
    }
    tracing::info!(用户 = %id, "已删除用户");
    Ok(Json(json!({ "status": "ok" })))
}

/// 解析角色字符串（大小写不敏感）；非法值返回 400。
fn parse_role(s: &str) -> Result<Role, ApiError> {
    match s.to_ascii_lowercase().as_str() {
        "admin" => Ok(Role::Admin),
        "user" => Ok(Role::User),
        _ => Err(ApiError::BadRequest(format!("非法角色: {s}"))),
    }
}
