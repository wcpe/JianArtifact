//! 指标时序后台采样与清理任务（FR-105，ADR-0027）。
//!
//! 设计要点：
//! - **后台定时采样**：`tokio::time::interval` 按可配间隔每拍采样一组 gauge，组装同一 ts 的
//!   一批样本经 `meta` 落库；采集失败只 WARN、不影响业务。间隔走配置、不硬编码。
//! - **保留期 + 行数兜底清理**：固定清理周期内按保留天数删旧 + 行数上限兜底（沿用 audit 范式）。
//! - **锁外做 DB IO**：先锁共享 `System` 取完主机读数立即释放锁，再做 meta 查询与插入；
//!   持锁期间只做纯内存 + 系统调用的 refresh，不在锁内做 DB IO。
//! - **纯函数可测**：字节 → 百分比、按步长降采样抽成无副作用纯函数，便于穷举单测。
//! - **本机内部、不外发**：时序为本机内部运行数据，落本地、默认不外发。

use std::sync::Arc;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use sysinfo::System;
use tokio::sync::Mutex;
use tokio::task::JoinHandle;

use crate::api::anomaly_ban::BanRegistry;
use crate::api::rate_limit::RateLimiter;
use crate::meta::{MetaStore, MetricSample, NewMetricSample, UsageAction};
use crate::monitor::{self, HostMetrics};

/// 指标键常量（避免魔法串散落）。
pub const KEY_CPU_PERCENT: &str = "host.cpu_percent";
pub const KEY_MEMORY_PERCENT: &str = "host.memory_percent";
pub const KEY_DISK_PERCENT: &str = "host.disk_percent";
pub const KEY_REPO_COUNT: &str = "storage.repo_count";
pub const KEY_BLOB_COUNT: &str = "storage.blob_count";
pub const KEY_TOTAL_BYTES: &str = "storage.total_bytes";
pub const KEY_ACTIVE_BANS: &str = "protection.active_bans";
pub const KEY_RATE_LIMITED_TOTAL: &str = "protection.rate_limited_total";
pub const KEY_ACCESS_TOTAL: &str = "usage.access_total";
pub const KEY_DOWNLOAD_TOTAL: &str = "usage.download_total";

/// 保留期 / 行数兜底清理的扫描周期。
const RETENTION_INTERVAL: Duration = Duration::from_secs(3600);

/// 时序点 DTO（查询响应与降采样输出）。
#[derive(Debug, Clone, PartialEq, serde::Serialize)]
pub struct TsPoint {
    /// 时刻（Unix 毫秒，UTC）。降采样后为桶起点。
    pub ts: i64,
    /// 取值（降采样后为桶内平均）。
    pub value: f64,
}

/// 主机字节读数 → 三项百分比（cpu, mem, disk）。无副作用、除零保护（总量 0 → 0.0）。
///
/// cpu 直接取 `usage_percent`；内存 = 已用 / 总量 × 100；磁盘 =（总量 − 可用）/ 总量 × 100。
pub fn host_percentages(m: &HostMetrics) -> (f64, f64, f64) {
    let cpu = m.cpu.usage_percent as f64;
    let mem = if m.memory.total_bytes == 0 {
        0.0
    } else {
        m.memory.used_bytes as f64 / m.memory.total_bytes as f64 * 100.0
    };
    let disk = if m.disk.total_bytes == 0 {
        0.0
    } else {
        (m.disk.total_bytes - m.disk.available_bytes) as f64 / m.disk.total_bytes as f64 * 100.0
    };
    (cpu, mem, disk)
}

/// 按 `step_ms` 毫秒分桶，桶内取平均；`step_ms <= 0` 视为不降采样（每样本一点）。无副作用。
///
/// 样本已按 ts 升序，用 Vec 顺序聚合（遇到新桶就 flush 上一个），不用 HashMap 以保证顺序确定。
/// 桶起点 = `ts - (ts % step_ms)`，输出按桶起点升序。
pub fn downsample(samples: &[MetricSample], step_ms: i64) -> Vec<TsPoint> {
    if step_ms <= 0 {
        return samples
            .iter()
            .map(|s| TsPoint {
                ts: s.ts,
                value: s.value,
            })
            .collect();
    }

    let mut out: Vec<TsPoint> = Vec::new();
    // 当前桶的归属起点与累加态（和 + 计数），用于求平均
    let mut cur_bucket: Option<i64> = None;
    let mut sum = 0.0f64;
    let mut count = 0u64;
    for s in samples {
        let bucket = s.ts - s.ts.rem_euclid(step_ms);
        match cur_bucket {
            Some(b) if b == bucket => {
                sum += s.value;
                count += 1;
            }
            _ => {
                // 切换到新桶前先 flush 上一个桶的平均
                if let Some(b) = cur_bucket {
                    out.push(TsPoint {
                        ts: b,
                        value: sum / count as f64,
                    });
                }
                cur_bucket = Some(bucket);
                sum = s.value;
                count = 1;
            }
        }
    }
    if let Some(b) = cur_bucket {
        out.push(TsPoint {
            ts: b,
            value: sum / count as f64,
        });
    }
    out
}

/// 当前 Unix 毫秒（UTC）。无 chrono 依赖，用 std 时间计算。
fn now_millis() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_millis() as i64)
        .unwrap_or(0)
}

/// 锁内只取一次主机读数（纯内存 + 系统调用），随即释放锁。磁盘列表按拍新建刷新。
async fn collect_host(host_system: &Arc<Mutex<System>>) -> HostMetrics {
    let mut system = host_system.lock().await;
    let mut disks = sysinfo::Disks::new();
    monitor::collect(&mut system, &mut disks)
    // 锁在此处随 system/disks 一并释放，后续 DB IO 均在锁外
}

/// 采样一组 gauge，组装同一 ts 的一批样本。所有 DB 查询在主机锁释放后进行（锁外做 IO）。
async fn sample_once(
    meta: &MetaStore,
    host_system: &Arc<Mutex<System>>,
    rate_limiter: &RateLimiter,
    ban_registry: &BanRegistry,
) -> Vec<NewMetricSample> {
    let ts = now_millis();

    // 1) 主机读数：锁内取完即释放，再算百分比
    let host = collect_host(host_system).await;
    let (cpu, mem, disk) = host_percentages(&host);

    // 2) 使用分析累计值（counter，存累计、差分留前端）
    let access = meta
        .usage_total_by_action(UsageAction::Access)
        .await
        .unwrap_or(0);
    let download = meta
        .usage_total_by_action(UsageAction::Download)
        .await
        .unwrap_or(0);

    // 3) 存储 / 仓库计数
    let repos = meta.count_repositories().await.unwrap_or(0);
    let blobs = meta.count_distinct_blobs().await.unwrap_or(0);
    let total_bytes = meta.total_blob_bytes().await.unwrap_or(0);

    // 4) 防护进程内计数（活跃封禁数、限流累计被拒数）
    let active_bans = ban_registry.active_ban_count(std::time::Instant::now()) as f64;
    let rate_limited = rate_limiter.rejected_count() as f64;

    let mk = |key: &str, value: f64| NewMetricSample {
        metric_key: key.to_string(),
        ts,
        value,
    };
    vec![
        mk(KEY_CPU_PERCENT, cpu),
        mk(KEY_MEMORY_PERCENT, mem),
        mk(KEY_DISK_PERCENT, disk),
        mk(KEY_REPO_COUNT, repos as f64),
        mk(KEY_BLOB_COUNT, blobs as f64),
        mk(KEY_TOTAL_BYTES, total_bytes as f64),
        mk(KEY_ACTIVE_BANS, active_bans),
        mk(KEY_RATE_LIMITED_TOTAL, rate_limited),
        mk(KEY_ACCESS_TOTAL, access as f64),
        mk(KEY_DOWNLOAD_TOTAL, download as f64),
    ]
}

/// 启动指标时序采样后台任务：按间隔每拍采样一组 gauge 落库；落库失败只 WARN、不影响业务。
pub fn spawn_metrics_sampler(
    meta: MetaStore,
    host_system: Arc<Mutex<System>>,
    rate_limiter: Arc<RateLimiter>,
    ban_registry: Arc<BanRegistry>,
    interval_secs: u64,
) -> JoinHandle<()> {
    tokio::spawn(async move {
        // 间隔至少 1 秒，避免 0 间隔忙转
        let mut ticker = tokio::time::interval(Duration::from_secs(interval_secs.max(1)));
        loop {
            ticker.tick().await;
            let samples = sample_once(&meta, &host_system, &rate_limiter, &ban_registry).await;
            if let Err(e) = meta.insert_metric_samples(&samples).await {
                tracing::warn!(错误 = %e, "指标时序样本落库失败，丢弃本拍（不影响业务）");
            }
        }
    })
}

/// 启动指标时序保留期清理后台任务：周期内按天数删旧 + 行数兜底。
pub fn spawn_metrics_retention(
    meta: MetaStore,
    retention_days: u32,
    max_rows: u64,
) -> JoinHandle<()> {
    tokio::spawn(async move {
        let mut ticker = tokio::time::interval(RETENTION_INTERVAL);
        loop {
            ticker.tick().await;
            let now_ms = now_millis();
            match meta
                .prune_metric_samples_by_age(retention_days, now_ms)
                .await
            {
                Ok(n) if n > 0 => tracing::info!(
                    删除行数 = n,
                    保留天数 = retention_days,
                    "指标时序按保留期轮转完成"
                ),
                Ok(_) => {}
                Err(e) => tracing::warn!(错误 = %e, "指标时序保留期轮转失败"),
            }
            match meta.prune_metric_samples_by_max_rows(max_rows).await {
                Ok(n) if n > 0 => tracing::warn!(
                    删除行数 = n,
                    行数上限 = max_rows,
                    "指标时序超行数上限，已删最旧行"
                ),
                Ok(_) => {}
                Err(e) => tracing::warn!(错误 = %e, "指标时序行数兜底轮转失败"),
            }
        }
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::monitor::{CpuMetrics, DiskMetrics, MemoryMetrics};

    /// 便捷：构造主机指标快照。
    fn 主机(
        cpu: f32,
        mem_total: u64,
        mem_used: u64,
        disk_total: u64,
        disk_avail: u64,
    ) -> HostMetrics {
        HostMetrics {
            cpu: CpuMetrics {
                usage_percent: cpu,
                logical_cores: 4,
            },
            memory: MemoryMetrics {
                total_bytes: mem_total,
                used_bytes: mem_used,
                swap_total_bytes: 0,
                swap_used_bytes: 0,
            },
            disk: DiskMetrics {
                total_bytes: disk_total,
                available_bytes: disk_avail,
                disks: vec![],
            },
            uptime_secs: 1,
        }
    }

    /// 便捷：构造时序样本。
    fn 样本(ts: i64, value: f64) -> MetricSample {
        MetricSample {
            metric_key: "k".to_string(),
            ts,
            value,
        }
    }

    #[test]
    fn 百分比_正常读数() {
        let m = 主机(42.5, 1000, 250, 200, 50);
        let (cpu, mem, disk) = host_percentages(&m);
        assert!((cpu - 42.5).abs() < 1e-9);
        // 内存 250/1000 = 25%
        assert!((mem - 25.0).abs() < 1e-9);
        // 磁盘 (200-50)/200 = 75%
        assert!((disk - 75.0).abs() < 1e-9);
    }

    #[test]
    fn 百分比_总量为零不_panic_返回零() {
        let m = 主机(0.0, 0, 0, 0, 0);
        let (cpu, mem, disk) = host_percentages(&m);
        assert_eq!(cpu, 0.0);
        assert_eq!(mem, 0.0);
        assert_eq!(disk, 0.0);
    }

    #[test]
    fn 降采样_空输入返回空() {
        assert!(downsample(&[], 1000).is_empty());
        assert!(downsample(&[], 0).is_empty());
    }

    #[test]
    fn 降采样_步长非正每样本一点() {
        let s = vec![样本(10, 1.0), 样本(20, 2.0), 样本(30, 3.0)];
        let out = downsample(&s, 0);
        assert_eq!(
            out,
            vec![
                TsPoint { ts: 10, value: 1.0 },
                TsPoint { ts: 20, value: 2.0 },
                TsPoint { ts: 30, value: 3.0 },
            ]
        );
        // 负步长同样不降采样
        assert_eq!(downsample(&s, -5).len(), 3);
    }

    #[test]
    fn 降采样_单桶取平均() {
        // step=100：ts 100/150/199 同属桶 100，平均 (1+2+3)/3=2
        let s = vec![样本(100, 1.0), 样本(150, 2.0), 样本(199, 3.0)];
        let out = downsample(&s, 100);
        assert_eq!(
            out,
            vec![TsPoint {
                ts: 100,
                value: 2.0
            }]
        );
    }

    #[test]
    fn 降采样_跨桶分别平均并按桶升序() {
        // step=100：桶100 含 ts100(2),ts180(4) 平均3；桶200 含 ts200(10) 平均10
        let s = vec![样本(100, 2.0), 样本(180, 4.0), 样本(200, 10.0)];
        let out = downsample(&s, 100);
        assert_eq!(
            out,
            vec![
                TsPoint {
                    ts: 100,
                    value: 3.0
                },
                TsPoint {
                    ts: 200,
                    value: 10.0
                },
            ]
        );
    }

    /// 集成：用缩短间隔（1 秒）spawn 真实采样任务，断言随时间多拍累积出样本。
    ///
    /// 覆盖「后台采样随时间累积」的集成维度（缩短间隔等价长时段真机行为；
    /// 默认 60s / 7 天滚动的长时段真机另行复验，见 spec 验收）。用真实时钟短等待，
    /// 不依赖 tokio `test-util`（未启用该 feature）。
    #[tokio::test]
    async fn 集成_后台采样随时间累积出多拍样本() {
        let meta = MetaStore::open_in_memory().await.unwrap();
        let host_system = Arc::new(Mutex::new(System::new()));
        let rate_limiter = Arc::new(RateLimiter::new());
        let ban_registry = Arc::new(BanRegistry::new());

        let handle = spawn_metrics_sampler(
            meta.clone(),
            host_system,
            rate_limiter,
            ban_registry,
            1, // 1 秒一拍：interval 首拍立即触发，随后每秒一拍
        );

        // 真实等待约 2.2 秒：首拍（t≈0）+ 之后两拍（t≈1、2），应至少落 2 拍
        tokio::time::sleep(Duration::from_millis(2200)).await;
        handle.abort();

        // CPU 指标每拍一条，应累计出多条（≥2，宽松断言避免调度抖动误报）
        let rows = meta
            .query_metric_samples(KEY_CPU_PERCENT, 0, i64::MAX)
            .await
            .unwrap();
        assert!(
            rows.len() >= 2,
            "后台采样应随时间累积出多拍样本，实得 {}",
            rows.len()
        );
    }

    /// 集成：采样落库后用真实 meta 清理，断言保留期滚动只删旧样本、新样本保留。
    #[tokio::test]
    async fn 集成_保留期滚动只删旧样本() {
        let meta = MetaStore::open_in_memory().await.unwrap();
        // 直接构造一旧一新两条样本（旧早于 cutoff、新晚于 cutoff），驱动真实清理
        let day = 86_400_000i64;
        let now = 100 * day;
        meta.insert_metric_samples(&[
            NewMetricSample {
                metric_key: KEY_CPU_PERCENT.to_string(),
                ts: now - 3 * day, // 旧：超出 1 天保留期
                value: 1.0,
            },
            NewMetricSample {
                metric_key: KEY_CPU_PERCENT.to_string(),
                ts: now - 1000, // 新：保留期内
                value: 2.0,
            },
        ])
        .await
        .unwrap();

        let removed = meta.prune_metric_samples_by_age(1, now).await.unwrap();
        assert_eq!(removed, 1, "保留期滚动应只删 1 条旧样本");
        let rows = meta
            .query_metric_samples(KEY_CPU_PERCENT, 0, i64::MAX)
            .await
            .unwrap();
        assert_eq!(rows.len(), 1);
        assert_eq!(rows[0].value, 2.0, "保留下来的应为新样本");
    }
}
