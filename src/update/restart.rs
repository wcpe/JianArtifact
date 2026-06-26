//! 重启请求与关停通知（FR-85，ADR-0021，§3.8）。
//!
//! 自更新替换成功后，handler 经本句柄置位重启请求并触发优雅停机；`main` 在 `serve` 返回后
//! 据请求拉起新进程（`self`）或直接退出（`exit`）。重启的真正拉起进程 + 端口序列无真机不可验，
//! 本句柄只承载「请求记录 + 关停唤醒」这一最薄状态。

use std::path::PathBuf;
use std::sync::Mutex;

use tokio::sync::Notify;

use super::RestartMode;

/// 一次重启请求：模式 + 待拉起的二进制路径 + 透传给新进程的参数。
#[derive(Debug, Clone)]
pub struct RestartRequest {
    /// 重启模式（自拉起 / 仅退出）。
    pub mode: RestartMode,
    /// 待拉起的二进制路径（替换后落地的 current_exe）。
    pub exe: PathBuf,
    /// 透传给新进程的命令行参数（不含 argv[0]）。
    pub argv: Vec<std::ffi::OsString>,
}

/// 进程级重启句柄：随 `AppState` 共享，承载关停通知与待处理的重启请求。
#[derive(Debug)]
pub struct RestartHandle {
    /// 关停通知：`request_restart` 后唤醒 `shutdown_signal` 的等待路。
    notify: Notify,
    /// 待处理的重启请求（`None` 表示无）；`serve` 返回后由 `main` 取出。
    pending: Mutex<Option<RestartRequest>>,
}

impl Default for RestartHandle {
    fn default() -> Self {
        Self {
            notify: Notify::new(),
            pending: Mutex::new(None),
        }
    }
}

impl RestartHandle {
    /// 置位重启请求并触发关停通知（自更新替换成功后由 handler 调用）。
    ///
    /// 先记录请求、再唤醒等待者，确保 `serve` 返回后 `main` 必能取到请求。
    pub fn request_restart(&self, request: RestartRequest) {
        *self.pending.lock().unwrap_or_else(|e| e.into_inner()) = Some(request);
        self.notify.notify_one();
    }

    /// 等待重启通知（供 `shutdown_signal` 的 select 分支）。
    pub async fn notified(&self) {
        self.notify.notified().await;
    }

    /// 取出待处理的重启请求（`serve` 返回后由 `main` 调用，取走即清空）。
    pub fn take(&self) -> Option<RestartRequest> {
        self.pending
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .take()
    }
}
