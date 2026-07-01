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
    /// 类型字符串（hosted | proxy | group，大小写不敏感）。
    pub r#type: String,
    /// 可见性字符串（public | private，大小写不敏感）。
    pub visibility: String,
    /// 上游地址（proxy 必填）。
    pub upstream_url: Option<String>,
    /// 上游凭据引用（仅引用，真值走配置 / env，绝不入库明文）。
    pub upstream_auth_ref: Option<String>,
    /// 成员仓库名（有序，仅 group 用；解析顺序即此列表顺序）。
    pub members: Option<Vec<String>>,
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
    /// 成员仓库名（有序，可选，仅 group 用；提供时整体替换成员列表）。
    pub members: Option<Vec<String>>,
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

    // group 仓库：先解析并校验成员（格式一致、非 group、存在、去重），再落库 + 写成员关联。
    // 成员校验在落 group 行之前完成，校验失败则根本不建库（不留半截 group）。
    let member_ids = if r#type == RepoType::Group {
        let names = input.members.clone().unwrap_or_default();
        Some(resolve_and_validate_group_members(meta, &format, &names).await?)
    } else {
        None
    };

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

    // group 成员关联落库（成员已在上面校验通过）
    if let Some(ids) = member_ids {
        meta.set_repo_group_members(&id, &ids).await?;
    }

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

    // 提供 members 时：仓库须存在且为 group，按其格式校验成员后整体替换。
    // 校验在更新可见性等字段之前完成，校验失败不改任何字段。
    let member_ids = match &input.members {
        Some(names) => {
            let repo = match meta.get_repository_by_id(id).await? {
                Some(r) => r,
                None => return Ok(None),
            };
            if RepoType::from_db_str(&repo.r#type) != RepoType::Group {
                return Err(RepoError::Invalid(
                    "仅 group 仓库可配置成员列表".to_string(),
                ));
            }
            Some(resolve_and_validate_group_members(meta, &repo.format, names).await?)
        }
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
    // 仅当未提供 members 时，以 update_repository 的命中与否判断仓库是否存在；
    // 提供 members 时仓库存在性已在上面校验（命中 group），即便本次无可见性等字段变化也应继续设成员。
    if !updated && member_ids.is_none() {
        return Ok(None);
    }
    // 成员关联整体替换（仅 group 且提供了 members 时）
    if let Some(ids) = member_ids {
        meta.set_repo_group_members(id, &ids).await?;
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
        "group" => Ok(RepoType::Group),
        _ => Err(RepoError::Invalid(format!("非法仓库类型: {s}"))),
    }
}

/// 解析并校验 group 成员仓库名列表 → 有序成员 id 列表（FR-136）。
///
/// 逐条校验（任一不满足即拒绝，不建库 / 不改成员）：
/// - 成员仓库存在（按名查得）；
/// - 成员格式与 group 一致（maven group 只聚合 maven 成员）；
/// - 成员非 group 类型（禁止嵌套 group，防环与递归解析）；
/// - 列表内无重复（同一成员只能出现一次）。
///
/// 空列表合法（空 group：解析恒未命中）。返回的 id 顺序与入参名顺序一致。
async fn resolve_and_validate_group_members(
    meta: &MetaStore,
    group_format: &str,
    member_names: &[String],
) -> Result<Vec<String>, RepoError> {
    let mut ids = Vec::with_capacity(member_names.len());
    let mut seen = std::collections::HashSet::new();
    for name in member_names {
        let member = meta
            .get_repository_by_name(name)
            .await?
            .ok_or_else(|| RepoError::Invalid(format!("成员仓库不存在: {name}")))?;
        if member.format != group_format {
            return Err(RepoError::Invalid(format!(
                "成员仓库格式与 group 不一致: {name}（{} ≠ {group_format}）",
                member.format
            )));
        }
        if RepoType::from_db_str(&member.r#type) == RepoType::Group {
            return Err(RepoError::Invalid(format!(
                "不允许嵌套 group：成员仓库本身为 group: {name}"
            )));
        }
        if !seen.insert(member.id.clone()) {
            return Err(RepoError::Invalid(format!("成员仓库重复: {name}")));
        }
        ids.push(member.id);
    }
    Ok(ids)
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
            members: None,
        }
    }

    /// 便捷：构造 group 创建入参（指定格式与有序成员名）。
    fn group_入参(name: &str, format: &str, members: &[&str]) -> CreateRepoInput {
        CreateRepoInput {
            name: name.to_string(),
            format: format.to_string(),
            r#type: "group".to_string(),
            visibility: "public".to_string(),
            upstream_url: None,
            upstream_auth_ref: None,
            members: Some(members.iter().map(|s| s.to_string()).collect()),
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

    #[tokio::test]
    async fn 创建_group_含有序成员并保留顺序() {
        let meta = MetaStore::open_in_memory().await.unwrap();
        create(&meta, 入参("a", "maven", "hosted", "public", None))
            .await
            .unwrap();
        create(
            &meta,
            入参(
                "b",
                "maven",
                "proxy",
                "public",
                Some("https://repo1.maven.org"),
            ),
        )
        .await
        .unwrap();
        // group 成员顺序 [b, a]：应原样保留为解析顺序
        let g = create(&meta, group_入参("mvn-group", "maven", &["b", "a"]))
            .await
            .unwrap();
        assert_eq!(g.r#type, "group");
        let members = meta.list_repo_group_members(&g.id).await.unwrap();
        let names: Vec<&str> = members.iter().map(|m| m.name.as_str()).collect();
        assert_eq!(names, vec!["b", "a"], "成员应按入参顺序解析");
    }

    #[tokio::test]
    async fn 创建_group_成员格式不一致被拒() {
        let meta = MetaStore::open_in_memory().await.unwrap();
        create(&meta, 入参("mvn", "maven", "hosted", "public", None))
            .await
            .unwrap();
        create(&meta, 入参("npmrepo", "npm", "hosted", "public", None))
            .await
            .unwrap();
        // maven group 加入 npm 成员：格式不一致应拒绝
        let err = create(&meta, group_入参("g", "maven", &["mvn", "npmrepo"])).await;
        assert!(matches!(err, Err(RepoError::Invalid(_))));
        // 失败后不应留下半截 group
        assert!(meta.get_repository_by_name("g").await.unwrap().is_none());
    }

    #[tokio::test]
    async fn 创建_group_成员不存在被拒() {
        let meta = MetaStore::open_in_memory().await.unwrap();
        let err = create(&meta, group_入参("g", "raw", &["无此仓库"])).await;
        assert!(matches!(err, Err(RepoError::Invalid(_))));
    }

    #[tokio::test]
    async fn 创建_group_禁止嵌套与重复成员() {
        let meta = MetaStore::open_in_memory().await.unwrap();
        create(&meta, 入参("a", "raw", "hosted", "public", None))
            .await
            .unwrap();
        let inner = create(&meta, group_入参("inner", "raw", &["a"]))
            .await
            .unwrap();
        assert_eq!(inner.r#type, "group");
        // 嵌套：group 成员本身为 group → 拒绝
        let nested = create(&meta, group_入参("outer", "raw", &["inner"])).await;
        assert!(matches!(nested, Err(RepoError::Invalid(_))));
        // 重复成员 → 拒绝
        let dup = create(&meta, group_入参("dupg", "raw", &["a", "a"])).await;
        assert!(matches!(dup, Err(RepoError::Invalid(_))));
    }

    #[tokio::test]
    async fn 创建_空_group_合法() {
        let meta = MetaStore::open_in_memory().await.unwrap();
        let g = create(&meta, group_入参("empty", "raw", &[]))
            .await
            .unwrap();
        assert_eq!(g.r#type, "group");
        assert!(meta
            .list_repo_group_members(&g.id)
            .await
            .unwrap()
            .is_empty());
    }

    #[tokio::test]
    async fn 更新_group_成员替换与非_group_拒配置成员() {
        let meta = MetaStore::open_in_memory().await.unwrap();
        create(&meta, 入参("a", "raw", "hosted", "public", None))
            .await
            .unwrap();
        create(&meta, 入参("b", "raw", "hosted", "public", None))
            .await
            .unwrap();
        let g = create(&meta, group_入参("g", "raw", &["a"])).await.unwrap();
        // 仅替换成员（不改可见性）：应生效为 [b, a]
        update(
            &meta,
            &g.id,
            UpdateRepoInput {
                members: Some(vec!["b".to_string(), "a".to_string()]),
                ..Default::default()
            },
        )
        .await
        .unwrap()
        .unwrap();
        let names: Vec<String> = meta
            .list_repo_group_members(&g.id)
            .await
            .unwrap()
            .into_iter()
            .map(|m| m.name)
            .collect();
        assert_eq!(names, vec!["b", "a"]);

        // 非 group 仓库配置成员 → 拒绝
        let hosted = meta.get_repository_by_name("a").await.unwrap().unwrap();
        let err = update(
            &meta,
            &hosted.id,
            UpdateRepoInput {
                members: Some(vec!["b".to_string()]),
                ..Default::default()
            },
        )
        .await;
        assert!(matches!(err, Err(RepoError::Invalid(_))));
    }
}
