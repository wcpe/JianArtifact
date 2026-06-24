//! Go 模块代理协议端点（FR-28）：list / info / mod / zip / latest 的获取，以及 hosted 上传。
//!
//! 协议适配在本模块，业务机理下沉到 `format::ArtifactService`（存储 / 校验和 / 事务 / 单飞缓存）
//! 与 `format::GoFormat`（GOPROXY 路径解析、bang 编解码、聚合文档生成等纯函数）。
//! 经既有授权编排门控：上传需 write，读受 visibility / ACL，private 无权一律 404。
//!
//! 存储约定（与 GoFormat 一致）：`.info` / `.mod` / `.zip` 各存为一条制品记录，路径
//! `{module_bang}/@v/{version}.{ext}`；`@v/list` 与 `@latest` 不存储，hosted 据已存版本聚合、
//! proxy 回源透传。

use axum::{
    body::Body,
    http::{header, StatusCode},
    response::{IntoResponse, Response},
};
use futures_util::TryStreamExt;
use tokio_util::io::{ReaderStream, StreamReader};

use crate::format::{ArtifactKind, GoError, GoFormat, GoRequest, VersionFile};
use crate::meta::{RepoType, RepositoryRecord};

use super::{ApiError, AppState};

/// `@v/list` / `@latest` 等聚合文档回源缓冲上限（4 MiB）：版本索引文本远小于此，超限按上游异常处理。
const AGGREGATE_MAX_BYTES: usize = 4 * 1024 * 1024;

/// 处理 Go 读请求（GET）：据 GOPROXY 端点分派到 list / latest / 单版本文件。
///
/// 仅做协议分派与响应封装，不在此写业务逻辑（聚合 / 回源经下层与纯函数）。
pub async fn get(
    state: &AppState,
    repo: &RepositoryRecord,
    path: &str,
) -> Result<Response, ApiError> {
    let req = GoFormat::parse_request(path).map_err(go_err_to_api)?;
    match req {
        GoRequest::List { module_bang, .. } => get_version_list(state, repo, &module_bang).await,
        GoRequest::Latest { module_bang, .. } => get_latest(state, repo, &module_bang).await,
        GoRequest::Version {
            module_bang,
            version,
            file,
            ..
        } => get_version_file(state, repo, &module_bang, &version, file).await,
    }
}

/// 处理 Go 上传请求（PUT）：仅接受 `.info` / `.mod` / `.zip`，经通用机理流式落盘。
///
/// 不可变预检由 `GoFormat::can_overwrite == false` 触发既有 `OverwriteForbidden`→409。
pub async fn put(
    state: &AppState,
    repo: &RepositoryRecord,
    path: &str,
    body: Body,
) -> Result<Response, ApiError> {
    let req = GoFormat::parse_request(path).map_err(go_err_to_api)?;
    // 仅单版本文件可上传；list / latest 为只读聚合端点
    let (module_bang, version, file) = match req {
        GoRequest::Version {
            module_bang,
            version,
            file,
            ..
        } => (module_bang, version, file),
        GoRequest::List { .. } | GoRequest::Latest { .. } => {
            return Err(ApiError::BadRequest(
                "@v/list 与 @latest 为只读聚合端点，不可上传".to_string(),
            ));
        }
    };

    let format = go_format(state, repo)?;
    let storage_path = GoFormat::version_storage_path(&module_bang, &version, file);
    let coords = format.parse_path(&storage_path)?;

    // 请求体字节流 → AsyncRead（流式，不整体载入内存）
    let stream = body
        .into_data_stream()
        .map_err(|e| std::io::Error::other(e.to_string()));
    let reader = StreamReader::new(stream);

    let outcome = state
        .artifacts
        .put_hosted(
            repo,
            format,
            &coords,
            reader,
            state.config.limits.max_artifact_size,
        )
        .await?;

    tracing::info!(仓库 = %repo.name, 模块 = %module_bang, 版本 = %version, 文件 = file.ext(), "已上传 Go 模块文件");
    // Go 模块不可变，正常路径不会覆盖；新建一律 201
    let _ = outcome;
    Ok(StatusCode::CREATED.into_response())
}

/// 获取版本列表（`GET {module}/@v/list`）：hosted 据已存版本聚合；proxy 回源透传。
async fn get_version_list(
    state: &AppState,
    repo: &RepositoryRecord,
    module_bang: &str,
) -> Result<Response, ApiError> {
    if RepoType::from_db_str(&repo.r#type) == RepoType::Proxy {
        // proxy：每次回源拉取最新版本列表（易变，不缓存）
        let rel = format!("{module_bang}/@v/list");
        let upstream = state
            .artifacts
            .fetch_upstream_doc(repo, &rel, AGGREGATE_MAX_BYTES)
            .await?;
        return text_response(upstream);
    }

    // hosted：列出本模块已存在的版本（据 .mod / .info / .zip 制品聚合，去重）
    let versions = list_hosted_versions(state, repo, module_bang).await?;
    text_response(versions.join("\n").into_bytes())
}

/// 获取最新版本 info（`GET {module}/@latest`）：hosted 取已存最大版本；proxy 回源透传。
async fn get_latest(
    state: &AppState,
    repo: &RepositoryRecord,
    module_bang: &str,
) -> Result<Response, ApiError> {
    if RepoType::from_db_str(&repo.r#type) == RepoType::Proxy {
        let rel = format!("{module_bang}/@latest");
        let upstream = state
            .artifacts
            .fetch_upstream_doc(repo, &rel, AGGREGATE_MAX_BYTES)
            .await?;
        return json_response(upstream);
    }

    // hosted：取已存版本的最大者，返回其 info（已存 .info 直读，否则按 .mod 合成）
    let versions = list_hosted_versions(state, repo, module_bang).await?;
    let latest = GoFormat::latest_version(&versions).ok_or(ApiError::NotFound)?;
    let info = resolve_info(state, repo, module_bang, &latest).await?;
    json_response(info)
}

/// 获取单个版本文件（`.info` / `.mod` / `.zip`）：流式返回；proxy cache-miss 回源。
///
/// `.info` 在 hosted 未显式上传时按对应 `.mod` 制品的 `created_at` 合成（满足客户端字段要求）。
async fn get_version_file(
    state: &AppState,
    repo: &RepositoryRecord,
    module_bang: &str,
    version: &str,
    file: VersionFile,
) -> Result<Response, ApiError> {
    let format = go_format(state, repo)?;
    let storage_path = GoFormat::version_storage_path(module_bang, version, file);
    let coords = format.parse_path(&storage_path)?;

    match state.artifacts.get(repo, format, &coords).await {
        Ok((handle, kind)) => {
            if kind == ArtifactKind::FetchedFromUpstream {
                tracing::debug!(仓库 = %repo.name, 模块 = %module_bang, 版本 = %version, "proxy 回源 Go 模块文件");
            }
            stream_response(handle, file)
        }
        // hosted 下 .info 缺失时按 .mod 合成（仅 hosted；proxy 已在 get 内回源）
        Err(crate::format::ServiceError::NotFound)
            if file == VersionFile::Info
                && RepoType::from_db_str(&repo.r#type) == RepoType::Hosted =>
        {
            let info = resolve_info(state, repo, module_bang, version).await?;
            json_response(info)
        }
        Err(e) => Err(e.into()),
    }
}

/// 解析某版本的 `.info` 字节：已显式上传则直读，否则据 `.mod` 制品 `created_at` 合成。
///
/// 仅在 hosted 路径调用（proxy 的 info 由上游提供）。版本不存在（无 .info 也无 .mod）返回 404。
async fn resolve_info(
    state: &AppState,
    repo: &RepositoryRecord,
    module_bang: &str,
    version: &str,
) -> Result<Vec<u8>, ApiError> {
    // 优先读已上传的 .info
    let info_path = GoFormat::version_storage_path(module_bang, version, VersionFile::Info);
    if let Some(rec) = state.meta.get_artifact(&repo.id, &info_path).await? {
        let mut handle = state
            .artifacts
            .get(
                repo,
                go_format(state, repo)?,
                &go_coords(state, repo, &info_path)?,
            )
            .await?
            .0;
        use tokio::io::AsyncReadExt;
        let mut buf = Vec::new();
        handle
            .blob
            .read_to_end(&mut buf)
            .await
            .map_err(|_| ApiError::Internal)?;
        let _ = rec;
        return Ok(buf);
    }

    // 退而求其次：据 .mod 制品的 created_at 合成 info
    let mod_path = GoFormat::version_storage_path(module_bang, version, VersionFile::Mod);
    let mod_rec = state
        .meta
        .get_artifact(&repo.id, &mod_path)
        .await?
        .ok_or(ApiError::NotFound)?;
    let time = GoFormat::timestamp_to_rfc3339(&mod_rec.created_at);
    Ok(GoFormat::build_info_json(version, &time))
}

/// 列出 hosted 仓库内某模块的全部版本（据 `{module_bang}/@v/{version}.{ext}` 制品聚合去重）。
async fn list_hosted_versions(
    state: &AppState,
    repo: &RepositoryRecord,
    module_bang: &str,
) -> Result<Vec<String>, ApiError> {
    let prefix = format!("{module_bang}/@v/");
    let records = state.meta.list_artifacts_by_repo(&repo.id).await?;
    let mut versions: Vec<String> = records
        .into_iter()
        .filter_map(|r| version_from_storage_path(&r.path, &prefix))
        .collect();
    versions.sort();
    versions.dedup();
    Ok(versions)
}

/// 从存储路径中提取版本号：路径须以 `{prefix}` 开头且末段为 `{version}.{info|mod|zip}`。
fn version_from_storage_path(path: &str, prefix: &str) -> Option<String> {
    let rest = path.strip_prefix(prefix)?;
    // rest 形如 `{version}.{ext}`，且不能再含 `/`（排除非本模块的更深路径）
    if rest.contains('/') {
        return None;
    }
    let (version, ext) = rest.rsplit_once('.')?;
    VersionFile::from_ext(ext)?;
    if version.is_empty() {
        return None;
    }
    Some(version.to_string())
}

/// 把读句柄按文件类型封装为流式响应。
fn stream_response(
    handle: crate::format::service::ReadHandle,
    file: VersionFile,
) -> Result<Response, ApiError> {
    let content_type = match file {
        VersionFile::Zip => "application/zip",
        VersionFile::Mod => "text/plain; charset=utf-8",
        VersionFile::Info => "application/json",
    };
    let body = Body::from_stream(ReaderStream::new(handle.blob));
    Response::builder()
        .status(StatusCode::OK)
        .header(header::CONTENT_TYPE, content_type)
        .header(header::CONTENT_LENGTH, handle.record.size)
        .header("x-checksum-sha256", handle.record.sha256)
        .body(body)
        .map_err(|_| ApiError::Internal)
}

/// 取 Go 格式处理器；非 go 格式视为内部路由错误（调用前应已据格式分派）。
fn go_format<'a>(
    state: &'a AppState,
    repo: &RepositoryRecord,
) -> Result<&'a dyn crate::format::Format, ApiError> {
    state
        .formats
        .get(&repo.format)
        .ok_or(ApiError::BadRequest("仓库格式未实现".to_string()))
}

/// 据存储路径构造坐标（复用格式的归一化与穿越校验）。
fn go_coords(
    state: &AppState,
    repo: &RepositoryRecord,
    storage_path: &str,
) -> Result<crate::format::ArtifactCoordinates, ApiError> {
    Ok(go_format(state, repo)?.parse_path(storage_path)?)
}

/// 把字节封装为 `text/plain` 200 响应（list / mod 文本）。
fn text_response(bytes: Vec<u8>) -> Result<Response, ApiError> {
    let len = bytes.len();
    Response::builder()
        .status(StatusCode::OK)
        .header(header::CONTENT_TYPE, "text/plain; charset=utf-8")
        .header(header::CONTENT_LENGTH, len)
        .body(Body::from(bytes))
        .map_err(|_| ApiError::Internal)
}

/// 把 JSON 字节封装为 200 响应（info / latest）。
fn json_response(bytes: Vec<u8>) -> Result<Response, ApiError> {
    let len = bytes.len();
    Response::builder()
        .status(StatusCode::OK)
        .header(header::CONTENT_TYPE, "application/json")
        .header(header::CONTENT_LENGTH, len)
        .body(Body::from(bytes))
        .map_err(|_| ApiError::Internal)
}

/// 把 Go 协议错误映射为 HTTP 错误（端点不合法 → 400）。
fn go_err_to_api(e: GoError) -> ApiError {
    match e {
        GoError::InvalidEndpoint => ApiError::NotFound,
        GoError::InvalidBang => ApiError::BadRequest("模块路径编码非法".to_string()),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn 从存储路径提取版本() {
        let prefix = "golang.org/x/text/@v/";
        assert_eq!(
            version_from_storage_path("golang.org/x/text/@v/v0.3.7.mod", prefix).as_deref(),
            Some("v0.3.7")
        );
        assert_eq!(
            version_from_storage_path("golang.org/x/text/@v/v0.3.7.zip", prefix).as_deref(),
            Some("v0.3.7")
        );
        // 非本模块前缀
        assert!(version_from_storage_path("other/@v/v1.0.0.mod", prefix).is_none());
        // 末段含更深路径
        assert!(version_from_storage_path("golang.org/x/text/@v/sub/v1.0.0.mod", prefix).is_none());
        // 扩展名非版本文件
        assert!(version_from_storage_path("golang.org/x/text/@v/v0.3.7.txt", prefix).is_none());
    }
}
