//! 使用分析数据面板查询端点（FR-58，ADR-0009）：消费 FR-57 采集的本地聚合数据，
//! 返回访问量 / 下载量、热门制品（按下载）、仓库用量（按下载）等聚合视图，供控制台数据面板展示。
//!
//! 设计要点：
//! - **薄 handler**：只做鉴权编排、调用 `meta` 聚合查询入口、组装响应；不写业务逻辑。
//! - **仅 Admin**：富数据面板属管理视图，未认证 401、非管理员 403。
//! - **隐私红线（ADR-0009）**：只查本机内部聚合数据（`usage_stats`），**绝不外发、不向外部
//!   遥测 phone-home**；本端点不接任何外部导出 / 上报，纯本地查询。

use axum::{
    extract::{Query, State},
    Json,
};
use serde::{Deserialize, Serialize};

use crate::meta::{RepoUsageRow, UsageAction, UsageStatRow};

use super::{ApiError, AppState, Identity};

/// 默认返回的热门 / 仓库用量条数。
const DEFAULT_TOP: i64 = 10;
/// 热门 / 仓库用量条数上限（控制单次返回量级，避免大结果集）。
const MAX_TOP: i64 = 100;

/// 数据面板查询参数。
#[derive(Debug, Deserialize)]
pub struct UsageAnalyticsQuery {
    /// 热门制品 / 仓库用量各取前 N 条（默认 10，上限 100）。
    #[serde(default)]
    pub top: Option<i64>,
}

/// 单条制品级聚合（热门制品）。
#[derive(Debug, Serialize, PartialEq, Eq)]
pub struct ArtifactUsageDto {
    /// 所属仓库名。
    pub repo_name: String,
    /// 制品仓库内路径。
    pub repo_path: String,
    /// 累计次数。
    pub count: i64,
    /// 最近一次发生时间（UTC）。
    pub last_at: String,
}

impl From<UsageStatRow> for ArtifactUsageDto {
    fn from(r: UsageStatRow) -> Self {
        Self {
            repo_name: r.repo_name,
            repo_path: r.repo_path,
            count: r.count,
            last_at: r.last_at,
        }
    }
}

/// 单条仓库级聚合（仓库用量）。
#[derive(Debug, Serialize, PartialEq, Eq)]
pub struct RepoUsageDto {
    /// 仓库名。
    pub repo_name: String,
    /// 该仓库累计次数（跨制品路径汇总）。
    pub count: i64,
}

impl From<RepoUsageRow> for RepoUsageDto {
    fn from(r: RepoUsageRow) -> Self {
        Self {
            repo_name: r.repo_name,
            count: r.count,
        }
    }
}

/// 使用分析聚合视图（数据面板总览）。
#[derive(Debug, Serialize)]
pub struct UsageAnalyticsDto {
    /// 全局累计访问量。
    pub total_access: i64,
    /// 全局累计下载量。
    pub total_download: i64,
    /// 热门制品（按下载量倒序，前 `top` 条）。
    pub top_downloads: Vec<ArtifactUsageDto>,
    /// 仓库用量（按下载量倒序汇总到仓库，前 `top` 条）。
    pub repo_usage: Vec<RepoUsageDto>,
}

/// 查询使用分析聚合（仅 Admin）：消费本地聚合计数，返回访问 / 下载总量、热门制品、仓库用量。
///
/// 纯查本机内部聚合数据、不外发（ADR-0009 隐私红线）；非管理员被拒。
pub async fn usage_analytics(
    State(state): State<AppState>,
    identity: Identity,
    Query(query): Query<UsageAnalyticsQuery>,
) -> Result<Json<UsageAnalyticsDto>, ApiError> {
    identity.require_admin()?;
    let top = query.top.unwrap_or(DEFAULT_TOP).clamp(1, MAX_TOP);

    let total_access = state
        .meta
        .usage_total_by_action(UsageAction::Access)
        .await?;
    let total_download = state
        .meta
        .usage_total_by_action(UsageAction::Download)
        .await?;
    let top_downloads = state
        .meta
        .top_usage_by_action(UsageAction::Download, top)
        .await?
        .into_iter()
        .map(ArtifactUsageDto::from)
        .collect();
    let repo_usage = state
        .meta
        .top_repo_usage_by_action(UsageAction::Download, top)
        .await?
        .into_iter()
        .map(RepoUsageDto::from)
        .collect();

    Ok(Json(UsageAnalyticsDto {
        total_access,
        total_download,
        top_downloads,
        repo_usage,
    }))
}

#[cfg(test)]
mod tests {
    use super::super::tests::{测试用状态, 读_json};
    use super::*;
    use crate::auth::hash_password;
    use crate::meta::Role;
    use axum::body::Body;
    use axum::http::{Request, StatusCode};
    use tower::ServiceExt;

    /// 在状态库内建一个指定角色用户并签发其会话 JWT。
    async fn 签发令牌(state: &AppState, name: &str, role: Role) -> String {
        let uid = state
            .meta
            .create_user(name, &hash_password("pw").unwrap(), role)
            .await
            .unwrap();
        state.jwt.issue(&uid, name, role).unwrap()
    }

    /// 便捷：带可选 Bearer 令牌请求分析端点。
    async fn 请求分析(state: AppState, 令牌: Option<&str>) -> axum::response::Response {
        let app = super::super::build_router(state);
        let mut builder = Request::builder().uri("/api/v1/analytics/usage");
        if let Some(t) = 令牌 {
            builder = builder.header("Authorization", format!("Bearer {t}"));
        }
        app.oneshot(builder.body(Body::empty()).unwrap())
            .await
            .unwrap()
    }

    #[tokio::test]
    async fn 匿名访问被拒_401() {
        let (state, _dir) = 测试用状态().await;
        let resp = 请求分析(state, None).await;
        assert_eq!(resp.status(), StatusCode::UNAUTHORIZED);
    }

    #[tokio::test]
    async fn 普通用户访问被拒_403() {
        let (state, _dir) = 测试用状态().await;
        let token = 签发令牌(&state, "u", Role::User).await;
        let resp = 请求分析(state, Some(&token)).await;
        assert_eq!(resp.status(), StatusCode::FORBIDDEN);
    }

    #[tokio::test]
    async fn 管理员查询聚合正确() {
        let (state, _dir) = 测试用状态().await;
        let token = 签发令牌(&state, "admin", Role::Admin).await;
        // 直接经 meta 聚合落库：repoA/x 下载 3、repoB/y 下载 1、repoA/x 访问 2
        for _ in 0..3 {
            state
                .meta
                .insert_usage_batch(
                    &[crate::meta::NewUsageEvent {
                        repo_name: "repoA".into(),
                        repo_path: "x".into(),
                        action: "download".into(),
                        actor: "anonymous".into(),
                        source_ip: None,
                    }],
                    false,
                )
                .await
                .unwrap();
        }
        state
            .meta
            .insert_usage_batch(
                &[crate::meta::NewUsageEvent {
                    repo_name: "repoB".into(),
                    repo_path: "y".into(),
                    action: "download".into(),
                    actor: "anonymous".into(),
                    source_ip: None,
                }],
                false,
            )
            .await
            .unwrap();
        for _ in 0..2 {
            state
                .meta
                .insert_usage_batch(
                    &[crate::meta::NewUsageEvent {
                        repo_name: "repoA".into(),
                        repo_path: "x".into(),
                        action: "access".into(),
                        actor: "anonymous".into(),
                        source_ip: None,
                    }],
                    false,
                )
                .await
                .unwrap();
        }

        let resp = 请求分析(state, Some(&token)).await;
        assert_eq!(resp.status(), StatusCode::OK);
        let body = 读_json(resp).await;
        assert_eq!(body["total_download"], 4);
        assert_eq!(body["total_access"], 2);
        // 热门制品按下载量倒序：repoA/x（3）在前
        assert_eq!(body["top_downloads"][0]["repo_name"], "repoA");
        assert_eq!(body["top_downloads"][0]["repo_path"], "x");
        assert_eq!(body["top_downloads"][0]["count"], 3);
        // 仓库用量按下载汇总倒序：repoA（3）在前、repoB（1）在后
        assert_eq!(body["repo_usage"][0]["repo_name"], "repoA");
        assert_eq!(body["repo_usage"][0]["count"], 3);
        assert_eq!(body["repo_usage"][1]["repo_name"], "repoB");
        assert_eq!(body["repo_usage"][1]["count"], 1);
    }

    #[tokio::test]
    async fn 空库管理员查询返回零总览() {
        let (state, _dir) = 测试用状态().await;
        let token = 签发令牌(&state, "admin", Role::Admin).await;
        let resp = 请求分析(state, Some(&token)).await;
        assert_eq!(resp.status(), StatusCode::OK);
        let body = 读_json(resp).await;
        assert_eq!(body["total_download"], 0);
        assert_eq!(body["total_access"], 0);
        assert!(body["top_downloads"].as_array().unwrap().is_empty());
        assert!(body["repo_usage"].as_array().unwrap().is_empty());
    }
}
