//! PyPI 格式端点（FR-27）：Simple Repository API（PEP503/691）索引、包文件下载、twine 上传。
//!
//! 协议适配在本模块，业务机理下沉到 `format::ArtifactService`（存储 / 校验和 / 事务 / 单飞缓存）
//! 与 `format::PypiFormat`（项目名规范化 / Simple 页面生成 / 上传解析 / 代理重写等纯函数）。
//! 经既有授权编排门控：上传需 write，读受 visibility / ACL，private 无权一律 404。
//!
//! 存储约定（与 PypiFormat 一致）：发行文件存于 `packages/{规范名}/{文件名}`；
//! Simple 页面 hosted 由存储文件实时枚举生成、proxy 每次回源上游（索引不缓存，仅包文件缓存）。

use axum::{
    body::Body,
    extract::Multipart,
    http::{header, HeaderMap, StatusCode},
    response::Response,
};

use crate::format::{
    ArtifactKind, MultipartField, PypiError, PypiFormat, PYPI_PACKAGES_PREFIX,
    PYPI_PEP691_CONTENT_TYPE, PYPI_SIMPLE_SEGMENT,
};
use crate::meta::{RepoType, RepositoryRecord};

use super::{ApiError, AppState};

/// 上游 Simple 文档缓冲上限（16 MiB）：Simple 项目页是文件链接 HTML，远小于此；超限按上游异常处理。
const SIMPLE_DOC_MAX_BYTES: usize = 16 * 1024 * 1024;

/// HTML 内容类型（PEP503 Simple 页面）。
const SIMPLE_HTML_CONTENT_TYPE: &str = "text/html; charset=utf-8";

/// 上传发行文件（`POST /{repo}/`，twine）：解析 multipart、落 blob、校验摘要、不可覆盖。
///
/// 次序：① 解析 multipart 体（按上传上限约束）；② 据规范名拼存储路径；
/// ③ 经通用机理流式落 blob（边写边算四摘要，已存在同文件由 `can_overwrite=false` 拒为 409）；
/// ④ 校验客户端声明的 sha256_digest 与服务端算得一致（不符 400，并回滚已落 blob）。
pub async fn upload(
    state: &AppState,
    repo: &RepositoryRecord,
    multipart: Multipart,
) -> Result<Response, ApiError> {
    let format = pypi_format(state, repo)?;

    // ① 把 multipart 各字段读入内存（受上传上限约束，超限 413）
    let fields = read_multipart_fields(multipart, state.config.limits.max_artifact_size).await?;
    let req = PypiFormat::parse_upload(&fields).map_err(pypi_err_to_api)?;

    // ② 据 PEP503 规范名拼存储路径
    let path = PypiFormat::package_path(&req.name, &req.filename);
    let coords = format.parse_path(&path)?;

    // ③ 落 blob：已存在同文件时 can_overwrite=false → OverwriteForbidden → 409
    let outcome = state
        .artifacts
        .put_hosted(
            repo,
            format,
            &coords,
            &req.content[..],
            state.config.limits.max_artifact_size,
        )
        .await?;

    // ④ 校验客户端声明的 sha256_digest；不符则回滚刚落的索引 + blob 并报 400
    if let Err(e) = PypiFormat::verify_digest(req.sha256_digest.as_deref(), &outcome.record.sha256)
    {
        // 删除刚写入的制品（索引 + 无引用 blob），不留下与声明摘要不符的内容
        let _ = state.artifacts.delete(repo, &coords).await;
        return Err(pypi_err_to_api(e));
    }

    tracing::info!(仓库 = %repo.name, 项目 = %req.name, 文件 = %req.filename, "已上传 PyPI 发行文件");
    Response::builder()
        .status(StatusCode::OK)
        .header(header::CONTENT_TYPE, "text/plain; charset=utf-8")
        .body(Body::from("OK"))
        .map_err(|_| ApiError::Internal)
}

/// Simple 根索引（`GET /{repo}/simple/`）：列出仓库内全部项目。
///
/// hosted 由存储的发行文件枚举去重出项目名；proxy 回源上游 `/simple/` 并原样透传（链接为相对项目段）。
pub async fn simple_index(
    state: &AppState,
    repo: &RepositoryRecord,
    headers: &HeaderMap,
) -> Result<Response, ApiError> {
    if RepoType::from_db_str(&repo.r#type) == RepoType::Proxy {
        // proxy：回源上游根索引（项目列表 HTML），原样返回（其链接为相对项目段，pip 据当前 URL 解析）
        let upstream = state
            .artifacts
            .fetch_upstream_doc(
                repo,
                &format!("{PYPI_SIMPLE_SEGMENT}/"),
                SIMPLE_DOC_MAX_BYTES,
            )
            .await?;
        return Ok(html_response(upstream));
    }

    // hosted：枚举存储文件去重出规范化项目名
    let projects = list_projects(state, repo).await?;
    if wants_json(headers) {
        let json = PypiFormat::simple_index_json(&projects);
        return json_simple_response(json);
    }
    let html = PypiFormat::simple_index_html(&projects);
    Ok(html_response(html.into_bytes()))
}

/// Simple 项目页（`GET /{repo}/simple/{project}/`）：列出该项目所有发行文件及其 sha256。
///
/// hosted 据存储文件生成；proxy 回源上游项目页并把文件链接重写为本仓库 packages 路径。
pub async fn simple_project(
    state: &AppState,
    repo: &RepositoryRecord,
    project: &str,
    headers: &HeaderMap,
) -> Result<Response, ApiError> {
    if RepoType::from_db_str(&repo.r#type) == RepoType::Proxy {
        // proxy：回源上游项目页 HTML，重写文件链接指向本仓库（保留 #sha256= 片段）
        let rel = format!("{PYPI_SIMPLE_SEGMENT}/{project}/");
        let upstream = state
            .artifacts
            .fetch_upstream_doc(repo, &rel, SIMPLE_DOC_MAX_BYTES)
            .await?;
        let upstream_html = String::from_utf8_lossy(&upstream);
        let (rewritten, _mapping) = PypiFormat::rewrite_proxy_project_html(&upstream_html, project);
        return Ok(html_response(rewritten.into_bytes()));
    }

    // hosted：据存储文件生成项目页（文件名 + sha256）
    let files = list_project_files(state, repo, project).await?;
    if files.is_empty() {
        // 该项目无任何文件 → 404（与 PyPI 行为一致）
        return Err(ApiError::NotFound);
    }
    if wants_json(headers) {
        let json = PypiFormat::simple_project_json(project, &files);
        return json_simple_response(json);
    }
    let html = PypiFormat::simple_project_html(project, &files);
    Ok(html_response(html.into_bytes()))
}

/// 包文件下载（`GET /{repo}/packages/{规范名}/{文件}`）：流式返回；proxy cache-miss 回源。
///
/// proxy 回源前先取上游项目页解析出该文件的上游 URL（PyPI 文件常跨主机），再经显式 URL 回源缓存。
pub async fn download(
    state: &AppState,
    repo: &RepositoryRecord,
    path: &str,
) -> Result<Response, ApiError> {
    let format = pypi_format(state, repo)?;
    let coords = format.parse_path(path)?;

    let (handle, kind) = if RepoType::from_db_str(&repo.r#type) == RepoType::Proxy {
        // 缓存命中走本地；未命中需解析上游文件 URL 后回源
        match state.artifacts.get(repo, format, &coords).await {
            // get 的相对路径回源模型对 PyPI 跨主机文件不适用，故未命中时改走显式 URL 回源
            Ok(hit) => hit,
            Err(crate::format::ServiceError::Upstream)
            | Err(crate::format::ServiceError::NotFound) => {
                let upstream_url = resolve_upstream_file_url(state, repo, &coords.path).await?;
                state
                    .artifacts
                    .get_or_fetch_from(repo, format, &coords, &upstream_url)
                    .await?
            }
            Err(e) => return Err(e.into()),
        }
    } else {
        state.artifacts.get(repo, format, &coords).await?
    };

    if kind == ArtifactKind::FetchedFromUpstream {
        tracing::debug!(仓库 = %repo.name, 路径 = %coords.path, "proxy 回源 PyPI 包文件");
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

/// proxy 包文件回源前：取上游项目页，解析出请求文件名对应的上游 URL。
///
/// 包文件路径形如 `packages/{规范名}/{文件}`，据规范名回源对应 Simple 项目页，
/// 从其文件链接映射中取出该文件名的上游 URL（PyPI 文件多托管于 files.pythonhosted.org）。
async fn resolve_upstream_file_url(
    state: &AppState,
    repo: &RepositoryRecord,
    cache_path: &str,
) -> Result<String, ApiError> {
    let project = PypiFormat::project_of_package_path(cache_path).ok_or(ApiError::NotFound)?;
    let filename = cache_path.rsplit('/').next().unwrap_or("");

    let rel = format!("{PYPI_SIMPLE_SEGMENT}/{project}/");
    let upstream = state
        .artifacts
        .fetch_upstream_doc(repo, &rel, SIMPLE_DOC_MAX_BYTES)
        .await?;
    let upstream_html = String::from_utf8_lossy(&upstream);
    let (_rewritten, mapping) = PypiFormat::rewrite_proxy_project_html(&upstream_html, project);

    mapping
        .into_iter()
        .find(|(name, _)| name == filename)
        .map(|(_, url)| url)
        .ok_or(ApiError::NotFound)
}

/// 取 PyPI 格式处理器；非 PyPI 格式视为内部路由错误（调用前应已据格式分派）。
fn pypi_format<'a>(
    state: &'a AppState,
    repo: &RepositoryRecord,
) -> Result<&'a dyn crate::format::Format, ApiError> {
    state
        .formats
        .get(&repo.format)
        .ok_or(ApiError::BadRequest("仓库格式未实现".to_string()))
}

/// 枚举 hosted 仓库内全部项目（据存储发行文件路径去重出规范化项目名，按名排序）。
async fn list_projects(state: &AppState, repo: &RepositoryRecord) -> Result<Vec<String>, ApiError> {
    let records = state.meta.list_artifacts_by_repo(&repo.id).await?;
    let mut projects: Vec<String> = records
        .iter()
        .filter_map(|r| PypiFormat::project_of_package_path(&r.path))
        .map(str::to_string)
        .collect();
    projects.sort();
    projects.dedup();
    Ok(projects)
}

/// 枚举 hosted 仓库内某项目的全部发行文件（文件名 + sha256），按文件名排序。
async fn list_project_files(
    state: &AppState,
    repo: &RepositoryRecord,
    project: &str,
) -> Result<Vec<(String, String)>, ApiError> {
    let norm = PypiFormat::normalize_project(project);
    let prefix = format!("{PYPI_PACKAGES_PREFIX}/{norm}/");
    let records = state.meta.list_artifacts_by_repo(&repo.id).await?;
    let mut files: Vec<(String, String)> = records
        .into_iter()
        .filter(|r| r.path.starts_with(&prefix))
        .filter_map(|r| {
            r.path
                .rsplit('/')
                .next()
                .map(|f| (f.to_string(), r.sha256.clone()))
        })
        .collect();
    files.sort();
    Ok(files)
}

/// 据 Accept 头判断客户端是否要 PEP691 JSON（含 `application/vnd.pypi.simple.v1+json`）。
fn wants_json(headers: &HeaderMap) -> bool {
    headers
        .get(header::ACCEPT)
        .and_then(|v| v.to_str().ok())
        .map(|a| a.contains(PYPI_PEP691_CONTENT_TYPE))
        .unwrap_or(false)
}

/// 把 HTML 字节封装为 200 响应（Simple 页面）。
fn html_response(bytes: Vec<u8>) -> Response {
    let len = bytes.len();
    Response::builder()
        .status(StatusCode::OK)
        .header(header::CONTENT_TYPE, SIMPLE_HTML_CONTENT_TYPE)
        .header(header::CONTENT_LENGTH, len)
        .body(Body::from(bytes))
        .expect("构造 HTML 响应不应失败")
}

/// 把 PEP691 JSON 封装为 200 响应（带专属内容类型）。
fn json_simple_response(json: serde_json::Value) -> Result<Response, ApiError> {
    let bytes = serde_json::to_vec(&json).map_err(|_| ApiError::Internal)?;
    let len = bytes.len();
    Response::builder()
        .status(StatusCode::OK)
        .header(header::CONTENT_TYPE, PYPI_PEP691_CONTENT_TYPE)
        .header(header::CONTENT_LENGTH, len)
        .body(Body::from(bytes))
        .map_err(|_| ApiError::Internal)
}

/// 逐字段读取 multipart 上传体到内存，累计受上传上限约束（超限 413）。
///
/// twine 上传体含一个文件字段（wheel/sdist）与若干文本字段；按上限缓冲单次上传总字节，
/// 超过 `max` 即拒绝并返回 413，不继续读入。
async fn read_multipart_fields(
    mut multipart: Multipart,
    max: Option<u64>,
) -> Result<Vec<MultipartField>, ApiError> {
    let mut fields = Vec::new();
    let mut total: u64 = 0;
    loop {
        let field = match multipart.next_field().await {
            Ok(Some(f)) => f,
            Ok(None) => break,
            Err(_) => return Err(ApiError::BadRequest("multipart 解析失败".to_string())),
        };
        let name = field.name().unwrap_or("").to_string();
        let filename = field.file_name().map(str::to_string);
        let bytes = field
            .bytes()
            .await
            .map_err(|_| ApiError::BadRequest("读取 multipart 字段失败".to_string()))?;
        total = total.saturating_add(bytes.len() as u64);
        if let Some(limit) = max {
            if total > limit {
                return Err(ApiError::PayloadTooLarge);
            }
        }
        fields.push(MultipartField {
            name,
            filename,
            bytes: bytes.to_vec(),
        });
    }
    Ok(fields)
}

/// 把 PyPI 协议错误映射为 HTTP 错误。
fn pypi_err_to_api(e: PypiError) -> ApiError {
    match e {
        PypiError::InvalidBody(msg) => ApiError::BadRequest(msg),
        PypiError::DigestMismatch { .. } => {
            // 不回显双方摘要细节给客户端，仅给通用提示（避免噪声，错误已在服务端日志）
            ApiError::BadRequest("上传文件 sha256 摘要与声明不符".to_string())
        }
    }
}
