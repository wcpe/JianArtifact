//! Nexus OSS 迁移入口端点（ADR-0006）：发现 / 预览 + proxy / hosted 搬运。
//!
//! - 发现 / 预览：在线 REST 入口（FR-36）+ 离线 blob store 入口（FR-37），仅枚举源系统
//!   可迁移内容与基本元数据，不搬运任何制品。
//! - 搬运：proxy 仓库配置 + 缓存制品（FR-38）、hosted 仓库配置 + 完整制品（FR-39）。
//!
//! 所有端点仅管理员可调用。handler 保持薄：解析请求、调用 `migrate` 模块编排、做错误映射，
//! 不写业务逻辑。

use axum::{extract::State, Json};
use serde::Deserialize;

use crate::migrate::{
    self, HostedMigrationReport, HttpNexusClient, MigrateError, NexusRepoSummary,
    OfflineRepoSummary, OnlineMigrationReport, ProxyMigrationReport,
};

use super::{ApiError, AppState, Identity};

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
    let client = HttpNexusClient::new(std::time::Duration::from_secs(
        state.config.proxy.upstream_timeout_secs,
    ))
    .map_err(ApiError::from)?;

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

/// 预览本地 Nexus blob store 可迁移内容（仅管理员）。
///
/// 从给定本地 blob store 目录解析磁盘布局，按 repo 枚举可迁移 blob 及基本元数据后返回；
/// 仅做发现 / 预览，不读取也不搬运 blob 本体。
pub async fn preview_nexus_offline(
    _state: State<AppState>,
    identity: Identity,
    Json(req): Json<NexusOfflinePreviewRequest>,
) -> Result<Json<Vec<OfflineRepoSummary>>, ApiError> {
    identity.require_admin()?;

    let path = req.path.trim().to_string();
    if path.is_empty() {
        return Err(ApiError::BadRequest("blob store 路径不能为空".to_string()));
    }

    // 离线枚举是同步阻塞文件 IO（遍历目录 + 读 .properties），放到阻塞线程池执行，
    // 不阻塞异步运行时工作线程
    let repos = tokio::task::spawn_blocking(move || {
        migrate::enumerate_blob_store(std::path::Path::new(&path))
    })
    .await
    .map_err(|e| {
        tracing::error!(错误 = %e, "离线 blob store 枚举任务异常");
        ApiError::Internal
    })??;

    Ok(Json(repos))
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
    identity: Identity,
    Json(req): Json<NexusProxyMigrateRequest>,
) -> Result<Json<ProxyMigrationReport>, ApiError> {
    identity.require_admin()?;

    let offline_path = req.offline_path.trim().to_string();
    if offline_path.is_empty() {
        return Err(ApiError::BadRequest("offline_path 不能为空".to_string()));
    }

    // ① 在线枚举源 proxy 仓库配置（格式 / 上游地址）
    let client = HttpNexusClient::new(std::time::Duration::from_secs(
        state.config.proxy.upstream_timeout_secs,
    ))
    .map_err(ApiError::from)?;
    let source_repos =
        migrate::discover_repositories(&client, &req.base_url, req.auth_ref.as_deref()).await?;

    // ② 据配置建仓 + 从离线 blob store 搬运缓存制品本体
    let report = migrate::migrate_proxy_repositories(
        &state.meta,
        &state.artifacts,
        &state.formats,
        &source_repos,
        std::path::Path::new(&offline_path),
    )
    .await?;

    Ok(Json(report))
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
    identity: Identity,
    Json(req): Json<NexusHostedMigrateRequest>,
) -> Result<Json<HostedMigrationReport>, ApiError> {
    identity.require_admin()?;

    let offline_path = req.offline_path.trim().to_string();
    if offline_path.is_empty() {
        return Err(ApiError::BadRequest("offline_path 不能为空".to_string()));
    }

    // ① 在线枚举源 hosted 仓库配置（格式 / 可见性）
    let client = HttpNexusClient::new(std::time::Duration::from_secs(
        state.config.proxy.upstream_timeout_secs,
    ))
    .map_err(ApiError::from)?;
    let source_repos =
        migrate::discover_repositories(&client, &req.base_url, req.auth_ref.as_deref()).await?;

    // ② 据配置建 hosted 仓库 + 从离线 blob store 搬运全部制品本体
    let report = migrate::migrate_hosted_repositories(
        &state.meta,
        &state.artifacts,
        &state.formats,
        &source_repos,
        std::path::Path::new(&offline_path),
        state.config.limits.max_artifact_size,
    )
    .await?;

    Ok(Json(report))
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

/// 执行 Nexus 在线拉取迁移（仅管理员，FR-82）。
///
/// 经在线 REST 枚举源仓库配置 → 匹配所选源仓库 → 经 components API 枚举其 asset 并 HTTP 流式
/// 下载、落为本系统 hosted 制品（**无需离线 blob store**）。仅 Maven hosted 参与，其余整体跳过。
/// 凭据真值走 env，绝不入库 / 不进日志。
pub async fn migrate_nexus_online(
    State(state): State<AppState>,
    identity: Identity,
    Json(req): Json<NexusOnlineMigrateRequest>,
) -> Result<Json<OnlineMigrationReport>, ApiError> {
    identity.require_admin()?;
    if req.repositories.is_empty() {
        return Err(ApiError::BadRequest("未选择要迁移的仓库".to_string()));
    }

    let client = HttpNexusClient::new(std::time::Duration::from_secs(
        state.config.proxy.upstream_timeout_secs,
    ))
    .map_err(ApiError::from)?;

    // 解析凭据（用于 components 枚举与 asset 下载）；匿名源可省略
    let credential = match req.auth_ref.as_deref() {
        Some(r) if !r.is_empty() => Some(migrate::resolve_credential(r)?),
        _ => None,
    };

    // 在线枚举源仓库列表，按所选 source 匹配出其摘要（含格式 / 类型，供范围判定）
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

    let report = migrate::migrate_online_repositories(
        &client,
        &state.meta,
        &state.artifacts,
        &state.formats,
        &req.base_url,
        credential.as_ref(),
        &selections,
        state.config.limits.max_artifact_size,
    )
    .await?;

    Ok(Json(report))
}
