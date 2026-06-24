//! 仓库管理与浏览端点（FR-06/07/08/09/10/13）。
//!
//! 管理类操作（创建 / 更新 / 删除）仅管理员；读类操作（详情 / 制品浏览）经授权判定，
//! 私有仓库对未授权方一律映射为 404 隐藏存在性（docs/API.md §2 定式）。
//! handler 保持薄：身份解析在中间件、判定在 `authz` 纯函数，本层只做编排与错误映射。

use axum::{
    extract::{Path, State},
    Json,
};
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};

use crate::meta::{ArtifactRecord, RepositoryRecord, Visibility};
use crate::repo::{self, CreateRepoInput, RepoError, UpdateRepoInput};

use super::repo_access::load_readable_repo;
use super::{ApiError, AppState, Identity};

/// 对外仓库视图（字段对齐 docs/API.md）。
#[derive(Debug, Serialize)]
pub struct RepositoryDto {
    /// 仓库主键。
    pub id: String,
    /// 仓库名。
    pub name: String,
    /// 格式（maven | npm | docker | raw | pypi）。
    pub format: String,
    /// 类型（hosted | proxy）。
    #[serde(rename = "type")]
    pub r#type: String,
    /// 可见性（public | private）。
    pub visibility: String,
    /// 上游地址（proxy 适用）。
    pub upstream_url: Option<String>,
    /// 创建时间（ISO8601）。
    pub created_at: String,
}

impl From<RepositoryRecord> for RepositoryDto {
    fn from(r: RepositoryRecord) -> Self {
        // 不回显 upstream_auth_ref：它是凭据引用，无须对外暴露
        Self {
            id: r.id,
            name: r.name,
            format: r.format,
            r#type: r.r#type,
            visibility: r.visibility,
            upstream_url: r.upstream_url,
            created_at: r.created_at,
        }
    }
}

/// 制品索引视图（字段对齐 docs/API.md 浏览制品）。
#[derive(Debug, Serialize)]
pub struct ArtifactDto {
    /// 制品路径。
    pub path: String,
    /// 字节大小。
    pub size: i64,
    /// sha256 摘要。
    pub sha256: String,
    /// 内容类型。
    pub content_type: Option<String>,
    /// 是否为 proxy 缓存制品。
    pub cached: bool,
    /// 创建时间。
    pub created_at: String,
}

impl From<ArtifactRecord> for ArtifactDto {
    fn from(r: ArtifactRecord) -> Self {
        Self {
            path: r.path,
            size: r.size,
            sha256: r.sha256,
            content_type: r.content_type,
            cached: r.cached != 0,
            created_at: r.created_at,
        }
    }
}

/// 把仓库生命周期错误映射为 HTTP 错误：非法入参 400、重名 409、其余转内部。
impl From<RepoError> for ApiError {
    fn from(e: RepoError) -> Self {
        match e {
            RepoError::Invalid(msg) => ApiError::BadRequest(msg),
            RepoError::NameConflict => ApiError::Conflict("仓库名已存在".to_string()),
            RepoError::Meta(meta) => meta.into(),
        }
    }
}

/// 创建仓库请求体。
#[derive(Debug, Deserialize)]
pub struct CreateRepositoryRequest {
    /// 仓库名。
    pub name: String,
    /// 格式（maven | npm | docker | raw | pypi）。
    pub format: String,
    /// 类型（hosted | proxy）。
    #[serde(rename = "type")]
    pub r#type: String,
    /// 可见性（public | private）。
    pub visibility: String,
    /// 上游地址（proxy 适用）。
    #[serde(default)]
    pub upstream_url: Option<String>,
    /// 上游凭据引用（仅引用，真值走配置 / env，不入库明文）。
    #[serde(default)]
    pub upstream_auth_ref: Option<String>,
}

/// 更新仓库请求体：字段可选，仅更新提供的项。
#[derive(Debug, Deserialize)]
pub struct UpdateRepositoryRequest {
    /// 可见性（public | private）。
    #[serde(default)]
    pub visibility: Option<String>,
    /// 上游地址（proxy 适用）。
    #[serde(default)]
    pub upstream_url: Option<String>,
    /// 上游凭据引用。
    #[serde(default)]
    pub upstream_auth_ref: Option<String>,
}

/// 列出仓库：按调用方身份过滤可见仓库（匿名仅见 public）。
pub async fn list_repositories(
    State(state): State<AppState>,
    identity: Identity,
) -> Result<Json<Vec<RepositoryDto>>, ApiError> {
    let all = state.meta.list_repositories().await?;

    // 管理员可见全部；其余按可见性与读 ACL 过滤
    if identity.0.is_admin() {
        return Ok(Json(all.into_iter().map(RepositoryDto::from).collect()));
    }

    // 登录用户：取其有读权限的私有仓库主键集合，避免逐仓库查库（防 N+1）
    let readable_private: std::collections::HashSet<String> = match identity.0.user() {
        Some(u) => state
            .meta
            .list_repo_ids_with_read(&u.user_id)
            .await?
            .into_iter()
            .collect(),
        None => std::collections::HashSet::new(),
    };

    let visible = all
        .into_iter()
        .filter(|r| match Visibility::from_db_str(&r.visibility) {
            // 公开仓库任何人可见
            Visibility::Public => true,
            // 私有仓库仅当登录用户命中读 ACL 才可见
            Visibility::Private => readable_private.contains(&r.id),
        })
        .map(RepositoryDto::from)
        .collect();
    Ok(Json(visible))
}

/// 创建仓库（仅管理员）。业务规则校验与落库下沉到 `repo` 模块。
pub async fn create_repository(
    State(state): State<AppState>,
    identity: Identity,
    Json(req): Json<CreateRepositoryRequest>,
) -> Result<(axum::http::StatusCode, Json<RepositoryDto>), ApiError> {
    identity.require_admin()?;
    let created = repo::create(
        &state.meta,
        CreateRepoInput {
            name: req.name,
            format: req.format,
            r#type: req.r#type,
            visibility: req.visibility,
            upstream_url: req.upstream_url,
            upstream_auth_ref: req.upstream_auth_ref,
        },
    )
    .await?;
    Ok((axum::http::StatusCode::CREATED, Json(created.into())))
}

/// 获取仓库详情：受读权限约束，无权 private 映射为 404。
pub async fn get_repository(
    State(state): State<AppState>,
    identity: Identity,
    Path(id): Path<String>,
) -> Result<Json<RepositoryDto>, ApiError> {
    let repo = load_readable_repo(&state, &identity, &id).await?;
    Ok(Json(repo.into()))
}

/// 更新仓库（仅管理员）。业务规则校验与落库下沉到 `repo` 模块。
pub async fn update_repository(
    State(state): State<AppState>,
    identity: Identity,
    Path(id): Path<String>,
    Json(req): Json<UpdateRepositoryRequest>,
) -> Result<Json<RepositoryDto>, ApiError> {
    identity.require_admin()?;
    let updated = repo::update(
        &state.meta,
        &id,
        UpdateRepoInput {
            visibility: req.visibility,
            upstream_url: req.upstream_url,
            upstream_auth_ref: req.upstream_auth_ref,
        },
    )
    .await?
    .ok_or(ApiError::NotFound)?;
    Ok(Json(updated.into()))
}

/// 删除仓库（仅管理员）。
pub async fn delete_repository(
    State(state): State<AppState>,
    identity: Identity,
    Path(id): Path<String>,
) -> Result<Json<Value>, ApiError> {
    identity.require_admin()?;
    if !repo::delete(&state.meta, &id).await? {
        return Err(ApiError::NotFound);
    }
    Ok(Json(json!({ "status": "ok" })))
}

/// 浏览仓库制品索引：受读权限约束，无权 private 映射为 404。
pub async fn list_artifacts(
    State(state): State<AppState>,
    identity: Identity,
    Path(id): Path<String>,
) -> Result<Json<Vec<ArtifactDto>>, ApiError> {
    let repo = load_readable_repo(&state, &identity, &id).await?;
    let artifacts = state.meta.list_artifacts_by_repo(&repo.id).await?;
    Ok(Json(artifacts.into_iter().map(ArtifactDto::from).collect()))
}
