//! 多维速率限制与并发上限（FR-33 + FR-51，ADR-0008 的「多维限流 + 并发/连接上限」部分）。
//!
//! 进程内存按 **IP / 身份（用户及其所有 Token）/ 仓库** 维度用固定时间窗计数请求，单窗内任一
//! 维度超阈值即返回 `429 Too Many Requests`（带 `Retry-After`）；并按 **IP / 用户 / 仓库** 维度
//! 限制在途并发请求数，超并发上限同样返回 `429`。本批承接 FR-33 基础限流向多维扩展，**不做**
//! FR-52~56 的慢速 / 封禁 / CC / WAF / 告警。
//!
//! 设计要点（对齐 testing-and-quality §2.7）：
//! - **热路径低开销**：限流计数每请求只取一次 `Mutex`、做整型自增与窗口比较，临界区内无 IO、无
//!   格式化；并发计数走分片 `Mutex`（按键散列分片，降低争用），入站 +1、请求结束 -1。
//! - **并发计数可靠归还**：并发占用以 RAII `ConcurrencyGuard` 持有，无论请求成功 / 出错 / panic，
//!   `Drop` 都会 -1，绝不泄漏在途计数。
//! - **防误杀**：默认阈值保守且按窗口宽放，新增的仓库 / 并发维度默认 0（不启用），正常包管理器
//!   批量并发拉取不应触顶；整体默认关闭，须运维显式开启。
//! - **防绕过**：来源 IP 取连接级 `ConnectInfo`（与登录防护一致），**不采信 XFF 头**，伪造来源不
//!   绕过；已认证请求额外按用户维度计数，轮换 IP 也受身份维度阈值约束。
//! - **配置即时生效**：阈值 / 窗口 / 并发上限从运行时防护热替换槽（`AppState.protection`）的当前快照
//!   读取，PATCH 配置热替换后下个请求即按新值判定（FR-79）。

use std::collections::HashMap;
use std::net::SocketAddr;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use axum::{
    extract::{ConnectInfo, Request, State},
    http::{header::RETRY_AFTER, StatusCode},
    middleware::Next,
    response::{IntoResponse, Response},
    Json,
};
use serde_json::json;

use crate::auth::AuthIdentity;
use crate::metrics_keys as keys;

use super::alerts::ProtectionDimension;
use super::AppState;

/// 限流维度键：把来源归一为 IP / 身份 / 仓库维度，避免不同维度计数互相串味。
#[derive(Debug, Clone, PartialEq, Eq, Hash)]
enum RateKey {
    /// 按连接来源 IP 计数（匿名与未认证流量的主维度）。
    Ip(String),
    /// 按已认证主体（用户 id，含其所有 Token / 会话）计数；轮换 IP 也受此约束。
    Identity(String),
    /// 按目标仓库名计数（FR-51 仓库维度，按格式路径首段解析）。
    Repo(String),
}

/// 单个键在当前固定窗内的计数状态。
#[derive(Debug, Clone, Copy)]
struct WindowState {
    /// 当前窗内已计数的请求数。
    count: u64,
    /// 当前窗的起始时刻；距今超过窗口时长即翻入新窗、计数清零。
    window_start: Instant,
}

/// 进程内速率限制器：线程安全地维护各维度键的固定窗计数与在途并发计数。
///
/// 克隆经 `Arc`（随 `AppState`）共享；本结构自身不实现 `Clone`，由调用方用 `Arc` 包裹。
pub struct RateLimiter {
    /// 各维度键的窗口计数表。
    state: Mutex<HashMap<RateKey, WindowState>>,
    /// 触顶被拒请求累计数（供观测，不影响判定）。
    rejected: AtomicU64,
    /// 并发上限计数器（FR-51 并发/连接上限）。
    concurrency: ConcurrencyLimiter,
}

/// 限流判定结果：放行，或被拒并附建议等待秒数。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Decision {
    /// 允许通过。
    Allow,
    /// 超阈值被拒，附 `Retry-After` 建议秒数（当前窗剩余时长，至少 1 秒）。
    Limited { retry_after_secs: u64 },
}

impl RateLimiter {
    /// 构造空的限流器。
    pub fn new() -> Self {
        Self {
            state: Mutex::new(HashMap::new()),
            rejected: AtomicU64::new(0),
            concurrency: ConcurrencyLimiter::new(),
        }
    }

    /// 对单个维度键计数并按给定阈值 / 窗口判定。
    ///
    /// `now` 由调用方传入便于测试可控时钟；窗口翻新与过期清理在此一并完成。
    fn check_key(
        &self,
        key: RateKey,
        max_requests: u64,
        window: Duration,
        now: Instant,
    ) -> Decision {
        let mut guard = self.state.lock().unwrap_or_else(|e| e.into_inner());

        // 顺带按概率清理过期键，避免一次性维护成本压在某个请求上，也防止表无界增长。
        Self::maybe_evict_expired(&mut guard, window, now);

        let entry = guard.entry(key).or_insert(WindowState {
            count: 0,
            window_start: now,
        });
        // 当前窗已越过窗口时长：翻入新窗、计数清零
        if now.duration_since(entry.window_start) >= window {
            entry.count = 0;
            entry.window_start = now;
        }
        entry.count += 1;
        if entry.count > max_requests {
            let elapsed = now.duration_since(entry.window_start);
            let remaining = window.saturating_sub(elapsed).as_secs().max(1);
            drop(guard);
            self.rejected.fetch_add(1, Ordering::Relaxed);
            Decision::Limited {
                retry_after_secs: remaining,
            }
        } else {
            Decision::Allow
        }
    }

    /// 概率性清理过期键：仅当表偏大时才扫描，且只在已加锁期间顺带做，控制热路径开销。
    fn maybe_evict_expired(
        state: &mut HashMap<RateKey, WindowState>,
        window: Duration,
        now: Instant,
    ) {
        // 表较小直接跳过（绝大多数请求走此分支，零额外扫描开销）
        if state.len() < EVICT_THRESHOLD {
            return;
        }
        state.retain(|_, s| now.duration_since(s.window_start) < window);
    }

    /// 触顶被拒请求累计数（供测试与后续观测读取）。
    pub fn rejected_count(&self) -> u64 {
        self.rejected.load(Ordering::Relaxed)
    }
}

impl Default for RateLimiter {
    fn default() -> Self {
        Self::new()
    }
}

/// 触发过期清理的表大小阈值：低于此值不扫描，避免给常态小表加无谓开销。
const EVICT_THRESHOLD: usize = 1024;

/// 并发计数分片数：按键散列分片，降低高并发下单锁争用（取 2 的幂便于掩码取模）。
const CONCURRENCY_SHARDS: usize = 16;

/// 在途并发上限计数器（FR-51 并发/连接上限）。
///
/// 维护各维度键当前在途请求数：入站 `try_acquire` +1，请求结束由 `ConcurrencyGuard::drop` -1。
/// 计数表按键散列分到多个分片，各分片独立加锁，避免所有请求争用同一把锁（热路径开销可控）。
/// 计数归零的键随即从分片表移除，防止无界增长。
struct ConcurrencyLimiter {
    /// 分片计数表，每片维护「键 → 当前在途数」。
    shards: Vec<Mutex<HashMap<RateKey, u64>>>,
}

impl ConcurrencyLimiter {
    /// 构造空的分片并发计数器。
    fn new() -> Self {
        let mut shards = Vec::with_capacity(CONCURRENCY_SHARDS);
        for _ in 0..CONCURRENCY_SHARDS {
            shards.push(Mutex::new(HashMap::new()));
        }
        Self { shards }
    }

    /// 按键散列选定分片。
    fn shard(&self, key: &RateKey) -> &Mutex<HashMap<RateKey, u64>> {
        use std::collections::hash_map::DefaultHasher;
        use std::hash::{Hash, Hasher};
        let mut h = DefaultHasher::new();
        key.hash(&mut h);
        // CONCURRENCY_SHARDS 为 2 的幂，用掩码取模
        let idx = (h.finish() as usize) & (CONCURRENCY_SHARDS - 1);
        &self.shards[idx]
    }

    /// 尝试占用一个在途名额：当前在途数 `< max` 时 +1 并返回 `true`；已达上限返回 `false`、不增计数。
    fn try_increment(&self, key: &RateKey, max: u64) -> bool {
        let mut guard = self.shard(key).lock().unwrap_or_else(|e| e.into_inner());
        let entry = guard.entry(key.clone()).or_insert(0);
        if *entry >= max {
            // 已达上限：不占用，若是本次新建的 0 值键则清掉，避免遗留空键
            if *entry == 0 {
                guard.remove(key);
            }
            return false;
        }
        *entry += 1;
        true
    }

    /// 归还一个在途名额：计数 -1，归零后移除键，防止表无界增长。
    fn release(&self, key: &RateKey) {
        let mut guard = self.shard(key).lock().unwrap_or_else(|e| e.into_inner());
        if let Some(entry) = guard.get_mut(key) {
            *entry = entry.saturating_sub(1);
            if *entry == 0 {
                guard.remove(key);
            }
        }
    }
}

impl RateLimiter {
    /// 尝试为某并发维度键占用一个在途名额（FR-51 并发上限）。
    ///
    /// `max == 0` 表示该维度不限并发，返回一个不计数的空占位 guard。占用成功返回 `Some(guard)`，
    /// `guard` 在 `Drop` 时自动归还名额；超上限返回 `None`（调用方据此拒 429）。
    fn acquire_concurrency(self: &Arc<Self>, key: RateKey, max: u64) -> Option<ConcurrencyGuard> {
        if max == 0 {
            return Some(ConcurrencyGuard { limiter: None, key });
        }
        if self.concurrency.try_increment(&key, max) {
            Some(ConcurrencyGuard {
                limiter: Some(Arc::clone(self)),
                key,
            })
        } else {
            // 并发触顶也计入被拒累计数（与限流被拒同口径，供观测）
            self.rejected.fetch_add(1, Ordering::Relaxed);
            None
        }
    }
}

/// 在途并发名额的 RAII 占位：持有期间计数 +1，`Drop` 时 -1，确保异常 / panic 也可靠归还。
///
/// `limiter` 为 `None` 时表示该维度不限并发（`max == 0`），`Drop` 不做任何事。
struct ConcurrencyGuard {
    /// 归还目标限流器（不限并发时为 `None`）。
    limiter: Option<Arc<RateLimiter>>,
    /// 本次占用的维度键。
    key: RateKey,
}

impl Drop for ConcurrencyGuard {
    fn drop(&mut self) {
        if let Some(limiter) = &self.limiter {
            limiter.concurrency.release(&self.key);
        }
    }
}

/// 限流中间件：置于身份解析之后、业务 handler 之前。
///
/// 先按 IP / 身份（用户）/ 仓库维度做固定窗速率判定，任一触顶即在进入业务前返回 429；再按
/// IP / 用户 / 仓库维度尝试占用并发名额，任一超并发上限同样返回 429。占用的并发名额由 RAII
/// guard 持有至 `next.run` 返回后自动归还（请求结束可靠 -1，异常 / panic 也不泄漏）。
/// 配置未启用（`enabled=false`）时直接放行，零计数开销。
pub async fn rate_limit_layer(
    State(state): State<AppState>,
    request: Request,
    next: Next,
) -> Response {
    // 从热替换槽取当前快照（读锁极短、锁外判定）；防护配置真源为本槽，不读 config.protection
    let snapshot = state.protection.snapshot();
    let cfg = &snapshot.config.rate_limit;
    if !cfg.enabled {
        return next.run(request).await;
    }

    let now = Instant::now();
    let window = Duration::from_secs(cfg.window_secs.max(1));
    let limiter = &state.rate_limiter;

    // 预解析三维来源：IP 取连接级 ConnectInfo（不采信 XFF）；用户取已认证身份；仓库取格式路径首段
    let ip = client_ip(&request);
    let user_id = authenticated_user_id(&request);
    let repo = repo_name(&request);

    // —— 速率维度：任一触顶即拒 429 ——
    if let Some(ip) = &ip {
        if let Decision::Limited { retry_after_secs } =
            limiter.check_key(RateKey::Ip(ip.clone()), cfg.ip_max_requests, window, now)
        {
            record_rejection(&state, &snapshot.config.alerts, keys::DIMENSION_IP);
            return too_many_requests(retry_after_secs);
        }
    }
    if let Some(user_id) = &user_id {
        if let Decision::Limited { retry_after_secs } = limiter.check_key(
            RateKey::Identity(user_id.clone()),
            cfg.identity_max_requests,
            window,
            now,
        ) {
            record_rejection(&state, &snapshot.config.alerts, keys::DIMENSION_TOKEN);
            return too_many_requests(retry_after_secs);
        }
    }
    if let Some(repo) = &repo {
        if cfg.repo_max_requests > 0 {
            if let Decision::Limited { retry_after_secs } = limiter.check_key(
                RateKey::Repo(repo.clone()),
                cfg.repo_max_requests,
                window,
                now,
            ) {
                record_rejection(&state, &snapshot.config.alerts, keys::DIMENSION_REPO);
                return too_many_requests(retry_after_secs);
            }
        }
    }

    // —— 并发维度：依次占用 IP / 用户 / 仓库名额，任一超上限即拒；占用 guard 持有至请求结束 ——
    // guard 变量绑定到本作用域，next.run 返回（含出错 / panic 展开）后随作用域 Drop 归还计数。
    let _ip_guard = match ip {
        Some(ip) => match limiter.acquire_concurrency(RateKey::Ip(ip), cfg.ip_max_concurrent) {
            Some(g) => Some(g),
            None => {
                record_rejection(&state, &snapshot.config.alerts, keys::DIMENSION_CONCURRENCY);
                return too_many_requests_concurrency();
            }
        },
        None => None,
    };
    let _user_guard = match user_id {
        Some(user_id) => {
            match limiter.acquire_concurrency(RateKey::Identity(user_id), cfg.user_max_concurrent) {
                Some(g) => Some(g),
                None => {
                    record_rejection(&state, &snapshot.config.alerts, keys::DIMENSION_CONCURRENCY);
                    return too_many_requests_concurrency();
                }
            }
        }
        None => None,
    };
    let _repo_guard = match repo {
        Some(repo) => {
            match limiter.acquire_concurrency(RateKey::Repo(repo), cfg.repo_max_concurrent) {
                Some(g) => Some(g),
                None => {
                    record_rejection(&state, &snapshot.config.alerts, keys::DIMENSION_CONCURRENCY);
                    return too_many_requests_concurrency();
                }
            }
        }
        None => None,
    };

    next.run(request).await
}

/// 在限流被拒命中点累加指标与告警评估（FR-56，ADR-0017）。
///
/// 指标用低基数标签 `dimension`（ip / token / repo / concurrency）；metrics 未启用时宏为 no-op。
/// 告警评估按 `RateLimit` 维度累加窗内计数，达阈值即告警；热路径只做原子累加 + 一次内存计数，不做 IO。
/// 告警配置由调用方从当前防护快照传入（与限流判定同一份快照，避免热替换中途读到不一致配置）。
fn record_rejection(
    state: &AppState,
    alerts_cfg: &crate::config::AlertsConfig,
    dimension: &'static str,
) {
    metrics::counter!(keys::RATE_LIMIT_REJECTED_TOTAL, keys::LABEL_DIMENSION => dimension)
        .increment(1);
    state
        .alert_engine
        .record(ProtectionDimension::RateLimit, alerts_cfg, Instant::now());
}

/// 取连接级来源 IP（由 `into_make_service_with_connect_info` 注入）；缺失（如单元测试）返回 None。
///
/// 只认连接对端地址，**不读 `X-Forwarded-For` 等可伪造头**，确保伪造来源 IP 不能绕过限流。
fn client_ip(request: &Request) -> Option<String> {
    request
        .extensions()
        .get::<ConnectInfo<SocketAddr>>()
        .map(|ci| ci.0.ip().to_string())
}

/// 从请求扩展取已认证用户 id；匿名 / 未认证返回 None。
///
/// 身份由 `identity_layer` 先行注入；按用户 id 计数使同一主体的多 Token / 会话共享额度。
fn authenticated_user_id(request: &Request) -> Option<String> {
    match request.extensions().get::<AuthIdentity>() {
        Some(AuthIdentity::Authenticated(u)) => Some(u.user_id.clone()),
        _ => None,
    }
}

/// 从请求路径解析目标仓库名（FR-51 仓库维度），无法判定时返回 None。
///
/// 格式 API 路径形如 `/{repo}/{path..}`，仓库名即首个路径段；跳过保留前缀（`api`/`v2`/`health`/
/// `metrics`/`assets` 等非仓库路由），避免把管理端点 / Docker / 健康检查误算到某个仓库维度。
/// Docker（`/v2/...`）的仓库名为多段且与路由解析耦合，本中间件不在此处解析其仓库维度。
fn repo_name(request: &Request) -> Option<String> {
    let path = request.uri().path();
    let first = path.trim_start_matches('/').split('/').next()?;
    if first.is_empty() || RESERVED_PATH_PREFIXES.contains(&first) {
        return None;
    }
    Some(first.to_string())
}

/// 非仓库路由的保留首段：这些前缀下的路径不归属任一仓库，不参与仓库维度限流 / 并发。
const RESERVED_PATH_PREFIXES: &[&str] = &["api", "v2", "health", "metrics", "assets"];

/// 构造并发超限的 429 响应：统一错误体，提示降低并发后重试（并发上限无固定重试窗，不带 Retry-After）。
fn too_many_requests_concurrency() -> Response {
    let body = Json(json!({
        "error": {
            "code": "too_many_requests",
            "message": "并发请求过多，请降低并发后重试",
        }
    }));
    (StatusCode::TOO_MANY_REQUESTS, body).into_response()
}

/// 构造 429 响应：统一错误体 + `Retry-After` 头（秒）。
fn too_many_requests(retry_after_secs: u64) -> Response {
    let body = Json(json!({
        "error": {
            "code": "too_many_requests",
            "message": format!("请求过于频繁，请在 {retry_after_secs} 秒后重试"),
        }
    }));
    (
        StatusCode::TOO_MANY_REQUESTS,
        [(RETRY_AFTER, retry_after_secs.to_string())],
        body,
    )
        .into_response()
}

#[cfg(test)]
mod tests {
    use super::*;

    /// 同一 IP 窗内未触顶应放行、触顶后被拒。
    #[test]
    fn ip维度触顶后被拒() {
        let rl = RateLimiter::new();
        let now = Instant::now();
        let window = Duration::from_secs(60);
        // 阈值 3：前 3 次放行
        for _ in 0..3 {
            assert_eq!(
                rl.check_key(RateKey::Ip("1.1.1.1".into()), 3, window, now),
                Decision::Allow
            );
        }
        // 第 4 次触顶被拒，带正的 Retry-After
        match rl.check_key(RateKey::Ip("1.1.1.1".into()), 3, window, now) {
            Decision::Limited { retry_after_secs } => assert!(retry_after_secs >= 1),
            other => panic!("应被限流，实际 {other:?}"),
        }
        assert_eq!(rl.rejected_count(), 1);
    }

    /// 不同 IP 互不影响：一个 IP 触顶不应误杀另一个正常 IP（防误杀）。
    #[test]
    fn 不同ip互不影响不误杀() {
        let rl = RateLimiter::new();
        let now = Instant::now();
        let window = Duration::from_secs(60);
        for _ in 0..4 {
            let _ = rl.check_key(RateKey::Ip("9.9.9.9".into()), 3, window, now);
        }
        // 攻击 IP 已被限
        assert!(matches!(
            rl.check_key(RateKey::Ip("9.9.9.9".into()), 3, window, now),
            Decision::Limited { .. }
        ));
        // 正常 IP 不受影响
        assert_eq!(
            rl.check_key(RateKey::Ip("1.1.1.1".into()), 3, window, now),
            Decision::Allow
        );
    }

    /// 窗口翻新后计数清零、自动恢复放行（防长期误杀）。
    #[test]
    fn 窗口翻新后恢复放行() {
        let rl = RateLimiter::new();
        let t0 = Instant::now();
        let window = Duration::from_secs(60);
        for _ in 0..3 {
            let _ = rl.check_key(RateKey::Ip("1.1.1.1".into()), 3, window, t0);
        }
        assert!(matches!(
            rl.check_key(RateKey::Ip("1.1.1.1".into()), 3, window, t0),
            Decision::Limited { .. }
        ));
        // 推进超过一个窗口：应翻新计数、放行
        let t1 = t0 + Duration::from_secs(61);
        assert_eq!(
            rl.check_key(RateKey::Ip("1.1.1.1".into()), 3, window, t1),
            Decision::Allow
        );
    }

    /// 身份维度独立于 IP 维度：同一用户换 IP 仍受身份阈值约束（防轮换 IP 绕过）。
    #[test]
    fn 身份维度跨ip生效() {
        let rl = RateLimiter::new();
        let now = Instant::now();
        let window = Duration::from_secs(60);
        // 身份阈值 2：第 3 次触顶
        assert_eq!(
            rl.check_key(RateKey::Identity("u1".into()), 2, window, now),
            Decision::Allow
        );
        assert_eq!(
            rl.check_key(RateKey::Identity("u1".into()), 2, window, now),
            Decision::Allow
        );
        assert!(matches!(
            rl.check_key(RateKey::Identity("u1".into()), 2, window, now),
            Decision::Limited { .. }
        ));
    }

    /// 表超过阈值时过期键被清理，防止无界增长（热路径开销可控）。
    #[test]
    fn 过期键被清理防止无界增长() {
        let rl = RateLimiter::new();
        let t0 = Instant::now();
        let window = Duration::from_secs(60);
        // 填入超过清理阈值数量的不同 IP（均在 t0 窗内）
        for i in 0..(EVICT_THRESHOLD + 10) {
            let _ = rl.check_key(
                RateKey::Ip(format!("10.0.{}.{}", i / 256, i % 256)),
                100,
                window,
                t0,
            );
        }
        {
            let g = rl.state.lock().unwrap();
            assert!(g.len() >= EVICT_THRESHOLD, "填充后表应较大");
        }
        // 推进超过一个窗口后再访问一个新键，触发过期清理
        let t1 = t0 + Duration::from_secs(120);
        let _ = rl.check_key(RateKey::Ip("8.8.8.8".into()), 100, window, t1);
        let g = rl.state.lock().unwrap();
        // 旧窗的键应已被清掉，仅剩新键量级（远小于填充量）
        assert!(
            g.len() < EVICT_THRESHOLD,
            "过期键应被清理，实际剩 {}",
            g.len()
        );
    }

    /// 并发计数一致：N 个线程各打 M 次同一键，被拒数应恰为 N*M - 阈值（无丢计 / 无重复）。
    #[test]
    fn 并发下计数一致() {
        use std::sync::Arc;
        let rl = Arc::new(RateLimiter::new());
        let now = Instant::now();
        let window = Duration::from_secs(60);
        let max = 100u64;
        let threads = 8;
        let per = 50u64; // 总请求 400，远超阈值 100
        let mut handles = Vec::new();
        for _ in 0..threads {
            let rl = Arc::clone(&rl);
            handles.push(std::thread::spawn(move || {
                let mut allowed = 0u64;
                for _ in 0..per {
                    if rl.check_key(RateKey::Ip("1.2.3.4".into()), max, window, now)
                        == Decision::Allow
                    {
                        allowed += 1;
                    }
                }
                allowed
            }));
        }
        let total_allowed: u64 = handles.into_iter().map(|h| h.join().unwrap()).sum();
        let total = threads as u64 * per;
        // 放行恰为阈值，被拒恰为其余；并发下无丢计 / 无重复
        assert_eq!(total_allowed, max, "放行数应恰为阈值");
        assert_eq!(rl.rejected_count(), total - max, "被拒数应为总数减阈值");
    }

    /// 仓库维度独立于 IP / 身份：同一仓库触顶不影响另一个仓库（防误杀）。
    #[test]
    fn 仓库维度独立触顶() {
        let rl = RateLimiter::new();
        let now = Instant::now();
        let window = Duration::from_secs(60);
        // 仓库 A 阈值 2：第 3 次触顶
        assert_eq!(
            rl.check_key(RateKey::Repo("maven-hosted".into()), 2, window, now),
            Decision::Allow
        );
        assert_eq!(
            rl.check_key(RateKey::Repo("maven-hosted".into()), 2, window, now),
            Decision::Allow
        );
        assert!(matches!(
            rl.check_key(RateKey::Repo("maven-hosted".into()), 2, window, now),
            Decision::Limited { .. }
        ));
        // 另一仓库不受影响（防误杀）
        assert_eq!(
            rl.check_key(RateKey::Repo("npm-hosted".into()), 2, window, now),
            Decision::Allow
        );
    }

    /// 并发上限触顶：占满名额后再申请被拒，归还（Drop）一个名额后又可申请（RAII 可靠归还）。
    #[test]
    fn 并发上限触顶后归还可再申请() {
        use std::sync::Arc;
        let rl = Arc::new(RateLimiter::new());
        let key = || RateKey::Ip("1.1.1.1".into());
        // 上限 2：连占 2 个成功
        let g1 = rl.acquire_concurrency(key(), 2);
        let g2 = rl.acquire_concurrency(key(), 2);
        assert!(g1.is_some() && g2.is_some(), "前两次占用应成功");
        // 第 3 次超上限被拒
        assert!(
            rl.acquire_concurrency(key(), 2).is_none(),
            "超并发上限应被拒"
        );
        // 归还一个名额（Drop g1）后又可申请
        drop(g1);
        assert!(
            rl.acquire_concurrency(key(), 2).is_some(),
            "归还后应可再次占用"
        );
        drop(g2);
    }

    /// 并发上限为 0 表示不限并发：任意数量占用均成功（默认不误杀正常并发）。
    #[test]
    fn 并发上限为零不限制() {
        use std::sync::Arc;
        let rl = Arc::new(RateLimiter::new());
        let mut guards = Vec::new();
        for _ in 0..100 {
            let g = rl.acquire_concurrency(RateKey::Ip("1.1.1.1".into()), 0);
            assert!(g.is_some(), "不限并发时应一律成功");
            guards.push(g);
        }
    }

    /// 不同维度键并发互不串味：IP 维度占满不影响仓库维度（各维度独立计数）。
    #[test]
    fn 并发各维度独立() {
        use std::sync::Arc;
        let rl = Arc::new(RateLimiter::new());
        let g_ip1 = rl.acquire_concurrency(RateKey::Ip("1.1.1.1".into()), 1);
        assert!(g_ip1.is_some());
        // 同一 IP 再占被拒
        assert!(rl
            .acquire_concurrency(RateKey::Ip("1.1.1.1".into()), 1)
            .is_none());
        // 仓库维度不受 IP 维度影响
        let g_repo = rl.acquire_concurrency(RateKey::Repo("r1".into()), 1);
        assert!(g_repo.is_some(), "仓库维度并发应独立于 IP 维度");
        drop(g_ip1);
        drop(g_repo);
    }

    /// 并发计数在多线程下一致：N 线程并发占用同一键，成功数恰为上限，且全部归还后计数清零。
    #[test]
    fn 并发占用计数一致且全部归还() {
        use std::sync::atomic::{AtomicU64, Ordering};
        use std::sync::{Arc, Barrier};
        let rl = Arc::new(RateLimiter::new());
        let max = 50u64;
        let threads = 16usize;
        let per = 20u64; // 总申请 320，远超上限 50
        let acquired = Arc::new(AtomicU64::new(0));
        let barrier = Arc::new(Barrier::new(threads));
        let mut handles = Vec::new();
        for _ in 0..threads {
            let rl = Arc::clone(&rl);
            let acquired = Arc::clone(&acquired);
            let barrier = Arc::clone(&barrier);
            handles.push(std::thread::spawn(move || {
                barrier.wait();
                // 各线程持有自己拿到的 guard 直到本轮结束，制造真实并发占用
                let mut held = Vec::new();
                for _ in 0..per {
                    if let Some(g) = rl.acquire_concurrency(RateKey::Ip("9.9.9.9".into()), max) {
                        acquired.fetch_add(1, Ordering::Relaxed);
                        held.push(g);
                    }
                }
                // 线程结束时 held 内 guard 全部 Drop，归还名额
                held
            }));
        }
        // 等所有线程跑完（其 guard 随返回值在此处统一 Drop）
        let all: Vec<_> = handles.into_iter().map(|h| h.join().unwrap()).collect();
        // 并发占用成功数恰为上限（无超发、无丢计）
        assert_eq!(
            acquired.load(Ordering::Relaxed),
            max,
            "成功占用数应恰为并发上限"
        );
        drop(all);
        // 全部归还后该键应可再次占满（计数已清零、键已移除）
        let mut again = Vec::new();
        for _ in 0..max {
            again.push(rl.acquire_concurrency(RateKey::Ip("9.9.9.9".into()), max));
        }
        assert!(
            again.iter().all(|g| g.is_some()),
            "全部归还后应能再次占满，证明计数已可靠清零"
        );
    }
}

// ============ 中间件端到端测试（经真实路由）============
#[cfg(test)]
mod middleware_tests {
    use super::super::tests::测试用状态;
    use super::super::{build_router, AppState};
    use axum::body::Body;
    use axum::extract::ConnectInfo;
    use axum::http::{Request, StatusCode};
    use std::net::SocketAddr;
    use std::sync::Arc;
    use tower::ServiceExt;

    /// 以给定速率限制配置定制测试状态。
    async fn 限流状态(
        enabled: bool,
        ip_max: u64,
        identity_max: u64,
    ) -> (AppState, tempfile::TempDir) {
        let (mut state, dir) = 测试用状态().await;
        let mut cfg = (*state.config).clone();
        cfg.protection.rate_limit.enabled = enabled;
        cfg.protection.rate_limit.window_secs = 60;
        cfg.protection.rate_limit.ip_max_requests = ip_max;
        cfg.protection.rate_limit.identity_max_requests = identity_max;
        // 经防护热替换槽装载定制配置（中间件从本槽读取，与生产同一路径）
        state.protection.replace(cfg.protection.clone());
        state.config = Arc::new(cfg);
        (state, dir)
    }

    /// 仅设仓库维度每窗阈值（其余维度放宽到极大，隔离观察仓库维度行为）的测试状态。
    async fn 仓库限流状态(repo_max: u64) -> (AppState, tempfile::TempDir) {
        let (mut state, dir) = 测试用状态().await;
        let mut cfg = (*state.config).clone();
        cfg.protection.rate_limit.enabled = true;
        cfg.protection.rate_limit.window_secs = 60;
        cfg.protection.rate_limit.ip_max_requests = u64::MAX;
        cfg.protection.rate_limit.identity_max_requests = u64::MAX;
        cfg.protection.rate_limit.repo_max_requests = repo_max;
        // 经防护热替换槽装载定制配置（中间件从本槽读取，与生产同一路径）
        state.protection.replace(cfg.protection.clone());
        state.config = Arc::new(cfg);
        (state, dir)
    }

    /// 用指定来源 IP 对任意路径发一发 GET 请求，返回状态码（经真实路由与限流中间件）。
    async fn 打路径(app: axum::Router, ip: &str, uri: &str) -> StatusCode {
        let addr: SocketAddr = format!("{ip}:50000").parse().unwrap();
        let mut req = Request::builder().uri(uri).body(Body::empty()).unwrap();
        req.extensions_mut().insert(ConnectInfo(addr));
        app.oneshot(req).await.unwrap().status()
    }

    /// 用指定来源 IP（连接级）发一发 /health 请求。
    async fn 打健康(app: axum::Router, ip: &str, xff: Option<&str>) -> StatusCode {
        let addr: SocketAddr = format!("{ip}:50000").parse().unwrap();
        let mut builder = Request::builder().uri("/health");
        if let Some(v) = xff {
            builder = builder.header("X-Forwarded-For", v);
        }
        let mut req = builder.body(Body::empty()).unwrap();
        // 注入连接信息（生产由 into_make_service_with_connect_info 注入）
        req.extensions_mut().insert(ConnectInfo(addr));
        app.oneshot(req).await.unwrap().status()
    }

    #[tokio::test]
    async fn 未启用时正常高频放行不误杀() {
        // 关闭限流：即便连发远超阈值也全部放行（防误杀基线）
        let (state, _dir) = 限流状态(false, 2, 2).await;
        let app = build_router(state);
        for _ in 0..20 {
            let st = 打健康(app.clone(), "1.1.1.1", None).await;
            assert_eq!(st, StatusCode::OK);
        }
    }

    #[tokio::test]
    async fn 同ip超阈值返回429() {
        // 阈值 3：前 3 次 200，第 4 次起 429
        let (state, _dir) = 限流状态(true, 3, 1000).await;
        let app = build_router(state);
        for _ in 0..3 {
            assert_eq!(打健康(app.clone(), "1.1.1.1", None).await, StatusCode::OK);
        }
        assert_eq!(
            打健康(app.clone(), "1.1.1.1", None).await,
            StatusCode::TOO_MANY_REQUESTS
        );
    }

    #[tokio::test]
    async fn 不同ip互不影响放行() {
        // 阈值 2：一个 IP 打满后另一个正常 IP 仍放行（防误杀）
        let (state, _dir) = 限流状态(true, 2, 1000).await;
        let app = build_router(state);
        for _ in 0..3 {
            let _ = 打健康(app.clone(), "9.9.9.9", None).await;
        }
        assert_eq!(
            打健康(app.clone(), "9.9.9.9", None).await,
            StatusCode::TOO_MANY_REQUESTS
        );
        // 正常 IP 不受影响
        assert_eq!(打健康(app.clone(), "1.1.1.1", None).await, StatusCode::OK);
    }

    #[tokio::test]
    async fn xff伪造来源不绕过限流() {
        // 同一连接 IP，即便每次伪造不同 XFF，仍按真实连接 IP 计数、照常触顶 429（防绕过）
        let (state, _dir) = 限流状态(true, 3, 1000).await;
        let app = build_router(state);
        for i in 0..3 {
            let xff = format!("203.0.113.{i}");
            assert_eq!(
                打健康(app.clone(), "1.1.1.1", Some(&xff)).await,
                StatusCode::OK
            );
        }
        // 第 4 次换个 XFF 仍被限（XFF 不被采信）
        assert_eq!(
            打健康(app.clone(), "1.1.1.1", Some("198.51.100.7")).await,
            StatusCode::TOO_MANY_REQUESTS
        );
    }

    #[tokio::test]
    async fn 限流响应带retry_after头() {
        let (state, _dir) = 限流状态(true, 1, 1000).await;
        let app = build_router(state);
        assert_eq!(打健康(app.clone(), "1.1.1.1", None).await, StatusCode::OK);
        let addr: SocketAddr = "1.1.1.1:50000".parse().unwrap();
        let mut req = Request::builder()
            .uri("/health")
            .body(Body::empty())
            .unwrap();
        req.extensions_mut().insert(ConnectInfo(addr));
        let resp = app.oneshot(req).await.unwrap();
        assert_eq!(resp.status(), StatusCode::TOO_MANY_REQUESTS);
        assert!(
            resp.headers().contains_key(axum::http::header::RETRY_AFTER),
            "429 响应应带 Retry-After 头"
        );
    }

    /// 仓库维度超阈值返回 429：同一仓库路径连发超过 repo_max 后被限（FR-51 仓库维度）。
    #[tokio::test]
    async fn 同仓库超阈值返回429() {
        // 仓库阈值 3：前 3 次进入业务（仓库不存在返回 404），第 4 次被限流中间件拒 429
        let (state, _dir) = 仓库限流状态(3).await;
        let app = build_router(state);
        for _ in 0..3 {
            let st = 打路径(app.clone(), "1.1.1.1", "/maven-hosted/a/b.jar").await;
            assert_ne!(st, StatusCode::TOO_MANY_REQUESTS, "未触顶前不应被限流");
        }
        assert_eq!(
            打路径(app.clone(), "1.1.1.1", "/maven-hosted/a/b.jar").await,
            StatusCode::TOO_MANY_REQUESTS
        );
    }

    /// 不同仓库互不影响：一个仓库打满后另一个仓库仍可访问（防误杀）。
    #[tokio::test]
    async fn 不同仓库互不影响() {
        let (state, _dir) = 仓库限流状态(2).await;
        let app = build_router(state);
        for _ in 0..3 {
            let _ = 打路径(app.clone(), "1.1.1.1", "/repo-a/x").await;
        }
        // repo-a 已被限
        assert_eq!(
            打路径(app.clone(), "1.1.1.1", "/repo-a/x").await,
            StatusCode::TOO_MANY_REQUESTS
        );
        // repo-b 不受影响（防误杀）
        assert_ne!(
            打路径(app.clone(), "1.1.1.1", "/repo-b/x").await,
            StatusCode::TOO_MANY_REQUESTS
        );
    }

    /// 保留前缀不计入仓库维度：连发 /health 远超仓库阈值也不会被仓库维度限流（避免误算系统路由）。
    #[tokio::test]
    async fn 保留前缀不计入仓库维度() {
        let (state, _dir) = 仓库限流状态(2).await;
        let app = build_router(state);
        // /health 属保留前缀，不归属任何仓库；连发 10 次均不应因仓库维度被限
        for _ in 0..10 {
            assert_eq!(
                打路径(app.clone(), "1.1.1.1", "/health").await,
                StatusCode::OK
            );
        }
    }

    /// 并发上限端到端：单 IP 并发上限为 1 时，两个重叠在途请求中至少一个被拒 429，
    /// 且占用名额在请求结束后归还、后续请求恢复放行（验证 RAII 归还 + 不泄漏）。
    #[tokio::test]
    async fn 并发上限端到端触顶与归还() {
        use axum::routing::get;
        use axum::Router;
        use std::time::Duration;
        use tokio::sync::Notify;

        let (mut state, _dir) = 测试用状态().await;
        let mut cfg = (*state.config).clone();
        cfg.protection.rate_limit.enabled = true;
        // 速率维度放到极大，单独观察并发维度
        cfg.protection.rate_limit.ip_max_requests = u64::MAX;
        cfg.protection.rate_limit.identity_max_requests = u64::MAX;
        cfg.protection.rate_limit.ip_max_concurrent = 1;
        // 经防护热替换槽装载定制配置（中间件从本槽读取，与生产同一路径）
        state.protection.replace(cfg.protection.clone());
        state.config = Arc::new(cfg);

        // 用一个可被唤醒前一直挂起的 handler 制造确定性的「在途重叠」
        let gate = Arc::new(Notify::new());
        let gate_for_handler = Arc::clone(&gate);
        let app = Router::new()
            .route(
                "/slow",
                get(move || {
                    let gate = Arc::clone(&gate_for_handler);
                    async move {
                        gate.notified().await;
                        StatusCode::OK
                    }
                }),
            )
            .with_state(state.clone())
            .layer(axum::middleware::from_fn_with_state(
                state.clone(),
                super::rate_limit_layer,
            ));

        let mk = |app: Router| async move {
            let addr: SocketAddr = "1.1.1.1:50000".parse().unwrap();
            let mut req = Request::builder().uri("/slow").body(Body::empty()).unwrap();
            req.extensions_mut().insert(ConnectInfo(addr));
            app.oneshot(req).await.unwrap().status()
        };

        // 先发起第一个请求（会挂在 handler 内，持有并发名额）
        let first = tokio::spawn(mk(app.clone()));
        // 给第一个请求时间进入 handler（已占用名额）
        tokio::time::sleep(Duration::from_millis(50)).await;
        // 第二个请求重叠在途：并发上限为 1，应被并发维度拒 429
        let second_status = mk(app.clone()).await;
        assert_eq!(
            second_status,
            StatusCode::TOO_MANY_REQUESTS,
            "重叠在途请求应被并发上限拒"
        );
        // 放行第一个请求，使其完成并归还名额
        gate.notify_waiters();
        let first_status = first.await.unwrap();
        assert_eq!(first_status, StatusCode::OK, "第一个请求应正常完成");
        // 名额已归还：再发一个请求，它能拿到名额、进入 handler 并被唤醒完成（不泄漏在途计数）
        let third = tokio::spawn(mk(app.clone()));
        tokio::time::sleep(Duration::from_millis(50)).await;
        gate.notify_waiters();
        let third_status = third.await.unwrap();
        assert_eq!(third_status, StatusCode::OK, "并发名额归还后请求应恢复放行");
    }
}
