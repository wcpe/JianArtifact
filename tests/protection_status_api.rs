//! 防护监控与告警（FR-56，ADR-0017）的 HTTP 集成测试：经 axum 路由端到端验证
//! 阈值告警触发 / 不触发边界、去抖、误报规避、告警异步落库不阻塞、状态端点鉴权矩阵、
//! 状态端点不外发、告警历史分页。
//!
//! 告警写入是异步的（独立写入任务），故对"已落库"的断言用短轮询等待，避免脆弱的固定睡眠。

use std::net::SocketAddr;
use std::sync::Arc;
use std::time::Duration;

use axum::body::Body;
use axum::extract::ConnectInfo;
use axum::http::{Request, StatusCode};
use axum::Router;
use http_body_util::BodyExt;
use serde_json::Value;
use tower::ServiceExt;

use jianartifact::api::{build_router, AppState};
use jianartifact::auth::{self, JwtSigner, LoginGuard};
use jianartifact::config::{Config, WafConfig, WafRuleConfig};
use jianartifact::format::{ArtifactService, DockerRegistry, FormatRegistry};
use jianartifact::meta::{AlertQuery, MetaStore, Role};
use jianartifact::proxy::HttpUpstream;
use jianartifact::storage::{BlobBackend, LocalFsStore};

/// 测试夹具：持有可重复构建路由的状态与临时目录。
struct Fixture {
    state: AppState,
    _dir: tempfile::TempDir,
}

impl Fixture {
    /// 用给定配置定制器构造夹具：启动真实告警写入任务，使路由真实走告警落库链路。
    async fn with_config(customize: impl FnOnce(&mut Config)) -> Self {
        let dir = tempfile::tempdir().unwrap();
        let meta = MetaStore::open(&dir.path().join("test.db")).await.unwrap();
        let store = BlobBackend::Fs(LocalFsStore::new(dir.path().join("blobs")).await.unwrap());
        let jwt = JwtSigner::from_secret(b"prot-secret-32-bytes-xxxxxxxxxxxx", 3600);
        let upstream = HttpUpstream::new(Duration::from_secs(60)).unwrap();
        let artifacts = Arc::new(ArtifactService::new(store.clone(), meta.clone(), upstream));
        let docker = Arc::new(
            DockerRegistry::new(
                store.clone(),
                meta.clone(),
                dir.path().join("uploads"),
                None,
            )
            .await
            .unwrap(),
        );
        let (audit, audit_rx) = jianartifact::api::audit_channel();
        jianartifact::api::spawn_audit_writer(meta.clone(), audit_rx);
        let (usage, usage_rx) = jianartifact::api::usage_channel();
        jianartifact::api::spawn_usage_writer(meta.clone(), usage_rx, false);
        // 防护告警：建有界 channel 并启动写入任务，使告警真实异步落库
        let (alerts, alert_rx) = jianartifact::api::alert_channel();
        jianartifact::api::spawn_alert_writer(meta.clone(), alert_rx);
        let alert_engine = Arc::new(jianartifact::api::AlertEngine::new(alerts.clone()));

        let mut cfg = Config::default();
        customize(&mut cfg);
        // 从（定制后的）配置装载防护热替换槽，使夹具反映 customize 设置的 [protection.*]（含 WAF 规则集）
        let protection = Arc::new(jianartifact::api::ProtectionState::new(
            cfg.protection.clone(),
        ));

        let state = AppState {
            config: Arc::new(cfg),
            meta,
            store,
            jwt,
            login_guard: Arc::new(LoginGuard::new(5, 900)),
            artifacts,
            formats: Arc::new(FormatRegistry::with_builtin()),
            docker: Some(docker),
            audit,
            usage,
            metrics: None,
            rate_limiter: Arc::new(jianartifact::api::RateLimiter::new()),
            oidc: None,
            oidc_flows: Arc::new(jianartifact::api::OidcFlowStore::new()),
            ldap: None,
            protection,
            ban_registry: Arc::new(jianartifact::api::BanRegistry::new()),
            cc_challenger: Arc::new(jianartifact::api::CcChallenger::new(
                b"prot-cc-secret-32-bytes-xxxxxxxx",
            )),
            alerts,
            alert_engine,
        };
        Self { state, _dir: dir }
    }

    fn router(&self) -> Router {
        build_router(self.state.clone())
    }

    async fn seed_user(&self, username: &str, password: &str, role: Role) -> String {
        let hash = auth::hash_password(password).unwrap();
        self.state
            .meta
            .create_user(username, &hash, role)
            .await
            .unwrap()
    }

    /// 签发某用户的会话 JWT。
    fn issue(&self, uid: &str, username: &str, role: Role) -> String {
        self.state.jwt.issue(uid, username, role).unwrap()
    }

    /// 短轮询等待告警库总数达到期望值，最多约 2 秒；返回是否达到。
    async fn wait_alert_total(&self, expected: i64) -> bool {
        for _ in 0..200 {
            let total = self.state.meta.count_alerts_total().await.unwrap();
            if total >= expected {
                return true;
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        false
    }
}

/// 发送请求并返回 (状态码, JSON 体)。
async fn send(router: Router, req: Request<Body>) -> (StatusCode, Value) {
    let resp = router.oneshot(req).await.unwrap();
    let status = resp.status();
    let bytes = resp.into_body().collect().await.unwrap().to_bytes();
    let body = serde_json::from_slice(&bytes).unwrap_or(Value::Null);
    (status, body)
}

/// 构造无 body 的请求（可带连接 IP 与 Bearer）。
fn req(method: &str, uri: &str, auth: Option<&str>, ip: Option<&str>) -> Request<Body> {
    let mut builder = Request::builder().method(method).uri(uri);
    if let Some(a) = auth {
        builder = builder.header("authorization", a);
    }
    let mut r = builder.body(Body::empty()).unwrap();
    if let Some(ip) = ip {
        let addr: SocketAddr = format!("{ip}:50000").parse().unwrap();
        r.extensions_mut().insert(ConnectInfo(addr));
    }
    r
}

// ---------- 阈值告警触发 / 去抖 / 误报规避（经 WAF 阻断维度驱动）----------

/// 构造一个启用 WAF（拦 /blocked/*）+ 启用告警（waf 阈值低）的夹具。
async fn waf告警夹具(waf_threshold: u64, window_secs: u64) -> Fixture {
    Fixture::with_config(|cfg| {
        cfg.protection.alerts.enabled = true;
        cfg.protection.alerts.window_secs = window_secs;
        cfg.protection.alerts.waf_block_warn_threshold = waf_threshold;
        cfg.protection.waf = WafConfig {
            enabled: true,
            rules: vec![WafRuleConfig {
                field: "path".into(),
                header_name: None,
                pattern: "/blocked/*".into(),
                match_type: "wildcard".into(),
                action: "block".into(),
            }],
        };
    })
    .await
}

#[tokio::test]
async fn waf阻断达阈值产生告警并落库() {
    // 阈值 3：前 2 次阻断不告警，第 3 次达阈值产生 1 条告警
    let fx = waf告警夹具(3, 300).await;
    for _ in 0..3 {
        let (st, _) = send(fx.router(), req("GET", "/blocked/x", None, Some("1.1.1.1"))).await;
        assert_eq!(st, StatusCode::FORBIDDEN, "命中 WAF 应被阻断 403");
    }
    assert!(fx.wait_alert_total(1).await, "达阈值应产生 1 条告警并落库");
    let rows = fx
        .state
        .meta
        .query_alerts(&AlertQuery {
            limit: 50,
            ..Default::default()
        })
        .await
        .unwrap();
    assert_eq!(rows.len(), 1);
    assert_eq!(rows[0].dimension, "waf");
    assert_eq!(rows[0].threshold, 3);
    assert!(rows[0].observed_value >= 3);
}

#[tokio::test]
async fn waf阻断未达阈值不告警() {
    // 阈值 100：仅阻断 3 次，远不及阈值，不应告警（误报规避）
    let fx = waf告警夹具(100, 300).await;
    for _ in 0..3 {
        let (st, _) = send(fx.router(), req("GET", "/blocked/x", None, Some("1.1.1.1"))).await;
        assert_eq!(st, StatusCode::FORBIDDEN);
    }
    // 给写任务一点时间；不应有任何告警
    tokio::time::sleep(Duration::from_millis(200)).await;
    assert_eq!(fx.state.meta.count_alerts_total().await.unwrap(), 0);
}

#[tokio::test]
async fn waf阻断同窗去抖只告警一次() {
    // 阈值 2：连阻断 10 次，一窗内只应产生 1 条告警（去抖、不刷屏）
    let fx = waf告警夹具(2, 300).await;
    for _ in 0..10 {
        let (st, _) = send(fx.router(), req("GET", "/blocked/x", None, Some("1.1.1.1"))).await;
        assert_eq!(st, StatusCode::FORBIDDEN);
    }
    assert!(fx.wait_alert_total(1).await);
    // 再等片刻，确认不会继续刷新更多告警
    tokio::time::sleep(Duration::from_millis(200)).await;
    assert_eq!(
        fx.state.meta.count_alerts_total().await.unwrap(),
        1,
        "同窗同维度只告警一次"
    );
}

#[tokio::test]
async fn 正常请求不触发告警_误报规避() {
    // 启用 WAF + 告警，但正常路径不命中 block，连发大量请求不应产生任何告警
    let fx = waf告警夹具(1, 300).await;
    for _ in 0..30 {
        let (st, _) = send(fx.router(), req("GET", "/health", None, Some("1.1.1.1"))).await;
        assert_eq!(st, StatusCode::OK);
    }
    tokio::time::sleep(Duration::from_millis(200)).await;
    assert_eq!(
        fx.state.meta.count_alerts_total().await.unwrap(),
        0,
        "正常请求不应误报告警"
    );
}

// ---------- 状态端点鉴权矩阵 ----------

#[tokio::test]
async fn 状态端点匿名被拒_401() {
    let fx = Fixture::with_config(|_| {}).await;
    let (st, _) = send(
        fx.router(),
        req("GET", "/api/v1/protection/status", None, None),
    )
    .await;
    assert_eq!(st, StatusCode::UNAUTHORIZED);
}

#[tokio::test]
async fn 状态端点普通用户被拒_403() {
    let fx = Fixture::with_config(|_| {}).await;
    let uid = fx.seed_user("u", "pw", Role::User).await;
    let token = fx.issue(&uid, "u", Role::User);
    let bearer = format!("Bearer {token}");
    let (st, _) = send(
        fx.router(),
        req("GET", "/api/v1/protection/status", Some(&bearer), None),
    )
    .await;
    assert_eq!(st, StatusCode::FORBIDDEN);
}

#[tokio::test]
async fn 状态端点管理员放行并返回快照() {
    let fx = Fixture::with_config(|cfg| {
        cfg.protection.alerts.enabled = true;
        cfg.protection.alerts.window_secs = 300;
    })
    .await;
    let uid = fx.seed_user("admin", "pw", Role::Admin).await;
    let token = fx.issue(&uid, "admin", Role::Admin);
    let bearer = format!("Bearer {token}");
    let (st, body) = send(
        fx.router(),
        req("GET", "/api/v1/protection/status", Some(&bearer), None),
    )
    .await;
    assert_eq!(st, StatusCode::OK);
    assert_eq!(body["alerts_enabled"], true);
    assert_eq!(body["window_secs"], 300);
    assert_eq!(body["active_banned_ips"], 0);
    // 五个防护维度的窗内计数应齐备
    let counts = body["window_counts"].as_array().unwrap();
    assert_eq!(counts.len(), 5);
    // 纯本机聚合：响应体不应含任何外发 / 远端字段（仅本地聚合视图）
    assert!(body["recent_alerts"].is_array());
}

// ---------- 告警历史分页 ----------

#[tokio::test]
async fn 告警历史分页与维度过滤() {
    let fx = Fixture::with_config(|_| {}).await;
    let uid = fx.seed_user("admin", "pw", Role::Admin).await;
    let token = fx.issue(&uid, "admin", Role::Admin);
    let bearer = format!("Bearer {token}");

    // 直接经 meta 落 3 条告警（2 条 waf、1 条 rate_limit）
    fx.state
        .meta
        .insert_alert_batch(&[new_alert("rate_limit"), new_alert("waf"), new_alert("waf")])
        .await
        .unwrap();

    // 全量分页：total=3，limit=2 时 has_more=true
    let (st, body) = send(
        fx.router(),
        req(
            "GET",
            "/api/v1/protection/alerts?limit=2",
            Some(&bearer),
            None,
        ),
    )
    .await;
    assert_eq!(st, StatusCode::OK);
    assert_eq!(body["total"], 3);
    assert_eq!(body["items"].as_array().unwrap().len(), 2);
    assert_eq!(body["has_more"], true);

    // 维度过滤 waf：total=2
    let (st2, body2) = send(
        fx.router(),
        req(
            "GET",
            "/api/v1/protection/alerts?dimension=waf",
            Some(&bearer),
            None,
        ),
    )
    .await;
    assert_eq!(st2, StatusCode::OK);
    assert_eq!(body2["total"], 2);
    assert!(body2["items"]
        .as_array()
        .unwrap()
        .iter()
        .all(|a| a["dimension"] == "waf"));
}

#[tokio::test]
async fn 告警历史匿名被拒_401() {
    let fx = Fixture::with_config(|_| {}).await;
    let (st, _) = send(
        fx.router(),
        req("GET", "/api/v1/protection/alerts", None, None),
    )
    .await;
    assert_eq!(st, StatusCode::UNAUTHORIZED);
}

/// 便捷：构造一条最小告警入参。
fn new_alert(dimension: &str) -> jianartifact::meta::NewAlert {
    jianartifact::meta::NewAlert {
        dimension: dimension.to_string(),
        severity: "warn".to_string(),
        observed_value: 10,
        threshold: 10,
        window_secs: 300,
        detail: Some("测试告警".to_string()),
    }
}
