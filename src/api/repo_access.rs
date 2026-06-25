//! API 层共享的仓库授权编排：据调用方身份与目标仓库构造授权视图、施加读 / 写判定。
//!
//! 把"查 ACL → 构造 RepoView → authorize → 按定式映射 404/403"这段编排集中一处，
//! 供仓库管理、制品浏览 / 详情、Raw 格式端点等复用，避免各 handler 重复且语义漂移。
//! handler 仍保持薄：判定本身在 `authz` 纯函数，这里只做装配与错误映射。

use crate::authz::{authorize, Action, Decision, RepoView};
use crate::meta::{RepositoryRecord, Visibility};

use super::{ApiError, AppState, Identity};

/// 为调用方在某仓库上构造授权视图：解析其 ACL 命中情况（匿名不查库，避免无谓 IO）。
///
/// 已认证用户的有效权限 = 直接授予其的 ACL ∪ 其经所属各组继承的组 ACL（FR-49）。
/// 两路权限取并集后交 `authz` 纯函数按动作蕴含关系判定，既有直接-ACL 判定结论不变。
pub(crate) async fn build_repo_view(
    state: &AppState,
    identity: &Identity,
    repo: &RepositoryRecord,
) -> Result<RepoView, ApiError> {
    let visibility = Visibility::from_db_str(&repo.visibility);
    let perms = match identity.0.user() {
        // 已认证：直接-用户 ACL 与经组继承的组 ACL 取并集
        Some(u) => {
            let mut perms = state
                .meta
                .list_user_permissions(&repo.id, &u.user_id)
                .await?;
            let group_perms = state
                .meta
                .list_user_group_permissions(&repo.id, &u.user_id)
                .await?;
            perms.extend(group_perms);
            perms
        }
        // 匿名：无任何 ACL（含组），匿名不属于任何组
        None => Vec::new(),
    };
    Ok(RepoView::from_permissions(visibility, &perms))
}

/// 解析仓库并施加读授权：仓库不存在 → 404；无读权限（含无权 private）→ 404 隐藏存在性。
pub(crate) async fn load_readable_repo(
    state: &AppState,
    identity: &Identity,
    id: &str,
) -> Result<RepositoryRecord, ApiError> {
    let repo = state
        .meta
        .get_repository_by_id(id)
        .await?
        .ok_or(ApiError::NotFound)?;
    let view = build_repo_view(state, identity, &repo).await?;
    match authorize(&identity.0, &view, Action::Read) {
        Decision::Allow => Ok(repo),
        // 无读权限一律 404 隐藏存在性（遵 API §2 定式）
        Decision::Deny => Err(ApiError::NotFound),
    }
}

/// 按仓库名解析并施加读授权：仓库不存在 / 无读权限（含匿名 private）一律 404 隐藏存在性。
///
/// 格式端点据路径中的仓库名定位仓库，故单列按名解析的读授权编排，复用按 id 的读判定。
pub(crate) async fn load_readable_repo_by_name(
    state: &AppState,
    identity: &Identity,
    repo_name: &str,
) -> Result<RepositoryRecord, ApiError> {
    let repo = state
        .meta
        .get_repository_by_name(repo_name)
        .await?
        .ok_or(ApiError::NotFound)?;
    load_readable_repo(state, identity, &repo.id).await
}

/// 解析仓库并施加写授权：先按读判定隐藏存在性（无读权限 404），有读但无写返回 403。
///
/// 遵 API §2 定式：私有仓库对无读权限者返回 404（不暴露存在性）；有读无写返回 403。
pub(crate) async fn load_writable_repo(
    state: &AppState,
    identity: &Identity,
    id: &str,
) -> Result<RepositoryRecord, ApiError> {
    let repo = state
        .meta
        .get_repository_by_id(id)
        .await?
        .ok_or(ApiError::NotFound)?;
    let view = build_repo_view(state, identity, &repo).await?;
    // 先过读判定：无读权限者（含匿名访问 private）一律 404，不泄露仓库存在
    if authorize(&identity.0, &view, Action::Read) == Decision::Deny {
        return Err(ApiError::NotFound);
    }
    // 能读但无写 → 403（有读无写不得越权写）
    match authorize(&identity.0, &view, Action::Write) {
        Decision::Allow => Ok(repo),
        Decision::Deny => Err(ApiError::Forbidden),
    }
}
