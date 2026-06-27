//! 仪表盘全局概览聚合端点（FR-108，增强 FR-18）：Admin 在首页一眼看清全局规模。
//!
//! 设计要点：
//! - **薄 handler**：只做鉴权编排（仅 Admin）+ 调既有 `meta` 计数方法 + 组装 DTO，无业务逻辑。
//! - **仅 Admin**：全局规模属管理视图，未认证 401、非管理员 403；普通用户 / 匿名的首页降级展示
//!   由前端只用可见仓库列表承载，不调本端点。
//! - **只读 / 不入审计**：纯 GET 聚合查询（GET 读取类不入审计，与 FR-97 一致）。
//! - **复用既有计数**：仓库数 / 制品数 / 去重存储字节 / 用户数均经 `meta` 既有（或同形新增）
//!   计数方法取得，不在此重复造聚合 SQL；存储字节按 sha256 去重（同一 blob 只计一次）。

use axum::{extract::State, Json};
use serde::Serialize;

use super::{ApiError, AppState, Identity};

/// 仪表盘 KPI 概览（GET /api/v1/dashboard/summary 响应体）。
///
/// `artifact_count` 为制品**索引条目数**（不去重，含同一 blob 被多仓库引用的多条），
/// 与 `total_bytes`（按 sha256 去重的占盘字节）语义互补：一个是引用数、一个是实际占用字节。
#[derive(Debug, Serialize)]
pub struct DashboardSummaryDto {
    /// 仓库总数。
    pub repo_count: i64,
    /// 制品索引条目总数（不去重）。
    pub artifact_count: i64,
    /// 去重存储用量（字节，按 sha256 去重求和）。
    pub total_bytes: i64,
    /// 用户总数。
    pub user_count: i64,
}

/// 仪表盘概览（仅 Admin）：返回仓库 / 制品 / 去重存储字节 / 用户数四项 KPI。
///
/// 四项均为本机内部规模数据、纯本地聚合、零外发；未认证 401、非管理员 403。
pub async fn dashboard_summary(
    State(state): State<AppState>,
    identity: Identity,
) -> Result<Json<DashboardSummaryDto>, ApiError> {
    identity.require_admin()?;
    let repo_count = state.meta.count_repositories().await?;
    let artifact_count = state.meta.count_artifacts().await?;
    let total_bytes = state.meta.total_blob_bytes().await?;
    let user_count = state.meta.count_users().await?;
    Ok(Json(DashboardSummaryDto {
        repo_count,
        artifact_count,
        total_bytes,
        user_count,
    }))
}

#[cfg(test)]
mod tests {
    use super::super::tests::{测试用状态, 读_json};
    use super::super::AppState;
    use crate::auth::hash_password;
    use crate::meta::{NewArtifact, NewRepository, RepoType, Role, Visibility};
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

    /// 便捷：带可选 Bearer 令牌请求仪表盘概览端点。
    async fn 请求概览(state: AppState, 令牌: Option<&str>) -> axum::response::Response {
        let app = super::super::build_router(state);
        let mut builder = Request::builder().uri("/api/v1/dashboard/summary");
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
        let resp = 请求概览(state, None).await;
        assert_eq!(resp.status(), StatusCode::UNAUTHORIZED);
    }

    #[tokio::test]
    async fn 普通用户访问被拒_403() {
        let (state, _dir) = 测试用状态().await;
        let token = 签发令牌(&state, "u", Role::User).await;
        let resp = 请求概览(state, Some(&token)).await;
        assert_eq!(resp.status(), StatusCode::FORBIDDEN);
    }

    #[tokio::test]
    async fn 管理员查询返回正确计数() {
        let (state, _dir) = 测试用状态().await;
        // 建一个仓库、写三条制品（其中两条共享同一 sha256），另签发用户造成用户数。
        let repo_id = state
            .meta
            .create_repository(NewRepository {
                name: "r1",
                format: "raw",
                r#type: RepoType::Hosted,
                visibility: Visibility::Public,
                upstream_url: None,
                upstream_auth_ref: None,
            })
            .await
            .unwrap();
        let 制品 = |path: &'static str, sha: &'static str, size: i64| NewArtifact {
            repo_id: &repo_id,
            path,
            size,
            sha256: sha,
            sha1: "s1",
            md5: "m",
            sha512: "s5",
            content_type: None,
            cached: false,
        };
        state
            .meta
            .upsert_artifact(制品("a.txt", "共享", 100))
            .await
            .unwrap();
        state
            .meta
            .upsert_artifact(制品("b.txt", "共享", 100))
            .await
            .unwrap();
        state
            .meta
            .upsert_artifact(制品("c.txt", "独立", 30))
            .await
            .unwrap();

        // 令牌签发会建一个 admin 用户（user_count 含它）
        let token = 签发令牌(&state, "admin", Role::Admin).await;
        let resp = 请求概览(state, Some(&token)).await;
        assert_eq!(resp.status(), StatusCode::OK);
        let body = 读_json(resp).await;
        assert_eq!(body["repo_count"].as_i64().unwrap(), 1, "仓库数应为 1");
        assert_eq!(
            body["artifact_count"].as_i64().unwrap(),
            3,
            "制品索引条目数不去重应为 3"
        );
        // 去重字节：共享 100 计一次 + 独立 30 = 130
        assert_eq!(
            body["total_bytes"].as_i64().unwrap(),
            130,
            "去重存储字节应为 130"
        );
        assert_eq!(body["user_count"].as_i64().unwrap(), 1, "用户数应为 1");
    }
}
