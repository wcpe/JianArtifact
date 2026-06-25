//! Nexus **在线拉取**制品迁移（FR-82，ADR-0006 在线入口「读取并搬运」的补齐实现）。
//!
//! 源 Nexus 在线时，经其 REST `components` API 枚举某 hosted 仓库的全部 asset，按各 asset 的
//! `downloadUrl` HTTP 流式下载，经既有 [`ArtifactService::ingest_hosted`] 落为本系统 hosted
//! 制品——**无需离线 blob store 目录**。落定后比对 sha256 保证文件字节一致；单 asset 失败记
//! WARN 跳过、不中断整批。
//!
//! 范围（scope-discipline）：仅 **Maven（`maven2`）hosted** 仓库；其余格式 / 类型整体跳过。
//! 与离线目录搬运（FR-39，见 [`super::hosted`]）互补：本路径制品本体来自 HTTP，而非磁盘 blob。
//!
//! 关键约束：凭据不入库、不进日志；锁外做 IO；下载流式经 `ingest_hosted`，不整体载入内存。

use crate::format::{ArtifactCoordinates, ArtifactService, Format, FormatRegistry, ServiceError};
use crate::meta::{MetaStore, RepositoryRecord};
use crate::proxy::Upstream;
use crate::storage::BlobStore;

use super::hosted::ensure_hosted_repo;
use super::{
    map_nexus_format, parse_components, MigrateError, NexusAsset, NexusClient, NexusCredential,
    NexusRepoSummary,
};

/// 单个 asset 下载 / 写入的最大尝试次数（含首次）：网络中断 / 流式解码失败等瞬时错误自动重试。
const MAX_ASSET_ATTEMPTS: u32 = 3;

/// 在线拉取的网络错误是否为可重试的瞬时错误（传输失败 / 源系统 5xx；4xx 等确定性错误不重试）。
fn is_transient_migrate(e: &MigrateError) -> bool {
    match e {
        MigrateError::Transport(_) => true,
        MigrateError::Status(s) => *s >= 500,
        _ => false,
    }
}

/// 写入错误是否为可重试的瞬时错误（流式下载中断在写入读流时浮现为存储 IO 失败）。
fn is_transient_service(e: &ServiceError) -> bool {
    matches!(e, ServiceError::Storage(_))
}

/// 重试前的指数退避等待 + 中文 WARN 日志（不打印凭据 / URL 明文）。
async fn retry_backoff(repo: &RepositoryRecord, path: &str, attempt: u32, stage: &str) {
    let backoff = std::time::Duration::from_millis(200u64 * 2u64.pow(attempt - 1));
    tracing::warn!(仓库 = %repo.name, 路径 = %path, 第 = attempt, 阶段 = stage, "asset 瞬时失败，退避后重试");
    tokio::time::sleep(backoff).await;
}

/// 在线拉取的单个仓库选择项：源仓库摘要 + 目标仓库名（可自定义，默认与源同名）。
#[derive(Debug, Clone)]
pub struct OnlinePullSelection {
    /// 源 Nexus 仓库摘要（来自 discover）。
    pub source: NexusRepoSummary,
    /// 本系统目标仓库名（允许与源不同名；默认应由上层填为源名）。
    pub target_repo: String,
}

/// 单个仓库的在线拉取结果明细。
#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize)]
pub struct OnlineRepoMigrationOutcome {
    /// 源仓库名。
    pub source_repo: String,
    /// 本系统目标仓库名。
    pub target_repo: String,
    /// 本系统格式名（本批恒为 `maven`）。
    pub format: String,
    /// 目标仓库是否新建（false 表示同名仓库已存在、复用）。
    pub created: bool,
    /// 成功搬运的 asset 数。
    pub migrated_artifacts: usize,
    /// 跳过 / 失败的 asset 数（路径非法、下载失败、sha256 不符、不可覆盖、写入失败等，均不中断整批）。
    pub skipped_artifacts: usize,
}

/// 整批在线拉取报告。
#[derive(Debug, Clone, Default, PartialEq, Eq, serde::Serialize)]
pub struct OnlineMigrationReport {
    /// 各仓库的在线拉取结果明细。
    pub repos: Vec<OnlineRepoMigrationOutcome>,
    /// 因非 hosted / 非 maven（范围外）而整体跳过的源仓库名列表。
    pub skipped_repos: Vec<String>,
}

/// 执行 Nexus 在线拉取迁移：逐选中仓库经 REST 枚举 + HTTP 下载搬运 Maven hosted 制品。
///
/// `base_url` 为源 Nexus 基址；`credential` 为可选凭据（匿名源可不给）；`selections` 为已选源仓库
/// 及其目标仓库名；`max_size` 为单制品上传上限。仅 `maven2` + `hosted` 参与，其余计入 `skipped_repos`。
#[allow(clippy::too_many_arguments)]
pub async fn migrate_online_repositories<C, S, U>(
    client: &C,
    meta: &MetaStore,
    artifacts: &ArtifactService<S, U>,
    formats: &FormatRegistry,
    base_url: &str,
    credential: Option<&NexusCredential>,
    selections: &[OnlinePullSelection],
    max_size: Option<u64>,
) -> Result<OnlineMigrationReport, MigrateError>
where
    C: NexusClient,
    S: BlobStore,
    U: Upstream,
{
    let base = base_url.trim().trim_end_matches('/');
    if base.is_empty() {
        return Err(MigrateError::Invalid(
            "源系统 base URL 不能为空".to_string(),
        ));
    }

    let mut report = OnlineMigrationReport::default();
    for sel in selections {
        let src = &sel.source;
        // 范围：仅 maven2 hosted；其余格式 / 类型整体跳过，不越界建仓
        if src.r#type != "hosted" || map_nexus_format(&src.format) != Some("maven") {
            tracing::info!(
                仓库 = %src.name, 源格式 = %src.format, 类型 = %src.r#type,
                "非 Maven hosted，跳过在线拉取迁移"
            );
            report.skipped_repos.push(src.name.clone());
            continue;
        }

        // 建 / 复用目标 hosted 仓库（名取 target_repo，允许与源不同名）
        let (repo, created) = ensure_hosted_repo(meta, &sel.target_repo, "maven").await?;

        let (migrated, skipped) = pull_repo_assets(
            client, artifacts, formats, base, &src.name, &repo, credential, max_size,
        )
        .await?;

        tracing::info!(
            源仓库 = %src.name, 目标仓库 = %sel.target_repo,
            新建 = created, 已搬运 = migrated, 已跳过 = skipped,
            "Maven hosted 仓库在线拉取迁移完成"
        );
        report.repos.push(OnlineRepoMigrationOutcome {
            source_repo: src.name.clone(),
            target_repo: sel.target_repo.clone(),
            format: "maven".to_string(),
            created,
            migrated_artifacts: migrated,
            skipped_artifacts: skipped,
        });
    }

    Ok(report)
}

/// 分页枚举某源仓库的 components 并逐 asset 下载搬运，返回 (成功数, 跳过数)。
#[allow(clippy::too_many_arguments)]
async fn pull_repo_assets<C, S, U>(
    client: &C,
    artifacts: &ArtifactService<S, U>,
    formats: &FormatRegistry,
    base_url: &str,
    source_repo: &str,
    target_repo: &RepositoryRecord,
    credential: Option<&NexusCredential>,
    max_size: Option<u64>,
) -> Result<(usize, usize), MigrateError>
where
    C: NexusClient,
    S: BlobStore,
    U: Upstream,
{
    let Some(format) = formats.get(&target_repo.format) else {
        // 防御：目标仓库已按 maven 建成，注册表理应有处理器
        tracing::warn!(仓库 = %target_repo.name, 格式 = %target_repo.format, "格式处理器未注册，跳过在线拉取");
        return Ok((0, 0));
    };

    let mut migrated = 0usize;
    let mut skipped = 0usize;
    let mut token: Option<String> = None;
    loop {
        // 枚举失败（鉴权 / 网络 / 解析）向上冒泡——整仓拉取无法继续
        let body = client
            .fetch_components(base_url, source_repo, token.as_deref(), credential)
            .await?;
        let page = parse_components(&body)?;

        for asset in &page.assets {
            match pull_one_asset(
                client,
                artifacts,
                format,
                target_repo,
                asset,
                credential,
                max_size,
            )
            .await
            {
                Ok(()) => migrated += 1,
                // 单 asset 失败已在内部记 WARN / INFO，计跳过、不中断整批
                Err(()) => skipped += 1,
            }
        }

        match page.continuation_token {
            Some(t) => token = Some(t),
            None => break,
        }
    }
    Ok((migrated, skipped))
}

/// 拉取并落定单个 asset：解析路径 → 流式下载 → `ingest_hosted` → 比对 sha256（不符回滚）。
///
/// 任一步失败返回 `Err(())`（已记日志），由调用方计跳过、不中断整批。
async fn pull_one_asset<C, S, U>(
    client: &C,
    artifacts: &ArtifactService<S, U>,
    format: &dyn Format,
    repo: &RepositoryRecord,
    asset: &NexusAsset,
    credential: Option<&NexusCredential>,
    max_size: Option<u64>,
) -> Result<(), ()>
where
    C: NexusClient,
    S: BlobStore,
    U: Upstream,
{
    // 归一化并校验路径：非法路径（穿越 / 空）跳过
    let coords: ArtifactCoordinates = match format.parse_path(&asset.path) {
        Ok(c) => c,
        Err(e) => {
            tracing::warn!(仓库 = %repo.name, 路径 = %asset.path, 错误 = %e, "asset 路径非法，跳过");
            return Err(());
        }
    };

    // 下载 + 流式写入；网络中断 / 流式解码失败等瞬时错误自动重试（指数退避），
    // 确定性失败（不可覆盖 / 超限 / 路径非法等）不重试。
    let mut attempt = 0u32;
    let record = loop {
        attempt += 1;

        // 流式下载（不整体载入内存）
        let reader = match client.download_asset(&asset.download_url, credential).await {
            Ok(r) => r,
            Err(e) => {
                if is_transient_migrate(&e) && attempt < MAX_ASSET_ATTEMPTS {
                    retry_backoff(repo, &asset.path, attempt, "下载").await;
                    continue;
                }
                tracing::warn!(仓库 = %repo.name, 路径 = %asset.path, 错误 = %e, "asset 下载失败，跳过");
                return Err(());
            }
        };

        // 流式写入：下载流中断会在读流时浮现为存储 IO 失败
        match artifacts
            .ingest_hosted(repo, format, &coords, reader, max_size)
            .await
        {
            Ok(r) => break r,
            // 不可覆盖为确定性失败，不重试
            Err(ServiceError::OverwriteForbidden) => {
                tracing::info!(仓库 = %repo.name, 路径 = %asset.path, "同坐标制品已存在且不可覆盖，跳过");
                return Err(());
            }
            Err(e) => {
                if is_transient_service(&e) && attempt < MAX_ASSET_ATTEMPTS {
                    retry_backoff(repo, &asset.path, attempt, "写入").await;
                    continue;
                }
                tracing::warn!(仓库 = %repo.name, 路径 = %asset.path, 错误 = %e, "asset 写入失败，跳过");
                return Err(());
            }
        }
    };

    // 文件一致：落定 sha256 须与源报告一致，否则视为下载损坏，回滚该制品并跳过
    if let Some(expected) = &asset.sha256 {
        if !record.sha256.eq_ignore_ascii_case(expected) {
            tracing::warn!(仓库 = %repo.name, 路径 = %asset.path, "下载内容 sha256 与源报告不符，回滚该制品并跳过");
            let _ = artifacts.delete(repo, &coords).await;
            return Err(());
        }
    }

    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::HashMap;
    use std::sync::Arc;

    use tokio::io::AsyncReadExt;

    use crate::format::{ArtifactCoordinates, MavenFormat, RawFormat};
    use crate::proxy::{Upstream, UpstreamBody, UpstreamError};
    use crate::storage::LocalFsStore;

    /// 永不被触达的 mock 上游：在线拉取不走 proxy 回源。
    struct NeverUpstream;
    impl Upstream for NeverUpstream {
        async fn fetch(&self, _b: &str, _p: &str) -> Result<UpstreamBody, UpstreamError> {
            panic!("在线拉取不应触发上游回源");
        }
    }

    /// mock Nexus 客户端：按 token 序返回预置 components 页，按 downloadUrl 返回预置字节。
    struct MockOnline {
        /// components 页（JSON 文本）按页序排列；token 即页下标字符串。
        pages: Vec<String>,
        /// downloadUrl → 字节；缺失即下载失败。
        assets: HashMap<String, Vec<u8>>,
    }

    impl NexusClient for MockOnline {
        async fn fetch_repositories(
            &self,
            _base_url: &str,
            _credential: Option<&NexusCredential>,
        ) -> Result<String, MigrateError> {
            unimplemented!("在线拉取用例不调用 fetch_repositories")
        }

        async fn fetch_components(
            &self,
            _base_url: &str,
            _repository: &str,
            continuation_token: Option<&str>,
            _credential: Option<&NexusCredential>,
        ) -> Result<String, MigrateError> {
            let idx: usize = continuation_token.and_then(|t| t.parse().ok()).unwrap_or(0);
            self.pages
                .get(idx)
                .cloned()
                .ok_or_else(|| MigrateError::Parse("页越界".to_string()))
        }

        async fn download_asset(
            &self,
            download_url: &str,
            _credential: Option<&NexusCredential>,
        ) -> Result<Box<dyn tokio::io::AsyncRead + Send + Unpin>, MigrateError> {
            match self.assets.get(download_url) {
                Some(bytes) => Ok(Box::new(std::io::Cursor::new(bytes.clone()))),
                None => Err(MigrateError::Transport(format!("404 {download_url}"))),
            }
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

    fn src(name: &str, format: &str, r#type: &str) -> NexusRepoSummary {
        NexusRepoSummary {
            name: name.to_string(),
            format: format.to_string(),
            r#type: r#type.to_string(),
            upstream_url: None,
        }
    }

    fn sel(source: NexusRepoSummary, target: &str) -> OnlinePullSelection {
        OnlinePullSelection {
            source,
            target_repo: target.to_string(),
        }
    }

    /// 固定载荷与其已知 sha256（与 tests/upload_api.rs 同一对照值）。
    const FIXTURE: &[u8] = b"sidecar-maven-fixture-v1";
    const FIXTURE_SHA256: &str = "68e9e9702196267c9832e413cb66fe40836d9388c7b5296302b7e571f5e062c9";

    async fn read_artifact(
        svc: &Arc<ArtifactService<LocalFsStore, NeverUpstream>>,
        repo: &RepositoryRecord,
        path: &str,
    ) -> Option<Vec<u8>> {
        let coords = ArtifactCoordinates {
            path: path.to_string(),
        };
        let (mut h, _) = svc.get(repo, &MavenFormat, &coords).await.ok()?;
        let mut buf = Vec::new();
        h.blob.read_to_end(&mut buf).await.unwrap();
        Some(buf)
    }

    #[tokio::test]
    async fn 在线拉取建仓并按asset落制品字节一致且校验sha256() {
        let (meta, svc, formats, _d) = 新建().await;
        let url = "https://nx/repository/r3d/com/foo/lib/1.0/lib-1.0.jar";
        let page = format!(
            r#"{{ "items": [ {{ "assets": [
                {{ "path": "com/foo/lib/1.0/lib-1.0.jar", "downloadUrl": "{url}", "checksum": {{ "sha256": "{FIXTURE_SHA256}" }} }}
            ]}} ], "continuationToken": null }}"#
        );
        let client = MockOnline {
            pages: vec![page],
            assets: HashMap::from([(url.to_string(), FIXTURE.to_vec())]),
        };

        let report = migrate_online_repositories(
            &client,
            &meta,
            &svc,
            &formats,
            "https://nx/",
            None,
            &[sel(src("r3d", "maven2", "hosted"), "r3d")],
            None,
        )
        .await
        .unwrap();

        assert_eq!(report.repos.len(), 1);
        let o = &report.repos[0];
        assert!(o.created);
        assert_eq!(o.format, "maven");
        assert_eq!(o.migrated_artifacts, 1);
        assert_eq!(o.skipped_artifacts, 0);

        // 目标仓库为 hosted，制品字节一致
        let repo = meta.get_repository_by_name("r3d").await.unwrap().unwrap();
        assert_eq!(repo.r#type, "hosted");
        let svc = Arc::new(svc);
        let got = read_artifact(&svc, &repo, "com/foo/lib/1.0/lib-1.0.jar")
            .await
            .unwrap();
        assert_eq!(got, FIXTURE);
    }

    #[tokio::test]
    async fn sha256与源报告不符时回滚并跳过() {
        let (meta, svc, formats, _d) = 新建().await;
        let url = "https://nx/repository/r3d/a.jar";
        let page = format!(
            r#"{{ "items": [ {{ "assets": [
                {{ "path": "com/foo/a/1.0/a-1.0.jar", "downloadUrl": "{url}", "checksum": {{ "sha256": "00000000000000000000000000000000000000000000000000000000deadbeef" }} }}
            ]}} ], "continuationToken": null }}"#
        );
        let client = MockOnline {
            pages: vec![page],
            assets: HashMap::from([(url.to_string(), FIXTURE.to_vec())]),
        };

        let report = migrate_online_repositories(
            &client,
            &meta,
            &svc,
            &formats,
            "https://nx",
            None,
            &[sel(src("r3d", "maven2", "hosted"), "r3d")],
            None,
        )
        .await
        .unwrap();

        assert_eq!(report.repos[0].migrated_artifacts, 0);
        assert_eq!(report.repos[0].skipped_artifacts, 1);
        // 回滚后制品不可取回
        let repo = meta.get_repository_by_name("r3d").await.unwrap().unwrap();
        let svc = Arc::new(svc);
        assert!(read_artifact(&svc, &repo, "com/foo/a/1.0/a-1.0.jar")
            .await
            .is_none());
    }

    #[tokio::test]
    async fn 目标仓库可改名落于新名仓库() {
        let (meta, svc, formats, _d) = 新建().await;
        let url = "https://nx/repository/r3d/x.jar";
        let page = format!(
            r#"{{ "items": [ {{ "assets": [ {{ "path": "x/1.0/x-1.0.jar", "downloadUrl": "{url}", "checksum": {{}} }} ]}} ], "continuationToken": null }}"#
        );
        let client = MockOnline {
            pages: vec![page],
            assets: HashMap::from([(url.to_string(), b"xx".to_vec())]),
        };
        let report = migrate_online_repositories(
            &client,
            &meta,
            &svc,
            &formats,
            "https://nx",
            None,
            &[sel(src("r3d", "maven2", "hosted"), "r3d-copy")],
            None,
        )
        .await
        .unwrap();

        assert_eq!(report.repos[0].source_repo, "r3d");
        assert_eq!(report.repos[0].target_repo, "r3d-copy");
        // 落于改名后的仓库；源名仓库未建
        assert!(meta
            .get_repository_by_name("r3d-copy")
            .await
            .unwrap()
            .is_some());
        assert!(meta.get_repository_by_name("r3d").await.unwrap().is_none());
    }

    #[tokio::test]
    async fn 非maven或非hosted整体跳过不建仓() {
        let (meta, svc, formats, _d) = 新建().await;
        let client = MockOnline {
            pages: vec![],
            assets: HashMap::new(),
        };
        let report = migrate_online_repositories(
            &client,
            &meta,
            &svc,
            &formats,
            "https://nx",
            None,
            &[
                // maven proxy（非 hosted）
                sel(src("maven-central", "maven2", "proxy"), "maven-central"),
                // npm hosted（非 maven）
                sel(src("npm-release", "npm", "hosted"), "npm-release"),
            ],
            None,
        )
        .await
        .unwrap();

        assert!(report.repos.is_empty());
        assert!(report.skipped_repos.contains(&"maven-central".to_string()));
        assert!(report.skipped_repos.contains(&"npm-release".to_string()));
        assert!(meta
            .get_repository_by_name("maven-central")
            .await
            .unwrap()
            .is_none());
    }

    #[tokio::test]
    async fn 分页枚举跨页搬运全部asset() {
        let (meta, svc, formats, _d) = 新建().await;
        let u1 = "https://nx/repository/r3d/p1.jar";
        let u2 = "https://nx/repository/r3d/p2.jar";
        // 第 0 页 continuationToken=1 指向第 1 页；第 1 页 null 收尾
        let p0 = format!(
            r#"{{ "items": [ {{ "assets": [ {{ "path": "a/1.0/a-1.0.jar", "downloadUrl": "{u1}", "checksum": {{}} }} ]}} ], "continuationToken": "1" }}"#
        );
        let p1 = format!(
            r#"{{ "items": [ {{ "assets": [ {{ "path": "b/1.0/b-1.0.jar", "downloadUrl": "{u2}", "checksum": {{}} }} ]}} ], "continuationToken": null }}"#
        );
        let client = MockOnline {
            pages: vec![p0, p1],
            assets: HashMap::from([
                (u1.to_string(), b"aa".to_vec()),
                (u2.to_string(), b"bb".to_vec()),
            ]),
        };
        let report = migrate_online_repositories(
            &client,
            &meta,
            &svc,
            &formats,
            "https://nx",
            None,
            &[sel(src("r3d", "maven2", "hosted"), "r3d")],
            None,
        )
        .await
        .unwrap();
        assert_eq!(report.repos[0].migrated_artifacts, 2);
        assert_eq!(report.repos[0].skipped_artifacts, 0);
    }

    #[tokio::test]
    async fn 单asset下载失败不中断整批() {
        let (meta, svc, formats, _d) = 新建().await;
        let ok_url = "https://nx/repository/r3d/ok.jar";
        let bad_url = "https://nx/repository/r3d/missing.jar";
        let page = format!(
            r#"{{ "items": [ {{ "assets": [
                {{ "path": "ok/1.0/ok-1.0.jar", "downloadUrl": "{ok_url}", "checksum": {{}} }},
                {{ "path": "bad/1.0/bad-1.0.jar", "downloadUrl": "{bad_url}", "checksum": {{}} }}
            ]}} ], "continuationToken": null }}"#
        );
        // bad_url 不在 assets 映射 → 下载失败
        let client = MockOnline {
            pages: vec![page],
            assets: HashMap::from([(ok_url.to_string(), b"ok".to_vec())]),
        };
        let report = migrate_online_repositories(
            &client,
            &meta,
            &svc,
            &formats,
            "https://nx",
            None,
            &[sel(src("r3d", "maven2", "hosted"), "r3d")],
            None,
        )
        .await
        .unwrap();
        assert_eq!(report.repos[0].migrated_artifacts, 1);
        assert_eq!(report.repos[0].skipped_artifacts, 1);
    }

    #[tokio::test]
    async fn 空_base_url_被拒() {
        let (meta, svc, formats, _d) = 新建().await;
        let client = MockOnline {
            pages: vec![],
            assets: HashMap::new(),
        };
        let err = migrate_online_repositories(
            &client,
            &meta,
            &svc,
            &formats,
            "   ",
            None,
            &[sel(src("r3d", "maven2", "hosted"), "r3d")],
            None,
        )
        .await;
        assert!(matches!(err, Err(MigrateError::Invalid(_))));
    }

    /// 「先失败 N 次再成功」的下载 mock：验证瞬时失败自动重试后恢复。
    struct FlakyClient {
        page: String,
        url: String,
        bytes: Vec<u8>,
        fail_times: u32,
        count: std::sync::Mutex<u32>,
    }

    impl NexusClient for FlakyClient {
        async fn fetch_repositories(
            &self,
            _b: &str,
            _c: Option<&NexusCredential>,
        ) -> Result<String, MigrateError> {
            unimplemented!()
        }
        async fn fetch_components(
            &self,
            _b: &str,
            _r: &str,
            _t: Option<&str>,
            _c: Option<&NexusCredential>,
        ) -> Result<String, MigrateError> {
            Ok(self.page.clone())
        }
        async fn download_asset(
            &self,
            download_url: &str,
            _c: Option<&NexusCredential>,
        ) -> Result<Box<dyn tokio::io::AsyncRead + Send + Unpin>, MigrateError> {
            assert_eq!(download_url, self.url);
            let mut n = self.count.lock().unwrap();
            *n += 1;
            if *n <= self.fail_times {
                // 模拟流式下载瞬时中断（传输类错误，应被重试）
                Err(MigrateError::Transport(format!("瞬时中断 第{n}次")))
            } else {
                Ok(Box::new(std::io::Cursor::new(self.bytes.clone())))
            }
        }
    }

    #[tokio::test]
    async fn 下载瞬时失败重试后成功() {
        let (meta, svc, formats, _d) = 新建().await;
        let url = "https://nx/repository/r3d/flaky.jar";
        let page = format!(
            r#"{{ "items": [ {{ "assets": [ {{ "path": "f/1.0/f-1.0.jar", "downloadUrl": "{url}", "checksum": {{}} }} ]}} ], "continuationToken": null }}"#
        );
        // 前 2 次下载失败、第 3 次成功；MAX_ASSET_ATTEMPTS=3 应恰好恢复
        let client = FlakyClient {
            page,
            url: url.to_string(),
            bytes: b"flaky-ok".to_vec(),
            fail_times: 2,
            count: std::sync::Mutex::new(0),
        };
        let report = migrate_online_repositories(
            &client,
            &meta,
            &svc,
            &formats,
            "https://nx",
            None,
            &[sel(src("r3d", "maven2", "hosted"), "r3d")],
            None,
        )
        .await
        .unwrap();
        // 重试后成功搬运、未计跳过；恰好尝试 3 次
        assert_eq!(report.repos[0].migrated_artifacts, 1);
        assert_eq!(report.repos[0].skipped_artifacts, 0);
        assert_eq!(*client.count.lock().unwrap(), 3);
    }
}
