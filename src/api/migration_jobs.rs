//! 进程内迁移任务注册表（FR-83 / ADR-0019）。
//!
//! 在线拉取迁移异步化后，触发端点立即返回 `job_id`、后台任务跑，进度存本注册表（**不落库**），
//! 由查询端点轮询。注册表**有界**（保留最近 N 个、超出按登记时序淘汰），避免内存无界增长。
//! 服务器重启即丢失——靠迁移幂等重跑恢复（见 ADR-0019，保留 ADR-0006「无须持久化迁移任务表」）。

use std::collections::{HashMap, VecDeque};
use std::sync::{Arc, Mutex, RwLock};

use crate::migrate::OnlinePullProgress;

/// 单个任务的进度共享态：后台任务持续更新，查询端点读取。
pub type JobProgress = Arc<Mutex<OnlinePullProgress>>;

/// 进程内迁移任务注册表：`job_id` → 进度共享态，有界。
pub struct MigrationJobs {
    inner: RwLock<Inner>,
    capacity: usize,
}

struct Inner {
    jobs: HashMap<String, JobProgress>,
    /// 登记时序（淘汰最旧用）。
    order: VecDeque<String>,
}

impl Default for MigrationJobs {
    fn default() -> Self {
        Self::with_capacity(50)
    }
}

impl MigrationJobs {
    /// 以给定容量构造（至少保留 1 个）。
    pub fn with_capacity(capacity: usize) -> Self {
        Self {
            inner: RwLock::new(Inner {
                jobs: HashMap::new(),
                order: VecDeque::new(),
            }),
            capacity: capacity.max(1),
        }
    }

    /// 登记一个任务的进度共享态；超出容量按登记时序淘汰最旧任务。
    pub fn register(&self, job_id: String, progress: JobProgress) {
        let mut g = self.inner.write().unwrap_or_else(|e| e.into_inner());
        g.jobs.insert(job_id.clone(), progress);
        g.order.push_back(job_id);
        while g.order.len() > self.capacity {
            if let Some(old) = g.order.pop_front() {
                g.jobs.remove(&old);
            }
        }
    }

    /// 取某任务的进度共享态（未知返回 None）。
    pub fn get(&self, job_id: &str) -> Option<JobProgress> {
        self.inner
            .read()
            .unwrap_or_else(|e| e.into_inner())
            .jobs
            .get(job_id)
            .cloned()
    }

    /// 列出所有任务的 (job_id, 进度快照)，按登记时序（新在后）。
    pub fn list(&self) -> Vec<(String, OnlinePullProgress)> {
        let g = self.inner.read().unwrap_or_else(|e| e.into_inner());
        g.order
            .iter()
            .filter_map(|id| {
                g.jobs.get(id).map(|p| {
                    let snap = p.lock().unwrap_or_else(|e| e.into_inner()).clone();
                    (id.clone(), snap)
                })
            })
            .collect()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn progress() -> JobProgress {
        Arc::new(Mutex::new(OnlinePullProgress::default()))
    }

    #[test]
    fn 登记后可查与列出() {
        let jobs = MigrationJobs::with_capacity(10);
        let p = progress();
        p.lock().unwrap().migrated = 3;
        jobs.register("job-1".to_string(), p);

        let got = jobs.get("job-1").unwrap();
        assert_eq!(got.lock().unwrap().migrated, 3);
        assert!(jobs.get("不存在").is_none());

        let list = jobs.list();
        assert_eq!(list.len(), 1);
        assert_eq!(list[0].0, "job-1");
        assert_eq!(list[0].1.migrated, 3);
    }

    #[test]
    fn 超出容量按时序淘汰最旧() {
        let jobs = MigrationJobs::with_capacity(2);
        jobs.register("a".to_string(), progress());
        jobs.register("b".to_string(), progress());
        jobs.register("c".to_string(), progress()); // 触发淘汰最旧 a

        assert!(jobs.get("a").is_none(), "最旧任务 a 应被淘汰");
        assert!(jobs.get("b").is_some());
        assert!(jobs.get("c").is_some());
        // 列表按时序保留 b、c
        let ids: Vec<String> = jobs.list().into_iter().map(|(id, _)| id).collect();
        assert_eq!(ids, vec!["b".to_string(), "c".to_string()]);
    }

    #[test]
    fn 进度共享态可被外部更新后查到() {
        let jobs = MigrationJobs::with_capacity(4);
        let p = progress();
        jobs.register("j".to_string(), p.clone());
        // 模拟后台任务更新进度
        {
            let mut g = p.lock().unwrap();
            g.total_assets = 10;
            g.done_assets = 4;
        }
        let snap = &jobs.list()[0].1;
        assert_eq!(snap.total_assets, 10);
        assert_eq!(snap.done_assets, 4);
    }
}
