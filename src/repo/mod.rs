//! 仓库领域模型与生命周期（FR-06/07/10）。
//!
//! 本模块集中仓库的业务规则——格式 / 类型 / 可见性校验、proxy 上游约束，以及
//! 创建 / 更新 / 删除编排，持久化下沉到 `meta`（数据访问唯一入口）。api 层只调用
//! 本模块做生命周期、调用 `authz` 做判定，不在 handler 内写仓库业务规则。
//! 依赖方向：`api` → `repo` → `meta`（单向无环）。

use crate::meta::{MetaError, MetaStore, NewRepository, RepoType, RepositoryRecord, Visibility};

/// 已实现并可创建仓库的格式集合（P1 的 FR-14~17 + P2 的 FR-28 Go、FR-26 Cargo、FR-27 PyPI、FR-29 NuGet）。
/// 其余格式由各自批次实现后在此登记，未实现格式不提前接受（越界）。
const SUPPORTED_FORMATS: [&str; 8] = [
    "maven", "npm", "docker", "raw", "go", "cargo", "pypi", "nuget",
];

/// 仓库生命周期错误。
#[derive(Debug, thiserror::Error)]
pub enum RepoError {
    /// 入参不合法（格式 / 类型 / 可见性非法，或 proxy 缺上游）。
    #[error("{0}")]
    Invalid(String),
    /// 仓库名已存在。
    #[error("仓库名已存在")]
    NameConflict,
    /// 元数据访问失败。
    #[error(transparent)]
    Meta(#[from] MetaError),
}

/// 创建仓库的领域入参（字符串字段未校验，由本模块校验后落库）。
#[derive(Debug, Clone)]
pub struct CreateRepoInput {
    /// 仓库名。
    pub name: String,
    /// 格式字符串（大小写不敏感，须在支持集合内）。
    pub format: String,
    /// 类型字符串（hosted | proxy，大小写不敏感）。
    pub r#type: String,
    /// 可见性字符串（public | private，大小写不敏感）。
    pub visibility: String,
    /// 上游地址（proxy 必填）。
    pub upstream_url: Option<String>,
    /// 上游凭据引用（仅引用，真值走配置 / env，绝不入库明文）。
    pub upstream_auth_ref: Option<String>,
}

/// 更新仓库的领域入参：字段可选，仅更新提供的项。
#[derive(Debug, Clone, Default)]
pub struct UpdateRepoInput {
    /// 可见性字符串（可选）。
    pub visibility: Option<String>,
    /// 上游地址（可选）。
    pub upstream_url: Option<String>,
    /// 上游凭据引用（可选）。
    pub upstream_auth_ref: Option<String>,
}

/// 创建仓库：校验业务规则后落库，返回新建记录。
///
/// 规则：格式须为第一期四种；proxy 必须提供 upstream_url；上游凭据仅存引用。
pub async fn create(
    meta: &MetaStore,
    input: CreateRepoInput,
) -> Result<RepositoryRecord, RepoError> {
    if input.name.is_empty() {
        return Err(RepoError::Invalid("仓库名不能为空".to_string()));
    }
    let format = normalize_format(&input.format)?;
    let r#type = parse_repo_type(&input.r#type)?;
    let visibility = parse_visibility(&input.visibility)?;

    // proxy 仓库须提供上游地址（hosted 不强制）
    if r#type == RepoType::Proxy && input.upstream_url.as_deref().unwrap_or("").is_empty() {
        return Err(RepoError::Invalid(
            "proxy 仓库必须提供 upstream_url".to_string(),
        ));
    }

    let id = match meta
        .create_repository(NewRepository {
            name: &input.name,
            format: &format,
            r#type,
            visibility,
            upstream_url: input.upstream_url.as_deref(),
            upstream_auth_ref: input.upstream_auth_ref.as_deref(),
        })
        .await
    {
        Ok(id) => id,
        Err(MetaError::Database(sqlx::Error::Database(db))) if db.is_unique_violation() => {
            return Err(RepoError::NameConflict);
        }
        Err(e) => return Err(e.into()),
    };

    tracing::info!(仓库名 = %input.name, 格式 = %format, "已创建仓库");
    meta.get_repository_by_id(&id)
        .await?
        .ok_or_else(|| RepoError::Meta(MetaError::Database(sqlx::Error::RowNotFound)))
}

/// 更新仓库可配置字段：校验可见性合法后落库，返回更新后记录；仓库不存在返回 None。
pub async fn update(
    meta: &MetaStore,
    id: &str,
    input: UpdateRepoInput,
) -> Result<Option<RepositoryRecord>, RepoError> {
    let visibility = match &input.visibility {
        Some(s) => Some(parse_visibility(s)?),
        None => None,
    };
    let updated = meta
        .update_repository(
            id,
            visibility,
            input.upstream_url.as_deref(),
            input.upstream_auth_ref.as_deref(),
        )
        .await?;
    if !updated {
        return Ok(None);
    }
    tracing::info!(仓库 = %id, "已更新仓库");
    Ok(meta.get_repository_by_id(id).await?)
}

/// 删除仓库（级联清理其 ACL 与制品索引由外键保证）。返回是否命中记录。
pub async fn delete(meta: &MetaStore, id: &str) -> Result<bool, RepoError> {
    let deleted = meta.delete_repository(id).await?;
    if deleted {
        tracing::info!(仓库 = %id, "已删除仓库");
    }
    Ok(deleted)
}

/// 归一化并校验格式：大小写不敏感，仅接受已实现并登记的格式（见 [`SUPPORTED_FORMATS`]）。
fn normalize_format(s: &str) -> Result<String, RepoError> {
    let lower = s.to_ascii_lowercase();
    if SUPPORTED_FORMATS.contains(&lower.as_str()) {
        Ok(lower)
    } else {
        Err(RepoError::Invalid(format!("不支持的格式: {s}")))
    }
}

/// 解析仓库类型（大小写不敏感）。
fn parse_repo_type(s: &str) -> Result<RepoType, RepoError> {
    match s.to_ascii_lowercase().as_str() {
        "hosted" => Ok(RepoType::Hosted),
        "proxy" => Ok(RepoType::Proxy),
        _ => Err(RepoError::Invalid(format!("非法仓库类型: {s}"))),
    }
}

/// 解析可见性（大小写不敏感）。
fn parse_visibility(s: &str) -> Result<Visibility, RepoError> {
    match s.to_ascii_lowercase().as_str() {
        "public" => Ok(Visibility::Public),
        "private" => Ok(Visibility::Private),
        _ => Err(RepoError::Invalid(format!("非法可见性: {s}"))),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// 便捷：构造创建入参。
    fn 入参(
        name: &str,
        format: &str,
        r#type: &str,
        vis: &str,
        upstream: Option<&str>,
    ) -> CreateRepoInput {
        CreateRepoInput {
            name: name.to_string(),
            format: format.to_string(),
            r#type: r#type.to_string(),
            visibility: vis.to_string(),
            upstream_url: upstream.map(str::to_string),
            upstream_auth_ref: None,
        }
    }

    #[tokio::test]
    async fn 创建_hosted_仓库大小写不敏感归一化() {
        let meta = MetaStore::open_in_memory().await.unwrap();
        let rec = create(&meta, 入参("libs", "Maven", "HOSTED", "Public", None))
            .await
            .unwrap();
        assert_eq!(rec.format, "maven");
        assert_eq!(rec.r#type, "hosted");
        assert_eq!(rec.visibility, "public");
    }

    #[tokio::test]
    async fn 创建非法格式被拒() {
        let meta = MetaStore::open_in_memory().await.unwrap();
        // rubygems 属后续阶段（P3）尚未实现，创建应被拒（go/cargo/pypi/nuget 已实现，见 SUPPORTED_FORMATS）
        let err = create(&meta, 入参("x", "rubygems", "hosted", "public", None)).await;
        assert!(matches!(err, Err(RepoError::Invalid(_))));
    }

    #[tokio::test]
    async fn 创建_go_格式仓库被接受() {
        // FR-28：Go 已实现并登记进受支持格式，应可创建仓库（大小写不敏感）
        let meta = MetaStore::open_in_memory().await.unwrap();
        let rec = create(&meta, 入参("gomod", "Go", "hosted", "public", None))
            .await
            .unwrap();
        assert_eq!(rec.format, "go");
    }

    #[tokio::test]
    async fn 创建_proxy_缺上游被拒() {
        let meta = MetaStore::open_in_memory().await.unwrap();
        let err = create(&meta, 入参("m", "npm", "proxy", "public", None)).await;
        assert!(matches!(err, Err(RepoError::Invalid(_))));
        // 带上游则成功
        let ok = create(
            &meta,
            入参(
                "m2",
                "npm",
                "proxy",
                "public",
                Some("https://registry.npmjs.org"),
            ),
        )
        .await
        .unwrap();
        assert_eq!(ok.r#type, "proxy");
        assert_eq!(
            ok.upstream_url.as_deref(),
            Some("https://registry.npmjs.org")
        );
    }

    #[tokio::test]
    async fn 创建重名返回冲突() {
        let meta = MetaStore::open_in_memory().await.unwrap();
        create(&meta, 入参("dup", "raw", "hosted", "public", None))
            .await
            .unwrap();
        let err = create(&meta, 入参("dup", "raw", "hosted", "private", None)).await;
        assert!(matches!(err, Err(RepoError::NameConflict)));
    }

    #[tokio::test]
    async fn 更新可见性与删除() {
        let meta = MetaStore::open_in_memory().await.unwrap();
        let rec = create(&meta, 入参("r", "raw", "hosted", "public", None))
            .await
            .unwrap();
        let updated = update(
            &meta,
            &rec.id,
            UpdateRepoInput {
                visibility: Some("private".to_string()),
                ..Default::default()
            },
        )
        .await
        .unwrap()
        .unwrap();
        assert_eq!(updated.visibility, "private");

        // 非法可见性被拒
        let err = update(
            &meta,
            &rec.id,
            UpdateRepoInput {
                visibility: Some("secret".to_string()),
                ..Default::default()
            },
        )
        .await;
        assert!(matches!(err, Err(RepoError::Invalid(_))));

        // 更新不存在仓库返回 None
        assert!(update(&meta, "无此仓库", UpdateRepoInput::default())
            .await
            .unwrap()
            .is_none());

        // 删除
        assert!(delete(&meta, &rec.id).await.unwrap());
        assert!(!delete(&meta, &rec.id).await.unwrap());
    }
}
