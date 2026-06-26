//! 防护配置 API（FR-79）的 HTTP 集成测试：经 axum 路由端到端验证
//! - 鉴权矩阵：匿名 / 普通用户 / Admin 对 GET、PATCH 的放行与拒绝。
//! - PATCH 后**即时生效**：改限流阈值 / IP 黑名单 / WAF 规则后，下一个请求即按新值判定（无须重启）。
//! - PATCH 非法体返回 400 且不改变现有生效配置。
//! - 并发改配置一致性：多写多读并发不 panic、最终一致。
//!
//! 派生态（IP 名单匹配器、WAF 规则集）经热替换槽按新配置重建，验证不是仅改了配置字段而派生态滞后。

use std::net::SocketAddr;
use std::sync::Arc;
use std::time::Duration;

use axum::body::Body;
use axum::extract::ConnectInfo;
use axum::http::{Request, StatusCode};
use axum::Router;
use http_body_util::BodyExt;
use serde_json::{json, Value};
use tower::ServiceExt;

use jianartifact::api::{build_router, AppState, ProtectionState};
use jianartifact::auth::{self, JwtSigner, LoginGuard};
use jianartifact::config::{Config, ProtectionConfig};
use jianartifact::format::{ArtifactService, DockerRegistry, FormatRegistry};
use jianartifact::meta::{MetaStore, Role};
use jianartifact::proxy::HttpUpstream;
use jianartifact::storage::{BlobBackend, LocalFsStore};

/// 测试夹具：持有可重复构建路由的状态与临时目录。
struct Fixture {
    state: AppState,
    _dir: tempfile::TempDir,
}

impl Fixture {
    async fn new() -> Self {
        let dir = tempfile::tempdir().unwrap();
        let meta = MetaStore::open(&dir.path().join("test.db")).await.unwrap();
        let store = BlobBackend::Fs(LocalFsStore::new(dir.path().join("blobs")).await.unwrap());
        let jwt = JwtSigner::from_secret(b"cfg-secret-32-bytes-xxxxxxxxxxxxx", 3600);
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
        let (alerts, alert_rx) = jianartifact::api::alert_channel();
        jianartifact::api::spawn_alert_writer(meta.clone(), alert_rx);
        let alert_engine = Arc::new(jianartifact::api::AlertEngine::new(alerts.clone()));

        let cfg = Config::default();
        let protection = Arc::new(ProtectionState::new(cfg.protection.clone()));

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
                b"cfg-cc-secret-32-bytes-xxxxxxxxx",
            )),
            alerts,
            alert_engine,
            restart: std::sync::Arc::new(jianartifact::update::RestartHandle::default()),
            settings: std::sync::Arc::new(
                jianartifact::config::EditableSettings::new(
                    jianartifact::config::NetworkProxyConfig::default(),
                    std::time::Duration::from_secs(60),
                    &jianartifact::config::UpdateConfig::default(),
                )
                .unwrap(),
            ),
            host_system: std::sync::Arc::new(tokio::sync::Mutex::new(sysinfo::System::new())),
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

    fn issue(&self, uid: &str, username: &str, role: Role) -> String {
        self.state.jwt.issue(uid, username, role).unwrap()
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

/// 构造请求（可带 JSON body、Bearer、连接 IP）。
fn req(
    method: &str,
    uri: &str,
    auth: Option<&str>,
    ip: Option<&str>,
    body: Option<Value>,
) -> Request<Body> {
    let mut builder = Request::builder().method(method).uri(uri);
    if let Some(a) = auth {
        builder = builder.header("authorization", a);
    }
    let body = match body {
        Some(v) => {
            builder = builder.header("content-type", "application/json");
            Body::from(serde_json::to_vec(&v).unwrap())
        }
        None => Body::empty(),
    };
    let mut r = builder.body(body).unwrap();
    if let Some(ip) = ip {
        let addr: SocketAddr = format!("{ip}:50000").parse().unwrap();
        r.extensions_mut().insert(ConnectInfo(addr));
    }
    r
}

/// 以指定 IP 打一发 /health，返回状态码（经全部防护中间件链）。
async fn 打健康(router: Router, ip: &str) -> StatusCode {
    let r = req("GET", "/health", None, Some(ip), None);
    router.oneshot(r).await.unwrap().status()
}

/// 取一个带 Admin Bearer 的会话令牌。
async fn admin_token(fx: &Fixture) -> String {
    let uid = fx.seed_user("admin", "pw", Role::Admin).await;
    fx.issue(&uid, "admin", Role::Admin)
}

/// 取一个带普通 User Bearer 的会话令牌。
async fn user_token(fx: &Fixture) -> String {
    let uid = fx.seed_user("alice", "pw", Role::User).await;
    fx.issue(&uid, "alice", Role::User)
}

// ---------- 鉴权矩阵 ----------

#[tokio::test]
async fn 匿名读写防护配置被拒_401() {
    let fx = Fixture::new().await;
    let (st, _) = send(
        fx.router(),
        req("GET", "/api/v1/protection/config", None, None, None),
    )
    .await;
    assert_eq!(st, StatusCode::UNAUTHORIZED);
    let (st, _) = send(
        fx.router(),
        req(
            "PATCH",
            "/api/v1/protection/config",
            None,
            None,
            Some(json!({})),
        ),
    )
    .await;
    assert_eq!(st, StatusCode::UNAUTHORIZED);
}

#[tokio::test]
async fn 普通用户读写防护配置被拒_403() {
    let fx = Fixture::new().await;
    let token = user_token(&fx).await;
    let bearer = format!("Bearer {token}");
    let (st, _) = send(
        fx.router(),
        req(
            "GET",
            "/api/v1/protection/config",
            Some(&bearer),
            None,
            None,
        ),
    )
    .await;
    assert_eq!(st, StatusCode::FORBIDDEN);
    let (st, _) = send(
        fx.router(),
        req(
            "PATCH",
            "/api/v1/protection/config",
            Some(&bearer),
            None,
            Some(json!({})),
        ),
    )
    .await;
    assert_eq!(st, StatusCode::FORBIDDEN);
}

#[tokio::test]
async fn admin读取返回当前防护配置() {
    let fx = Fixture::new().await;
    let token = admin_token(&fx).await;
    let bearer = format!("Bearer {token}");
    let (st, body) = send(
        fx.router(),
        req(
            "GET",
            "/api/v1/protection/config",
            Some(&bearer),
            None,
            None,
        ),
    )
    .await;
    assert_eq!(st, StatusCode::OK);
    // 默认配置：各防护默认关闭，结构含七个维度
    assert_eq!(body["rate_limit"]["enabled"], json!(false));
    assert!(body.get("ip_list").is_some());
    assert!(body.get("waf").is_some());
    assert!(body.get("cc_challenge").is_some());
}

// ---------- PATCH 非法体 → 400 且不改变现有配置 ----------

#[tokio::test]
async fn patch非法配置返回400且不改变现有配置() {
    let fx = Fixture::new().await;
    let token = admin_token(&fx).await;
    let bearer = format!("Bearer {token}");

    // 取当前配置，把限流窗口置 0（非法），其余照搬
    let (_, mut current) = send(
        fx.router(),
        req(
            "GET",
            "/api/v1/protection/config",
            Some(&bearer),
            None,
            None,
        ),
    )
    .await;
    current["rate_limit"]["window_secs"] = json!(0);

    let (st, body) = send(
        fx.router(),
        req(
            "PATCH",
            "/api/v1/protection/config",
            Some(&bearer),
            None,
            Some(current),
        ),
    )
    .await;
    assert_eq!(st, StatusCode::BAD_REQUEST);
    assert_eq!(body["error"]["code"], json!("bad_request"));

    // 现有配置未被改变：重新 GET 仍是默认窗口 60
    let (_, after) = send(
        fx.router(),
        req(
            "GET",
            "/api/v1/protection/config",
            Some(&bearer),
            None,
            None,
        ),
    )
    .await;
    assert_eq!(after["rate_limit"]["window_secs"], json!(60));
}

// ---------- PATCH 即时生效：限流维度 ----------

#[tokio::test]
async fn patch限流配置后下一请求即按新阈值429() {
    let fx = Fixture::new().await;
    let token = admin_token(&fx).await;
    let bearer = format!("Bearer {token}");

    // 初始限流关闭：连发 /health 全 200（基线）
    for _ in 0..5 {
        assert_eq!(打健康(fx.router(), "1.1.1.1").await, StatusCode::OK);
    }

    // 热改：启用限流、IP 每窗上限 2
    let mut cfg = ProtectionConfig::default();
    cfg.rate_limit.enabled = true;
    cfg.rate_limit.window_secs = 60;
    cfg.rate_limit.ip_max_requests = 2;
    let patch_body = serde_json::to_value(&cfg).unwrap();
    let (st, _) = send(
        fx.router(),
        req(
            "PATCH",
            "/api/v1/protection/config",
            Some(&bearer),
            None,
            Some(patch_body),
        ),
    )
    .await;
    assert_eq!(st, StatusCode::OK);

    // 即时生效：同一 IP 前 2 次 200，第 3 次起 429（无须重启）
    assert_eq!(打健康(fx.router(), "2.2.2.2").await, StatusCode::OK);
    assert_eq!(打健康(fx.router(), "2.2.2.2").await, StatusCode::OK);
    assert_eq!(
        打健康(fx.router(), "2.2.2.2").await,
        StatusCode::TOO_MANY_REQUESTS,
        "PATCH 限流配置后应即时生效"
    );
}

// ---------- PATCH 即时生效：IP 黑名单（派生匹配器须重建）----------

#[tokio::test]
async fn patch新增ip黑名单后该ip即被拒403() {
    let fx = Fixture::new().await;
    let token = admin_token(&fx).await;
    let bearer = format!("Bearer {token}");

    // 基线：该 IP 未被名单拦截
    assert_eq!(打健康(fx.router(), "203.0.113.7").await, StatusCode::OK);

    // 热改：把该 IP 加入黑名单
    let mut cfg = ProtectionConfig::default();
    cfg.ip_list.deny = vec!["203.0.113.7".to_string()];
    let patch_body = serde_json::to_value(&cfg).unwrap();
    let (st, _) = send(
        fx.router(),
        req(
            "PATCH",
            "/api/v1/protection/config",
            Some(&bearer),
            None,
            Some(patch_body),
        ),
    )
    .await;
    assert_eq!(st, StatusCode::OK);

    // 即时生效：该 IP 立即被黑名单拒（派生匹配器已按新配置重建），其他 IP 不受影响
    assert_eq!(
        打健康(fx.router(), "203.0.113.7").await,
        StatusCode::FORBIDDEN,
        "PATCH 黑名单后命中 IP 应即时被拒"
    );
    assert_eq!(打健康(fx.router(), "10.0.0.1").await, StatusCode::OK);
}

// ---------- PATCH 即时生效：WAF 规则（派生规则集须重新编译）----------

#[tokio::test]
async fn patch新增waf规则后命中路径即被拦403() {
    let fx = Fixture::new().await;
    let token = admin_token(&fx).await;
    let bearer = format!("Bearer {token}");

    // 基线：WAF 关闭，访问任意路径不被 WAF 拦（/forbidden-zone/x 走业务，非 403-by-WAF）
    let (st, _) = send(
        fx.router(),
        req("GET", "/forbidden-zone/x", None, Some("9.9.9.9"), None),
    )
    .await;
    assert_ne!(st, StatusCode::FORBIDDEN);

    // 热改：启用 WAF，拦截以 /forbidden-zone/ 开头的路径
    let cfg = json!({
        "rate_limit": ProtectionConfig::default().rate_limit,
        "ip_list": ProtectionConfig::default().ip_list,
        "ban": ProtectionConfig::default().ban,
        "slowloris": ProtectionConfig::default().slowloris,
        "cc_challenge": ProtectionConfig::default().cc_challenge,
        "alerts": ProtectionConfig::default().alerts,
        "waf": {
            "enabled": true,
            "rules": [
                {"field": "path", "pattern": "/forbidden-zone/*", "match_type": "wildcard", "action": "block"}
            ]
        }
    });
    let (st, _) = send(
        fx.router(),
        req(
            "PATCH",
            "/api/v1/protection/config",
            Some(&bearer),
            None,
            Some(cfg),
        ),
    )
    .await;
    assert_eq!(st, StatusCode::OK);

    // 即时生效：命中路径被 WAF 拦 403（规则集已重新编译）
    let (st, body) = send(
        fx.router(),
        req("GET", "/forbidden-zone/x", None, Some("9.9.9.9"), None),
    )
    .await;
    assert_eq!(
        st,
        StatusCode::FORBIDDEN,
        "PATCH WAF 规则后命中路径应即时被拦"
    );
    assert_eq!(body["error"]["code"], json!("forbidden"));
    // 未命中路径仍放行（防误杀）
    let (st, _) = send(
        fx.router(),
        req("GET", "/other-zone/x", None, Some("9.9.9.9"), None),
    )
    .await;
    assert_ne!(st, StatusCode::FORBIDDEN);
}

// ---------- 并发改配置一致性 ----------

#[tokio::test]
async fn 并发patch与读取不panic且最终一致() {
    let fx = Fixture::new().await;
    let token = admin_token(&fx).await;
    let bearer = Arc::new(format!("Bearer {token}"));
    let state = fx.state.clone();

    // 并发：多个任务交替 PATCH「启用限流」与「关闭限流」，同时多个任务 GET 读取
    let mut handles = Vec::new();
    for i in 0..8 {
        let router = build_router(state.clone());
        let bearer = Arc::clone(&bearer);
        handles.push(tokio::spawn(async move {
            let mut cfg = ProtectionConfig::default();
            cfg.rate_limit.enabled = i % 2 == 0;
            let body = serde_json::to_value(&cfg).unwrap();
            let r = req(
                "PATCH",
                "/api/v1/protection/config",
                Some(&bearer),
                None,
                Some(body),
            );
            router.oneshot(r).await.unwrap().status()
        }));
    }
    for i in 0..8 {
        let router = build_router(state.clone());
        let bearer = Arc::clone(&bearer);
        handles.push(tokio::spawn(async move {
            let _ = i;
            let r = req(
                "GET",
                "/api/v1/protection/config",
                Some(&bearer),
                None,
                None,
            );
            router.oneshot(r).await.unwrap().status()
        }));
    }
    for h in handles {
        let st = h.await.unwrap();
        assert!(st == StatusCode::OK, "并发读写应均成功，实际 {st}");
    }

    // 最终再 PATCH 一个确定状态，断言 GET 反映之
    let router = build_router(state.clone());
    let mut cfg = ProtectionConfig::default();
    cfg.rate_limit.enabled = true;
    cfg.rate_limit.ip_max_requests = 7;
    let body = serde_json::to_value(&cfg).unwrap();
    let (st, _) = send(
        router,
        req(
            "PATCH",
            "/api/v1/protection/config",
            Some(&bearer),
            None,
            Some(body),
        ),
    )
    .await;
    assert_eq!(st, StatusCode::OK);

    let router = build_router(state);
    let (st, after) = send(
        router,
        req(
            "GET",
            "/api/v1/protection/config",
            Some(&bearer),
            None,
            None,
        ),
    )
    .await;
    assert_eq!(st, StatusCode::OK);
    assert_eq!(after["rate_limit"]["enabled"], json!(true));
    assert_eq!(after["rate_limit"]["ip_max_requests"], json!(7));
}
