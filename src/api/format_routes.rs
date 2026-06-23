//! 格式 API 路由（FR-11/12/17）：按各格式原生协议暴露制品直传 / 下载 / 删除。
//!
//! 本批实现 Raw：`PUT/GET/DELETE /{repo}/{path..}`，路径中含仓库名以定位仓库。
//! 经既有授权编排门控——写需 write、读受 public/private + 读 ACL、private 对无权一律 404。
//! handler 保持薄：流式 IO 与制品机理下沉到 `format::ArtifactService`，判定在 `authz`。

use axum::{
    body::Body,
    extract::{Path, State},
    http::{header, StatusCode},
    response::{IntoResponse, Response},
};
use futures_util::TryStreamExt;
use tokio_util::io::{ReaderStream, StreamReader};

use crate::format::ArtifactKind;
use crate::meta::RepositoryRecord;

use super::repo_access::{build_repo_view, load_readable_repo};
use super::{ApiError, AppState, Identity};
use crate::authz::{authorize, Action, Decision};

/// 默认内容类型：格式无法推断且制品未记录时回退。
const DEFAULT_CONTENT_TYPE: &str = "application/octet-stream";

/// npm 格式名：据此把 npm 仓库的请求分派到其原生协议处理。
const NPM_FORMAT: &str = "npm";

/// npm tarball 在包内的目录分隔段（npm 协议固定为 `-`）。
const NPM_TARBALL_SEGMENT: &str = "/-/";

/// 分派 npm 读请求：含 `/-/` 段者为 tarball 下载，否则为 packument 获取。
///
/// 仅做协议分派，业务在 `npm_routes`；不在此写 npm 业务逻辑。
async fn get_npm(
    state: &AppState,
    repo: &RepositoryRecord,
    path: &str,
) -> Result<Response, ApiError> {
    match path.split_once(NPM_TARBALL_SEGMENT) {
        // tarball：`{包名}/-/{文件}`
        Some((package, tarball_name)) => {
            super::npm_routes::get_tarball(state, repo, package, tarball_name).await
        }
        // packument：`{包名}`
        None => super::npm_routes::get_packument(state, repo, path).await,
    }
}

/// 上传制品（PUT）：写授权后流式落 blob 并写索引，按格式覆盖策略处理重复。
///
/// 流式：请求体经 body stream → AsyncRead 喂给制品机理，大文件不整体载入内存；
/// 超 `limits.max_artifact_size` 在写入途中即拒，返回 413 且不留半截 blob。
pub async fn put_artifact(
    State(state): State<AppState>,
    identity: Identity,
    Path((repo_name, path)): Path<(String, String)>,
    body: Body,
) -> Result<Response, ApiError> {
    let repo = resolve_writable_repo(&state, &identity, &repo_name).await?;
    // npm 发布走其原生协议（请求体为含 base64 tarball 的 JSON，须整体解析）
    if repo.format == NPM_FORMAT {
        return super::npm_routes::publish(&state, &repo, body).await;
    }
    let format = state
        .formats
        .get(&repo.format)
        .ok_or(ApiError::BadRequest("仓库格式未实现".to_string()))?;
    let coords = format.parse_path(&path)?;

    // 请求体字节流 → AsyncRead（流式，不整体载入内存）
    let stream = body
        .into_data_stream()
        .map_err(|e| std::io::Error::other(e.to_string()));
    let reader = StreamReader::new(stream);

    let max_size = state.config.limits.max_artifact_size;
    let outcome = state
        .artifacts
        .put_hosted(&repo, format, &coords, reader, max_size)
        .await?;

    // 覆盖返回 200，新建返回 201（贴近 Raw / HTTP 习惯）
    let status = if outcome.overwritten {
        StatusCode::OK
    } else {
        StatusCode::CREATED
    };
    Ok(status.into_response())
}

/// 下载制品（GET）：读授权后流式返回 blob；hosted 命中本地，proxy cache-miss 回源后返回。
pub async fn get_artifact(
    State(state): State<AppState>,
    identity: Identity,
    Path((repo_name, path)): Path<(String, String)>,
) -> Result<Response, ApiError> {
    // 读授权（无权 private → 404 隐藏存在性）
    let repo = load_readable_repo_by_name(&state, &identity, &repo_name).await?;
    // npm 读走其原生协议：tarball（含 `/-/` 段）按 blob 返回，否则按 packument 文档返回
    if repo.format == NPM_FORMAT {
        return get_npm(&state, &repo, &path).await;
    }
    let format = state
        .formats
        .get(&repo.format)
        .ok_or(ApiError::BadRequest("仓库格式未实现".to_string()))?;
    let coords = format.parse_path(&path)?;

    let (handle, kind) = state.artifacts.get(&repo, format, &coords).await?;
    if kind == ArtifactKind::FetchedFromUpstream {
        tracing::debug!(仓库 = %repo.name, 路径 = %coords.path, "proxy 回源命中并返回");
    }

    // 流式返回 blob 文件（不整体载入内存）
    let content_type = handle
        .record
        .content_type
        .clone()
        .unwrap_or_else(|| DEFAULT_CONTENT_TYPE.to_string());
    let body = Body::from_stream(ReaderStream::new(handle.blob));
    let response = Response::builder()
        .status(StatusCode::OK)
        .header(header::CONTENT_TYPE, content_type)
        .header(header::CONTENT_LENGTH, handle.record.size)
        // 暴露 sha256 校验头，供下载方校验完整性（FR-69）
        .header("x-checksum-sha256", handle.record.sha256)
        .body(body)
        .map_err(|_| ApiError::Internal)?;
    Ok(response)
}

/// 删除制品（DELETE）：写授权后删除；hosted 删本体 + 索引，proxy 删缓存。
pub async fn delete_artifact(
    State(state): State<AppState>,
    identity: Identity,
    Path((repo_name, path)): Path<(String, String)>,
) -> Result<Response, ApiError> {
    let repo = resolve_writable_repo(&state, &identity, &repo_name).await?;
    let format = state
        .formats
        .get(&repo.format)
        .ok_or(ApiError::BadRequest("仓库格式未实现".to_string()))?;
    let coords = format.parse_path(&path)?;
    state.artifacts.delete(&repo, &coords).await?;
    Ok(StatusCode::NO_CONTENT.into_response())
}

/// 按仓库名解析并施加读授权：仓库不存在 / 无读权限一律 404（隐藏存在性）。
async fn load_readable_repo_by_name(
    state: &AppState,
    identity: &Identity,
    repo_name: &str,
) -> Result<RepositoryRecord, ApiError> {
    let repo = get_repo_by_name(state, repo_name).await?;
    // 复用按 id 的读授权编排（已封装查 ACL + 判定 + 404 定式）
    load_readable_repo(state, identity, &repo.id).await
}

/// 按仓库名解析并施加写授权：无读权限 404、有读无写 403。
async fn resolve_writable_repo(
    state: &AppState,
    identity: &Identity,
    repo_name: &str,
) -> Result<RepositoryRecord, ApiError> {
    let repo = get_repo_by_name(state, repo_name).await?;
    let view = build_repo_view(state, identity, &repo).await?;
    // 先过读判定：无读权限者（含匿名访问 private）一律 404，不泄露仓库存在
    if authorize(&identity.0, &view, Action::Read) == Decision::Deny {
        return Err(ApiError::NotFound);
    }
    match authorize(&identity.0, &view, Action::Write) {
        Decision::Allow => Ok(repo),
        Decision::Deny => Err(ApiError::Forbidden),
    }
}

/// 按仓库名查仓库记录；不存在返回 404。
async fn get_repo_by_name(state: &AppState, repo_name: &str) -> Result<RepositoryRecord, ApiError> {
    state
        .meta
        .get_repository_by_name(repo_name)
        .await?
        .ok_or(ApiError::NotFound)
}
