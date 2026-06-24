//! Cargo 稀疏索引协议端点（FR-26）：registry config、稀疏索引、发布、下载、yank/unyank。
//!
//! 协议适配在本模块，业务机理下沉到 `format::ArtifactService`（存储 / 校验和 / 事务 / 单飞缓存）
//! 与 `format::CargoFormat`（索引行生成 / 合并、yank 翻转、config 生成等纯函数）。
//! 经既有授权编排门控：发布 / yank 需 write，读受 visibility / ACL，private 无权一律 404。
//!
//! 存储约定（与 CargoFormat 一致）：索引存于 `index/{index_path}`，`.crate` 存于
//! `crates/{name}/{name}-{vers}.crate`，`config.json` 动态生成不落存储。

use axum::{
    body::Body,
    http::{header, StatusCode},
    response::Response,
};
use tokio::io::AsyncReadExt;

use crate::format::{ArtifactKind, CargoError, CargoFormat};
use crate::meta::{RepoType, RepositoryRecord};

use super::{ApiError, AppState};

/// 索引文件缓冲上限（16 MiB）：单包索引为版本列表 JSON，远小于此；超限按上游异常处理。
const INDEX_MAX_BYTES: usize = 16 * 1024 * 1024;

/// registry 配置文件子路径。
const CONFIG_PATH: &str = "config.json";

/// 下载 API 子路径前缀：`api/v1/crates/{name}/{version}/download`。
const DOWNLOAD_PREFIX: &str = "api/v1/crates/";

/// 处理 Cargo 读请求（GET）：据子路径分派 config.json / 下载 / 稀疏索引。
pub async fn get(
    state: &AppState,
    repo: &RepositoryRecord,
    path: &str,
) -> Result<Response, ApiError> {
    // config.json：动态生成，指回本仓库
    if path == CONFIG_PATH {
        let base = super::artifacts::public_base_url(state);
        let bytes = CargoFormat::config_json(&base, &repo.name);
        return json_response(bytes);
    }

    // 下载：api/v1/crates/{name}/{version}/download
    if let Some(rest) = path.strip_prefix(DOWNLOAD_PREFIX) {
        if let Some((name, version)) = parse_download_path(rest) {
            return get_crate(state, repo, &name, &version).await;
        }
    }

    // 其余视为稀疏索引文件请求（如 se/rd/serde）
    get_index(state, repo, path).await
}

/// 发布 crate（`PUT /{repo}/api/v1/crates/new`）：解析 publish 体、落 `.crate`、更新索引。
///
/// 次序：① 解析二进制体；② 版本不可变预检（读既有索引含该版本 → 409，不写 blob）；
/// ③ 落 `.crate`（流式校验，得 sha256）；④ 生成索引行、合并索引并落定。任一步失败回滚不留孤儿。
pub async fn publish(
    state: &AppState,
    repo: &RepositoryRecord,
    body: Body,
) -> Result<Response, ApiError> {
    let format = cargo_format(state, repo)?;

    // 缓冲 publish 体（含内嵌 .crate 字节），按上传上限约束防超大体（超限 413）
    let raw = read_body_limited(body, state.config.limits.max_artifact_size).await?;
    let req = CargoFormat::parse_publish(&raw).map_err(cargo_err_to_api)?;

    // ① 版本不可变预检：读既有索引，若已含该版本则 409（不写 blob）
    let index_path = CargoFormat::index_storage_path(&req.name);
    let existing_index = read_stored_bytes(state, repo, &index_path).await?;
    if index_has_version(existing_index.as_deref(), &req.vers) {
        return Err(ApiError::Conflict(format!(
            "版本 {} 已发布，不可覆盖",
            req.vers
        )));
    }

    // ② 落 .crate：经通用机理流式写 blob（边写边算四摘要），得 sha256 供索引 cksum 用
    let crate_path = CargoFormat::crate_storage_path(&req.name, &req.vers);
    let crate_coords = format.parse_path(&crate_path)?;
    let outcome = state
        .artifacts
        .put_hosted(
            repo,
            format,
            &crate_coords,
            &req.crate_bytes[..],
            state.config.limits.max_artifact_size,
        )
        .await?;

    // ③ 生成索引行（cksum 用 sha256）并合并进既有索引，整体落定
    let line = CargoFormat::index_line(&req, &outcome.record.sha256).map_err(cargo_err_to_api)?;
    let merged =
        CargoFormat::merge_index(existing_index.as_deref().unwrap_or(&[]), &line, &req.vers)
            .map_err(cargo_err_to_api)?;
    let index_coords = format.parse_path(&index_path)?;
    state
        .artifacts
        .put_hosted(
            repo,
            format,
            &index_coords,
            &merged[..],
            state.config.limits.max_artifact_size,
        )
        .await?;

    tracing::info!(仓库 = %repo.name, 包 = %req.name, 版本 = %req.vers, "已发布 cargo crate");
    // Cargo publish 成功返回 200 + warnings 结构（即便无 warning 也须含该结构）
    let body =
        Body::from(r#"{"warnings":{"invalid_categories":[],"invalid_badges":[],"other":[]}}"#);
    ok_json(body)
}

/// yank / unyank（`DELETE`/`PUT /{repo}/api/v1/crates/{name}/{version}/yank`）：翻转索引行的 `yanked`。
pub async fn set_yanked(
    state: &AppState,
    repo: &RepositoryRecord,
    name: &str,
    version: &str,
    yanked: bool,
) -> Result<Response, ApiError> {
    let format = cargo_format(state, repo)?;
    let index_path = CargoFormat::index_storage_path(name);
    let existing = read_stored_bytes(state, repo, &index_path)
        .await?
        .ok_or(ApiError::NotFound)?;

    let updated = CargoFormat::set_yanked(&existing, version, yanked).map_err(cargo_err_to_api)?;
    let index_coords = format.parse_path(&index_path)?;
    state
        .artifacts
        .put_hosted(
            repo,
            format,
            &index_coords,
            &updated[..],
            state.config.limits.max_artifact_size,
        )
        .await?;

    tracing::info!(仓库 = %repo.name, 包 = %name, 版本 = %version, yank = yanked, "已更新 cargo yank 状态");
    ok_json(Body::from(r#"{"ok":true}"#))
}

/// 下载 `.crate`（`GET /{repo}/api/v1/crates/{name}/{version}/download`）：流式返回；proxy cache-miss 回源。
async fn get_crate(
    state: &AppState,
    repo: &RepositoryRecord,
    name: &str,
    version: &str,
) -> Result<Response, ApiError> {
    let format = cargo_format(state, repo)?;
    let path = CargoFormat::crate_storage_path(name, version);
    let coords = format.parse_path(&path)?;

    // 本地存储键（crates/{name}/{name}-{vers}.crate）异于上游下载 API 路径
    // （api/v1/crates/{name}/{version}/download），proxy 回源须按后者拉取。
    let upstream_rel = format!("{DOWNLOAD_PREFIX}{name}/{version}/download");
    let (handle, kind) = state
        .artifacts
        .get_with_upstream_path(repo, format, &coords, Some(&upstream_rel))
        .await?;
    if kind == ArtifactKind::FetchedFromUpstream {
        tracing::debug!(仓库 = %repo.name, 包 = %name, "proxy 回源 cargo crate");
    }
    let body = Body::from_stream(tokio_util::io::ReaderStream::new(handle.blob));
    Response::builder()
        .status(StatusCode::OK)
        .header(header::CONTENT_TYPE, "application/octet-stream")
        .header(header::CONTENT_LENGTH, handle.record.size)
        .header("x-checksum-sha256", handle.record.sha256)
        .body(body)
        .map_err(|_| ApiError::Internal)
}

/// 获取稀疏索引文件（`GET /{repo}/{index_path}`）：hosted 返回存储文档；proxy 回源上游索引（不缓存）。
async fn get_index(
    state: &AppState,
    repo: &RepositoryRecord,
    index_path: &str,
) -> Result<Response, ApiError> {
    if RepoType::from_db_str(&repo.r#type) == RepoType::Proxy {
        // proxy：每次回源拉取最新索引（索引易变，不缓存）；上游稀疏索引路径与本仓库一致
        let upstream = state
            .artifacts
            .fetch_upstream_doc(repo, index_path, INDEX_MAX_BYTES)
            .await?;
        return index_response(upstream);
    }

    // hosted：返回存储的索引文件（存于 index/{index_path}）
    let storage_path = format!("index/{index_path}");
    let bytes = read_stored_bytes(state, repo, &storage_path)
        .await?
        .ok_or(ApiError::NotFound)?;
    index_response(bytes)
}

/// 解析下载子路径 `{name}/{version}/download` → (name, version)；不匹配返回 None。
fn parse_download_path(rest: &str) -> Option<(String, String)> {
    // 形如 {name}/{version}/download，name 不含斜杠（crate 名无斜杠）
    let stripped = rest.strip_suffix("/download")?;
    let (name, version) = stripped.split_once('/')?;
    if name.is_empty() || version.is_empty() {
        return None;
    }
    Some((name.to_string(), version.to_string()))
}

/// 取 cargo 格式处理器；非 cargo 格式视为内部路由错误（调用前应已据格式分派）。
fn cargo_format<'a>(
    state: &'a AppState,
    repo: &RepositoryRecord,
) -> Result<&'a dyn crate::format::Format, ApiError> {
    state
        .formats
        .get(&repo.format)
        .ok_or(ApiError::BadRequest("仓库格式未实现".to_string()))
}

/// 读取仓库内某存储路径的字节；不存在返回 None。仅用于 hosted 存储文档（索引）读取。
async fn read_stored_bytes(
    state: &AppState,
    repo: &RepositoryRecord,
    storage_path: &str,
) -> Result<Option<Vec<u8>>, ApiError> {
    let format = cargo_format(state, repo)?;
    let coords = format.parse_path(storage_path)?;
    match state.artifacts.get(repo, format, &coords).await {
        Ok((mut handle, _)) => {
            let mut buf = Vec::new();
            handle
                .blob
                .read_to_end(&mut buf)
                .await
                .map_err(|_| ApiError::Internal)?;
            Ok(Some(buf))
        }
        Err(crate::format::ServiceError::NotFound) => Ok(None),
        Err(e) => Err(e.into()),
    }
}

/// 判断索引字节中是否已含某版本（扫描每行 JSON 的 `vers`）。
fn index_has_version(index: Option<&[u8]>, version: &str) -> bool {
    let Some(bytes) = index else {
        return false;
    };
    let Ok(text) = std::str::from_utf8(bytes) else {
        return false;
    };
    text.lines().any(|line| {
        serde_json::from_str::<serde_json::Value>(line.trim())
            .ok()
            .and_then(|v| {
                v.get("vers")
                    .and_then(serde_json::Value::as_str)
                    .map(|s| s == version)
            })
            .unwrap_or(false)
    })
}

/// 把 JSON 字节封装为 200 响应（用于 config.json）。
fn json_response(bytes: Vec<u8>) -> Result<Response, ApiError> {
    let len = bytes.len();
    Response::builder()
        .status(StatusCode::OK)
        .header(header::CONTENT_TYPE, "application/json")
        .header(header::CONTENT_LENGTH, len)
        .body(Body::from(bytes))
        .map_err(|_| ApiError::Internal)
}

/// 把稀疏索引字节封装为 200 响应（Cargo 索引内容类型为 text/plain）。
fn index_response(bytes: Vec<u8>) -> Result<Response, ApiError> {
    let len = bytes.len();
    Response::builder()
        .status(StatusCode::OK)
        .header(header::CONTENT_TYPE, "text/plain; charset=utf-8")
        .header(header::CONTENT_LENGTH, len)
        .body(Body::from(bytes))
        .map_err(|_| ApiError::Internal)
}

/// 构造 200 + JSON 响应体（用于 publish / yank 的简单 JSON 返回）。
fn ok_json(body: Body) -> Result<Response, ApiError> {
    Response::builder()
        .status(StatusCode::OK)
        .header(header::CONTENT_TYPE, "application/json")
        .body(body)
        .map_err(|_| ApiError::Internal)
}

/// 按上限缓冲请求体到内存；超限返回 413（cargo 发布体须整体解析，无法纯流式）。
async fn read_body_limited(body: Body, max: Option<u64>) -> Result<Vec<u8>, ApiError> {
    let limit = max.map(|m| m as usize).unwrap_or(usize::MAX);
    match axum::body::to_bytes(body, limit).await {
        Ok(bytes) => Ok(bytes.to_vec()),
        Err(_) => {
            if max.is_some() {
                Err(ApiError::PayloadTooLarge)
            } else {
                Err(ApiError::BadRequest("读取请求体失败".to_string()))
            }
        }
    }
}

/// 把 Cargo 协议错误映射为 HTTP 错误。
fn cargo_err_to_api(e: CargoError) -> ApiError {
    match e {
        CargoError::VersionExists(v) => ApiError::Conflict(format!("版本 {v} 已发布，不可覆盖")),
        // yank 目标版本不存在按 404（不泄露细节）
        CargoError::VersionNotFound(_) => ApiError::NotFound,
        CargoError::InvalidBody(msg) => ApiError::BadRequest(msg),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn 解析下载子路径() {
        assert_eq!(
            parse_download_path("serde/1.0.0/download"),
            Some(("serde".to_string(), "1.0.0".to_string()))
        );
        // 缺 download 后缀
        assert_eq!(parse_download_path("serde/1.0.0"), None);
        // 缺版本
        assert_eq!(parse_download_path("serde//download"), None);
    }

    #[test]
    fn 索引版本存在判定() {
        let idx = b"{\"name\":\"a\",\"vers\":\"1.0.0\"}\n{\"name\":\"a\",\"vers\":\"2.0.0\"}\n";
        assert!(index_has_version(Some(idx), "1.0.0"));
        assert!(index_has_version(Some(idx), "2.0.0"));
        assert!(!index_has_version(Some(idx), "3.0.0"));
        assert!(!index_has_version(None, "1.0.0"));
    }
}
