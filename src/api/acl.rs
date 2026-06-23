//! 每仓库 ACL 管理端点（FR-07）：列出 / 新增 / 移除某仓库的读写授权，仅管理员可操作。
//!
//! handler 保持薄：仅做请求解析、权限门、调用 meta 与错误映射。

use axum::{
    extract::{Path, State},
    Json,
};
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};

use crate::meta::{AclRecord, Permission};

use super::{ApiError, AppState, Identity};

/// 对外 ACL 条目视图。
#[derive(Debug, Serialize)]
pub struct AclDto {
    /// ACL 条目主键。
    pub id: String,
    /// 被授权用户主键。
    pub user_id: String,
    /// 权限（read | write）。
    pub permission: String,
}

impl From<AclRecord> for AclDto {
    fn from(r: AclRecord) -> Self {
        Self {
            id: r.id,
            user_id: r.user_id,
            permission: r.permission,
        }
    }
}

/// 新增 ACL 请求体。
#[derive(Debug, Deserialize)]
pub struct CreateAclRequest {
    /// 被授权用户主键。
    pub user_id: String,
    /// 权限（read | write，大小写不敏感）。
    pub permission: String,
}

/// 列出某仓库的全部 ACL 条目（仅管理员）。仓库不存在返回 404。
pub async fn list_acl(
    State(state): State<AppState>,
    identity: Identity,
    Path(id): Path<String>,
) -> Result<Json<Vec<AclDto>>, ApiError> {
    identity.require_admin()?;
    // 仓库须存在才列其 ACL
    state
        .meta
        .get_repository_by_id(&id)
        .await?
        .ok_or(ApiError::NotFound)?;
    let acl = state.meta.list_acl_by_repo(&id).await?;
    Ok(Json(acl.into_iter().map(AclDto::from).collect()))
}

/// 新增一条 ACL（仅管理员）。仓库 / 用户不存在 404；重复授权 409。
pub async fn create_acl(
    State(state): State<AppState>,
    identity: Identity,
    Path(id): Path<String>,
    Json(req): Json<CreateAclRequest>,
) -> Result<(axum::http::StatusCode, Json<AclDto>), ApiError> {
    identity.require_admin()?;
    let permission = parse_permission(&req.permission)?;

    // 仓库与用户都须存在（外键虽会拦，但提前校验以返回精确 404）
    state
        .meta
        .get_repository_by_id(&id)
        .await?
        .ok_or(ApiError::NotFound)?;
    state
        .meta
        .get_user_by_id(&req.user_id)
        .await?
        .ok_or(ApiError::NotFound)?;

    let acl_id = match state.meta.create_acl(&id, &req.user_id, permission).await {
        Ok(acl_id) => acl_id,
        Err(crate::meta::MetaError::Database(sqlx::Error::Database(db)))
            if db.is_unique_violation() =>
        {
            return Err(ApiError::Conflict("该用户的同类授权已存在".to_string()));
        }
        Err(e) => return Err(e.into()),
    };

    tracing::info!(仓库 = %id, 用户 = %req.user_id, 权限 = %permission.as_str(), "已新增仓库 ACL");
    let record = state
        .meta
        .get_acl_by_id(&acl_id)
        .await?
        .ok_or(ApiError::Internal)?;
    Ok((axum::http::StatusCode::CREATED, Json(record.into())))
}

/// 移除一条 ACL（仅管理员）。仓库或 ACL 条目不存在返回 404。
pub async fn delete_acl(
    State(state): State<AppState>,
    identity: Identity,
    Path((id, acl_id)): Path<(String, String)>,
) -> Result<Json<Value>, ApiError> {
    identity.require_admin()?;
    // 校验 ACL 条目存在且确属该仓库，避免跨仓库误删
    let acl = state
        .meta
        .get_acl_by_id(&acl_id)
        .await?
        .ok_or(ApiError::NotFound)?;
    if acl.repo_id != id {
        return Err(ApiError::NotFound);
    }
    state.meta.delete_acl(&acl_id).await?;
    tracing::info!(仓库 = %id, acl = %acl_id, "已移除仓库 ACL");
    Ok(Json(json!({ "status": "ok" })))
}

/// 解析权限（大小写不敏感）；非法值返回 400。
fn parse_permission(s: &str) -> Result<Permission, ApiError> {
    match s.to_ascii_lowercase().as_str() {
        "read" => Ok(Permission::Read),
        "write" => Ok(Permission::Write),
        _ => Err(ApiError::BadRequest(format!("非法权限: {s}"))),
    }
}
