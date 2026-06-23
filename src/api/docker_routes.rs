//! Docker Registry v2 / OCI Distribution 格式 API（FR-16）：挂载于 `/v2/`。
//!
//! handler 保持薄：只做协议适配（路径解析、状态码与头组装、错误→registry v2 错误体）、
//! 认证 / 鉴权编排，存储与状态机下沉到 `format::DockerRegistry`。
//!
//! 鉴权要点（testing-and-quality §2.1）：
//! - **未认证访问受保护资源 → 401 + `WWW-Authenticate`**（docker 客户端据此带凭据重试）。
//! - **已认证但无权**：按既有 authz——无读权限的 private 返回 404 隐藏存在性；有读无写返回 403。
//! - `{name}` 形如 `{仓库}/{镜像}`：首段为 JianArtifact 仓库名，其余为镜像名（可多段）。

use axum::{
    body::Body,
    extract::{Path, Query, State},
    http::{header, HeaderValue, StatusCode},
    response::{IntoResponse, Response},
    Json,
};
use futures_util::TryStreamExt;
use serde::Deserialize;
use serde_json::json;
use tokio_util::io::{ReaderStream, StreamReader};

use crate::authz::{authorize, Action, Decision};
use crate::format::docker;
use crate::format::DockerError;
use crate::meta::RepositoryRecord;

use super::repo_access::build_repo_view;
use super::{AppState, Identity};

/// Docker Distribution API 版本头名。
const API_VERSION_HEADER: &str = "Docker-Distribution-Api-Version";
/// Docker Distribution API 版本值。
const API_VERSION_VALUE: &str = "registry/2.0";
/// manifest digest 响应头名。
const CONTENT_DIGEST_HEADER: &str = "Docker-Content-Digest";

/// PUT blob 完成时的 digest 查询参数。
#[derive(Debug, Deserialize)]
pub struct DigestQuery {
    /// 客户端声明的 blob digest（`sha256:{hex}`）。
    digest: Option<String>,
}

/// docker 协议错误：转为 registry v2 规范错误体并附带状态码。
///
/// 不复用管理 API 的统一 JSON 错误结构（registry v2 客户端按其自有 errors 数组解析）。
struct DockerApiError {
    /// HTTP 状态码。
    status: StatusCode,
    /// registry v2 错误码（如 `BLOB_UNKNOWN` / `MANIFEST_UNKNOWN` / `UNAUTHORIZED`）。
    code: &'static str,
    /// 面向客户端的可读说明。
    message: String,
    /// 是否需要 `WWW-Authenticate` 头（未认证访问受保护资源时为真）。
    需要认证: bool,
}

impl DockerApiError {
    fn new(status: StatusCode, code: &'static str, message: impl Into<String>) -> Self {
        Self {
            status,
            code,
            message: message.into(),
            需要认证: false,
        }
    }

    /// 未认证访问受保护资源：401 + WWW-Authenticate，引导 docker 客户端带凭据重试。
    fn unauthorized() -> Self {
        Self {
            status: StatusCode::UNAUTHORIZED,
            code: "UNAUTHORIZED",
            message: "需要认证".to_string(),
            需要认证: true,
        }
    }

    fn not_found(code: &'static str) -> Self {
        Self::new(StatusCode::NOT_FOUND, code, "资源不存在")
    }
}

impl IntoResponse for DockerApiError {
    fn into_response(self) -> Response {
        let body = Json(json!({
            "errors": [{ "code": self.code, "message": self.message }]
        }));
        let mut resp = (self.status, body).into_response();
        resp.headers_mut().insert(
            API_VERSION_HEADER,
            HeaderValue::from_static(API_VERSION_VALUE),
        );
        if self.需要认证 {
            // Basic 认证质询：docker 客户端据此用 docker login 的凭据重试
            resp.headers_mut().insert(
                header::WWW_AUTHENTICATE,
                HeaderValue::from_static("Basic realm=\"JianArtifact\""),
            );
        }
        resp
    }
}

/// 把 DockerRegistry 存储错误映射为 registry v2 协议错误。
fn map_docker_error(e: DockerError) -> DockerApiError {
    match e {
        DockerError::NotFound => DockerApiError::not_found("BLOB_UNKNOWN"),
        DockerError::UnknownUpload => {
            DockerApiError::not_found("BLOB_UPLOAD_UNKNOWN")
        }
        DockerError::DigestMismatch => {
            DockerApiError::new(StatusCode::BAD_REQUEST, "DIGEST_INVALID", "digest 与内容不匹配")
        }
        DockerError::InvalidDigest => {
            DockerApiError::new(StatusCode::BAD_REQUEST, "DIGEST_INVALID", "digest 格式非法")
        }
        DockerError::UnsupportedMediaType => DockerApiError::new(
            StatusCode::BAD_REQUEST,
            "MANIFEST_INVALID",
            "manifest 媒体类型不受支持",
        ),
        DockerError::TooLarge => DockerApiError::new(
            StatusCode::PAYLOAD_TOO_LARGE,
            "BLOB_UPLOAD_INVALID",
            "上传体积超过上限",
        ),
        DockerError::Storage(err) => {
            tracing::error!(错误 = %err, "docker blob 存储访问失败");
            DockerApiError::new(StatusCode::INTERNAL_SERVER_ERROR, "UNKNOWN", "内部错误")
        }
        DockerError::Meta(err) => {
            tracing::error!(错误 = %err, "docker 元数据访问失败");
            DockerApiError::new(StatusCode::INTERNAL_SERVER_ERROR, "UNKNOWN", "内部错误")
        }
    }
}

/// 版本检查（`GET /v2/`）：返回 200 与版本头，供 docker 客户端探活。
pub async fn version_check() -> Response {
    let mut resp = StatusCode::OK.into_response();
    resp.headers_mut().insert(
        API_VERSION_HEADER,
        HeaderValue::from_static(API_VERSION_VALUE),
    );
    resp
}

/// 把 docker `{name}` 拆为 `(JianArtifact 仓库名, 镜像名)`。
///
/// 首段为仓库名，其余为镜像名（docker 镜像名可多段，如 `library/alpine`）。
/// 仅一段时视为缺镜像名，返回 None。
fn split_name(name: &str) -> Option<(String, String)> {
    let (repo, image) = name.split_once('/')?;
    if repo.is_empty() || image.is_empty() {
        return None;
    }
    Some((repo.to_string(), image.to_string()))
}

/// 解析仓库并施加读授权（docker 语义）。
///
/// - 仓库不存在：匿名 → 401（引导带凭据，不暴露不存在）；已认证 → 404。
/// - 有读权限：放行。
/// - 无读权限：匿名 → 401（带 WWW-Authenticate）；已认证 → 404（隐藏存在性）。
async fn load_readable_repo(
    state: &AppState,
    identity: &Identity,
    repo_name: &str,
) -> Result<RepositoryRecord, DockerApiError> {
    let repo = match state.meta.get_repository_by_name(repo_name).await {
        Ok(Some(r)) => r,
        Ok(None) => return Err(deny_read(identity)),
        Err(e) => return Err(map_docker_error(DockerError::Meta(e))),
    };
    let view = build_repo_view(state, identity, &repo)
        .await
        .map_err(|_| DockerApiError::new(StatusCode::INTERNAL_SERVER_ERROR, "UNKNOWN", "内部错误"))?;
    match authorize(&identity.0, &view, Action::Read) {
        Decision::Allow => Ok(repo),
        Decision::Deny => Err(deny_read(identity)),
    }
}

/// 解析仓库并施加写授权（docker 语义）。
///
/// - 未认证：一律 401 + WWW-Authenticate（写必须认证）。
/// - 已认证：无读权限 → 404（隐藏存在性）；有读无写 → 403；有写 → 放行。
async fn load_writable_repo(
    state: &AppState,
    identity: &Identity,
    repo_name: &str,
) -> Result<RepositoryRecord, DockerApiError> {
    // 写必须认证：匿名直接 401 引导登录
    if !identity.0.is_authenticated() {
        return Err(DockerApiError::unauthorized());
    }
    let repo = match state.meta.get_repository_by_name(repo_name).await {
        Ok(Some(r)) => r,
        // 已认证但仓库不存在 → 404
        Ok(None) => return Err(DockerApiError::not_found("NAME_UNKNOWN")),
        Err(e) => return Err(map_docker_error(DockerError::Meta(e))),
    };
    let view = build_repo_view(state, identity, &repo)
        .await
        .map_err(|_| DockerApiError::new(StatusCode::INTERNAL_SERVER_ERROR, "UNKNOWN", "内部错误"))?;
    // 先过读判定：无读权限（含登录无 ACL 访问 private）→ 404 隐藏存在性
    if authorize(&identity.0, &view, Action::Read) == Decision::Deny {
        return Err(DockerApiError::not_found("NAME_UNKNOWN"));
    }
    match authorize(&identity.0, &view, Action::Write) {
        Decision::Allow => Ok(repo),
        Decision::Deny => Err(DockerApiError::new(
            StatusCode::FORBIDDEN,
            "DENIED",
            "无写权限",
        )),
    }
}

/// 读拒绝的状态映射：匿名 → 401 引导认证；已认证 → 404 隐藏存在性。
fn deny_read(identity: &Identity) -> DockerApiError {
    if identity.0.is_authenticated() {
        DockerApiError::not_found("NAME_UNKNOWN")
    } else {
        DockerApiError::unauthorized()
    }
}

/// 校验仓库格式为 docker，否则 404（该路由仅服务 docker 仓库）。
fn ensure_docker(repo: &RepositoryRecord) -> Result<(), DockerApiError> {
    if repo.format == "docker" {
        Ok(())
    } else {
        Err(DockerApiError::not_found("NAME_UNKNOWN"))
    }
}

/// 取 docker registry 服务句柄（启动时必装配；缺失视为内部错误）。
fn registry(state: &AppState) -> Result<&super::AppDockerRegistry, DockerApiError> {
    state.docker.as_deref().ok_or_else(|| {
        tracing::error!("docker registry 未装配");
        DockerApiError::new(StatusCode::INTERNAL_SERVER_ERROR, "UNKNOWN", "内部错误")
    })
}

/// 把对外基础地址（去 scheme）与路径拼成 Location 头值。
fn location(state: &AppState, path: &str) -> String {
    let base = state
        .config
        .server
        .public_base_url
        .as_deref()
        .map(|u| u.trim_end_matches('/').to_string())
        .unwrap_or_default();
    format!("{base}{path}")
}

/// `/v2/` 之后的路径解析结果：把 `{name}/{后缀}` 归类为具体的 registry 操作。
///
/// docker 的 `{name}` 可含多段（如 `repo/library/alpine`），而后缀模式固定
/// （`/blobs/uploads/...` / `/blobs/{digest}` / `/manifests/{ref}`），故从右侧的后缀标记切分。
enum V2Route {
    /// 启动 blob 上传：`{name}/blobs/uploads/`。
    StartUpload { name: String },
    /// 续传 / 完成 blob 上传：`{name}/blobs/uploads/{uuid}`。
    Upload { name: String, uuid: String },
    /// blob 读取：`{name}/blobs/{digest}`。
    Blob { name: String, digest: String },
    /// manifest 存取：`{name}/manifests/{reference}`。
    Manifest { name: String, reference: String },
    /// tag 列表：`{name}/tags/list`。
    TagsList { name: String },
}

/// 解析 `/v2/` 之后的相对路径为 [`V2Route`]；无法识别返回 None。
fn parse_v2_route(rest: &str) -> Option<V2Route> {
    // tag 列表：以 `/tags/list` 结尾
    if let Some(name) = rest.strip_suffix("/tags/list") {
        return non_empty(name).map(|n| V2Route::TagsList { name: n });
    }
    // 启动上传：以 `/blobs/uploads/` 结尾（uuid 为空）
    if let Some(name) = rest.strip_suffix("/blobs/uploads/") {
        return non_empty(name).map(|n| V2Route::StartUpload { name: n });
    }
    // 续传 / 完成上传：含 `/blobs/uploads/{uuid}`
    if let Some((name, uuid)) = split_marker(rest, "/blobs/uploads/") {
        return Some(V2Route::Upload { name, uuid });
    }
    // blob 读取：含 `/blobs/{digest}`
    if let Some((name, digest)) = split_marker(rest, "/blobs/") {
        return Some(V2Route::Blob { name, digest });
    }
    // manifest 存取：含 `/manifests/{reference}`
    if let Some((name, reference)) = split_marker(rest, "/manifests/") {
        return Some(V2Route::Manifest { name, reference });
    }
    None
}

/// 按后缀标记切分为 `(标记前 name, 标记后 tail)`，两侧均非空才成立。
fn split_marker(rest: &str, marker: &str) -> Option<(String, String)> {
    let (name, tail) = rest.split_once(marker)?;
    if name.is_empty() || tail.is_empty() {
        return None;
    }
    Some((name.to_string(), tail.to_string()))
}

/// 非空字符串过滤工具。
fn non_empty(s: &str) -> Option<String> {
    if s.is_empty() {
        None
    } else {
        Some(s.to_string())
    }
}

// ---------------- 方法分发器（catch-all `/v2/{*path}` 按方法路由） ----------------

/// POST 分发：仅 `{name}/blobs/uploads/` 合法（启动上传）。
pub async fn dispatch_post(
    state: State<AppState>,
    identity: Identity,
    Path(rest): Path<String>,
    q: Query<DigestQuery>,
    body: Body,
) -> Response {
    match parse_v2_route(&rest) {
        Some(V2Route::StartUpload { name }) => {
            start_blob_upload(state, identity, name, q, body).await
        }
        _ => DockerApiError::not_found("NAME_UNKNOWN").into_response(),
    }
}

/// PATCH 分发：仅 `{name}/blobs/uploads/{uuid}` 合法（续传）。
pub async fn dispatch_patch(
    state: State<AppState>,
    identity: Identity,
    Path(rest): Path<String>,
    body: Body,
) -> Response {
    match parse_v2_route(&rest) {
        Some(V2Route::Upload { name, uuid }) => {
            patch_blob_upload(state, identity, name, uuid, body).await
        }
        _ => DockerApiError::not_found("NAME_UNKNOWN").into_response(),
    }
}

/// PUT 分发：`{name}/blobs/uploads/{uuid}`（完成上传）或 `{name}/manifests/{ref}`（写 manifest）。
pub async fn dispatch_put(
    state: State<AppState>,
    identity: Identity,
    Path(rest): Path<String>,
    q: Query<DigestQuery>,
    headers: axum::http::HeaderMap,
    body: axum::body::Bytes,
) -> Response {
    match parse_v2_route(&rest) {
        Some(V2Route::Upload { name, uuid }) => {
            // 完成上传需把已读 body 作为末段；这里 body 已聚合为 Bytes（manifest 与完成上传共用 PUT）
            put_blob_upload(state, identity, name, uuid, q, body).await
        }
        Some(V2Route::Manifest { name, reference }) => {
            put_manifest(state, identity, name, reference, headers, body).await
        }
        _ => DockerApiError::not_found("NAME_UNKNOWN").into_response(),
    }
}

/// GET 分发：`/v2/`（版本检查由专门路由处理）、blob 或 manifest 读取。
pub async fn dispatch_get(
    state: State<AppState>,
    identity: Identity,
    Path(rest): Path<String>,
) -> Response {
    match parse_v2_route(&rest) {
        Some(V2Route::Blob { name, digest }) => {
            blob_request(state.0, identity, name, digest, true).await
        }
        Some(V2Route::Manifest { name, reference }) => {
            manifest_request(state.0, identity, name, reference, true).await
        }
        Some(V2Route::TagsList { name }) => tags_list(state.0, identity, name).await,
        _ => DockerApiError::not_found("NAME_UNKNOWN").into_response(),
    }
}

/// HEAD 分发：blob 或 manifest 存在性检查。
pub async fn dispatch_head(
    state: State<AppState>,
    identity: Identity,
    Path(rest): Path<String>,
) -> Response {
    match parse_v2_route(&rest) {
        Some(V2Route::Blob { name, digest }) => {
            blob_request(state.0, identity, name, digest, false).await
        }
        Some(V2Route::Manifest { name, reference }) => {
            manifest_request(state.0, identity, name, reference, false).await
        }
        _ => DockerApiError::not_found("NAME_UNKNOWN").into_response(),
    }
}

/// tag 列表（`GET /v2/{name}/tags/list`）：返回该镜像在本仓库下的全部 tag。
///
/// 经读授权（private 对无权 → 404/401，与 manifest 读一致）；tag 取自存储中
/// `{image}/tags/{tag}` 指针索引。无任何 tag 视为名称未知，返回 404 NAME_UNKNOWN。
async fn tags_list(state: AppState, identity: Identity, name: String) -> Response {
    let (repo_name, image) = match split_name(&name) {
        Some(v) => v,
        None => return DockerApiError::not_found("NAME_UNKNOWN").into_response(),
    };
    let repo = match load_readable_repo(&state, &identity, &repo_name).await {
        Ok(r) => r,
        Err(e) => return e.into_response(),
    };
    if let Err(e) = ensure_docker(&repo) {
        return e.into_response();
    }
    let arts = match state.meta.list_artifacts_by_repo(&repo.id).await {
        Ok(a) => a,
        Err(e) => return map_docker_error(DockerError::Meta(e)).into_response(),
    };
    // tag 指针存储键形如 `{image}/tags/{tag}`：按前缀筛出本镜像的 tag。
    let prefix = format!("{image}/tags/");
    let mut tags: Vec<String> = arts
        .iter()
        .filter_map(|a| a.path.strip_prefix(&prefix))
        .filter(|t| !t.is_empty() && !t.contains('/'))
        .map(|t| t.to_string())
        .collect();
    tags.sort();
    tags.dedup();
    if tags.is_empty() {
        return DockerApiError::not_found("NAME_UNKNOWN").into_response();
    }
    let mut resp = Json(json!({ "name": name, "tags": tags })).into_response();
    resp.headers_mut().insert(
        API_VERSION_HEADER,
        HeaderValue::from_static(API_VERSION_VALUE),
    );
    resp
}

// ---------------- blob 上传状态机 ----------------

/// 启动 blob 上传（`POST /v2/{name}/blobs/uploads/`）：返回 202 + Location（含 uuid）。
async fn start_blob_upload(
    State(state): State<AppState>,
    identity: Identity,
    name: String,
    Query(q): Query<DigestQuery>,
    body: Body,
) -> Response {
    let (repo_name, image) = match split_name(&name) {
        Some(v) => v,
        None => return DockerApiError::not_found("NAME_UNKNOWN").into_response(),
    };
    let repo = match load_writable_repo(&state, &identity, &repo_name).await {
        Ok(r) => r,
        Err(e) => return e.into_response(),
    };
    if let Err(e) = ensure_docker(&repo) {
        return e.into_response();
    }
    let reg = match registry(&state) {
        Ok(r) => r,
        Err(e) => return e.into_response(),
    };

    let started = match reg.start_upload().await {
        Ok(s) => s,
        Err(e) => return map_docker_error(e).into_response(),
    };

    // 单体上传：POST 直接带 digest 与 body（POST-then-PUT 的合并形态）
    if let Some(digest) = q.digest {
        let reader = StreamReader::new(
            body.into_data_stream()
                .map_err(|e| std::io::Error::other(e.to_string())),
        );
        if let Err(e) = reg.append_upload(&started.upload_id, reader).await {
            reg.cancel_upload(&started.upload_id).await;
            return map_docker_error(e).into_response();
        }
        return finalize_blob(&state, &repo, &image, &started.upload_id, &digest).await;
    }

    // 分块上传：返回 202 + Location，客户端后续 PATCH / PUT
    let loc = location(
        &state,
        &format!("/v2/{repo_name}/{image}/blobs/uploads/{}", started.upload_id),
    );
    upload_accepted(&loc, &started.upload_id, 0)
}

/// 追加 blob 分块（`PATCH /v2/{name}/blobs/uploads/{uuid}`）：流式写入，返回 202 + Range。
async fn patch_blob_upload(
    State(state): State<AppState>,
    identity: Identity,
    name: String,
    uuid: String,
    body: Body,
) -> Response {
    let (repo_name, image) = match split_name(&name) {
        Some(v) => v,
        None => return DockerApiError::not_found("NAME_UNKNOWN").into_response(),
    };
    let repo = match load_writable_repo(&state, &identity, &repo_name).await {
        Ok(r) => r,
        Err(e) => return e.into_response(),
    };
    if let Err(e) = ensure_docker(&repo) {
        return e.into_response();
    }
    let reg = match registry(&state) {
        Ok(r) => r,
        Err(e) => return e.into_response(),
    };

    let reader = StreamReader::new(
        body.into_data_stream()
            .map_err(|e| std::io::Error::other(e.to_string())),
    );
    let outcome = match reg.append_upload(&uuid, reader).await {
        Ok(o) => o,
        Err(e) => {
            // 超限等错误：取消会话清理临时文件
            if matches!(e, DockerError::TooLarge) {
                reg.cancel_upload(&uuid).await;
            }
            return map_docker_error(e).into_response();
        }
    };

    let loc = location(&state, &format!("/v2/{repo_name}/{image}/blobs/uploads/{uuid}"));
    upload_accepted(&loc, &uuid, outcome.written)
}

/// 完成 blob 上传（`PUT /v2/{name}/blobs/uploads/{uuid}?digest=...`）：可携末段 body。
async fn put_blob_upload(
    State(state): State<AppState>,
    identity: Identity,
    name: String,
    uuid: String,
    Query(q): Query<DigestQuery>,
    body: axum::body::Bytes,
) -> Response {
    let (repo_name, image) = match split_name(&name) {
        Some(v) => v,
        None => return DockerApiError::not_found("NAME_UNKNOWN").into_response(),
    };
    let repo = match load_writable_repo(&state, &identity, &repo_name).await {
        Ok(r) => r,
        Err(e) => return e.into_response(),
    };
    if let Err(e) = ensure_docker(&repo) {
        return e.into_response();
    }
    let reg = match registry(&state) {
        Ok(r) => r,
        Err(e) => return e.into_response(),
    };

    let digest = match q.digest {
        Some(d) => d,
        None => {
            return DockerApiError::new(
                StatusCode::BAD_REQUEST,
                "DIGEST_INVALID",
                "完成上传需提供 digest",
            )
            .into_response()
        }
    };

    // PUT 可能携带最后一段字节（先追加再完成）；末段通常很小或为空
    if !body.is_empty() {
        let reader = std::io::Cursor::new(body);
        if let Err(e) = reg.append_upload(&uuid, reader).await {
            if matches!(e, DockerError::TooLarge) {
                reg.cancel_upload(&uuid).await;
            }
            return map_docker_error(e).into_response();
        }
    }

    finalize_blob(&state, &repo, &image, &uuid, &digest).await
}

/// 完成上传并组装 201 响应（含 Location 与 Docker-Content-Digest）。
async fn finalize_blob(
    state: &AppState,
    repo: &RepositoryRecord,
    image: &str,
    upload_id: &str,
    digest: &str,
) -> Response {
    let reg = match registry(state) {
        Ok(r) => r,
        Err(e) => return e.into_response(),
    };
    match reg.finish_upload(repo, image, upload_id, digest).await {
        Ok(final_digest) => {
            let loc = location(
                state,
                &format!("/v2/{}/{}/blobs/{}", repo.name, image, final_digest),
            );
            let mut resp = StatusCode::CREATED.into_response();
            let h = resp.headers_mut();
            insert_api_version(h);
            if let Ok(v) = HeaderValue::from_str(&loc) {
                h.insert(header::LOCATION, v);
            }
            if let Ok(v) = HeaderValue::from_str(&final_digest) {
                h.insert(CONTENT_DIGEST_HEADER, v);
            }
            resp
        }
        Err(e) => map_docker_error(e).into_response(),
    }
}

/// 组装上传进行中的 202 响应（Location + Range + Upload-UUID）。
fn upload_accepted(location: &str, uuid: &str, written: u64) -> Response {
    let mut resp = StatusCode::ACCEPTED.into_response();
    let h = resp.headers_mut();
    insert_api_version(h);
    if let Ok(v) = HeaderValue::from_str(location) {
        h.insert(header::LOCATION, v);
    }
    if let Ok(v) = HeaderValue::from_str(uuid) {
        h.insert("Docker-Upload-UUID", v);
    }
    // Range 表示已接收字节区间（0-N），N 为最后一个已写字节的偏移
    let range = if written == 0 {
        "0-0".to_string()
    } else {
        format!("0-{}", written - 1)
    };
    if let Ok(v) = HeaderValue::from_str(&range) {
        h.insert(header::RANGE, v);
    }
    resp
}

// ---------------- blob 读取 ----------------

/// blob 读取公共流程：`with_body` 区分 GET（带体）与 HEAD（仅头）。
async fn blob_request(
    state: AppState,
    identity: Identity,
    name: String,
    digest: String,
    with_body: bool,
) -> Response {
    let (repo_name, image) = match split_name(&name) {
        Some(v) => v,
        None => return DockerApiError::not_found("NAME_UNKNOWN").into_response(),
    };
    let repo = match load_readable_repo(&state, &identity, &repo_name).await {
        Ok(r) => r,
        Err(e) => return e.into_response(),
    };
    if let Err(e) = ensure_docker(&repo) {
        return e.into_response();
    }
    let reg = match registry(&state) {
        Ok(r) => r,
        Err(e) => return e.into_response(),
    };

    let handle = match reg.get_blob(&repo, &image, &digest).await {
        Ok(h) => h,
        Err(e) => return map_docker_error(e).into_response(),
    };

    let body = if with_body {
        Body::from_stream(ReaderStream::new(handle.blob))
    } else {
        Body::empty()
    };
    Response::builder()
        .status(StatusCode::OK)
        .header(API_VERSION_HEADER, API_VERSION_VALUE)
        .header(CONTENT_DIGEST_HEADER, handle.digest)
        .header(header::CONTENT_TYPE, "application/octet-stream")
        .header(header::CONTENT_LENGTH, handle.size)
        .body(body)
        .unwrap_or_else(|_| StatusCode::INTERNAL_SERVER_ERROR.into_response())
}

// ---------------- manifest 存取 ----------------

/// PUT manifest（`PUT /v2/{name}/manifests/{reference}`）：写入并返回 201 + digest 头。
async fn put_manifest(
    State(state): State<AppState>,
    identity: Identity,
    name: String,
    reference: String,
    headers: axum::http::HeaderMap,
    body: axum::body::Bytes,
) -> Response {
    let (repo_name, image) = match split_name(&name) {
        Some(v) => v,
        None => return DockerApiError::not_found("NAME_UNKNOWN").into_response(),
    };
    let repo = match load_writable_repo(&state, &identity, &repo_name).await {
        Ok(r) => r,
        Err(e) => return e.into_response(),
    };
    if let Err(e) = ensure_docker(&repo) {
        return e.into_response();
    }
    let reg = match registry(&state) {
        Ok(r) => r,
        Err(e) => return e.into_response(),
    };

    // 媒体类型取自 Content-Type 头（docker push 必带），缺失则用默认 schema2
    let media_type = headers
        .get(header::CONTENT_TYPE)
        .and_then(|v| v.to_str().ok())
        .map(str::to_string)
        .unwrap_or_else(|| docker::MEDIA_TYPE_MANIFEST_V2.to_string());

    match reg
        .put_manifest(&repo, &image, &reference, &media_type, body.to_vec())
        .await
    {
        Ok(digest) => {
            let loc = location(
                &state,
                &format!("/v2/{repo_name}/{image}/manifests/{reference}"),
            );
            let mut resp = StatusCode::CREATED.into_response();
            let h = resp.headers_mut();
            insert_api_version(h);
            if let Ok(v) = HeaderValue::from_str(&loc) {
                h.insert(header::LOCATION, v);
            }
            if let Ok(v) = HeaderValue::from_str(&digest) {
                h.insert(CONTENT_DIGEST_HEADER, v);
            }
            resp
        }
        Err(e) => map_docker_error(e).into_response(),
    }
}

/// manifest 读取公共流程：`with_body` 区分 GET 与 HEAD。
async fn manifest_request(
    state: AppState,
    identity: Identity,
    name: String,
    reference: String,
    with_body: bool,
) -> Response {
    let (repo_name, image) = match split_name(&name) {
        Some(v) => v,
        None => return DockerApiError::not_found("NAME_UNKNOWN").into_response(),
    };
    let repo = match load_readable_repo(&state, &identity, &repo_name).await {
        Ok(r) => r,
        Err(e) => return e.into_response(),
    };
    if let Err(e) = ensure_docker(&repo) {
        return e.into_response();
    }
    let reg = match registry(&state) {
        Ok(r) => r,
        Err(e) => return e.into_response(),
    };

    let handle = match reg.get_manifest(&repo, &image, &reference).await {
        Ok(h) => h,
        Err(DockerError::NotFound) => {
            return DockerApiError::not_found("MANIFEST_UNKNOWN").into_response()
        }
        Err(e) => return map_docker_error(e).into_response(),
    };

    let len = handle.bytes.len();
    let body = if with_body {
        Body::from(handle.bytes)
    } else {
        Body::empty()
    };
    Response::builder()
        .status(StatusCode::OK)
        .header(API_VERSION_HEADER, API_VERSION_VALUE)
        .header(CONTENT_DIGEST_HEADER, handle.digest)
        .header(header::CONTENT_TYPE, handle.media_type)
        .header(header::CONTENT_LENGTH, len)
        .body(body)
        .unwrap_or_else(|_| StatusCode::INTERNAL_SERVER_ERROR.into_response())
}

/// 插入 API 版本头。
fn insert_api_version(headers: &mut axum::http::HeaderMap) {
    headers.insert(
        API_VERSION_HEADER,
        HeaderValue::from_static(API_VERSION_VALUE),
    );
}
