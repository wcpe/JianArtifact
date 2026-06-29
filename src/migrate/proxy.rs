//! Nexus 迁移**proxy 仓库配置 + 缓存制品搬运**（FR-38，ADR-0006）。
//!
//! 把源 Nexus 的 **proxy 类型仓库**搬到本系统，分两步：
//! - **仓库配置**：据在线 REST 枚举到的 proxy 仓库配置（格式 + 上游 `remoteUrl`，见
//!   [`super::NexusRepoSummary`]）在本系统创建对应 proxy 仓库；
//! - **缓存制品搬运**：从离线 blob store 读取源已缓存的 proxy 制品本体，经既有
//!   [`ArtifactService::ingest_cached`] 写入本系统对应仓库的缓存（blob 先落盘并校验 sha256
//!   再写元数据索引，失败回滚不留孤儿；流式不整体载入内存）。
//!
//! 数据来源分工：proxy 仓库的**类型 / 上游地址**仅在线 REST 枚举（FR-36）携带，故配置取自
//! `NexusRepoSummary`；制品**本体**在离线 blob store 的 `.bytes` 文件（FR-37 已能枚举其元数据），
//! 故搬运取自离线 blob store。二者按仓库名关联。
//!
//! 幂等与容错（testing-and-quality §2.5）：
//! - 同名仓库已存在则复用（不重复建仓）；
//! - 单个制品搬运失败不中断整批（记录跳过），可重入（同坐标同内容的搬运为幂等）；
//! - 格式无法映射到本系统已实现格式的仓库整体跳过（不越界为未实现格式建仓）。
//!
//! 范围纪律：**只做 proxy 仓库配置 + 缓存制品搬运**，不做 hosted 仓库制品搬运（FR-39）。

use std::path::Path;

use crate::format::{ArtifactCoordinates, ArtifactService, FormatRegistry};
use crate::meta::{MetaStore, NewRepository, RepoType, RepositoryRecord, Visibility};
use crate::proxy::Upstream;
use crate::storage::BlobStore;

use super::online::{await_control, mark_cancelled};
use super::{
    map_nexus_format, normalize_blob_path, JobControl, MigrateError, NexusRepoSummary,
    OnlinePullPhase, OnlinePullProgress, OnlineRepoMigrationOutcome,
};

/// 在一个 blob 处理完成后推进进度计数（FR-125）：`done_assets` +1，按结果累加 `migrated` / `skipped`。
fn bump_progress(progress: &std::sync::Mutex<OnlinePullProgress>, migrated: bool) {
    let mut p = progress.lock().unwrap_or_else(|e| e.into_inner());
    p.done_assets += 1;
    if migrated {
        p.migrated += 1;
    } else {
        p.skipped += 1;
    }
}

/// 单个 proxy 仓库的搬运结果明细。
#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize)]
pub struct RepoMigrationOutcome {
    /// 源仓库名（同时作为本系统仓库名）。
    pub repo_name: String,
    /// 映射后的本系统格式名。
    pub format: String,
    /// 本仓库是否新建（false 表示同名仓库已存在、复用）。
    pub created: bool,
    /// 成功搬运的缓存制品数。
    pub migrated_artifacts: usize,
    /// 跳过 / 失败的制品数（路径非法、读本体失败、写入失败等，均不中断整批）。
    pub skipped_artifacts: usize,
}

/// 整批 proxy 迁移报告。
#[derive(Debug, Clone, Default, PartialEq, Eq, serde::Serialize)]
pub struct ProxyMigrationReport {
    /// 各 proxy 仓库的搬运结果明细。
    pub repos: Vec<RepoMigrationOutcome>,
    /// 因格式无法映射（未实现格式）而整体跳过的源仓库名列表。
    pub skipped_repos: Vec<String>,
}

/// 据源仓库配置在本系统创建 / 复用 proxy 仓库，返回其记录。
///
/// 同名仓库已存在则直接复用（幂等，不重复建仓、不改其既有配置）；否则按映射格式 + 上游地址
/// 新建一个 public proxy 仓库。
async fn ensure_proxy_repo(
    meta: &MetaStore,
    name: &str,
    format: &str,
    upstream_url: &str,
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
            r#type: RepoType::Proxy,
            visibility: Visibility::Public,
            upstream_url: Some(upstream_url),
            // 迁移不搬运源系统上游凭据：凭据真源在 env / 配置，需运维另行配置（凭据不入库）
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

/// 搬运一个 proxy 仓库的全部离线缓存制品本体，返回 (成功数, 跳过数)。
///
/// 逐条流式读取 `.bytes` 本体并经 [`ArtifactService::ingest_cached`] 写入缓存；单条失败
/// （路径非法 / 读本体失败 / 写入失败）记 WARN 后跳过，不中断整批。
/// 返回 `(成功数, 跳过数, 是否中途被取消)`。每条前在 blob 边界响应取消 / 暂停（FR-91/125）：
/// 取消即提前结束本仓库（`cancelled=true`）、暂停即挂起等待继续；边搬边推进进度
/// （`current_path` / `done_assets` / `migrated` / `skipped`）。
async fn migrate_repo_artifacts<S: BlobStore, U: Upstream>(
    artifacts: &ArtifactService<S, U>,
    formats: &FormatRegistry,
    repo: &RepositoryRecord,
    entries: &[super::OfflineBlobEntry],
    progress: &std::sync::Mutex<OnlinePullProgress>,
    control: &JobControl,
) -> (usize, usize, bool) {
    let Some(format) = formats.get(&repo.format) else {
        // 仓库已按映射格式建成，注册表理应有对应处理器；缺失则整批跳过（防御）
        tracing::warn!(仓库 = %repo.name, 格式 = %repo.format, "格式处理器未注册，跳过该仓库制品搬运");
        let mut p = progress.lock().unwrap_or_else(|e| e.into_inner());
        p.skipped += entries.len();
        p.done_assets += entries.len();
        return (0, entries.len(), false);
    };

    let mut migrated = 0usize;
    let mut skipped = 0usize;
    for entry in entries {
        // blob 边界响应取消 / 暂停（FR-91）：取消即提前结束本仓库（已搬运保留）
        if await_control(control, progress).await {
            return (migrated, skipped, true);
        }
        {
            let mut p = progress.lock().unwrap_or_else(|e| e.into_inner());
            p.phase = OnlinePullPhase::Downloading;
            p.current_repo = Some(repo.name.clone());
            p.current_path = Some(entry.blob_name.clone());
        }

        // 归一化并校验路径：非法路径（穿越 / 空）跳过
        let rel = normalize_blob_path(&entry.blob_name);
        let coords: ArtifactCoordinates = match format.parse_path(rel) {
            Ok(c) => c,
            Err(e) => {
                tracing::warn!(仓库 = %repo.name, blob = %entry.blob_name, 错误 = %e, "制品路径非法，跳过搬运");
                skipped += 1;
                bump_progress(progress, false);
                continue;
            }
        };

        // 流式打开 `.bytes` 本体（不整体载入内存）
        let file = match tokio::fs::File::open(&entry.bytes_path).await {
            Ok(f) => f,
            Err(e) => {
                tracing::warn!(仓库 = %repo.name, 路径 = %entry.bytes_path.display(), 错误 = %e, "读取 blob 本体失败，跳过搬运");
                skipped += 1;
                bump_progress(progress, false);
                continue;
            }
        };

        match artifacts.ingest_cached(repo, format, &coords, file).await {
            Ok(_) => {
                migrated += 1;
                bump_progress(progress, true);
            }
            Err(e) => {
                tracing::warn!(仓库 = %repo.name, blob = %entry.blob_name, 错误 = %e, "缓存制品搬运失败，跳过");
                skipped += 1;
                bump_progress(progress, false);
            }
        }
    }
    (migrated, skipped, false)
}

/// 执行 proxy 仓库配置创建 + 缓存制品搬运（FR-38）。
///
/// `source_repos` 为在线 REST 枚举到的源仓库摘要（本函数仅取其中 `type == "proxy"` 者）；
/// `offline_root` 为源离线 blob store 根目录，提供缓存制品本体。逐 proxy 仓库：映射格式
/// （不可映射则整体跳过）→ 创建 / 复用本系统 proxy 仓库 → 按仓库名搬运其离线缓存制品。
pub async fn migrate_proxy_repositories<S: BlobStore, U: Upstream>(
    meta: &MetaStore,
    artifacts: &ArtifactService<S, U>,
    formats: &FormatRegistry,
    source_repos: &[NexusRepoSummary],
    offline_root: &Path,
) -> Result<ProxyMigrationReport, MigrateError> {
    // 无进度上报的便捷入口（同步调用 / 测试用）：用一次性进度态 + 永不触发的控制句柄委托
    let progress = std::sync::Mutex::new(OnlinePullProgress::default());
    let control = JobControl::default();
    migrate_proxy_repositories_with_progress(
        meta,
        artifacts,
        formats,
        source_repos,
        offline_root,
        &progress,
        &control,
    )
    .await
}

/// 执行 proxy 仓库配置创建 + 缓存制品搬运，边搬边上报进度、在 blob 边界响应取消 / 暂停（FR-125）。
///
/// 语义同 [`migrate_proxy_repositories`]，额外把进度写入 `progress`（供 `GET /migrate/jobs/{id}` 轮询）、
/// 在每个 blob 与每个仓库边界响应 `control`（FR-91 取消 / 暂停；取消即停止后续、已搬运保留）。
pub async fn migrate_proxy_repositories_with_progress<S: BlobStore, U: Upstream>(
    meta: &MetaStore,
    artifacts: &ArtifactService<S, U>,
    formats: &FormatRegistry,
    source_repos: &[NexusRepoSummary],
    offline_root: &Path,
    progress: &std::sync::Mutex<OnlinePullProgress>,
    control: &JobControl,
) -> Result<ProxyMigrationReport, MigrateError> {
    progress.lock().unwrap_or_else(|e| e.into_inner()).phase = OnlinePullPhase::Enumerating;

    // 离线 blob store 中的可搬运条目，按仓库名归组（一次枚举、避免逐仓库重复遍历磁盘）
    let entries = crate::migrate::enumerate_blob_entries(offline_root)?;
    // total_assets 作进度分母：离线 store 全部可搬运条目数
    progress
        .lock()
        .unwrap_or_else(|e| e.into_inner())
        .total_assets = entries.len();
    let mut by_repo: std::collections::HashMap<&str, Vec<super::OfflineBlobEntry>> =
        std::collections::HashMap::new();
    for e in &entries {
        by_repo
            .entry(e.repo_name.as_str())
            .or_default()
            .push(e.clone());
    }

    let mut report = ProxyMigrationReport::default();
    for src in source_repos {
        // 仓库边界响应取消：已请求取消则不再开始新仓库（FR-91）
        if control.is_cancelled() {
            mark_cancelled(progress);
            return Ok(report);
        }
        // 仅迁移 proxy 类型仓库（hosted / group 不在本批范围）
        if src.r#type != "proxy" {
            continue;
        }
        // 映射格式：不可映射（未实现格式）整体跳过，不越界建仓
        let Some(format) = map_nexus_format(&src.format) else {
            tracing::info!(仓库 = %src.name, 源格式 = %src.format, "源格式未实现，跳过该 proxy 仓库迁移");
            report.skipped_repos.push(src.name.clone());
            progress
                .lock()
                .unwrap_or_else(|e| e.into_inner())
                .skipped_repos
                .push(src.name.clone());
            continue;
        };
        // proxy 仓库须有上游地址；缺失则跳过（无法建合法 proxy 仓库）
        let Some(upstream_url) = src.upstream_url.as_deref().filter(|u| !u.is_empty()) else {
            tracing::warn!(仓库 = %src.name, "proxy 仓库缺上游地址，跳过迁移");
            report.skipped_repos.push(src.name.clone());
            progress
                .lock()
                .unwrap_or_else(|e| e.into_inner())
                .skipped_repos
                .push(src.name.clone());
            continue;
        };

        let (repo, created) = ensure_proxy_repo(meta, &src.name, format, upstream_url).await?;

        let repo_entries = by_repo.remove(src.name.as_str()).unwrap_or_default();
        let (migrated, skipped, cancelled) =
            migrate_repo_artifacts(artifacts, formats, &repo, &repo_entries, progress, control)
                .await;

        tracing::info!(
            仓库 = %src.name,
            格式 = %format,
            新建 = created,
            已搬运 = migrated,
            已跳过 = skipped,
            已取消 = cancelled,
            "proxy 仓库迁移完成"
        );
        report.repos.push(RepoMigrationOutcome {
            repo_name: src.name.clone(),
            format: format.to_string(),
            created,
            migrated_artifacts: migrated,
            skipped_artifacts: skipped,
        });
        // 进度内同记一份（统一 OnlineRepoMigrationOutcome 形态供轮询展示，target 同源名）
        progress
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .repos
            .push(OnlineRepoMigrationOutcome {
                source_repo: src.name.clone(),
                target_repo: src.name.clone(),
                format: format.to_string(),
                created,
                migrated_artifacts: migrated,
                skipped_artifacts: skipped,
            });

        // 仓库内被取消：标记终态并停止后续仓库（不算失败，已搬运保留）
        if cancelled {
            mark_cancelled(progress);
            return Ok(report);
        }
    }

    Ok(report)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::fs;
    use std::sync::Arc;

    use tokio::io::AsyncReadExt;

    use crate::format::RawFormat;
    use crate::proxy::{Upstream, UpstreamBody, UpstreamError};
    use crate::storage::LocalFsStore;

    /// 永不被触达的 mock 上游：搬运路径不应回源（搬运的字节来自离线本体，非上游）。
    struct NeverUpstream;
    impl Upstream for NeverUpstream {
        async fn fetch(&self, _b: &str, _p: &str) -> Result<UpstreamBody, UpstreamError> {
            panic!("搬运不应触发上游回源");
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
        (meta, svc, formats, dir)
    }

    fn src_repo(name: &str, format: &str, r#type: &str, up: Option<&str>) -> NexusRepoSummary {
        NexusRepoSummary {
            name: name.to_string(),
            format: format.to_string(),
            r#type: r#type.to_string(),
            upstream_url: up.map(str::to_string),
        }
    }

    #[tokio::test]
    async fn 建_proxy_仓库并搬运缓存制品() {
        let (meta, svc, formats, store_dir) = 新建().await;
        let blob_root = store_dir.path().join("nexus");
        build_store(
            &blob_root,
            &[
                ("raw-proxy", "/dir/a.bin", "内容A"),
                ("raw-proxy", "/dir/b.bin", "内容B"),
            ],
        );

        let src = vec![src_repo(
            "raw-proxy",
            "raw",
            "proxy",
            Some("https://up.example"),
        )];
        let report = migrate_proxy_repositories(&meta, &svc, &formats, &src, &blob_root)
            .await
            .unwrap();

        assert_eq!(report.repos.len(), 1);
        let o = &report.repos[0];
        assert!(o.created);
        assert_eq!(o.migrated_artifacts, 2);
        assert_eq!(o.skipped_artifacts, 0);

        // 仓库已按 proxy 建成，缓存命中可读回内容
        let repo = meta
            .get_repository_by_name("raw-proxy")
            .await
            .unwrap()
            .unwrap();
        assert_eq!(repo.r#type, "proxy");
        assert_eq!(repo.upstream_url.as_deref(), Some("https://up.example"));
        let svc = Arc::new(svc);
        let coords = ArtifactCoordinates {
            path: "dir/a.bin".to_string(),
        };
        let (mut h, _) = svc.get(&repo, &RawFormat, &coords).await.unwrap();
        let mut buf = Vec::new();
        h.blob.read_to_end(&mut buf).await.unwrap();
        assert_eq!(buf, "内容A".as_bytes());
    }

    #[tokio::test]
    async fn 跳过_hosted_与未实现格式仓库() {
        let (meta, svc, formats, store_dir) = 新建().await;
        let blob_root = store_dir.path().join("nexus");
        build_store(&blob_root, &[("raw-proxy", "/a.bin", "x")]);

        let src = vec![
            // hosted 不在本批范围
            src_repo("maven-releases", "maven2", "hosted", None),
            // 未实现格式：整体跳过
            src_repo("gems-proxy", "rubygems", "proxy", Some("https://g.example")),
            // proxy 缺上游：跳过
            src_repo("bad-proxy", "raw", "proxy", None),
            // 正常 proxy
            src_repo("raw-proxy", "raw", "proxy", Some("https://up.example")),
        ];
        let report = migrate_proxy_repositories(&meta, &svc, &formats, &src, &blob_root)
            .await
            .unwrap();

        // 仅 raw-proxy 被迁移
        assert_eq!(report.repos.len(), 1);
        assert_eq!(report.repos[0].repo_name, "raw-proxy");
        // gems-proxy（未实现格式）与 bad-proxy（缺上游）进 skipped；hosted 不计入
        assert!(report.skipped_repos.contains(&"gems-proxy".to_string()));
        assert!(report.skipped_repos.contains(&"bad-proxy".to_string()));
        assert!(meta
            .get_repository_by_name("maven-releases")
            .await
            .unwrap()
            .is_none());
    }

    #[tokio::test]
    async fn 同名仓库已存在则复用且搬运幂等可重入() {
        let (meta, svc, formats, store_dir) = 新建().await;
        let blob_root = store_dir.path().join("nexus");
        build_store(&blob_root, &[("raw-proxy", "/a.bin", "同一内容")]);

        let src = vec![src_repo(
            "raw-proxy",
            "raw",
            "proxy",
            Some("https://up.example"),
        )];

        // 首次：新建 + 搬运
        let r1 = migrate_proxy_repositories(&meta, &svc, &formats, &src, &blob_root)
            .await
            .unwrap();
        assert!(r1.repos[0].created);
        assert_eq!(r1.repos[0].migrated_artifacts, 1);

        // 重入：复用既有仓库，同坐标同内容幂等，索引仍只一条
        let r2 = migrate_proxy_repositories(&meta, &svc, &formats, &src, &blob_root)
            .await
            .unwrap();
        assert!(!r2.repos[0].created, "同名仓库应复用而非重建");
        let repo = meta
            .get_repository_by_name("raw-proxy")
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
                ("raw-proxy", "/ok.bin", "好"),
                ("raw-proxy", "/../evil.bin", "坏"),
            ],
        );

        let src = vec![src_repo(
            "raw-proxy",
            "raw",
            "proxy",
            Some("https://up.example"),
        )];
        let report = migrate_proxy_repositories(&meta, &svc, &formats, &src, &blob_root)
            .await
            .unwrap();
        // 合法 1 条搬运成功、非法 1 条跳过，整批未中断
        assert_eq!(report.repos[0].migrated_artifacts, 1);
        assert_eq!(report.repos[0].skipped_artifacts, 1);
    }

    #[tokio::test]
    async fn with_progress_边搬边上报进度() {
        let (meta, svc, formats, store_dir) = 新建().await;
        let blob_root = store_dir.path().join("nexus");
        build_store(
            &blob_root,
            &[("raw-proxy", "/a.bin", "A"), ("raw-proxy", "/b.bin", "B")],
        );
        let src = vec![src_repo(
            "raw-proxy",
            "raw",
            "proxy",
            Some("https://up.example"),
        )];
        let progress = std::sync::Mutex::new(OnlinePullProgress::default());
        let control = JobControl::default();
        let report = migrate_proxy_repositories_with_progress(
            &meta, &svc, &formats, &src, &blob_root, &progress, &control,
        )
        .await
        .unwrap();

        assert_eq!(report.repos[0].migrated_artifacts, 2);
        let p = progress.lock().unwrap();
        assert_eq!(p.total_assets, 2, "total_assets 为离线条目总数");
        assert_eq!(p.done_assets, 2);
        assert_eq!(p.migrated, 2);
        // 进度内同记一份仓库结果（供轮询展示，target 同源名）
        assert_eq!(p.repos.len(), 1);
        assert_eq!(p.repos[0].source_repo, "raw-proxy");
        assert_eq!(p.repos[0].target_repo, "raw-proxy");
        assert_eq!(p.repos[0].migrated_artifacts, 2);
    }

    #[tokio::test]
    async fn with_progress_预先取消则不搬运并标终态() {
        let (meta, svc, formats, store_dir) = 新建().await;
        let blob_root = store_dir.path().join("nexus");
        build_store(&blob_root, &[("raw-proxy", "/a.bin", "A")]);
        let src = vec![src_repo(
            "raw-proxy",
            "raw",
            "proxy",
            Some("https://up.example"),
        )];
        let progress = std::sync::Mutex::new(OnlinePullProgress::default());
        let control = JobControl::default();
        control.request_cancel(); // 仓库边界前即请求取消

        let report = migrate_proxy_repositories_with_progress(
            &meta, &svc, &formats, &src, &blob_root, &progress, &control,
        )
        .await
        .unwrap();

        // 仓库边界取消：未搬任何制品、报告为空、进度终态为 Cancelled（不算失败）
        assert!(report.repos.is_empty());
        let p = progress.lock().unwrap();
        assert_eq!(p.phase, OnlinePullPhase::Cancelled);
        assert_eq!(p.migrated, 0);
    }
}
