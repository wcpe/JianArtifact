//! Nexus OSS 迁移入口端点（ADR-0006）：在线 REST 入口（FR-36）+ 离线 blob store 入口（FR-37）。
//!
//! 两端点都只提供迁移的**发现 / 预览**：枚举源系统可迁移内容与基本元数据，
//! 不搬运任何制品（搬运属 FR-38/39，未实现）。仅管理员可调用。
//! handler 保持薄：解析请求、调用 `migrate` 模块编排、做错误映射，不写业务逻辑。

use axum::{extract::State, Json};
use serde::Deserialize;

use crate::migrate::{self, HttpNexusClient, MigrateError, NexusRepoSummary, OfflineRepoSummary};

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
