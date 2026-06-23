//! 登录暴力破解防护（FR-65）。
//!
//! 进程内存按“用户名 + 来源 IP”计数连续失败，达阈值后在锁定窗口内拒绝尝试；
//! 成功或窗口过期即清零。失败计数不落 DB（见 ARCHITECTURE §3）。
//!
//! 注意：本批按连接 IP 计数；XFF 头仅在可信前置代理时才可采信，留待 P2 七层防护增强。

use std::collections::HashMap;
use std::sync::Mutex;
use std::time::{Duration, Instant};

/// 单个 (用户名, IP) 维度的失败计数状态。
#[derive(Debug, Clone)]
struct FailureState {
    /// 连续失败次数。
    count: u32,
    /// 最近一次失败的时刻，用于判断窗口是否过期。
    last_failure: Instant,
}

/// 登录被锁定错误，携带建议的剩余等待秒数。
#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
#[error("登录尝试过于频繁，请在 {retry_after_secs} 秒后重试")]
pub struct LockoutError {
    /// 锁定剩余秒数。
    pub retry_after_secs: u64,
}

/// 登录防护守卫：线程安全地维护各 (用户名, IP) 的失败计数。
pub struct LoginGuard {
    /// 触发锁定的连续失败阈值。
    max_failures: u32,
    /// 锁定时长。
    lockout: Duration,
    /// 失败计数表，键为 (用户名, IP)。
    state: Mutex<HashMap<(String, String), FailureState>>,
}

impl std::fmt::Debug for LoginGuard {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("LoginGuard")
            .field("max_failures", &self.max_failures)
            .field("lockout_secs", &self.lockout.as_secs())
            .finish_non_exhaustive()
    }
}

impl LoginGuard {
    /// 构造守卫：`max_failures` 为触发锁定的连续失败次数，`lockout_secs` 为锁定时长。
    pub fn new(max_failures: u32, lockout_secs: u64) -> Self {
        Self {
            max_failures,
            lockout: Duration::from_secs(lockout_secs),
            state: Mutex::new(HashMap::new()),
        }
    }

    /// 登录尝试前检查是否处于锁定中；锁定则返回剩余等待秒数。
    ///
    /// 已过锁定窗口的过期记录在此顺带清理，使锁定自动恢复。
    pub fn check(&self, username: &str, ip: &str) -> Result<(), LockoutError> {
        let key = (username.to_string(), ip.to_string());
        let mut guard = self.lock_state();
        match guard.get(&key) {
            Some(s) if self.is_locked(s) => {
                let elapsed = s.last_failure.elapsed();
                let remaining = self.lockout.saturating_sub(elapsed).as_secs();
                Err(LockoutError {
                    // 至少提示 1 秒，避免向客户端回报 0 造成误解
                    retry_after_secs: remaining.max(1),
                })
            }
            // 存在但窗口已过期：清理后视为未锁定
            Some(s) if s.last_failure.elapsed() >= self.lockout => {
                guard.remove(&key);
                Ok(())
            }
            _ => Ok(()),
        }
    }

    /// 记录一次登录失败：累加计数并刷新时间戳。
    pub fn record_failure(&self, username: &str, ip: &str) {
        let key = (username.to_string(), ip.to_string());
        let mut guard = self.lock_state();
        let entry = guard.entry(key).or_insert(FailureState {
            count: 0,
            last_failure: Instant::now(),
        });
        // 若上次失败已越过锁定窗口，则重新计数（连续性被打断）
        if entry.last_failure.elapsed() >= self.lockout {
            entry.count = 0;
        }
        entry.count = entry.count.saturating_add(1);
        entry.last_failure = Instant::now();
    }

    /// 记录一次登录成功：清空该 (用户名, IP) 的失败计数。
    pub fn record_success(&self, username: &str, ip: &str) {
        let key = (username.to_string(), ip.to_string());
        let mut guard = self.lock_state();
        guard.remove(&key);
    }

    /// 判断给定失败状态是否已达锁定条件且仍在窗口内。
    fn is_locked(&self, s: &FailureState) -> bool {
        s.count >= self.max_failures && s.last_failure.elapsed() < self.lockout
    }

    /// 取内部状态锁；锁中毒时取回内部数据继续（计数表损坏不致命）。
    fn lock_state(&self) -> std::sync::MutexGuard<'_, HashMap<(String, String), FailureState>> {
        self.state.lock().unwrap_or_else(|e| e.into_inner())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn 未达阈值不锁定() {
        let guard = LoginGuard::new(3, 900);
        guard.record_failure("alice", "1.1.1.1");
        guard.record_failure("alice", "1.1.1.1");
        // 仅 2 次，未达阈值 3，应放行
        assert!(guard.check("alice", "1.1.1.1").is_ok());
    }

    #[test]
    fn 达阈值后锁定() {
        let guard = LoginGuard::new(3, 900);
        for _ in 0..3 {
            guard.record_failure("alice", "1.1.1.1");
        }
        let err = guard.check("alice", "1.1.1.1").unwrap_err();
        assert!(err.retry_after_secs > 0);
    }

    #[test]
    fn 成功清零失败计数() {
        let guard = LoginGuard::new(3, 900);
        for _ in 0..3 {
            guard.record_failure("alice", "1.1.1.1");
        }
        assert!(guard.check("alice", "1.1.1.1").is_err());
        // 成功（如运维直接重置）后应解除锁定
        guard.record_success("alice", "1.1.1.1");
        assert!(guard.check("alice", "1.1.1.1").is_ok());
    }

    #[test]
    fn 锁定到期自动恢复() {
        // 锁定窗口 1 秒，便于快速验证自动恢复
        let guard = LoginGuard::new(2, 1);
        guard.record_failure("alice", "1.1.1.1");
        guard.record_failure("alice", "1.1.1.1");
        assert!(guard.check("alice", "1.1.1.1").is_err());
        std::thread::sleep(Duration::from_millis(1100));
        // 越过窗口应自动恢复放行
        assert!(guard.check("alice", "1.1.1.1").is_ok());
    }

    #[test]
    fn 不同_ip_互不影响不误锁正常用户() {
        let guard = LoginGuard::new(2, 900);
        // 攻击者从某 IP 连续失败
        guard.record_failure("alice", "9.9.9.9");
        guard.record_failure("alice", "9.9.9.9");
        assert!(guard.check("alice", "9.9.9.9").is_err());
        // 同名用户从另一正常 IP 不应被误锁
        assert!(guard.check("alice", "1.1.1.1").is_ok());
    }

    #[test]
    fn 不同用户名互不影响() {
        let guard = LoginGuard::new(2, 900);
        guard.record_failure("attacker", "1.1.1.1");
        guard.record_failure("attacker", "1.1.1.1");
        assert!(guard.check("attacker", "1.1.1.1").is_err());
        // 同 IP 的另一用户不受影响
        assert!(guard.check("alice", "1.1.1.1").is_ok());
    }
}
