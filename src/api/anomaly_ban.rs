//! 访问异常检测与自动封禁 + IP 黑/白名单（FR-53，ADR-0008 的「访问异常检测与自动封禁、IP 黑/白名单」部分）。
//!
//! 置于请求热路径**前端**（黑名单 / 封禁检查尽量靠前，先于限流与重处理）：
//! - **IP 黑/白名单**：配置声明 IP / CIDR 黑名单（直接拒 403）与白名单（豁免一切应用层防护）；
//!   **白名单优先级最高**——命中白名单的来源不参与限流 / 封禁 / 异常统计，照常进入业务。
//! - **访问异常检测 + 自动封禁**：在固定时间窗内按来源 IP 统计异常信号（4xx 客户端错误，含
//!   401/403 鉴权失败；及被限流拒绝的 429），单 IP 一窗内异常信号数达阈值即自动封禁一个时间窗，
//!   封禁期内该 IP 一律拒绝（403）；窗口到期自动解封。阈值 / 窗口 / 封禁时长可配置。
//!
//! 设计要点（对齐 testing-and-quality §2.7）：
//! - **只做 L7**：仅应用层封禁与名单；L3/L4 体积型攻击交前置反向代理 / CDN / WAF（架构不变量）。
//! - **热路径低开销**：每请求前置只做一次名单匹配（命中早返回）与一次封禁查表（取一次 `Mutex`、
//!   整型比较），临界区内无 IO、无格式化；异常信号统计仅对非 2xx/3xx 响应触发，正常响应零计数。
//! - **防误杀**：默认关闭且阈值保守宽放；白名单豁免一切防护，正常包管理器批量拉取（偶发 404 /
//!   鉴权重试）不应触顶；名单未配置时不影响现有行为。
//! - **防绕过**：来源 IP 取连接级 `ConnectInfo`（与限流 / 登录防护一致），**不采信 XFF 头**，
//!   伪造来源不绕过黑名单 / 封禁，也不能借伪造头逃避异常统计。
//! - **封禁状态进程内内存**（时间窗，重启即清）：与登录失败计数同源（ARCHITECTURE §3），不落 DB。
//! - **并发一致**：封禁与信号计数均经 `Mutex` 保护，并发下计数不丢不重；过期键顺带清理防无界增长。
//! - **配置即时生效**：开关 / 阈值 / 窗口 / 名单从 `AppState.config` 读取，配置热替换后下个请求即按新值判定。

use std::collections::HashMap;
use std::net::{IpAddr, SocketAddr};
use std::sync::Mutex;
use std::time::{Duration, Instant};

use axum::{
    extract::{ConnectInfo, Request, State},
    http::StatusCode,
    middleware::Next,
    response::{IntoResponse, Response},
    Json,
};
use serde_json::json;

use crate::config::IpListConfig;

use super::AppState;

/// 单个 CIDR 网段（或单 IP，视作前缀长度为满位的网段）。
///
/// 以「网络地址 + 前缀位数」表示，匹配时按前缀位比较，IPv4 / IPv6 各自处理。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
struct IpCidr {
    /// 网络基址。
    network: IpAddr,
    /// 前缀位数（IPv4 ≤ 32，IPv6 ≤ 128）。
    prefix_len: u8,
}

impl IpCidr {
    /// 解析单条名单项：支持 `addr/prefix` 网段与裸 `addr`（视作满位前缀的单 IP）。
    ///
    /// 解析失败（非法地址 / 前缀越界）返回 `None`，由调用方记 WARN 并跳过该条，不致命。
    fn parse(s: &str) -> Option<Self> {
        let s = s.trim();
        match s.split_once('/') {
            Some((addr, prefix)) => {
                let network: IpAddr = addr.trim().parse().ok()?;
                let prefix_len: u8 = prefix.trim().parse().ok()?;
                let max = if network.is_ipv4() { 32 } else { 128 };
                if prefix_len > max {
                    return None;
                }
                Some(Self {
                    network,
                    prefix_len,
                })
            }
            None => {
                let network: IpAddr = s.parse().ok()?;
                let prefix_len = if network.is_ipv4() { 32 } else { 128 };
                Some(Self {
                    network,
                    prefix_len,
                })
            }
        }
    }

    /// 判断给定 IP 是否落在本网段内。
    ///
    /// IPv4 与 IPv6 不互相匹配；按前缀位逐字节 + 掩码比较高位。
    fn contains(&self, ip: &IpAddr) -> bool {
        match (self.network, ip) {
            (IpAddr::V4(net), IpAddr::V4(addr)) => {
                prefix_match(&net.octets(), &addr.octets(), self.prefix_len)
            }
            (IpAddr::V6(net), IpAddr::V6(addr)) => {
                prefix_match(&net.octets(), &addr.octets(), self.prefix_len)
            }
            // IPv4 与 IPv6 不跨族匹配
            _ => false,
        }
    }
}

/// 按前缀位比较两段等长字节是否高位一致（用于 CIDR 匹配，纯函数便于穷举测试）。
fn prefix_match(network: &[u8], addr: &[u8], prefix_len: u8) -> bool {
    let full_bytes = (prefix_len / 8) as usize;
    let rem_bits = prefix_len % 8;
    // 比较完整字节
    if network[..full_bytes] != addr[..full_bytes] {
        return false;
    }
    // 比较剩余不足一字节的高位
    if rem_bits > 0 {
        let mask = 0xFFu8 << (8 - rem_bits);
        if (network[full_bytes] & mask) != (addr[full_bytes] & mask) {
            return false;
        }
    }
    true
}

/// IP 名单匹配器：把配置中的字符串名单项预解析为网段集合，匹配时不再反复解析（热路径低开销）。
///
/// 非法名单项在构造时被跳过（记 WARN），不阻断启动；空名单匹配恒为 false。
#[derive(Debug, Clone, Default)]
pub struct IpMatcher {
    /// 白名单网段集合。
    allow: Vec<IpCidr>,
    /// 黑名单网段集合。
    deny: Vec<IpCidr>,
}

impl IpMatcher {
    /// 从配置构造匹配器：逐条解析黑 / 白名单，非法项记 WARN 并跳过。
    pub fn from_config(cfg: &IpListConfig) -> Self {
        Self {
            allow: parse_list(&cfg.allow, "白名单"),
            deny: parse_list(&cfg.deny, "黑名单"),
        }
    }

    /// 是否命中白名单（豁免一切应用层防护）。
    fn is_allowed(&self, ip: &IpAddr) -> bool {
        self.allow.iter().any(|c| c.contains(ip))
    }

    /// 是否命中黑名单（直接拒绝）。
    fn is_denied(&self, ip: &IpAddr) -> bool {
        self.deny.iter().any(|c| c.contains(ip))
    }

    /// 黑 / 白名单是否均为空（未配置任何名单项）。
    fn is_empty(&self) -> bool {
        self.allow.is_empty() && self.deny.is_empty()
    }
}

/// 解析一组名单项为网段集合，非法项记 WARN 并跳过。
fn parse_list(items: &[String], kind: &str) -> Vec<IpCidr> {
    items
        .iter()
        .filter_map(|s| match IpCidr::parse(s) {
            Some(c) => Some(c),
            None => {
                tracing::warn!(条目 = %s, 名单 = kind, "IP 名单项解析失败，已跳过");
                None
            }
        })
        .collect()
}

/// 单个 IP 在当前窗内的异常信号计数状态。
#[derive(Debug, Clone, Copy)]
struct SignalState {
    /// 当前窗内累计的异常信号数。
    count: u64,
    /// 当前窗起始时刻；距今超过窗口时长即翻入新窗、计数清零。
    window_start: Instant,
}

/// 触发过期键清理的表大小阈值：低于此值不扫描，避免给常态小表加无谓开销。
const EVICT_THRESHOLD: usize = 1024;

/// 自动封禁登记表：进程内内存维护「IP → 异常信号窗计数」与「IP → 封禁到期时刻」。
///
/// 随 `AppState` 经 `Arc` 共享。封禁与信号计数均经各自 `Mutex` 保护，并发下计数一致；
/// 过期键在加锁期间按表大小阈值顺带清理，防止无界增长。封禁状态重启即清（不落 DB）。
pub struct BanRegistry {
    /// 各 IP 当前窗内的异常信号计数。
    signals: Mutex<HashMap<IpAddr, SignalState>>,
    /// 各被封禁 IP 的封禁到期时刻；当前时刻早于到期即处于封禁中。
    bans: Mutex<HashMap<IpAddr, Instant>>,
}

impl BanRegistry {
    /// 构造空的封禁登记表。
    pub fn new() -> Self {
        Self {
            signals: Mutex::new(HashMap::new()),
            bans: Mutex::new(HashMap::new()),
        }
    }

    /// 查询给定 IP 当前是否处于封禁中；顺带清理已到期的封禁记录（到期自动解封）。
    fn is_banned(&self, ip: &IpAddr, now: Instant) -> bool {
        let mut guard = self.bans.lock().unwrap_or_else(|e| e.into_inner());
        match guard.get(ip) {
            Some(&until) if now < until => true,
            // 已到期：移除记录，视为未封禁（自动解封）
            Some(_) => {
                guard.remove(ip);
                false
            }
            None => false,
        }
    }

    /// 记录一次来源 IP 的异常信号；窗内累计达阈值则封禁该 IP 一个时长。
    ///
    /// `now` 由调用方传入便于测试可控时钟；窗口翻新、过期清理在此一并完成。
    /// 返回是否**因本次信号新触发了封禁**（供日志区分，避免重复刷封禁日志）。
    fn record_signal(
        &self,
        ip: IpAddr,
        threshold: u64,
        window: Duration,
        ban_duration: Duration,
        now: Instant,
    ) -> bool {
        let crossed = {
            let mut guard = self.signals.lock().unwrap_or_else(|e| e.into_inner());
            evict_expired_signals(&mut guard, window, now);
            let entry = guard.entry(ip).or_insert(SignalState {
                count: 0,
                window_start: now,
            });
            // 当前窗已越过窗口时长：翻入新窗、计数清零
            if now.duration_since(entry.window_start) >= window {
                entry.count = 0;
                entry.window_start = now;
            }
            entry.count += 1;
            entry.count >= threshold
        };
        if !crossed {
            return false;
        }
        // 达阈值：登记封禁到期时刻，并清掉该 IP 的信号计数（封禁期内不再重复累计触发）
        let until = now + ban_duration;
        let newly = {
            let mut bans = self.bans.lock().unwrap_or_else(|e| e.into_inner());
            evict_expired_bans(&mut bans, now);
            // 仅当原先未封禁（或已到期）才视为「新封禁」，避免封禁期内反复刷日志
            let was_active = matches!(bans.get(&ip), Some(&u) if now < u);
            bans.insert(ip, until);
            !was_active
        };
        {
            let mut signals = self.signals.lock().unwrap_or_else(|e| e.into_inner());
            signals.remove(&ip);
        }
        newly
    }

    /// 当前封禁中的 IP 数量（供测试与后续观测读取）。
    #[cfg(test)]
    fn active_ban_count(&self, now: Instant) -> usize {
        let guard = self.bans.lock().unwrap_or_else(|e| e.into_inner());
        guard.values().filter(|&&until| now < until).count()
    }
}

impl Default for BanRegistry {
    fn default() -> Self {
        Self::new()
    }
}

/// 概率性清理过期信号键：仅当表偏大时才扫描，控制热路径开销、防表无界增长。
fn evict_expired_signals(state: &mut HashMap<IpAddr, SignalState>, window: Duration, now: Instant) {
    if state.len() < EVICT_THRESHOLD {
        return;
    }
    state.retain(|_, s| now.duration_since(s.window_start) < window);
}

/// 清理已到期封禁键：仅当表偏大时扫描，防表无界增长。
fn evict_expired_bans(state: &mut HashMap<IpAddr, Instant>, now: Instant) {
    if state.len() < EVICT_THRESHOLD {
        return;
    }
    state.retain(|_, &mut until| now < until);
}

/// 异常检测与自动封禁中间件：置于限流之前（热路径前端），先于重处理。
///
/// 顺序：白名单豁免 → 黑名单拒 → 封禁中拒 → 放行业务 → 按响应状态统计异常信号、触阈即封禁。
/// 名单与封禁均未启用（名单空 + `ban.enabled=false`）时只多一次空名单匹配，开销可忽略。
pub async fn anomaly_ban_layer(
    State(state): State<AppState>,
    request: Request,
    next: Next,
) -> Response {
    // 防护整体未启用（名单空 + 异常检测关闭）：直接放行，零计数 / 零加锁开销。
    // 此快路径确保 FR-53 默认配置不给请求热路径增加任何可观测开销。
    if state.ip_matcher.is_empty() && !state.config.protection.ban.enabled {
        return next.run(request).await;
    }

    let ip = client_ip(&request);

    // 无法取得连接 IP（如单元测试未注入 ConnectInfo）：不做名单 / 封禁判定，直接放行
    let Some(ip) = ip else {
        return next.run(request).await;
    };

    // —— 白名单优先级最高：命中即豁免一切应用层防护，照常进入业务、不统计异常 ——
    if state.ip_matcher.is_allowed(&ip) {
        return next.run(request).await;
    }

    // —— 黑名单：命中即在进入业务前直接拒 403 ——
    if state.ip_matcher.is_denied(&ip) {
        return forbidden("来源 IP 在黑名单中，访问被拒绝");
    }

    let ban_cfg = &state.config.protection.ban;

    // —— 封禁中：命中即拒 403（即便异常检测已关闭，仍需放行已到期解封）——
    let now = Instant::now();
    if state.ban_registry.is_banned(&ip, now) {
        return forbidden("来源 IP 处于封禁中，请稍后再试");
    }

    // 异常检测关闭：不统计、不封禁，放行
    if !ban_cfg.enabled {
        return next.run(request).await;
    }

    // 放行业务，按响应状态判定是否为异常信号
    let response = next.run(request).await;
    if is_anomaly_signal(response.status()) {
        let window = Duration::from_secs(ban_cfg.window_secs.max(1));
        let ban_duration = Duration::from_secs(ban_cfg.duration_secs.max(1));
        let newly = state.ban_registry.record_signal(
            ip,
            ban_cfg.threshold.max(1),
            window,
            ban_duration,
            now,
        );
        if newly {
            // 仅记封禁动作，不含凭据 / 完整路径等敏感信息
            tracing::warn!(来源 = %ip, 时长秒 = ban_cfg.duration_secs, "来源 IP 异常访问触顶，已自动封禁");
        }
    }
    response
}

/// 取连接级来源 IP（由 `into_make_service_with_connect_info` 注入）；缺失（如单元测试）返回 None。
///
/// 只认连接对端地址，**不读 `X-Forwarded-For` 等可伪造头**，确保伪造来源不绕过黑名单 / 封禁。
fn client_ip(request: &Request) -> Option<IpAddr> {
    request
        .extensions()
        .get::<ConnectInfo<SocketAddr>>()
        .map(|ci| ci.0.ip())
}

/// 判断响应状态是否计为异常信号：4xx 客户端错误（含 401/403 鉴权失败、429 限流拒绝）。
///
/// 仅统计客户端侧可疑响应；2xx/3xx 正常响应与 5xx 服务端错误不计（5xx 是本服务问题，不应据此封禁来源）。
fn is_anomaly_signal(status: StatusCode) -> bool {
    status.is_client_error()
}

/// 构造 403 响应：统一错误体（与 `ApiError::Forbidden` 同形），不泄露名单 / 封禁内部细节。
fn forbidden(message: &str) -> Response {
    let body = Json(json!({
        "error": {
            "code": "forbidden",
            "message": message,
        }
    }));
    (StatusCode::FORBIDDEN, body).into_response()
}

#[cfg(test)]
mod tests {
    use super::*;

    fn ip(s: &str) -> IpAddr {
        s.parse().unwrap()
    }

    // ===== CIDR 匹配纯函数 =====

    #[test]
    fn 单ip精确匹配() {
        let c = IpCidr::parse("203.0.113.7").unwrap();
        assert!(c.contains(&ip("203.0.113.7")));
        assert!(!c.contains(&ip("203.0.113.8")));
    }

    #[test]
    fn cidr网段匹配v4() {
        let c = IpCidr::parse("10.0.0.0/8").unwrap();
        assert!(c.contains(&ip("10.1.2.3")));
        assert!(c.contains(&ip("10.255.255.255")));
        assert!(!c.contains(&ip("11.0.0.1")));
    }

    #[test]
    fn cidr非整字节前缀匹配() {
        // /20：前 20 位匹配
        let c = IpCidr::parse("192.168.16.0/20").unwrap();
        assert!(c.contains(&ip("192.168.16.1")));
        assert!(c.contains(&ip("192.168.31.255")));
        assert!(!c.contains(&ip("192.168.32.1")));
    }

    #[test]
    fn cidr网段匹配v6() {
        let c = IpCidr::parse("2001:db8::/32").unwrap();
        assert!(c.contains(&ip("2001:db8::1")));
        assert!(c.contains(&ip("2001:db8:ffff::1")));
        assert!(!c.contains(&ip("2001:db9::1")));
    }

    #[test]
    fn v4与v6不跨族匹配() {
        let c = IpCidr::parse("0.0.0.0/0").unwrap();
        // 全 0/0 的 v4 网段不应匹配任何 v6 地址
        assert!(!c.contains(&ip("::1")));
        assert!(c.contains(&ip("1.2.3.4")));
    }

    #[test]
    fn 非法名单项解析失败() {
        assert!(IpCidr::parse("not-an-ip").is_none());
        assert!(IpCidr::parse("10.0.0.0/99").is_none());
        assert!(IpCidr::parse("10.0.0.0/abc").is_none());
    }

    #[test]
    fn 匹配器跳过非法项不影响合法项() {
        let cfg = IpListConfig {
            allow: vec!["不是IP".into(), "1.2.3.4".into()],
            deny: vec!["10.0.0.0/8".into()],
        };
        let m = IpMatcher::from_config(&cfg);
        // 合法白名单项仍生效
        assert!(m.is_allowed(&ip("1.2.3.4")));
        assert!(!m.is_allowed(&ip("1.2.3.5")));
        // 黑名单网段生效
        assert!(m.is_denied(&ip("10.9.9.9")));
    }

    // ===== 封禁登记表 =====

    #[test]
    fn 异常信号达阈值触发封禁() {
        let reg = BanRegistry::new();
        let now = Instant::now();
        let window = Duration::from_secs(60);
        let dur = Duration::from_secs(900);
        let addr = ip("9.9.9.9");
        // 阈值 3：前 2 次不封禁
        assert!(!reg.record_signal(addr, 3, window, dur, now));
        assert!(!reg.is_banned(&addr, now));
        assert!(!reg.record_signal(addr, 3, window, dur, now));
        // 第 3 次达阈值，新触发封禁
        assert!(reg.record_signal(addr, 3, window, dur, now));
        assert!(reg.is_banned(&addr, now));
    }

    #[test]
    fn 封禁到期自动解封() {
        let reg = BanRegistry::new();
        let t0 = Instant::now();
        let window = Duration::from_secs(60);
        let dur = Duration::from_secs(10);
        let addr = ip("9.9.9.9");
        for _ in 0..3 {
            reg.record_signal(addr, 3, window, dur, t0);
        }
        assert!(reg.is_banned(&addr, t0));
        // 推进超过封禁时长：自动解封
        let t1 = t0 + Duration::from_secs(11);
        assert!(!reg.is_banned(&addr, t1));
    }

    #[test]
    fn 不同ip互不影响不误封() {
        let reg = BanRegistry::new();
        let now = Instant::now();
        let window = Duration::from_secs(60);
        let dur = Duration::from_secs(900);
        // 攻击 IP 触顶被封
        for _ in 0..3 {
            reg.record_signal(ip("9.9.9.9"), 3, window, dur, now);
        }
        assert!(reg.is_banned(&ip("9.9.9.9"), now));
        // 正常 IP 偶发异常不受影响
        reg.record_signal(ip("1.1.1.1"), 3, window, dur, now);
        assert!(!reg.is_banned(&ip("1.1.1.1"), now));
    }

    #[test]
    fn 窗口翻新后信号计数清零() {
        let reg = BanRegistry::new();
        let t0 = Instant::now();
        let window = Duration::from_secs(60);
        let dur = Duration::from_secs(900);
        let addr = ip("1.1.1.1");
        // 阈值 5，先打 4 次（未触顶）
        for _ in 0..4 {
            assert!(!reg.record_signal(addr, 5, window, dur, t0));
        }
        // 跨窗后再打 4 次：计数应已清零，仍不触顶（证明信号不跨窗累计、防长期误封）
        let t1 = t0 + Duration::from_secs(61);
        for _ in 0..4 {
            assert!(!reg.record_signal(addr, 5, window, dur, t1));
        }
        assert!(!reg.is_banned(&addr, t1));
    }

    #[test]
    fn 封禁期内不重复触发新封禁() {
        let reg = BanRegistry::new();
        let now = Instant::now();
        let window = Duration::from_secs(60);
        let dur = Duration::from_secs(900);
        let addr = ip("9.9.9.9");
        for _ in 0..2 {
            reg.record_signal(addr, 3, window, dur, now);
        }
        // 第 3 次新触发封禁
        assert!(reg.record_signal(addr, 3, window, dur, now));
        // 封禁期内信号已清零，即便再积累达阈值也只刷新到期、不算「新封禁」
        for _ in 0..2 {
            reg.record_signal(addr, 3, window, dur, now);
        }
        assert!(!reg.record_signal(addr, 3, window, dur, now));
        assert!(reg.is_banned(&addr, now));
    }

    #[test]
    fn 并发记录信号封禁状态一致() {
        use std::sync::Arc;
        let reg = Arc::new(BanRegistry::new());
        let now = Instant::now();
        let window = Duration::from_secs(60);
        let dur = Duration::from_secs(900);
        let addr = ip("9.9.9.9");
        let threshold = 200u64;
        let threads = 8;
        let per = 50u64; // 总信号 400，远超阈值 200
        let mut handles = Vec::new();
        for _ in 0..threads {
            let reg = Arc::clone(&reg);
            handles.push(std::thread::spawn(move || {
                let mut newly = 0u64;
                for _ in 0..per {
                    if reg.record_signal(addr, threshold, window, dur, now) {
                        newly += 1;
                    }
                }
                newly
            }));
        }
        let total_newly: u64 = handles.into_iter().map(|h| h.join().unwrap()).sum();
        // 并发下封禁只应被「新触发」恰好一次（无重复封禁、无丢封禁）
        assert_eq!(total_newly, 1, "并发达阈值应只新触发一次封禁");
        assert!(reg.is_banned(&addr, now));
        assert_eq!(reg.active_ban_count(now), 1, "应恰有一个 IP 处于封禁");
    }

    #[test]
    fn 过期信号键被清理防无界增长() {
        let reg = BanRegistry::new();
        let t0 = Instant::now();
        let window = Duration::from_secs(60);
        let dur = Duration::from_secs(900);
        // 填入超过清理阈值数量的不同 IP（阈值高到不触发封禁）
        for i in 0..(EVICT_THRESHOLD + 10) {
            let a = IpAddr::V4(std::net::Ipv4Addr::new(
                10,
                0,
                (i / 256) as u8,
                (i % 256) as u8,
            ));
            reg.record_signal(a, u64::MAX, window, dur, t0);
        }
        {
            let g = reg.signals.lock().unwrap();
            assert!(g.len() >= EVICT_THRESHOLD, "填充后信号表应较大");
        }
        // 跨窗后再访问一个新键，触发过期清理
        let t1 = t0 + Duration::from_secs(120);
        reg.record_signal(ip("8.8.8.8"), u64::MAX, window, dur, t1);
        let g = reg.signals.lock().unwrap();
        assert!(
            g.len() < EVICT_THRESHOLD,
            "过期信号键应被清理，实际剩 {}",
            g.len()
        );
    }

    // ===== 异常信号判定 =====

    #[test]
    fn 异常信号仅计客户端错误() {
        assert!(is_anomaly_signal(StatusCode::NOT_FOUND));
        assert!(is_anomaly_signal(StatusCode::UNAUTHORIZED));
        assert!(is_anomaly_signal(StatusCode::FORBIDDEN));
        assert!(is_anomaly_signal(StatusCode::TOO_MANY_REQUESTS));
        // 正常响应与服务端错误不计
        assert!(!is_anomaly_signal(StatusCode::OK));
        assert!(!is_anomaly_signal(StatusCode::FOUND));
        assert!(!is_anomaly_signal(StatusCode::INTERNAL_SERVER_ERROR));
    }
}

// ============ 中间件端到端测试（经真实路由）============
#[cfg(test)]
mod middleware_tests {
    use super::super::tests::测试用状态;
    use super::super::{build_router, AppState};
    use super::{BanRegistry, IpMatcher};
    use crate::config::IpListConfig;
    use axum::body::Body;
    use axum::extract::ConnectInfo;
    use axum::http::{Request, StatusCode};
    use std::net::SocketAddr;
    use std::sync::Arc;
    use tower::ServiceExt;

    /// 以给定黑/白名单定制测试状态（异常检测保持关闭，单独观察名单行为）。
    async fn 名单状态(allow: Vec<&str>, deny: Vec<&str>) -> (AppState, tempfile::TempDir) {
        let (mut state, dir) = 测试用状态().await;
        let cfg = IpListConfig {
            allow: allow.into_iter().map(String::from).collect(),
            deny: deny.into_iter().map(String::from).collect(),
        };
        state.ip_matcher = Arc::new(IpMatcher::from_config(&cfg));
        (state, dir)
    }

    /// 以给定异常检测阈值定制测试状态（名单为空，单独观察封禁行为）。
    async fn 封禁状态(threshold: u64) -> (AppState, tempfile::TempDir) {
        let (mut state, dir) = 测试用状态().await;
        let mut cfg = (*state.config).clone();
        cfg.protection.ban.enabled = true;
        cfg.protection.ban.window_secs = 60;
        cfg.protection.ban.threshold = threshold;
        cfg.protection.ban.duration_secs = 900;
        state.config = Arc::new(cfg);
        // 共享同一封禁登记表，确保跨请求计数累积
        state.ban_registry = Arc::new(BanRegistry::new());
        (state, dir)
    }

    /// 用指定连接 IP（可选 XFF）对某路径发 GET，返回状态码（经真实路由与异常封禁中间件）。
    async fn 打(app: axum::Router, ip: &str, uri: &str, xff: Option<&str>) -> StatusCode {
        let addr: SocketAddr = format!("{ip}:50000").parse().unwrap();
        let mut builder = Request::builder().uri(uri);
        if let Some(v) = xff {
            builder = builder.header("X-Forwarded-For", v);
        }
        let mut req = builder.body(Body::empty()).unwrap();
        req.extensions_mut().insert(ConnectInfo(addr));
        app.oneshot(req).await.unwrap().status()
    }

    #[tokio::test]
    async fn 黑名单来源被拒_403() {
        let (state, _dir) = 名单状态(vec![], vec!["9.9.9.9"]).await;
        let app = build_router(state);
        // 黑名单 IP 即便访问健康检查也被拒
        assert_eq!(
            打(app.clone(), "9.9.9.9", "/health", None).await,
            StatusCode::FORBIDDEN
        );
        // 非黑名单 IP 正常放行
        assert_eq!(打(app, "1.1.1.1", "/health", None).await, StatusCode::OK);
    }

    #[tokio::test]
    async fn 黑名单网段按cidr匹配() {
        let (state, _dir) = 名单状态(vec![], vec!["10.0.0.0/8"]).await;
        let app = build_router(state);
        assert_eq!(
            打(app.clone(), "10.1.2.3", "/health", None).await,
            StatusCode::FORBIDDEN
        );
        assert_eq!(打(app, "11.0.0.1", "/health", None).await, StatusCode::OK);
    }

    #[tokio::test]
    async fn 白名单优先级高于黑名单且豁免封禁() {
        // 同一 IP 同时在黑白名单：白名单优先，应放行
        let (mut state, _dir) = 名单状态(vec!["1.1.1.1"], vec!["1.1.1.1"]).await;
        // 同时开启异常检测且阈值极低，验证白名单豁免封禁统计
        let mut cfg = (*state.config).clone();
        cfg.protection.ban.enabled = true;
        cfg.protection.ban.window_secs = 60;
        cfg.protection.ban.threshold = 1;
        cfg.protection.ban.duration_secs = 900;
        state.config = Arc::new(cfg);
        let app = build_router(state);
        // 连发若干次会产生异常信号的请求（不存在仓库 → 4xx），白名单应一律放行、不被封禁
        for _ in 0..5 {
            let st = 打(app.clone(), "1.1.1.1", "/no-such-repo/x", None).await;
            assert_ne!(st, StatusCode::FORBIDDEN, "白名单来源不应被封禁/拒绝");
        }
    }

    #[tokio::test]
    async fn 正常高频访问不误封() {
        // 阈值 5，但正常请求返回 2xx（健康检查），不产生异常信号，连发远超阈值也不封
        let (state, _dir) = 封禁状态(5).await;
        let app = build_router(state);
        for _ in 0..20 {
            assert_eq!(
                打(app.clone(), "1.1.1.1", "/health", None).await,
                StatusCode::OK
            );
        }
    }

    #[tokio::test]
    async fn 异常访问达阈值后自动封禁() {
        // 阈值 3：连发产生 4xx 的请求（不存在仓库），达阈值后该 IP 被封、后续一律 403
        let (state, _dir) = 封禁状态(3).await;
        let app = build_router(state);
        // 前 3 次进入业务返回非 403 的客户端错误（仓库不存在 → 404）
        for _ in 0..3 {
            let st = 打(app.clone(), "9.9.9.9", "/no-such-repo/x", None).await;
            assert_ne!(st, StatusCode::FORBIDDEN, "未触顶前不应被封禁拒绝");
        }
        // 触顶后：即便访问正常端点也被封禁中间件前置拒 403
        assert_eq!(
            打(app.clone(), "9.9.9.9", "/health", None).await,
            StatusCode::FORBIDDEN
        );
        // 另一正常 IP 不受影响（防误杀）
        assert_eq!(打(app, "1.1.1.1", "/health", None).await, StatusCode::OK);
    }

    #[tokio::test]
    async fn xff伪造来源不绕过封禁() {
        // 同一连接 IP，每次伪造不同 XFF，仍按真实连接 IP 统计异常、照常触顶被封（防绕过）
        let (state, _dir) = 封禁状态(3).await;
        let app = build_router(state);
        for i in 0..3 {
            let xff = format!("203.0.113.{i}");
            let _ = 打(app.clone(), "9.9.9.9", "/no-such-repo/x", Some(&xff)).await;
        }
        // 换个 XFF 仍被封（XFF 不被采信）
        assert_eq!(
            打(app, "9.9.9.9", "/health", Some("198.51.100.7")).await,
            StatusCode::FORBIDDEN
        );
    }

    #[tokio::test]
    async fn 异常检测关闭时不封禁() {
        // 默认测试状态异常检测关闭：连发大量 4xx 也不封禁（须运维显式开启）
        let (state, _dir) = 测试用状态().await;
        let app = build_router(state);
        for _ in 0..50 {
            let st = 打(app.clone(), "9.9.9.9", "/no-such-repo/x", None).await;
            assert_ne!(st, StatusCode::FORBIDDEN, "关闭异常检测时不应封禁");
        }
    }
}
