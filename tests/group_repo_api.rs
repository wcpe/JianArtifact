//! group / 虚拟聚合仓库的 HTTP 集成测试（FR-136）。
//!
//! 重点穷举 group GET 解析的鉴权矩阵（#1 高风险区，命门）：
//! 成员 public/private × 调用方（Admin / User-有读ACL / User-无ACL / 匿名）逐格断言
//! 「有权且存在 → 命中返回 / 无权 → 视同不存在返 404、不泄露存在性」；
//! 并覆盖成员有序解析（靠后成员命中 / 多成员命中靠前）、都无 → 404、
//! 写 / 删 / POST 到 group → 405、建 group 成员格式一致校验、proxy 成员 cache-miss 回源。

use std::sync::Arc;

use axum::body::Body;
use axum::http::{Request, StatusCode};
use axum::Router;
use base64::{engine::general_purpose::STANDARD, Engine};
use http_body_util::BodyExt;
use serde_json::{json, Value};
use tower::ServiceExt;

use jianartifact::api::{build_router, AppState};
use jianartifact::auth::{self, JwtSigner, LoginGuard};
use jianartifact::config::Config;
use jianartifact::format::{ArtifactService, DockerRegistry, FormatRegistry};
use jianartifact::meta::{MetaStore, NewRepository, Permission, RepoType, Role, Visibility};
use jianartifact::proxy::HttpUpstream;
use jianartifact::storage::{BlobBackend, LocalFsStore};

/// 测试夹具：走真实 SQLite 文件与本地 blob 存储，验证迁移、外键级联与解析链路。
struct Fixture {
    state: AppState,
    _dir: tempfile::TempDir,
}

impl Fixture {
    async fn new() -> Self {
        let dir = tempfile::tempdir().unwrap();
        let meta = MetaStore::open(&dir.path().join("test.db")).await.unwrap();
        let store = BlobBackend::Fs(LocalFsStore::new(dir.path().join("blobs")).await.unwrap());
        let jwt = JwtSigner::from_secret(b"group-repo-secret-32-bytes-xxxxxx", 3600);
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
            login_guard: Arc::new(LoginGuard::new(50, 900)),
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
            protection: std::sync::Arc::new(jianartifact::api::ProtectionState::new(
                jianartifact::config::ProtectionConfig::default(),
            )),
            ban_registry: std::sync::Arc::new(jianartifact::api::BanRegistry::new()),
            cc_challenger: std::sync::Arc::new(jianartifact::api::CcChallenger::new(
                b"test-secret-32-bytes-xxxxxxxxxxxx",
            )),
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
            tasks: std::sync::Arc::new(jianartifact::api::TaskRegistry::default()),
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

    /// 建一个 raw hosted 成员仓库，返回 id。
    async fn seed_raw_repo(&self, name: &str, visibility: Visibility) -> String {
        self.state
            .meta
            .create_repository(NewRepository {
                name,
                format: "raw",
                r#type: RepoType::Hosted,
                visibility,
                upstream_url: None,
                upstream_auth_ref: None,
            })
            .await
            .unwrap()
    }

    /// 建一个 raw proxy 成员仓库（指向给定上游基址），返回 id。
    async fn seed_raw_proxy_repo(
        &self,
        name: &str,
        visibility: Visibility,
        upstream: &str,
    ) -> String {
        self.state
            .meta
            .create_repository(NewRepository {
                name,
                format: "raw",
                r#type: RepoType::Proxy,
                visibility,
                upstream_url: Some(upstream),
                upstream_auth_ref: None,
            })
            .await
            .unwrap()
    }

    /// 建一个 group 仓库并设定有序成员，返回 group id。
    async fn seed_group(
        &self,
        name: &str,
        visibility: Visibility,
        member_ids: &[String],
    ) -> String {
        let gid = self
            .state
            .meta
            .create_repository(NewRepository {
                name,
                format: "raw",
                r#type: RepoType::Group,
                visibility,
                upstream_url: None,
                upstream_auth_ref: None,
            })
            .await
            .unwrap();
        self.state
            .meta
            .set_repo_group_members(&gid, member_ids)
            .await
            .unwrap();
        gid
    }

    async fn seed_acl(&self, repo_id: &str, user_id: &str, permission: Permission) {
        self.state
            .meta
            .create_acl(repo_id, user_id, permission)
            .await
            .unwrap();
    }

    /// 以指定身份向成员仓库 PUT 上传一个 raw 制品（走真实格式路由，端到端落 blob + 索引）。
    async fn put_artifact(&self, auth: &str, repo_name: &str, path: &str, body: &[u8]) {
        let req = raw_req(
            "PUT",
            &format!("/{repo_name}/{path}"),
            Some(auth),
            body.to_vec(),
        );
        let resp = self.router().oneshot(req).await.unwrap();
        assert!(
            resp.status() == StatusCode::CREATED || resp.status() == StatusCode::OK,
            "上传应成功: {}",
            resp.status()
        );
    }

    async fn login_token(&self, username: &str, password: &str) -> String {
        let resp = self
            .router()
            .oneshot(json_req(
                "POST",
                "/api/v1/auth/login",
                None,
                json!({ "username": username, "password": password }),
            ))
            .await
            .unwrap();
        assert_eq!(resp.status(), StatusCode::OK, "登录应成功");
        let bytes = resp.into_body().collect().await.unwrap().to_bytes();
        let body: Value = serde_json::from_slice(&bytes).unwrap();
        body["access_token"].as_str().unwrap().to_string()
    }
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

fn raw_req(method: &str, uri: &str, auth: Option<&str>, body: Vec<u8>) -> Request<Body> {
    let mut builder = Request::builder().method(method).uri(uri);
    if let Some(a) = auth {
        builder = builder.header("authorization", a);
    }
    builder.body(Body::from(body)).unwrap()
}

fn empty_req(method: &str, uri: &str, auth: Option<&str>) -> Request<Body> {
    let mut builder = Request::builder().method(method).uri(uri);
    if let Some(a) = auth {
        builder = builder.header("authorization", a);
    }
    builder.body(Body::empty()).unwrap()
}

/// 发请求返回 (状态码, 原始字节)。
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

/// GET group 内某路径，返回 (状态码, 字节)。
async fn get_group(
    fx: &Fixture,
    group: &str,
    path: &str,
    auth: Option<&str>,
) -> (StatusCode, Vec<u8>) {
    send_bytes(
        fx.router(),
        empty_req("GET", &format!("/{group}/{path}"), auth),
    )
    .await
}

// ==================== 成员有序解析 ====================

#[tokio::test]
async fn 有序解析_命中靠前成员_单命中靠后成员_全无则404() {
    let fx = Fixture::new().await;
    let admin_id = fx.seed_user("admin", "pw", Role::Admin).await;
    let a = fx.seed_raw_repo("mem-a", Visibility::Public).await;
    let b = fx.seed_raw_repo("mem-b", Visibility::Public).await;
    // 授 admin 对两成员写权限以上传（admin 角色本身放行写）
    fx.seed_acl(&a, &admin_id, Permission::Write).await;
    fx.seed_acl(&b, &admin_id, Permission::Write).await;
    let admin = format!("Bearer {}", fx.login_token("admin", "pw").await);
    // group 成员顺序 [a, b]
    fx.seed_group("g", Visibility::Public, &[a.clone(), b.clone()])
        .await;

    // ① 制品仅在靠后成员 b → 解析命中 b
    fx.put_artifact(&admin, "mem-b", "only-b.txt", b"from-b")
        .await;
    let (status, bytes) = get_group(&fx, "g", "only-b.txt", None).await;
    assert_eq!(status, StatusCode::OK, "应命中靠后成员 b");
    assert_eq!(bytes, b"from-b");

    // ② 同路径两成员都有 → 命中靠前成员 a（顺序优先）
    fx.put_artifact(&admin, "mem-a", "both.txt", b"from-a")
        .await;
    fx.put_artifact(&admin, "mem-b", "both.txt", b"from-b")
        .await;
    let (status, bytes) = get_group(&fx, "g", "both.txt", None).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(bytes, b"from-a", "多成员命中应取靠前成员 a");

    // ③ 都无该制品 → 404
    let (status, _) = get_group(&fx, "g", "nowhere.txt", None).await;
    assert_eq!(status, StatusCode::NOT_FOUND, "全部成员未命中应 404");
}

#[tokio::test]
async fn 空_group_解析恒404() {
    let fx = Fixture::new().await;
    fx.seed_group("empty-g", Visibility::Public, &[]).await;
    let (status, _) = get_group(&fx, "empty-g", "anything.txt", None).await;
    assert_eq!(status, StatusCode::NOT_FOUND);
}

// ==================== 鉴权矩阵（命门） ====================

// 成员为 public：命中不依赖调用方读 ACL，四类调用方均能命中。
#[tokio::test]
async fn public_成员_四类调用方均命中() {
    let fx = Fixture::new().await;
    let admin_id = fx.seed_user("admin", "pw", Role::Admin).await;
    fx.seed_user("reader", "pw", Role::User).await;
    fx.seed_user("outsider", "pw", Role::User).await;
    let m = fx.seed_raw_repo("pub-mem", Visibility::Public).await;
    fx.seed_acl(&m, &admin_id, Permission::Write).await;
    let admin = format!("Bearer {}", fx.login_token("admin", "pw").await);
    fx.seed_group("g", Visibility::Public, std::slice::from_ref(&m))
        .await;
    fx.put_artifact(&admin, "pub-mem", "f.txt", b"pub").await;

    // 匿名 / User-无ACL / User / Admin → 均命中 200
    for (name, auth) in [
        ("匿名", None),
        (
            "User-无ACL",
            Some(format!("Bearer {}", fx.login_token("outsider", "pw").await)),
        ),
        (
            "User",
            Some(format!("Bearer {}", fx.login_token("reader", "pw").await)),
        ),
        ("Admin", Some(admin.clone())),
    ] {
        let (status, bytes) = get_group(&fx, "g", "f.txt", auth.as_deref()).await;
        assert_eq!(status, StatusCode::OK, "public 成员对 {name} 应命中");
        assert_eq!(bytes, b"pub");
    }
}

// 成员为 private：仅 Admin 与有读 ACL 的 User 命中；User-无ACL 与匿名视同不存在 → 404（不泄露）。
#[tokio::test]
async fn private_成员_鉴权矩阵逐格断言() {
    let fx = Fixture::new().await;
    let admin_id = fx.seed_user("admin", "pw", Role::Admin).await;
    let reader = fx.seed_user("reader", "pw", Role::User).await;
    fx.seed_user("outsider", "pw", Role::User).await;
    let m = fx.seed_raw_repo("priv-mem", Visibility::Private).await;
    fx.seed_acl(&m, &admin_id, Permission::Write).await;
    fx.seed_acl(&m, &reader, Permission::Read).await;
    let admin = format!("Bearer {}", fx.login_token("admin", "pw").await);
    // group 自身 public，聚焦成员级判定
    fx.seed_group("g", Visibility::Public, std::slice::from_ref(&m))
        .await;
    fx.put_artifact(&admin, "priv-mem", "s.txt", b"secret")
        .await;

    // Admin → 命中
    let (status, bytes) = get_group(&fx, "g", "s.txt", Some(&admin)).await;
    assert_eq!(status, StatusCode::OK, "Admin 应命中 private 成员");
    assert_eq!(bytes, b"secret");

    // User-有读ACL → 命中
    let reader_auth = format!("Bearer {}", fx.login_token("reader", "pw").await);
    let (status, bytes) = get_group(&fx, "g", "s.txt", Some(&reader_auth)).await;
    assert_eq!(status, StatusCode::OK, "有读 ACL 的 User 应命中");
    assert_eq!(bytes, b"secret");

    // User-无ACL → 视同不存在 → 404（不泄露存在性）
    let out_auth = format!("Bearer {}", fx.login_token("outsider", "pw").await);
    let (status, _) = get_group(&fx, "g", "s.txt", Some(&out_auth)).await;
    assert_eq!(status, StatusCode::NOT_FOUND, "无 ACL User 应 404 不泄露");

    // 匿名 → 视同不存在 → 404（不泄露存在性）
    let (status, _) = get_group(&fx, "g", "s.txt", None).await;
    assert_eq!(
        status,
        StatusCode::NOT_FOUND,
        "匿名对 private 成员应 404 不泄露"
    );
}

// 私有成员被无权调用方跳过后，不影响后续 public 成员命中（跳过语义正确）。
#[tokio::test]
async fn 私有成员被跳过_后续public成员仍命中() {
    let fx = Fixture::new().await;
    let admin_id = fx.seed_user("admin", "pw", Role::Admin).await;
    fx.seed_user("outsider", "pw", Role::User).await;
    let priv_m = fx.seed_raw_repo("priv-first", Visibility::Private).await;
    let pub_m = fx.seed_raw_repo("pub-second", Visibility::Public).await;
    fx.seed_acl(&priv_m, &admin_id, Permission::Write).await;
    fx.seed_acl(&pub_m, &admin_id, Permission::Write).await;
    let admin = format!("Bearer {}", fx.login_token("admin", "pw").await);
    // group 顺序 [priv-first, pub-second]：同路径都有制品
    fx.seed_group("g", Visibility::Public, &[priv_m.clone(), pub_m.clone()])
        .await;
    fx.put_artifact(&admin, "priv-first", "x.txt", b"private-copy")
        .await;
    fx.put_artifact(&admin, "pub-second", "x.txt", b"public-copy")
        .await;

    // 匿名：靠前的私有成员被跳过（视同不存在），命中靠后的 public 成员
    let (status, bytes) = get_group(&fx, "g", "x.txt", None).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(
        bytes, b"public-copy",
        "私有成员应被跳过，命中后续 public 成员"
    );

    // Admin：靠前私有成员有权，命中靠前的私有副本
    let (status, bytes) = get_group(&fx, "g", "x.txt", Some(&admin)).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(bytes, b"private-copy", "Admin 有权命中靠前私有成员");
}

// group 自身为 private：不可见调用方对 group 整体 404（隐藏 group 存在性），即便成员为 public。
#[tokio::test]
async fn 私有group自身对无权调用方整体404() {
    let fx = Fixture::new().await;
    let admin_id = fx.seed_user("admin", "pw", Role::Admin).await;
    let outsider_id = fx.seed_user("outsider", "pw", Role::User).await;
    let m = fx.seed_raw_repo("pub-mem", Visibility::Public).await;
    fx.seed_acl(&m, &admin_id, Permission::Write).await;
    let admin = format!("Bearer {}", fx.login_token("admin", "pw").await);
    // group 自身 private，成员 public
    let gid = fx
        .seed_group("g", Visibility::Private, std::slice::from_ref(&m))
        .await;
    fx.put_artifact(&admin, "pub-mem", "f.txt", b"data").await;

    // 匿名对 private group → 404（先过 group 自身读判定，隐藏 group 存在性）
    let (status, _) = get_group(&fx, "g", "f.txt", None).await;
    assert_eq!(
        status,
        StatusCode::NOT_FOUND,
        "匿名对 private group 应整体 404"
    );

    // User-无ACL 对 private group → 404
    let out_auth = format!("Bearer {}", fx.login_token("outsider", "pw").await);
    let (status, _) = get_group(&fx, "g", "f.txt", Some(&out_auth)).await;
    assert_eq!(status, StatusCode::NOT_FOUND);

    // 给 outsider 授 group 自身读 ACL → 可越过 group 层，命中 public 成员
    fx.seed_acl(&gid, &outsider_id, Permission::Read).await;
    let (status, bytes) = get_group(&fx, "g", "f.txt", Some(&out_auth)).await;
    assert_eq!(status, StatusCode::OK, "有 group 读 ACL 应可访问");
    assert_eq!(bytes, b"data");
}

// 三身份通道（Bearer-JWT / Bearer-Token / Basic）对 private 成员的 group 解析判定一致。
#[tokio::test]
async fn 三身份通道对group内private成员判定一致() {
    let fx = Fixture::new().await;
    let admin_id = fx.seed_user("admin", "pw", Role::Admin).await;
    let reader = fx.seed_user("reader", "pw", Role::User).await;
    let m = fx.seed_raw_repo("priv-mem", Visibility::Private).await;
    fx.seed_acl(&m, &admin_id, Permission::Write).await;
    fx.seed_acl(&m, &reader, Permission::Read).await;
    let admin = format!("Bearer {}", fx.login_token("admin", "pw").await);
    fx.seed_group("g", Visibility::Public, std::slice::from_ref(&m))
        .await;
    fx.put_artifact(&admin, "priv-mem", "s.txt", b"secret")
        .await;

    // 有读 ACL 的 reader，三通道均命中
    let jwt = fx.login_token("reader", "pw").await;
    let plaintext = jianartifact::auth::generate_api_token();
    fx.state
        .meta
        .create_token(
            &reader,
            "ci",
            &jianartifact::auth::hash_api_token(&plaintext),
        )
        .await
        .unwrap();
    let basic = format!("Basic {}", STANDARD.encode("reader:pw"));
    for (name, header) in [
        ("JWT", format!("Bearer {jwt}")),
        ("Token", format!("Bearer {plaintext}")),
        ("Basic", basic),
    ] {
        let (status, bytes) = get_group(&fx, "g", "s.txt", Some(&header)).await;
        assert_eq!(status, StatusCode::OK, "{name} 通道有权应命中");
        assert_eq!(bytes, b"secret");
    }

    // 无权 outsider，三通道均 404（任一通道不绕过成员判定）
    let outsider = fx.seed_user("outsider", "pw", Role::User).await;
    let out_plain = jianartifact::auth::generate_api_token();
    fx.state
        .meta
        .create_token(
            &outsider,
            "ci",
            &jianartifact::auth::hash_api_token(&out_plain),
        )
        .await
        .unwrap();
    let out_jwt = fx.login_token("outsider", "pw").await;
    let out_basic = format!("Basic {}", STANDARD.encode("outsider:pw"));
    for (name, header) in [
        ("JWT", format!("Bearer {out_jwt}")),
        ("Token", format!("Bearer {out_plain}")),
        ("Basic", out_basic),
    ] {
        let (status, _) = get_group(&fx, "g", "s.txt", Some(&header)).await;
        assert_eq!(
            status,
            StatusCode::NOT_FOUND,
            "无权 {name} 通道应 404 不泄露"
        );
    }
}

// ==================== group 只读：写 / 删 / POST → 405 ====================

#[tokio::test]
async fn group_写删post均405() {
    let fx = Fixture::new().await;
    fx.seed_user("admin", "pw", Role::Admin).await;
    let admin = format!("Bearer {}", fx.login_token("admin", "pw").await);
    let m = fx.seed_raw_repo("mem", Visibility::Public).await;
    fx.seed_group("g", Visibility::Public, std::slice::from_ref(&m))
        .await;

    // PUT → 405
    let resp = fx
        .router()
        .oneshot(raw_req("PUT", "/g/x.txt", Some(&admin), b"data".to_vec()))
        .await
        .unwrap();
    assert_eq!(
        resp.status(),
        StatusCode::METHOD_NOT_ALLOWED,
        "group PUT 应 405"
    );

    // DELETE → 405
    let resp = fx
        .router()
        .oneshot(empty_req("DELETE", "/g/x.txt", Some(&admin)))
        .await
        .unwrap();
    assert_eq!(
        resp.status(),
        StatusCode::METHOD_NOT_ALLOWED,
        "group DELETE 应 405"
    );

    // POST（PyPI twine 形态兜底路由）→ 405。构造合法 multipart 体，
    // 使请求通过 Multipart 提取器进入 handler，让 group 只读判定得以拦截。
    let boundary = "----JianArtifactGroupBoundary";
    let mut body: Vec<u8> = Vec::new();
    body.extend_from_slice(format!("--{boundary}\r\n").as_bytes());
    body.extend_from_slice(
        b"Content-Disposition: form-data; name=\"content\"; filename=\"x.txt\"\r\n",
    );
    body.extend_from_slice(b"Content-Type: application/octet-stream\r\n\r\ndata\r\n");
    body.extend_from_slice(format!("--{boundary}--\r\n").as_bytes());
    let post_req = Request::builder()
        .method("POST")
        .uri("/g/x.txt")
        .header("authorization", &admin)
        .header(
            "content-type",
            format!("multipart/form-data; boundary={boundary}"),
        )
        .body(Body::from(body))
        .unwrap();
    let resp = fx.router().oneshot(post_req).await.unwrap();
    assert_eq!(
        resp.status(),
        StatusCode::METHOD_NOT_ALLOWED,
        "group POST 应 405"
    );
}

// 私有 group 对无权调用方的写：先过读判定返 404（不泄露存在性），不返 405。
#[tokio::test]
async fn 私有group对无权调用方写为404不泄露() {
    let fx = Fixture::new().await;
    fx.seed_user("outsider", "pw", Role::User).await;
    let m = fx.seed_raw_repo("mem", Visibility::Public).await;
    fx.seed_group("g", Visibility::Private, std::slice::from_ref(&m))
        .await;

    // 匿名对 private group 写 → 404（不泄露 group 存在，也不提前暴露 405）
    let resp = fx
        .router()
        .oneshot(raw_req("PUT", "/g/x.txt", None, b"data".to_vec()))
        .await
        .unwrap();
    assert_eq!(
        resp.status(),
        StatusCode::NOT_FOUND,
        "私有 group 匿名写应 404"
    );

    let out_auth = format!("Bearer {}", fx.login_token("outsider", "pw").await);
    let resp = fx
        .router()
        .oneshot(raw_req(
            "PUT",
            "/g/x.txt",
            Some(&out_auth),
            b"data".to_vec(),
        ))
        .await
        .unwrap();
    assert_eq!(
        resp.status(),
        StatusCode::NOT_FOUND,
        "私有 group 无权 User 写应 404"
    );
}

// ==================== 建 group：成员格式一致校验 ====================

#[tokio::test]
async fn 建group成员格式不一致_400() {
    let fx = Fixture::new().await;
    fx.seed_user("admin", "pw", Role::Admin).await;
    let admin = format!("Bearer {}", fx.login_token("admin", "pw").await);
    // 建一个 maven 成员与一个 npm 成员
    fx.state
        .meta
        .create_repository(NewRepository {
            name: "mvn-mem",
            format: "maven",
            r#type: RepoType::Hosted,
            visibility: Visibility::Public,
            upstream_url: None,
            upstream_auth_ref: None,
        })
        .await
        .unwrap();
    fx.state
        .meta
        .create_repository(NewRepository {
            name: "npm-mem",
            format: "npm",
            r#type: RepoType::Hosted,
            visibility: Visibility::Public,
            upstream_url: None,
            upstream_auth_ref: None,
        })
        .await
        .unwrap();

    // maven group 加入 npm 成员 → 400，且不留半截 group
    let resp = fx
        .router()
        .oneshot(json_req(
            "POST",
            "/api/v1/repositories",
            Some(&admin),
            json!({
                "name": "mvn-group",
                "format": "maven",
                "type": "group",
                "visibility": "public",
                "members": ["mvn-mem", "npm-mem"]
            }),
        ))
        .await
        .unwrap();
    assert_eq!(
        resp.status(),
        StatusCode::BAD_REQUEST,
        "格式不一致成员应 400"
    );
    assert!(
        fx.state
            .meta
            .get_repository_by_name("mvn-group")
            .await
            .unwrap()
            .is_none(),
        "校验失败不应留半截 group"
    );
}

#[tokio::test]
async fn 建group并经api创建与回显成员() {
    let fx = Fixture::new().await;
    fx.seed_user("admin", "pw", Role::Admin).await;
    let admin = format!("Bearer {}", fx.login_token("admin", "pw").await);
    fx.seed_raw_repo("a", Visibility::Public).await;
    fx.seed_raw_repo("b", Visibility::Public).await;

    let resp = fx
        .router()
        .oneshot(json_req(
            "POST",
            "/api/v1/repositories",
            Some(&admin),
            json!({
                "name": "raw-group",
                "format": "raw",
                "type": "group",
                "visibility": "public",
                "members": ["b", "a"]
            }),
        ))
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::CREATED);
    let bytes = resp.into_body().collect().await.unwrap().to_bytes();
    let body: Value = serde_json::from_slice(&bytes).unwrap();
    assert_eq!(body["type"], "group");
    // 成员按入参顺序回显（有序）
    let members: Vec<&str> = body["members"]
        .as_array()
        .unwrap()
        .iter()
        .map(|m| m.as_str().unwrap())
        .collect();
    assert_eq!(members, vec!["b", "a"], "成员应有序回显");
}

// ==================== proxy 成员 cache-miss 回源缓存 ====================

/// 启动本地 mock 上游（真实 TCP，走真链路），返回 (基址, 命中计数)。
async fn 启动_mock_上游(
    content: &'static [u8],
) -> (String, Arc<std::sync::atomic::AtomicUsize>) {
    use std::sync::atomic::Ordering;
    let calls = Arc::new(std::sync::atomic::AtomicUsize::new(0));
    let calls_in_handler = calls.clone();
    let app = Router::new().fallback(move || {
        let calls = calls_in_handler.clone();
        async move {
            calls.fetch_add(1, Ordering::SeqCst);
            content
        }
    });
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    tokio::spawn(async move {
        axum::serve(listener, app).await.unwrap();
    });
    (format!("http://{addr}"), calls)
}

#[tokio::test]
async fn group内proxy成员命中触发回源并缓存() {
    use std::sync::atomic::Ordering;
    let (上游基址, 上游命中) = 启动_mock_上游(b"from-upstream").await;
    let fx = Fixture::new().await;
    let proxy_m = fx
        .seed_raw_proxy_repo("proxy-mem", Visibility::Public, &上游基址)
        .await;
    fx.seed_group("g", Visibility::Public, std::slice::from_ref(&proxy_m))
        .await;

    // ① 经 group 匿名 GET：命中 proxy 成员触发 cache-miss 回源
    let (status, bytes) = get_group(&fx, "g", "lib/x.bin", None).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(bytes, b"from-upstream");
    assert_eq!(
        上游命中.load(Ordering::SeqCst),
        1,
        "首次经 group 应回源一次"
    );

    // ② 再取命中本地缓存，不再回源
    let (status, bytes) = get_group(&fx, "g", "lib/x.bin", None).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(bytes, b"from-upstream");
    assert_eq!(上游命中.load(Ordering::SeqCst), 1, "命中缓存不应再回源");
}
