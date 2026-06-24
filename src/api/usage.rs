//! 使用分析采集（FR-57，ADR-0009）：异步采集访问 / 下载事件，聚合落 SQLite。
//!
//! 设计（沿用 FR-31 审计同款异步有界 channel + 写入任务模式）：
//! - **异步不阻塞**：事件经进程内有界 channel 投递给独立写入任务批量聚合入库；主请求路径只做
//!   一次非阻塞 `try_send`，**采集 / 写入失败只记 WARN、不影响业务**；channel 满时按
//!   "丢弃并计数 + WARN"降级，绝不反压主路径（testing-and-quality §2.8）。
//! - **聚合为主、明细可选**：聚合计数 UPSERT 累加（并发下计数准确）；明细仅在配置开启时写入，
//!   行数兜底由后台裁剪任务，避免撑爆 SQLite。
//! - **隐私红线**：统计数据落本地、**默认不外发、不向外部遥测 phone-home**；任何外部导出默认
//!   关闭（本批不做导出）。`actor` 只记用户名或 anonymous，绝不记凭据。
//! - **采集点**：制品下载 / 访问成功路径（格式 GET 命中 200 后按读成功计数）。

use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;
use std::time::Duration;

use tokio::sync::mpsc;

use crate::meta::{MetaStore, NewUsageEvent, UsageAction};

/// 使用事件 channel 容量（有界）：满则丢弃 + 计数，绝不反压主路径。
const USAGE_CHANNEL_CAPACITY: usize = 4096;
/// 写入任务单批最大条数：达到即落库，平衡时延与批处理收益。
const USAGE_BATCH_MAX: usize = 64;
/// 写入任务批间最长等待：不足一批时也会在该间隔内落库，避免事件长时间滞留。
const USAGE_FLUSH_INTERVAL: Duration = Duration::from_millis(500);
/// 明细量级兜底裁剪的扫描周期。
const USAGE_PRUNE_INTERVAL: Duration = Duration::from_secs(3600);

/// 使用采集投递端：克隆廉价（内含 channel sender 与丢弃计数 Arc），随 AppState 共享。
///
/// 主路径只调用 `record` 做一次非阻塞投递；写入与裁剪在独立后台任务进行。
#[derive(Clone)]
pub struct UsageSink {
    sender: mpsc::Sender<NewUsageEvent>,
    /// channel 满而被丢弃的事件累计数（供观测 / 后续指标埋点）。
    dropped: Arc<AtomicU64>,
}

impl UsageSink {
    /// 非阻塞投递一条使用事件。channel 满时丢弃并计数 + WARN，绝不阻塞主路径。
    fn enqueue(&self, event: NewUsageEvent) {
        match self.sender.try_send(event) {
            Ok(()) => {}
            Err(mpsc::error::TrySendError::Full(dropped)) => {
                let total = self.dropped.fetch_add(1, Ordering::Relaxed) + 1;
                tracing::warn!(
                    仓库 = %dropped.repo_name,
                    动作 = %dropped.action,
                    累计丢弃 = total,
                    "使用统计队列已满，丢弃本条事件（采集降级，不影响业务）"
                );
            }
            Err(mpsc::error::TrySendError::Closed(_)) => {
                // 写入任务已退出（仅发生在停机阶段），按降级处理不报错
                tracing::warn!("使用统计写入任务已关闭，丢弃事件");
            }
        }
    }

    /// 记录一次制品访问 / 下载。由格式 GET 路径在读成功后调用（非阻塞）。
    ///
    /// `actor` 为用户名或 `anonymous`，绝不记凭据；`source_ip` 可空。
    pub fn record(
        &self,
        action: UsageAction,
        repo_name: &str,
        repo_path: &str,
        actor: &str,
        source_ip: Option<&str>,
    ) {
        self.enqueue(NewUsageEvent {
            repo_name: repo_name.to_string(),
            repo_path: repo_path.to_string(),
            action: action.as_str().to_string(),
            actor: actor.to_string(),
            source_ip: source_ip.map(str::to_owned),
        });
    }

    /// 已丢弃事件累计数（供测试与后续指标读取）。
    pub fn dropped_count(&self) -> u64 {
        self.dropped.load(Ordering::Relaxed)
    }
}

/// 创建使用采集投递端与配套接收端。接收端交由 `spawn_usage_writer` 消费。
pub fn channel() -> (UsageSink, mpsc::Receiver<NewUsageEvent>) {
    let (sender, receiver) = mpsc::channel(USAGE_CHANNEL_CAPACITY);
    let sink = UsageSink {
        sender,
        dropped: Arc::new(AtomicU64::new(0)),
    };
    (sink, receiver)
}

/// 启动使用采集写入后台任务：从 channel 聚批聚合写入 SQLite。
///
/// `write_detail` 控制是否同时落明细；落库失败只记 WARN、丢弃该批，不让采集失败影响业务。
/// 所有 sender 释放后 channel 关闭，任务收尾退出。
pub fn spawn_usage_writer(
    meta: MetaStore,
    mut receiver: mpsc::Receiver<NewUsageEvent>,
    write_detail: bool,
) {
    tokio::spawn(async move {
        let mut batch: Vec<NewUsageEvent> = Vec::with_capacity(USAGE_BATCH_MAX);
        loop {
            // 先阻塞等第一条；channel 关闭则把残余落库后退出
            let first = match receiver.recv().await {
                Some(e) => e,
                None => {
                    flush_batch(&meta, &mut batch, write_detail).await;
                    break;
                }
            };
            batch.push(first);

            // 在 flush 间隔内尽量多收几条凑批，超时或满批即落库
            let _ = tokio::time::timeout(USAGE_FLUSH_INTERVAL, async {
                while batch.len() < USAGE_BATCH_MAX {
                    match receiver.recv().await {
                        Some(e) => batch.push(e),
                        None => break,
                    }
                }
            })
            .await;

            flush_batch(&meta, &mut batch, write_detail).await;
        }
    });
}

/// 落库一批使用事件；失败只记 WARN 并清空该批（采集失败不影响业务）。
async fn flush_batch(meta: &MetaStore, batch: &mut Vec<NewUsageEvent>, write_detail: bool) {
    if batch.is_empty() {
        return;
    }
    if let Err(e) = meta.insert_usage_batch(batch, write_detail).await {
        tracing::warn!(错误 = %e, 条数 = batch.len(), "使用统计批量写入失败，丢弃本批（不影响业务）");
    }
    batch.clear();
}

/// 启动明细量级兜底裁剪后台任务：周期性按行数上限删最旧明细。
///
/// 仅在开启明细时有意义；聚合计数是长期统计真源、不随之裁剪。
pub fn spawn_usage_pruner(meta: MetaStore, max_rows: u64) {
    tokio::spawn(async move {
        let mut ticker = tokio::time::interval(USAGE_PRUNE_INTERVAL);
        loop {
            ticker.tick().await;
            match meta.prune_usage_events_by_max_rows(max_rows).await {
                Ok(n) if n > 0 => {
                    tracing::info!(
                        删除行数 = n,
                        行数上限 = max_rows,
                        "使用明细超行数上限，已删最旧行"
                    )
                }
                Ok(_) => {}
                Err(e) => tracing::warn!(错误 = %e, "使用明细行数兜底裁剪失败"),
            }
        }
    });
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn 满队列丢弃并计数() {
        // 容量 1：写满后再投递应被丢弃并计数，绝不阻塞
        let (sender, _receiver) = mpsc::channel(1);
        let sink = UsageSink {
            sender,
            dropped: Arc::new(AtomicU64::new(0)),
        };
        sink.record(UsageAction::Download, "libs", "a.jar", "anonymous", None); // 占满容量
        sink.record(UsageAction::Download, "libs", "a.jar", "anonymous", None); // 丢弃 + 计数
        sink.record(UsageAction::Download, "libs", "a.jar", "anonymous", None); // 再丢弃 + 计数
        assert_eq!(sink.dropped_count(), 2);
    }

    #[tokio::test]
    async fn 采集后写入任务聚合落库() {
        let meta = MetaStore::open_in_memory().await.unwrap();
        let (sink, rx) = channel();
        spawn_usage_writer(meta.clone(), rx, false);

        // 同一制品下载 3 次，聚合应累加为 3
        for _ in 0..3 {
            sink.record(
                UsageAction::Download,
                "libs",
                "a.jar",
                "dev",
                Some("10.0.0.1"),
            );
        }
        // 关闭 sink 触发写入任务收尾刷库，等待其完成
        drop(sink);

        // 轮询等聚合落库（写入任务异步，给其完成时间；不依赖固定睡眠时长）
        let mut count = 0;
        for _ in 0..50 {
            count = meta
                .usage_count("libs", "a.jar", UsageAction::Download)
                .await
                .unwrap();
            if count == 3 {
                break;
            }
            tokio::time::sleep(Duration::from_millis(20)).await;
        }
        assert_eq!(count, 3);
        // 关明细：不应落明细行
        assert_eq!(meta.count_usage_events().await.unwrap(), 0);
    }
}
