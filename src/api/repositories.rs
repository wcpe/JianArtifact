//! 仓库管理与浏览端点（FR-06/07/08/09/10/13）。
//!
//! 管理类操作（创建 / 更新 / 删除）仅管理员；读类操作（详情 / 制品浏览）经授权判定，
//! 私有仓库对未授权方一律映射为 404 隐藏存在性（docs/API.md §2 定式）。
//! handler 保持薄：身份解析在中间件、判定在 `authz` 纯函数，本层只做编排与错误映射。

use axum::{
    extract::{Path, State},
    Json,
};
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};

use std::collections::HashMap;

use crate::meta::{ArtifactRecord, RepositoryRecord, Visibility};
use crate::repo::{self, CreateRepoInput, RepoError, UpdateRepoInput};

use super::repo_access::load_readable_repo;
use super::{ApiError, AppState, Identity};

/// 对外仓库视图（字段对齐 docs/API.md）。
#[derive(Debug, Serialize)]
pub struct RepositoryDto {
    /// 仓库主键。
    pub id: String,
    /// 仓库名。
    pub name: String,
    /// 格式（maven | npm | docker | raw | pypi）。
    pub format: String,
    /// 类型（hosted | proxy | group）。
    #[serde(rename = "type")]
    pub r#type: String,
    /// 可见性（public | private）。
    pub visibility: String,
    /// 上游地址（proxy 适用）。
    pub upstream_url: Option<String>,
    /// 成员仓库名（有序，仅 group 适用；非 group 省略）。
    #[serde(skip_serializing_if = "Option::is_none")]
    pub members: Option<Vec<String>>,
    /// 创建时间（ISO8601）。
    pub created_at: String,
    /// 制品索引条目数（不去重，FR-135）。
    pub artifact_count: i64,
    /// 去重 sha256 后的总字节数（同 sha256 只计一次，FR-135）。
    pub total_size: i64,
    /// 仓库状态（FR-135；P1 固定 "active"，后续可扩展为 "disabled" 等）。
    pub status: String,
}

impl From<RepositoryRecord> for RepositoryDto {
    fn from(r: RepositoryRecord) -> Self {
        // 不回显 upstream_auth_ref：它是凭据引用，无须对外暴露。
        // members 由能访问 meta 的 handler 按需填充（本 From 不查库，故默认 None）。
        // 统计字段由 list_repositories 批量聚合后填入，单条查询场景均填 0。
        Self {
            id: r.id,
            name: r.name,
            format: r.format,
            r#type: r.r#type,
            visibility: r.visibility,
            upstream_url: r.upstream_url,
            members: None,
            created_at: r.created_at,
            artifact_count: 0,
            total_size: 0,
            status: "active".to_string(),
        }
    }
}

/// 为 group 仓库 DTO 填充有序成员名（非 group 仓库不查库、不填）。
///
/// 单仓库详情 / 创建 / 更新响应据此回显成员；列表端点为防 N+1 不逐仓库填成员（见 list 注释）。
async fn fill_group_members(
    state: &AppState,
    mut dto: RepositoryDto,
) -> Result<RepositoryDto, ApiError> {
    if dto.r#type == crate::meta::RepoType::Group.as_str() {
        let members = state.meta.list_repo_group_members(&dto.id).await?;
        dto.members = Some(members.into_iter().map(|m| m.name).collect());
    }
    Ok(dto)
}

/// 制品索引视图（字段对齐 docs/API.md 浏览制品）。
#[derive(Debug, Serialize)]
pub struct ArtifactDto {
    /// 制品路径。
    pub path: String,
    /// 字节大小。
    pub size: i64,
    /// sha256 摘要。
    pub sha256: String,
    /// 内容类型。
    pub content_type: Option<String>,
    /// 是否为 proxy 缓存制品。
    pub cached: bool,
    /// 创建时间。
    pub created_at: String,
}

impl From<ArtifactRecord> for ArtifactDto {
    fn from(r: ArtifactRecord) -> Self {
        Self {
            path: r.path,
            size: r.size,
            sha256: r.sha256,
            content_type: r.content_type,
            cached: r.cached != 0,
            created_at: r.created_at,
        }
    }
}

/// 把仓库生命周期错误映射为 HTTP 错误：非法入参 400、重名 409、其余转内部。
impl From<RepoError> for ApiError {
    fn from(e: RepoError) -> Self {
        match e {
            RepoError::Invalid(msg) => ApiError::BadRequest(msg),
            RepoError::NameConflict => ApiError::Conflict("仓库名已存在".to_string()),
            RepoError::Meta(meta) => meta.into(),
        }
    }
}

/// 创建仓库请求体。
#[derive(Debug, Deserialize)]
pub struct CreateRepositoryRequest {
    /// 仓库名。
    pub name: String,
    /// 格式（maven | npm | docker | raw | pypi）。
    pub format: String,
    /// 类型（hosted | proxy | group）。
    #[serde(rename = "type")]
    pub r#type: String,
    /// 可见性（public | private）。
    pub visibility: String,
    /// 上游地址（proxy 适用）。
    #[serde(default)]
    pub upstream_url: Option<String>,
    /// 上游凭据引用（仅引用，真值走配置 / env，不入库明文）。
    #[serde(default)]
    pub upstream_auth_ref: Option<String>,
    /// 成员仓库名（有序，仅 group 适用）。
    #[serde(default)]
    pub members: Option<Vec<String>>,
}

/// 更新仓库请求体：字段可选，仅更新提供的项。
#[derive(Debug, Deserialize)]
pub struct UpdateRepositoryRequest {
    /// 可见性（public | private）。
    #[serde(default)]
    pub visibility: Option<String>,
    /// 上游地址（proxy 适用）。
    #[serde(default)]
    pub upstream_url: Option<String>,
    /// 上游凭据引用。
    #[serde(default)]
    pub upstream_auth_ref: Option<String>,
    /// 成员仓库名（有序，仅 group 适用；提供时整体替换成员列表）。
    #[serde(default)]
    pub members: Option<Vec<String>>,
}

/// 将统计数据合并到 RepositoryDto（内联辅助，避免重复代码）。
fn apply_stats(mut dto: RepositoryDto, stats: &HashMap<String, (i64, i64)>) -> RepositoryDto {
    if let Some(&(count, size)) = stats.get(&dto.id) {
        dto.artifact_count = count;
        dto.total_size = size;
    }
    dto
}

/// 列出仓库：按调用方身份过滤可见仓库（匿名仅见 public）；含每仓统计（FR-135）。
pub async fn list_repositories(
    State(state): State<AppState>,
    identity: Identity,
) -> Result<Json<Vec<RepositoryDto>>, ApiError> {
    let all = state.meta.list_repositories().await?;

    // 批量取统计（一次聚合，避免 N+1），构建 repo_id → (count, size) 映射
    let stat_rows = state.meta.list_repo_stats().await?;
    let stats: HashMap<String, (i64, i64)> = stat_rows
        .into_iter()
        .map(|r| (r.repo_id, (r.artifact_count, r.total_size)))
        .collect();

    // 管理员可见全部；其余按可见性与读 ACL 过滤
    if identity.0.is_admin() {
        return Ok(Json(
            all.into_iter()
                .map(|r| apply_stats(RepositoryDto::from(r), &stats))
                .collect(),
        ));
    }

    // 登录用户：取其有读权限的私有仓库主键集合，避免逐仓库查库（防 N+1）
    let readable_private: std::collections::HashSet<String> = match identity.0.user() {
        Some(u) => state
            .meta
            .list_repo_ids_with_read(&u.user_id)
            .await?
            .into_iter()
            .collect(),
        None => std::collections::HashSet::new(),
    };

    let visible = all
        .into_iter()
        .filter(|r| match Visibility::from_db_str(&r.visibility) {
            // 公开仓库任何人可见
            Visibility::Public => true,
            // 私有仓库仅当登录用户命中读 ACL 才可见
            Visibility::Private => readable_private.contains(&r.id),
        })
        .map(|r| apply_stats(RepositoryDto::from(r), &stats))
        .collect();
    Ok(Json(visible))
}

/// 创建仓库（仅管理员）。业务规则校验与落库下沉到 `repo` 模块。
pub async fn create_repository(
    State(state): State<AppState>,
    identity: Identity,
    Json(req): Json<CreateRepositoryRequest>,
) -> Result<(axum::http::StatusCode, Json<RepositoryDto>), ApiError> {
    identity.require_admin()?;
    let created = repo::create(
        &state.meta,
        CreateRepoInput {
            name: req.name,
            format: req.format,
            r#type: req.r#type,
            visibility: req.visibility,
            upstream_url: req.upstream_url,
            upstream_auth_ref: req.upstream_auth_ref,
            members: req.members,
        },
    )
    .await?;
    let dto = fill_group_members(&state, created.into()).await?;
    Ok((axum::http::StatusCode::CREATED, Json(dto)))
}

/// 获取仓库详情：受读权限约束，无权 private 映射为 404。
pub async fn get_repository(
    State(state): State<AppState>,
    identity: Identity,
    Path(id): Path<String>,
) -> Result<Json<RepositoryDto>, ApiError> {
    let repo = load_readable_repo(&state, &identity, &id).await?;
    let dto = fill_group_members(&state, repo.into()).await?;
    Ok(Json(dto))
}

/// 更新仓库（仅管理员）。业务规则校验与落库下沉到 `repo` 模块。
pub async fn update_repository(
    State(state): State<AppState>,
    identity: Identity,
    Path(id): Path<String>,
    Json(req): Json<UpdateRepositoryRequest>,
) -> Result<Json<RepositoryDto>, ApiError> {
    identity.require_admin()?;
    let updated = repo::update(
        &state.meta,
        &id,
        UpdateRepoInput {
            visibility: req.visibility,
            upstream_url: req.upstream_url,
            upstream_auth_ref: req.upstream_auth_ref,
            members: req.members,
        },
    )
    .await?
    .ok_or(ApiError::NotFound)?;
    let dto = fill_group_members(&state, updated.into()).await?;
    Ok(Json(dto))
}

/// 删除仓库（仅管理员）。
pub async fn delete_repository(
    State(state): State<AppState>,
    identity: Identity,
    Path(id): Path<String>,
) -> Result<Json<Value>, ApiError> {
    identity.require_admin()?;
    if !repo::delete(&state.meta, &id).await? {
        return Err(ApiError::NotFound);
    }
    Ok(Json(json!({ "status": "ok" })))
}

/// 浏览仓库制品索引：受读权限约束，无权 private 映射为 404。
pub async fn list_artifacts(
    State(state): State<AppState>,
    identity: Identity,
    Path(id): Path<String>,
) -> Result<Json<Vec<ArtifactDto>>, ApiError> {
    let repo = load_readable_repo(&state, &identity, &id).await?;
    let artifacts = state.meta.list_artifacts_by_repo(&repo.id).await?;
    Ok(Json(artifacts.into_iter().map(ArtifactDto::from).collect()))
}

/// proxy 仓库连通性测试响应（FR-135，复用 FR-128 结构）。
#[derive(Debug, Serialize)]
pub struct ConnectivityResult {
    /// 是否连通：能收到响应即为 true，连接失败 / 超时为 false。
    pub ok: bool,
    /// HTTP 响应状态码（仅 ok=true 时有值）。
    #[serde(skip_serializing_if = "Option::is_none")]
    pub status: Option<u16>,
    /// 往返耗时（毫秒）。
    pub elapsed_ms: u64,
    /// 失败原因（仅 ok=false 时有值，不含凭据 / upstream URL 明文）。
    #[serde(skip_serializing_if = "Option::is_none")]
    pub error: Option<String>,
}

/// proxy 仓库连通性测试（FR-135，仅 Admin）。
///
/// 取该仓库的 upstream_url，经当前生效出站 client（含代理配置）发 GET；
/// 非 proxy 或无 upstream_url 返回 400；复用 FR-128 的出站客户端与超时机制。
pub async fn test_repo_connectivity(
    State(state): State<AppState>,
    identity: Identity,
    Path(id): Path<String>,
) -> Result<Json<ConnectivityResult>, ApiError> {
    identity.require_admin()?;

    // 取仓库记录（不存在返回 404）
    let repo = state
        .meta
        .get_repository_by_id(&id)
        .await?
        .ok_or(ApiError::NotFound)?;

    // 仅 proxy 且有 upstream_url 才可测试
    let upstream_url = repo.upstream_url.ok_or_else(|| {
        ApiError::BadRequest("该仓库非 proxy 类型或未配置 upstream URL，无法测试连通性".to_string())
    })?;

    // 取当前生效出站 client（含出站代理），读锁极短、锁外发请求
    let client = state.settings.network.client();

    let start = std::time::Instant::now();
    // 带 10s 超时的 GET 请求（与 FR-128 保持一致）
    let result = client
        .get(&upstream_url)
        .timeout(std::time::Duration::from_secs(10))
        .send()
        .await;
    let elapsed_ms = start.elapsed().as_millis() as u64;

    match result {
        Ok(resp) => {
            let status = resp.status().as_u16();
            tracing::info!(
                操作者 = %identity.actor_name(),
                仓库 = %repo.name,
                状态码 = status,
                耗时毫秒 = elapsed_ms,
                "仓库连通性测试成功"
            );
            Ok(Json(ConnectivityResult {
                ok: true,
                status: Some(status),
                elapsed_ms,
                error: None,
            }))
        }
        Err(e) => {
            // 错误描述不含 upstream URL / 凭据明文
            let error_msg = if e.is_timeout() {
                "连接超时".to_string()
            } else if e.is_connect() {
                "连接失败".to_string()
            } else if e.is_builder() {
                "URL 格式非法".to_string()
            } else {
                "请求失败".to_string()
            };
            tracing::info!(
                操作者 = %identity.actor_name(),
                仓库 = %repo.name,
                耗时毫秒 = elapsed_ms,
                "仓库连通性测试失败"
            );
            Ok(Json(ConnectivityResult {
                ok: false,
                status: None,
                elapsed_ms,
                error: Some(error_msg),
            }))
        }
    }
}

#[cfg(test)]
mod tests {
    use super::super::tests::{测试用状态, 读_json};
    use crate::auth::hash_password;
    use crate::meta::{NewArtifact, NewRepository, RepoType, Role, Visibility};
    use axum::body::Body;
    use axum::http::{Request, StatusCode};
    use tower::ServiceExt;

    use super::super::build_router;

    /// 建管理员并签发令牌。
    async fn 建管理员(state: &super::AppState) -> String {
        let uid = state
            .meta
            .create_user("admin-repo", &hash_password("pw").unwrap(), Role::Admin)
            .await
            .unwrap();
        state.jwt.issue(&uid, "admin-repo", Role::Admin).unwrap()
    }

    /// 建普通用户并签发令牌。
    async fn 建普通用户(state: &super::AppState) -> String {
        let uid = state
            .meta
            .create_user("user-repo", &hash_password("pw").unwrap(), Role::User)
            .await
            .unwrap();
        state.jwt.issue(&uid, "user-repo", Role::User).unwrap()
    }

    /// 建 proxy 仓库，返回仓库 id。
    async fn 建proxy仓库(state: &super::AppState, upstream: &str) -> String {
        state
            .meta
            .create_repository(NewRepository {
                name: "proxy-test",
                format: "raw",
                r#type: RepoType::Proxy,
                visibility: Visibility::Public,
                upstream_url: Some(upstream),
                upstream_auth_ref: None,
            })
            .await
            .unwrap()
    }

    /// 建 hosted 仓库（无 upstream），返回仓库 id。
    async fn 建hosted仓库(state: &super::AppState) -> String {
        state
            .meta
            .create_repository(NewRepository {
                name: "hosted-test",
                format: "raw",
                r#type: RepoType::Hosted,
                visibility: Visibility::Public,
                upstream_url: None,
                upstream_auth_ref: None,
            })
            .await
            .unwrap()
    }

    // ===== 统计字段测试 =====

    #[tokio::test]
    async fn 列表_包含统计字段_无制品时为零() {
        let (state, _dir) = 测试用状态().await;
        let token = 建管理员(&state).await;
        state
            .meta
            .create_repository(NewRepository {
                name: "empty-repo",
                format: "raw",
                r#type: RepoType::Hosted,
                visibility: Visibility::Public,
                upstream_url: None,
                upstream_auth_ref: None,
            })
            .await
            .unwrap();

        let app = build_router(state);
        let resp = app
            .oneshot(
                Request::builder()
                    .uri("/api/v1/repositories")
                    .header("Authorization", format!("Bearer {token}"))
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(resp.status(), StatusCode::OK);
        let body = 读_json(resp).await;
        let repos = body.as_array().unwrap();
        assert_eq!(repos.len(), 1);
        assert_eq!(repos[0]["artifact_count"], 0);
        assert_eq!(repos[0]["total_size"], 0);
        assert_eq!(repos[0]["status"], "active");
    }

    #[tokio::test]
    async fn 列表_统计字段含制品数与去重字节() {
        let (state, _dir) = 测试用状态().await;
        let token = 建管理员(&state).await;
        let rid = state
            .meta
            .create_repository(NewRepository {
                name: "r1",
                format: "raw",
                r#type: RepoType::Hosted,
                visibility: Visibility::Public,
                upstream_url: None,
                upstream_auth_ref: None,
            })
            .await
            .unwrap();

        // 写两条制品：同 sha256（50 bytes）+ 不同 sha256（30 bytes）
        state
            .meta
            .upsert_artifact(NewArtifact {
                repo_id: &rid,
                path: "a.bin",
                size: 50,
                sha256: "shaA",
                sha1: "s1",
                md5: "m",
                sha512: "s5",
                content_type: None,
                cached: false,
            })
            .await
            .unwrap();
        state
            .meta
            .upsert_artifact(NewArtifact {
                repo_id: &rid,
                path: "b.bin",
                size: 50,
                sha256: "shaA",
                sha1: "s1",
                md5: "m",
                sha512: "s5",
                content_type: None,
                cached: false,
            })
            .await
            .unwrap();
        state
            .meta
            .upsert_artifact(NewArtifact {
                repo_id: &rid,
                path: "c.bin",
                size: 30,
                sha256: "shaB",
                sha1: "s1",
                md5: "m",
                sha512: "s5",
                content_type: None,
                cached: false,
            })
            .await
            .unwrap();

        let app = build_router(state);
        let resp = app
            .oneshot(
                Request::builder()
                    .uri("/api/v1/repositories")
                    .header("Authorization", format!("Bearer {token}"))
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(resp.status(), StatusCode::OK);
        let body = 读_json(resp).await;
        let repos = body.as_array().unwrap();
        assert_eq!(repos.len(), 1);
        // 3 条制品索引
        assert_eq!(repos[0]["artifact_count"], 3);
        // 去重 sha256：shaA(50) + shaB(30) = 80
        assert_eq!(repos[0]["total_size"], 80);
    }

    // ===== 连通性测试端点鉴权 =====

    #[tokio::test]
    async fn 连通性测试_非管理员返回_403() {
        let (state, _dir) = 测试用状态().await;
        let token = 建普通用户(&state).await;
        let rid = 建proxy仓库(&state, "https://example.com").await;

        let app = build_router(state);
        let resp = app
            .oneshot(
                Request::builder()
                    .method("POST")
                    .uri(format!("/api/v1/repositories/{rid}/test-connectivity"))
                    .header("Authorization", format!("Bearer {token}"))
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(resp.status(), StatusCode::FORBIDDEN);
    }

    #[tokio::test]
    async fn 连通性测试_未认证返回_401() {
        let (state, _dir) = 测试用状态().await;
        let rid = 建proxy仓库(&state, "https://example.com").await;

        let app = build_router(state);
        let resp = app
            .oneshot(
                Request::builder()
                    .method("POST")
                    .uri(format!("/api/v1/repositories/{rid}/test-connectivity"))
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(resp.status(), StatusCode::UNAUTHORIZED);
    }

    #[tokio::test]
    async fn 连通性测试_仓库不存在返回_404() {
        let (state, _dir) = 测试用状态().await;
        let token = 建管理员(&state).await;

        let app = build_router(state);
        let resp = app
            .oneshot(
                Request::builder()
                    .method("POST")
                    .uri("/api/v1/repositories/不存在的仓库id/test-connectivity")
                    .header("Authorization", format!("Bearer {token}"))
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(resp.status(), StatusCode::NOT_FOUND);
    }

    #[tokio::test]
    async fn 连通性测试_hosted仓库无upstream返回_400() {
        let (state, _dir) = 测试用状态().await;
        let token = 建管理员(&state).await;
        let rid = 建hosted仓库(&state).await;

        let app = build_router(state);
        let resp = app
            .oneshot(
                Request::builder()
                    .method("POST")
                    .uri(format!("/api/v1/repositories/{rid}/test-connectivity"))
                    .header("Authorization", format!("Bearer {token}"))
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(resp.status(), StatusCode::BAD_REQUEST);
    }

    #[tokio::test]
    async fn 连通性测试_不可达上游返回_ok_false_不_panic() {
        let (state, _dir) = 测试用状态().await;
        let token = 建管理员(&state).await;
        // 用 127.0.0.2 上的不可能存在的端口，保证连接失败
        let rid = 建proxy仓库(&state, "http://127.0.0.2:19999").await;

        let app = build_router(state);
        let resp = app
            .oneshot(
                Request::builder()
                    .method("POST")
                    .uri(format!("/api/v1/repositories/{rid}/test-connectivity"))
                    .header("Authorization", format!("Bearer {token}"))
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        // 连接失败时应返回 200（业务成功），响应体 ok=false
        assert_eq!(resp.status(), StatusCode::OK);
        let body = 读_json(resp).await;
        assert_eq!(body["ok"], false);
        assert!(body["error"].is_string());
    }
}
