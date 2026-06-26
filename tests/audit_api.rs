//! 审计日志（FR-31，ADR-0015）的 HTTP 集成测试：经 axum 路由端到端验证
//! 事件入库、异步不阻塞主路径、采集失败降级不影响业务、管理查询仅 Admin、脱敏。
//!
//! 审计写入是异步的（独立写入任务），故对"已落库"的断言用短轮询等待，避免脆弱的固定睡眠。

use std::sync::Arc;
use std::time::Duration;

use axum::body::Body;
use axum::http::{Request, StatusCode};
use axum::Router;
use http_body_util::BodyExt;
use serde_json::{json, Value};
use tower::ServiceExt;

use jianartifact::api::{build_router, AppState};
use jianartifact::auth::{self, JwtSigner, LoginGuard};
use jianartifact::config::Config;
use jianartifact::format::{ArtifactService, DockerRegistry, FormatRegistry};
use jianartifact::meta::{AuditQuery, MetaStore, Role};
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
        let jwt = JwtSigner::from_secret(b"audit-secret-32-bytes-xxxxxxxxxxx", 3600);
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
        // 使用分析采集：建有界 channel 并启动写入任务（关明细），使路由真实走采集链路
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
            oidc: None,
            oidc_flows: std::sync::Arc::new(jianartifact::api::OidcFlowStore::new()),
            ldap: None,
            // FR-53：测试默认名单空、封禁登记表空（异常检测默认关闭）
            protection: std::sync::Arc::new(jianartifact::api::ProtectionState::new(
                jianartifact::config::ProtectionConfig::default(),
            )),
            ban_registry: std::sync::Arc::new(jianartifact::api::BanRegistry::new()),
            // FR-54：测试默认 CC 挑战关闭；挑战器用固定密钥
            cc_challenger: std::sync::Arc::new(jianartifact::api::CcChallenger::new(
                b"test-secret-32-bytes-xxxxxxxxxxxx",
            )),
            // FR-56：防护告警默认关闭，引擎与投递端就绪（关闭时 record 直接返回）
            alerts: jianartifact::api::alert_channel().0,
            alert_engine: std::sync::Arc::new(jianartifact::api::AlertEngine::new(
                jianartifact::api::alert_channel().0,
            )),
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

    /// 短轮询等待审计库出现满足条件的行，最多等约 2 秒；超时返回最后一次查询结果。
    async fn wait_audit<F>(&self, predicate: F) -> Vec<jianartifact::meta::AuditEntry>
    where
        F: Fn(&[jianartifact::meta::AuditEntry]) -> bool,
    {
        for _ in 0..200 {
            let rows = self
                .state
                .meta
                .query_audit(&AuditQuery {
                    limit: 1000,
                    ..Default::default()
                })
                .await
                .unwrap();
            if predicate(&rows) {
                return rows;
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        self.state
            .meta
            .query_audit(&AuditQuery {
                limit: 1000,
                ..Default::default()
            })
            .await
            .unwrap()
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

/// 构造带 JSON 体的请求。
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

/// 构造无 body 的请求。
fn empty_req(method: &str, uri: &str, auth: Option<&str>) -> Request<Body> {
    let mut builder = Request::builder().method(method).uri(uri);
    if let Some(a) = auth {
        builder = builder.header("authorization", a);
    }
    builder.body(Body::empty()).unwrap()
}

/// 登录取回 JWT 访问令牌。
async fn login_token(fx: &Fixture, username: &str, password: &str) -> String {
    let (status, body) = send(
        fx.router(),
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

// ---------- 事件入库 ----------

#[tokio::test]
async fn 仓库创建事件落审计库() {
    let fx = Fixture::new().await;
    fx.seed_user("admin", "S3cret!", Role::Admin).await;
    let token = login_token(&fx, "admin", "S3cret!").await;
    let bearer = format!("Bearer {token}");

    let (status, _) = send(
        fx.router(),
        json_req(
            "POST",
            "/api/v1/repositories",
            Some(&bearer),
            json!({ "name": "libs", "format": "raw", "type": "hosted", "visibility": "private" }),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::CREATED);

    let rows = fx
        .wait_audit(|rows| rows.iter().any(|e| e.action == "repo.create"))
        .await;
    let ev = rows.iter().find(|e| e.action == "repo.create").unwrap();
    assert_eq!(ev.actor, "admin");
    assert_eq!(ev.actor_kind, "session");
    assert_eq!(ev.result, "success");
}

#[tokio::test]
async fn 制品上传事件带仓库与路径() {
    let fx = Fixture::new().await;
    fx.seed_user("admin", "S3cret!", Role::Admin).await;
    let token = login_token(&fx, "admin", "S3cret!").await;
    let bearer = format!("Bearer {token}");

    // 建一个 raw hosted 仓库再上传
    send(
        fx.router(),
        json_req(
            "POST",
            "/api/v1/repositories",
            Some(&bearer),
            json!({ "name": "files", "format": "raw", "type": "hosted", "visibility": "public" }),
        ),
    )
    .await;
    let put = Request::builder()
        .method("PUT")
        .uri("/files/dir/a.txt")
        .header("authorization", &bearer)
        .body(Body::from("hello"))
        .unwrap();
    let resp = fx.router().oneshot(put).await.unwrap();
    assert!(resp.status().is_success(), "上传应成功: {}", resp.status());

    let rows = fx
        .wait_audit(|rows| rows.iter().any(|e| e.action == "artifact.upload"))
        .await;
    let ev = rows.iter().find(|e| e.action == "artifact.upload").unwrap();
    assert_eq!(ev.target_repo.as_deref(), Some("files"));
    assert_eq!(ev.target.as_deref(), Some("dir/a.txt"));
    assert_eq!(ev.result, "success");
}

// ---------- 异步不阻塞主路径 ----------

#[tokio::test]
async fn 审计采集不阻塞主请求路径() {
    let fx = Fixture::new().await;
    fx.seed_user("admin", "S3cret!", Role::Admin).await;
    let token = login_token(&fx, "admin", "S3cret!").await;
    let bearer = format!("Bearer {token}");

    // 连续多次管理写操作；每次响应都应立即返回成功，不被审计写入拖慢
    let started = std::time::Instant::now();
    for i in 0..20 {
        let (status, _) = send(
            fx.router(),
            json_req(
                "POST",
                "/api/v1/repositories",
                Some(&bearer),
                json!({ "name": format!("r{i}"), "format": "raw", "type": "hosted", "visibility": "public" }),
            ),
        )
        .await;
        assert_eq!(status, StatusCode::CREATED);
    }
    // 20 次创建应远快于审计批量落库的累计时延（主路径只做非阻塞 enqueue）
    assert!(
        started.elapsed() < Duration::from_secs(5),
        "主路径不应被审计写入阻塞，实际耗时 {:?}",
        started.elapsed()
    );

    // 审计最终都会落库（异步）
    let rows = fx
        .wait_audit(|rows| rows.iter().filter(|e| e.action == "repo.create").count() >= 20)
        .await;
    assert!(rows.iter().filter(|e| e.action == "repo.create").count() >= 20);
}

// ---------- 采集失败降级不影响业务 ----------

#[tokio::test]
async fn 审计写入任务缺失时业务仍成功() {
    // 构造一个不启动写入任务的状态：channel 接收端立即丢弃，enqueue 走 Closed/Full 降级分支。
    // 业务请求仍应正常返回，证明"采集失败不影响业务"。
    let dir = tempfile::tempdir().unwrap();
    let meta = MetaStore::open(&dir.path().join("test.db")).await.unwrap();
    let store = BlobBackend::Fs(LocalFsStore::new(dir.path().join("blobs")).await.unwrap());
    let jwt = JwtSigner::from_secret(b"audit-secret-32-bytes-xxxxxxxxxxx", 3600);
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
    // 注意：丢弃接收端（不 spawn 写入任务），channel 随即关闭
    let (audit, audit_rx) = jianartifact::api::audit_channel();
    drop(audit_rx);
    // 使用分析同样丢弃接收端：本用例只验证审计降级，不需要使用采集落库
    let (usage, usage_rx) = jianartifact::api::usage_channel();
    drop(usage_rx);
    let state = AppState {
        config: Arc::new(Config::default()),
        meta: meta.clone(),
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
        oidc_flows: std::sync::Arc::new(jianartifact::api::OidcFlowStore::new()),
        ldap: None,
        // FR-53：测试默认名单空、封禁登记表空（异常检测默认关闭）
        protection: std::sync::Arc::new(jianartifact::api::ProtectionState::new(
            jianartifact::config::ProtectionConfig::default(),
        )),
        ban_registry: std::sync::Arc::new(jianartifact::api::BanRegistry::new()),
        // FR-54：测试默认 CC 挑战关闭；挑战器用固定密钥
        cc_challenger: std::sync::Arc::new(jianartifact::api::CcChallenger::new(
            b"test-secret-32-bytes-xxxxxxxxxxxx",
        )),
        // FR-56：防护告警默认关闭，引擎与投递端就绪（关闭时 record 直接返回）
        alerts: jianartifact::api::alert_channel().0,
        alert_engine: std::sync::Arc::new(jianartifact::api::AlertEngine::new(
            jianartifact::api::alert_channel().0,
        )),
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
    let hash = auth::hash_password("S3cret!").unwrap();
    meta.create_user("admin", &hash, Role::Admin).await.unwrap();

    // 登录（成功）——即便审计 channel 已关闭，登录业务仍应成功
    let (status, body) = send(
        build_router(state.clone()),
        json_req(
            "POST",
            "/api/v1/auth/login",
            None,
            json!({ "username": "admin", "password": "S3cret!" }),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::OK, "审计降级不应影响登录业务: {body}");
    let token = body["access_token"].as_str().unwrap().to_string();

    // 管理写操作同样不受影响
    let (status, _) = send(
        build_router(state.clone()),
        json_req(
            "POST",
            "/api/v1/repositories",
            Some(&format!("Bearer {token}")),
            json!({ "name": "libs", "format": "raw", "type": "hosted", "visibility": "public" }),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::CREATED, "审计降级不应影响仓库创建业务");
}

// ---------- 管理查询仅 Admin ----------

#[tokio::test]
async fn 审计查询匿名_401() {
    let fx = Fixture::new().await;
    let (status, _) = send(fx.router(), empty_req("GET", "/api/v1/audit", None)).await;
    assert_eq!(status, StatusCode::UNAUTHORIZED);
}

#[tokio::test]
async fn 审计查询普通用户_403() {
    let fx = Fixture::new().await;
    fx.seed_user("dev", "pw", Role::User).await;
    let token = login_token(&fx, "dev", "pw").await;
    let (status, _) = send(
        fx.router(),
        empty_req("GET", "/api/v1/audit", Some(&format!("Bearer {token}"))),
    )
    .await;
    assert_eq!(status, StatusCode::FORBIDDEN);
}

#[tokio::test]
async fn 审计查询管理员可读且分页过滤生效() {
    let fx = Fixture::new().await;
    fx.seed_user("admin", "S3cret!", Role::Admin).await;
    let token = login_token(&fx, "admin", "S3cret!").await;
    let bearer = format!("Bearer {token}");

    // 制造两类事件：登录（已发生）+ 仓库创建
    send(
        fx.router(),
        json_req(
            "POST",
            "/api/v1/repositories",
            Some(&bearer),
            json!({ "name": "libs", "format": "raw", "type": "hosted", "visibility": "public" }),
        ),
    )
    .await;
    fx.wait_audit(|rows| rows.iter().any(|e| e.action == "repo.create"))
        .await;

    // 全量查询
    let (status, body) = send(
        fx.router(),
        empty_req("GET", "/api/v1/audit?limit=50", Some(&bearer)),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert!(body["total"].as_i64().unwrap() >= 2);
    assert!(body["items"].as_array().unwrap().len() >= 2);

    // 按动作过滤只剩 repo.create
    let (status, body) = send(
        fx.router(),
        empty_req("GET", "/api/v1/audit?action=repo.create", Some(&bearer)),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    let items = body["items"].as_array().unwrap();
    assert!(!items.is_empty());
    assert!(items.iter().all(|it| it["action"] == "repo.create"));
}

// ---------- 脱敏 ----------

#[tokio::test]
async fn 登录事件不含口令且失败记被尝试用户名() {
    let fx = Fixture::new().await;
    fx.seed_user("admin", "TopS3cretPw", Role::Admin).await;

    // 一次失败登录（口令错误）
    let (status, _) = send(
        fx.router(),
        json_req(
            "POST",
            "/api/v1/auth/login",
            None,
            json!({ "username": "admin", "password": "WrongPw" }),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::UNAUTHORIZED);

    let rows = fx
        .wait_audit(|rows| rows.iter().any(|e| e.action == "login"))
        .await;
    let ev = rows.iter().find(|e| e.action == "login").unwrap();
    // 失败登录记被尝试的用户名与 denied 结果
    assert_eq!(ev.actor, "admin");
    assert_eq!(ev.result, "denied");

    // 全字段拼起来检查：绝不含任何口令明文（正确口令与被尝试口令都不得出现）
    let serialized = format!("{ev:?}");
    assert!(
        !serialized.contains("TopS3cretPw") && !serialized.contains("WrongPw"),
        "审计记录不得包含口令明文: {serialized}"
    );
}

#[tokio::test]
async fn token_签发审计不含明文token() {
    let fx = Fixture::new().await;
    fx.seed_user("dev", "pw", Role::User).await;
    let token = login_token(&fx, "dev", "pw").await;
    let bearer = format!("Bearer {token}");

    let (status, body) = send(
        fx.router(),
        json_req(
            "POST",
            "/api/v1/tokens",
            Some(&bearer),
            json!({ "name": "ci" }),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::CREATED);
    let plaintext = body["token"].as_str().unwrap().to_string();
    assert!(plaintext.starts_with("jna_"));

    let rows = fx
        .wait_audit(|rows| rows.iter().any(|e| e.action == "token.issue"))
        .await;
    let ev = rows.iter().find(|e| e.action == "token.issue").unwrap();
    assert_eq!(ev.result, "success");
    // 明文 Token 绝不入审计
    let serialized = format!("{ev:?}");
    assert!(
        !serialized.contains(&plaintext),
        "审计记录不得包含明文 Token"
    );
}

// ---------- 授权拒绝入审计 ----------

#[tokio::test]
async fn 普通用户越权创建仓库记_denied() {
    let fx = Fixture::new().await;
    fx.seed_user("dev", "pw", Role::User).await;
    let token = login_token(&fx, "dev", "pw").await;
    let bearer = format!("Bearer {token}");

    // 普通用户无权创建仓库 → 403
    let (status, _) = send(
        fx.router(),
        json_req(
            "POST",
            "/api/v1/repositories",
            Some(&bearer),
            json!({ "name": "libs", "format": "raw", "type": "hosted", "visibility": "public" }),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::FORBIDDEN);

    let rows = fx
        .wait_audit(|rows| {
            rows.iter()
                .any(|e| e.action == "repo.create" && e.result == "denied")
        })
        .await;
    let ev = rows
        .iter()
        .find(|e| e.action == "repo.create" && e.result == "denied")
        .unwrap();
    assert_eq!(ev.actor, "dev");
    assert_eq!(ev.actor_kind, "session");
}

// ---------- 全量非读：新覆盖的变更端点产事件（FR-97） ----------

#[tokio::test]
async fn 设置patch事件落审计库() {
    let fx = Fixture::new().await;
    fx.seed_user("admin", "S3cret!", Role::Admin).await;
    let token = login_token(&fx, "admin", "S3cret!").await;
    let bearer = format!("Bearer {token}");

    // PATCH 设置（空代理 + 关在线更新，避免依赖外部代理）：变更类请求须留痕
    let (status, _) = send(
        fx.router(),
        json_req(
            "PATCH",
            "/api/v1/settings",
            Some(&bearer),
            json!({
                "network_proxy": { "http": null, "https": null, "no_proxy": null },
                "update": {
                    "enabled": false,
                    "repo": "owner/repo",
                    "api_base_url": "https://api.github.com",
                    "restart_mode": "exit"
                }
            }),
        ),
    )
    .await;
    assert!(status.is_success(), "设置 PATCH 应成功: {status}");

    let rows = fx
        .wait_audit(|rows| rows.iter().any(|e| e.action == "settings.update"))
        .await;
    let ev = rows.iter().find(|e| e.action == "settings.update").unwrap();
    assert_eq!(ev.actor, "admin");
}

#[tokio::test]
async fn 防护配置patch事件落审计库() {
    let fx = Fixture::new().await;
    fx.seed_user("admin", "S3cret!", Role::Admin).await;
    let token = login_token(&fx, "admin", "S3cret!").await;
    let bearer = format!("Bearer {token}");

    let (status, _) = send(
        fx.router(),
        json_req(
            "PATCH",
            "/api/v1/protection/config",
            Some(&bearer),
            json!({}),
        ),
    )
    .await;
    assert!(
        status.is_success() || status == StatusCode::BAD_REQUEST,
        "防护配置 PATCH 应被路由处理: {status}"
    );

    let rows = fx
        .wait_audit(|rows| rows.iter().any(|e| e.action == "protection.config.update"))
        .await;
    assert!(rows
        .iter()
        .any(|e| e.action == "protection.config.update" && e.actor == "admin"));
}

#[tokio::test]
async fn 迁移任务控制事件落审计库() {
    let fx = Fixture::new().await;
    fx.seed_user("admin", "S3cret!", Role::Admin).await;
    let token = login_token(&fx, "admin", "S3cret!").await;
    let bearer = format!("Bearer {token}");

    // 未知任务控制返回 404，但仍属变更类请求、须留痕
    let (status, _) = send(
        fx.router(),
        empty_req("POST", "/api/v1/migrate/jobs/nope/cancel", Some(&bearer)),
    )
    .await;
    assert_eq!(status, StatusCode::NOT_FOUND);

    let rows = fx
        .wait_audit(|rows| rows.iter().any(|e| e.action == "migrate.job.control"))
        .await;
    let ev = rows
        .iter()
        .find(|e| e.action == "migrate.job.control")
        .unwrap();
    assert_eq!(ev.actor, "admin");
    assert_eq!(ev.target.as_deref(), Some("nope/cancel"));
    // 未知任务 404 归类 denied（不泄露存在性，仅留痕动作）
    assert_eq!(ev.result, "denied");
}

#[tokio::test]
async fn 用户组创建事件落审计库() {
    let fx = Fixture::new().await;
    fx.seed_user("admin", "S3cret!", Role::Admin).await;
    let token = login_token(&fx, "admin", "S3cret!").await;
    let bearer = format!("Bearer {token}");

    let (status, _) = send(
        fx.router(),
        json_req(
            "POST",
            "/api/v1/groups",
            Some(&bearer),
            json!({ "name": "team-a" }),
        ),
    )
    .await;
    assert!(status.is_success(), "用户组创建应成功: {status}");

    let rows = fx
        .wait_audit(|rows| rows.iter().any(|e| e.action == "group.create"))
        .await;
    assert!(rows
        .iter()
        .any(|e| e.action == "group.create" && e.actor == "admin"));
}

// ---------- 读取类一律不入审计（GET 下载/浏览/搜索/详情） ----------

#[tokio::test]
async fn 读取类请求不产审计事件() {
    let fx = Fixture::new().await;
    fx.seed_user("admin", "S3cret!", Role::Admin).await;
    let token = login_token(&fx, "admin", "S3cret!").await;
    let bearer = format!("Bearer {token}");

    // 先做一次变更产生一条审计基线，便于"等到该基线落库后再断言无读事件"
    send(
        fx.router(),
        json_req(
            "POST",
            "/api/v1/repositories",
            Some(&bearer),
            json!({ "name": "g-read", "format": "raw", "type": "hosted", "visibility": "public" }),
        ),
    )
    .await;

    // 一批读取请求：列表 / 详情 / 搜索 / 下载
    for (m, uri) in [
        ("GET", "/api/v1/repositories"),
        ("GET", "/api/v1/users"),
        ("GET", "/api/v1/search?q=x"),
        ("GET", "/api/v1/repositories/g-read"),
        ("GET", "/g-read/no/such.txt"),
    ] {
        send(fx.router(), empty_req(m, uri, Some(&bearer))).await;
    }

    // 等基线变更事件落库
    let rows = fx
        .wait_audit(|rows| rows.iter().any(|e| e.action == "repo.create"))
        .await;
    // 读取类动作一律不应出现在审计中
    let read_actions = [
        "repo.list",
        "user.list",
        "search",
        "artifact.access",
        "artifact.download",
        "change.get",
    ];
    for a in read_actions {
        assert!(
            rows.iter().all(|e| e.action != a),
            "读取动作 {a} 不应入审计"
        );
    }
    // 也不应有任何以 GET 兜底的事件
    assert!(rows.iter().all(|e| !e.action.contains("get")));
}
