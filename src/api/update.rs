//! 在线更新入口端点（FR-85，ADR-0021）：更新检查 + 应用更新，仅 Admin。
//!
//! handler 保持薄：鉴权（`require_admin`）、调用 `update` 模块编排、错误映射、置位重启请求，
//! 不写业务逻辑。出站经统一出站客户端 helper（FR-84，honor `[network.proxy]`）。
//! token / 凭据绝不进日志 / 错误 / 序列化回显。

use axum::extract::State;
use axum::Json;
use serde::Serialize;

use crate::update::{
    self, GithubReleaseSource, ReleaseSource, RestartMode, RestartRequest, UpdateCheck, UpdateError,
};

use super::{ApiError, AppState, Identity};

/// 把在线更新错误映射为 HTTP 错误（spec §3.10）。
impl From<UpdateError> for ApiError {
    fn from(e: UpdateError) -> Self {
        match e {
            // 未启用：返回 409（端点存在但功能关闭）
            UpdateError::Disabled => ApiError::Conflict("在线更新未启用".to_string()),
            // 平台不支持 → 400（明确文案）
            UpdateError::UnsupportedPlatform(p) => {
                ApiError::BadRequest(format!("当前平台不支持自更新: {p}"))
            }
            // 版本串非法 → 400
            UpdateError::InvalidVersion(v) => ApiError::BadRequest(format!("版本串非法: {v}")),
            // 无更新可用 → 409
            UpdateError::NoUpdate(msg) => ApiError::Conflict(msg),
            // 缺资产 / 校验失败 → 422（不可用的发布内容）
            UpdateError::MissingAsset(name) => {
                ApiError::UnprocessableEntity(format!("发布缺少所需资产: {name}"))
            }
            UpdateError::ChecksumMismatch => {
                ApiError::UnprocessableEntity("下载内容校验和不一致，已拒绝替换".to_string())
            }
            // 上游不可达 / 超时 / 错误状态 → 502（不泄露内部细节，仅记日志）
            UpdateError::Upstream(err) => {
                tracing::warn!(错误 = %err, "在线更新出站访问失败");
                ApiError::BadGateway
            }
            UpdateError::Parse(err) => {
                tracing::warn!(错误 = %err, "解析在线更新上游响应失败");
                ApiError::BadGateway
            }
            // 本地替换 / 落盘失败 → 500
            UpdateError::Io(err) => {
                tracing::error!(错误 = %err, "在线更新本地文件操作失败");
                ApiError::Internal
            }
        }
    }
}

/// 应用更新成功响应。
#[derive(Debug, Serialize)]
pub struct ApplyResponse {
    /// 固定状态文案。
    pub status: String,
    /// 替换后的新版本号。
    pub new_version: String,
}

/// 据**热替换槽当前值**构造 GitHub Release 来源（出站默认关闭时返回 `Disabled`）。
///
/// 读运行时可编辑设置槽（FR-88，ADR-0022）的在线更新配置（含 `enabled` 开关），出站经共享出站网络
/// 热替换槽取当前 client；PATCH 翻 `enabled` / 改 repo / 改代理后即时生效，无须重启。
fn build_source(state: &AppState) -> Result<GithubReleaseSource, UpdateError> {
    let cfg = state.settings.update();
    if !cfg.enabled {
        return Err(UpdateError::Disabled);
    }
    Ok(GithubReleaseSource::with_network_state(
        state.settings.network.clone(),
        cfg.api_base_url.clone(),
        cfg.repo.clone(),
        cfg.token.clone(),
        std::time::Duration::from_secs(cfg.download_timeout_secs),
    ))
}

/// 当前运行版本（编译期注入）。
fn current_version() -> &'static str {
    env!("CARGO_PKG_VERSION")
}

/// 更新检查（仅 Admin）：查最新稳定 Release、比对版本，返回是否有更新。
///
/// `enabled=false` 返回 409「在线更新未启用」（不联网）；非 Admin / 匿名 403 / 401。
pub async fn check_update(
    State(state): State<AppState>,
    identity: Identity,
) -> Result<Json<UpdateCheck>, ApiError> {
    identity.require_admin()?;
    let source = build_source(&state)?;
    let release = source.fetch_latest_release().await?;
    let check = update::build_check(current_version(), &release)?;
    Ok(Json(check))
}

/// 应用更新（仅 Admin，手动触发）：下载 → 校验 → 原子替换 → 置位重启请求触发优雅停机。
///
/// 成功返回 `200 {status, new_version}`，随后 axum 排空在途请求后 `serve` 返回，`main` 据
/// 重启请求拉起新进程或退出。`enabled=false` 拒绝；非 Admin / 匿名 403 / 401；
/// sha256 不一致 → 422、保留旧二进制；平台不支持 → 400；上游不可达 → 502。
pub async fn apply_update(
    State(state): State<AppState>,
    identity: Identity,
) -> Result<Json<ApplyResponse>, ApiError> {
    identity.require_admin()?;

    // 进程级单飞互斥（M2）：先抢占 apply 标志（鉴权之后、出站 / 替换之前），抢不到说明已有自更新
    // 在途，立即 409「更新进行中」，不竞争下载临时文件与 .bak/.old/.new。guard 持有至本 handler
    // 全程结束（含出错 / 早返回），析构时可靠复位，不泄漏占用。
    let _apply_guard = state
        .restart
        .try_begin_apply()
        .ok_or_else(|| ApiError::Conflict("更新进行中".to_string()))?;

    let source = build_source(&state)?;

    // 当前 exe 与数据目录由配置 / 运行时给出；替换在阻塞线程池执行，校验通过才替换
    let current_exe = std::env::current_exe().map_err(|e| {
        tracing::error!(错误 = %e, "无法定位当前可执行文件，拒绝自更新");
        ApiError::Internal
    })?;
    let data_dir = state.config.data.data_dir.clone();

    let outcome = update::apply_update(&source, current_version(), &current_exe, &data_dir).await?;

    // 替换成功：置位重启请求（透传当前 argv，不含 argv[0]）+ 触发优雅停机
    // 重启模式读热替换槽当前值（PATCH 可调），不再读启动期 config.update
    let mode = RestartMode::from_config(&state.settings.update().restart_mode);
    let argv: Vec<std::ffi::OsString> = std::env::args_os().skip(1).collect();
    state.restart.request_restart(RestartRequest {
        mode,
        exe: outcome.exe,
        argv,
    });
    tracing::info!(新版本 = %outcome.new_version, "已置位重启请求，等待优雅停机后拉起新进程");

    Ok(Json(ApplyResponse {
        status: "已更新，正在重启".to_string(),
        new_version: outcome.new_version,
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

    /// 便捷：带可选 Bearer 令牌请求某更新端点（GET check / POST apply）。
    async fn 请求(
        state: AppState,
        path: &str,
        method: &str,
        令牌: Option<&str>,
    ) -> axum::response::Response {
        let app = super::super::build_router(state);
        let mut builder = Request::builder().method(method).uri(path);
        if let Some(t) = 令牌 {
            builder = builder.header("Authorization", format!("Bearer {t}"));
        }
        app.oneshot(builder.body(Body::empty()).unwrap())
            .await
            .unwrap()
    }

    #[tokio::test]
    async fn check_匿名被拒_401() {
        let (state, _dir) = 测试用状态().await;
        let resp = 请求(state, "/api/v1/update/check", "GET", None).await;
        assert_eq!(resp.status(), StatusCode::UNAUTHORIZED);
    }

    #[tokio::test]
    async fn check_普通用户被拒_403() {
        let (state, _dir) = 测试用状态().await;
        let token = 签发令牌(&state, "u", Role::User).await;
        let resp = 请求(state, "/api/v1/update/check", "GET", Some(&token)).await;
        assert_eq!(resp.status(), StatusCode::FORBIDDEN);
    }

    #[tokio::test]
    async fn check_管理员但未启用_409() {
        // 默认配置 update.enabled=false：管理员访问亦返回 409，不联网
        let (state, _dir) = 测试用状态().await;
        let token = 签发令牌(&state, "admin", Role::Admin).await;
        let resp = 请求(state, "/api/v1/update/check", "GET", Some(&token)).await;
        assert_eq!(resp.status(), StatusCode::CONFLICT);
    }

    #[tokio::test]
    async fn apply_匿名被拒_401() {
        let (state, _dir) = 测试用状态().await;
        let resp = 请求(state, "/api/v1/update/apply", "POST", None).await;
        assert_eq!(resp.status(), StatusCode::UNAUTHORIZED);
    }

    #[tokio::test]
    async fn apply_普通用户被拒_403() {
        let (state, _dir) = 测试用状态().await;
        let token = 签发令牌(&state, "u", Role::User).await;
        let resp = 请求(state, "/api/v1/update/apply", "POST", Some(&token)).await;
        assert_eq!(resp.status(), StatusCode::FORBIDDEN);
    }

    #[tokio::test]
    async fn apply_管理员但未启用_409() {
        let (state, _dir) = 测试用状态().await;
        let restart = state.restart.clone();
        let token = 签发令牌(&state, "admin", Role::Admin).await;
        let resp = 请求(state, "/api/v1/update/apply", "POST", Some(&token)).await;
        assert_eq!(resp.status(), StatusCode::CONFLICT);
        // 未启用即不应置位重启请求
        assert!(restart.take().is_none(), "未启用时不得置位重启请求");
    }

    #[tokio::test]
    async fn apply_并发在途第二个返回_409_更新进行中() {
        // M2：手动占用 apply 单飞标志，模拟已有一次自更新在途；
        // 再触发 apply 的 Admin 应拿到 409「更新进行中」，且不进入替换 / 不置位重启请求。
        let (state, _dir) = 测试用状态().await;
        let restart = state.restart.clone();
        let token = 签发令牌(&state, "admin", Role::Admin).await;
        // 占用单飞标志（持有 guard 至断言后），代表“已有 apply 在途”
        let in_flight = restart.try_begin_apply().expect("测试前置：首个抢占应成功");

        let resp = 请求(state, "/api/v1/update/apply", "POST", Some(&token)).await;
        assert_eq!(resp.status(), StatusCode::CONFLICT);
        let body = super::super::tests::读_json(resp).await;
        assert_eq!(body["error"]["message"], "更新进行中");
        // 在途期间第二个不得置位重启请求
        assert!(restart.take().is_none(), "在途时不得置位重启请求");

        // 释放在途标志后，单飞标志复位、可再次抢占（验证 guard 未泄漏）
        drop(in_flight);
        assert!(
            restart.try_begin_apply().is_some(),
            "在途结束后标志应复位、可再次 apply"
        );
    }
}
