//! 在线更新入口端点（FR-85/87，ADR-0021；FR-126 异步化）：检查 / 应用 / 回滚，仅 Admin。
//!
//! FR-126：检查 / 应用 / 回滚改为**进程内异步 job**——触发端点立即返回 `job_id`（202），后台任务
//! 逐阶段更新进度，前端经 `GET /update/jobs/{id}` 轮询。apply / 检查的**终态**留存到数据目录状态
//! 文件（`update::state`），重启后回填进度供续看。`GET /update/check` 改为**只读留存结果**（不联网），
//! `POST /update/check` 才触发联网检查 job。
//!
//! handler 保持薄：鉴权（`require_admin`）、抢单飞、起后台任务、置位重启请求，业务在 `update` 模块。
//! 出站经统一出站客户端 helper（FR-84，honor `[network.proxy]`）；token / 凭据绝不进日志 / 错误 /
//! 序列化回显 / 状态文件。

use std::sync::{Arc, Mutex};

use axum::extract::{Path, State};
use axum::{http::StatusCode, Extension, Json};
use serde::Serialize;

use crate::config::UpdateChannel;
use crate::update::{
    self, ApplyGuard, GithubReleaseSource, ProgressSlot, RestartMode, RestartRequest, UpdateCheck,
    UpdateError, UpdateKind, UpdatePhase, UpdateProgress,
};

use super::{ApiError, AppState, Identity, TaskKind, TaskRegistry, TaskState, UpdateJobs};

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
            // 无可回滚的备份版本 → 409（FR-104，明确文案、不静默）
            UpdateError::NoBackup => ApiError::Conflict("无可回滚的备份版本".to_string()),
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

/// 异步任务触发响应（FR-126）：返回 `job_id`，执行在后台。
#[derive(Debug, Serialize)]
pub struct UpdateJobCreatedDto {
    /// 任务 id，供轮询 `GET /update/jobs/{id}`。
    pub job_id: String,
}

/// 单任务进度响应（FR-126）：`job_id` + 进度快照（展平）。
#[derive(Debug, Serialize)]
pub struct UpdateJobDto {
    /// 任务 id。
    pub job_id: String,
    /// 进度快照。
    #[serde(flatten)]
    pub progress: UpdateProgress,
}

/// 留存的检查结果响应（FR-126）：`GET /update/check` 只读，不联网。
#[derive(Debug, Serialize)]
pub struct CachedCheckDto {
    /// 上次检查结果（无留存为 `None`）。
    pub result: Option<UpdateCheck>,
    /// 检查时刻（Unix 秒，无留存为 `None`）。
    pub checked_at: Option<u64>,
}

/// 据**热替换槽当前值**构造 GitHub Release 来源 + 解析当前更新通道（出站默认关闭时返回 `Disabled`）。
///
/// 读运行时可编辑设置槽（FR-88，ADR-0022）的在线更新配置（含 `enabled` 开关与 `channel` 通道），
/// 出站经共享出站网络热替换槽取当前 client；PATCH 翻 `enabled` / 改 repo / 改 channel / 改代理后即时生效。
fn build_source(state: &AppState) -> Result<(GithubReleaseSource, UpdateChannel), UpdateError> {
    let cfg = state.settings.update();
    if !cfg.enabled {
        return Err(UpdateError::Disabled);
    }
    let source = GithubReleaseSource::with_network_state(
        state.settings.network.clone(),
        cfg.api_base_url.clone(),
        cfg.repo.clone(),
        cfg.token.clone(),
        std::time::Duration::from_secs(cfg.download_timeout_secs),
    );
    Ok((source, UpdateChannel::from_config(&cfg.channel)))
}

/// 当前运行版本：优先 CI 注入的完整版本串（含 prerelease `dev.N.sha`），回退 `CARGO_PKG_VERSION`。
fn current_version() -> &'static str {
    crate::version::build_version()
}

/// 生成新 job_id（UUID v4）。
fn new_job_id() -> String {
    uuid::Uuid::new_v4().to_string()
}

/// 同步更新任务终态到统一任务注册表（FR-131）：`error == Some` → Failed（带错误），否则 Succeeded。
///
/// apply / rollback 成功后进程会重启（kind 专表标 Restarting）；统一表把成功视为 Succeeded
/// （进度明细仍由 kind 专表的 Restarting 体现，统一表只记轻量终态）。
fn sync_update_task_state(tasks: &Arc<TaskRegistry>, job_id: &str, error: Option<String>) {
    match error {
        Some(e) => tasks.set_state(job_id, TaskState::Failed, Some(e)),
        None => tasks.set_state(job_id, TaskState::Succeeded, None),
    }
}

/// 触发在线更新检查（仅 Admin，FR-126 异步）：起后台联网检查 job，立即返回 `job_id`（202）。
///
/// `enabled=false` 返回 409「在线更新未启用」（不联网、不开任务）；非 Admin / 匿名 403 / 401。
/// 检查完成后把结果写进度并留存到状态文件，供 `GET /update/check` 不联网读回。
pub async fn trigger_check_update(
    State(state): State<AppState>,
    Extension(jobs): Extension<Arc<UpdateJobs>>,
    identity: Identity,
) -> Result<(StatusCode, Json<UpdateJobCreatedDto>), ApiError> {
    identity.require_admin()?;
    // 同步预校验：未启用即 409、不开任务（沿用 FR-85 出站默认关闭门）
    let (source, channel) = build_source(&state)?;

    let job_id = new_job_id();
    let progress: Arc<ProgressSlot> = Arc::new(Mutex::new(UpdateProgress::new(
        UpdateKind::Check,
        current_version(),
    )));
    jobs.register(job_id.clone(), progress.clone());
    // 登记统一任务（FR-131）
    state.tasks.register_with_id(
        job_id.clone(),
        TaskKind::Update,
        Some("检查更新".to_string()),
    );

    let data_dir = state.config.data.data_dir.clone();
    let tasks = state.tasks.clone();
    let task_job_id = job_id.clone();
    tokio::spawn(async move {
        let result = update::check_with_progress(&source, channel, current_version()).await;
        // 先在锁内更新进度并取出 check（不持锁做 IO：锁外再留存）；同时取出终态错误供统一表收敛
        let mut task_error: Option<String> = None;
        let check_to_persist = {
            let mut p = progress.lock().unwrap_or_else(|e| e.into_inner());
            match result {
                Ok(check) => {
                    p.phase = UpdatePhase::Done;
                    p.latest_version = Some(check.latest_version.clone());
                    p.check = Some(check.clone());
                    Some(check)
                }
                Err(e) => {
                    tracing::warn!(任务 = %task_job_id, "在线更新检查任务失败");
                    let msg = format!("{e}");
                    p.fail(msg.clone());
                    task_error = Some(msg);
                    None
                }
            }
        };
        // 同步统一任务终态（FR-131）：检查成功 succeeded、失败 failed
        sync_update_task_state(&tasks, &task_job_id, task_error);
        // 锁外留存检查结果（不含凭据），供 GET /update/check 不联网读回
        if let Some(check) = check_to_persist {
            let persist = update::update_state(&data_dir, |s| {
                s.last_check = Some(update::CachedCheck {
                    result: check,
                    checked_at: update::now_unix_secs(),
                });
            })
            .await;
            if let Err(e) = persist {
                tracing::warn!(错误 = %e, "留存更新检查结果失败（不影响本次结果展示）");
            }
        }
    });

    Ok((StatusCode::ACCEPTED, Json(UpdateJobCreatedDto { job_id })))
}

/// 读取留存的检查结果（仅 Admin，FR-126）：不联网，返回上次检查结果（无则空）。
pub async fn get_cached_check(
    State(state): State<AppState>,
    identity: Identity,
) -> Result<Json<CachedCheckDto>, ApiError> {
    identity.require_admin()?;
    let state_data = update::load_state(&state.config.data.data_dir).await;
    let cached = state_data.and_then(|s| s.last_check);
    Ok(Json(match cached {
        Some(c) => CachedCheckDto {
            result: Some(c.result),
            checked_at: Some(c.checked_at),
        },
        None => CachedCheckDto {
            result: None,
            checked_at: None,
        },
    }))
}

/// 触发应用更新（仅 Admin，FR-126 异步）：抢单飞 → 起后台 apply job → 立即返回 `job_id`（202）。
///
/// 后台执行「下载 → 校验 → 替换」，替换成功后把终态（Restarting + new_version）留存到状态文件，
/// 再置位重启请求触发优雅停机。`enabled=false` 拒绝；非 Admin / 匿名 403 / 401；已有在途 409。
/// **安全门不变**：仅 sha256 校验通过才替换、原子替换、单飞互斥（apply 不支持取消）。
pub async fn apply_update(
    State(state): State<AppState>,
    Extension(jobs): Extension<Arc<UpdateJobs>>,
    identity: Identity,
) -> Result<(StatusCode, Json<UpdateJobCreatedDto>), ApiError> {
    identity.require_admin()?;

    // 进程级单飞互斥：抢占 apply 标志（鉴权之后、出站 / 替换之前），抢不到 409「更新进行中」。
    // guard 移入后台任务，持有至 apply 全程结束（含出错），析构可靠复位、不泄漏占用。
    let guard = state
        .restart
        .try_begin_apply()
        .ok_or_else(|| ApiError::Conflict("更新进行中".to_string()))?;

    // 同步预校验：未启用即 409、释放 guard、不开任务
    let (source, channel) = build_source(&state)?;

    let current_exe = std::env::current_exe().map_err(|e| {
        tracing::error!(错误 = %e, "无法定位当前可执行文件，拒绝自更新");
        ApiError::Internal
    })?;

    let job_id = new_job_id();
    let progress: Arc<ProgressSlot> = Arc::new(Mutex::new(UpdateProgress::new(
        UpdateKind::Apply,
        current_version(),
    )));
    jobs.register(job_id.clone(), progress.clone());
    // 登记统一任务（FR-131）
    state.tasks.register_with_id(
        job_id.clone(),
        TaskKind::Update,
        Some("应用更新".to_string()),
    );

    let data_dir = state.config.data.data_dir.clone();
    let restart = state.restart.clone();
    let restart_mode = RestartMode::from_config(&state.settings.update().restart_mode);
    let task_job_id = job_id.clone();
    tokio::spawn(run_apply_job(ApplyJobCtx {
        source,
        channel,
        current_exe,
        data_dir,
        progress,
        restart,
        restart_mode,
        guard,
        tasks: state.tasks.clone(),
        job_id: task_job_id,
        kind: UpdateKind::Apply,
    }));

    Ok((StatusCode::ACCEPTED, Json(UpdateJobCreatedDto { job_id })))
}

/// 触发回滚（仅 Admin，FR-104 + FR-126 异步）：与 apply 共用单飞，起后台 rollback job、立即返回 job_id。
///
/// 纯本地操作、不出站，**不受 `[update] enabled` 约束**。无可回滚备份 → 后台任务标 Failed
/// （同步阶段不预检备份存在性，交后台 `rollback` 报 NoBackup）。
pub async fn rollback_update(
    State(state): State<AppState>,
    Extension(jobs): Extension<Arc<UpdateJobs>>,
    identity: Identity,
) -> Result<(StatusCode, Json<UpdateJobCreatedDto>), ApiError> {
    identity.require_admin()?;

    let guard = state
        .restart
        .try_begin_apply()
        .ok_or_else(|| ApiError::Conflict("更新进行中".to_string()))?;

    let current_exe = std::env::current_exe().map_err(|e| {
        tracing::error!(错误 = %e, "无法定位当前可执行文件，拒绝回滚");
        ApiError::Internal
    })?;

    let job_id = new_job_id();
    let progress: Arc<ProgressSlot> = Arc::new(Mutex::new(UpdateProgress::new(
        UpdateKind::Rollback,
        current_version(),
    )));
    jobs.register(job_id.clone(), progress.clone());
    // 登记统一任务（FR-131）
    state.tasks.register_with_id(
        job_id.clone(),
        TaskKind::Update,
        Some("回滚更新".to_string()),
    );

    let data_dir = state.config.data.data_dir.clone();
    let restart = state.restart.clone();
    let restart_mode = RestartMode::from_config(&state.settings.update().restart_mode);
    let tasks = state.tasks.clone();
    let task_job_id = job_id.clone();
    let progress_task = progress.clone();
    tokio::spawn(async move {
        // guard 随任务持有至结束（含出错），析构可靠复位
        let _guard = guard;
        {
            let mut p = progress_task.lock().unwrap_or_else(|e| e.into_inner());
            p.phase = UpdatePhase::Replacing;
        }
        tracing::info!(任务 = %task_job_id, "在线更新：开始回滚到上一版二进制");
        match update::rollback(&current_exe).await {
            Ok(outcome) => {
                persist_and_restart(
                    &data_dir,
                    &progress_task,
                    &restart,
                    restart_mode,
                    None,
                    outcome.exe,
                    UpdateKind::Rollback,
                )
                .await;
                // 同步统一任务终态（FR-131）：回滚成功（即将重启）视为 succeeded
                sync_update_task_state(&tasks, &task_job_id, None);
            }
            Err(e) => {
                tracing::warn!(任务 = %task_job_id, "在线更新回滚任务失败");
                let msg = format!("{e}");
                progress_task
                    .lock()
                    .unwrap_or_else(|e| e.into_inner())
                    .fail(msg.clone());
                sync_update_task_state(&tasks, &task_job_id, Some(msg));
            }
        }
    });

    Ok((StatusCode::ACCEPTED, Json(UpdateJobCreatedDto { job_id })))
}

/// apply 后台任务上下文（避免参数过多 clippy 告警）。
struct ApplyJobCtx {
    source: GithubReleaseSource,
    channel: UpdateChannel,
    current_exe: std::path::PathBuf,
    data_dir: std::path::PathBuf,
    progress: Arc<ProgressSlot>,
    restart: Arc<update::RestartHandle>,
    restart_mode: RestartMode,
    guard: ApplyGuard,
    tasks: Arc<TaskRegistry>,
    job_id: String,
    kind: UpdateKind,
}

/// apply 后台任务主体：执行带进度的 apply，成功则留存终态 + 置位重启，失败标 Failed。
async fn run_apply_job(ctx: ApplyJobCtx) {
    // guard 持有至任务结束（含出错），析构可靠复位单飞标志
    let _guard = ctx.guard;
    let result = update::apply_update_with_progress(
        &ctx.source,
        ctx.channel,
        current_version(),
        &ctx.current_exe,
        &ctx.data_dir,
        Some(&ctx.progress),
    )
    .await;
    match result {
        Ok(outcome) => {
            persist_and_restart(
                &ctx.data_dir,
                &ctx.progress,
                &ctx.restart,
                ctx.restart_mode,
                Some(outcome.new_version.clone()),
                outcome.exe,
                ctx.kind,
            )
            .await;
            // 同步统一任务终态（FR-131）：替换成功（即将重启）视为 succeeded
            sync_update_task_state(&ctx.tasks, &ctx.job_id, None);
        }
        Err(e) => {
            tracing::warn!(任务 = %ctx.job_id, "在线更新应用任务失败");
            let msg = format!("{e}");
            ctx.progress
                .lock()
                .unwrap_or_else(|e| e.into_inner())
                .fail(msg.clone());
            sync_update_task_state(&ctx.tasks, &ctx.job_id, Some(msg));
        }
    }
}

/// 替换 / 回滚成功后的收尾：标终态 Restarting + 留存到状态文件 + 置位重启请求。
///
/// **次序**：先把终态留存到状态文件（重启后可读回续看），再置位重启请求触发优雅停机——确保进程
/// 在停机前已把「上次更新结果」落盘。
async fn persist_and_restart(
    data_dir: &std::path::Path,
    progress: &Arc<ProgressSlot>,
    restart: &Arc<update::RestartHandle>,
    restart_mode: RestartMode,
    new_version: Option<String>,
    exe: std::path::PathBuf,
    kind: UpdateKind,
) {
    // 组装终态快照（Restarting）
    let snapshot = {
        let mut p = progress.lock().unwrap_or_else(|e| e.into_inner());
        p.phase = UpdatePhase::Restarting;
        p.new_version = new_version;
        p.clone()
    };
    // 先留存终态（重启后回填续看），失败仅 WARN、不阻断重启
    let persist = update::update_state(data_dir, |s| s.last_apply = Some(snapshot)).await;
    if let Err(e) = persist {
        tracing::warn!(错误 = %e, "留存更新终态失败（不影响本次替换与重启）");
    }
    // 置位重启请求（透传当前 argv，不含 argv[0]）+ 触发优雅停机
    let argv: Vec<std::ffi::OsString> = std::env::args_os().skip(1).collect();
    restart.request_restart(RestartRequest {
        mode: restart_mode,
        exe,
        argv,
    });
    let 动作 = match kind {
        UpdateKind::Rollback => "回滚",
        _ => "升级",
    };
    tracing::info!(
        动作 = 动作,
        "在线更新：已置位重启请求，等待优雅停机后拉起目标版本"
    );
}

/// 查询某更新任务进度（仅 Admin，FR-126）。未知 id 返回 404。
pub async fn get_update_job(
    Extension(jobs): Extension<Arc<UpdateJobs>>,
    identity: Identity,
    Path(job_id): Path<String>,
) -> Result<Json<UpdateJobDto>, ApiError> {
    identity.require_admin()?;
    let progress = jobs.get(&job_id).ok_or(ApiError::NotFound)?;
    let snap = progress.lock().unwrap_or_else(|e| e.into_inner()).clone();
    Ok(Json(UpdateJobDto {
        job_id,
        progress: snap,
    }))
}

/// 列出活动 / 近期 + 重启后回填的更新任务（仅 Admin，FR-126），供重连续看。
pub async fn list_update_jobs(
    Extension(jobs): Extension<Arc<UpdateJobs>>,
    identity: Identity,
) -> Result<Json<Vec<UpdateJobDto>>, ApiError> {
    identity.require_admin()?;
    let list = jobs
        .list()
        .into_iter()
        .map(|(job_id, progress)| UpdateJobDto { job_id, progress })
        .collect();
    Ok(Json(list))
}

/// 重启后回填上次 apply / rollback 终态（FR-126）：从状态文件读 `last_apply`，以固定 job_id 回填注册表。
///
/// 在 `build_router` 构造 `UpdateJobs` 后调用：使重启后 `GET /update/jobs` 即含「上次更新结果」，
/// 前端无须本进程曾跑过任务也能续看。回填的进度标 `restarted=true`，区别于本进程活动任务。
pub async fn backfill_last_apply(jobs: &Arc<UpdateJobs>, data_dir: &std::path::Path) {
    let Some(state) = update::load_state(data_dir).await else {
        return;
    };
    let Some(mut last) = state.last_apply else {
        return;
    };
    last.restarted = true;
    let progress: Arc<ProgressSlot> = Arc::new(Mutex::new(last));
    jobs.register("last-apply".to_string(), progress);
    tracing::info!("在线更新：已从状态文件回填上次更新终态，供重启后续看");
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

    /// 便捷：带可选 Bearer 令牌请求某更新端点。
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

    // ---------- 鉴权矩阵：检查触发 / 读取 / 应用 / 回滚 / 任务查询 ----------

    #[tokio::test]
    async fn check_触发_匿名被拒_401() {
        let (state, _dir) = 测试用状态().await;
        let resp = 请求(state, "/api/v1/update/check", "POST", None).await;
        assert_eq!(resp.status(), StatusCode::UNAUTHORIZED);
    }

    #[tokio::test]
    async fn check_触发_普通用户被拒_403() {
        let (state, _dir) = 测试用状态().await;
        let token = 签发令牌(&state, "u", Role::User).await;
        let resp = 请求(state, "/api/v1/update/check", "POST", Some(&token)).await;
        assert_eq!(resp.status(), StatusCode::FORBIDDEN);
    }

    #[tokio::test]
    async fn check_触发_管理员但未启用_409() {
        // 默认配置 update.enabled=false：管理员触发检查亦返回 409，不联网、不开任务
        let (state, _dir) = 测试用状态().await;
        let token = 签发令牌(&state, "admin", Role::Admin).await;
        let resp = 请求(state, "/api/v1/update/check", "POST", Some(&token)).await;
        assert_eq!(resp.status(), StatusCode::CONFLICT);
    }

    #[tokio::test]
    async fn check_读取_匿名被拒_401() {
        let (state, _dir) = 测试用状态().await;
        let resp = 请求(state, "/api/v1/update/check", "GET", None).await;
        assert_eq!(resp.status(), StatusCode::UNAUTHORIZED);
    }

    #[tokio::test]
    async fn check_读取_管理员无留存返回空() {
        // GET /update/check 只读留存、不联网：未启用也应 200（不报 409），无留存返回 null 结果
        let (state, _dir) = 测试用状态().await;
        let token = 签发令牌(&state, "admin", Role::Admin).await;
        let resp = 请求(state, "/api/v1/update/check", "GET", Some(&token)).await;
        assert_eq!(resp.status(), StatusCode::OK);
        let body = super::super::tests::读_json(resp).await;
        assert!(body["result"].is_null(), "无留存时 result 应为 null");
        assert!(body["checked_at"].is_null());
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
    async fn apply_管理员但未启用_409_且不置位重启() {
        let (state, _dir) = 测试用状态().await;
        let restart = state.restart.clone();
        let token = 签发令牌(&state, "admin", Role::Admin).await;
        let resp = 请求(state, "/api/v1/update/apply", "POST", Some(&token)).await;
        assert_eq!(resp.status(), StatusCode::CONFLICT);
        assert!(restart.take().is_none(), "未启用时不得置位重启请求");
        // 单飞标志应已释放（同步预校验失败后 guard 析构）
        assert!(
            restart.try_begin_apply().is_some(),
            "未启用早返回后单飞标志应复位"
        );
    }

    #[tokio::test]
    async fn apply_并发在途第二个返回_409_更新进行中() {
        let (state, _dir) = 测试用状态().await;
        let restart = state.restart.clone();
        let token = 签发令牌(&state, "admin", Role::Admin).await;
        let in_flight = restart.try_begin_apply().expect("测试前置：首个抢占应成功");

        let resp = 请求(state, "/api/v1/update/apply", "POST", Some(&token)).await;
        assert_eq!(resp.status(), StatusCode::CONFLICT);
        let body = super::super::tests::读_json(resp).await;
        assert_eq!(body["error"]["message"], "更新进行中");
        assert!(restart.take().is_none(), "在途时不得置位重启请求");

        drop(in_flight);
        assert!(
            restart.try_begin_apply().is_some(),
            "在途结束后标志应复位、可再次 apply"
        );
    }

    #[tokio::test]
    async fn rollback_匿名被拒_401() {
        let (state, _dir) = 测试用状态().await;
        let resp = 请求(state, "/api/v1/update/rollback", "POST", None).await;
        assert_eq!(resp.status(), StatusCode::UNAUTHORIZED);
    }

    #[tokio::test]
    async fn rollback_普通用户被拒_403() {
        let (state, _dir) = 测试用状态().await;
        let token = 签发令牌(&state, "u", Role::User).await;
        let resp = 请求(state, "/api/v1/update/rollback", "POST", Some(&token)).await;
        assert_eq!(resp.status(), StatusCode::FORBIDDEN);
    }

    #[tokio::test]
    async fn rollback_并发在途第二个返回_409_更新进行中() {
        let (state, _dir) = 测试用状态().await;
        let restart = state.restart.clone();
        let token = 签发令牌(&state, "admin", Role::Admin).await;
        let in_flight = restart.try_begin_apply().expect("测试前置：首个抢占应成功");

        let resp = 请求(state, "/api/v1/update/rollback", "POST", Some(&token)).await;
        assert_eq!(resp.status(), StatusCode::CONFLICT);
        let body = super::super::tests::读_json(resp).await;
        assert_eq!(body["error"]["message"], "更新进行中");

        drop(in_flight);
    }

    #[tokio::test]
    async fn job_查询_匿名被拒_401() {
        let (state, _dir) = 测试用状态().await;
        let resp = 请求(state, "/api/v1/update/jobs/x", "GET", None).await;
        assert_eq!(resp.status(), StatusCode::UNAUTHORIZED);
    }

    #[tokio::test]
    async fn job_查询_普通用户被拒_403() {
        let (state, _dir) = 测试用状态().await;
        let token = 签发令牌(&state, "u", Role::User).await;
        let resp = 请求(state, "/api/v1/update/jobs/x", "GET", Some(&token)).await;
        assert_eq!(resp.status(), StatusCode::FORBIDDEN);
    }

    #[tokio::test]
    async fn job_查询_未知id_404() {
        let (state, _dir) = 测试用状态().await;
        let token = 签发令牌(&state, "admin", Role::Admin).await;
        let resp = 请求(state, "/api/v1/update/jobs/不存在", "GET", Some(&token)).await;
        assert_eq!(resp.status(), StatusCode::NOT_FOUND);
    }

    #[tokio::test]
    async fn jobs_列表_匿名被拒_401() {
        let (state, _dir) = 测试用状态().await;
        let resp = 请求(state, "/api/v1/update/jobs", "GET", None).await;
        assert_eq!(resp.status(), StatusCode::UNAUTHORIZED);
    }

    // ---------- 重启后回填：状态文件有终态 → GET /update/jobs 含之 ----------

    #[tokio::test]
    async fn 重启后回填_状态文件终态可经jobs读回() {
        let (state, _dir) = 测试用状态().await;
        let data_dir = state.config.data.data_dir.clone();
        // 预置状态文件：模拟「上次 apply 升级到 0.5.0、置位重启」的终态
        let mut last = UpdateProgress::new(UpdateKind::Apply, "0.4.0");
        last.phase = UpdatePhase::Restarting;
        last.new_version = Some("0.5.0".to_string());
        update::update_state(&data_dir, |s| s.last_apply = Some(last))
            .await
            .unwrap();

        // 模拟「重启」：新建注册表 + 回填（build_router 启动时做的事）
        let jobs = Arc::new(UpdateJobs::default());
        backfill_last_apply(&jobs, &data_dir).await;

        let list = jobs.list();
        assert_eq!(list.len(), 1, "回填后应有一条上次更新终态");
        let (_, p) = &list[0];
        assert_eq!(p.phase, UpdatePhase::Restarting);
        assert_eq!(p.new_version.as_deref(), Some("0.5.0"));
        assert!(p.restarted, "回填的终态应标 restarted=true");
    }
}
