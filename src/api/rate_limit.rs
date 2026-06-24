//! 基础速率限制（FR-33，ADR-0008 的「仅基础限流」部分）。
//!
//! 进程内存按 **IP 维度** 与 **Token / 用户维度** 用固定时间窗计数请求，单窗内超阈值即返回
//! `429 Too Many Requests`（带 `Retry-After`）。本批只做基础 IP / Token 限流，**不做** FR-51
//! 多维并发 / 连接上限、FR-52~56 的慢速 / 封禁 / CC / WAF / 告警。
//!
//! 设计要点（对齐 testing-and-quality §2.7）：
//! - **热路径低开销**：每请求只取一次 `Mutex`、做整型自增与窗口比较，临界区内无 IO、无格式化、
//!   无锁外重活；窗口表的过期清理顺带在加锁期间按概率触发，避免单独后台任务与长时间持锁。
//! - **防误杀**：默认阈值保守且按窗口宽放，正常包管理器批量拉取不应触顶；默认关闭，须运维显式开启。
//! - **防绕过**：来源 IP 取连接级 `ConnectInfo`（与登录防护一致），**不采信 XFF 头**，伪造来源不绕过；
//!   已认证请求额外按 Token / 用户维度计数，轮换 IP 也受身份维度阈值约束。
//! - **配置即时生效**：阈值 / 窗口从 `AppState.config` 读取，配置热替换后下个请求即按新值判定。

use std::collections::HashMap;
use std::net::SocketAddr;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Mutex;
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

use super::AppState;

/// 限流维度键：把来源归一为 IP 维度或身份维度，避免不同维度计数互相串味。
#[derive(Debug, Clone, PartialEq, Eq, Hash)]
enum RateKey {
    /// 按连接来源 IP 计数（匿名与未认证流量的主维度）。
    Ip(String),
    /// 按已认证主体（用户 id，含其所有 Token / 会话）计数；轮换 IP 也受此约束。
    Identity(String),
}

/// 单个键在当前固定窗内的计数状态。
#[derive(Debug, Clone, Copy)]
struct WindowState {
    /// 当前窗内已计数的请求数。
    count: u64,
    /// 当前窗的起始时刻；距今超过窗口时长即翻入新窗、计数清零。
    window_start: Instant,
}

/// 进程内速率限制器：线程安全地维护各维度键的固定窗计数。
///
/// 克隆经 `Arc`（随 `AppState`）共享；本结构自身不实现 `Clone`，由调用方用 `Arc` 包裹。
pub struct RateLimiter {
    /// 各维度键的窗口计数表。
    state: Mutex<HashMap<RateKey, WindowState>>,
    /// 触顶被拒请求累计数（供观测，不影响判定）。
    rejected: AtomicU64,
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

/// 限流中间件：置于身份解析之后、业务 handler 之前。
///
/// 先按连接 IP 维度计数；已认证请求再按身份维度计数。任一维度触顶即在进入业务前返回 429。
/// 配置未启用（`enabled=false`）时直接放行，零计数开销。
pub async fn rate_limit_layer(
    State(state): State<AppState>,
    request: Request,
    next: Next,
) -> Response {
    let cfg = &state.config.protection.rate_limit;
    if !cfg.enabled {
        return next.run(request).await;
    }

    let now = Instant::now();
    let window = Duration::from_secs(cfg.window_secs.max(1));

    // IP 维度：来源取连接级 ConnectInfo，绝不采信 XFF 头（防伪造来源绕过）
    if let Some(ip) = client_ip(&request) {
        if let Decision::Limited { retry_after_secs } =
            state
                .rate_limiter
                .check_key(RateKey::Ip(ip), cfg.ip_max_requests, window, now)
        {
            return too_many_requests(retry_after_secs);
        }
    }

    // 身份维度：已认证请求按用户 id 计数（含其所有 Token / 会话），轮换 IP 也受此约束
    if let Some(user_id) = authenticated_user_id(&request) {
        if let Decision::Limited { retry_after_secs } = state.rate_limiter.check_key(
            RateKey::Identity(user_id),
            cfg.identity_max_requests,
            window,
            now,
        ) {
            return too_many_requests(retry_after_secs);
        }
    }

    next.run(request).await
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
        state.config = Arc::new(cfg);
        (state, dir)
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
}
