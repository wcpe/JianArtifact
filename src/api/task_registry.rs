//! 统一进程内异步任务注册表（FR-131，修订 ADR-0019）。
//!
//! 把迁移 / 在线更新 / 漏洞库刷新等多类长耗时任务收口到**同一注册表**：每个任务登记一条轻量
//! [`TaskRecord`]（`id` + `kind` + 统一状态 + 起 / 止 / 更新时间 + 可选 error），供「一处看全部
//! 活跃 + 近期任务」与「离页 / 重连找回」。**进度明细仍归各 kind 专表**（`MigrationJobs` 的
//! `OnlinePullProgress`、`UpdateJobs` 的 `UpdateProgress`），统一表只存轻量记录、与专表用同一
//! `job_id` 关联，不复制进度成第二份真源。
//!
//! 注册表**有界**（保留近期 N 个、超出按登记时序淘汰）且**保留已完成 / 失败任务**供找回。
//! **进程内、不落库、重启即清**——与 ADR-0019「不落库」一致，不引入任务持久化。
//!
//! **迁移单飞**：迁移搬运任务同时只允许一个在途，[`TaskRegistry::try_begin_migration`] 在同一把
//! 写锁内原子完成「检查无在途迁移 → 登记」，杜绝竞态双开。

use std::collections::{HashMap, VecDeque};
use std::sync::RwLock;
use std::time::{SystemTime, UNIX_EPOCH};

use serde::Serialize;
use uuid::Uuid;

/// 任务类别：统一收口的三类长耗时任务。
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum TaskKind {
    /// Nexus 迁移（在线拉取 / 离线预览 / 离线搬运）。
    Migration,
    /// 在线更新（检查 / 应用 / 回滚）。
    Update,
    /// 漏洞库镜像刷新。
    Vuln,
}

/// 任务统一状态：跨 kind 归一的生命周期态。
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum TaskState {
    /// 运行中。
    Running,
    /// 已暂停（仅迁移可暂停，FR-91）。
    Paused,
    /// 成功完成。
    Succeeded,
    /// 失败。
    Failed,
    /// 已取消（不算失败）。
    Cancelled,
}

impl TaskState {
    /// 是否为终态（succeeded / failed / cancelled）。
    fn is_terminal(self) -> bool {
        matches!(
            self,
            TaskState::Succeeded | TaskState::Failed | TaskState::Cancelled
        )
    }
}

/// 单个任务的轻量记录：统一表中跨 kind 一致的元数据。
#[derive(Debug, Clone, Serialize)]
pub struct TaskRecord {
    /// 任务 id（与对应 kind 专表的 `job_id` 一致）。
    pub id: String,
    /// 任务类别。
    pub kind: TaskKind,
    /// 统一状态。
    pub state: TaskState,
    /// 人类可读标签（如「在线拉取迁移」「应用更新」「漏洞库刷新」）。
    #[serde(skip_serializing_if = "Option::is_none")]
    pub label: Option<String>,
    /// 登记时刻（Unix 秒）。
    pub started_at: u64,
    /// 最近一次状态更新时刻（Unix 秒）。
    pub updated_at: u64,
    /// 终态时刻（Unix 秒，未结束为 None）。
    #[serde(skip_serializing_if = "Option::is_none")]
    pub finished_at: Option<u64>,
    /// 失败 / 异常原因（`state == failed` 时）。
    #[serde(skip_serializing_if = "Option::is_none")]
    pub error: Option<String>,
}

/// 取当前 Unix 秒（系统时钟回拨等异常时回 0，不 panic）。
fn now_unix_secs() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0)
}

struct Inner {
    tasks: HashMap<String, TaskRecord>,
    /// 登记时序（淘汰最旧用）。
    order: VecDeque<String>,
}

/// 统一进程内任务注册表：`id` → [`TaskRecord`]，有界。
pub struct TaskRegistry {
    inner: RwLock<Inner>,
    capacity: usize,
}

impl Default for TaskRegistry {
    fn default() -> Self {
        Self::with_capacity(100)
    }
}

impl TaskRegistry {
    /// 以给定容量构造（至少保留 1 个）。
    pub fn with_capacity(capacity: usize) -> Self {
        Self {
            inner: RwLock::new(Inner {
                tasks: HashMap::new(),
                order: VecDeque::new(),
            }),
            capacity: capacity.max(1),
        }
    }

    /// 登记一个新任务（自生成 UUID），返回其 id。
    pub fn register(&self, kind: TaskKind, label: Option<String>) -> String {
        let id = Uuid::new_v4().to_string();
        self.register_with_id(id.clone(), kind, label);
        id
    }

    /// 以调用方给定 id 登记一个新任务（供迁移 / 更新复用其既有 `job_id`，统一表与专表同 id）。
    ///
    /// 超出容量按登记时序淘汰最旧任务（含已完成的历史任务）。
    pub fn register_with_id(&self, id: String, kind: TaskKind, label: Option<String>) {
        let now = now_unix_secs();
        let mut g = self.inner.write().unwrap_or_else(|e| e.into_inner());
        let record = TaskRecord {
            id: id.clone(),
            kind,
            state: TaskState::Running,
            label,
            started_at: now,
            updated_at: now,
            finished_at: None,
            error: None,
        };
        // 同 id 重复登记视为覆盖（不重复入 order，避免淘汰指针错乱）
        if g.tasks.insert(id.clone(), record).is_none() {
            g.order.push_back(id);
        }
        while g.order.len() > self.capacity {
            if let Some(old) = g.order.pop_front() {
                g.tasks.remove(&old);
            }
        }
    }

    /// 是否已有迁移任务在途（`Migration` 且非终态）。供触发端点在昂贵同步阶段前**早拒**第二个迁移
    /// （快路径优化 + 可测）；真正杜绝竞态仍以 [`Self::try_begin_migration`] 的原子判定为准。
    pub fn migration_in_flight(&self) -> bool {
        let g = self.inner.read().unwrap_or_else(|e| e.into_inner());
        g.tasks
            .values()
            .any(|t| t.kind == TaskKind::Migration && !t.state.is_terminal())
    }

    /// 原子「检查无在途迁移 → 登记」：单飞门。
    ///
    /// 若已有 `Migration` 任务处于非终态（`Running`/`Paused`）则返回 `false`（拒第二个）；
    /// 否则在同一把写锁内登记并返回 `true`。判定与登记同临界区完成，杜绝并发竞态双开。
    pub fn try_begin_migration(&self, id: String, label: Option<String>) -> bool {
        let now = now_unix_secs();
        let mut g = self.inner.write().unwrap_or_else(|e| e.into_inner());
        let in_flight = g
            .tasks
            .values()
            .any(|t| t.kind == TaskKind::Migration && !t.state.is_terminal());
        if in_flight {
            return false;
        }
        let record = TaskRecord {
            id: id.clone(),
            kind: TaskKind::Migration,
            state: TaskState::Running,
            label,
            started_at: now,
            updated_at: now,
            finished_at: None,
            error: None,
        };
        if g.tasks.insert(id.clone(), record).is_none() {
            g.order.push_back(id);
        }
        while g.order.len() > self.capacity {
            if let Some(old) = g.order.pop_front() {
                g.tasks.remove(&old);
            }
        }
        true
    }

    /// 更新某任务状态（未知 id 为空操作）。终态自动记 `finished_at`；`error` 仅在给定时覆盖。
    pub fn set_state(&self, id: &str, state: TaskState, error: Option<String>) {
        let now = now_unix_secs();
        let mut g = self.inner.write().unwrap_or_else(|e| e.into_inner());
        if let Some(t) = g.tasks.get_mut(id) {
            t.state = state;
            t.updated_at = now;
            if state.is_terminal() {
                t.finished_at = Some(now);
            }
            if let Some(e) = error {
                t.error = Some(e);
            }
        }
    }

    /// 取某任务记录快照（未知返回 None）。
    pub fn get(&self, id: &str) -> Option<TaskRecord> {
        self.inner
            .read()
            .unwrap_or_else(|e| e.into_inner())
            .tasks
            .get(id)
            .cloned()
    }

    /// 列出所有任务记录，按登记时序（新在后）。
    pub fn list(&self) -> Vec<TaskRecord> {
        let g = self.inner.read().unwrap_or_else(|e| e.into_inner());
        g.order
            .iter()
            .filter_map(|id| g.tasks.get(id).cloned())
            .collect()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn 登记后可查与列出() {
        let reg = TaskRegistry::with_capacity(10);
        let id = reg.register(TaskKind::Update, Some("应用更新".to_string()));
        let got = reg.get(&id).unwrap();
        assert_eq!(got.kind, TaskKind::Update);
        assert_eq!(got.state, TaskState::Running);
        assert_eq!(got.label.as_deref(), Some("应用更新"));
        assert!(got.finished_at.is_none());
        assert!(reg.get("不存在").is_none());

        let list = reg.list();
        assert_eq!(list.len(), 1);
        assert_eq!(list[0].id, id);
    }

    #[test]
    fn 三类任务进同一表并列出() {
        let reg = TaskRegistry::with_capacity(10);
        reg.register(TaskKind::Migration, None);
        reg.register(TaskKind::Update, None);
        reg.register(TaskKind::Vuln, None);
        let kinds: Vec<TaskKind> = reg.list().into_iter().map(|t| t.kind).collect();
        assert_eq!(
            kinds,
            vec![TaskKind::Migration, TaskKind::Update, TaskKind::Vuln]
        );
    }

    #[test]
    fn 超出容量按时序淘汰最旧() {
        let reg = TaskRegistry::with_capacity(2);
        reg.register_with_id("a".to_string(), TaskKind::Vuln, None);
        reg.register_with_id("b".to_string(), TaskKind::Vuln, None);
        reg.register_with_id("c".to_string(), TaskKind::Vuln, None); // 淘汰最旧 a

        assert!(reg.get("a").is_none(), "最旧任务 a 应被淘汰");
        assert!(reg.get("b").is_some());
        assert!(reg.get("c").is_some());
        let ids: Vec<String> = reg.list().into_iter().map(|t| t.id).collect();
        assert_eq!(ids, vec!["b".to_string(), "c".to_string()]);
    }

    #[test]
    fn 有界历史保留已完成任务供找回() {
        let reg = TaskRegistry::with_capacity(10);
        let id = reg.register(TaskKind::Migration, None);
        reg.set_state(&id, TaskState::Succeeded, None);
        // 已完成任务仍在列表中（保留近期历史，不只活跃任务）
        let got = reg.get(&id).unwrap();
        assert_eq!(got.state, TaskState::Succeeded);
        assert!(got.finished_at.is_some());
        assert_eq!(reg.list().len(), 1);
    }

    #[test]
    fn 迁移单飞_第二个被拒() {
        let reg = TaskRegistry::with_capacity(10);
        assert!(
            reg.try_begin_migration("m1".to_string(), None),
            "首个迁移应放行"
        );
        assert!(
            !reg.try_begin_migration("m2".to_string(), None),
            "已有在途迁移时第二个应被拒"
        );
        // 第二个被拒后不应入表
        assert!(reg.get("m2").is_none());
    }

    #[test]
    fn 迁移结束后可再次开启() {
        let reg = TaskRegistry::with_capacity(10);
        assert!(reg.try_begin_migration("m1".to_string(), None));
        reg.set_state("m1", TaskState::Succeeded, None);
        // 在途迁移已终态，单飞门放行下一个
        assert!(
            reg.try_begin_migration("m2".to_string(), None),
            "前一迁移结束后应可再次开启"
        );
    }

    #[test]
    fn 暂停态仍占用单飞门() {
        let reg = TaskRegistry::with_capacity(10);
        assert!(reg.try_begin_migration("m1".to_string(), None));
        reg.set_state("m1", TaskState::Paused, None);
        // 暂停不是终态，仍占用单飞门
        assert!(
            !reg.try_begin_migration("m2".to_string(), None),
            "暂停态迁移仍在途，第二个应被拒"
        );
    }

    #[test]
    fn 不同_kind_不互斥单飞() {
        let reg = TaskRegistry::with_capacity(10);
        // 有在途更新 / vuln 不应阻塞迁移单飞门
        reg.register(TaskKind::Update, None);
        reg.register(TaskKind::Vuln, None);
        assert!(
            reg.try_begin_migration("m1".to_string(), None),
            "非迁移在途任务不应占用迁移单飞门"
        );
    }

    #[test]
    fn 置失败态记录错误与终态时间() {
        let reg = TaskRegistry::with_capacity(10);
        let id = reg.register(TaskKind::Update, None);
        reg.set_state(&id, TaskState::Failed, Some("网络错误".to_string()));
        let got = reg.get(&id).unwrap();
        assert_eq!(got.state, TaskState::Failed);
        assert_eq!(got.error.as_deref(), Some("网络错误"));
        assert!(got.finished_at.is_some());
    }

    #[test]
    fn 置状态_未知id为空操作() {
        let reg = TaskRegistry::with_capacity(10);
        reg.set_state("不存在", TaskState::Succeeded, None); // 不 panic
        assert!(reg.list().is_empty());
    }
}
