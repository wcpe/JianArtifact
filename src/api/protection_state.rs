//! 运行时防护配置热替换槽（FR-79，扩展 ADR-0008）。
//!
//! ADR-0008 的七层防护各维度阈值 / 开关 / 难度 / 名单 / WAF 规则来自 `[protection.*]` 配置，过去只能
//! 改 TOML 并重启才能调整。本模块提供进程内**运行时热替换**：把「当前生效的防护配置 + 由其派生的态」
//! 收拢为一个可原子替换的快照，PATCH 后下一个请求即按新值判定、无须重启。
//!
//! 设计要点（对齐架构不变量「锁外做 IO，临界区只护内存态、短持有」）：
//! - **用 std 实现**：`RwLock<Arc<ProtectionSnapshot>>`，不引入 arc-swap 等外部依赖。
//! - **读路径廉价**：[`ProtectionState::snapshot`] 读锁内仅克隆一个 `Arc`（引用计数 +1）后立即放锁，
//!   调用方持 `Arc` 在锁外读配置与派生态；热路径不在持锁期间做任何 IO / 匹配。
//! - **替换锁外重建**：[`ProtectionState::replace`] 先在**锁外**按新配置重建派生态（IP 名单匹配器、
//!   WAF 规则集——含正则预编译等开销），再短持写锁把整个快照 `Arc` 一次性原子替换，写临界区只做一次
//!   指针赋值、不做编译 / IO。
//! - **运行态不在此槽**：限流计数、封禁登记、CC 签名器、告警评估器等进程内累计 / 无状态构件由 `AppState`
//!   独立持有，**不随配置替换重建**——改一次配置不应放空已积累的防护状态（限流计数 / 封禁记录 / 告警去抖）。

use std::sync::{Arc, RwLock};

use crate::config::ProtectionConfig;

use super::anomaly_ban::IpMatcher;
use super::waf::WafRuleSet;

/// 当前生效的防护配置及其派生态的不可变快照。
///
/// 持有一份 `ProtectionConfig`（中间件读各维度阈值 / 开关）与两份由其编译来的派生态：IP 名单匹配器
/// （`[protection.ip_list]` 预解析的网段集合）、WAF 规则集（`[protection.waf]` 预编译的有序规则）。
/// 整体经 `Arc` 共享、整体替换，确保配置与派生态始终一致（不会出现配置已换、派生态还是旧的中间态）。
pub struct ProtectionSnapshot {
    /// 当前生效的防护配置（各维度阈值 / 开关 / 难度 / 名单原文 / WAF 规则原文）。
    pub config: ProtectionConfig,
    /// 由 `config.ip_list` 预解析的黑 / 白名单网段匹配器。
    pub ip_matcher: IpMatcher,
    /// 由 `config.waf` 预编译的有序 WAF 规则集（正则预编译、非法规则跳过）。
    pub waf_rules: WafRuleSet,
}

impl ProtectionSnapshot {
    /// 从一份防护配置构建快照：派生态（IP 名单匹配器、WAF 规则集）按配置编译一次。
    fn from_config(config: ProtectionConfig) -> Self {
        let ip_matcher = IpMatcher::from_config(&config.ip_list);
        let waf_rules = WafRuleSet::from_config(&config.waf);
        Self {
            config,
            ip_matcher,
            waf_rules,
        }
    }
}

/// 运行时防护配置热替换槽：随 `AppState` 经 `Arc` 共享，内部 `RwLock` 保护当前快照指针。
///
/// 中间件经 [`Self::snapshot`] 取当前快照（读锁极短）、在锁外判定；管理端 PATCH 经 [`Self::replace`]
/// 原子替换。读多写极少，`RwLock` 读路径无争用、热路径开销可忽略。
pub struct ProtectionState {
    /// 当前生效快照；替换时整体换 `Arc`，读时克隆 `Arc` 出锁。
    current: RwLock<Arc<ProtectionSnapshot>>,
}

impl ProtectionState {
    /// 用初始防护配置构造热替换槽（启动期由 `[protection.*]` 文件配置装载）。
    pub fn new(config: ProtectionConfig) -> Self {
        Self {
            current: RwLock::new(Arc::new(ProtectionSnapshot::from_config(config))),
        }
    }

    /// 取当前生效快照：读锁内克隆 `Arc` 立即放锁，调用方在锁外读配置与派生态。
    ///
    /// 返回的 `Arc<ProtectionSnapshot>` 是替换时刻的一致视图——即便随后被 [`Self::replace`] 替换，
    /// 本次持有的快照仍有效（旧 `Arc` 在最后一个持有者释放后回收），不会读到半新半旧的中间态。
    pub fn snapshot(&self) -> Arc<ProtectionSnapshot> {
        Arc::clone(&self.current.read().unwrap_or_else(|e| e.into_inner()))
    }

    /// 用新配置原子替换当前快照：**锁外**重建派生态，再短持写锁换指针。
    ///
    /// 调用方应在调用前对 `config` 做 [`ProtectionConfig::validate`] 校验；本方法不做校验，只负责
    /// 编译派生态与原子替换。替换后下一个 [`Self::snapshot`] 即返回新快照，对应下一个请求即按新值判定。
    pub fn replace(&self, config: ProtectionConfig) {
        // 锁外重建派生态（IP 名单解析、WAF 正则预编译等开销均在临界区外完成）
        let next = Arc::new(ProtectionSnapshot::from_config(config));
        // 写临界区只做一次指针赋值，短持有、不做任何编译 / IO
        let mut guard = self.current.write().unwrap_or_else(|e| e.into_inner());
        *guard = next;
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::net::IpAddr;
    use std::sync::Barrier;
    use std::thread;

    /// 构造一份启用了某 IP 黑名单的防护配置，便于断言派生态随配置重建。
    fn 含黑名单的配置(deny_ip: &str) -> ProtectionConfig {
        let mut cfg = ProtectionConfig::default();
        cfg.ip_list.deny = vec![deny_ip.to_string()];
        cfg
    }

    #[test]
    fn 初始快照反映初始配置() {
        let state = ProtectionState::new(含黑名单的配置("203.0.113.7"));
        let snap = state.snapshot();
        let ip: IpAddr = "203.0.113.7".parse().unwrap();
        // 派生的 IP 匹配器应已按初始配置编译出该黑名单项
        assert!(snap.ip_matcher.is_denied_for_test(&ip));
    }

    #[test]
    fn replace后snapshot反映新配置与重建的派生态() {
        let state = ProtectionState::new(ProtectionConfig::default());
        // 初始无黑名单
        let ip: IpAddr = "203.0.113.7".parse().unwrap();
        assert!(!state.snapshot().ip_matcher.is_denied_for_test(&ip));

        // 热替换为含该黑名单的配置：派生的 IP 匹配器必须按新配置重建
        state.replace(含黑名单的配置("203.0.113.7"));
        let snap = state.snapshot();
        assert!(
            snap.ip_matcher.is_denied_for_test(&ip),
            "replace 后派生态应按新配置重建，命中新黑名单"
        );
        // 配置字段本身也应已更新
        assert_eq!(snap.config.ip_list.deny, vec!["203.0.113.7".to_string()]);
    }

    #[test]
    fn replace前持有的旧快照不受影响() {
        let state = ProtectionState::new(ProtectionConfig::default());
        let old = state.snapshot(); // 持有旧快照
        let ip: IpAddr = "203.0.113.7".parse().unwrap();
        state.replace(含黑名单的配置("203.0.113.7"));
        // 旧快照仍是替换前的一致视图（不会读到半新半旧）
        assert!(!old.ip_matcher.is_denied_for_test(&ip));
        // 新快照是新配置
        assert!(state.snapshot().ip_matcher.is_denied_for_test(&ip));
    }

    #[test]
    fn 并发replace与snapshot不panic且最终一致() {
        let state = Arc::new(ProtectionState::new(ProtectionConfig::default()));
        let writers = 8usize;
        let readers = 8usize;
        let per = 200usize;
        let barrier = Arc::new(Barrier::new(writers + readers));
        let mut handles = Vec::new();

        // 写线程：反复在「有黑名单」与「无黑名单」两份配置间热替换
        for w in 0..writers {
            let state = Arc::clone(&state);
            let barrier = Arc::clone(&barrier);
            handles.push(thread::spawn(move || {
                barrier.wait();
                for i in 0..per {
                    if (w + i) % 2 == 0 {
                        state.replace(含黑名单的配置("203.0.113.7"));
                    } else {
                        state.replace(ProtectionConfig::default());
                    }
                }
            }));
        }
        // 读线程：反复取快照并读派生态，断言每个快照都是自洽的（配置与派生态一致）
        for _ in 0..readers {
            let state = Arc::clone(&state);
            let barrier = Arc::clone(&barrier);
            handles.push(thread::spawn(move || {
                barrier.wait();
                let ip: IpAddr = "203.0.113.7".parse().unwrap();
                for _ in 0..per {
                    let snap = state.snapshot();
                    // 自洽性：配置里有该 deny 项 <=> 派生匹配器命中该 IP（不会半新半旧）
                    let has_in_cfg = snap.config.ip_list.deny.iter().any(|s| s == "203.0.113.7");
                    assert_eq!(has_in_cfg, snap.ip_matcher.is_denied_for_test(&ip));
                }
            }));
        }
        for h in handles {
            h.join().unwrap();
        }
    }
}
