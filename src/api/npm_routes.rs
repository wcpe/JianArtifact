//! npm registry 协议端点（FR-15）：发布、packument 获取、tarball 下载。
//!
//! 协议适配在本模块，业务机理下沉到 `format::ArtifactService`（存储 / 校验和 / 事务 / 单飞缓存）
//! 与 `format::NpmFormat`（packument 生成 / 合并、代理 URL 重写等纯函数）。
//! 经既有授权编排门控：发布需 write，读受 visibility / ACL，private 无权一律 404。
//!
//! 存储约定（与 NpmFormat 一致）：packument 存于路径 `{包名}`，tarball 存于 `{包名}/-/{文件}`。

use axum::{
    body::Body,
    http::{header, StatusCode},
    response::Response,
};
use serde_json::Value;
use tokio::io::AsyncReadExt;

use crate::format::{ArtifactKind, NpmError, NpmFormat};
use crate::meta::RepositoryRecord;

use super::{ApiError, AppState};

/// packument 文档缓冲上限（16 MiB）：packument 是版本索引 JSON，远小于此；超限按上游异常处理。
const PACKUMENT_MAX_BYTES: usize = 16 * 1024 * 1024;

/// 发布包（`PUT /{repo}/{package}`）：解析发布体、落 tarball、生成 / 合并 packument。
///
/// 次序：① 版本不可变预检（已发布 → 409，不写任何 blob）；② 落 tarball（流式校验，得摘要）；
/// ③ 据摘要重写 dist 后合并 packument 并落定。任一步失败回滚，不留孤儿。
pub async fn publish(
    state: &AppState,
    repo: &RepositoryRecord,
    body: Body,
) -> Result<Response, ApiError> {
    let format = npm_format(state, repo)?;

    // 缓冲发布体（含 base64 tarball）。npm publish 体须整体解析，按上传上限约束防超大体。
    let raw = read_body_limited(body, state.config.limits.max_artifact_size).await?;
    let req = NpmFormat::parse_publish(&raw).map_err(npm_err_to_api)?;

    // ① 版本不可变预检：读既有 packument，若已含该版本则 409（不写 blob）
    let existing = read_packument(state, repo, &req.package).await?;
    if packument_has_version(existing.as_ref(), &req.version) {
        return Err(ApiError::Conflict(format!("版本 {} 已发布，不可覆盖", req.version)));
    }

    // ② 落 tarball：经通用机理流式写 blob（边写边算四摘要），得摘要供 packument dist 用
    let tarball_path = NpmFormat::tarball_path(&req.package, &req.tarball_name);
    let tarball_coords = format.parse_path(&tarball_path)?;
    let outcome = state
        .artifacts
        .put_hosted(
            repo,
            format,
            &tarball_coords,
            &req.tarball[..],
            state.config.limits.max_artifact_size,
        )
        .await?;

    // ③ 合并 packument：dist.tarball 指向本仓库、shasum 用 sha1、integrity 用 sha512(base64)
    let base = super::artifacts::public_base_url(state);
    let sha512_b64 = hex_to_base64(&outcome.record.sha512).ok_or(ApiError::Internal)?;
    let packument = NpmFormat::merge_packument(
        existing.as_ref(),
        &req,
        &base,
        &repo.name,
        &outcome.record.sha1,
        &sha512_b64,
    )
    .map_err(npm_err_to_api)?;
    let packument_bytes = serde_json::to_vec(&packument).map_err(|_| ApiError::Internal)?;

    // 落定 packument（packument 可更新，由 NpmFormat::can_overwrite 放行覆盖）
    let pack_coords = format.parse_path(&req.package)?;
    state
        .artifacts
        .put_hosted(
            repo,
            format,
            &pack_coords,
            &packument_bytes[..],
            state.config.limits.max_artifact_size,
        )
        .await?;

    tracing::info!(仓库 = %repo.name, 包 = %req.package, 版本 = %req.version, "已发布 npm 包");
    // npm 发布成功返回 201 + 简单 JSON
    let body = Body::from(r#"{"ok":true}"#);
    Response::builder()
        .status(StatusCode::CREATED)
        .header(header::CONTENT_TYPE, "application/json")
        .body(body)
        .map_err(|_| ApiError::Internal)
}

/// 获取 packument（`GET /{repo}/{package}`）：hosted 返回存储文档；proxy 回源上游并重写 tarball URL。
pub async fn get_packument(
    state: &AppState,
    repo: &RepositoryRecord,
    package: &str,
) -> Result<Response, ApiError> {
    use crate::meta::RepoType;

    if RepoType::from_db_str(&repo.r#type) == RepoType::Proxy {
        // proxy：每次回源拉取最新 packument 后重写 tarball 指向本仓库（packument 易变，不缓存）
        let base = super::artifacts::public_base_url(state);
        let upstream = state
            .artifacts
            .fetch_upstream_doc(repo, package, PACKUMENT_MAX_BYTES)
            .await?;
        let rewritten = NpmFormat::rewrite_proxy_packument(&upstream, &base, &repo.name)
            .map_err(npm_err_to_api)?;
        return json_response(rewritten);
    }

    // hosted：返回存储的 packument 文档
    let bytes = read_packument_bytes(state, repo, package)
        .await?
        .ok_or(ApiError::NotFound)?;
    json_response(bytes)
}

/// 下载 tarball（`GET /{repo}/{package}/-/{tarball}`）：经通用机理流式返回；proxy cache-miss 回源。
pub async fn get_tarball(
    state: &AppState,
    repo: &RepositoryRecord,
    package: &str,
    tarball_name: &str,
) -> Result<Response, ApiError> {
    let format = npm_format(state, repo)?;
    let path = NpmFormat::tarball_path(package, tarball_name);
    let coords = format.parse_path(&path)?;

    let (handle, kind) = state.artifacts.get(repo, format, &coords).await?;
    if kind == ArtifactKind::FetchedFromUpstream {
        tracing::debug!(仓库 = %repo.name, 包 = %package, "proxy 回源 npm tarball");
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

/// 取 npm 格式处理器；非 npm 格式视为内部路由错误（调用前应已据格式分派）。
fn npm_format<'a>(
    state: &'a AppState,
    repo: &RepositoryRecord,
) -> Result<&'a dyn crate::format::Format, ApiError> {
    state
        .formats
        .get(&repo.format)
        .ok_or(ApiError::BadRequest("仓库格式未实现".to_string()))
}

/// 读取 hosted 仓库存储的 packument 字节；不存在返回 None。
async fn read_packument_bytes(
    state: &AppState,
    repo: &RepositoryRecord,
    package: &str,
) -> Result<Option<Vec<u8>>, ApiError> {
    let format = npm_format(state, repo)?;
    let coords = format.parse_path(package)?;
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

/// 读取并解析既有 packument（hosted）为 JSON；不存在返回 None。
async fn read_packument(
    state: &AppState,
    repo: &RepositoryRecord,
    package: &str,
) -> Result<Option<Value>, ApiError> {
    match read_packument_bytes(state, repo, package).await? {
        Some(bytes) => {
            let v = serde_json::from_slice(&bytes).map_err(|_| ApiError::Internal)?;
            Ok(Some(v))
        }
        None => Ok(None),
    }
}

/// 判断 packument 是否已含某版本。
fn packument_has_version(packument: Option<&Value>, version: &str) -> bool {
    packument
        .and_then(|p| p.get("versions"))
        .and_then(Value::as_object)
        .map(|m| m.contains_key(version))
        .unwrap_or(false)
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

/// 按上限缓冲请求体到内存；超限返回 413（npm 发布体须整体解析，无法纯流式）。
async fn read_body_limited(body: Body, max: Option<u64>) -> Result<Vec<u8>, ApiError> {
    // axum::body::to_bytes 在累计超过 limit 时即报错，避免读入超大体撑爆内存
    let limit = max.map(|m| m as usize).unwrap_or(usize::MAX);
    match axum::body::to_bytes(body, limit).await {
        Ok(bytes) => Ok(bytes.to_vec()),
        // 超限或读取失败：体积上限触发按 413，其余按 400
        Err(_) => {
            if max.is_some() {
                Err(ApiError::PayloadTooLarge)
            } else {
                Err(ApiError::BadRequest("读取请求体失败".to_string()))
            }
        }
    }
}

/// 把十六进制摘要转为 base64（npm integrity 用 `sha512-<base64(原始字节)>`）。
fn hex_to_base64(hex: &str) -> Option<String> {
    use base64::Engine;
    let bytes = hex_decode(hex)?;
    Some(base64::engine::general_purpose::STANDARD.encode(bytes))
}

/// 解码小写 / 大写十六进制串为字节；非法字符返回 None。
fn hex_decode(hex: &str) -> Option<Vec<u8>> {
    if !hex.len().is_multiple_of(2) {
        return None;
    }
    (0..hex.len())
        .step_by(2)
        .map(|i| u8::from_str_radix(&hex[i..i + 2], 16).ok())
        .collect()
}

/// 把 npm 协议错误映射为 HTTP 错误。
fn npm_err_to_api(e: NpmError) -> ApiError {
    match e {
        NpmError::VersionExists(v) => ApiError::Conflict(format!("版本 {v} 已发布，不可覆盖")),
        NpmError::InvalidBody(msg) => ApiError::BadRequest(msg),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn hex_转_base64() {
        // "abc" 的 sha1 与已知 base64 不在此验证，仅验证编码正确性
        assert_eq!(hex_to_base64("00ff").unwrap(), "AP8=");
        assert_eq!(hex_to_base64("").unwrap(), "");
        // 非法十六进制返回 None
        assert!(hex_to_base64("0g").is_none());
        assert!(hex_to_base64("abc").is_none());
    }

    #[test]
    fn packument_版本存在判定() {
        let p = serde_json::json!({ "versions": { "1.0.0": {} } });
        assert!(packument_has_version(Some(&p), "1.0.0"));
        assert!(!packument_has_version(Some(&p), "2.0.0"));
        assert!(!packument_has_version(None, "1.0.0"));
    }
}
