//! Nexus OSS 迁移入口端点（ADR-0006）：发现 / 预览 + proxy / hosted 搬运。
//!
//! - 发现 / 预览：在线 REST 入口（FR-36）+ 离线 blob store 入口（FR-37），仅枚举源系统
//!   可迁移内容与基本元数据，不搬运任何制品。
//! - 搬运：proxy 仓库配置 + 缓存制品（FR-38）、hosted 仓库配置 + 完整制品（FR-39）。
//!
//! 所有端点仅管理员可调用。handler 保持薄：解析请求、调用 `migrate` 模块编排、做错误映射，
//! 不写业务逻辑。

use std::sync::{Arc, Mutex};

use axum::extract::{Path, State};
use axum::{http::StatusCode, Extension, Json};
use serde::{Deserialize, Serialize};
use uuid::Uuid;

use crate::migrate::{
    self, HttpNexusClient, JobControl, MigrateError, NexusRepoSummary, OnlinePullPhase,
    OnlinePullProgress,
};

use super::{ApiError, AppState, Identity, MigrationJobs, TaskKind, TaskState};

/// 把迁移入口错误映射为 HTTP 错误。
impl From<MigrateError> for ApiError {
    fn from(e: MigrateError) -> Self {
        match e {
            // 入参非法 / 凭据引用未配置：可由调用方修正，归 400
            MigrateError::Invalid(msg) => ApiError::BadRequest(msg),
            MigrateError::MissingCredential(_) => {
                ApiError::BadRequest("凭据引用未在环境变量中配置".to_string())
            }
            // 源系统侧问题（鉴权失败 / 不可用 / 响应异常）统一归 502 上游网关错误，
            // 不向调用方泄露源系统内部细节，仅记录到服务日志
            MigrateError::Status(status) => {
                tracing::warn!(状态 = status, "源 Nexus 返回错误状态");
                ApiError::BadGateway
            }
            MigrateError::Transport(err) => {
                tracing::warn!(错误 = %err, "连接源 Nexus 失败");
                ApiError::BadGateway
            }
            MigrateError::Parse(err) => {
                tracing::warn!(错误 = %err, "解析源 Nexus 响应失败");
                ApiError::BadGateway
            }
        }
    }
}

/// 迁移预览请求体。
#[derive(Debug, Deserialize)]
pub struct NexusPreviewRequest {
    /// 源 Nexus 基址（如 `https://nexus.example`）。
    pub base_url: String,
    /// 上游凭据引用（仅引用；真值走 env `JIANARTIFACT_MIGRATE_<NAME>_USERNAME/PASSWORD`，不入库）。
    /// 匿名可访问的源系统可不提供。
    #[serde(default)]
    pub auth_ref: Option<String>,
}

/// 预览源 Nexus 可迁移仓库列表（仅管理员）。
///
/// 连接在线 Nexus、枚举其仓库列表与基本元数据后返回；不搬运任何制品。
pub async fn preview_nexus_repositories(
    State(state): State<AppState>,
    identity: Identity,
    Json(req): Json<NexusPreviewRequest>,
) -> Result<Json<Vec<NexusRepoSummary>>, ApiError> {
    identity.require_admin()?;

    // 复用 proxy 上游超时配置，避免慢速源系统拖垮请求线程
    // 持随 AppState 共享的出站网络热替换槽，出站时取当前 client（含运行时 PATCH 后的新代理）
    let client = HttpNexusClient::with_network_state(state.settings.network.clone());

    let repos =
        migrate::discover_repositories(&client, &req.base_url, req.auth_ref.as_deref()).await?;
    Ok(Json(repos))
}

/// 离线 blob store 预览请求体。
#[derive(Debug, Deserialize)]
pub struct NexusOfflinePreviewRequest {
    /// 本地 Nexus 文件型 blob store 根目录路径（服务进程可访问的本地文件系统路径）。
    pub path: String,
}

/// 预览本地 Nexus blob store 可迁移内容（仅管理员，FR-124 异步化）。
///
/// 仅做发现 / 预览（解析磁盘布局、按 repo 枚举可迁移 blob 及基本元数据），不读取也不搬运 blob 本体。
/// 上万 blob 的同步遍历会在前置反代后超时（504），故复用 FR-83 异步 job 基建：立即返回 `job_id`（202），
/// 后台执行枚举，结果经 `GET /migrate/jobs/{id}` 进度的 `offline_preview` 字段轮询取回。
pub async fn preview_nexus_offline(
    State(state): State<AppState>,
    Extension(jobs): Extension<Arc<MigrationJobs>>,
    identity: Identity,
    Json(req): Json<NexusOfflinePreviewRequest>,
) -> Result<(StatusCode, Json<JobCreatedDto>), ApiError> {
    identity.require_admin()?;

    let path = req.path.trim().to_string();
    if path.is_empty() {
        return Err(ApiError::BadRequest("blob store 路径不能为空".to_string()));
    }
    // 廉价同步预校验：路径必须是存在的目录——对明显错误的路径即时 400，无需起后台任务
    //（一次 stat 开销可忽略；真正高开销的 content/ 遍历 + 读上万 .properties 才放后台异步）
    if !std::path::Path::new(&path).is_dir() {
        return Err(ApiError::BadRequest(format!(
            "blob store 路径不存在或不是目录: {path}"
        )));
    }

    // 登记进度共享态与控制句柄（预览为单次枚举、无逐 asset 边界，控制句柄仅为统一注册表形态、不参与取消），
    // 起后台任务执行枚举，端点立即返回 job_id（202）
    let job_id = Uuid::new_v4().to_string();
    let progress = Arc::new(Mutex::new(OnlinePullProgress::default()));
    let control = Arc::new(JobControl::default());
    jobs.register(job_id.clone(), progress.clone(), control);
    // 登记统一任务（FR-131）：预览为枚举、不参与迁移单飞门（可与搬运并行），故用普通 register
    state.tasks.register_with_id(
        job_id.clone(),
        TaskKind::Migration,
        Some("离线预览".to_string()),
    );

    let tasks = state.tasks.clone();
    let task_job_id = job_id.clone();
    tokio::spawn(async move {
        // 离线枚举是同步阻塞文件 IO（遍历目录 + 读 .properties），放阻塞线程池，不占异步工作线程
        let result = tokio::task::spawn_blocking(move || {
            migrate::enumerate_blob_store(std::path::Path::new(&path))
        })
        .await;

        let final_phase = {
            let mut p = progress.lock().unwrap_or_else(|e| e.into_inner());
            match result {
                Ok(Ok(repos)) => {
                    // total_assets 记枚举到的 blob 总数，供 UI 展示规模
                    p.total_assets = repos.iter().map(|r| r.blob_count).sum();
                    p.offline_preview = Some(repos);
                    p.phase = OnlinePullPhase::Done;
                }
                Ok(Err(e)) => {
                    p.phase = OnlinePullPhase::Failed;
                    p.error = Some(e.to_string());
                }
                Err(e) => {
                    p.phase = OnlinePullPhase::Failed;
                    p.error = Some(format!("离线枚举任务异常: {e}"));
                }
            }
            p.phase
        };
        sync_migration_task_state(&tasks, &task_job_id, final_phase);
        tracing::info!(任务 = %task_job_id, "离线 blob store 预览枚举任务结束");
    });

    Ok((StatusCode::ACCEPTED, Json(JobCreatedDto { job_id })))
}

/// proxy 仓库配置 + 缓存制品搬运请求体（FR-38）。
#[derive(Debug, Deserialize)]
pub struct NexusProxyMigrateRequest {
    /// 源 Nexus 基址：经其 REST API 枚举 proxy 仓库配置（格式 / 上游地址）。
    pub base_url: String,
    /// 上游凭据引用（仅引用，真值走 env，不入库）；匿名可访问的源系统可省略。
    #[serde(default)]
    pub auth_ref: Option<String>,
    /// 源离线 blob store 根目录路径：提供已缓存 proxy 制品本体（其下应含 `content/` 子目录）。
    pub offline_path: String,
}

/// 执行 Nexus proxy 仓库配置创建 + 缓存制品搬运（仅管理员，FR-38）。
///
/// 经在线 REST 枚举源 proxy 仓库配置 → 在本系统建仓 → 从离线 blob store 搬运其缓存制品本体
/// （blob 先落盘校验再写索引，失败回滚不留孤儿；单制品失败不中断整批、可重入）。
/// 不搬运 hosted 仓库制品（FR-39 未实现）。
pub async fn migrate_nexus_proxy(
    State(state): State<AppState>,
    Extension(jobs): Extension<Arc<MigrationJobs>>,
    identity: Identity,
    Json(req): Json<NexusProxyMigrateRequest>,
) -> Result<(StatusCode, Json<JobCreatedDto>), ApiError> {
    identity.require_admin()?;

    let offline_path = req.offline_path.trim().to_string();
    if offline_path.is_empty() {
        return Err(ApiError::BadRequest("offline_path 不能为空".to_string()));
    }
    // 迁移单飞早拒（FR-131）：昂贵同步枚举前先快查在途迁移，已有即 409、不白跑上游
    if state.tasks.migration_in_flight() {
        return Err(ApiError::Conflict("已有迁移任务在途".to_string()));
    }

    // 同步阶段（失败即 400/502、不开任务）：在线枚举源 proxy 仓库配置（格式 / 上游地址）
    // 持随 AppState 共享的出站网络热替换槽，出站时取当前 client（含运行时 PATCH 后的新代理）
    let client = HttpNexusClient::with_network_state(state.settings.network.clone());
    let source_repos =
        migrate::discover_repositories(&client, &req.base_url, req.auth_ref.as_deref()).await?;

    // 迁移单飞（FR-131）：原子「检查无在途迁移 → 登记统一任务」，已有在途即 409。
    let job_id = Uuid::new_v4().to_string();
    if !state
        .tasks
        .try_begin_migration(job_id.clone(), Some("离线 proxy 搬运".to_string()))
    {
        return Err(ApiError::Conflict("已有迁移任务在途".to_string()));
    }
    // 登记任务进度 + 控制句柄，起后台任务执行搬运（大库不阻塞请求、不在反代后 504，FR-125），立即返回 job_id（202）
    let progress = Arc::new(Mutex::new(OnlinePullProgress::default()));
    let control = Arc::new(JobControl::default());
    jobs.register(job_id.clone(), progress.clone(), control.clone());

    let meta = state.meta.clone();
    let artifacts = state.artifacts.clone();
    let formats = state.formats.clone();
    let tasks = state.tasks.clone();
    let task_job_id = job_id.clone();
    tokio::spawn(async move {
        let result = migrate::migrate_proxy_repositories_with_progress(
            &meta,
            &artifacts,
            &formats,
            &source_repos,
            std::path::Path::new(&offline_path),
            &progress,
            &control,
        )
        .await;
        let final_phase = {
            let mut p = progress.lock().unwrap_or_else(|e| e.into_inner());
            p.current_path = None;
            match result {
                // 被取消时 with_progress 已标 Cancelled 终态，不覆盖为 Done（FR-91）
                Ok(_) if p.phase == OnlinePullPhase::Cancelled => {}
                Ok(_) => {
                    p.phase = OnlinePullPhase::Done;
                    p.current_repo = None;
                }
                Err(e) => {
                    p.phase = OnlinePullPhase::Failed;
                    p.error = Some(e.to_string());
                }
            }
            p.phase
        };
        sync_migration_task_state(&tasks, &task_job_id, final_phase);
        tracing::info!(任务 = %task_job_id, "离线 proxy 仓库搬运后台任务结束");
    });

    Ok((StatusCode::ACCEPTED, Json(JobCreatedDto { job_id })))
}

/// hosted 仓库配置 + 完整制品搬运请求体（FR-39）。
#[derive(Debug, Deserialize)]
pub struct NexusHostedMigrateRequest {
    /// 源 Nexus 基址：经其 REST API 枚举 hosted 仓库配置（格式 / 可见性）。
    pub base_url: String,
    /// 上游凭据引用（仅引用，真值走 env，不入库）；匿名可访问的源系统可省略。
    #[serde(default)]
    pub auth_ref: Option<String>,
    /// 源离线 blob store 根目录路径：提供 hosted 仓库制品本体（其下应含 `content/` 子目录）。
    pub offline_path: String,
}

/// 执行 Nexus hosted 仓库配置创建 + 完整制品搬运（仅管理员，FR-39）。
///
/// 经在线 REST 枚举源 hosted 仓库配置 → 在本系统建 hosted 仓库 → 从离线 blob store 搬运其全部
/// 制品本体（blob 先落盘校验再写 `cached = false` 索引，失败回滚不留孤儿；单制品失败 / 不可覆盖
/// 不中断整批、可重入）。超过 `limits.max_artifact_size` 的制品按跳过处理（不写半截 blob）。
/// 不搬运 proxy 仓库制品（proxy 走 FR-38 端点）。
pub async fn migrate_nexus_hosted(
    State(state): State<AppState>,
    Extension(jobs): Extension<Arc<MigrationJobs>>,
    identity: Identity,
    Json(req): Json<NexusHostedMigrateRequest>,
) -> Result<(StatusCode, Json<JobCreatedDto>), ApiError> {
    identity.require_admin()?;

    let offline_path = req.offline_path.trim().to_string();
    if offline_path.is_empty() {
        return Err(ApiError::BadRequest("offline_path 不能为空".to_string()));
    }
    // 迁移单飞早拒（FR-131）：昂贵同步枚举前先快查在途迁移，已有即 409、不白跑上游
    if state.tasks.migration_in_flight() {
        return Err(ApiError::Conflict("已有迁移任务在途".to_string()));
    }

    // 同步阶段（失败即 400/502、不开任务）：在线枚举源 hosted 仓库配置（格式 / 可见性）
    // 持随 AppState 共享的出站网络热替换槽，出站时取当前 client（含运行时 PATCH 后的新代理）
    let client = HttpNexusClient::with_network_state(state.settings.network.clone());
    let source_repos =
        migrate::discover_repositories(&client, &req.base_url, req.auth_ref.as_deref()).await?;

    // 登记任务进度 + 控制句柄，起后台任务执行搬运（大库不阻塞请求、不在反代后 504，FR-125），立即返回 job_id（202）
    // 迁移单飞（FR-131）：原子「检查无在途迁移 → 登记统一任务」，已有在途即 409。
    let job_id = Uuid::new_v4().to_string();
    if !state
        .tasks
        .try_begin_migration(job_id.clone(), Some("离线 hosted 搬运".to_string()))
    {
        return Err(ApiError::Conflict("已有迁移任务在途".to_string()));
    }
    let progress = Arc::new(Mutex::new(OnlinePullProgress::default()));
    let control = Arc::new(JobControl::default());
    jobs.register(job_id.clone(), progress.clone(), control.clone());

    let meta = state.meta.clone();
    let artifacts = state.artifacts.clone();
    let formats = state.formats.clone();
    let max_size = state.config.limits.max_artifact_size;
    let tasks = state.tasks.clone();
    let task_job_id = job_id.clone();
    tokio::spawn(async move {
        let result = migrate::migrate_hosted_repositories_with_progress(
            &meta,
            &artifacts,
            &formats,
            &source_repos,
            std::path::Path::new(&offline_path),
            max_size,
            &progress,
            &control,
        )
        .await;
        let final_phase = {
            let mut p = progress.lock().unwrap_or_else(|e| e.into_inner());
            p.current_path = None;
            match result {
                Ok(_) if p.phase == OnlinePullPhase::Cancelled => {}
                Ok(_) => {
                    p.phase = OnlinePullPhase::Done;
                    p.current_repo = None;
                }
                Err(e) => {
                    p.phase = OnlinePullPhase::Failed;
                    p.error = Some(e.to_string());
                }
            }
            p.phase
        };
        sync_migration_task_state(&tasks, &task_job_id, final_phase);
        tracing::info!(任务 = %task_job_id, "离线 hosted 仓库搬运后台任务结束");
    });

    Ok((StatusCode::ACCEPTED, Json(JobCreatedDto { job_id })))
}

/// 在线拉取迁移请求体（FR-82）：选中的源仓库 + 目标仓库名（无需离线 blob store）。
#[derive(Debug, Deserialize)]
pub struct NexusOnlineMigrateRequest {
    /// 源 Nexus 基址：经其 REST API 枚举仓库配置与制品。
    pub base_url: String,
    /// 上游凭据引用（仅引用，真值走 env，不入库）；匿名可访问的源系统可省略。
    #[serde(default)]
    pub auth_ref: Option<String>,
    /// 选中的源仓库及目标仓库名（`target` 省略 / 空则与源同名）。
    pub repositories: Vec<OnlineRepoSelectionDto>,
}

/// 在线拉取的单个仓库选择项。
#[derive(Debug, Deserialize)]
pub struct OnlineRepoSelectionDto {
    /// 源 Nexus 仓库名。
    pub source: String,
    /// 本系统目标仓库名（省略 / 空则与源同名，允许改名）。
    #[serde(default)]
    pub target: Option<String>,
}

/// 在线拉取迁移触发响应（FR-83）：返回 `job_id`，搬运在后台异步执行。
#[derive(Debug, Serialize)]
pub struct JobCreatedDto {
    /// 任务 id，供轮询 `GET /migrate/jobs/{id}`。
    pub job_id: String,
}

/// 单任务进度响应（FR-83）：`job_id` + 进度快照（展平）。
#[derive(Debug, Serialize)]
pub struct JobProgressDto {
    /// 任务 id。
    pub job_id: String,
    /// 进度快照。
    #[serde(flatten)]
    pub progress: OnlinePullProgress,
}

/// 任务列表项（FR-83）：供客户端重连找回活动 / 近期任务。
#[derive(Debug, Serialize)]
pub struct JobSummaryDto {
    /// 任务 id。
    pub job_id: String,
    /// 当前阶段。
    pub phase: OnlinePullPhase,
    /// 总 asset 数。
    pub total_assets: usize,
    /// 已处理 asset 数。
    pub done_assets: usize,
    /// 成功搬运数。
    pub migrated: usize,
    /// 跳过数。
    pub skipped: usize,
    /// 当前处理的源仓库。
    pub current_repo: Option<String>,
    /// 是否处于暂停态（FR-91）。
    pub paused: bool,
}

/// 触发 Nexus 在线拉取迁移（仅管理员，FR-82 + FR-83）。
///
/// 同步阶段：枚举源仓库配置、匹配所选仓库、解析凭据（失败即 400 / 502，不开任务）；随后**立即返回
/// `job_id`（202）**，实际 asset 枚举 + HTTP 流式下载 + 落地在后台 tokio 任务执行，进度经任务注册表
/// 轮询（`GET /migrate/jobs/{id}`）。仅 Maven hosted 参与，其余整体跳过。凭据真值走 env，绝不入库 / 不进日志。
pub async fn migrate_nexus_online(
    State(state): State<AppState>,
    Extension(jobs): Extension<Arc<MigrationJobs>>,
    identity: Identity,
    Json(req): Json<NexusOnlineMigrateRequest>,
) -> Result<(StatusCode, Json<JobCreatedDto>), ApiError> {
    identity.require_admin()?;
    if req.repositories.is_empty() {
        return Err(ApiError::BadRequest("未选择要迁移的仓库".to_string()));
    }
    // 迁移单飞早拒（FR-131）：昂贵同步枚举前先快查在途迁移，已有即 409、不白跑上游
    if state.tasks.migration_in_flight() {
        return Err(ApiError::Conflict("已有迁移任务在途".to_string()));
    }

    // 持随 AppState 共享的出站网络热替换槽，出站时取当前 client（含运行时 PATCH 后的新代理）
    let client = HttpNexusClient::with_network_state(state.settings.network.clone());

    // 解析凭据（用于 components 枚举与 asset 下载）；匿名源可省略
    let credential = match req.auth_ref.as_deref() {
        Some(r) if !r.is_empty() => Some(migrate::resolve_credential(r)?),
        _ => None,
    };

    // 同步枚举源仓库列表并匹配所选 source（含格式 / 类型）；失败即同步报错，不开任务
    let source_repos =
        migrate::discover_repositories(&client, &req.base_url, req.auth_ref.as_deref()).await?;

    let mut selections = Vec::with_capacity(req.repositories.len());
    for r in &req.repositories {
        let source = r.source.trim();
        let Some(summary) = source_repos.iter().find(|s| s.name == source) else {
            return Err(ApiError::BadRequest(format!("源仓库不存在: {source}")));
        };
        let target = r
            .target
            .as_deref()
            .map(str::trim)
            .filter(|t| !t.is_empty())
            .unwrap_or(source)
            .to_string();
        selections.push(migrate::OnlinePullSelection {
            source: summary.clone(),
            target_repo: target,
        });
    }

    // 迁移单飞（FR-131）：原子「检查无在途迁移 → 登记统一任务」，已有在途即 409。
    let job_id = Uuid::new_v4().to_string();
    if !state
        .tasks
        .try_begin_migration(job_id.clone(), Some("在线拉取迁移".to_string()))
    {
        return Err(ApiError::Conflict("已有迁移任务在途".to_string()));
    }
    // 登记任务进度共享态与控制句柄，起后台任务执行枚举 + 下载，端点立即返回 job_id（202）
    let progress = Arc::new(Mutex::new(OnlinePullProgress::default()));
    let control = Arc::new(JobControl::default());
    jobs.register(job_id.clone(), progress.clone(), control.clone());

    let meta = state.meta.clone();
    let artifacts = state.artifacts.clone();
    let formats = state.formats.clone();
    let max_size = state.config.limits.max_artifact_size;
    let base_url = req.base_url.clone();
    let tasks = state.tasks.clone();
    let task_job_id = job_id.clone();
    tokio::spawn(async move {
        let result = migrate::migrate_online_with_progress(
            &client,
            &meta,
            &artifacts,
            &formats,
            &base_url,
            credential.as_ref(),
            &selections,
            max_size,
            &progress,
            &control,
        )
        .await;
        let final_phase = {
            let mut p = progress.lock().unwrap_or_else(|e| e.into_inner());
            p.current_path = None;
            match result {
                // 被取消时 migrate_online_with_progress 已把进度标为 Cancelled 终态，
                // 此处不得覆盖为 Done（取消不算成功完成，FR-91）
                Ok(_) if p.phase == OnlinePullPhase::Cancelled => {}
                Ok(_) => {
                    p.phase = OnlinePullPhase::Done;
                    p.current_repo = None;
                }
                Err(e) => {
                    p.phase = OnlinePullPhase::Failed;
                    p.error = Some(e.to_string());
                }
            }
            p.phase
        };
        // 据 kind 专表终态同步统一注册表（FR-131），释放迁移单飞门
        sync_migration_task_state(&tasks, &task_job_id, final_phase);
        tracing::info!(任务 = %task_job_id, "在线拉取后台任务结束");
    });

    Ok((StatusCode::ACCEPTED, Json(JobCreatedDto { job_id })))
}

/// 把迁移 kind 专表的最终阶段映射并同步到统一任务注册表（FR-131）。
///
/// 终态收敛迁移单飞门：`done → succeeded` / `failed → failed`（带错误） / `cancelled → cancelled`；
/// 其余非终态阶段不更新（理论上后台任务结束时阶段已是终态之一）。
fn sync_migration_task_state(
    tasks: &Arc<super::TaskRegistry>,
    job_id: &str,
    phase: OnlinePullPhase,
) {
    match phase {
        OnlinePullPhase::Done => tasks.set_state(job_id, TaskState::Succeeded, None),
        OnlinePullPhase::Cancelled => tasks.set_state(job_id, TaskState::Cancelled, None),
        OnlinePullPhase::Failed => {
            tasks.set_state(job_id, TaskState::Failed, Some("迁移失败".to_string()))
        }
        // 非终态：后台任务正常结束不会停在此（防御性不更新，避免误把在途置终态）
        OnlinePullPhase::Enumerating | OnlinePullPhase::Downloading | OnlinePullPhase::Paused => {}
    }
}

/// 查询某在线拉取任务的进度（仅管理员，FR-83）。未知 id 返回 404。
pub async fn migrate_nexus_job(
    Extension(jobs): Extension<Arc<MigrationJobs>>,
    identity: Identity,
    Path(job_id): Path<String>,
) -> Result<Json<JobProgressDto>, ApiError> {
    identity.require_admin()?;
    let progress = jobs.get(&job_id).ok_or(ApiError::NotFound)?;
    let snap = progress.lock().unwrap_or_else(|e| e.into_inner()).clone();
    Ok(Json(JobProgressDto {
        job_id,
        progress: snap,
    }))
}

/// 列出活动 / 近期在线拉取任务（仅管理员，FR-83），供客户端重连找回。
pub async fn migrate_nexus_jobs(
    Extension(jobs): Extension<Arc<MigrationJobs>>,
    identity: Identity,
) -> Result<Json<Vec<JobSummaryDto>>, ApiError> {
    identity.require_admin()?;
    let list = jobs
        .list()
        .into_iter()
        .map(|(job_id, p)| JobSummaryDto {
            job_id,
            phase: p.phase,
            total_assets: p.total_assets,
            done_assets: p.done_assets,
            migrated: p.migrated,
            skipped: p.skipped,
            current_repo: p.current_repo,
            paused: p.paused,
        })
        .collect();
    Ok(Json(list))
}

/// 取某在线拉取任务的控制句柄；未知 id 返回 404（含已被注册表淘汰的旧任务，FR-91）。
fn job_control(jobs: &Arc<MigrationJobs>, job_id: &str) -> Result<Arc<JobControl>, ApiError> {
    jobs.control(job_id).ok_or(ApiError::NotFound)
}

/// 取消某在线拉取任务（仅管理员，FR-91）。
///
/// 置取消信号，后台循环在下一 asset 边界停止后续搬运、任务标 `cancelled`（不算失败，已搬运保留）。
/// 未知 id 返回 404；对已结束任务为幂等空操作、返回 200（`request_cancel` 自身吞掉）。
pub async fn migrate_nexus_job_cancel(
    Extension(jobs): Extension<Arc<MigrationJobs>>,
    identity: Identity,
    Path(job_id): Path<String>,
) -> Result<StatusCode, ApiError> {
    identity.require_admin()?;
    let control = job_control(&jobs, &job_id)?;
    control.request_cancel();
    tracing::info!(任务 = %job_id, "已请求取消在线拉取任务");
    Ok(StatusCode::OK)
}

/// 暂停某在线拉取任务（仅管理员，FR-91）。
///
/// 置暂停信号，后台循环在下一 asset 边界挂起、不再推进。未知 id 返回 404；
/// 对已取消 / 已结束任务为幂等空操作、返回 200。
pub async fn migrate_nexus_job_pause(
    Extension(jobs): Extension<Arc<MigrationJobs>>,
    identity: Identity,
    Path(job_id): Path<String>,
) -> Result<StatusCode, ApiError> {
    identity.require_admin()?;
    let control = job_control(&jobs, &job_id)?;
    control.request_pause();
    tracing::info!(任务 = %job_id, "已请求暂停在线拉取任务");
    Ok(StatusCode::OK)
}

/// 继续某已暂停的在线拉取任务（仅管理员，FR-91）。
///
/// 清暂停信号并唤醒挂起的后台循环恢复搬运。未知 id 返回 404；
/// 对未暂停 / 已结束任务为幂等空操作、返回 200。
pub async fn migrate_nexus_job_resume(
    Extension(jobs): Extension<Arc<MigrationJobs>>,
    identity: Identity,
    Path(job_id): Path<String>,
) -> Result<StatusCode, ApiError> {
    identity.require_admin()?;
    let control = job_control(&jobs, &job_id)?;
    control.request_resume();
    tracing::info!(任务 = %job_id, "已请求继续在线拉取任务");
    Ok(StatusCode::OK)
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

    /// 便捷：带 Bearer 令牌 POST JSON 到某迁移端点。
    async fn 请求(
        state: AppState,
        path: &str,
        令牌: &str,
        body: serde_json::Value,
    ) -> axum::response::Response {
        let app = super::super::build_router(state);
        app.oneshot(
            Request::builder()
                .method("POST")
                .uri(path)
                .header("Authorization", format!("Bearer {令牌}"))
                .header("Content-Type", "application/json")
                .body(Body::from(body.to_string()))
                .unwrap(),
        )
        .await
        .unwrap()
    }

    #[tokio::test]
    async fn 在线迁移_已有在途迁移时第二个_409_且不白跑上游() {
        // 预置一个在途迁移任务占用单飞门，触发在线迁移应在同步枚举前早拒 409
        let (state, _dir) = 测试用状态().await;
        state
            .tasks
            .try_begin_migration("in-flight".to_string(), Some("占位".to_string()));
        let token = 签发令牌(&state, "admin", Role::Admin).await;
        let resp = 请求(
            state,
            "/api/v1/migrate/nexus/online/migrate",
            &token,
            serde_json::json!({ "base_url": "http://127.0.0.1:0", "repositories": [{ "source": "a" }] }),
        )
        .await;
        assert_eq!(resp.status(), StatusCode::CONFLICT);
        let body = 读_json(resp).await;
        assert_eq!(body["error"]["message"], "已有迁移任务在途");
    }

    #[tokio::test]
    async fn 离线proxy搬运_已有在途迁移时第二个_409() {
        let (state, _dir) = 测试用状态().await;
        state
            .tasks
            .try_begin_migration("in-flight".to_string(), None);
        let token = 签发令牌(&state, "admin", Role::Admin).await;
        let resp = 请求(
            state,
            "/api/v1/migrate/nexus/proxy/migrate",
            &token,
            serde_json::json!({ "base_url": "http://127.0.0.1:0", "offline_path": "/tmp/x" }),
        )
        .await;
        assert_eq!(resp.status(), StatusCode::CONFLICT);
    }

    #[tokio::test]
    async fn 在线迁移_无在途时不被单飞门拦() {
        // 无在途迁移时单飞门放行，端点进入同步枚举阶段；本测试只断言「未被 409 早拒」
        //（base_url 指向不可达地址，枚举失败应是 502，而非 409）
        let (state, _dir) = 测试用状态().await;
        let token = 签发令牌(&state, "admin", Role::Admin).await;
        let resp = 请求(
            state,
            "/api/v1/migrate/nexus/online/migrate",
            &token,
            serde_json::json!({ "base_url": "http://127.0.0.1:0", "repositories": [{ "source": "a" }] }),
        )
        .await;
        assert_ne!(
            resp.status(),
            StatusCode::CONFLICT,
            "无在途迁移时不应被单飞门 409 拦截"
        );
    }
}
