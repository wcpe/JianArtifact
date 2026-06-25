//! Nexus 迁移**hosted 仓库配置 + 完整制品搬运**（FR-39，ADR-0006）。
//!
//! 把源 Nexus 的 **hosted 类型仓库**完整搬到本系统，分两步：
//! - **仓库配置**：据在线 REST 枚举到的 hosted 仓库配置（格式 + 可见性，见
//!   [`super::NexusRepoSummary`]）在本系统创建对应 hosted 仓库；
//! - **完整制品搬运**：从离线 blob store 读取源 hosted 仓库的全部制品本体 + 坐标，经既有
//!   [`ArtifactService::ingest_hosted`] 写入本系统对应仓库（blob 先落盘并校验 sha256 再写
//!   元数据索引，`cached = false` 走 hosted 正常制品语义；失败回滚不留孤儿；流式不整体载入内存）。
//!
//! 与 proxy 搬运（FR-38，见 [`super::proxy`]）的区别：本系统建的是 hosted 仓库（无上游地址），
//! 制品落为正常 hosted 制品（`cached = false`）而非缓存，并据各格式覆盖 / 不可变策略处理重复搬运。
//! 离线 blob 枚举与按仓库归组的编排与 proxy 同款，复用 [`super::enumerate_blob_entries`]。
//!
//! 幂等与容错（testing-and-quality §2.5）：
//! - 同名仓库已存在则复用（不重复建仓）；
//! - 单个制品搬运失败不中断整批（记录跳过），可重入（同坐标同内容的搬运为幂等）；
//! - 按格式覆盖 / 不可变策略：同坐标不同内容且格式不可覆盖（如 Maven release）时跳过该制品
//!   （计入跳过数，不中断整批）；
//! - 格式无法映射到本系统已实现格式的仓库整体跳过（不越界为未实现格式建仓）。
//!
//! 范围纪律：**只做 hosted 仓库配置 + 完整制品搬运**，不重复实现 proxy 搬运（复用 FR-38）。

use std::path::Path;

use crate::format::{ArtifactCoordinates, ArtifactService, FormatRegistry, ServiceError};
use crate::meta::{MetaStore, NewRepository, RepoType, RepositoryRecord, Visibility};
use crate::proxy::Upstream;
use crate::storage::BlobStore;

use super::{map_nexus_format, normalize_blob_path, MigrateError, NexusRepoSummary};

/// 单个 hosted 仓库的搬运结果明细。
#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize)]
pub struct HostedRepoMigrationOutcome {
    /// 源仓库名（同时作为本系统仓库名）。
    pub repo_name: String,
    /// 映射后的本系统格式名。
    pub format: String,
    /// 本仓库是否新建（false 表示同名仓库已存在、复用）。
    pub created: bool,
    /// 成功搬运的制品数。
    pub migrated_artifacts: usize,
    /// 跳过 / 失败的制品数（路径非法、读本体失败、不可覆盖、写入失败等，均不中断整批）。
    pub skipped_artifacts: usize,
}

/// 整批 hosted 迁移报告。
#[derive(Debug, Clone, Default, PartialEq, Eq, serde::Serialize)]
pub struct HostedMigrationReport {
    /// 各 hosted 仓库的搬运结果明细。
    pub repos: Vec<HostedRepoMigrationOutcome>,
    /// 因格式无法映射（未实现格式）而整体跳过的源仓库名列表。
    pub skipped_repos: Vec<String>,
}

/// 据源仓库配置在本系统创建 / 复用 hosted 仓库，返回其记录与是否新建。
///
/// 同名仓库已存在则直接复用（幂等，不重复建仓、不改其既有配置）；否则按映射格式新建一个
/// public hosted 仓库（hosted 无上游地址 / 上游凭据）。
pub(crate) async fn ensure_hosted_repo(
    meta: &MetaStore,
    name: &str,
    format: &str,
) -> Result<(RepositoryRecord, bool), MigrateError> {
    if let Some(existing) = meta
        .get_repository_by_name(name)
        .await
        .map_err(|e| MigrateError::Invalid(e.to_string()))?
    {
        return Ok((existing, false));
    }

    let id = meta
        .create_repository(NewRepository {
            name,
            format,
            r#type: RepoType::Hosted,
            visibility: Visibility::Public,
            // hosted 仓库无上游地址 / 上游凭据
            upstream_url: None,
            upstream_auth_ref: None,
        })
        .await
        .map_err(|e| MigrateError::Invalid(e.to_string()))?;

    let record = meta
        .get_repository_by_id(&id)
        .await
        .map_err(|e| MigrateError::Invalid(e.to_string()))?
        .ok_or_else(|| MigrateError::Invalid("新建仓库后回查为空".to_string()))?;
    Ok((record, true))
}

/// 搬运一个 hosted 仓库的全部离线制品本体，返回 (成功数, 跳过数)。
///
/// 逐条流式读取 `.bytes` 本体并经 [`ArtifactService::ingest_hosted`] 写入；单条失败
/// （路径非法 / 读本体失败 / 不可覆盖 / 写入失败）记 WARN 后跳过，不中断整批。
/// `max_size` 为单制品上传上限（超限的制品按跳过处理，不写半截 blob）。
async fn migrate_repo_artifacts<S: BlobStore, U: Upstream>(
    artifacts: &ArtifactService<S, U>,
    formats: &FormatRegistry,
    repo: &RepositoryRecord,
    entries: &[super::OfflineBlobEntry],
    max_size: Option<u64>,
) -> (usize, usize) {
    let Some(format) = formats.get(&repo.format) else {
        // 仓库已按映射格式建成，注册表理应有对应处理器；缺失则整批跳过（防御）
        tracing::warn!(仓库 = %repo.name, 格式 = %repo.format, "格式处理器未注册，跳过该仓库制品搬运");
        return (0, entries.len());
    };

    let mut migrated = 0usize;
    let mut skipped = 0usize;
    for entry in entries {
        // 归一化并校验路径：非法路径（穿越 / 空）跳过
        let rel = normalize_blob_path(&entry.blob_name);
        let coords: ArtifactCoordinates = match format.parse_path(rel) {
            Ok(c) => c,
            Err(e) => {
                tracing::warn!(仓库 = %repo.name, blob = %entry.blob_name, 错误 = %e, "制品路径非法，跳过搬运");
                skipped += 1;
                continue;
            }
        };

        // 流式打开 `.bytes` 本体（不整体载入内存）
        let file = match tokio::fs::File::open(&entry.bytes_path).await {
            Ok(f) => f,
            Err(e) => {
                tracing::warn!(仓库 = %repo.name, 路径 = %entry.bytes_path.display(), 错误 = %e, "读取 blob 本体失败，跳过搬运");
                skipped += 1;
                continue;
            }
        };

        match artifacts
            .ingest_hosted(repo, format, &coords, file, max_size)
            .await
        {
            Ok(_) => migrated += 1,
            // 不可覆盖（如 Maven release 已存在不同内容）：按覆盖 / 不可变策略跳过，不中断整批
            Err(ServiceError::OverwriteForbidden) => {
                tracing::info!(仓库 = %repo.name, blob = %entry.blob_name, "同坐标制品已存在且不可覆盖，跳过搬运");
                skipped += 1;
            }
            Err(e) => {
                tracing::warn!(仓库 = %repo.name, blob = %entry.blob_name, 错误 = %e, "hosted 制品搬运失败，跳过");
                skipped += 1;
            }
        }
    }
    (migrated, skipped)
}

/// 执行 hosted 仓库配置创建 + 完整制品搬运（FR-39）。
///
/// `source_repos` 为在线 REST 枚举到的源仓库摘要（本函数仅取其中 `type == "hosted"` 者）；
/// `offline_root` 为源离线 blob store 根目录，提供制品本体；`max_size` 为单制品上传上限。
/// 逐 hosted 仓库：映射格式（不可映射则整体跳过）→ 创建 / 复用本系统 hosted 仓库 →
/// 按仓库名搬运其离线制品。
pub async fn migrate_hosted_repositories<S: BlobStore, U: Upstream>(
    meta: &MetaStore,
    artifacts: &ArtifactService<S, U>,
    formats: &FormatRegistry,
    source_repos: &[NexusRepoSummary],
    offline_root: &Path,
    max_size: Option<u64>,
) -> Result<HostedMigrationReport, MigrateError> {
    // 离线 blob store 中的可搬运条目，按仓库名归组（一次枚举、避免逐仓库重复遍历磁盘）
    let entries = super::enumerate_blob_entries(offline_root)?;
    let mut by_repo: std::collections::HashMap<&str, Vec<super::OfflineBlobEntry>> =
        std::collections::HashMap::new();
    for e in &entries {
        by_repo
            .entry(e.repo_name.as_str())
            .or_default()
            .push(e.clone());
    }

    let mut report = HostedMigrationReport::default();
    for src in source_repos {
        // 仅迁移 hosted 类型仓库（proxy / group 不在本批范围；proxy 走 FR-38）
        if src.r#type != "hosted" {
            continue;
        }
        // 映射格式：不可映射（未实现格式）整体跳过，不越界建仓
        let Some(format) = map_nexus_format(&src.format) else {
            tracing::info!(仓库 = %src.name, 源格式 = %src.format, "源格式未实现，跳过该 hosted 仓库迁移");
            report.skipped_repos.push(src.name.clone());
            continue;
        };

        let (repo, created) = ensure_hosted_repo(meta, &src.name, format).await?;

        let repo_entries = by_repo.remove(src.name.as_str()).unwrap_or_default();
        let (migrated, skipped) =
            migrate_repo_artifacts(artifacts, formats, &repo, &repo_entries, max_size).await;

        tracing::info!(
            仓库 = %src.name,
            格式 = %format,
            新建 = created,
            已搬运 = migrated,
            已跳过 = skipped,
            "hosted 仓库迁移完成"
        );
        report.repos.push(HostedRepoMigrationOutcome {
            repo_name: src.name.clone(),
            format: format.to_string(),
            created,
            migrated_artifacts: migrated,
            skipped_artifacts: skipped,
        });
    }

    Ok(report)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::fs;
    use std::sync::Arc;

    use tokio::io::AsyncReadExt;

    use crate::format::{MavenFormat, RawFormat};
    use crate::proxy::{Upstream, UpstreamBody, UpstreamError};
    use crate::storage::LocalFsStore;

    /// 永不被触达的 mock 上游：hosted 搬运不应回源（字节来自离线本体）。
    struct NeverUpstream;
    impl Upstream for NeverUpstream {
        async fn fetch(&self, _b: &str, _p: &str) -> Result<UpstreamBody, UpstreamError> {
            panic!("hosted 搬运不应触发上游回源");
        }
    }

    /// 在临时目录铺一个最小 Nexus 文件型 blob store（content/vol-01/chap-01 下放成对 .properties/.bytes）。
    fn build_store(root: &Path, blobs: &[(&str, &str, &str)]) {
        let chap = root.join("content").join("vol-01").join("chap-01");
        fs::create_dir_all(&chap).unwrap();
        for (i, (repo, blob_name, body)) in blobs.iter().enumerate() {
            let stem = format!("blob-{i}");
            let props = format!(
                "@Bucket.repo-name={repo}\n@BlobStore.blob-name={blob_name}\nsize={}\nsha1=x\ndeleted=false\n",
                body.len()
            );
            fs::write(chap.join(format!("{stem}.properties")), props).unwrap();
            fs::write(chap.join(format!("{stem}.bytes")), body).unwrap();
        }
    }

    async fn 新建() -> (
        MetaStore,
        ArtifactService<LocalFsStore, NeverUpstream>,
        FormatRegistry,
        tempfile::TempDir,
    ) {
        let dir = tempfile::tempdir().unwrap();
        let meta = MetaStore::open_in_memory().await.unwrap();
        let store = LocalFsStore::new(dir.path().join("blobs")).await.unwrap();
        let svc = ArtifactService::new(store, meta.clone(), NeverUpstream);
        let mut formats = FormatRegistry::new();
        formats.register(Box::new(RawFormat));
        formats.register(Box::new(MavenFormat));
        (meta, svc, formats, dir)
    }

    fn src_repo(name: &str, format: &str, r#type: &str) -> NexusRepoSummary {
        NexusRepoSummary {
            name: name.to_string(),
            format: format.to_string(),
            r#type: r#type.to_string(),
            upstream_url: None,
        }
    }

    #[tokio::test]
    async fn 建_hosted_仓库并完整搬运制品() {
        let (meta, svc, formats, store_dir) = 新建().await;
        let blob_root = store_dir.path().join("nexus");
        build_store(
            &blob_root,
            &[
                ("raw-hosted", "/dir/a.bin", "内容A"),
                ("raw-hosted", "/dir/b.bin", "内容B"),
            ],
        );

        let src = vec![src_repo("raw-hosted", "raw", "hosted")];
        let report = migrate_hosted_repositories(&meta, &svc, &formats, &src, &blob_root, None)
            .await
            .unwrap();

        assert_eq!(report.repos.len(), 1);
        let o = &report.repos[0];
        assert!(o.created);
        assert_eq!(o.migrated_artifacts, 2);
        assert_eq!(o.skipped_artifacts, 0);

        // 仓库建为 hosted（无上游地址），制品非缓存可读回
        let repo = meta
            .get_repository_by_name("raw-hosted")
            .await
            .unwrap()
            .unwrap();
        assert_eq!(repo.r#type, "hosted");
        assert_eq!(repo.upstream_url, None);
        let svc = Arc::new(svc);
        let coords = ArtifactCoordinates {
            path: "dir/a.bin".to_string(),
        };
        let (mut h, _) = svc.get(&repo, &RawFormat, &coords).await.unwrap();
        assert_eq!(h.record.cached, 0);
        let mut buf = Vec::new();
        h.blob.read_to_end(&mut buf).await.unwrap();
        assert_eq!(buf, "内容A".as_bytes());
    }

    #[tokio::test]
    async fn 跳过_proxy_与未实现格式仓库() {
        let (meta, svc, formats, store_dir) = 新建().await;
        let blob_root = store_dir.path().join("nexus");
        build_store(&blob_root, &[("raw-hosted", "/a.bin", "x")]);

        let src = vec![
            // proxy 不在本批范围（走 FR-38）
            src_repo("nuget-proxy", "nuget", "proxy"),
            // 未实现格式：整体跳过
            src_repo("gems-hosted", "rubygems", "hosted"),
            // 正常 hosted
            src_repo("raw-hosted", "raw", "hosted"),
        ];
        let report = migrate_hosted_repositories(&meta, &svc, &formats, &src, &blob_root, None)
            .await
            .unwrap();

        // 仅 raw-hosted 被迁移
        assert_eq!(report.repos.len(), 1);
        assert_eq!(report.repos[0].repo_name, "raw-hosted");
        // gems-hosted（未实现格式）进 skipped；proxy 不计入
        assert!(report.skipped_repos.contains(&"gems-hosted".to_string()));
        assert!(meta
            .get_repository_by_name("nuget-proxy")
            .await
            .unwrap()
            .is_none());
    }

    #[tokio::test]
    async fn 同名仓库已存在则复用且搬运幂等可重入() {
        let (meta, svc, formats, store_dir) = 新建().await;
        let blob_root = store_dir.path().join("nexus");
        build_store(&blob_root, &[("raw-hosted", "/a.bin", "同一内容")]);

        let src = vec![src_repo("raw-hosted", "raw", "hosted")];

        // 首次：新建 + 搬运
        let r1 = migrate_hosted_repositories(&meta, &svc, &formats, &src, &blob_root, None)
            .await
            .unwrap();
        assert!(r1.repos[0].created);
        assert_eq!(r1.repos[0].migrated_artifacts, 1);

        // 重入：复用既有仓库，同坐标同内容幂等，索引仍只一条
        let r2 = migrate_hosted_repositories(&meta, &svc, &formats, &src, &blob_root, None)
            .await
            .unwrap();
        assert!(!r2.repos[0].created, "同名仓库应复用而非重建");
        let repo = meta
            .get_repository_by_name("raw-hosted")
            .await
            .unwrap()
            .unwrap();
        assert_eq!(
            meta.list_artifacts_by_repo(&repo.id).await.unwrap().len(),
            1
        );
    }

    #[tokio::test]
    async fn 单制品失败不中断整批() {
        let (meta, svc, formats, store_dir) = 新建().await;
        let blob_root = store_dir.path().join("nexus");
        // 一个合法、一个路径非法（含 .. 穿越，被 parse_path 拒）
        build_store(
            &blob_root,
            &[
                ("raw-hosted", "/ok.bin", "好"),
                ("raw-hosted", "/../evil.bin", "坏"),
            ],
        );

        let src = vec![src_repo("raw-hosted", "raw", "hosted")];
        let report = migrate_hosted_repositories(&meta, &svc, &formats, &src, &blob_root, None)
            .await
            .unwrap();
        // 合法 1 条搬运成功、非法 1 条跳过，整批未中断
        assert_eq!(report.repos[0].migrated_artifacts, 1);
        assert_eq!(report.repos[0].skipped_artifacts, 1);
    }

    #[tokio::test]
    async fn maven_release_不可覆盖时跳过不中断整批() {
        let (meta, svc, formats, store_dir) = 新建().await;
        let blob_root = store_dir.path().join("nexus");
        let coord = "/com/foo/lib/1.0/lib-1.0.jar";

        // 先建仓并搬入 v1 release 制品
        build_store(&blob_root, &[("maven-releases", coord, "release-v1")]);
        let src = vec![src_repo("maven-releases", "maven2", "hosted")];
        let r1 = migrate_hosted_repositories(&meta, &svc, &formats, &src, &blob_root, None)
            .await
            .unwrap();
        assert_eq!(r1.repos[0].migrated_artifacts, 1);

        // 同坐标改不同内容再搬：release 不可覆盖 → 跳过（不中断、不改既有内容）
        let blob_root2 = store_dir.path().join("nexus2");
        build_store(
            &blob_root2,
            &[("maven-releases", coord, "release-v2-tampered")],
        );
        let r2 = migrate_hosted_repositories(&meta, &svc, &formats, &src, &blob_root2, None)
            .await
            .unwrap();
        assert!(!r2.repos[0].created);
        assert_eq!(r2.repos[0].migrated_artifacts, 0);
        assert_eq!(r2.repos[0].skipped_artifacts, 1);

        // 既有制品仍是 v1（未被覆盖）
        let repo = meta
            .get_repository_by_name("maven-releases")
            .await
            .unwrap()
            .unwrap();
        let svc = Arc::new(svc);
        let coords = ArtifactCoordinates {
            path: "com/foo/lib/1.0/lib-1.0.jar".to_string(),
        };
        let (mut h, _) = svc.get(&repo, &MavenFormat, &coords).await.unwrap();
        let mut buf = Vec::new();
        h.blob.read_to_end(&mut buf).await.unwrap();
        assert_eq!(buf, b"release-v1");
    }

    #[tokio::test]
    async fn 超限制品被跳过不中断整批() {
        let (meta, svc, formats, store_dir) = 新建().await;
        let blob_root = store_dir.path().join("nexus");
        build_store(
            &blob_root,
            &[
                ("raw-hosted", "/small.bin", "小"),
                ("raw-hosted", "/big.bin", "这是一个超过上限的较大制品内容"),
            ],
        );

        let src = vec![src_repo("raw-hosted", "raw", "hosted")];
        // 上限 5 字节：small（"小" 3 字节）通过、big 超限被跳过
        let report = migrate_hosted_repositories(&meta, &svc, &formats, &src, &blob_root, Some(5))
            .await
            .unwrap();
        assert_eq!(report.repos[0].migrated_artifacts, 1);
        assert_eq!(report.repos[0].skipped_artifacts, 1);
    }
}
