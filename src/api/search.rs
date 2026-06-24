//! 跨仓库制品搜索端点（FR-67）：在调用方可见范围内全局搜制品。
//!
//! 安全定式（testing-and-quality §2.1 检索鉴权过滤）：结果 / 计数 / 错误均**只含调用方有读权限
//! 的仓库制品**——匿名仅 public，登录用户加上其有读 ACL 的 private；**绝不泄露无权 private 的
//! 存在 / 计数**。分页在权限过滤之后施加，避免经 total 间接泄露无权制品数量。

use axum::{
    extract::{Query, State},
    Json,
};
use serde::{Deserialize, Serialize};

use crate::meta::{ArtifactSearchHit, Visibility};

use super::{ApiError, AppState, Identity};

/// 默认分页容量。
const DEFAULT_LIMIT: i64 = 50;
/// 分页容量上限（对齐 API.md）。
const MAX_LIMIT: i64 = 1000;
/// 权限过滤前的候选拉取上限：先取一批候选再按权限过滤，避免无界载入。
const CANDIDATE_CAP: i64 = 5000;

/// 搜索查询参数。
#[derive(Debug, Deserialize)]
pub struct SearchQuery {
    /// 关键字 / 坐标（按制品路径包含匹配）。
    pub q: String,
    /// 可选格式过滤（maven | npm | docker | raw | pypi）。
    #[serde(default)]
    pub format: Option<String>,
    /// 分页起点（默认 0）。
    #[serde(default)]
    pub offset: Option<i64>,
    /// 分页容量（默认 50，上限 1000）。
    #[serde(default)]
    pub limit: Option<i64>,
}

/// 单条搜索命中视图。
#[derive(Debug, Serialize)]
pub struct SearchHitDto {
    /// 所属仓库主键。
    pub repo_id: String,
    /// 所属仓库名。
    pub repo_name: String,
    /// 所属仓库格式。
    pub format: String,
    /// 制品路径。
    pub path: String,
    /// sha256 摘要。
    pub sha256: String,
    /// 字节大小。
    pub size: i64,
    /// 创建时间。
    pub created_at: String,
}

impl From<ArtifactSearchHit> for SearchHitDto {
    fn from(h: ArtifactSearchHit) -> Self {
        Self {
            repo_id: h.repo_id,
            repo_name: h.repo_name,
            format: h.repo_format,
            path: h.path,
            sha256: h.sha256,
            size: h.size,
            created_at: h.created_at,
        }
    }
}

/// 统一分页响应结构（对齐 API.md §1）。
#[derive(Debug, Serialize)]
pub struct Paginated {
    /// 本页命中项。
    pub items: Vec<SearchHitDto>,
    /// 过滤后总命中数（仅计调用方有权访问者）。
    pub total: usize,
    /// 本页起点。
    pub offset: i64,
    /// 本页容量。
    pub limit: i64,
    /// 是否还有更多。
    pub has_more: bool,
}

/// 跨仓库搜索：先检索候选，再按读权限过滤，最后分页。
pub async fn search(
    State(state): State<AppState>,
    identity: Identity,
    Query(query): Query<SearchQuery>,
) -> Result<Json<Paginated>, ApiError> {
    if query.q.trim().is_empty() {
        return Err(ApiError::BadRequest("搜索关键字不能为空".to_string()));
    }
    let offset = query.offset.unwrap_or(0).max(0);
    let limit = query.limit.unwrap_or(DEFAULT_LIMIT).clamp(1, MAX_LIMIT);

    // ① 检索候选（带回所属仓库可见性，供下一步据读权限过滤）
    let candidates = state
        .meta
        .search_artifacts(query.q.trim(), query.format.as_deref(), 0, CANDIDATE_CAP)
        .await?;

    // ② 按调用方读权限过滤：管理员见全部；其余仅 public + 自己有读 ACL 的 private
    let readable_private = readable_private_repos(&state, &identity).await?;
    let is_admin = identity.0.is_admin();
    let filtered: Vec<ArtifactSearchHit> = candidates
        .into_iter()
        .filter(|h| can_read(h, is_admin, &readable_private))
        .collect();

    // ③ 过滤后分页（total 取过滤后的数量，绝不暴露无权制品计数）
    let total = filtered.len();
    let start = (offset as usize).min(total);
    let end = (start + limit as usize).min(total);
    let items: Vec<SearchHitDto> = filtered[start..end]
        .iter()
        .cloned()
        .map(SearchHitDto::from)
        .collect();
    let has_more = end < total;

    Ok(Json(Paginated {
        items,
        total,
        offset,
        limit,
        has_more,
    }))
}

/// 命中是否对调用方可读：public 任意可读；private 仅管理员或命中读 ACL 的用户可读。
fn can_read(
    hit: &ArtifactSearchHit,
    is_admin: bool,
    readable_private: &std::collections::HashSet<String>,
) -> bool {
    match Visibility::from_db_str(&hit.repo_visibility) {
        Visibility::Public => true,
        Visibility::Private => is_admin || readable_private.contains(&hit.repo_id),
    }
}

/// 取调用方有读权限的私有仓库主键集合（匿名为空，避免无谓查库）。
async fn readable_private_repos(
    state: &AppState,
    identity: &Identity,
) -> Result<std::collections::HashSet<String>, ApiError> {
    match identity.0.user() {
        Some(u) => Ok(state
            .meta
            .list_repo_ids_with_read(&u.user_id)
            .await?
            .into_iter()
            .collect()),
        None => Ok(std::collections::HashSet::new()),
    }
}
