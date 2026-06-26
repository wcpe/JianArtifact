//! 主机 / 系统监控查询端点（FR-98，ADR-0023）：按请求采样返回本机基础资源画像。
//!
//! 设计要点：
//! - **薄 handler**：只做鉴权编排（仅 Admin）、取共享 `System` 串行刷新采样、组装响应；
//!   采集与汇总逻辑下沉 `crate::monitor`，本 handler 不写业务。
//! - **仅 Admin**：主机资源画像属管理视图，未认证 401、非管理员 403。
//! - **本机内部、不外发**（ADR-0009 / 0015 基调）：纯本地采样查询，不接任何外部上报 / 导出。
//! - **按请求采样**：`sysinfo::System` 的 refresh 需 `&mut`，单进程共享一份经 `Mutex` 串行化；
//!   不做后台轮询、不落库。GET 读取类不入审计（与 FR-97 一致）。

use axum::{extract::State, Json};

use crate::monitor::{self, HostMetrics};

use super::{ApiError, AppState, Identity};

/// 查询主机指标（仅 Admin）：刷新共享 `System` 采样并返回 CPU / 内存 / 磁盘 / uptime 快照。
///
/// 锁内只做刷新 + 读数（纯内存 + 系统调用，无网络 / blob IO），符合「锁外做重 IO」。
pub async fn monitor_host(
    State(state): State<AppState>,
    identity: Identity,
) -> Result<Json<HostMetrics>, ApiError> {
    identity.require_admin()?;
    // 串行化共享 System 的刷新与读数；磁盘列表随请求新建刷新
    let mut system = state.host_system.lock().await;
    let mut disks = sysinfo::Disks::new();
    let metrics = monitor::collect(&mut system, &mut disks);
    Ok(Json(metrics))
}

#[cfg(test)]
mod tests {
    use super::super::tests::{测试用状态, 读_json};
    use super::super::AppState;
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

    /// 便捷：带可选 Bearer 令牌请求主机监控端点。
    async fn 请求监控(state: AppState, 令牌: Option<&str>) -> axum::response::Response {
        let app = super::super::build_router(state);
        let mut builder = Request::builder().uri("/api/v1/monitor/host");
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
        let resp = 请求监控(state, None).await;
        assert_eq!(resp.status(), StatusCode::UNAUTHORIZED);
    }

    #[tokio::test]
    async fn 普通用户访问被拒_403() {
        let (state, _dir) = 测试用状态().await;
        let token = 签发令牌(&state, "u", Role::User).await;
        let resp = 请求监控(state, Some(&token)).await;
        assert_eq!(resp.status(), StatusCode::FORBIDDEN);
    }

    #[tokio::test]
    async fn 管理员查询返回主机指标结构() {
        let (state, _dir) = 测试用状态().await;
        let token = 签发令牌(&state, "admin", Role::Admin).await;
        let resp = 请求监控(state, Some(&token)).await;
        assert_eq!(resp.status(), StatusCode::OK);
        let body = 读_json(resp).await;
        // 结构含 cpu / memory / disk / uptime_secs
        assert!(body["cpu"].is_object(), "应含 cpu 字段");
        assert!(body["memory"].is_object(), "应含 memory 字段");
        assert!(body["disk"].is_object(), "应含 disk 字段");
        assert!(body["uptime_secs"].is_u64(), "应含 uptime_secs 字段");
        // 合理范围：内存总量 > 0、逻辑核数 ≥ 1
        assert!(
            body["memory"]["total_bytes"].as_u64().unwrap() > 0,
            "内存总量应大于 0"
        );
        assert!(
            body["cpu"]["logical_cores"].as_u64().unwrap() >= 1,
            "逻辑核数应至少为 1"
        );
        // 磁盘明细为数组（可能为空，取决于运行环境）
        assert!(body["disk"]["disks"].is_array(), "disk.disks 应为数组");
    }
}
