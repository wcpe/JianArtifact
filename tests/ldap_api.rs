//! LDAP 认证集成的 HTTP 集成测试（FR-35 / ADR-0016）。
//!
//! 本机通常无真实 LDAP 目录，故这里覆盖**不依赖目录**即可断言的关键行为：
//! - 配置了 LDAP 但目录不可达时，`/auth/login` 与 Basic Auth 仍优雅回退（401 / 匿名），不崩溃；
//! - 本地口令登录与 Basic Auth 在 LDAP 配置存在时不受干扰（与既有四通道并存不串味）；
//! - 明文 `ldap://` 默认被安全前置拒绝（不外发 bind）。
//!
//! 真机互通（对接 AD / OpenLDAP，bind 成功 → JIT 映射 → 签发会话）待真机验（需 LDAP 目录），
//! bind 编排 / 搜索过滤 / 主体提取 / 脱敏的纯逻辑由 `src/auth/ldap.rs` 单测穷举覆盖。

use std::sync::Arc;

use axum::body::Body;
use axum::http::{Request, StatusCode};
use axum::Router;
use base64::{engine::general_purpose::STANDARD, Engine};
use tower::ServiceExt;

use jianartifact::api::{build_router, AppState};
use jianartifact::auth::{self, JwtSigner, LdapProvider, LdapSettings, LoginGuard};
use jianartifact::config::{Config, LdapConfig};
use jianartifact::format::{ArtifactService, DockerRegistry, FormatRegistry};
use jianartifact::meta::{MetaStore, Role};
use jianartifact::proxy::HttpUpstream;
use jianartifact::storage::{BlobBackend, LocalFsStore};

/// 造一份指向「不可达地址」的 LDAP 配置（端口 1 通常无服务，连接即失败）。
///
/// `url` 用 ldaps://（满足安全前置），但目标不可达，用于验证「目录不可达回退」。
fn unreachable_ldap_config(auto_provision: bool) -> LdapConfig {
    LdapConfig {
        url: "ldaps://127.0.0.1:1".to_string(),
        bind_dn: "cn=svc,dc=ex,dc=org".to_string(),
        bind_password: "svc-pw".to_string(),
        user_search_base: "ou=people,dc=ex,dc=org".to_string(),
        user_filter: "(uid={username})".to_string(),
        username_attr: "uid".to_string(),
        starttls: false,
        allow_insecure: false,
        conn_timeout_secs: 2,
        auto_provision,
    }
}

/// 据 LDAP 配置（可选）构造被测应用状态。
async fn build_state(ldap_cfg: Option<LdapConfig>) -> (AppState, tempfile::TempDir) {
    let dir = tempfile::tempdir().unwrap();
    let meta = MetaStore::open(&dir.path().join("test.db")).await.unwrap();
    let store = BlobBackend::Fs(LocalFsStore::new(dir.path().join("blobs")).await.unwrap());
    let jwt = JwtSigner::from_secret(b"ldap-integration-secret-32-bytesx", 3600);
    let upstream = HttpUpstream::new(std::time::Duration::from_secs(60)).unwrap();
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

    // 把 LDAP 配置同时落到 config（供 JIT 开关读取）与 provider（供 bind 编排）
    let mut cfg = Config::default();
    let ldap = ldap_cfg.map(|c| {
        let provider = LdapProvider::new(LdapSettings {
            url: c.url.clone(),
            bind_dn: c.bind_dn.clone(),
            bind_password: c.bind_password.clone(),
            user_search_base: c.user_search_base.clone(),
            user_filter: c.user_filter.clone(),
            username_attr: c.username_attr.clone(),
            starttls: c.starttls,
            allow_insecure: c.allow_insecure,
            conn_timeout_secs: c.conn_timeout_secs,
        });
        cfg.auth.ldap = Some(c);
        Arc::new(provider)
    });

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
        ldap,
        protection: Arc::new(jianartifact::api::ProtectionState::new(
            jianartifact::config::ProtectionConfig::default(),
        )),
        ban_registry: Arc::new(jianartifact::api::BanRegistry::new()),
        // FR-54：测试默认 CC 挑战关闭；挑战器用固定密钥
        cc_challenger: Arc::new(jianartifact::api::CcChallenger::new(
            b"test-secret-32-bytes-xxxxxxxxxxxx",
        )),
        // FR-56：防护告警默认关闭，引擎与投递端就绪（关闭时 record 直接返回）
        alerts: jianartifact::api::alert_channel().0,
        alert_engine: Arc::new(jianartifact::api::AlertEngine::new(
            jianartifact::api::alert_channel().0,
        )),
    };
    (state, dir)
}

/// 在库中预置本地用户。
async fn seed_user(state: &AppState, username: &str, password: &str, role: Role) {
    let hash = auth::hash_password(password).unwrap();
    state.meta.create_user(username, &hash, role).await.unwrap();
}

/// 发起 JSON 登录请求。
async fn login(app: Router, username: &str, password: &str) -> axum::http::Response<Body> {
    let body = serde_json::json!({ "username": username, "password": password }).to_string();
    app.oneshot(
        Request::builder()
            .method("POST")
            .uri("/api/v1/auth/login")
            .header("content-type", "application/json")
            .body(Body::from(body))
            .unwrap(),
    )
    .await
    .unwrap()
}

/// 带 Basic Auth 头访问受保护端点 /api/v1/me。
async fn me_with_basic(app: Router, username: &str, secret: &str) -> axum::http::Response<Body> {
    let header = format!("Basic {}", STANDARD.encode(format!("{username}:{secret}")));
    app.oneshot(
        Request::builder()
            .uri("/api/v1/me")
            .header("Authorization", header)
            .body(Body::empty())
            .unwrap(),
    )
    .await
    .unwrap()
}

#[tokio::test]
async fn 本地用户登录在配置_ldap_时仍成功() {
    // LDAP 配置存在（但目录不可达）不应干扰本地口令登录：本地命中即返回，不触达 LDAP。
    let (state, _dir) = build_state(Some(unreachable_ldap_config(false))).await;
    seed_user(&state, "alice", "s3cret", Role::Admin).await;
    let app = build_router(state);
    let resp = login(app, "alice", "s3cret").await;
    assert_eq!(resp.status(), StatusCode::OK);
}

#[tokio::test]
async fn ldap_目录不可达时登录优雅回退_401() {
    // 本地无此用户、LDAP 目录不可达：登录失败统一 401，不崩溃、不泄露细节、不建用户。
    let (state, _dir) = build_state(Some(unreachable_ldap_config(true))).await;
    let app = build_router(state.clone());
    let resp = login(app, "ldap-only-user", "any-pw").await;
    assert_eq!(resp.status(), StatusCode::UNAUTHORIZED);
    // JIT 开启也不得在目录不可达（未通过 bind）时静默建用户
    assert_eq!(state.meta.count_users().await.unwrap(), 0);
}

#[tokio::test]
async fn 明文_ldap_默认登录被拒且不建用户() {
    // url 为明文 ldap:// 且未启用 StartTLS / allow_insecure：安全前置拒绝，登录 401。
    let mut cfg = unreachable_ldap_config(true);
    cfg.url = "ldap://127.0.0.1:1".to_string();
    let (state, _dir) = build_state(Some(cfg)).await;
    let app = build_router(state.clone());
    let resp = login(app, "anyone", "any-pw").await;
    assert_eq!(resp.status(), StatusCode::UNAUTHORIZED);
    assert_eq!(state.meta.count_users().await.unwrap(), 0);
}

#[tokio::test]
async fn basic_auth_本地用户在配置_ldap_时仍解析身份() {
    // Basic Auth 本地口令命中即授予身份，不受 LDAP 配置干扰（四通道并存不串味）。
    let (state, _dir) = build_state(Some(unreachable_ldap_config(false))).await;
    seed_user(&state, "bob", "pw123", Role::User).await;
    let app = build_router(state);
    let resp = me_with_basic(app, "bob", "pw123").await;
    assert_eq!(resp.status(), StatusCode::OK);
}

#[tokio::test]
async fn basic_auth_ldap_目录不可达时回退匿名_401() {
    // 本地无此用户、非 Token、LDAP 目录不可达：Basic 解析回退匿名，受保护端点 401。
    let (state, _dir) = build_state(Some(unreachable_ldap_config(true))).await;
    let app = build_router(state);
    let resp = me_with_basic(app, "ghost", "any-pw").await;
    assert_eq!(resp.status(), StatusCode::UNAUTHORIZED);
}

#[tokio::test]
async fn 未配置_ldap_时纯本地行为不变() {
    // 不配置 LDAP：登录与 Basic Auth 走纯本地路径，行为与基线一致。
    let (state, _dir) = build_state(None).await;
    seed_user(&state, "carol", "pw", Role::User).await;
    let app = build_router(state.clone());
    // 本地登录成功
    assert_eq!(
        login(app.clone(), "carol", "pw").await.status(),
        StatusCode::OK
    );
    // 错误口令 401
    assert_eq!(
        login(app, "carol", "wrong").await.status(),
        StatusCode::UNAUTHORIZED
    );
}
