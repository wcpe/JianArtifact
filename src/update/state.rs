//! 在线更新任务进度态与跨重启留存（FR-126）。
//!
//! FR-85/87 的检查 / 应用是同步阻塞端点；FR-126 把执行改为进程内异步 job：触发即返回 `job_id`，
//! 前端轮询本进度态。与迁移 job（ADR-0019「不落库、重启即丢、靠幂等重跑」）的**关键差异**：
//! apply 替换二进制后自动重启，进程内注册表随之消失，且更新结果无法靠重跑恢复——故把**检查结果**
//! 与 **apply 终态**留存到 `{data_dir}/update-state.json` 状态文件，重启后读回供用户续看。这是对
//! ADR-0019 的**有意例外**（守 `update → config` 分层：不依赖 `meta`、不碰 SQLite，仅写数据目录文件）。
//!
//! 安全：状态文件**绝不写入 token / 凭据**（只存版本号、阶段、发布说明等非敏感信息）。

use std::path::{Path, PathBuf};

use serde::{Deserialize, Serialize};

use super::{UpdateCheck, UpdateError};

/// 状态文件名（位于数据目录下）。
const STATE_FILE_NAME: &str = "update-state.json";

/// 更新任务类别（检查 / 应用 / 回滚）。
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum UpdateKind {
    /// 联网检查最新版本。
    Check,
    /// 下载并应用更新（替换二进制 + 重启）。
    Apply,
    /// 回滚到上一版（FR-104）。
    Rollback,
}

/// 更新任务阶段（FR-126）：按**阶段**反馈进度，不做字节级假百分比（根治旧「卡 95%」）。
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum UpdatePhase {
    /// 检查中：正在联网查 Release、比对版本。
    #[default]
    Checking,
    /// 下载中：正在流式下载资产并边算 sha256。
    Downloading,
    /// 校验中：正在比对 sha256。
    Verifying,
    /// 替换中：校验通过、正在原子替换二进制。
    Replacing,
    /// 即将重启：替换成功、已置位重启请求，等待优雅停机后拉起新进程。
    Restarting,
    /// 已完成（检查 job 成功亦用此终态）。
    Done,
    /// 已失败（联网 / 校验 / 替换等致命错误）。
    Failed,
}

impl UpdatePhase {
    /// 是否为终态（轮询见终态即停）。`Restarting` 视为终态——后续连接随重启而断，
    /// 前端据此进入「正在重启」提示并保留重连。
    pub fn is_terminal(self) -> bool {
        matches!(
            self,
            UpdatePhase::Restarting | UpdatePhase::Done | UpdatePhase::Failed
        )
    }
}

/// 更新任务进度快照（FR-126）：任务执行期间持续更新，`GET /update/jobs/{id}` 直接序列化之。
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct UpdateProgress {
    /// 任务类别。
    pub kind: UpdateKind,
    /// 当前阶段。
    pub phase: UpdatePhase,
    /// 当前运行版本。
    pub current_version: String,
    /// 检查到的最新版本（检查 / 应用 job 联网后填）。
    #[serde(skip_serializing_if = "Option::is_none")]
    pub latest_version: Option<String>,
    /// 检查结果（检查 job 完成时填，供前端展示「是否有更新 / 资产名 / 发布说明」）。
    #[serde(skip_serializing_if = "Option::is_none")]
    pub check: Option<UpdateCheck>,
    /// 替换后的新版本号（apply / rollback 成功时填）。
    #[serde(skip_serializing_if = "Option::is_none")]
    pub new_version: Option<String>,
    /// 失败原因（`phase == Failed` 时）。
    #[serde(skip_serializing_if = "Option::is_none")]
    pub error: Option<String>,
    /// 是否为「重启后从状态文件回填」的历史终态（重启续看用，区别于本进程内活动任务）。
    #[serde(default)]
    pub restarted: bool,
}

impl UpdateProgress {
    /// 以给定类别与当前版本构造初始进度（阶段为 `Checking`）。
    pub fn new(kind: UpdateKind, current_version: &str) -> Self {
        Self {
            kind,
            phase: UpdatePhase::Checking,
            current_version: current_version.to_string(),
            latest_version: None,
            check: None,
            new_version: None,
            error: None,
            restarted: false,
        }
    }

    /// 标记失败并记录原因。
    pub fn fail(&mut self, error: impl Into<String>) {
        self.phase = UpdatePhase::Failed;
        self.error = Some(error.into());
    }
}

/// 跨重启留存的更新状态（FR-126）：写 `{data_dir}/update-state.json`。
///
/// 只存「上次检查结果」与「上次 apply / rollback 终态」两份单条目，不存历史多任务（非任务表）。
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct UpdateState {
    /// 上次检查结果（含检查时刻 Unix 秒），供 `GET /update/check` 不联网读回。
    #[serde(skip_serializing_if = "Option::is_none")]
    pub last_check: Option<CachedCheck>,
    /// 上次 apply / rollback 终态，供重启后回填 `UpdateJobs` 续看。
    #[serde(skip_serializing_if = "Option::is_none")]
    pub last_apply: Option<UpdateProgress>,
}

/// 留存的检查结果 + 检查时刻。
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct CachedCheck {
    /// 检查结果。
    pub result: UpdateCheck,
    /// 检查时刻（Unix 秒）。
    pub checked_at: u64,
}

/// 状态文件路径（纯函数，可测）。
pub fn state_path(data_dir: &Path) -> PathBuf {
    data_dir.join(STATE_FILE_NAME)
}

/// 取当前 Unix 秒（系统时钟回拨等异常时回 0，不 panic）。
pub fn now_unix_secs() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0)
}

/// 读取留存的更新状态（无文件 / 解析失败均返回 `None`，不报错——留存只为续看，缺失即视作无）。
pub async fn load_state(data_dir: &Path) -> Option<UpdateState> {
    let path = state_path(data_dir);
    let bytes = tokio::fs::read(&path).await.ok()?;
    match serde_json::from_slice::<UpdateState>(&bytes) {
        Ok(state) => Some(state),
        Err(e) => {
            tracing::warn!(错误 = %e, "更新状态文件解析失败，视作无留存");
            None
        }
    }
}

/// 原子写入更新状态（先写临时文件再 rename，避免半截文件被读到）。
///
/// **绝不写入凭据**：`UpdateState` 仅含版本号 / 阶段 / 发布说明等非敏感字段。
pub async fn persist_state(data_dir: &Path, state: &UpdateState) -> Result<(), UpdateError> {
    // 数据目录缺失时按需创建（首启 / 测试用 tempdir 下数据目录可能尚未落盘）
    tokio::fs::create_dir_all(data_dir)
        .await
        .map_err(|e| UpdateError::Io(e.to_string()))?;
    let path = state_path(data_dir);
    let tmp = path.with_extension("json.tmp");
    let json = serde_json::to_vec_pretty(state)
        .map_err(|e| UpdateError::Io(format!("序列化更新状态失败: {e}")))?;
    tokio::fs::write(&tmp, &json)
        .await
        .map_err(|e| UpdateError::Io(e.to_string()))?;
    tokio::fs::rename(&tmp, &path)
        .await
        .map_err(|e| UpdateError::Io(e.to_string()))?;
    Ok(())
}

/// 读—改—写：以闭包更新留存状态后原子落盘（缺文件视作空状态起改）。
pub async fn update_state<F>(data_dir: &Path, f: F) -> Result<(), UpdateError>
where
    F: FnOnce(&mut UpdateState),
{
    let mut state = load_state(data_dir).await.unwrap_or_default();
    f(&mut state);
    persist_state(data_dir, &state).await
}

#[cfg(test)]
mod tests {
    use super::*;

    fn 样例检查() -> UpdateCheck {
        UpdateCheck {
            current_version: "0.4.0".to_string(),
            latest_version: "0.5.0".to_string(),
            update_available: true,
            asset_name: "jianartifact-0.5.0-x".to_string(),
            notes: "发布说明".to_string(),
        }
    }

    #[test]
    fn 阶段终态判定() {
        assert!(!UpdatePhase::Checking.is_terminal());
        assert!(!UpdatePhase::Downloading.is_terminal());
        assert!(UpdatePhase::Restarting.is_terminal());
        assert!(UpdatePhase::Done.is_terminal());
        assert!(UpdatePhase::Failed.is_terminal());
    }

    #[tokio::test]
    async fn 状态往返_检查结果与apply终态() {
        let dir = tempfile::tempdir().unwrap();
        // 无文件时返回 None
        assert!(load_state(dir.path()).await.is_none());

        // 写入检查结果 + apply 终态
        let mut apply = UpdateProgress::new(UpdateKind::Apply, "0.4.0");
        apply.phase = UpdatePhase::Restarting;
        apply.new_version = Some("0.5.0".to_string());
        let state = UpdateState {
            last_check: Some(CachedCheck {
                result: 样例检查(),
                checked_at: 1234,
            }),
            last_apply: Some(apply),
        };
        persist_state(dir.path(), &state).await.unwrap();

        // 读回一致
        let loaded = load_state(dir.path()).await.expect("应读回留存");
        let cached = loaded.last_check.expect("应有检查留存");
        assert_eq!(cached.checked_at, 1234);
        assert_eq!(cached.result.latest_version, "0.5.0");
        let last = loaded.last_apply.expect("应有 apply 终态");
        assert_eq!(last.phase, UpdatePhase::Restarting);
        assert_eq!(last.new_version.as_deref(), Some("0.5.0"));
    }

    #[tokio::test]
    async fn 状态文件不含凭据字样() {
        // 守安全红线：留存结构无任何 token / 凭据字段，序列化结果不含敏感键名
        let dir = tempfile::tempdir().unwrap();
        let state = UpdateState {
            last_check: Some(CachedCheck {
                result: 样例检查(),
                checked_at: 1,
            }),
            last_apply: None,
        };
        persist_state(dir.path(), &state).await.unwrap();
        let text = tokio::fs::read_to_string(state_path(dir.path()))
            .await
            .unwrap();
        let lower = text.to_lowercase();
        assert!(!lower.contains("token"), "状态文件不得含 token");
        assert!(!lower.contains("password"), "状态文件不得含 password");
        assert!(!lower.contains("secret"), "状态文件不得含 secret");
    }

    #[tokio::test]
    async fn 读改写_增量更新检查不动apply() {
        let dir = tempfile::tempdir().unwrap();
        // 先写一份 apply 终态
        let mut apply = UpdateProgress::new(UpdateKind::Apply, "0.4.0");
        apply.phase = UpdatePhase::Done;
        update_state(dir.path(), |s| s.last_apply = Some(apply.clone()))
            .await
            .unwrap();
        // 再只更新检查结果，apply 终态应保留
        update_state(dir.path(), |s| {
            s.last_check = Some(CachedCheck {
                result: 样例检查(),
                checked_at: 9,
            })
        })
        .await
        .unwrap();

        let loaded = load_state(dir.path()).await.unwrap();
        assert!(loaded.last_apply.is_some(), "增量更新检查不应丢 apply 终态");
        assert_eq!(loaded.last_check.unwrap().checked_at, 9);
    }
}
