//! Nexus 迁移 **group 仓库本体**（FR-137，增强 FR-36/39，依赖 FR-136）。
//!
//! 把源 Nexus 的 **group 类型仓库**搬到本系统，分两步：
//! - **仓库配置**：据在线 REST 枚举到的 group 仓库配置（格式 / 成员名列表，见
//!   [`super::NexusRepoSummary::group_members`]）在本系统创建对应 group 仓库；
//! - **成员映射**：按成员名查本系统已存在的仓库（成员应已由 FR-38/39 proxy/hosted 迁移建好），
//!   映射为有序成员 id 列表，调 FR-136 的 [`MetaStore::set_repo_group_members`] 写入关联。
//!
//! 迁移顺序约定：调用方（`api/migrate.rs`）须保证成员仓库先建好，group 后建；
//! 本函数不强制感知调用顺序，只按名查本系统已有仓库——成员缺失时记告警 + 跳过该成员（不中断整 group 建立）。
//!
//! 幂等与容错：
//! - 同名 group 已存在 → 更新其成员映射（`set_repo_group_members` 覆盖）；
//! - 成员缺失（在本系统找不到对应仓库）→ 记告警 + 加入 `skipped_members`，不中断；
//! - 格式无法映射（未实现格式）→ 整体跳过（`skipped_repos`）；
//! - Docker format 整体跳过（FR-136 已界定 Docker 不支持 group）；
//! - 非 group 类型源仓库 → 跳过（不处理）。
//!
//! 范围：**只做 group 本体迁移**（建 group + 映射成员），不搬运制品本体（制品走 FR-38/39/125）。

use crate::meta::{MetaStore, NewRepository, RepoType, Visibility};

use super::{map_nexus_format, MigrateError, NexusRepoSummary};

/// 单个 group 仓库的迁移结果明细。
#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize)]
pub struct GroupRepoOutcome {
    /// 源仓库名（同时作为本系统 group 仓库名）。
    pub name: String,
    /// 映射后的本系统格式名。
    pub format: String,
    /// 本 group 仓库是否新建（false 表示同名已存在、已更新成员映射）。
    pub created: bool,
    /// 成功映射的成员数（在本系统找到对应仓库的成员）。
    pub member_count: usize,
    /// 因本系统无对应仓库而跳过的成员名列表。
    pub skipped_members: Vec<String>,
}

/// 整批 group 迁移报告。
#[derive(Debug, Clone, Default, PartialEq, Eq, serde::Serialize)]
pub struct GroupMigrationReport {
    /// 已迁移（新建或更新成员映射）的各 group 仓库结果明细。
    pub migrated: Vec<GroupRepoOutcome>,
    /// 因格式无法映射（未实现格式 / docker）而整体跳过的源仓库名列表。
    pub skipped_repos: Vec<String>,
}

/// 执行 group 仓库本体迁移（FR-137）。
///
/// `source_repos` 为在线 REST 枚举到的源仓库摘要（本函数仅取其中 `type == "group"` 者）；
/// 对每个 group 仓库：映射格式（不可映射 / docker 则整体跳过）→ 按成员名查本系统已有仓库
/// → 创建 / 复用本系统 group 仓库 → 设成员映射。
pub async fn migrate_group_repositories(
    meta: &MetaStore,
    source_repos: &[NexusRepoSummary],
) -> Result<GroupMigrationReport, MigrateError> {
    let mut report = GroupMigrationReport::default();

    for src in source_repos {
        // 仅迁移 group 类型仓库（hosted / proxy 不在本批范围）
        if src.r#type != "group" {
            continue;
        }

        // 映射格式：不可映射（未实现格式）整体跳过
        let Some(format) = map_nexus_format(&src.format) else {
            tracing::info!(
                仓库 = %src.name,
                源格式 = %src.format,
                "源格式未实现，跳过该 group 仓库迁移"
            );
            report.skipped_repos.push(src.name.clone());
            continue;
        };

        // Docker group 暂不支持（FR-136 已界定），跳过
        if format == "docker" {
            tracing::info!(仓库 = %src.name, "Docker group 暂不支持，跳过迁移");
            report.skipped_repos.push(src.name.clone());
            continue;
        }

        // 按成员名查本系统已有仓库，缺失记告警 + 跳过该成员（不中断整 group）
        let mut member_ids: Vec<String> = Vec::with_capacity(src.group_members.len());
        let mut skipped_members: Vec<String> = Vec::new();
        for member_name in &src.group_members {
            match meta.get_repository_by_name(member_name).await {
                Ok(Some(member)) => member_ids.push(member.id),
                Ok(None) => {
                    tracing::warn!(
                        group = %src.name,
                        成员 = %member_name,
                        "成员仓库在本系统中不存在，跳过该成员（可先运行 proxy/hosted 迁移建成员）"
                    );
                    skipped_members.push(member_name.clone());
                }
                Err(e) => {
                    tracing::warn!(
                        group = %src.name,
                        成员 = %member_name,
                        错误 = %e,
                        "查询成员仓库失败，跳过该成员"
                    );
                    skipped_members.push(member_name.clone());
                }
            }
        }

        // 同名 group 已存在 → 仅更新成员映射（幂等覆盖）；否则新建
        let (group_id, created) = ensure_group_repo(meta, &src.name, format).await?;
        meta.set_repo_group_members(&group_id, &member_ids)
            .await
            .map_err(|e| MigrateError::Invalid(e.to_string()))?;

        tracing::info!(
            仓库 = %src.name,
            格式 = %format,
            新建 = created,
            已映射成员 = member_ids.len(),
            跳过成员 = skipped_members.len(),
            "group 仓库迁移完成"
        );
        report.migrated.push(GroupRepoOutcome {
            name: src.name.clone(),
            format: format.to_string(),
            created,
            member_count: member_ids.len(),
            skipped_members,
        });
    }

    Ok(report)
}

/// 确保本系统存在指定名称的 group 仓库，返回 (id, 是否新建)。
///
/// 同名仓库已存在则直接复用（幂等，不重复建仓、不改其既有格式 / 可见性）；
/// 否则按映射格式新建一个 public group 仓库（group 本身无上游地址）。
async fn ensure_group_repo(
    meta: &MetaStore,
    name: &str,
    format: &str,
) -> Result<(String, bool), MigrateError> {
    if let Some(existing) = meta
        .get_repository_by_name(name)
        .await
        .map_err(|e| MigrateError::Invalid(e.to_string()))?
    {
        return Ok((existing.id, false));
    }

    let id = meta
        .create_repository(NewRepository {
            name,
            format,
            r#type: RepoType::Group,
            visibility: Visibility::Public,
            upstream_url: None,
            upstream_auth_ref: None,
        })
        .await
        .map_err(|e| MigrateError::Invalid(e.to_string()))?;

    Ok((id, true))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::meta::{MetaStore, NewRepository, RepoType, Visibility};

    /// 在内存库中建一个 hosted 仓库，返回其 id。
    async fn 建_hosted_仓库(meta: &MetaStore, name: &str, format: &str) -> String {
        meta.create_repository(NewRepository {
            name,
            format,
            r#type: RepoType::Hosted,
            visibility: Visibility::Public,
            upstream_url: None,
            upstream_auth_ref: None,
        })
        .await
        .unwrap()
    }

    /// 便捷：构造 group 类型源仓库摘要。
    fn group_src(name: &str, format: &str, members: &[&str]) -> NexusRepoSummary {
        NexusRepoSummary {
            name: name.to_string(),
            format: format.to_string(),
            r#type: "group".to_string(),
            upstream_url: None,
            group_members: members.iter().map(|s| s.to_string()).collect(),
        }
    }

    #[tokio::test]
    async fn 建_group_并映射成员() {
        let meta = MetaStore::open_in_memory().await.unwrap();
        // 先建成员仓库
        let aid = 建_hosted_仓库(&meta, "maven-releases", "maven").await;
        let bid = 建_hosted_仓库(&meta, "maven-central", "maven").await;

        let src = vec![group_src(
            "maven-group",
            "maven2",
            &["maven-releases", "maven-central"],
        )];
        let report = migrate_group_repositories(&meta, &src).await.unwrap();

        assert_eq!(report.migrated.len(), 1);
        let o = &report.migrated[0];
        assert_eq!(o.name, "maven-group");
        assert_eq!(o.format, "maven");
        assert!(o.created);
        assert_eq!(o.member_count, 2);
        assert!(o.skipped_members.is_empty());
        assert!(report.skipped_repos.is_empty());

        // 验证 group 在本系统存在
        let group = meta
            .get_repository_by_name("maven-group")
            .await
            .unwrap()
            .unwrap();
        assert_eq!(group.r#type, "group");
        assert_eq!(group.format, "maven");

        // 验证成员映射顺序正确（按源列表顺序）
        let members = meta.list_repo_group_members(&group.id).await.unwrap();
        assert_eq!(members.len(), 2);
        assert_eq!(members[0].id, aid);
        assert_eq!(members[1].id, bid);
    }

    #[tokio::test]
    async fn 同名_group_已存在则更新成员映射() {
        let meta = MetaStore::open_in_memory().await.unwrap();
        let aid = 建_hosted_仓库(&meta, "maven-releases", "maven").await;
        let _bid = 建_hosted_仓库(&meta, "maven-central", "maven").await;

        // 首次迁移：两个成员
        let src = vec![group_src(
            "maven-group",
            "maven2",
            &["maven-releases", "maven-central"],
        )];
        let r1 = migrate_group_repositories(&meta, &src).await.unwrap();
        assert!(r1.migrated[0].created);
        assert_eq!(r1.migrated[0].member_count, 2);

        // 再次迁移（只选一个成员）：复用既有 group，更新成员映射
        let src2 = vec![group_src("maven-group", "maven2", &["maven-releases"])];
        let r2 = migrate_group_repositories(&meta, &src2).await.unwrap();
        assert!(!r2.migrated[0].created, "同名 group 应复用而非重建");
        assert_eq!(r2.migrated[0].member_count, 1);

        // 成员映射已更新为只含 maven-releases
        let group = meta
            .get_repository_by_name("maven-group")
            .await
            .unwrap()
            .unwrap();
        let members = meta.list_repo_group_members(&group.id).await.unwrap();
        assert_eq!(members.len(), 1);
        assert_eq!(members[0].id, aid);
    }

    #[tokio::test]
    async fn 成员缺失则跳过该成员但_group_仍建成() {
        let meta = MetaStore::open_in_memory().await.unwrap();
        let aid = 建_hosted_仓库(&meta, "maven-releases", "maven").await;
        // maven-central 不存在（未先迁移）

        let src = vec![group_src(
            "maven-group",
            "maven2",
            &["maven-releases", "maven-central"],
        )];
        let report = migrate_group_repositories(&meta, &src).await.unwrap();

        assert_eq!(report.migrated.len(), 1);
        let o = &report.migrated[0];
        assert_eq!(o.member_count, 1, "仅 maven-releases 映射成功");
        assert_eq!(o.skipped_members, vec!["maven-central"]);

        // group 已建成，只含可用成员
        let group = meta
            .get_repository_by_name("maven-group")
            .await
            .unwrap()
            .unwrap();
        let members = meta.list_repo_group_members(&group.id).await.unwrap();
        assert_eq!(members.len(), 1);
        assert_eq!(members[0].id, aid);
    }

    #[tokio::test]
    async fn 全部成员缺失则建空_group() {
        let meta = MetaStore::open_in_memory().await.unwrap();
        // 无任何成员在本系统

        let src = vec![group_src("maven-group", "maven2", &["maven-releases"])];
        let report = migrate_group_repositories(&meta, &src).await.unwrap();

        assert_eq!(report.migrated.len(), 1);
        let o = &report.migrated[0];
        assert_eq!(o.member_count, 0);
        assert_eq!(o.skipped_members, vec!["maven-releases"]);

        // group 已建成，成员为空
        let group = meta
            .get_repository_by_name("maven-group")
            .await
            .unwrap()
            .unwrap();
        let members = meta.list_repo_group_members(&group.id).await.unwrap();
        assert!(members.is_empty());
    }

    #[tokio::test]
    async fn 空成员列表建空_group() {
        let meta = MetaStore::open_in_memory().await.unwrap();
        let src = vec![group_src("npm-group", "npm", &[])];
        let report = migrate_group_repositories(&meta, &src).await.unwrap();
        assert_eq!(report.migrated.len(), 1);
        assert_eq!(report.migrated[0].member_count, 0);
        assert!(report.migrated[0].skipped_members.is_empty());
    }

    #[tokio::test]
    async fn 未实现格式跳过整_group() {
        let meta = MetaStore::open_in_memory().await.unwrap();
        let src = vec![group_src("gems-group", "rubygems", &["gems-hosted"])];
        let report = migrate_group_repositories(&meta, &src).await.unwrap();

        assert!(report.migrated.is_empty());
        assert_eq!(report.skipped_repos, vec!["gems-group"]);
        // 未在本系统建仓库
        assert!(meta
            .get_repository_by_name("gems-group")
            .await
            .unwrap()
            .is_none());
    }

    #[tokio::test]
    async fn docker_group_跳过() {
        let meta = MetaStore::open_in_memory().await.unwrap();
        let src = vec![group_src("docker-group", "docker", &["docker-hosted"])];
        let report = migrate_group_repositories(&meta, &src).await.unwrap();

        assert!(report.migrated.is_empty());
        assert_eq!(report.skipped_repos, vec!["docker-group"]);
    }

    #[tokio::test]
    async fn 非_group_类型源仓库跳过() {
        let meta = MetaStore::open_in_memory().await.unwrap();
        // hosted / proxy 不应被 group 迁移处理
        let src = vec![
            NexusRepoSummary {
                name: "maven-releases".to_string(),
                format: "maven2".to_string(),
                r#type: "hosted".to_string(),
                upstream_url: None,
                group_members: vec![],
            },
            NexusRepoSummary {
                name: "maven-proxy".to_string(),
                format: "maven2".to_string(),
                r#type: "proxy".to_string(),
                upstream_url: Some("https://repo1.maven.org/maven2/".to_string()),
                group_members: vec![],
            },
        ];
        let report = migrate_group_repositories(&meta, &src).await.unwrap();
        assert!(report.migrated.is_empty());
        assert!(report.skipped_repos.is_empty());
    }

    #[tokio::test]
    async fn 混合源列表只处理_group() {
        let meta = MetaStore::open_in_memory().await.unwrap();
        建_hosted_仓库(&meta, "npm-hosted", "npm").await;

        let src = vec![
            // hosted：跳过
            NexusRepoSummary {
                name: "npm-hosted".to_string(),
                format: "npm".to_string(),
                r#type: "hosted".to_string(),
                upstream_url: None,
                group_members: vec![],
            },
            // group：处理
            group_src("npm-group", "npm", &["npm-hosted"]),
            // 未实现格式 group：跳过
            group_src("conan-group", "conan", &["conan-hosted"]),
        ];
        let report = migrate_group_repositories(&meta, &src).await.unwrap();

        assert_eq!(report.migrated.len(), 1);
        assert_eq!(report.migrated[0].name, "npm-group");
        assert_eq!(report.migrated[0].member_count, 1);
        assert_eq!(report.skipped_repos, vec!["conan-group"]);
    }
}
