//! NuGet v3 协议端点（FR-29）：服务索引、扁平容器版本列表、.nupkg / .nuspec 下载与 `nuget push` 发布。
//!
//! 协议适配在本模块，业务机理下沉到 `format::ArtifactService`（存储 / 校验和 / 事务 / 单飞缓存）
//! 与 `format::NuGetFormat`（服务索引 / 版本列表 / .nuspec 解析等纯函数）。
//! 经既有授权编排门控：发布需 write，读受 visibility / ACL，private 无权一律 404。
//!
//! 存储约定（与 NuGetFormat 一致，均为扁平容器相对键、id / version 小写）：
//! - .nupkg 存于 `{id}/{version}/{id}.{version}.nupkg`
//! - .nuspec 存于 `{id}/{version}/{id}.nuspec`
//! - 版本列表由元数据索引动态生成，不另存聚合文档。

use axum::{
    body::Body,
    extract::{FromRequest, Multipart},
    http::{header, HeaderMap, Request, StatusCode},
    response::Response,
};

use crate::format::{ArtifactKind, NuGetError, NuGetFormat};
use crate::meta::{RepoType, RepositoryRecord};

use super::{ApiError, AppState};

/// 服务索引 / 版本列表回源缓冲上限（16 MiB）：均为小型 JSON 文档，远小于此；超限按上游异常处理。
const DOC_MAX_BYTES: usize = 16 * 1024 * 1024;

/// 发布包（`PUT /{repo}/v3/package`，`nuget push`）：解析 multipart 取 .nupkg → 读 .nuspec 得
/// id/version → 版本不可变预检 409 → 落 .nupkg → 落 .nuspec，失败回滚不留孤儿。
///
/// 次序：① 取 multipart 内 .nupkg 字节；② 解压读 .nuspec 解析 id/version；③ 版本不可变预检
/// （已发布 → 409，不写任何 blob）；④ 落 .nupkg blob（流式校验四摘要）；⑤ 落 .nuspec blob。
pub async fn publish(
    state: &AppState,
    repo: &RepositoryRecord,
    headers: HeaderMap,
    body: Body,
) -> Result<Response, ApiError> {
    let format = nuget_format(state, repo)?;

    // 据请求头与体重建请求交 axum Multipart 提取器解析（按 content-type 的 boundary 切分）
    let mut multipart = parse_multipart(headers, body).await?;

    // ① 从 multipart 取出 .nupkg 字节（nuget push 把包作为单个文件字段提交）
    let nupkg = read_nupkg_field(&mut multipart, state.config.limits.max_artifact_size).await?;

    // ② 解压读取内嵌 .nuspec 并解析出 id / version
    let nuspec = NuGetFormat::read_nuspec_from_nupkg(&nupkg).map_err(nuget_err_to_api)?;
    let identity = NuGetFormat::parse_nuspec(&nuspec).map_err(nuget_err_to_api)?;

    let nupkg_path = NuGetFormat::nupkg_path(&identity.id, &identity.version);
    let nuspec_path = NuGetFormat::nuspec_path(&identity.id, &identity.version);

    // ③ 版本不可变预检：同 id+version 的 .nupkg 已存在 → 409（不写任何 blob）
    let nupkg_coords = format.parse_path(&nupkg_path)?;
    if state
        .meta
        .get_artifact(&repo.id, &nupkg_coords.path)
        .await?
        .is_some()
    {
        return Err(ApiError::Conflict(format!(
            "包 {} 版本 {} 已发布，不可覆盖",
            identity.id, identity.version
        )));
    }

    // ④ 落 .nupkg：经通用机理流式落盘并校验四摘要（blob 先落盘再写索引）
    let max_size = state.config.limits.max_artifact_size;
    state
        .artifacts
        .put_hosted(repo, format, &nupkg_coords, &nupkg[..], max_size)
        .await?;

    // ⑤ 落 .nuspec：从 .nupkg 内提取的清单字节落盘，供 `GET .nuspec` 直接返回
    let nuspec_coords = format.parse_path(&nuspec_path)?;
    state
        .artifacts
        .put_hosted(repo, format, &nuspec_coords, &nuspec[..], max_size)
        .await?;

    tracing::info!(仓库 = %repo.name, 包 = %identity.id, 版本 = %identity.version, "已发布 NuGet 包");
    // nuget push 成功返回 201（NuGet 协议对 PackagePublish 约定 201/202）
    Response::builder()
        .status(StatusCode::CREATED)
        .body(Body::empty())
        .map_err(|_| ApiError::Internal)
}

/// 获取服务索引（`GET /{repo}/v3/index.json`）：hosted 生成、proxy 回源重写指向本仓库。
pub async fn get_service_index(
    state: &AppState,
    repo: &RepositoryRecord,
) -> Result<Response, ApiError> {
    let base = super::artifacts::public_base_url(state);

    if RepoType::from_db_str(&repo.r#type) == RepoType::Proxy {
        // proxy：回源上游服务索引后把各 resource @id 重写为指向本代理（索引易变，不缓存）
        let upstream = state
            .artifacts
            .fetch_upstream_doc(repo, NuGetFormat::SERVICE_INDEX_PATH, DOC_MAX_BYTES)
            .await?;
        let rewritten = NuGetFormat::rewrite_proxy_service_index(&upstream, &base, &repo.name)
            .map_err(nuget_err_to_api)?;
        return json_response(rewritten);
    }

    // hosted：据本仓库地址生成服务索引（PackageBaseAddress + PackagePublish）
    let idx = NuGetFormat::service_index(&base, &repo.name);
    let bytes = serde_json::to_vec(&idx).map_err(|_| ApiError::Internal)?;
    json_response(bytes)
}

/// 获取扁平容器版本列表（`GET /{repo}/v3-flatcontainer/{id}/index.json`）。
///
/// hosted 由元数据索引动态汇总该包所有 .nupkg 版本生成；proxy 回源上游版本列表（透传）。
pub async fn get_versions_index(
    state: &AppState,
    repo: &RepositoryRecord,
    id: &str,
) -> Result<Response, ApiError> {
    if RepoType::from_db_str(&repo.r#type) == RepoType::Proxy {
        // proxy：回源上游版本列表（扁平容器版本列表易变，不缓存）
        let rel = NuGetFormat::versions_index_path(id);
        let upstream = state
            .artifacts
            .fetch_upstream_doc(repo, &rel, DOC_MAX_BYTES)
            .await?;
        return json_response(upstream);
    }

    // hosted：据仓库制品索引动态汇总版本（SQLite 唯一真源，不另存聚合文档）
    let records = state.meta.list_artifacts_by_repo(&repo.id).await?;
    let versions = NuGetFormat::collect_versions(&records, id);
    if versions.is_empty() {
        // 无任何版本即该包不存在
        return Err(ApiError::NotFound);
    }
    let idx = NuGetFormat::versions_index(&versions);
    let bytes = serde_json::to_vec(&idx).map_err(|_| ApiError::Internal)?;
    json_response(bytes)
}

/// 下载扁平容器内的 .nupkg / .nuspec（`GET /{repo}/v3-flatcontainer/{id}/{version}/{file}`）。
///
/// `path` 为仓库内完整路径（含 `v3-flatcontainer/` 前缀），即制品存储键，亦为 proxy 回源 rel_path。
/// 经通用机理流式返回；proxy cache-miss 回源缓存、命中不回源。
pub async fn get_flat_artifact(
    state: &AppState,
    repo: &RepositoryRecord,
    path: &str,
) -> Result<Response, ApiError> {
    let format = nuget_format(state, repo)?;
    // 扁平容器键统一小写（NuGet 约定 id / version 小写），保证大小写不一致请求命中同一制品
    let coords = format.parse_path(&path.to_ascii_lowercase())?;

    let (handle, kind) = state.artifacts.get(repo, format, &coords).await?;
    if kind == ArtifactKind::FetchedFromUpstream {
        tracing::debug!(仓库 = %repo.name, 路径 = %coords.path, "proxy 回源 NuGet 制品");
    }
    let content_type = handle
        .record
        .content_type
        .clone()
        .unwrap_or_else(|| "application/octet-stream".to_string());
    let body = Body::from_stream(tokio_util::io::ReaderStream::new(handle.blob));
    Response::builder()
        .status(StatusCode::OK)
        .header(header::CONTENT_TYPE, content_type)
        .header(header::CONTENT_LENGTH, handle.record.size)
        .header("x-checksum-sha256", handle.record.sha256)
        .body(body)
        .map_err(|_| ApiError::Internal)
}

/// 取 NuGet 格式处理器；非 nuget 格式视为内部路由错误（调用前应已据格式分派）。
fn nuget_format<'a>(
    state: &'a AppState,
    repo: &RepositoryRecord,
) -> Result<&'a dyn crate::format::Format, ApiError> {
    state
        .formats
        .get(&repo.format)
        .ok_or(ApiError::BadRequest("仓库格式未实现".to_string()))
}

/// 据请求头与体构造 axum Multipart 提取器。
///
/// catch-all 路由已先取走 `Body`，无法在 handler 签名上直接用 Multipart 提取器；这里据原始
/// content-type 头与体重建一个最小请求交 `Multipart::from_request` 解析，避免手写 multipart 拆包。
/// content-type 缺失 / 非 multipart 时提取器自身返回 400。
async fn parse_multipart(headers: HeaderMap, body: Body) -> Result<Multipart, ApiError> {
    let mut req = Request::new(body);
    *req.headers_mut() = headers;
    Multipart::from_request(req, &())
        .await
        .map_err(|_| ApiError::BadRequest("请求体不是有效的 multipart/form-data".to_string()))
}

/// 从 multipart 表单中取出第一个文件字段的字节（`nuget push` 仅含一个 .nupkg 文件）。
///
/// 按上传上限约束累计字节，超限返回 413；无任何字段返回 400。.nupkg 须整体读入以解压读
/// 内嵌 .nuspec（落 blob 仍走流式机理，从内存字节读取）。
async fn read_nupkg_field(
    multipart: &mut Multipart,
    max: Option<u64>,
) -> Result<Vec<u8>, ApiError> {
    let limit = max.map(|m| m as usize).unwrap_or(usize::MAX);
    // 取第一个字段（nuget push 把包作为单一文件字段提交）；无字段则 400
    let field = multipart
        .next_field()
        .await
        .map_err(|_| ApiError::BadRequest("multipart 解析失败".to_string()))?
        .ok_or_else(|| ApiError::BadRequest("multipart 中未找到上传包".to_string()))?;
    let bytes = field
        .bytes()
        .await
        .map_err(|_| ApiError::BadRequest("读取 multipart 字段失败".to_string()))?;
    if bytes.len() > limit {
        return Err(ApiError::PayloadTooLarge);
    }
    Ok(bytes.to_vec())
}

/// 把 JSON 字节封装为 200 响应。
fn json_response(bytes: Vec<u8>) -> Result<Response, ApiError> {
    let len = bytes.len();
    Response::builder()
        .status(StatusCode::OK)
        .header(header::CONTENT_TYPE, "application/json")
        .header(header::CONTENT_LENGTH, len)
        .body(Body::from(bytes))
        .map_err(|_| ApiError::Internal)
}

/// 把 NuGet 协议错误映射为 HTTP 错误。
fn nuget_err_to_api(e: NuGetError) -> ApiError {
    match e {
        NuGetError::VersionExists(id, ver) => {
            ApiError::Conflict(format!("包 {id} 版本 {ver} 已发布，不可覆盖"))
        }
        NuGetError::InvalidPackage(msg) => ApiError::BadRequest(msg),
    }
}
