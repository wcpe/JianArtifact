//! 防护监控与阈值告警（FR-56，ADR-0017）。
//!
//! 在既有七层防护（限流 / 自动封禁 / CC 挑战 / WAF / 慢速攻击）的命中点上挂一个**进程内告警
//! 评估器**：在固定时间窗内按维度累加防护事件计数，单维度窗内计数达阈值即产生一条告警——按
//! 严重度记**中文分级日志**（WARN）并**异步不阻塞落 SQLite**（`protection_alerts` 表）。
//!
//! 设计要点（对齐 testing-and-quality §2.7 / §2.8 与架构不变量）：
//! - **热路径低开销**：`record` 每次只取一次该维度的 `Mutex`、做整型自增与窗口比较，临界区内
//!   **无 IO、无格式化、无锁内落库**；告警入库经有界 channel 异步投递（非阻塞 `try_send`）。
//! - **去抖防刷屏**：每个维度在一个时间窗内**只告警一次**；跨窗计数清零、去抖标志复位，下一窗可再告警。
//! - **防误报**：默认关闭；阈值默认保守宽放（与 ban / cc 默认关闭风格一致），正常高频访问 / 合法
//!   批量拉取不应触顶；未启用时 `record` 直接返回，零计数开销。
//! - **数据不外发**：告警是本机内部数据，只落本地 SQLite、不内置外发型通知（Webhook / 邮件等
//!   若未来要做须另写 ADR）；日志不打印凭据，IP 之外不必要的隐私不记。
//! - **采集失败不影响业务**：channel 满 / 写库失败只记 WARN、丢弃，绝不反压主路径（同审计 / 使用分析范式）。

use std::collections::HashMap;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use tokio::sync::mpsc;

use crate::config::AlertsConfig;
use crate::meta::{MetaStore, NewAlert};

/// 告警 channel 容量（有界）：满则丢弃 + 计数，绝不反压主路径。
const ALERT_CHANNEL_CAPACITY: usize = 1024;
/// 写入任务单批最大条数：达到即落库。告警量远小于审计 / 使用，单批即可。
const ALERT_BATCH_MAX: usize = 32;
/// 写入任务批间最长等待：不足一批时也会在该间隔内落库，避免事件长时间滞留。
const ALERT_FLUSH_INTERVAL: Duration = Duration::from_millis(500);
/// 行数兜底裁剪的扫描周期。
const ALERT_PRUNE_INTERVAL: Duration = Duration::from_secs(3600);

/// 防护维度：告警与窗内计数按此枚举归类，入库为有界小写字符串（避免魔法字符串散落）。
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum ProtectionDimension {
    /// 限流被拒（含并发上限被拒）。
    RateLimit,
    /// 自动封禁触发。
    Ban,
    /// CC 挑战证明校验失败。
    CcChallenge,
    /// WAF 阻断。
    Waf,
    /// 慢速攻击超时 / 截断拒绝。
    Slowloris,
}

impl ProtectionDimension {
    /// 入库 / 上报字符串。
    pub fn as_str(self) -> &'static str {
        match self {
            ProtectionDimension::RateLimit => "rate_limit",
            ProtectionDimension::Ban => "ban",
            ProtectionDimension::CcChallenge => "cc_challenge",
            ProtectionDimension::Waf => "waf",
            ProtectionDimension::Slowloris => "slowloris",
        }
    }

    /// 全部维度（供状态端点快照遍历与窗口滚动）。
    pub const ALL: [ProtectionDimension; 5] = [
        ProtectionDimension::RateLimit,
        ProtectionDimension::Ban,
        ProtectionDimension::CcChallenge,
        ProtectionDimension::Waf,
        ProtectionDimension::Slowloris,
    ];
}

/// 告警严重度。以小写字符串入库。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Severity {
    /// 警告级。
    Warn,
    /// 错误级（窗内计数远超阈值时升级）。
    Error,
}

impl Severity {
    /// 入库字符串。
    fn as_str(self) -> &'static str {
        match self {
            Severity::Warn => "warn",
            Severity::Error => "error",
        }
    }
}

/// 升级为 Error 的倍数：窗内观测值达阈值的该倍数时把严重度从 Warn 升级为 Error。
const ERROR_ESCALATION_FACTOR: u64 = 5;

/// 单个维度在当前窗内的计数与去抖状态。
#[derive(Debug, Clone, Copy)]
struct DimensionWindow {
    /// 当前窗内累计的事件计数。
    count: u64,
    /// 当前窗起始时刻；距今超过窗口时长即翻入新窗、计数清零、去抖复位。
    window_start: Instant,
    /// 本窗内是否已就该维度告警过（去抖：一窗内只告警一次，不刷屏）。
    alerted: bool,
}

impl DimensionWindow {
    /// 新建一个以 `now` 为窗起点、计数与去抖归零的窗状态。
    fn fresh(now: Instant) -> Self {
        Self {
            count: 0,
            window_start: now,
            alerted: false,
        }
    }
}

/// 告警投递端：克隆廉价（内含 channel sender 与丢弃计数 Arc），随 AppState 共享。
///
/// 主路径只调用 `enqueue` 做一次非阻塞投递；写入与裁剪在独立后台任务进行。
#[derive(Clone)]
pub struct AlertSink {
    sender: mpsc::Sender<NewAlert>,
    /// channel 满而被丢弃的告警累计数（供观测）。
    dropped: Arc<AtomicU64>,
}

impl AlertSink {
    /// 非阻塞投递一条告警。channel 满时丢弃并计数 + WARN，绝不阻塞主路径。
    fn enqueue(&self, alert: NewAlert) {
        match self.sender.try_send(alert) {
            Ok(()) => {}
            Err(mpsc::error::TrySendError::Full(dropped)) => {
                let total = self.dropped.fetch_add(1, Ordering::Relaxed) + 1;
                tracing::warn!(
                    维度 = %dropped.dimension,
                    累计丢弃 = total,
                    "防护告警队列已满，丢弃本条告警（采集降级，不影响业务）"
                );
            }
            Err(mpsc::error::TrySendError::Closed(_)) => {
                // 写入任务已退出（仅发生在停机阶段），按降级处理不报错
                tracing::warn!("防护告警写入任务已关闭，丢弃告警");
            }
        }
    }

    /// 已丢弃告警累计数（供测试与观测读取）。
    pub fn dropped_count(&self) -> u64 {
        self.dropped.load(Ordering::Relaxed)
    }
}

/// 创建告警投递端与配套接收端。接收端交由 `spawn_alert_writer` 消费。
pub fn channel() -> (AlertSink, mpsc::Receiver<NewAlert>) {
    let (sender, receiver) = mpsc::channel(ALERT_CHANNEL_CAPACITY);
    let sink = AlertSink {
        sender,
        dropped: Arc::new(AtomicU64::new(0)),
    };
    (sink, receiver)
}

/// 启动告警写入后台任务：从 channel 聚批写入 SQLite。
///
/// 落库失败只记 WARN、丢弃该批，不让采集失败影响业务。所有 sender 释放后 channel 关闭，任务收尾退出。
pub fn spawn_alert_writer(meta: MetaStore, mut receiver: mpsc::Receiver<NewAlert>) {
    tokio::spawn(async move {
        let mut batch: Vec<NewAlert> = Vec::with_capacity(ALERT_BATCH_MAX);
        loop {
            let first = match receiver.recv().await {
                Some(a) => a,
                None => {
                    flush_batch(&meta, &mut batch).await;
                    break;
                }
            };
            batch.push(first);

            let _ = tokio::time::timeout(ALERT_FLUSH_INTERVAL, async {
                while batch.len() < ALERT_BATCH_MAX {
                    match receiver.recv().await {
                        Some(a) => batch.push(a),
                        None => break,
                    }
                }
            })
            .await;

            flush_batch(&meta, &mut batch).await;
        }
    });
}

/// 落库一批告警；失败只记 WARN 并清空该批（采集失败不影响业务）。
async fn flush_batch(meta: &MetaStore, batch: &mut Vec<NewAlert>) {
    if batch.is_empty() {
        return;
    }
    if let Err(e) = meta.insert_alert_batch(batch).await {
        tracing::warn!(错误 = %e, 条数 = batch.len(), "防护告警批量写入失败，丢弃本批（不影响业务）");
    }
    batch.clear();
}

/// 启动告警行数兜底裁剪后台任务：周期性按行数上限删最旧。
pub fn spawn_alert_pruner(meta: MetaStore, max_rows: u64) {
    tokio::spawn(async move {
        let mut ticker = tokio::time::interval(ALERT_PRUNE_INTERVAL);
        loop {
            ticker.tick().await;
            match meta.prune_alerts_by_max_rows(max_rows).await {
                Ok(n) if n > 0 => {
                    tracing::info!(
                        删除行数 = n,
                        行数上限 = max_rows,
                        "防护告警超行数上限，已删最旧行"
                    )
                }
                Ok(_) => {}
                Err(e) => tracing::warn!(错误 = %e, "防护告警行数兜底裁剪失败"),
            }
        }
    });
}

/// 进程内防护告警评估器：随 `AppState` 经 `Arc` 共享。
///
/// 在各防护中间件命中点调用 [`AlertEngine::record`]：按维度累加窗内计数，达阈值且本窗未告警过即
/// 记中文分级日志并经 `AlertSink` 异步投递一条告警入库。计数 / 去抖状态进程内内存维护（重启即清）。
pub struct AlertEngine {
    /// 各维度的窗内计数与去抖状态（每维度一把锁，降低跨维度争用）。
    windows: HashMap<ProtectionDimension, Mutex<DimensionWindow>>,
    /// 告警投递端（异步落库）。
    sink: AlertSink,
}

impl AlertEngine {
    /// 用给定投递端构造评估器，各维度窗状态以 `now` 为初始窗起点。
    pub fn new(sink: AlertSink) -> Self {
        let now = Instant::now();
        let mut windows = HashMap::new();
        for d in ProtectionDimension::ALL {
            windows.insert(d, Mutex::new(DimensionWindow::fresh(now)));
        }
        Self { windows, sink }
    }

    /// 记录某维度发生一次防护事件（在中间件命中点调用），按配置阈值判定是否告警。
    ///
    /// `cfg` 取自 `AppState.config`（配置热替换后下次调用即按新阈值判定）；`now` 由调用方传入便于
    /// 测试可控时钟。未启用（`cfg.enabled=false`）时直接返回、零计数开销。
    ///
    /// 评估纯在内存态完成：达阈值且本窗未告警过 → 记日志 + 异步投递告警，并置去抖标志。
    /// 锁内只做整型计数与窗口比较，**日志与投递在锁外**完成（不持锁做格式化 / channel 发送）。
    pub fn record(&self, dimension: ProtectionDimension, cfg: &AlertsConfig, now: Instant) {
        if !cfg.enabled {
            return;
        }
        let window = Duration::from_secs(cfg.window_secs.max(1));
        let threshold = threshold_for(dimension, cfg);
        // 阈值 0 视作该维度不告警（避免误配 0 导致每次都告警）
        if threshold == 0 {
            return;
        }

        // —— 锁内：累加窗内计数、判定是否本次跨阈值（首次达阈值且本窗未告警过）——
        let crossed = {
            let cell = match self.windows.get(&dimension) {
                Some(c) => c,
                None => return,
            };
            let mut w = cell.lock().unwrap_or_else(|e| e.into_inner());
            if now.duration_since(w.window_start) >= window {
                *w = DimensionWindow::fresh(now);
            }
            w.count += 1;
            if w.count >= threshold && !w.alerted {
                w.alerted = true;
                Some(w.count)
            } else {
                None
            }
        };

        // —— 锁外：达阈值则记中文分级日志 + 异步投递告警入库 ——
        if let Some(observed) = crossed {
            self.emit(dimension, observed, threshold, cfg.window_secs.max(1));
        }
    }

    /// 当前各维度窗内计数快照（供状态端点聚合，不改动状态）。
    ///
    /// 同时顺带把已越窗的维度滚动到新窗（清零计数 + 复位去抖），使快照反映「当前窗」而非陈旧窗。
    pub fn snapshot(&self, now: Instant, window_secs: u64) -> Vec<DimensionCount> {
        let window = Duration::from_secs(window_secs.max(1));
        let mut out = Vec::with_capacity(ProtectionDimension::ALL.len());
        for d in ProtectionDimension::ALL {
            let count = if let Some(cell) = self.windows.get(&d) {
                let mut w = cell.lock().unwrap_or_else(|e| e.into_inner());
                if now.duration_since(w.window_start) >= window {
                    *w = DimensionWindow::fresh(now);
                }
                w.count
            } else {
                0
            };
            out.push(DimensionCount {
                dimension: d,
                count,
            });
        }
        out
    }

    /// 记中文分级日志并异步投递告警入库（锁外调用）。
    fn emit(
        &self,
        dimension: ProtectionDimension,
        observed: u64,
        threshold: u64,
        window_secs: u64,
    ) {
        let severity = severity_for(observed, threshold);
        let detail = format!(
            "防护维度 {} 在 {} 秒窗内计数 {} 达阈值 {}",
            dimension.as_str(),
            window_secs,
            observed,
            threshold
        );
        // 按严重度记中文分级日志（不打印凭据 / 不必要隐私；仅记维度与计数等上下文）
        match severity {
            Severity::Warn => tracing::warn!(
                维度 = dimension.as_str(),
                当前值 = observed,
                阈值 = threshold,
                时间窗秒 = window_secs,
                "防护事件窗内计数达阈值，已告警"
            ),
            Severity::Error => tracing::error!(
                维度 = dimension.as_str(),
                当前值 = observed,
                阈值 = threshold,
                时间窗秒 = window_secs,
                "防护事件窗内计数远超阈值，已升级告警"
            ),
        }
        self.sink.enqueue(NewAlert {
            dimension: dimension.as_str().to_string(),
            severity: severity.as_str().to_string(),
            observed_value: observed as i64,
            threshold: threshold as i64,
            window_secs: window_secs as i64,
            detail: Some(detail),
        });
    }
}

/// 某维度的当前窗内计数（状态端点快照项）。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct DimensionCount {
    /// 防护维度。
    pub dimension: ProtectionDimension,
    /// 当前窗内累计计数。
    pub count: u64,
}

/// 取某维度对应的窗内告警阈值（纯函数，便于穷举测试）。
fn threshold_for(dimension: ProtectionDimension, cfg: &AlertsConfig) -> u64 {
    match dimension {
        ProtectionDimension::RateLimit => cfg.rate_limit_warn_threshold,
        ProtectionDimension::Ban => cfg.ban_warn_threshold,
        ProtectionDimension::CcChallenge => cfg.cc_challenge_fail_warn_threshold,
        ProtectionDimension::Waf => cfg.waf_block_warn_threshold,
        ProtectionDimension::Slowloris => cfg.slowloris_warn_threshold,
    }
}

/// 按观测值与阈值的比值决定严重度：达阈值即 Warn，远超阈值（≥ 倍数）升级为 Error（纯函数）。
fn severity_for(observed: u64, threshold: u64) -> Severity {
    if threshold > 0 && observed >= threshold.saturating_mul(ERROR_ESCALATION_FACTOR) {
        Severity::Error
    } else {
        Severity::Warn
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// 便捷：构造启用的告警配置（各维度阈值与窗口可定制）。
    fn 配置(enabled: bool, window_secs: u64, 各阈值: u64) -> AlertsConfig {
        AlertsConfig {
            enabled,
            window_secs,
            rate_limit_warn_threshold: 各阈值,
            ban_warn_threshold: 各阈值,
            cc_challenge_fail_warn_threshold: 各阈值,
            waf_block_warn_threshold: 各阈值,
            slowloris_warn_threshold: 各阈值,
            max_rows: 100_000,
        }
    }

    /// 便捷：构造引擎并返回其接收端，便于断言投递的告警内容。
    fn 引擎() -> (AlertEngine, mpsc::Receiver<NewAlert>) {
        let (sink, rx) = channel();
        (AlertEngine::new(sink), rx)
    }

    #[test]
    fn 严重度按倍数升级() {
        assert_eq!(severity_for(0, 10), Severity::Warn);
        assert_eq!(severity_for(10, 10), Severity::Warn);
        assert_eq!(severity_for(49, 10), Severity::Warn);
        // 达 5 倍升级为 Error
        assert_eq!(severity_for(50, 10), Severity::Error);
    }

    #[test]
    fn 阈值映射各维度独立() {
        let cfg = AlertsConfig {
            enabled: true,
            window_secs: 300,
            rate_limit_warn_threshold: 1,
            ban_warn_threshold: 2,
            cc_challenge_fail_warn_threshold: 3,
            waf_block_warn_threshold: 4,
            slowloris_warn_threshold: 5,
            max_rows: 100_000,
        };
        assert_eq!(threshold_for(ProtectionDimension::RateLimit, &cfg), 1);
        assert_eq!(threshold_for(ProtectionDimension::Ban, &cfg), 2);
        assert_eq!(threshold_for(ProtectionDimension::CcChallenge, &cfg), 3);
        assert_eq!(threshold_for(ProtectionDimension::Waf, &cfg), 4);
        assert_eq!(threshold_for(ProtectionDimension::Slowloris, &cfg), 5);
    }

    #[tokio::test]
    async fn 未启用时不计数不告警() {
        let (engine, mut rx) = 引擎();
        let cfg = 配置(false, 300, 1);
        let now = Instant::now();
        for _ in 0..10 {
            engine.record(ProtectionDimension::Waf, &cfg, now);
        }
        // 关闭：无任何告警投递
        assert!(rx.try_recv().is_err());
        // 快照计数仍为 0（未计数）
        let snap = engine.snapshot(now, 300);
        assert!(snap.iter().all(|d| d.count == 0));
    }

    #[tokio::test]
    async fn 达阈值告警一次去抖不刷屏() {
        let (engine, mut rx) = 引擎();
        let cfg = 配置(true, 300, 3);
        let now = Instant::now();
        // 前 2 次未达阈值，不告警
        engine.record(ProtectionDimension::RateLimit, &cfg, now);
        engine.record(ProtectionDimension::RateLimit, &cfg, now);
        assert!(rx.try_recv().is_err(), "未达阈值不应告警");
        // 第 3 次达阈值，告警一次
        engine.record(ProtectionDimension::RateLimit, &cfg, now);
        let a = rx.try_recv().expect("达阈值应告警一次");
        assert_eq!(a.dimension, "rate_limit");
        assert_eq!(a.observed_value, 3);
        assert_eq!(a.threshold, 3);
        // 同窗继续累加：去抖，不再刷新告警
        for _ in 0..10 {
            engine.record(ProtectionDimension::RateLimit, &cfg, now);
        }
        assert!(rx.try_recv().is_err(), "同窗同维度只告警一次（去抖）");
    }

    #[tokio::test]
    async fn 窗口滚动后重新计数可再次告警() {
        let (engine, mut rx) = 引擎();
        let cfg = 配置(true, 60, 2);
        let t0 = Instant::now();
        // 第一窗达阈值告警
        engine.record(ProtectionDimension::Ban, &cfg, t0);
        engine.record(ProtectionDimension::Ban, &cfg, t0);
        assert!(rx.try_recv().is_ok(), "第一窗应告警");
        // 跨窗：计数清零、去抖复位
        let t1 = t0 + Duration::from_secs(61);
        engine.record(ProtectionDimension::Ban, &cfg, t1);
        assert!(rx.try_recv().is_err(), "新窗未达阈值不应告警");
        engine.record(ProtectionDimension::Ban, &cfg, t1);
        assert!(rx.try_recv().is_ok(), "新窗达阈值应再次告警");
    }

    #[tokio::test]
    async fn 维度间互不影响() {
        let (engine, mut rx) = 引擎();
        let cfg = 配置(true, 300, 2);
        let now = Instant::now();
        // waf 达阈值，cc_challenge 仅 1 次未达
        engine.record(ProtectionDimension::Waf, &cfg, now);
        engine.record(ProtectionDimension::Waf, &cfg, now);
        engine.record(ProtectionDimension::CcChallenge, &cfg, now);
        let a = rx.try_recv().expect("waf 达阈值应告警");
        assert_eq!(a.dimension, "waf");
        // 只有 waf 一条告警，cc_challenge 不告警
        assert!(rx.try_recv().is_err());
    }

    #[tokio::test]
    async fn 正常高频但未达阈值不误报() {
        let (engine, mut rx) = 引擎();
        // 阈值很高，模拟正常高频访问下偶发防护事件远不及阈值
        let cfg = 配置(true, 300, 10_000);
        let now = Instant::now();
        for _ in 0..500 {
            engine.record(ProtectionDimension::Slowloris, &cfg, now);
        }
        assert!(rx.try_recv().is_err(), "未达高阈值不应误报");
        let snap = engine.snapshot(now, 300);
        let slow = snap
            .iter()
            .find(|d| d.dimension == ProtectionDimension::Slowloris)
            .unwrap();
        assert_eq!(slow.count, 500);
    }

    #[tokio::test]
    async fn 并发计数准确达阈值只告警一次() {
        use std::sync::Arc;
        let (sink, mut rx) = channel();
        let engine = Arc::new(AlertEngine::new(sink));
        let now = Instant::now();
        let threshold = 200u64;
        let cfg = Arc::new(配置(true, 300, threshold));
        let threads = 8;
        let per = 50u64; // 总 400，远超阈值 200
        let mut handles = Vec::new();
        for _ in 0..threads {
            let engine = Arc::clone(&engine);
            let cfg = Arc::clone(&cfg);
            handles.push(std::thread::spawn(move || {
                for _ in 0..per {
                    engine.record(ProtectionDimension::RateLimit, &cfg, now);
                }
            }));
        }
        for h in handles {
            h.join().unwrap();
        }
        // 并发达阈值只应告警一次（去抖在锁内判定，不重复）
        let mut count = 0;
        while rx.try_recv().is_ok() {
            count += 1;
        }
        assert_eq!(count, 1, "并发达阈值应只告警一次");
        // 窗内计数应为总事件数（无丢计 / 无重复）
        let snap = engine.snapshot(now, 300);
        let rl = snap
            .iter()
            .find(|d| d.dimension == ProtectionDimension::RateLimit)
            .unwrap();
        assert_eq!(rl.count, threads as u64 * per);
    }

    #[tokio::test]
    async fn 满队列丢弃并计数不阻塞() {
        // 容量 1：写满后再投递应被丢弃并计数，绝不阻塞
        let (sender, _receiver) = mpsc::channel(1);
        let sink = AlertSink {
            sender,
            dropped: Arc::new(AtomicU64::new(0)),
        };
        let mk = || NewAlert {
            dimension: "waf".into(),
            severity: "warn".into(),
            observed_value: 1,
            threshold: 1,
            window_secs: 300,
            detail: None,
        };
        sink.enqueue(mk()); // 占满
        sink.enqueue(mk()); // 丢弃 + 计数
        sink.enqueue(mk()); // 再丢弃 + 计数
        assert_eq!(sink.dropped_count(), 2);
    }

    #[tokio::test]
    async fn 告警异步落库不阻塞() {
        let meta = MetaStore::open_in_memory().await.unwrap();
        let (sink, rx) = channel();
        spawn_alert_writer(meta.clone(), rx);
        let engine = AlertEngine::new(sink);
        let cfg = 配置(true, 300, 1);
        let now = Instant::now();
        // 触发一条 waf 告警
        engine.record(ProtectionDimension::Waf, &cfg, now);
        // 触发一条 ban 告警
        engine.record(ProtectionDimension::Ban, &cfg, now);
        // 关闭 engine（drop sink）触发写任务收尾刷库
        drop(engine);

        // 轮询等落库（异步写入任务）
        let mut total = 0;
        for _ in 0..50 {
            total = meta.count_alerts_total().await.unwrap();
            if total == 2 {
                break;
            }
            tokio::time::sleep(Duration::from_millis(20)).await;
        }
        assert_eq!(total, 2, "两条告警应异步落库");
    }
}
