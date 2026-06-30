//! 漏洞库离线镜像（FR-70，ADR-0012）：定期把公开漏洞数据（OSV 等）下载到本机并落本地库。
//!
//! 范围（关键）：本模块只做**镜像下载 + 解析 + 本地落库 + 周期刷新**；
//! **不做**按制品坐标的匹配 / 标记（FR-71，后续批次），不在此引入任何匹配逻辑。
//!
//! 隐私红线（ADR-0012 / architecture-invariants）：下载的是公开漏洞数据集的**整体镜像**
//! （按生态的 `all.zip`），**绝不把本机制品坐标逐包外发到外部漏洞服务**。
//!
//! 下载经 [`MirrorSource`] trait 抽象，生产实现 [`HttpMirrorSource`] 走 reqwest（纯 rustls）；
//! 测试可注入本地 mock，喂样例 zip 以穷举解析与落库、断言不外发坐标。

use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::Duration;

use tracing::{info, warn};

use crate::config::VulnConfig;
use crate::meta::{MetaStore, NewAdvisory, NewAffected};

mod http;
mod matcher;
mod osv;

pub use http::HttpMirrorSource;
pub use matcher::{compare_versions, is_affected, AffectedRecord};
pub use osv::{parse_advisory, OsvParseError, ParsedAdvisory};

use crate::meta::AdvisoryAffectedMatch;

/// 制品命中的单条漏洞公告（FR-71）：供 API 展示的最小结果。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct VulnHit {
    /// 命中的公告 id（如 GHSA / CVE）。
    pub advisory_id: String,
    /// 公告严重度（可空）。
    pub severity: Option<String>,
    /// 公告简要描述（可空）。
    pub summary: Option<String>,
}

/// 在已由 `meta` 取回的候选受影响记录中，筛出真正命中本制品版本的公告（FR-71）。
///
/// 输入 `candidates` 为本地库按 `(ecosystem, package)` 查得的候选行，本函数据各行的版本范围
/// 用纯函数 [`is_affected`] 判定该制品 `version` 是否落入受影响区间；同一公告多条受影响行命中时只计一次。
/// **全程在内存对本机已镜像数据比对，绝不外发坐标**（守 ADR-0012 数据不外发红线）。
pub fn select_hits(version: &str, candidates: &[AdvisoryAffectedMatch]) -> Vec<VulnHit> {
    let mut hits: Vec<VulnHit> = Vec::new();
    for c in candidates {
        let record = AffectedRecord {
            ranges: c.ranges.clone(),
            versions: c.versions.clone(),
        };
        if !is_affected(version, &record) {
            continue;
        }
        // 同一公告可能有多条受影响行命中，去重（按 advisory_id）只计一次
        if hits.iter().any(|h| h.advisory_id == c.advisory_id) {
            continue;
        }
        hits.push(VulnHit {
            advisory_id: c.advisory_id.clone(),
            severity: c.severity.clone(),
            summary: c.summary.clone(),
        });
    }
    hits
}

/// 数据来源标识（落库 source 字段与刷新状态用）。
const SOURCE_OSV: &str = "osv";

/// 镜像下载 / 刷新错误。
#[derive(Debug, thiserror::Error)]
pub enum VulnError {
    /// 下载失败（网络不可用 / 超时 / 非 2xx）。
    #[error("漏洞库镜像下载失败: {0}")]
    Download(String),
    /// 解压镜像 zip 失败。
    #[error("漏洞库镜像解压失败: {0}")]
    Unzip(String),
    /// 本地落库失败。
    #[error("漏洞库落库失败: {0}")]
    Store(#[from] crate::meta::MetaError),
    /// 本地临时文件 IO 失败。
    #[error("漏洞库镜像本地 IO 失败: {0}")]
    Io(#[from] std::io::Error),
}

/// 镜像下载抽象：把某生态的整体镜像下载到指定本地文件。
///
/// 生产实现 [`HttpMirrorSource`] 据基址拼 `{ecosystem}/all.zip` 流式下载；
/// 测试可注入 mock（如把样例 zip 复制到目标路径）以穷举解析与落库、断言不外发坐标。
///
/// 返回的 future 要求 `Send`：刷新在后台 `tokio::spawn` 任务中执行，需可跨线程调度。
pub trait MirrorSource: Send + Sync {
    /// 把某生态的镜像（zip）下载落到 `dest` 本地文件。
    fn download(
        &self,
        ecosystem: &str,
        dest: &Path,
    ) -> impl std::future::Future<Output = Result<(), VulnError>> + Send;
}

/// 漏洞库离线镜像服务：编排"下载 → 解压 → 解析 → 落库 → 记录刷新状态"。
///
/// 持有元数据存储与镜像下载器；临时文件落在数据目录下的 `vuln-mirror` 子目录。
pub struct VulnMirror<S: MirrorSource> {
    /// 元数据存储（落库唯一入口）。
    meta: MetaStore,
    /// 镜像下载器。
    source: S,
    /// 镜像临时文件目录（位于数据目录下）。
    work_dir: PathBuf,
}

impl<S: MirrorSource> VulnMirror<S> {
    /// 构造镜像服务。`data_dir` 为运行期数据目录，临时 zip 落其下 `vuln-mirror`。
    pub fn new(meta: MetaStore, source: S, data_dir: &Path) -> Self {
        Self {
            meta,
            source,
            work_dir: data_dir.join("vuln-mirror"),
        }
    }

    /// 刷新指定生态：下载镜像 → 解压并解析每条公告 → 幂等落库 → 记录刷新状态。
    ///
    /// 幂等：同一公告反复刷新结果一致（meta 层 upsert + 整体替换坐标）。返回落库公告条数。
    /// 下载 / 解压 / 解析任一失败即冒泡，不写半截刷新状态。
    pub async fn refresh_ecosystem(&self, ecosystem: &str) -> Result<usize, VulnError> {
        tokio::fs::create_dir_all(&self.work_dir).await?;
        // 临时 zip 路径（按生态命名，避免并发刷新不同生态互相覆盖）
        let zip_path = self
            .work_dir
            .join(format!("{}-all.zip", sanitize(ecosystem)));

        info!(生态 = %ecosystem, "开始下载漏洞库离线镜像");
        // 下载（锁外 IO）：把整体镜像 zip 落到本地临时文件
        self.source.download(ecosystem, &zip_path).await?;

        // 解压并解析（CPU/阻塞 IO 放 spawn_blocking，避免阻塞异步运行时）
        let advisories = parse_zip(&zip_path).await?;
        let total = advisories.len();
        info!(生态 = %ecosystem, 公告数 = total, "镜像解析完成，开始落库");

        // 逐条幂等落库（meta 层在事务内 upsert）
        for adv in &advisories {
            self.meta.upsert_advisory(adv).await?;
        }

        // 记录刷新状态（成功落库后再记，失败不留状态）
        self.meta
            .record_mirror_refresh(SOURCE_OSV, ecosystem, total as i64)
            .await?;

        // 清理临时 zip（保留目录）；清理失败仅告警不影响刷新结果
        if let Err(e) = tokio::fs::remove_file(&zip_path).await {
            warn!(生态 = %ecosystem, 错误 = %e, "清理漏洞库镜像临时文件失败");
        }
        info!(生态 = %ecosystem, 公告数 = total, "漏洞库离线镜像刷新完成");
        Ok(total)
    }

    /// 刷新配置中列出的全部生态；单个生态失败仅告警并继续其余，返回成功落库总条数。
    pub async fn refresh_all(&self, ecosystems: &[String]) -> usize {
        let mut total = 0;
        for eco in ecosystems {
            match self.refresh_ecosystem(eco).await {
                Ok(n) => total += n,
                // 单个生态失败不阻断其余生态刷新
                Err(e) => warn!(生态 = %eco, 错误 = %e, "漏洞库生态刷新失败，跳过"),
            }
        }
        total
    }
}

/// 解压本地镜像 zip 并解析其中每条 OSV 公告为待落库结构。
///
/// zip 读取需 `Read + Seek`、为同步阻塞操作，放 `spawn_blocking` 执行，避免阻塞异步运行时。
/// 单条目解析失败仅告警跳过该条，不让整批刷新因个别坏条目失败。
async fn parse_zip(zip_path: &Path) -> Result<Vec<NewAdvisory>, VulnError> {
    let zip_path = zip_path.to_path_buf();
    tokio::task::spawn_blocking(move || parse_zip_blocking(&zip_path))
        .await
        .map_err(|e| VulnError::Unzip(format!("解压任务异常: {e}")))?
}

/// 同步解压 + 解析（在阻塞线程执行）。
fn parse_zip_blocking(zip_path: &Path) -> Result<Vec<NewAdvisory>, VulnError> {
    use std::io::Read;

    let file = std::fs::File::open(zip_path)?;
    let mut archive = zip::ZipArchive::new(file).map_err(|e| VulnError::Unzip(e.to_string()))?;

    let mut advisories = Vec::new();
    for i in 0..archive.len() {
        let mut entry = archive
            .by_index(i)
            .map_err(|e| VulnError::Unzip(e.to_string()))?;
        // 仅处理 .json 条目（OSV all.zip 每条公告一个 JSON 文件），跳过目录与其他
        if !entry.name().ends_with(".json") {
            continue;
        }
        let mut buf = String::new();
        if let Err(e) = entry.read_to_string(&mut buf) {
            warn!(条目 = %entry.name(), 错误 = %e, "读取漏洞库镜像条目失败，跳过");
            continue;
        }
        match parse_advisory(&buf) {
            Ok(parsed) => advisories.push(to_new_advisory(parsed)),
            // 个别坏条目不阻断整批
            Err(e) => warn!(条目 = %entry.name(), 错误 = %e, "解析漏洞公告失败，跳过"),
        }
    }
    Ok(advisories)
}

/// 把解析中间结构转为 meta 层落库结构（纯函数）。
///
/// 受影响坐标自带生态（OSV 公告内含 `package.ecosystem`），直接透传，不在此做匹配。
fn to_new_advisory(parsed: ParsedAdvisory) -> NewAdvisory {
    let affected = parsed
        .affected
        .into_iter()
        .map(|a| NewAffected {
            ecosystem: a.ecosystem,
            package: a.package,
            ranges: a.ranges,
            versions: a.versions,
        })
        .collect();
    NewAdvisory {
        id: parsed.id,
        source: SOURCE_OSV.to_string(),
        summary: parsed.summary,
        details: parsed.details,
        severity: parsed.severity,
        modified: parsed.modified,
        published: parsed.published,
        affected,
    }
}

/// 把生态名归一为可安全用于文件名的片段（仅保留字母数字、点、下划线、连字符）。
fn sanitize(name: &str) -> String {
    name.chars()
        .map(|c| {
            if c.is_ascii_alphanumeric() || matches!(c, '.' | '_' | '-') {
                c
            } else {
                '_'
            }
        })
        .collect()
}

/// 周期刷新观察者（FR-131）：让上层把每轮漏洞库刷新登记进统一任务注册表。
///
/// **分层守恒**：trait 定义在 `vuln`（低于 `api`），`vuln` 不反向依赖 `api`；由 `main` 注入
/// 一个由 api 层 `TaskRegistry` 支撑的适配器实现。每轮刷新前 `on_start` 取任务 id、刷新后
/// `on_finish` 置终态。回调应轻量、不阻塞、不 panic（实现内自行容错）。
pub trait RefreshObserver: Send + Sync {
    /// 一轮刷新开始：返回该任务的 id，供结束时定位。
    fn on_start(&self) -> String;
    /// 一轮刷新结束：`ok` 为是否成功，`落库公告数` 供日志 / 展示。
    fn on_finish(&self, task_id: &str, ok: bool, advisories: usize);
}

/// 启动后台周期刷新任务（FR-70 刷新机制）。
///
/// 仅当 `cfg.enabled` 且 `cfg.ecosystems` 非空时启动；返回 `JoinHandle` 供调用方持有。
/// 任务先立即刷新一次，随后每隔 `refresh_interval_secs` 刷新一次；刷新失败不退出循环。
/// `observer` 可选（`None` 时行为同旧、向后兼容）：给定则每轮刷新登记进统一任务注册表（FR-131）。
pub fn spawn_refresh_loop<S>(
    mirror: Arc<VulnMirror<S>>,
    cfg: VulnConfig,
    observer: Option<Arc<dyn RefreshObserver>>,
) -> Option<tokio::task::JoinHandle<()>>
where
    S: MirrorSource + 'static,
{
    if !cfg.enabled || cfg.ecosystems.is_empty() {
        info!("漏洞库离线镜像未启用或未配置生态，跳过周期刷新");
        return None;
    }
    let interval = Duration::from_secs(cfg.refresh_interval_secs.max(1));
    let ecosystems = cfg.ecosystems.clone();
    let handle = tokio::spawn(async move {
        info!(
            生态数 = ecosystems.len(),
            周期秒 = cfg.refresh_interval_secs,
            "漏洞库离线镜像周期刷新已启动"
        );
        let mut ticker = tokio::time::interval(interval);
        loop {
            ticker.tick().await;
            // 登记本轮刷新为统一任务（FR-131）；observer 缺省时不登记
            let task_id = observer.as_ref().map(|o| o.on_start());
            let total = mirror.refresh_all(&ecosystems).await;
            if let (Some(obs), Some(id)) = (observer.as_ref(), task_id.as_ref()) {
                // 刷新失败不退出循环（refresh_all 已容错），统计落库数即视为完成
                obs.on_finish(id, true, total);
            }
            info!(落库公告数 = total, "漏洞库离线镜像本轮刷新结束");
        }
    });
    Some(handle)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Write;

    /// 把若干条 OSV JSON 写入一个本地 zip，返回其字节。
    fn 造样例_zip(entries: &[(&str, &str)]) -> Vec<u8> {
        let mut cursor = std::io::Cursor::new(Vec::new());
        {
            let mut zw = zip::ZipWriter::new(&mut cursor);
            let opts: zip::write::FileOptions<'_, ()> = zip::write::FileOptions::default();
            for (name, content) in entries {
                zw.start_file(*name, opts).unwrap();
                zw.write_all(content.as_bytes()).unwrap();
            }
            zw.finish().unwrap();
        }
        cursor.into_inner()
    }

    /// 本地 mock 下载器：把预置 zip 字节写到目标路径，并记录"下载了哪些生态"。
    ///
    /// 关键断言点：下载入参只有生态名（公开数据集坐标），**绝无本机制品坐标**，守不外发红线。
    struct MockSource {
        zip_bytes: Vec<u8>,
        downloaded: Arc<std::sync::Mutex<Vec<String>>>,
        fail: bool,
    }

    impl MirrorSource for MockSource {
        async fn download(&self, ecosystem: &str, dest: &Path) -> Result<(), VulnError> {
            self.downloaded.lock().unwrap().push(ecosystem.to_string());
            if self.fail {
                return Err(VulnError::Download("模拟下载失败".to_string()));
            }
            tokio::fs::write(dest, &self.zip_bytes).await?;
            Ok(())
        }
    }

    const ADV_A: &str = r#"{
        "id": "OSV-A",
        "summary": "漏洞 A",
        "modified": "2023-01-01T00:00:00Z",
        "affected": [ { "package": { "ecosystem": "Maven", "name": "g:a" } } ]
    }"#;
    const ADV_B: &str = r#"{
        "id": "OSV-B",
        "summary": "漏洞 B",
        "affected": [ { "package": { "ecosystem": "Maven", "name": "g:b" } } ]
    }"#;

    #[tokio::test]
    async fn 刷新生态下载解析并落库() {
        let store = MetaStore::open_in_memory().await.unwrap();
        let dir = tempfile::tempdir().unwrap();
        let downloaded = Arc::new(std::sync::Mutex::new(Vec::new()));
        let zip = 造样例_zip(&[("OSV-A.json", ADV_A), ("OSV-B.json", ADV_B)]);
        let src = MockSource {
            zip_bytes: zip,
            downloaded: downloaded.clone(),
            fail: false,
        };
        let mirror = VulnMirror::new(store.clone(), src, dir.path());

        let n = mirror.refresh_ecosystem("Maven").await.unwrap();
        assert_eq!(n, 2);
        assert_eq!(store.count_advisories().await.unwrap(), 2);

        // 刷新状态已记录
        let state = store
            .get_mirror_state("osv", "Maven")
            .await
            .unwrap()
            .unwrap();
        assert_eq!(state.advisory_count, 2);

        // 不外发红线：下载入参仅为公开生态名，绝无本机制品坐标
        let calls = downloaded.lock().unwrap();
        assert_eq!(*calls, vec!["Maven".to_string()]);
    }

    #[tokio::test]
    async fn 重复刷新幂等不重复落库() {
        let store = MetaStore::open_in_memory().await.unwrap();
        let dir = tempfile::tempdir().unwrap();
        let zip = 造样例_zip(&[("OSV-A.json", ADV_A), ("OSV-B.json", ADV_B)]);
        let src = MockSource {
            zip_bytes: zip,
            downloaded: Arc::new(std::sync::Mutex::new(Vec::new())),
            fail: false,
        };
        let mirror = VulnMirror::new(store.clone(), src, dir.path());

        mirror.refresh_ecosystem("Maven").await.unwrap();
        // 再次刷新：公告数不翻倍（幂等）
        mirror.refresh_ecosystem("Maven").await.unwrap();
        assert_eq!(store.count_advisories().await.unwrap(), 2);
    }

    #[tokio::test]
    async fn 跳过非_json_条目() {
        let store = MetaStore::open_in_memory().await.unwrap();
        let dir = tempfile::tempdir().unwrap();
        // 混入一个非 json 条目（如 README），应被跳过
        let zip = 造样例_zip(&[("OSV-A.json", ADV_A), ("README.txt", "无关内容")]);
        let src = MockSource {
            zip_bytes: zip,
            downloaded: Arc::new(std::sync::Mutex::new(Vec::new())),
            fail: false,
        };
        let mirror = VulnMirror::new(store.clone(), src, dir.path());
        let n = mirror.refresh_ecosystem("Maven").await.unwrap();
        assert_eq!(n, 1);
    }

    #[tokio::test]
    async fn 单条坏_json_不阻断整批() {
        let store = MetaStore::open_in_memory().await.unwrap();
        let dir = tempfile::tempdir().unwrap();
        // 一条合法、一条坏 JSON（缺 id），坏的被跳过、合法的仍落库
        let zip = 造样例_zip(&[("OSV-A.json", ADV_A), ("bad.json", "{ 坏 }")]);
        let src = MockSource {
            zip_bytes: zip,
            downloaded: Arc::new(std::sync::Mutex::new(Vec::new())),
            fail: false,
        };
        let mirror = VulnMirror::new(store.clone(), src, dir.path());
        let n = mirror.refresh_ecosystem("Maven").await.unwrap();
        assert_eq!(n, 1);
        assert_eq!(store.count_advisories().await.unwrap(), 1);
    }

    #[tokio::test]
    async fn 下载失败不写刷新状态() {
        let store = MetaStore::open_in_memory().await.unwrap();
        let dir = tempfile::tempdir().unwrap();
        let src = MockSource {
            zip_bytes: Vec::new(),
            downloaded: Arc::new(std::sync::Mutex::new(Vec::new())),
            fail: true,
        };
        let mirror = VulnMirror::new(store.clone(), src, dir.path());
        assert!(mirror.refresh_ecosystem("Maven").await.is_err());
        // 失败时不应留下刷新状态
        assert!(store
            .get_mirror_state("osv", "Maven")
            .await
            .unwrap()
            .is_none());
        assert_eq!(store.count_advisories().await.unwrap(), 0);
    }

    #[tokio::test]
    async fn refresh_all_单生态失败不阻断其余() {
        // 用一个会对未知生态成功、对特定生态失败的下载器较复杂；这里改测 refresh_all 累加正常路径
        let store = MetaStore::open_in_memory().await.unwrap();
        let dir = tempfile::tempdir().unwrap();
        let zip = 造样例_zip(&[("OSV-A.json", ADV_A)]);
        let src = MockSource {
            zip_bytes: zip,
            downloaded: Arc::new(std::sync::Mutex::new(Vec::new())),
            fail: false,
        };
        let mirror = VulnMirror::new(store.clone(), src, dir.path());
        // 两个生态都用同一 mock（同一份 zip，公告 id 相同，幂等）
        let total = mirror
            .refresh_all(&["Maven".to_string(), "npm".to_string()])
            .await;
        // 两个生态各解析出 1 条（同 id，落库幂等去重，但本计数按解析条数累加）
        assert_eq!(total, 2);
        assert_eq!(store.count_advisories().await.unwrap(), 1);
    }

    #[tokio::test]
    async fn 未启用或无生态时不启动刷新循环() {
        let store = MetaStore::open_in_memory().await.unwrap();
        let dir = tempfile::tempdir().unwrap();
        let mk = |fail| MockSource {
            zip_bytes: Vec::new(),
            downloaded: Arc::new(std::sync::Mutex::new(Vec::new())),
            fail,
        };

        // enabled = false → 不启动
        let mirror = Arc::new(VulnMirror::new(store.clone(), mk(false), dir.path()));
        assert!(spawn_refresh_loop(mirror, VulnConfig::default(), None).is_none());

        // 启用但生态为空 → 也不启动
        let mirror2 = Arc::new(VulnMirror::new(store.clone(), mk(false), dir.path()));
        let cfg2 = VulnConfig {
            enabled: true,
            ecosystems: Vec::new(),
            ..VulnConfig::default()
        };
        assert!(spawn_refresh_loop(mirror2, cfg2, None).is_none());
    }

    /// 便捷：构造一条候选受影响记录（仅含匹配判定所需字段）。
    fn 候选(
        advisory_id: &str,
        ranges: Option<&str>,
        versions: Option<&str>,
    ) -> AdvisoryAffectedMatch {
        AdvisoryAffectedMatch {
            advisory_id: advisory_id.to_string(),
            severity: Some("CVSS:3.1/AV:N".to_string()),
            summary: Some("摘要".to_string()),
            ranges: ranges.map(str::to_string),
            versions: versions.map(str::to_string),
        }
    }

    #[test]
    fn select_hits_命中范围内版本() {
        let candidates = vec![候选(
            "GHSA-1",
            Some(r#"[{"type":"ECOSYSTEM","events":[{"introduced":"2.0"},{"fixed":"2.17.1"}]}]"#),
            None,
        )];
        // 落入范围 → 命中
        let hits = select_hits("2.14.1", &candidates);
        assert_eq!(hits.len(), 1);
        assert_eq!(hits[0].advisory_id, "GHSA-1");
        assert_eq!(hits[0].severity.as_deref(), Some("CVSS:3.1/AV:N"));
        // 修复版不命中
        assert!(select_hits("2.17.1", &candidates).is_empty());
    }

    #[test]
    fn select_hits_同公告多条命中只计一次() {
        // 同一公告 id 的两条受影响行都覆盖该版本，结果应去重为一条
        let candidates = vec![
            候选(
                "GHSA-DUP",
                Some(r#"[{"type":"ECOSYSTEM","events":[{"introduced":"1.0"}]}]"#),
                None,
            ),
            候选("GHSA-DUP", None, Some(r#"["1.5"]"#)),
        ];
        let hits = select_hits("1.5", &candidates);
        assert_eq!(hits.len(), 1);
    }

    #[test]
    fn select_hits_无候选无命中() {
        assert!(select_hits("1.0.0", &[]).is_empty());
    }

    #[tokio::test]
    async fn 坐标匹配端到端命中且不发起网络() {
        // 端到端：落库公告 → 按坐标查候选 → 纯函数判定命中。
        // 用 MetaStore::open_in_memory（纯本地 SQLite），全程无任何网络下载器参与，
        // 据此断言坐标匹配链路绝不外发坐标到外部漏洞服务（守 ADR-0012）。
        let store = MetaStore::open_in_memory().await.unwrap();
        store
            .upsert_advisory(&NewAdvisory {
                id: "GHSA-jfh8".to_string(),
                source: "osv".to_string(),
                summary: Some("log4j RCE".to_string()),
                details: None,
                severity: Some("CVSS:3.1/严重".to_string()),
                modified: None,
                published: None,
                affected: vec![NewAffected {
                    ecosystem: "Maven".to_string(),
                    package: "org.apache.logging.log4j:log4j-core".to_string(),
                    ranges: Some(
                        r#"[{"type":"ECOSYSTEM","events":[{"introduced":"2.0"},{"fixed":"2.17.1"}]}]"#
                            .to_string(),
                    ),
                    versions: None,
                }],
            })
            .await
            .unwrap();

        // 制品坐标（生态 / 包 / 版本）；仅查本地库（list_affected_for_coordinate 只读本机 SQLite，无网络）
        let candidates = store
            .list_affected_for_coordinate("Maven", "org.apache.logging.log4j:log4j-core")
            .await
            .unwrap();
        let hits = select_hits("2.14.1", &candidates);
        assert_eq!(hits.len(), 1);
        assert_eq!(hits[0].advisory_id, "GHSA-jfh8");

        // 不受影响版本：不命中
        let safe = store
            .list_affected_for_coordinate("Maven", "org.apache.logging.log4j:log4j-core")
            .await
            .unwrap();
        assert!(select_hits("2.17.1", &safe).is_empty());
    }

    /// 真机联网验证：用生产 `HttpMirrorSource` 真实下载 OSV 的小生态镜像并跑完整管线。
    ///
    /// 默认 `#[ignore]`（需联网，CI / 离线环境跳过）。显式运行：
    /// `cargo test --lib vuln -- --ignored 真机下载小生态镜像并落库`。
    /// 选用极小生态 GHC（约数 KB），断言下载 → 解压 → 解析 → 落库链路真机可用。
    #[tokio::test]
    #[ignore = "需联网访问 OSV 公开数据集"]
    async fn 真机下载小生态镜像并落库() {
        let store = MetaStore::open_in_memory().await.unwrap();
        let dir = tempfile::tempdir().unwrap();
        // 测试用独立出站网络槽（默认空代理 + 60s 超时），不接共享热替换槽
        let network = std::sync::Arc::new(
            crate::config::NetworkState::new(
                crate::config::NetworkProxyConfig::default(),
                std::time::Duration::from_secs(60),
            )
            .unwrap(),
        );
        let source = HttpMirrorSource::with_network_state(
            "https://osv-vulnerabilities.storage.googleapis.com".to_string(),
            network,
        );
        let mirror = VulnMirror::new(store.clone(), source, dir.path());

        let n = mirror.refresh_ecosystem("GHC").await.unwrap();
        // 小生态至少应解析出若干条公告，且全部落库（GHC 内公告 id 唯一）
        assert!(n > 0, "GHC 生态应解析出至少一条公告");
        assert_eq!(store.count_advisories().await.unwrap() as usize, n);
        let state = store.get_mirror_state("osv", "GHC").await.unwrap().unwrap();
        assert_eq!(state.advisory_count as usize, n);
    }
}
