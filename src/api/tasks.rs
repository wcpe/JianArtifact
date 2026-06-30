//! 统一任务注册表端点（FR-131，修订 ADR-0019）：跨 kind 列出活跃 + 近期任务、查单任务进度。
//!
//! handler 保持薄：鉴权（仅 Admin）、读统一注册表，`GET /tasks/{id}` 据 kind 从对应 kind 专表
//! （`MigrationJobs` / `UpdateJobs`）附进度明细。进度单一真源仍在专表，本端点不复制进度。

use std::sync::Arc;

use axum::extract::{Path, State};
use axum::{Extension, Json};
use serde::Serialize;

use super::task_registry::{TaskKind, TaskRecord};
use super::{ApiError, AppState, Identity, MigrationJobs, UpdateJobs};

/// 单任务详情响应（FR-131）：统一记录 + 据 kind 附进度明细（取不到则仅记录）。
#[derive(Debug, Serialize)]
pub struct TaskDetailDto {
    /// 统一任务记录（展平）。
    #[serde(flatten)]
    pub record: TaskRecord,
    /// 迁移进度明细（仅 `kind == migration` 且专表仍有该 job 时填）。
    #[serde(skip_serializing_if = "Option::is_none")]
    pub migration: Option<crate::migrate::OnlinePullProgress>,
    /// 更新进度明细（仅 `kind == update` 且专表仍有该 job 时填）。
    #[serde(skip_serializing_if = "Option::is_none")]
    pub update: Option<crate::update::UpdateProgress>,
}

/// 列出活跃 + 近期任务（跨 kind，仅 Admin，FR-131）。
pub async fn list_tasks(
    State(state): State<AppState>,
    identity: Identity,
) -> Result<Json<Vec<TaskRecord>>, ApiError> {
    identity.require_admin()?;
    Ok(Json(state.tasks.list()))
}

/// 查询某任务统一记录 + 进度明细（仅 Admin，FR-131）。未知 id 返回 404。
pub async fn get_task(
    State(state): State<AppState>,
    Extension(migration_jobs): Extension<Arc<MigrationJobs>>,
    Extension(update_jobs): Extension<Arc<UpdateJobs>>,
    identity: Identity,
    Path(id): Path<String>,
) -> Result<Json<TaskDetailDto>, ApiError> {
    identity.require_admin()?;
    let record = state.tasks.get(&id).ok_or(ApiError::NotFound)?;
    // 据 kind 从对应 kind 专表取进度明细（取不到——如已被专表淘汰——则仅回记录）
    let (migration, update) = match record.kind {
        TaskKind::Migration => {
            let p = migration_jobs
                .get(&id)
                .map(|slot| slot.lock().unwrap_or_else(|e| e.into_inner()).clone());
            (p, None)
        }
        TaskKind::Update => {
            let p = update_jobs
                .get(&id)
                .map(|slot| slot.lock().unwrap_or_else(|e| e.into_inner()).clone());
            (None, p)
        }
        TaskKind::Vuln => (None, None),
    };
    Ok(Json(TaskDetailDto {
        record,
        migration,
        update,
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

    /// 便捷：带可选 Bearer 令牌 GET 某端点。
    async fn 请求(state: AppState, path: &str, 令牌: Option<&str>) -> axum::response::Response {
        let app = super::super::build_router(state);
        let mut builder = Request::builder().method("GET").uri(path);
        if let Some(t) = 令牌 {
            builder = builder.header("Authorization", format!("Bearer {t}"));
        }
        app.oneshot(builder.body(Body::empty()).unwrap())
            .await
            .unwrap()
    }

    // ---------- 鉴权矩阵：/tasks 列表与 /tasks/{id} ----------

    #[tokio::test]
    async fn 列表_匿名被拒_401() {
        let (state, _dir) = 测试用状态().await;
        let resp = 请求(state, "/api/v1/tasks", None).await;
        assert_eq!(resp.status(), StatusCode::UNAUTHORIZED);
    }

    #[tokio::test]
    async fn 列表_普通用户被拒_403() {
        let (state, _dir) = 测试用状态().await;
        let token = 签发令牌(&state, "u", Role::User).await;
        let resp = 请求(state, "/api/v1/tasks", Some(&token)).await;
        assert_eq!(resp.status(), StatusCode::FORBIDDEN);
    }

    #[tokio::test]
    async fn 列表_管理员_200_含登记任务() {
        let (state, _dir) = 测试用状态().await;
        // 预置三类任务进统一表
        state
            .tasks
            .register(TaskKind::Migration, Some("迁移".to_string()));
        state
            .tasks
            .register(TaskKind::Update, Some("更新".to_string()));
        state
            .tasks
            .register(TaskKind::Vuln, Some("漏洞库刷新".to_string()));
        let token = 签发令牌(&state, "admin", Role::Admin).await;
        let resp = 请求(state, "/api/v1/tasks", Some(&token)).await;
        assert_eq!(resp.status(), StatusCode::OK);
        let body = 读_json(resp).await;
        let arr = body.as_array().expect("应为数组");
        assert_eq!(arr.len(), 3, "活跃 + 近期三类任务都应列出");
        // 跨 kind 列出
        let kinds: Vec<&str> = arr.iter().filter_map(|t| t["kind"].as_str()).collect();
        assert!(kinds.contains(&"migration"));
        assert!(kinds.contains(&"update"));
        assert!(kinds.contains(&"vuln"));
    }

    #[tokio::test]
    async fn 详情_未知id_404() {
        let (state, _dir) = 测试用状态().await;
        let token = 签发令牌(&state, "admin", Role::Admin).await;
        let resp = 请求(state, "/api/v1/tasks/不存在", Some(&token)).await;
        assert_eq!(resp.status(), StatusCode::NOT_FOUND);
    }

    #[tokio::test]
    async fn 详情_匿名被拒_401() {
        let (state, _dir) = 测试用状态().await;
        let id = state.tasks.register(TaskKind::Vuln, None);
        let resp = 请求(state, &format!("/api/v1/tasks/{id}"), None).await;
        assert_eq!(resp.status(), StatusCode::UNAUTHORIZED);
    }

    #[tokio::test]
    async fn 详情_管理员_200_返回统一记录() {
        let (state, _dir) = 测试用状态().await;
        let id = state
            .tasks
            .register(TaskKind::Vuln, Some("漏洞库刷新".to_string()));
        let token = 签发令牌(&state, "admin", Role::Admin).await;
        let resp = 请求(state, &format!("/api/v1/tasks/{id}"), Some(&token)).await;
        assert_eq!(resp.status(), StatusCode::OK);
        let body = 读_json(resp).await;
        assert_eq!(body["id"], id);
        assert_eq!(body["kind"], "vuln");
        assert_eq!(body["state"], "running");
        assert_eq!(body["label"], "漏洞库刷新");
        // vuln 任务无 kind 专表进度
        assert!(body.get("migration").is_none());
        assert!(body.get("update").is_none());
    }

    #[tokio::test]
    async fn 找回_已完成任务仍可经列表查到() {
        let (state, _dir) = 测试用状态().await;
        let id = state.tasks.register(TaskKind::Migration, None);
        state
            .tasks
            .set_state(&id, crate::api::TaskState::Succeeded, None);
        let token = 签发令牌(&state, "admin", Role::Admin).await;
        let resp = 请求(state, "/api/v1/tasks", Some(&token)).await;
        assert_eq!(resp.status(), StatusCode::OK);
        let body = 读_json(resp).await;
        let arr = body.as_array().unwrap();
        assert_eq!(arr.len(), 1);
        assert_eq!(
            arr[0]["state"], "succeeded",
            "已完成任务仍在近期历史中可找回"
        );
    }
}
