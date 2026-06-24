//! 制品详情与删除端点（FR-60/66/68/69）。
//!
//! 详情：返回四校验和 + 所属仓库 / 格式 + 按格式生成的"使用方式"片段（FR-68）。
//! 删除：需对应仓库写权限或管理员；hosted 删本体 + 索引，proxy 删缓存（下次可重新拉取）。
//! handler 保持薄：读 / 写授权编排复用 `repo_access`，制品机理下沉到 `format::ArtifactService`。

use axum::{
    extract::{Path, State},
    Json,
};
use serde::Serialize;
use serde_json::{json, Value};

use crate::format::UsageSnippet;
use crate::meta::ArtifactRecord;
use crate::vuln::{self, VulnHit};

use super::repo_access::{load_readable_repo, load_writable_repo};
use super::{ApiError, AppState, ClientIp, Identity};
use crate::meta::UsageAction;

/// 制品详情视图（字段对齐 docs/API.md 制品详情）。
#[derive(Debug, Serialize)]
pub struct ArtifactDetailDto {
    /// 所属仓库主键。
    pub repo_id: String,
    /// 所属仓库名。
    pub repo_name: String,
    /// 所属仓库格式。
    pub format: String,
    /// 制品路径。
    pub path: String,
    /// 字节大小。
    pub size: i64,
    /// 内容类型。
    pub content_type: Option<String>,
    /// 是否为 proxy 缓存制品。
    pub cached: bool,
    /// 创建时间。
    pub created_at: String,
    /// 四校验和（FR-69）。
    pub checksums: Checksums,
    /// 按格式生成的使用方式片段（FR-68）。
    pub usage: Vec<UsageSnippet>,
    /// 该制品命中的已知漏洞公告（FR-71，本地漏洞库坐标级匹配，不外发）。
    /// 无坐标的格式或未命中时为空数组。
    pub vulnerabilities: Vec<VulnerabilityDto>,
}

/// 制品命中的单条漏洞公告视图（FR-71）。
#[derive(Debug, Serialize)]
pub struct VulnerabilityDto {
    /// 公告 id（如 GHSA / CVE）。
    pub id: String,
    /// 严重度（可空）。
    pub severity: Option<String>,
    /// 简要描述（可空）。
    pub summary: Option<String>,
}

impl From<VulnHit> for VulnerabilityDto {
    fn from(h: VulnHit) -> Self {
        Self {
            id: h.advisory_id,
            severity: h.severity,
            summary: h.summary,
        }
    }
}

/// 四校验和分组。
#[derive(Debug, Serialize)]
pub struct Checksums {
    /// sha256。
    pub sha256: String,
    /// sha1。
    pub sha1: String,
    /// md5。
    pub md5: String,
    /// sha512。
    pub sha512: String,
}

impl From<ArtifactRecord> for Checksums {
    fn from(r: ArtifactRecord) -> Self {
        Self {
            sha256: r.sha256,
            sha1: r.sha1,
            md5: r.md5,
            sha512: r.sha512,
        }
    }
}

/// 制品详情：受读权限约束，无权 private 映射为 404；制品不存在 404。
pub async fn get_artifact_detail(
    State(state): State<AppState>,
    identity: Identity,
    ClientIp(client_ip): ClientIp,
    Path((id, path)): Path<(String, String)>,
) -> Result<Json<ArtifactDetailDto>, ApiError> {
    // 先过读授权（无权 private → 404 隐藏存在性）
    let repo = load_readable_repo(&state, &identity, &id).await?;

    // 取制品索引；不存在 404
    let record = state
        .meta
        .get_artifact(&repo.id, &path)
        .await?
        .ok_or(ApiError::NotFound)?;

    // 使用分析采集（FR-57）：详情查看记一次访问（非阻塞、采集失败不影响业务）。
    // 路径取已落库的制品路径，与下载采集口径一致，便于聚合到同一制品。
    state.usage.record(
        UsageAction::Access,
        &repo.name,
        &record.path,
        identity.actor_name(),
        Some(&client_ip),
    );

    // 据格式注册表多态生成使用片段（未注册格式给空片段，不报错）
    let usage = build_usage(&state, &repo.format, &repo.name, &record.path);

    // 本地漏洞库坐标级匹配（FR-71）：仅查本机已镜像数据，绝不外发坐标
    let vulnerabilities = scan_vulnerabilities(&state, &repo.format, &record.path).await?;

    let detail = ArtifactDetailDto {
        repo_id: repo.id.clone(),
        repo_name: repo.name.clone(),
        format: repo.format.clone(),
        path: record.path.clone(),
        size: record.size,
        content_type: record.content_type.clone(),
        cached: record.cached != 0,
        created_at: record.created_at.clone(),
        checksums: record.into(),
        usage,
        vulnerabilities,
    };
    Ok(Json(detail))
}

/// 据格式坐标在本地漏洞库做坐标级匹配，返回命中的漏洞公告视图（FR-71）。
///
/// 无标准生态坐标的格式（Raw / Docker 等）或无法反解坐标时返回空集——这些制品不参与坐标级匹配。
/// **全程仅查本机已镜像数据，绝不把坐标外发到外部漏洞服务**（守 ADR-0012 数据不外发红线）。
async fn scan_vulnerabilities(
    state: &AppState,
    format: &str,
    path: &str,
) -> Result<Vec<VulnerabilityDto>, ApiError> {
    // 据仓库格式取处理器并反解生态坐标；未注册格式 / 无坐标即不匹配
    let Some(handler) = state.formats.get(format) else {
        return Ok(Vec::new());
    };
    let Ok(coords) = handler.parse_path(path) else {
        return Ok(Vec::new());
    };
    let Some(coord) = handler.vuln_coordinate(&coords) else {
        return Ok(Vec::new());
    };

    // 仅按 (生态, 包) 查本地候选公告，再用纯函数据版本范围筛出命中
    let candidates = state
        .meta
        .list_affected_for_coordinate(&coord.ecosystem, &coord.package)
        .await?;
    let hits = vuln::select_hits(&coord.version, &candidates);
    Ok(hits.into_iter().map(VulnerabilityDto::from).collect())
}

/// 删除制品（FR-60）：需对应仓库写权限或管理员。无读权限 404、有读无写 403、不存在 404。
pub async fn delete_artifact(
    State(state): State<AppState>,
    identity: Identity,
    Path((id, path)): Path<(String, String)>,
) -> Result<Json<Value>, ApiError> {
    // 写授权编排：无读权限 404、有读无写 403
    let repo = load_writable_repo(&state, &identity, &id).await?;

    // 据格式归一化路径（拒绝穿越），再交制品机理删除
    let format = state
        .formats
        .get(&repo.format)
        .ok_or(ApiError::BadRequest("仓库格式未实现".to_string()))?;
    let coords = format.parse_path(&path)?;
    state.artifacts.delete(&repo, &coords).await?;
    Ok(Json(json!({ "status": "ok" })))
}

/// 据格式注册表生成使用方式片段；格式未注册时返回空集（不阻断详情展示）。
fn build_usage(state: &AppState, format: &str, repo_name: &str, path: &str) -> Vec<UsageSnippet> {
    let Some(handler) = state.formats.get(format) else {
        return Vec::new();
    };
    // 路径已是落库的合法路径，再解析一次取坐标；异常时返回空集
    let Ok(coords) = handler.parse_path(path) else {
        return Vec::new();
    };
    let base = public_base_url(state);
    handler.usage_snippets(&base, repo_name, &coords)
}

/// 推断对外基础地址：优先用配置的 public_base_url，否则按监听地址回退。
pub(crate) fn public_base_url(state: &AppState) -> String {
    state
        .config
        .server
        .public_base_url
        .clone()
        .unwrap_or_else(|| {
            format!(
                "http://{}:{}",
                state.config.server.listen_addr, state.config.server.port
            )
        })
}
