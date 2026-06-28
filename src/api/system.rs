//! 系统管理端点（FR-109，ADR-0033）：手动重启 / 关闭，仅 Admin。
//!
//! handler 保持薄：鉴权（`require_admin`）、抢自更新单飞互斥、置位重启请求触发优雅停机，
//! 不写停机逻辑（复用 ADR-0021/0032 的 `RestartHandle` + `main` 的 `handle_restart`）。
//! 纯本地进程操作、不出站，故**不受 `[update] enabled` 约束**（同 rollback）。

use axum::extract::State;
use axum::Json;
use serde::Serialize;

use crate::update::{RestartMode, RestartRequest};

use super::{ApiError, AppState, Identity};

/// 系统操作成功响应（重启 / 关闭共用）。
#[derive(Debug, Serialize)]
pub struct SystemActionResponse {
    /// 固定状态文案。
    pub status: String,
}

/// 抢单飞 + 置位重启请求的公共逻辑（重启 / 关闭仅 `mode` 与文案不同）。
///
/// 与自更新 apply / rollback **共用单飞互斥**：同一时刻只允许一个进程级变更在途（升级 / 回滚 /
/// 重启 / 关闭），抢不到立即 409「更新进行中」，杜绝并发停机或与换二进制互踩。透传当前 argv
/// （不含 argv[0]）；`exe` 取当前运行二进制（重启不换二进制、按 `mode` 拉起 / 退出）。
fn place_restart_request(state: &AppState, mode: RestartMode) -> Result<(), ApiError> {
    let _apply_guard = state
        .restart
        .try_begin_apply()
        .ok_or_else(|| ApiError::Conflict("更新进行中".to_string()))?;
    let exe = std::env::current_exe().map_err(|e| {
        tracing::error!(错误 = %e, "无法定位当前可执行文件，拒绝系统操作");
        ApiError::Internal
    })?;
    let argv: Vec<std::ffi::OsString> = std::env::args_os().skip(1).collect();
    state
        .restart
        .request_restart(RestartRequest { mode, exe, argv });
    Ok(())
}

/// 手动重启（仅 Admin，FR-109）：按运行时 `restart_mode` 拉起 / 交进程管理器重启（不换二进制）。
///
/// 成功返回 `200 {status}`，随后排空在途请求后据 `restart_mode` 重启（self 原地 exec / exit 交管理器，
/// 见 ADR-0032）。非 Admin / 匿名 403 / 401；与自更新在途冲突 → 409「更新进行中」。
pub async fn system_restart(
    State(state): State<AppState>,
    identity: Identity,
) -> Result<Json<SystemActionResponse>, ApiError> {
    identity.require_admin()?;
    // 重启模式读热替换槽当前值（与自更新重启同链路）
    let mode = RestartMode::from_config(&state.settings.update().restart_mode);
    place_restart_request(&state, mode)?;
    tracing::info!("已置位系统重启请求，等待优雅停机后按 restart_mode 重启");
    Ok(Json(SystemActionResponse {
        status: "正在重启".to_string(),
    }))
}

/// 手动关闭（仅 Admin，FR-109）：优雅排空后退出、不自拉起（强制 `Exit`）。
///
/// 成功返回 `200 {status}`，随后排空在途请求后进程退出。**若部署配了自动重启的进程管理器
/// （systemd / docker），进程会被其再起**——真正停机须经该管理器（见 ADR-0033）。
/// 非 Admin / 匿名 403 / 401；与自更新在途冲突 → 409「更新进行中」。
pub async fn system_shutdown(
    State(state): State<AppState>,
    identity: Identity,
) -> Result<Json<SystemActionResponse>, ApiError> {
    identity.require_admin()?;
    // 关闭强制 Exit：优雅退出、不自拉起（self 模式下也不得变成重启）
    place_restart_request(&state, RestartMode::Exit)?;
    tracing::info!("已置位系统关闭请求，等待优雅停机后退出");
    Ok(Json(SystemActionResponse {
        status: "正在关闭".to_string(),
    }))
}

#[cfg(test)]
mod tests {
    use super::super::tests::测试用状态;
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

    /// 便捷：带可选 Bearer 令牌 POST 某系统端点。
    async fn 请求(state: AppState, path: &str, 令牌: Option<&str>) -> axum::response::Response {
        let app = super::super::build_router(state);
        let mut builder = Request::builder().method("POST").uri(path);
        if let Some(t) = 令牌 {
            builder = builder.header("Authorization", format!("Bearer {t}"));
        }
        app.oneshot(builder.body(Body::empty()).unwrap())
            .await
            .unwrap()
    }

    // ---------- 鉴权矩阵：匿名 401 / User 403（重启 + 关闭各一遍）----------

    #[tokio::test]
    async fn restart_匿名被拒_401() {
        let (state, _dir) = 测试用状态().await;
        let restart = state.restart.clone();
        let resp = 请求(state, "/api/v1/system/restart", None).await;
        assert_eq!(resp.status(), StatusCode::UNAUTHORIZED);
        assert!(restart.take().is_none(), "未鉴权不得置位重启请求");
    }

    #[tokio::test]
    async fn restart_普通用户被拒_403() {
        let (state, _dir) = 测试用状态().await;
        let restart = state.restart.clone();
        let token = 签发令牌(&state, "u", Role::User).await;
        let resp = 请求(state, "/api/v1/system/restart", Some(&token)).await;
        assert_eq!(resp.status(), StatusCode::FORBIDDEN);
        assert!(restart.take().is_none(), "非 Admin 不得置位重启请求");
    }

    #[tokio::test]
    async fn shutdown_匿名被拒_401() {
        let (state, _dir) = 测试用状态().await;
        let restart = state.restart.clone();
        let resp = 请求(state, "/api/v1/system/shutdown", None).await;
        assert_eq!(resp.status(), StatusCode::UNAUTHORIZED);
        assert!(restart.take().is_none(), "未鉴权不得置位重启请求");
    }

    #[tokio::test]
    async fn shutdown_普通用户被拒_403() {
        let (state, _dir) = 测试用状态().await;
        let restart = state.restart.clone();
        let token = 签发令牌(&state, "u", Role::User).await;
        let resp = 请求(state, "/api/v1/system/shutdown", Some(&token)).await;
        assert_eq!(resp.status(), StatusCode::FORBIDDEN);
        assert!(restart.take().is_none(), "非 Admin 不得置位重启请求");
    }

    // ---------- Admin 放行：置位重启请求，模式正确 ----------

    #[tokio::test]
    async fn restart_管理员放行_置位请求() {
        let (state, _dir) = 测试用状态().await;
        let restart = state.restart.clone();
        let token = 签发令牌(&state, "admin", Role::Admin).await;
        let resp = 请求(state, "/api/v1/system/restart", Some(&token)).await;
        assert_eq!(resp.status(), StatusCode::OK);
        let req = restart.take().expect("Admin 重启应置位重启请求");
        // 默认 restart_mode 解析为自拉起（裸进程最安全默认）
        assert_eq!(req.mode, RestartMode::SelfRespawn);
    }

    #[tokio::test]
    async fn shutdown_管理员放行_强制_exit() {
        let (state, _dir) = 测试用状态().await;
        let restart = state.restart.clone();
        let token = 签发令牌(&state, "admin", Role::Admin).await;
        let resp = 请求(state, "/api/v1/system/shutdown", Some(&token)).await;
        assert_eq!(resp.status(), StatusCode::OK);
        let req = restart.take().expect("Admin 关闭应置位重启请求");
        // 关闭强制 Exit：不得自拉起
        assert_eq!(req.mode, RestartMode::Exit);
    }

    // ---------- 单飞互斥：与自更新共用，占用时 409 且不置位 ----------

    #[tokio::test]
    async fn restart_并发在途返回_409_更新进行中() {
        let (state, _dir) = 测试用状态().await;
        let restart = state.restart.clone();
        let token = 签发令牌(&state, "admin", Role::Admin).await;
        let in_flight = restart.try_begin_apply().expect("测试前置：首个抢占应成功");

        let resp = 请求(state, "/api/v1/system/restart", Some(&token)).await;
        assert_eq!(resp.status(), StatusCode::CONFLICT);
        let body = super::super::tests::读_json(resp).await;
        assert_eq!(body["error"]["message"], "更新进行中");
        assert!(restart.take().is_none(), "在途时不得置位重启请求");

        drop(in_flight);
        assert!(
            restart.try_begin_apply().is_some(),
            "在途结束后标志应复位、可再次触发"
        );
    }

    #[tokio::test]
    async fn shutdown_并发在途返回_409_更新进行中() {
        let (state, _dir) = 测试用状态().await;
        let restart = state.restart.clone();
        let token = 签发令牌(&state, "admin", Role::Admin).await;
        let in_flight = restart.try_begin_apply().expect("测试前置：首个抢占应成功");

        let resp = 请求(state, "/api/v1/system/shutdown", Some(&token)).await;
        assert_eq!(resp.status(), StatusCode::CONFLICT);
        let body = super::super::tests::读_json(resp).await;
        assert_eq!(body["error"]["message"], "更新进行中");
        assert!(restart.take().is_none(), "在途时不得置位重启请求");

        drop(in_flight);
    }
}
