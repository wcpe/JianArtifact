//! 格式 API 路由（FR-11/12/17）：按各格式原生协议暴露制品直传 / 下载 / 删除。
//!
//! 本批实现 Raw：`PUT/GET/DELETE /{repo}/{path..}`，路径中含仓库名以定位仓库。
//! 经既有授权编排门控——写需 write、读受 public/private + 读 ACL、private 对无权一律 404。
//! handler 保持薄：流式 IO 与制品机理下沉到 `format::ArtifactService`，判定在 `authz`。

use axum::{
    body::Body,
    extract::{Multipart, Path, State},
    http::{header, HeaderMap, StatusCode},
    response::{IntoResponse, Response},
};
use futures_util::TryStreamExt;
use tokio_util::io::{ReaderStream, StreamReader};

use crate::format::{ArtifactKind, PypiFormat, PYPI_SIMPLE_SEGMENT};
use crate::meta::RepositoryRecord;

use super::repo_access::{build_repo_view, load_readable_repo};
use super::{ApiError, AppState, Identity};
use crate::authz::{authorize, Action, Decision};

/// 默认内容类型：格式无法推断且制品未记录时回退。
const DEFAULT_CONTENT_TYPE: &str = "application/octet-stream";

/// npm 格式名：据此把 npm 仓库的请求分派到其原生协议处理。
const NPM_FORMAT: &str = "npm";

/// Go 格式名：据此把 Go 仓库的请求分派到 GOPROXY 协议处理。
const GO_FORMAT: &str = "go";
/// PyPI 格式名：据此把 PyPI 仓库的请求分派到其原生协议处理。
const PYPI_FORMAT: &str = "pypi";

/// npm tarball 在包内的目录分隔段（npm 协议固定为 `-`）。
const NPM_TARBALL_SEGMENT: &str = "/-/";

/// cargo 格式名：据此把 cargo 仓库的请求分派到其稀疏索引协议处理。
const CARGO_FORMAT: &str = "cargo";

/// cargo 发布 API 子路径。
const CARGO_PUBLISH_PATH: &str = "api/v1/crates/new";

/// cargo yank/unyank API 子路径前缀。
const CARGO_API_PREFIX: &str = "api/v1/crates/";

/// NuGet 格式名：据此把 NuGet 仓库的请求分派到其 v3 协议处理。
const NUGET_FORMAT: &str = "nuget";

/// NuGet 发布端点路径（`PUT /{repo}/v3/package`，`nuget push`）。
const NUGET_PUBLISH_PATH: &str = "v3/package";

/// NuGet 服务索引路径（`GET /{repo}/v3/index.json`）。
const NUGET_SERVICE_INDEX_PATH: &str = "v3/index.json";

/// NuGet 扁平容器前缀（版本列表与 .nupkg / .nuspec 下载均在其下）。
const NUGET_FLATCONTAINER_PREFIX: &str = "v3-flatcontainer/";

/// NuGet 版本列表文件名后缀（`v3-flatcontainer/{id}/index.json`）。
const NUGET_VERSIONS_INDEX_SUFFIX: &str = "/index.json";

/// 分派 NuGet GET 请求：服务索引 / 版本列表 / 扁平容器制品（.nupkg / .nuspec）。
///
/// 仅做前缀匹配的协议分派，业务在 `nuget_routes`；不在此写 NuGet 业务逻辑。
async fn get_nuget(
    state: &AppState,
    repo: &RepositoryRecord,
    path: &str,
) -> Result<Response, ApiError> {
    // 服务索引：列出本仓库 v3 资源
    if path == NUGET_SERVICE_INDEX_PATH {
        return super::nuget_routes::get_service_index(state, repo).await;
    }
    // 扁平容器子树：版本列表（`{id}/index.json`）或制品下载（.nupkg / .nuspec）
    if let Some(flat) = path.strip_prefix(NUGET_FLATCONTAINER_PREFIX) {
        // 版本列表：`{id}/index.json`（id 为单段，不含更深目录）
        if let Some(id) = flat.strip_suffix(NUGET_VERSIONS_INDEX_SUFFIX) {
            if !id.is_empty() && !id.contains('/') {
                return super::nuget_routes::get_versions_index(state, repo, id).await;
            }
        }
        // 其余为 .nupkg / .nuspec 下载：以完整路径（含前缀）为存储键
        return super::nuget_routes::get_flat_artifact(state, repo, path).await;
    }
    // 未实现的 v3 资源端点：不存在
    Err(ApiError::NotFound)
}

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

/// 分派 cargo PUT 请求：`api/v1/crates/new` 为发布；`.../{name}/{version}/unyank` 为取消 yank。
///
/// 仅做协议分派，业务在 `cargo_routes`；不在此写 cargo 业务逻辑。
async fn put_cargo(
    state: &AppState,
    repo: &RepositoryRecord,
    path: &str,
    body: Body,
) -> Result<Response, ApiError> {
    if path == CARGO_PUBLISH_PATH {
        return super::cargo_routes::publish(state, repo, body).await;
    }
    // unyank：api/v1/crates/{name}/{version}/unyank
    if let Some((name, version)) = parse_cargo_yank_path(path, "unyank") {
        return super::cargo_routes::set_yanked(state, repo, &name, &version, false).await;
    }
    Err(ApiError::BadRequest(
        "不支持的 cargo 写请求路径".to_string(),
    ))
}

/// 解析 cargo yank/unyank 子路径 `api/v1/crates/{name}/{version}/{action}` → (name, version)。
fn parse_cargo_yank_path(path: &str, action: &str) -> Option<(String, String)> {
    let rest = path.strip_prefix(CARGO_API_PREFIX)?;
    let stripped = rest.strip_suffix(&format!("/{action}"))?;
    let (name, version) = stripped.split_once('/')?;
    if name.is_empty() || version.is_empty() {
        return None;
    }
    Some((name.to_string(), version.to_string()))
}

/// 分派 PyPI 读请求：`simple/...` 为 Simple Repository API 索引，`packages/...` 为包文件下载。
///
/// 仅做协议分派，业务在 `pypi_routes`；不在此写 PyPI 业务逻辑。
async fn get_pypi(
    state: &AppState,
    repo: &RepositoryRecord,
    path: &str,
    headers: &HeaderMap,
) -> Result<Response, ApiError> {
    let simple_prefix = format!("{PYPI_SIMPLE_SEGMENT}/");
    if path == PYPI_SIMPLE_SEGMENT || path == simple_prefix {
        // Simple 根索引：`simple` / `simple/`
        return super::pypi_routes::simple_index(state, repo, headers).await;
    }
    if let Some(project) = PypiFormat::project_of_simple_path(path) {
        // Simple 项目页：`simple/{project}` / `simple/{project}/`
        return super::pypi_routes::simple_project(state, repo, &project, headers).await;
    }
    // 其余按包文件下载（`packages/{规范名}/{文件}`）
    super::pypi_routes::download(state, repo, path).await
}

/// 上传制品（PUT）：写授权后流式落 blob 并写索引，按格式覆盖策略处理重复。
///
/// 流式：请求体经 body stream → AsyncRead 喂给制品机理，大文件不整体载入内存；
/// 超 `limits.max_artifact_size` 在写入途中即拒，返回 413 且不留半截 blob。
pub async fn put_artifact(
    State(state): State<AppState>,
    identity: Identity,
    Path((repo_name, path)): Path<(String, String)>,
    headers: axum::http::HeaderMap,
    body: Body,
) -> Result<Response, ApiError> {
    let repo = resolve_writable_repo(&state, &identity, &repo_name).await?;
    // npm 发布走其原生协议（请求体为含 base64 tarball 的 JSON，须整体解析）
    if repo.format == NPM_FORMAT {
        return super::npm_routes::publish(&state, &repo, body).await;
    }
    // Go 上传走 GOPROXY 约定端点（PUT {module}/@v/{version}.{mod|zip|info}）
    if repo.format == GO_FORMAT {
        return super::go_routes::put(&state, &repo, &path, body).await;
    }
    // cargo 发布 / unyank 走其稀疏索引协议（请求体为二进制 publish 体或 unyank 无体）
    if repo.format == CARGO_FORMAT {
        return put_cargo(&state, &repo, &path, body).await;
    }
    // NuGet 发布走 v3 协议（`PUT /{repo}/v3/package`，multipart/form-data 内含 .nupkg）。
    // nuget / dotnet 客户端会给端点补尾斜杠（`v3/package/`），按去尾斜杠后比较以兼容。
    if repo.format == NUGET_FORMAT && path.trim_end_matches('/') == NUGET_PUBLISH_PATH {
        return super::nuget_routes::publish(&state, &repo, headers, body).await;
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

/// PyPI twine 上传（`POST /{repo}/`）：写授权后解析 multipart 落 wheel/sdist。
///
/// twine 默认上传到仓库根（空路径），故单列此路由；仅 PyPI 格式支持，其余格式 405 语义按 400 返回。
pub async fn post_artifact_root(
    State(state): State<AppState>,
    identity: Identity,
    Path(repo_name): Path<String>,
    multipart: Multipart,
) -> Result<Response, ApiError> {
    post_pypi_upload(&state, &identity, &repo_name, multipart).await
}

/// PyPI twine 上传兜底（`POST /{repo}/{*path}`）：覆盖 twine 的 `legacy/` 等带路径上传形态。
pub async fn post_artifact(
    State(state): State<AppState>,
    identity: Identity,
    Path((repo_name, _path)): Path<(String, String)>,
    multipart: Multipart,
) -> Result<Response, ApiError> {
    post_pypi_upload(&state, &identity, &repo_name, multipart).await
}

/// PyPI 上传共用编排：写授权 → 校验格式为 pypi → 交 `pypi_routes::upload` 落 blob。
async fn post_pypi_upload(
    state: &AppState,
    identity: &Identity,
    repo_name: &str,
    multipart: Multipart,
) -> Result<Response, ApiError> {
    let repo = resolve_writable_repo(state, identity, repo_name).await?;
    if repo.format != PYPI_FORMAT {
        // 仅 PyPI 走 POST 上传协议；其余格式不支持该方法
        return Err(ApiError::BadRequest(
            "该仓库格式不支持 POST 上传".to_string(),
        ));
    }
    super::pypi_routes::upload(state, &repo, multipart).await
}

/// 下载制品（GET）：读授权后流式返回 blob；hosted 命中本地，proxy cache-miss 回源后返回。
pub async fn get_artifact(
    State(state): State<AppState>,
    identity: Identity,
    headers: HeaderMap,
    Path((repo_name, path)): Path<(String, String)>,
) -> Result<Response, ApiError> {
    // 读授权（无权 private → 404 隐藏存在性）
    let repo = load_readable_repo_by_name(&state, &identity, &repo_name).await?;
    // npm 读走其原生协议：tarball（含 `/-/` 段）按 blob 返回，否则按 packument 文档返回
    if repo.format == NPM_FORMAT {
        return get_npm(&state, &repo, &path).await;
    }
    // Go 读走 GOPROXY 协议：据端点分派 list / info / mod / zip / latest
    if repo.format == GO_FORMAT {
        return super::go_routes::get(&state, &repo, &path).await;
    }
    // cargo 读走其稀疏索引协议：config.json / 下载 / 索引文件由 cargo_routes 内部分派
    if repo.format == CARGO_FORMAT {
        return super::cargo_routes::get(&state, &repo, &path).await;
    }
    // PyPI 读走其原生协议：simple/... 为索引，packages/... 为包文件下载
    if repo.format == PYPI_FORMAT {
        return get_pypi(&state, &repo, &path, &headers).await;
    }
    // NuGet 读走 v3 协议：服务索引 / 版本列表 / 扁平容器制品下载
    if repo.format == NUGET_FORMAT {
        return get_nuget(&state, &repo, &path).await;
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
    // cargo 的 DELETE 用于 yank：api/v1/crates/{name}/{version}/yank
    if repo.format == CARGO_FORMAT {
        if let Some((name, version)) = parse_cargo_yank_path(&path, "yank") {
            return super::cargo_routes::set_yanked(&state, &repo, &name, &version, true).await;
        }
        return Err(ApiError::BadRequest(
            "不支持的 cargo 删除请求路径".to_string(),
        ));
    }
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
