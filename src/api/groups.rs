//! 用户组/团队与组级仓库 ACL 管理端点（FR-49 / ADR-0007）：仅管理员可操作。
//!
//! 提供组 CRUD、成员加入 / 移出，以及对组授予 / 撤销仓库 ACL。
//! handler 保持薄：仅做请求解析、权限门、调用 meta 与错误映射；业务下沉到 `meta`。

use axum::{
    extract::{Path, State},
    http::StatusCode,
    Json,
};
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};

use crate::meta::{GroupAclRecord, GroupMemberRecord, GroupRecord, Permission};

use super::{ApiError, AppState, Identity};

/// 对外用户组视图。
#[derive(Debug, Serialize)]
pub struct GroupView {
    /// 组主键。
    pub id: String,
    /// 组名。
    pub name: String,
    /// 创建时间（ISO8601）。
    pub created_at: String,
}

impl From<GroupRecord> for GroupView {
    fn from(r: GroupRecord) -> Self {
        Self {
            id: r.id,
            name: r.name,
            created_at: r.created_at,
        }
    }
}

/// 对外组成员视图。
#[derive(Debug, Serialize)]
pub struct GroupMemberView {
    /// 成员用户主键。
    pub user_id: String,
    /// 成员用户名。
    pub username: String,
}

impl From<GroupMemberRecord> for GroupMemberView {
    fn from(r: GroupMemberRecord) -> Self {
        Self {
            user_id: r.user_id,
            username: r.username,
        }
    }
}

/// 对外组 ACL 条目视图。
#[derive(Debug, Serialize)]
pub struct GroupAclView {
    /// 组 ACL 条目主键。
    pub id: String,
    /// 被授权组主键。
    pub group_id: String,
    /// 权限动作（read | write | delete | admin）。
    pub permission: String,
}

impl From<GroupAclRecord> for GroupAclView {
    fn from(r: GroupAclRecord) -> Self {
        Self {
            id: r.id,
            group_id: r.group_id,
            permission: r.permission,
        }
    }
}

/// 创建用户组请求体。
#[derive(Debug, Deserialize)]
pub struct CreateGroupRequest {
    /// 组名。
    pub name: String,
}

/// 加入成员请求体。
#[derive(Debug, Deserialize)]
pub struct AddMemberRequest {
    /// 待加入用户主键。
    pub user_id: String,
}

/// 对组授予 ACL 请求体。
#[derive(Debug, Deserialize)]
pub struct CreateGroupAclRequest {
    /// 被授权组主键。
    pub group_id: String,
    /// 权限动作（read | write | delete | admin，大小写不敏感）。
    pub permission: String,
}

/// 列出全部用户组（仅管理员）。
pub async fn list_groups(
    State(state): State<AppState>,
    identity: Identity,
) -> Result<Json<Vec<GroupView>>, ApiError> {
    identity.require_admin()?;
    let groups = state.meta.list_groups().await?;
    Ok(Json(groups.into_iter().map(GroupView::from).collect()))
}

/// 创建用户组（仅管理员）。组名重复返回 409。
pub async fn create_group(
    State(state): State<AppState>,
    identity: Identity,
    Json(req): Json<CreateGroupRequest>,
) -> Result<(StatusCode, Json<GroupView>), ApiError> {
    identity.require_admin()?;
    if req.name.is_empty() {
        return Err(ApiError::BadRequest("组名不能为空".to_string()));
    }

    let id = match state.meta.create_group(&req.name).await {
        Ok(id) => id,
        Err(crate::meta::MetaError::Database(sqlx::Error::Database(db)))
            if db.is_unique_violation() =>
        {
            return Err(ApiError::Conflict("组名已存在".to_string()));
        }
        Err(e) => return Err(e.into()),
    };

    tracing::info!(组名 = %req.name, "已创建用户组");
    let created = state
        .meta
        .get_group_by_id(&id)
        .await?
        .ok_or(ApiError::Internal)?;
    Ok((StatusCode::CREATED, Json(created.into())))
}

/// 获取用户组详情（仅管理员）。组不存在返回 404。
pub async fn get_group(
    State(state): State<AppState>,
    identity: Identity,
    Path(id): Path<String>,
) -> Result<Json<GroupView>, ApiError> {
    identity.require_admin()?;
    let group = state
        .meta
        .get_group_by_id(&id)
        .await?
        .ok_or(ApiError::NotFound)?;
    Ok(Json(group.into()))
}

/// 删除用户组（仅管理员）。组不存在返回 404。
pub async fn delete_group(
    State(state): State<AppState>,
    identity: Identity,
    Path(id): Path<String>,
) -> Result<Json<Value>, ApiError> {
    identity.require_admin()?;
    let deleted = state.meta.delete_group(&id).await?;
    if !deleted {
        return Err(ApiError::NotFound);
    }
    tracing::info!(组 = %id, "已删除用户组");
    Ok(Json(json!({ "status": "ok" })))
}

/// 列出某组成员（仅管理员）。组不存在返回 404。
pub async fn list_members(
    State(state): State<AppState>,
    identity: Identity,
    Path(id): Path<String>,
) -> Result<Json<Vec<GroupMemberView>>, ApiError> {
    identity.require_admin()?;
    state
        .meta
        .get_group_by_id(&id)
        .await?
        .ok_or(ApiError::NotFound)?;
    let members = state.meta.list_group_members(&id).await?;
    Ok(Json(
        members.into_iter().map(GroupMemberView::from).collect(),
    ))
}

/// 把用户加入组（仅管理员）。组 / 用户不存在 404；重复加入 409。
pub async fn add_member(
    State(state): State<AppState>,
    identity: Identity,
    Path(id): Path<String>,
    Json(req): Json<AddMemberRequest>,
) -> Result<(StatusCode, Json<Value>), ApiError> {
    identity.require_admin()?;
    // 组与用户都须存在（外键虽会拦，但提前校验以返回精确 404）
    state
        .meta
        .get_group_by_id(&id)
        .await?
        .ok_or(ApiError::NotFound)?;
    state
        .meta
        .get_user_by_id(&req.user_id)
        .await?
        .ok_or(ApiError::NotFound)?;

    match state.meta.add_group_member(&id, &req.user_id).await {
        Ok(()) => {}
        Err(crate::meta::MetaError::Database(sqlx::Error::Database(db)))
            if db.is_unique_violation() =>
        {
            return Err(ApiError::Conflict("该用户已在组内".to_string()));
        }
        Err(e) => return Err(e.into()),
    }
    tracing::info!(组 = %id, 用户 = %req.user_id, "已加入组成员");
    Ok((StatusCode::CREATED, Json(json!({ "status": "ok" }))))
}

/// 把用户移出组（仅管理员）。组不存在或该用户本不在组内返回 404。
pub async fn remove_member(
    State(state): State<AppState>,
    identity: Identity,
    Path((id, user_id)): Path<(String, String)>,
) -> Result<Json<Value>, ApiError> {
    identity.require_admin()?;
    state
        .meta
        .get_group_by_id(&id)
        .await?
        .ok_or(ApiError::NotFound)?;
    let removed = state.meta.remove_group_member(&id, &user_id).await?;
    if !removed {
        return Err(ApiError::NotFound);
    }
    tracing::info!(组 = %id, 用户 = %user_id, "已移出组成员");
    Ok(Json(json!({ "status": "ok" })))
}

/// 列出某仓库的全部组 ACL（仅管理员）。仓库不存在返回 404。
pub async fn list_group_acl(
    State(state): State<AppState>,
    identity: Identity,
    Path(id): Path<String>,
) -> Result<Json<Vec<GroupAclView>>, ApiError> {
    identity.require_admin()?;
    state
        .meta
        .get_repository_by_id(&id)
        .await?
        .ok_or(ApiError::NotFound)?;
    let acl = state.meta.list_group_acl_by_repo(&id).await?;
    Ok(Json(acl.into_iter().map(GroupAclView::from).collect()))
}

/// 对组授予一条仓库 ACL（仅管理员）。仓库 / 组不存在 404；重复授权 409。
pub async fn create_group_acl(
    State(state): State<AppState>,
    identity: Identity,
    Path(id): Path<String>,
    Json(req): Json<CreateGroupAclRequest>,
) -> Result<(StatusCode, Json<GroupAclView>), ApiError> {
    identity.require_admin()?;
    let permission = parse_permission(&req.permission)?;

    // 仓库与组都须存在（外键虽会拦，但提前校验以返回精确 404）
    state
        .meta
        .get_repository_by_id(&id)
        .await?
        .ok_or(ApiError::NotFound)?;
    state
        .meta
        .get_group_by_id(&req.group_id)
        .await?
        .ok_or(ApiError::NotFound)?;

    let acl_id = match state
        .meta
        .create_group_acl(&id, &req.group_id, permission)
        .await
    {
        Ok(acl_id) => acl_id,
        Err(crate::meta::MetaError::Database(sqlx::Error::Database(db)))
            if db.is_unique_violation() =>
        {
            return Err(ApiError::Conflict("该组的同类授权已存在".to_string()));
        }
        Err(e) => return Err(e.into()),
    };

    tracing::info!(仓库 = %id, 组 = %req.group_id, 权限 = %permission.as_str(), "已对组新增仓库 ACL");
    let record = state
        .meta
        .get_group_acl_by_id(&acl_id)
        .await?
        .ok_or(ApiError::Internal)?;
    Ok((StatusCode::CREATED, Json(record.into())))
}

/// 撤销一条组 ACL（仅管理员）。仓库或组 ACL 条目不存在返回 404。
pub async fn delete_group_acl(
    State(state): State<AppState>,
    identity: Identity,
    Path((id, acl_id)): Path<(String, String)>,
) -> Result<Json<Value>, ApiError> {
    identity.require_admin()?;
    // 校验组 ACL 条目存在且确属该仓库，避免跨仓库误删
    let acl = state
        .meta
        .get_group_acl_by_id(&acl_id)
        .await?
        .ok_or(ApiError::NotFound)?;
    if acl.repo_id != id {
        return Err(ApiError::NotFound);
    }
    state.meta.delete_group_acl(&acl_id).await?;
    tracing::info!(仓库 = %id, 组acl = %acl_id, "已撤销组仓库 ACL");
    Ok(Json(json!({ "status": "ok" })))
}

/// 解析权限动作（大小写不敏感，四级动作）；非法值返回 400。
fn parse_permission(s: &str) -> Result<Permission, ApiError> {
    match s.to_ascii_lowercase().as_str() {
        "read" => Ok(Permission::Read),
        "write" => Ok(Permission::Write),
        "delete" => Ok(Permission::Delete),
        "admin" => Ok(Permission::Admin),
        _ => Err(ApiError::BadRequest(format!("非法权限: {s}"))),
    }
}
