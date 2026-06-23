//! 代理缓存与单飞合并（FR-12，ADR-0005）：proxy 仓库 cache-miss 时从上游拉取、
//! 校验、落盘、写索引；同一制品的并发 cache-miss 经**单飞合并**为一次上游拉取，
//! 上游失败按策略回退且**绝不把损坏内容写入缓存**。
//!
//! 关键约束（testing-and-quality §2.3 / §3）：
//! - **锁外做 IO**：单飞临界区只保护"in-flight 归属判定"，上游拉取 / 落盘 / 查库一律在锁外。
//! - 上游不可用 / 超时 / 错误时回退，不缓存损坏内容。
//!
//! 上游传输用 `reqwest`（纯 rustls，校验 HTTPS 证书）；为便于穷举竞态测试，
//! 上游拉取经 [`Upstream`] trait 抽象，生产实现为 [`HttpUpstream`]，测试可注入计数 mock。

use std::collections::HashMap;
use std::sync::{Arc, Mutex, Weak};

use tokio::io::AsyncRead;
use tokio::sync::{Notify, OnceCell};

mod http;

pub use http::HttpUpstream;

/// 上游拉取错误。
#[derive(Debug, thiserror::Error)]
pub enum UpstreamError {
    /// 上游返回非成功状态（如 404 / 5xx）。
    #[error("上游返回错误状态: {0}")]
    Status(u16),
    /// 上游不可用 / 超时 / 传输失败。
    #[error("上游请求失败: {0}")]
    Transport(String),
}

/// 上游响应体：以 `AsyncRead` 暴露字节流，供流式落盘（不整体载入内存）。
pub type UpstreamBody = Box<dyn AsyncRead + Send + Unpin>;

/// 上游拉取抽象：据上游基址与制品相对路径取回一个字节流。
///
/// 生产实现 [`HttpUpstream`] 走 reqwest 流式；测试可注入计数 mock 以穷举单飞竞态。
#[allow(async_fn_in_trait)]
pub trait Upstream: Send + Sync {
    /// 从上游拉取给定相对路径的制品；成功时返回字节流，失败 / 非 2xx 返回错误。
    async fn fetch(&self, base_url: &str, rel_path: &str) -> Result<UpstreamBody, UpstreamError>;
}

/// 单飞合并器：把同一 key 的并发拉取合并为一次实际执行。
///
/// 内部用 `Mutex<HashMap<Key, Weak<Shared>>>`——临界区只做"找到 / 建立归属"这一步内存判定，
/// 实际的拉取 + 落盘 + 写索引在锁外、由唯一的 leader 跑一次，followers 等其结果。
/// 用 `Weak` 持有 in-flight 项：所有等待者 drop 后自动从表中过期，无需手动清理竞态。
#[derive(Default)]
pub struct SingleFlight<T> {
    /// in-flight 表：key → 共享单元的弱引用。
    in_flight: Mutex<HashMap<String, Weak<Shared<T>>>>,
}

/// 单飞共享单元：承载一次执行的最终结果（成功值或错误信息）。
struct Shared<T> {
    /// 仅执行一次的结果槽：leader 写入，followers 读取。
    cell: OnceCell<Result<T, String>>,
    /// 结果就绪通知：leader 写入 `cell` 后唤醒所有等待的 follower。
    ready: Notify,
}

impl<T: Clone + Send + Sync + 'static> SingleFlight<T> {
    /// 构造空单飞合并器。
    pub fn new() -> Self {
        Self {
            in_flight: Mutex::new(HashMap::new()),
        }
    }

    /// 对给定 key 执行 `f`：并发同 key 调用只会真正执行一次 `f`，其余等待并共享其结果。
    ///
    /// `f` 的执行在锁外进行（临界区只用于归属判定）。`f` 返回错误时不缓存"成功值"，
    /// 等待者各自拿到同一错误副本；in-flight 项随等待者全部结束而过期，下次调用重新执行。
    pub async fn run<F, Fut>(&self, key: &str, f: F) -> Result<T, String>
    where
        F: FnOnce() -> Fut,
        Fut: std::future::Future<Output = Result<T, String>>,
    {
        // ① 临界区：仅做归属判定——复用在飞的 Shared 或新建一个，决定自己是否 leader
        let (shared, is_leader) = {
            let mut map = self.in_flight.lock().expect("单飞表锁未中毒");
            match map.get(key).and_then(Weak::upgrade) {
                // 已有在飞执行：作为 follower 复用其共享单元
                Some(existing) => (existing, false),
                // 无在飞执行：建立新共享单元并登记弱引用，自任 leader
                None => {
                    let shared = Arc::new(Shared {
                        cell: OnceCell::new(),
                        ready: Notify::new(),
                    });
                    map.insert(key.to_string(), Arc::downgrade(&shared));
                    (shared, true)
                }
            }
        };

        if is_leader {
            // ② 锁外执行：leader 真正跑一次 f（拉取 / 落盘 / 写索引都在此，均在锁外）
            let result = f().await;
            // 先摘除 in-flight 登记，再写结果槽并唤醒——确保此后新请求重新执行而非并入本次
            self.remove(key, &shared);
            let _ = shared.cell.set(result);
            // 唤醒所有已在等待的 follower
            shared.ready.notify_waiters();
            // 读回结果返回（set 之后 get 必有值）
            shared.cell.get().expect("leader 已写入结果").clone()
        } else {
            // ② follower：等待 leader 写入结果后共享同一份
            loop {
                // 先登记通知监听，再查槽——避免在"查到空"与"开始等待"之间错过唤醒
                let notified = shared.ready.notified();
                if let Some(result) = shared.cell.get() {
                    return result.clone();
                }
                notified.await;
            }
        }
    }

    /// 摘除 in-flight 登记：仅当表中该 key 仍指向同一 Shared 时才移除，避免误删后继请求。
    fn remove(&self, key: &str, shared: &Arc<Shared<T>>) {
        let mut map = self.in_flight.lock().expect("单飞表锁未中毒");
        if let Some(existing) = map.get(key).and_then(Weak::upgrade) {
            if Arc::ptr_eq(&existing, shared) {
                map.remove(key);
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicUsize, Ordering};

    #[tokio::test]
    async fn 并发同_key_只执行一次() {
        let sf: Arc<SingleFlight<usize>> = Arc::new(SingleFlight::new());
        let calls = Arc::new(AtomicUsize::new(0));

        // 并发发起 N 个同 key 请求，断言底层执行只发生一次
        let mut handles = Vec::new();
        for _ in 0..16 {
            let sf = sf.clone();
            let calls = calls.clone();
            handles.push(tokio::spawn(async move {
                sf.run("k", || async {
                    calls.fetch_add(1, Ordering::SeqCst);
                    // 留出窗口让其余请求并入同一次执行
                    tokio::time::sleep(std::time::Duration::from_millis(20)).await;
                    Ok::<usize, String>(42)
                })
                .await
            }));
        }
        for h in handles {
            assert_eq!(h.await.unwrap().unwrap(), 42);
        }
        assert_eq!(calls.load(Ordering::SeqCst), 1, "同 key 并发应只执行一次");
    }

    #[tokio::test]
    async fn 不同_key_各自执行() {
        let sf: Arc<SingleFlight<usize>> = Arc::new(SingleFlight::new());
        let calls = Arc::new(AtomicUsize::new(0));
        let c1 = calls.clone();
        let c2 = calls.clone();
        let sf1 = sf.clone();
        let sf2 = sf.clone();
        let a = tokio::spawn(async move {
            sf1.run("a", || async move {
                c1.fetch_add(1, Ordering::SeqCst);
                Ok::<usize, String>(1)
            })
            .await
        });
        let b = tokio::spawn(async move {
            sf2.run("b", || async move {
                c2.fetch_add(1, Ordering::SeqCst);
                Ok::<usize, String>(2)
            })
            .await
        });
        a.await.unwrap().unwrap();
        b.await.unwrap().unwrap();
        assert_eq!(calls.load(Ordering::SeqCst), 2, "不同 key 各执行一次");
    }

    #[tokio::test]
    async fn 失败不被后续请求复用() {
        let sf: Arc<SingleFlight<usize>> = Arc::new(SingleFlight::new());
        // 第一次失败
        let r1 = sf
            .run("k", || async {
                Err::<usize, String>("上游挂了".to_string())
            })
            .await;
        assert!(r1.is_err());
        // 第二次（已非并发）应重新执行并成功，而非复用上次的失败
        let r2 = sf.run("k", || async { Ok::<usize, String>(7) }).await;
        assert_eq!(r2.unwrap(), 7);
    }
}
