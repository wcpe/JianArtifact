//! OIDC 授权码流的 HTTP 集成测试（FR-34 / ADR-0016）。
//!
//! 本机通常无真实 IdP，故起一个进程内 mock IdP（axum）提供 discovery / JWKS / token 端点，
//! 用固定测试 RSA 密钥签发 ID Token，端到端覆盖：未配置 OIDC 时端点 404、登录重定向、
//! 回调 state 校验、换码与 ID Token 校验、JIT 关 / 开两路径、默认角色 User 不自动 Admin。
//!
//! 真机互通（对接 Keycloak / Azure AD / Okta 等真实 IdP）待真机验（需 OIDC IdP）。

use std::net::SocketAddr;
use std::sync::Arc;

use axum::body::Body;
use axum::extract::State as AxumState;
use axum::http::{Request, StatusCode};
use axum::response::IntoResponse;
use axum::routing::{get, post};
use axum::{Json, Router};
use jsonwebtoken::{encode, Algorithm, EncodingKey, Header};
use serde_json::{json, Value};
use tower::ServiceExt;

use jianartifact::api::{build_router, AppState};
use jianartifact::auth::{JwtSigner, LoginGuard, OidcProvider, OidcSettings};
use jianartifact::config::Config;
use jianartifact::format::{ArtifactService, DockerRegistry, FormatRegistry};
use jianartifact::meta::{MetaStore, Role};
use jianartifact::proxy::HttpUpstream;
use jianartifact::storage::{BlobBackend, LocalFsStore};

/// 固定测试 RSA 私钥（PKCS#1 DER），与下方 JWKS 的 n/e 配对（纯测试向量、非生产密钥）。
const TEST_RSA_PKCS1_DER: &[u8] = include_bytes!("../src/auth/testdata/oidc_test_rsa_pkcs1.der");
/// 上述公钥的 base64url 模数 n。
const TEST_JWK_N: &str = "tJOCUVcE473ahzlFWSRD7_vj6ZMHRKKCXyUWlVQqJx5O2yYu1ffXVBnU4nYzTTCzVqN0-3h97SFDk56lDXL5qSQK9yDQdC1ppflEdCs7T-73rpQHoAUvnGgQFEmTFGhJDbV7LXMg-3NoZoWodQ5WJwUCTevjG3xhgfSO69Z_0vEEVtWuBRpt4HaeBTOEGhhTbheVEOkIZ7ZYPEpkAL8vJrpz-waiOMWi-3gj5RK1tzy6vSJ_9GF8JqxQpr_Fx1nd95Lu8WmO6ZRz5-SGJkW3t1m9H9dVg9oPU3MMDTsxGZ7c45Yc3EO9oqhmHnD1QK1DxHx9XQJKwAZphprzCfuBew";
/// 上述公钥的 base64url 指数 e（65537）。
const TEST_JWK_E: &str = "AQAB";
/// 客户端 ID（mock IdP 与 provider 共用）。
const CLIENT_ID: &str = "test-client";

/// mock IdP 的共享状态：自身 issuer 基址与「上次签发用 nonce」。
#[derive(Clone)]
struct IdpState {
    issuer: String,
    /// 控制 token 端点签发的 ID Token 用哪个 nonce / sub（按测试需要预置）。
    nonce: Arc<std::sync::Mutex<String>>,
    sub: Arc<std::sync::Mutex<String>>,
}

/// 起一个进程内 mock IdP，返回 (issuer 基址, 关停句柄)。
async fn start_mock_idp() -> (String, tokio::task::JoinHandle<()>) {
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    let issuer = format!("http://{addr}");
    let state = IdpState {
        issuer: issuer.clone(),
        nonce: Arc::new(std::sync::Mutex::new(String::new())),
        sub: Arc::new(std::sync::Mutex::new("ext-sub-int".to_string())),
    };
    let app = Router::new()
        .route("/.well-known/openid-configuration", get(discovery))
        .route("/jwks", get(jwks))
        .route("/authorize", get(authorize))
        .route("/token", post(token))
        .with_state(state);
    let handle = tokio::spawn(async move {
        axum::serve(listener, app).await.unwrap();
    });
    // 等待端口就绪
    wait_ready(&addr).await;
    (issuer, handle)
}

/// 轮询直到端口可连接。
async fn wait_ready(addr: &SocketAddr) {
    for _ in 0..50 {
        if tokio::net::TcpStream::connect(addr).await.is_ok() {
            return;
        }
        tokio::time::sleep(std::time::Duration::from_millis(20)).await;
    }
    panic!("mock IdP 未就绪");
}

/// discovery 端点。
async fn discovery(AxumState(s): AxumState<IdpState>) -> Json<Value> {
    Json(json!({
        "issuer": s.issuer,
        "authorization_endpoint": format!("{}/authorize", s.issuer),
        "token_endpoint": format!("{}/token", s.issuer),
        "jwks_uri": format!("{}/jwks", s.issuer),
    }))
}

/// JWKS 端点：单 RSA 键（kid = test-kid）。
async fn jwks() -> Json<Value> {
    Json(json!({
        "keys": [{
            "kty": "RSA",
            "kid": "test-kid",
            "use": "sig",
            "alg": "RS256",
            "n": TEST_JWK_N,
            "e": TEST_JWK_E,
        }]
    }))
}

/// 授权端点：本测试不真正走浏览器，provider 只用 discovery 取该 URL；直接回 200。
async fn authorize() -> impl IntoResponse {
    StatusCode::OK
}

/// token 端点：用固定测试密钥签发 ID Token（nonce / sub 取 mock 状态预置值）。
async fn token(AxumState(s): AxumState<IdpState>) -> Json<Value> {
    let nonce = s.nonce.lock().unwrap().clone();
    let sub = s.sub.lock().unwrap().clone();
    let now = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .as_secs() as i64;
    let claims = json!({
        "iss": s.issuer,
        "aud": CLIENT_ID,
        "sub": sub,
        "exp": now + 3600,
        "iat": now,
        "nonce": nonce,
        "preferred_username": "int-user",
    });
    let mut header = Header::new(Algorithm::RS256);
    header.kid = Some("test-kid".to_string());
    let encoding = EncodingKey::from_rsa_der(TEST_RSA_PKCS1_DER);
    let id_token = encode(&header, &claims, &encoding).unwrap();
    Json(json!({
        "access_token": "irrelevant",
        "token_type": "Bearer",
        "id_token": id_token,
    }))
}

/// 构造被测应用状态（可选注入 OIDC provider）。
async fn build_state(oidc: Option<OidcProvider>) -> (AppState, tempfile::TempDir) {
    let dir = tempfile::tempdir().unwrap();
    let meta = MetaStore::open(&dir.path().join("test.db")).await.unwrap();
    let store = BlobBackend::Fs(LocalFsStore::new(dir.path().join("blobs")).await.unwrap());
    let jwt = JwtSigner::from_secret(b"oidc-integration-secret-32-bytesx", 3600);
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
    let state = AppState {
        config: Arc::new(Config::default()),
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
        oidc: oidc.map(Arc::new),
        oidc_flows: Arc::new(jianartifact::api::OidcFlowStore::new()),
        ldap: None,
        // FR-53：测试默认名单空、封禁登记表空（异常检测默认关闭）
        ip_matcher: Arc::new(jianartifact::api::IpMatcher::from_config(
            &jianartifact::config::IpListConfig::default(),
        )),
        ban_registry: Arc::new(jianartifact::api::BanRegistry::new()),
    };
    (state, dir)
}

/// 据 issuer 与 auto_provision 造一个指向 mock IdP 的 OIDC provider。
fn make_provider(issuer: &str, auto_provision: bool) -> OidcProvider {
    let http = reqwest::Client::builder()
        .timeout(std::time::Duration::from_secs(10))
        .build()
        .unwrap();
    OidcProvider::new(
        OidcSettings {
            issuer: issuer.to_string(),
            client_id: CLIENT_ID.to_string(),
            client_secret: "test-secret".to_string(),
            redirect_uri: "http://app/api/v1/auth/oidc/callback".to_string(),
            auto_provision,
        },
        http,
    )
}

/// 从 302 响应取 Location 头。
fn location(resp: &axum::http::Response<Body>) -> String {
    resp.headers()
        .get(axum::http::header::LOCATION)
        .and_then(|v| v.to_str().ok())
        .unwrap_or("")
        .to_string()
}

/// 解析 query 串里某参数值。
fn query_param(url: &str, key: &str) -> Option<String> {
    let q = url.split_once('?')?.1;
    for pair in q.split('&') {
        if let Some((k, v)) = pair.split_once('=') {
            if k == key {
                return Some(v.to_string());
            }
        }
    }
    None
}

#[tokio::test]
async fn 未配置_oidc_时登录与回调端点均_404() {
    let (state, _dir) = build_state(None).await;
    let app = build_router(state);
    let resp = app
        .clone()
        .oneshot(
            Request::builder()
                .uri("/api/v1/auth/oidc/login")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::NOT_FOUND);

    let resp = app
        .oneshot(
            Request::builder()
                .uri("/api/v1/auth/oidc/callback?code=x&state=y")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::NOT_FOUND);
}

#[tokio::test]
async fn 登录端点重定向到_idp_且带_pkce_与_state() {
    let (issuer, _idp) = start_mock_idp().await;
    let (state, _dir) = build_state(Some(make_provider(&issuer, false))).await;
    let app = build_router(state);
    let resp = app
        .oneshot(
            Request::builder()
                .uri("/api/v1/auth/oidc/login")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    // 302 重定向到 IdP 授权端点，URL 带 code_challenge / state / nonce
    assert_eq!(resp.status(), StatusCode::SEE_OTHER);
    let loc = location(&resp);
    assert!(loc.starts_with(&format!("{issuer}/authorize?")));
    assert!(query_param(&loc, "code_challenge").is_some());
    assert!(query_param(&loc, "code_challenge_method").as_deref() == Some("S256"));
    assert!(query_param(&loc, "state").is_some());
    assert!(query_param(&loc, "nonce").is_some());
}

#[tokio::test]
async fn 回调_state_不存在时拒绝_401_防_csrf() {
    let (issuer, _idp) = start_mock_idp().await;
    let (state, _dir) = build_state(Some(make_provider(&issuer, false))).await;
    let app = build_router(state);
    // 伪造一个从未下发的 state
    let resp = app
        .oneshot(
            Request::builder()
                .uri("/api/v1/auth/oidc/callback?code=abc&state=forged")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::UNAUTHORIZED);
}

/// 端到端：登录拿 state → 预置 mock IdP 的 nonce → 回调换码校验 → 据 JIT 行为断言。
async fn 走完整流程(
    idp_issuer: &str,
    idp_nonce: Arc<std::sync::Mutex<String>>,
    state: AppState,
) -> axum::http::Response<Body> {
    let app = build_router(state);
    // 1. 登录，取回下发的 state 与 nonce（从重定向 URL 解析）
    let resp = app
        .clone()
        .oneshot(
            Request::builder()
                .uri("/api/v1/auth/oidc/login")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    let loc = location(&resp);
    assert!(loc.starts_with(&format!("{idp_issuer}/authorize?")));
    let csrf_state = query_param(&loc, "state").unwrap();
    let nonce = query_param(&loc, "nonce").unwrap();
    // 2. 让 mock IdP 的 token 端点签发与本次 nonce 一致的 ID Token
    *idp_nonce.lock().unwrap() = nonce;
    // 3. 回调（code 任意，mock IdP 不校验 code）
    app.oneshot(
        Request::builder()
            .uri(format!(
                "/api/v1/auth/oidc/callback?code=anycode&state={csrf_state}"
            ))
            .body(Body::empty())
            .unwrap(),
    )
    .await
    .unwrap()
}

#[tokio::test]
async fn jit_关闭且无预置用户时回调拒绝_401() {
    let (issuer, nonce_handle) = start_mock_idp_with_handle().await;
    let provider = make_provider(&issuer, false);
    let (state, _dir) = build_state(Some(provider)).await;
    let resp = 走完整流程(&issuer, nonce_handle, state.clone()).await;
    // JIT 关闭、无预置绑定用户：回调被拒
    assert_eq!(resp.status(), StatusCode::UNAUTHORIZED);
    // 不得静默建用户
    assert_eq!(state.meta.count_users().await.unwrap(), 0);
}

#[tokio::test]
async fn jit_开启时回调即时开通默认角色_user_并签发会话() {
    let (issuer, nonce_handle) = start_mock_idp_with_handle().await;
    let provider = make_provider(&issuer, true);
    let (state, _dir) = build_state(Some(provider)).await;
    let resp = 走完整流程(&issuer, nonce_handle, state.clone()).await;
    // 成功登录：回跳前端并在 fragment 携带会话令牌
    assert_eq!(resp.status(), StatusCode::SEE_OTHER);
    let loc = location(&resp);
    assert!(loc.starts_with("/login#access_token="));
    assert!(loc.contains("token_type=Bearer"));
    // JIT 开通：建了一个本地用户，默认角色 User，绝不 Admin
    assert_eq!(state.meta.count_users().await.unwrap(), 1);
    let user = state
        .meta
        .get_user_by_username("int-user")
        .await
        .unwrap()
        .unwrap();
    assert_eq!(user.role, "user");
    assert_eq!(user.external_idp.as_deref(), Some("oidc"));
}

#[tokio::test]
async fn jit_关闭但预置绑定用户时回调成功复用() {
    let (issuer, nonce_handle) = start_mock_idp_with_handle().await;
    let provider = make_provider(&issuer, false);
    let (state, _dir) = build_state(Some(provider)).await;
    // 管理员预置外部用户并绑定 mock IdP 默认 sub（ext-sub-int）
    state
        .meta
        .create_external_user("preset", Role::User, "oidc", "ext-sub-int")
        .await
        .unwrap();
    let resp = 走完整流程(&issuer, nonce_handle, state.clone()).await;
    assert_eq!(resp.status(), StatusCode::SEE_OTHER);
    // 复用既有用户，不新增
    assert_eq!(state.meta.count_users().await.unwrap(), 1);
}

/// 起 mock IdP 并额外返回其 nonce 句柄（供端到端流程预置 token 的 nonce）。
async fn start_mock_idp_with_handle() -> (String, Arc<std::sync::Mutex<String>>) {
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    let issuer = format!("http://{addr}");
    let nonce = Arc::new(std::sync::Mutex::new(String::new()));
    let state = IdpState {
        issuer: issuer.clone(),
        nonce: nonce.clone(),
        sub: Arc::new(std::sync::Mutex::new("ext-sub-int".to_string())),
    };
    let app = Router::new()
        .route("/.well-known/openid-configuration", get(discovery))
        .route("/jwks", get(jwks))
        .route("/authorize", get(authorize))
        .route("/token", post(token))
        .with_state(state);
    tokio::spawn(async move {
        axum::serve(listener, app).await.unwrap();
    });
    wait_ready(&addr).await;
    (issuer, nonce)
}
