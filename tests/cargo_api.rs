//! Cargo 稀疏索引格式的 HTTP 集成测试（FR-26）。
//!
//! 覆盖：config.json 生成、发布 → 索引 → `.crate` 下载字节一致（hosted）、索引 cksum=sha256、
//! 重复发布同版本 409（版本不可变）、yank/unyank 翻转索引标记、写授权边界、private 无权 404，
//! 以及 proxy 回源索引（不缓存）+ `.crate` cache-miss→hit（真实 mock 上游走 HttpUpstream）。
//!
//! 鉴权与制品机理复用既有层，本文件只验 cargo 协议适配的正确性。

use std::sync::Arc;

use axum::body::Body;
use axum::http::{Request, StatusCode};
use axum::Router;
use http_body_util::BodyExt;
use serde_json::{json, Value};
use tower::ServiceExt;

use jianartifact::api::{build_router, AppState};
use jianartifact::auth::{self, JwtSigner, LoginGuard};
use jianartifact::config::Config;
use jianartifact::format::{ArtifactService, FormatRegistry};
use jianartifact::meta::{MetaStore, NewRepository, Permission, RepoType, Role, Visibility};
use jianartifact::proxy::HttpUpstream;
use jianartifact::storage::{BlobBackend, LocalFsStore};

/// 测试夹具：真实 SQLite 文件 + 临时 blob 目录 + 固定对外地址。
struct Fixture {
    state: AppState,
    _dir: tempfile::TempDir,
}

impl Fixture {
    async fn new() -> Self {
        let dir = tempfile::tempdir().unwrap();
        let meta = MetaStore::open(&dir.path().join("test.db")).await.unwrap();
        let store = BlobBackend::Fs(LocalFsStore::new(dir.path().join("blobs")).await.unwrap());
        let jwt = JwtSigner::from_secret(b"cargo-secret-32-bytes-xxxxxxxxxxx", 3600);
        let upstream = HttpUpstream::new(std::time::Duration::from_secs(60)).unwrap();
        let artifacts = Arc::new(ArtifactService::new(store.clone(), meta.clone(), upstream));
        let mut config = Config::default();
        // 固定对外地址，便于断言 config.json / 使用片段
        config.server.public_base_url = Some("http://localhost:8080".to_string());
        let (audit, audit_rx) = jianartifact::api::audit_channel();
        jianartifact::api::spawn_audit_writer(meta.clone(), audit_rx);
        // 使用分析采集：建有界 channel 并启动写入任务（关明细），使路由真实走采集链路
        let (usage, usage_rx) = jianartifact::api::usage_channel();
        jianartifact::api::spawn_usage_writer(meta.clone(), usage_rx, false);
        let state = AppState {
            config: Arc::new(config),
            meta,
            store,
            jwt,
            login_guard: Arc::new(LoginGuard::new(50, 900)),
            artifacts,
            formats: Arc::new(FormatRegistry::with_builtin()),
            docker: None,
            audit,
            usage,
            metrics: None,
            rate_limiter: Arc::new(jianartifact::api::RateLimiter::new()),
            oidc: None,
            oidc_flows: std::sync::Arc::new(jianartifact::api::OidcFlowStore::new()),
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

    /// 建一个 cargo hosted 仓库，返回 id。
    async fn seed_cargo_repo(&self, name: &str, visibility: Visibility) -> String {
        self.state
            .meta
            .create_repository(NewRepository {
                name,
                format: "cargo",
                r#type: RepoType::Hosted,
                visibility,
                upstream_url: None,
                upstream_auth_ref: None,
            })
            .await
            .unwrap()
    }

    /// 建一个 cargo proxy 仓库（指向给定上游基址），返回 id。
    async fn seed_cargo_proxy_repo(
        &self,
        name: &str,
        visibility: Visibility,
        upstream_url: &str,
    ) -> String {
        self.state
            .meta
            .create_repository(NewRepository {
                name,
                format: "cargo",
                r#type: RepoType::Proxy,
                visibility,
                upstream_url: Some(upstream_url),
                upstream_auth_ref: None,
            })
            .await
            .unwrap()
    }

    async fn seed_acl(&self, repo_id: &str, user_id: &str, permission: Permission) {
        self.state
            .meta
            .create_acl(repo_id, user_id, permission)
            .await
            .unwrap();
    }

    async fn login_token(&self, username: &str, password: &str) -> String {
        let (status, body) = send(
            self.router(),
            json_req(
                "POST",
                "/api/v1/auth/login",
                None,
                json!({ "username": username, "password": password }),
            ),
        )
        .await;
        assert_eq!(status, StatusCode::OK, "登录应成功: {body}");
        body["access_token"].as_str().unwrap().to_string()
    }
}

/// 发请求并返回 (状态码, JSON 体)。
async fn send(router: Router, req: Request<Body>) -> (StatusCode, Value) {
    let resp = router.oneshot(req).await.unwrap();
    let status = resp.status();
    let bytes = resp.into_body().collect().await.unwrap().to_bytes();
    let body = serde_json::from_slice(&bytes).unwrap_or(Value::Null);
    (status, body)
}

/// 发请求并返回 (状态码, 原始字节)。
async fn send_bytes(router: Router, req: Request<Body>) -> (StatusCode, Vec<u8>) {
    let resp = router.oneshot(req).await.unwrap();
    let status = resp.status();
    let bytes = resp
        .into_body()
        .collect()
        .await
        .unwrap()
        .to_bytes()
        .to_vec();
    (status, bytes)
}

fn json_req(method: &str, uri: &str, auth: Option<&str>, body: Value) -> Request<Body> {
    let mut builder = Request::builder()
        .method(method)
        .uri(uri)
        .header("content-type", "application/json");
    if let Some(a) = auth {
        builder = builder.header("authorization", a);
    }
    builder.body(Body::from(body.to_string())).unwrap()
}

fn empty_req(method: &str, uri: &str, auth: Option<&str>) -> Request<Body> {
    let mut builder = Request::builder().method(method).uri(uri);
    if let Some(a) = auth {
        builder = builder.header("authorization", a);
    }
    builder.body(Body::empty()).unwrap()
}

fn bytes_req(method: &str, uri: &str, auth: Option<&str>, body: Vec<u8>) -> Request<Body> {
    let mut builder = Request::builder()
        .method(method)
        .uri(uri)
        .header("content-type", "application/octet-stream");
    if let Some(a) = auth {
        builder = builder.header("authorization", a);
    }
    builder.body(Body::from(body)).unwrap()
}

/// 构造一份 Cargo publish 二进制体：`[4 字节 LE json 长度][metadata JSON][4 字节 LE crate 长度][.crate]`。
fn publish_body(name: &str, vers: &str, crate_bytes: &[u8]) -> Vec<u8> {
    let metadata = json!({
        "name": name,
        "vers": vers,
        "deps": [],
        "features": {},
        "authors": [],
        "description": null,
        "documentation": null,
        "homepage": null,
        "readme": null,
        "readme_file": null,
        "keywords": [],
        "categories": [],
        "license": null,
        "license_file": null,
        "repository": null,
        "badges": {},
        "links": null
    });
    let json = serde_json::to_vec(&metadata).unwrap();
    let mut body = Vec::new();
    body.extend_from_slice(&(json.len() as u32).to_le_bytes());
    body.extend_from_slice(&json);
    body.extend_from_slice(&(crate_bytes.len() as u32).to_le_bytes());
    body.extend_from_slice(crate_bytes);
    body
}

/// 计算字节的 sha256 十六进制（与服务端索引 cksum 对账）。
fn sha256_hex(bytes: &[u8]) -> String {
    use sha2::{Digest, Sha256};
    let mut h = Sha256::new();
    h.update(bytes);
    format!("{:x}", h.finalize())
}

// ---------- config.json：指回本仓库 ----------

#[tokio::test]
async fn cargo_config_json_指回本仓库() {
    let fx = Fixture::new().await;
    fx.seed_cargo_repo("crates-hosted", Visibility::Public)
        .await;

    let (status, cfg) = send(
        fx.router(),
        empty_req("GET", "/crates-hosted/config.json", None),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(
        cfg["dl"],
        "http://localhost:8080/crates-hosted/api/v1/crates"
    );
    assert_eq!(cfg["api"], "http://localhost:8080/crates-hosted");
}

// ---------- hosted：发布 → 索引 → 下载端到端 ----------

#[tokio::test]
async fn cargo_发布后取索引与_crate_端到端() {
    let fx = Fixture::new().await;
    let writer = fx.seed_user("dev", "pw", Role::User).await;
    let rid = fx
        .seed_cargo_repo("crates-hosted", Visibility::Public)
        .await;
    fx.seed_acl(&rid, &writer, Permission::Write).await;
    let auth = format!("Bearer {}", fx.login_token("dev", "pw").await);

    let crate_bytes = b"this-is-a-fake-crate-payload";
    // PUT /{repo}/api/v1/crates/new 发布
    let (status, _) = send(
        fx.router(),
        bytes_req(
            "PUT",
            "/crates-hosted/api/v1/crates/new",
            Some(&auth),
            publish_body("serde", "1.0.0", crate_bytes),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::OK, "发布应 200");

    // GET 稀疏索引（公开仓库匿名可读）：serde → se/rd/serde
    let (status, idx) = send_bytes(
        fx.router(),
        empty_req("GET", "/crates-hosted/se/rd/serde", None),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    let text = String::from_utf8(idx).unwrap();
    let line: Value = serde_json::from_str(text.trim()).unwrap();
    assert_eq!(line["name"], "serde");
    assert_eq!(line["vers"], "1.0.0");
    // FR-69：cksum 用服务端算好的真实 sha256
    assert_eq!(line["cksum"], sha256_hex(crate_bytes));
    assert_eq!(line["yanked"], false);

    // GET .crate：字节一致
    let (status, bytes) = send_bytes(
        fx.router(),
        empty_req(
            "GET",
            "/crates-hosted/api/v1/crates/serde/1.0.0/download",
            None,
        ),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(bytes, crate_bytes);
}

#[tokio::test]
async fn cargo_发布两个版本索引合并() {
    let fx = Fixture::new().await;
    let writer = fx.seed_user("dev", "pw", Role::User).await;
    let rid = fx
        .seed_cargo_repo("crates-hosted", Visibility::Public)
        .await;
    fx.seed_acl(&rid, &writer, Permission::Write).await;
    let auth = format!("Bearer {}", fx.login_token("dev", "pw").await);

    for ver in ["1.0.0", "1.1.0"] {
        let (status, _) = send(
            fx.router(),
            bytes_req(
                "PUT",
                "/crates-hosted/api/v1/crates/new",
                Some(&auth),
                publish_body("mycrate", ver, ver.as_bytes()),
            ),
        )
        .await;
        assert_eq!(status, StatusCode::OK, "版本 {ver} 发布应 200");
    }

    // mycrate → my/cr/mycrate；两行索引
    let (status, idx) = send_bytes(
        fx.router(),
        empty_req("GET", "/crates-hosted/my/cr/mycrate", None),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    let text = String::from_utf8(idx).unwrap();
    let versions: Vec<String> = text
        .lines()
        .map(|l| {
            serde_json::from_str::<Value>(l).unwrap()["vers"]
                .as_str()
                .unwrap()
                .to_string()
        })
        .collect();
    assert_eq!(versions, vec!["1.0.0", "1.1.0"]);
}

// ---------- FR-61 版本不可变：重复发布同版本 409 ----------

#[tokio::test]
async fn cargo_重复发布同版本返回_409() {
    let fx = Fixture::new().await;
    let writer = fx.seed_user("dev", "pw", Role::User).await;
    let rid = fx
        .seed_cargo_repo("crates-hosted", Visibility::Public)
        .await;
    fx.seed_acl(&rid, &writer, Permission::Write).await;
    let auth = format!("Bearer {}", fx.login_token("dev", "pw").await);

    let body = publish_body("dup", "1.0.0", b"v1");
    let (s1, _) = send(
        fx.router(),
        bytes_req(
            "PUT",
            "/crates-hosted/api/v1/crates/new",
            Some(&auth),
            body.clone(),
        ),
    )
    .await;
    assert_eq!(s1, StatusCode::OK);

    // 再次发布同版本 → 409
    let (s2, err) = send(
        fx.router(),
        bytes_req("PUT", "/crates-hosted/api/v1/crates/new", Some(&auth), body),
    )
    .await;
    assert_eq!(s2, StatusCode::CONFLICT);
    assert_eq!(err["error"]["code"], "conflict");
}

// ---------- yank / unyank：翻转索引标记 ----------

#[tokio::test]
async fn cargo_yank_unyank_翻转索引标记() {
    let fx = Fixture::new().await;
    let writer = fx.seed_user("dev", "pw", Role::User).await;
    let rid = fx
        .seed_cargo_repo("crates-hosted", Visibility::Public)
        .await;
    fx.seed_acl(&rid, &writer, Permission::Write).await;
    let auth = format!("Bearer {}", fx.login_token("dev", "pw").await);

    // 发布
    send(
        fx.router(),
        bytes_req(
            "PUT",
            "/crates-hosted/api/v1/crates/new",
            Some(&auth),
            publish_body("yankme", "1.0.0", b"x"),
        ),
    )
    .await;

    // yank：DELETE /{repo}/api/v1/crates/{name}/{version}/yank
    let (status, body) = send(
        fx.router(),
        empty_req(
            "DELETE",
            "/crates-hosted/api/v1/crates/yankme/1.0.0/yank",
            Some(&auth),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(body["ok"], true);

    // 索引中该版本 yanked=true（yankme → ya/nk/yankme）
    let (_, idx) = send_bytes(
        fx.router(),
        empty_req("GET", "/crates-hosted/ya/nk/yankme", None),
    )
    .await;
    let line: Value = serde_json::from_str(String::from_utf8(idx).unwrap().trim()).unwrap();
    assert_eq!(line["yanked"], true);

    // unyank：PUT /{repo}/api/v1/crates/{name}/{version}/unyank
    let (status, body) = send(
        fx.router(),
        empty_req(
            "PUT",
            "/crates-hosted/api/v1/crates/yankme/1.0.0/unyank",
            Some(&auth),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(body["ok"], true);

    let (_, idx) = send_bytes(
        fx.router(),
        empty_req("GET", "/crates-hosted/ya/nk/yankme", None),
    )
    .await;
    let line: Value = serde_json::from_str(String::from_utf8(idx).unwrap().trim()).unwrap();
    assert_eq!(line["yanked"], false);
}

// ---------- FR-09 写授权边界 ----------

#[tokio::test]
async fn cargo_无写权限发布被拒() {
    let fx = Fixture::new().await;
    // public cargo 仓库：匿名可读但无写 → 发布应 403
    fx.seed_cargo_repo("crates-pub", Visibility::Public).await;
    let (status, _) = send(
        fx.router(),
        bytes_req(
            "PUT",
            "/crates-pub/api/v1/crates/new",
            None,
            publish_body("x", "1.0.0", b"x"),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::FORBIDDEN);
}

#[tokio::test]
async fn cargo_私有仓库对无权者读_404_隐藏存在性() {
    let fx = Fixture::new().await;
    fx.seed_cargo_repo("crates-secret", Visibility::Private)
        .await;
    // 匿名读 private 索引 → 404（不泄露存在）
    let (status, _) = send(
        fx.router(),
        empty_req("GET", "/crates-secret/se/rd/serde", None),
    )
    .await;
    assert_eq!(status, StatusCode::NOT_FOUND);
}

// ---------- FR-12 proxy：回源索引 + `.crate` cache-miss→hit ----------

/// 启动一个 mock cargo 稀疏 registry：按路径返回索引或 `.crate`，记录上游命中次数。
///
/// 返回 (上游基址, 索引命中计数, crate 命中计数)。
async fn 启动_mock_registry(
    index_line: &'static str,
    crate_path: &'static str,
    crate_bytes: &'static [u8],
) -> (
    String,
    Arc<std::sync::atomic::AtomicUsize>,
    Arc<std::sync::atomic::AtomicUsize>,
) {
    use axum::routing::get;
    use std::sync::atomic::{AtomicUsize, Ordering};

    let idx_calls = Arc::new(AtomicUsize::new(0));
    let crate_calls = Arc::new(AtomicUsize::new(0));

    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    let base = format!("http://{addr}");

    let ic = idx_calls.clone();
    let cc = crate_calls.clone();
    // 索引路径 se/rd/serde；下载路径 api/v1/crates/serde/1.0.0/download
    let app = Router::new()
        .route(
            "/se/rd/serde",
            get(move || {
                let ic = ic.clone();
                async move {
                    ic.fetch_add(1, Ordering::SeqCst);
                    (
                        [(axum::http::header::CONTENT_TYPE, "text/plain")],
                        index_line,
                    )
                }
            }),
        )
        .route(
            crate_path,
            get(move || {
                let cc = cc.clone();
                async move {
                    cc.fetch_add(1, Ordering::SeqCst);
                    crate_bytes
                }
            }),
        );

    tokio::spawn(async move {
        axum::serve(listener, app).await.unwrap();
    });
    (base, idx_calls, crate_calls)
}

#[tokio::test]
async fn cargo_proxy_回源索引并缓存_crate() {
    use std::sync::atomic::Ordering;

    let crate_bytes: &'static [u8] = b"upstream-crate-bytes";
    let cksum = sha256_hex(crate_bytes);
    // 上游索引行（cksum 为上游算得）
    let index_line: &'static str = Box::leak(
        format!(
            r#"{{"name":"serde","vers":"1.0.0","deps":[],"cksum":"{cksum}","features":{{}},"yanked":false}}"#
        )
        .into_boxed_str(),
    );
    let (上游基址, idx_calls, crate_calls) = 启动_mock_registry(
        index_line,
        "/api/v1/crates/serde/1.0.0/download",
        crate_bytes,
    )
    .await;
    let fx = Fixture::new().await;
    fx.seed_cargo_proxy_repo("crates-mirror", Visibility::Public, &上游基址)
        .await;

    // ① 取索引：proxy 回源（索引不缓存，每次回源）
    let (status, idx) = send_bytes(
        fx.router(),
        empty_req("GET", "/crates-mirror/se/rd/serde", None),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    let line: Value = serde_json::from_str(String::from_utf8(idx).unwrap().trim()).unwrap();
    assert_eq!(line["vers"], "1.0.0");
    assert_eq!(line["cksum"], cksum);
    assert_eq!(idx_calls.load(Ordering::SeqCst), 1, "应回源拉一次索引");

    // 再取索引：仍回源（索引不缓存）
    let (status, _) = send_bytes(
        fx.router(),
        empty_req("GET", "/crates-mirror/se/rd/serde", None),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(
        idx_calls.load(Ordering::SeqCst),
        2,
        "索引易变不缓存，应再次回源"
    );

    // ② cache-miss 取 .crate：回源一次
    let (status, bytes) = send_bytes(
        fx.router(),
        empty_req(
            "GET",
            "/crates-mirror/api/v1/crates/serde/1.0.0/download",
            None,
        ),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(bytes, crate_bytes);
    assert_eq!(
        crate_calls.load(Ordering::SeqCst),
        1,
        "首次 .crate 应回源一次"
    );

    // ③ cache-hit 再取 .crate：命中本地缓存，不再回源
    let (status, bytes) = send_bytes(
        fx.router(),
        empty_req(
            "GET",
            "/crates-mirror/api/v1/crates/serde/1.0.0/download",
            None,
        ),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(bytes, crate_bytes);
    assert_eq!(
        crate_calls.load(Ordering::SeqCst),
        1,
        "命中缓存不应再回源 .crate"
    );
}
