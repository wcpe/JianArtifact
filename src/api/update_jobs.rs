//! 进程内在线更新任务注册表（FR-126）。
//!
//! 检查 / 应用异步化后，触发端点立即返回 `job_id`、后台任务跑，进度存本注册表，由查询端点轮询。
//! 注册表**有界**（保留最近 N 个、超出按登记时序淘汰），避免内存无界增长。
//!
//! 与迁移 job（`MigrationJobs`）的差异：① 进度结构为更新专用的 [`UpdateProgress`]；② **不引入
//! `JobControl`**——apply 不支持取消（半截替换会坏二进制，安全考量）。**apply 终态跨重启留存**靠
//! `update` 模块的状态文件（见 `update::state`），重启后由 [`MigrationJobs`] 之外的回填逻辑读回，
//! 本注册表自身仍是进程内、重启即清。

use std::collections::{HashMap, VecDeque};
use std::sync::{Arc, Mutex, RwLock};

use crate::update::UpdateProgress;

/// 单个更新任务的进度共享态：后台任务持续更新，查询端点读取。
pub type UpdateJobProgress = Arc<Mutex<UpdateProgress>>;

/// 进程内更新任务注册表：`job_id` → 进度共享态，有界。
pub struct UpdateJobs {
    inner: RwLock<Inner>,
    capacity: usize,
}

struct Inner {
    jobs: HashMap<String, UpdateJobProgress>,
    /// 登记时序（淘汰最旧用）。
    order: VecDeque<String>,
}

impl Default for UpdateJobs {
    fn default() -> Self {
        Self::with_capacity(20)
    }
}

impl UpdateJobs {
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
    pub fn register(&self, job_id: String, progress: UpdateJobProgress) {
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
    pub fn get(&self, job_id: &str) -> Option<UpdateJobProgress> {
        self.inner
            .read()
            .unwrap_or_else(|e| e.into_inner())
            .jobs
            .get(job_id)
            .cloned()
    }

    /// 列出所有任务的 (job_id, 进度快照)，按登记时序（新在后）。
    pub fn list(&self) -> Vec<(String, UpdateProgress)> {
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
    use crate::update::{UpdateKind, UpdatePhase};

    fn progress(version: &str) -> UpdateJobProgress {
        Arc::new(Mutex::new(UpdateProgress::new(UpdateKind::Apply, version)))
    }

    #[test]
    fn 登记后可查与列出() {
        let jobs = UpdateJobs::with_capacity(10);
        let p = progress("0.4.0");
        p.lock().unwrap().phase = UpdatePhase::Downloading;
        jobs.register("job-1".to_string(), p);

        let got = jobs.get("job-1").unwrap();
        assert_eq!(got.lock().unwrap().phase, UpdatePhase::Downloading);
        assert!(jobs.get("不存在").is_none());

        let list = jobs.list();
        assert_eq!(list.len(), 1);
        assert_eq!(list[0].0, "job-1");
    }

    #[test]
    fn 超出容量按时序淘汰最旧() {
        let jobs = UpdateJobs::with_capacity(2);
        jobs.register("a".to_string(), progress("0.4.0"));
        jobs.register("b".to_string(), progress("0.4.0"));
        jobs.register("c".to_string(), progress("0.4.0")); // 触发淘汰最旧 a

        assert!(jobs.get("a").is_none(), "最旧任务 a 应被淘汰");
        assert!(jobs.get("b").is_some());
        assert!(jobs.get("c").is_some());
        let ids: Vec<String> = jobs.list().into_iter().map(|(id, _)| id).collect();
        assert_eq!(ids, vec!["b".to_string(), "c".to_string()]);
    }

    #[test]
    fn 进度共享态可被外部更新后查到() {
        let jobs = UpdateJobs::with_capacity(4);
        let p = progress("0.4.0");
        jobs.register("j".to_string(), p.clone());
        {
            let mut g = p.lock().unwrap();
            g.phase = UpdatePhase::Done;
            g.new_version = Some("0.5.0".to_string());
        }
        let snap = &jobs.list()[0].1;
        assert_eq!(snap.phase, UpdatePhase::Done);
        assert_eq!(snap.new_version.as_deref(), Some("0.5.0"));
    }
}
