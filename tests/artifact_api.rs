//! 制品机理与 Raw 格式的 HTTP 集成测试（FR-11/17/60/61/62/64/66/67/68/69）。
//!
//! 覆盖：Raw 直传 / 下载 / 覆盖 / 删除端到端、制品详情四校验和 + 使用片段、
//! 上传超限 413、跨仓库搜索的读权限过滤（§2.1 高风险：匿名 / 无权用户搜私有仓库
//! 结果 / 计数 / 错误均不泄露其存在），以及格式端点的写授权边界与路径穿越拒绝。

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
use jianartifact::format::{ArtifactService, DockerRegistry, FormatRegistry};
use jianartifact::meta::{MetaStore, NewRepository, Permission, RepoType, Role, Visibility};
use jianartifact::proxy::HttpUpstream;
use jianartifact::storage::{BlobBackend, LocalFsStore};

/// 测试夹具。
struct Fixture {
    state: AppState,
    _dir: tempfile::TempDir,
}

impl Fixture {
    /// 默认上限不限；走真实 SQLite 文件。
    async fn new() -> Self {
        Self::with_max_size(None).await
    }

    /// 指定上传大小上限构造夹具。
    async fn with_max_size(max: Option<u64>) -> Self {
        let dir = tempfile::tempdir().unwrap();
        let meta = MetaStore::open(&dir.path().join("test.db")).await.unwrap();
        let store = BlobBackend::Fs(LocalFsStore::new(dir.path().join("blobs")).await.unwrap());
        let jwt = JwtSigner::from_secret(b"artifact-secret-32-bytes-xxxxxxxx", 3600);
        let upstream = HttpUpstream::new(std::time::Duration::from_secs(60)).unwrap();
        let artifacts = Arc::new(ArtifactService::new(store.clone(), meta.clone(), upstream));
        let docker = Arc::new(
            DockerRegistry::new(store.clone(), meta.clone(), dir.path().join("uploads"), max)
                .await
                .unwrap(),
        );
        let mut config = Config::default();
        config.limits.max_artifact_size = max;
        // 固定对外地址，便于断言使用片段
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
            docker: Some(docker),
            audit,
            usage,
            metrics: None,
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

    /// 建一个 raw hosted 仓库，返回 id。
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

    /// 建一个 raw proxy 仓库（指向给定上游基址），返回 id。
    async fn seed_raw_proxy_repo(
        &self,
        name: &str,
        visibility: Visibility,
        upstream_url: &str,
    ) -> String {
        self.state
            .meta
            .create_repository(NewRepository {
                name,
                format: "raw",
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

// ---------- FR-57 使用分析采集端到端 ----------

/// 轮询等待聚合下载计数达到期望值（写入任务异步落库，不依赖固定睡眠时长）。
async fn 等待下载计数(meta: &MetaStore, repo: &str, path: &str, want: i64) -> i64 {
    use jianartifact::meta::UsageAction;
    let mut got = 0;
    for _ in 0..100 {
        got = meta
            .usage_count(repo, path, UsageAction::Download)
            .await
            .unwrap();
        if got >= want {
            break;
        }
        tokio::time::sleep(std::time::Duration::from_millis(20)).await;
    }
    got
}

#[tokio::test]
async fn 下载成功被聚合计数_匿名与登录共计() {
    let fx = Fixture::new().await;
    let writer = fx.seed_user("writer", "pw", Role::User).await;
    let rid = fx.seed_raw_repo("files", Visibility::Public).await;
    fx.seed_acl(&rid, &writer, Permission::Write).await;
    let auth = format!("Bearer {}", fx.login_token("writer", "pw").await);

    // 上传一个制品
    let (status, _) = send(
        fx.router(),
        raw_req("PUT", "/files/a/b.txt", Some(&auth), b"data".to_vec()),
    )
    .await;
    assert_eq!(status, StatusCode::CREATED);

    // 公开仓库匿名下载 2 次 + 登录下载 1 次，聚合应累加为 3
    for _ in 0..2 {
        let (s, _) = send_bytes(fx.router(), empty_req("GET", "/files/a/b.txt", None)).await;
        assert_eq!(s, StatusCode::OK);
    }
    let (s, _) = send_bytes(fx.router(), empty_req("GET", "/files/a/b.txt", Some(&auth))).await;
    assert_eq!(s, StatusCode::OK);

    let got = 等待下载计数(&fx.state.meta, "files", "a/b.txt", 3).await;
    assert_eq!(got, 3, "匿名 + 登录的下载应聚合累加");
}

#[tokio::test]
async fn 无权访问私有仓库不计入下载统计() {
    // §2.1 高风险：私有仓库对无权者一律 404；这类被拒访问不得计入使用统计（不泄露存在性）
    let fx = Fixture::new().await;
    let writer = fx.seed_user("writer", "pw", Role::User).await;
    let rid = fx.seed_raw_repo("secret", Visibility::Private).await;
    fx.seed_acl(&rid, &writer, Permission::Write).await;
    let auth = format!("Bearer {}", fx.login_token("writer", "pw").await);
    // 写者上传一个私有制品
    let (status, _) = send(
        fx.router(),
        raw_req("PUT", "/secret/k.txt", Some(&auth), b"top".to_vec()),
    )
    .await;
    assert_eq!(status, StatusCode::CREATED);

    // 匿名访问私有制品被拒 404
    let (s, _) = send(fx.router(), empty_req("GET", "/secret/k.txt", None)).await;
    assert_eq!(s, StatusCode::NOT_FOUND);

    // 给异步采集留出时间窗后，被拒访问不应产生任何下载计数
    tokio::time::sleep(std::time::Duration::from_millis(300)).await;
    let got = 等待下载计数(&fx.state.meta, "secret", "k.txt", 1).await;
    assert_eq!(got, 0, "被拒的私有访问不得计入统计");
}

// ---------- FR-11/17 Raw 直传与下载端到端 ----------

#[tokio::test]
async fn raw_直传后下载内容一致() {
    let fx = Fixture::new().await;
    let writer = fx.seed_user("writer", "pw", Role::User).await;
    let rid = fx.seed_raw_repo("files", Visibility::Public).await;
    fx.seed_acl(&rid, &writer, Permission::Write).await;
    let auth = format!("Bearer {}", fx.login_token("writer", "pw").await);

    // PUT 上传
    let (status, _) = send(
        fx.router(),
        raw_req(
            "PUT",
            "/files/docs/readme.txt",
            Some(&auth),
            b"hello raw".to_vec(),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::CREATED);

    // GET 下载（公开仓库匿名亦可读）
    let (status, bytes) = send_bytes(
        fx.router(),
        empty_req("GET", "/files/docs/readme.txt", None),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(bytes, b"hello raw");
}

#[tokio::test]
async fn raw_覆盖允许返回_200() {
    let fx = Fixture::new().await;
    let writer = fx.seed_user("writer", "pw", Role::User).await;
    let rid = fx.seed_raw_repo("files", Visibility::Public).await;
    fx.seed_acl(&rid, &writer, Permission::Write).await;
    let auth = format!("Bearer {}", fx.login_token("writer", "pw").await);

    let (s1, _) = send(
        fx.router(),
        raw_req("PUT", "/files/a.bin", Some(&auth), b"v1".to_vec()),
    )
    .await;
    assert_eq!(s1, StatusCode::CREATED);
    // 覆盖返回 200
    let (s2, _) = send(
        fx.router(),
        raw_req("PUT", "/files/a.bin", Some(&auth), b"v2-new".to_vec()),
    )
    .await;
    assert_eq!(s2, StatusCode::OK);
    let (_, bytes) = send_bytes(fx.router(), empty_req("GET", "/files/a.bin", None)).await;
    assert_eq!(bytes, b"v2-new");
}

#[tokio::test]
async fn raw_删除后下载_404() {
    let fx = Fixture::new().await;
    let writer = fx.seed_user("writer", "pw", Role::User).await;
    let rid = fx.seed_raw_repo("files", Visibility::Public).await;
    fx.seed_acl(&rid, &writer, Permission::Write).await;
    let auth = format!("Bearer {}", fx.login_token("writer", "pw").await);

    send(
        fx.router(),
        raw_req("PUT", "/files/d.txt", Some(&auth), b"bye".to_vec()),
    )
    .await;
    // DELETE
    let resp = fx
        .router()
        .oneshot(empty_req("DELETE", "/files/d.txt", Some(&auth)))
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::NO_CONTENT);
    // 删后下载 404
    let (status, _) = send(fx.router(), empty_req("GET", "/files/d.txt", None)).await;
    assert_eq!(status, StatusCode::NOT_FOUND);
}

// ---------- FR-09 写授权边界（格式端点） ----------

#[tokio::test]
async fn 无写权限上传被拒() {
    let fx = Fixture::new().await;
    // public 仓库：匿名可读但无写 → 写应 403
    fx.seed_raw_repo("pubfiles", Visibility::Public).await;
    let (status, _) = send(
        fx.router(),
        raw_req("PUT", "/pubfiles/x.txt", None, b"data".to_vec()),
    )
    .await;
    // 匿名对 public 有读无写 → 403
    assert_eq!(status, StatusCode::FORBIDDEN);

    // 仅读 ACL 用户写 private → 也应 403（有读无写）
    let reader = fx.seed_user("reader", "pw", Role::User).await;
    let pid = fx.seed_raw_repo("privfiles", Visibility::Private).await;
    fx.seed_acl(&pid, &reader, Permission::Read).await;
    let auth = format!("Bearer {}", fx.login_token("reader", "pw").await);
    let (status, _) = send(
        fx.router(),
        raw_req("PUT", "/privfiles/x.txt", Some(&auth), b"d".to_vec()),
    )
    .await;
    assert_eq!(status, StatusCode::FORBIDDEN);
}

#[tokio::test]
async fn 私有仓库对无权者格式端点_404_隐藏存在性() {
    let fx = Fixture::new().await;
    fx.seed_user("outsider", "pw", Role::User).await;
    fx.seed_raw_repo("secret", Visibility::Private).await;

    // 匿名读 private → 404（不泄露存在）
    let (status, _) = send(fx.router(), empty_req("GET", "/secret/x.txt", None)).await;
    assert_eq!(status, StatusCode::NOT_FOUND);

    // 无 ACL 登录用户写 private → 404（先过读判定隐藏存在性，不暴露为 403）
    let auth = format!("Bearer {}", fx.login_token("outsider", "pw").await);
    let (status, _) = send(
        fx.router(),
        raw_req("PUT", "/secret/x.txt", Some(&auth), b"d".to_vec()),
    )
    .await;
    assert_eq!(status, StatusCode::NOT_FOUND);
}

#[tokio::test]
async fn 路径穿越被拒_400() {
    let fx = Fixture::new().await;
    let writer = fx.seed_user("writer", "pw", Role::User).await;
    let rid = fx.seed_raw_repo("files", Visibility::Public).await;
    fx.seed_acl(&rid, &writer, Permission::Write).await;
    let auth = format!("Bearer {}", fx.login_token("writer", "pw").await);
    // 含 .. 的路径应被拒
    let (status, _) = send(
        fx.router(),
        raw_req(
            "PUT",
            "/files/a/../../etc/passwd",
            Some(&auth),
            b"x".to_vec(),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::BAD_REQUEST);
}

// ---------- FR-64 上传大小限制 413 ----------

#[tokio::test]
async fn 上传超过上限返回_413() {
    let fx = Fixture::with_max_size(Some(4)).await;
    let writer = fx.seed_user("writer", "pw", Role::User).await;
    let rid = fx.seed_raw_repo("files", Visibility::Public).await;
    fx.seed_acl(&rid, &writer, Permission::Write).await;
    let auth = format!("Bearer {}", fx.login_token("writer", "pw").await);

    let (status, _) = send(
        fx.router(),
        raw_req("PUT", "/files/big.bin", Some(&auth), b"0123456789".to_vec()),
    )
    .await;
    assert_eq!(status, StatusCode::PAYLOAD_TOO_LARGE);
    // 超限不留索引
    let (status, _) = send(fx.router(), empty_req("GET", "/files/big.bin", None)).await;
    assert_eq!(status, StatusCode::NOT_FOUND);
}

// ---------- FR-66/68/69 制品详情：四校验和 + 使用片段 ----------

#[tokio::test]
async fn 制品详情含四校验和与使用片段() {
    let fx = Fixture::new().await;
    let writer = fx.seed_user("writer", "pw", Role::User).await;
    let rid = fx.seed_raw_repo("files", Visibility::Public).await;
    fx.seed_acl(&rid, &writer, Permission::Write).await;
    let auth = format!("Bearer {}", fx.login_token("writer", "pw").await);
    send(
        fx.router(),
        raw_req("PUT", "/files/dir/x.txt", Some(&auth), b"abc".to_vec()),
    )
    .await;

    // 详情（匿名读 public）
    let (status, detail) = send(
        fx.router(),
        empty_req(
            "GET",
            &format!("/api/v1/repositories/{rid}/artifacts/dir/x.txt"),
            None,
        ),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(detail["path"], "dir/x.txt");
    assert_eq!(detail["format"], "raw");
    // "abc" 四校验和标准向量
    assert_eq!(
        detail["checksums"]["sha256"],
        "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
    );
    assert_eq!(
        detail["checksums"]["sha1"],
        "a9993e364706816aba3e25717850c26c9cd0d89d"
    );
    assert_eq!(
        detail["checksums"]["md5"],
        "900150983cd24fb0d6963f7d28e17f72"
    );
    assert!(detail["checksums"]["sha512"].as_str().unwrap().len() == 128);
    // 使用片段非空且含完整 URL
    let usage = detail["usage"].as_array().unwrap();
    assert!(!usage.is_empty());
    assert!(usage.iter().any(|u| u["content"]
        .as_str()
        .unwrap()
        .contains("http://localhost:8080/files/dir/x.txt")));
}

#[tokio::test]
async fn 制品详情对无权私有仓库_404() {
    let fx = Fixture::new().await;
    fx.seed_user("outsider", "pw", Role::User).await;
    let rid = fx.seed_raw_repo("secret", Visibility::Private).await;
    // 直接在库里放一条索引（绕过 API 写）
    fx.state
        .meta
        .upsert_artifact(jianartifact::meta::NewArtifact {
            repo_id: &rid,
            path: "s.txt",
            size: 1,
            sha256: "x",
            sha1: "x",
            md5: "x",
            sha512: "x",
            content_type: None,
            cached: false,
        })
        .await
        .unwrap();

    // 匿名详情 → 404
    let (status, _) = send(
        fx.router(),
        empty_req(
            "GET",
            &format!("/api/v1/repositories/{rid}/artifacts/s.txt"),
            None,
        ),
    )
    .await;
    assert_eq!(status, StatusCode::NOT_FOUND);
    // 无权登录用户详情 → 404
    let auth = format!("Bearer {}", fx.login_token("outsider", "pw").await);
    let (status, _) = send(
        fx.router(),
        empty_req(
            "GET",
            &format!("/api/v1/repositories/{rid}/artifacts/s.txt"),
            Some(&auth),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::NOT_FOUND);
}

// ---------- FR-67 跨仓库搜索：读权限过滤（§2.1 高风险） ----------

#[tokio::test]
async fn 搜索结果按读权限过滤不泄露无权私有() {
    let fx = Fixture::new().await;
    let alice = fx.seed_user("alice", "pw", Role::User).await;
    fx.seed_user("admin", "pw", Role::Admin).await;
    let pub_id = fx.seed_raw_repo("pub", Visibility::Public).await;
    let priv_a = fx.seed_raw_repo("priv-a", Visibility::Private).await;
    let priv_b = fx.seed_raw_repo("priv-b", Visibility::Private).await;
    // 三个仓库各放一条都含关键字 "lib" 的制品
    for (rid, path) in [
        (&pub_id, "lib-public.txt"),
        (&priv_a, "lib-a.txt"),
        (&priv_b, "lib-b.txt"),
    ] {
        fx.state
            .meta
            .upsert_artifact(jianartifact::meta::NewArtifact {
                repo_id: rid,
                path,
                size: 1,
                sha256: "x",
                sha1: "x",
                md5: "x",
                sha512: "x",
                content_type: None,
                cached: false,
            })
            .await
            .unwrap();
    }
    // alice 仅对 priv-a 有读权限
    fx.seed_acl(&priv_a, &alice, Permission::Read).await;

    // 匿名搜索：仅见 public 那条，total=1，绝不含 priv-a / priv-b
    let (status, anon) = send(fx.router(), empty_req("GET", "/api/v1/search?q=lib", None)).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(anon["total"], 1);
    let anon_paths: Vec<&str> = anon["items"]
        .as_array()
        .unwrap()
        .iter()
        .map(|i| i["path"].as_str().unwrap())
        .collect();
    assert_eq!(anon_paths, vec!["lib-public.txt"]);

    // alice 搜索：见 public + priv-a，不见 priv-b，total=2
    let alice_auth = format!("Bearer {}", fx.login_token("alice", "pw").await);
    let (_, alist) = send(
        fx.router(),
        empty_req("GET", "/api/v1/search?q=lib", Some(&alice_auth)),
    )
    .await;
    assert_eq!(alist["total"], 2);
    let mut alice_paths: Vec<&str> = alist["items"]
        .as_array()
        .unwrap()
        .iter()
        .map(|i| i["path"].as_str().unwrap())
        .collect();
    alice_paths.sort();
    assert_eq!(alice_paths, vec!["lib-a.txt", "lib-public.txt"]);
    // 明确断言无权 private 不出现
    assert!(!alice_paths.contains(&"lib-b.txt"));

    // 管理员搜索：见全部三条
    let admin_auth = format!("Bearer {}", fx.login_token("admin", "pw").await);
    let (_, adm) = send(
        fx.router(),
        empty_req("GET", "/api/v1/search?q=lib", Some(&admin_auth)),
    )
    .await;
    assert_eq!(adm["total"], 3);
}

#[tokio::test]
async fn 搜索空关键字_400() {
    let fx = Fixture::new().await;
    let (status, _) = send(fx.router(), empty_req("GET", "/api/v1/search?q=", None)).await;
    assert_eq!(status, StatusCode::BAD_REQUEST);
}

#[tokio::test]
async fn 搜索分页结构正确() {
    let fx = Fixture::new().await;
    let pub_id = fx.seed_raw_repo("pub", Visibility::Public).await;
    for i in 0..3 {
        fx.state
            .meta
            .upsert_artifact(jianartifact::meta::NewArtifact {
                repo_id: &pub_id,
                path: &format!("pkg-{i}.txt"),
                size: 1,
                sha256: "x",
                sha1: "x",
                md5: "x",
                sha512: "x",
                content_type: None,
                cached: false,
            })
            .await
            .unwrap();
    }
    // limit=2 → 首页 2 条、has_more=true、total=3
    let (status, page) = send(
        fx.router(),
        empty_req("GET", "/api/v1/search?q=pkg&limit=2", None),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(page["total"], 3);
    assert_eq!(page["items"].as_array().unwrap().len(), 2);
    assert_eq!(page["has_more"], true);
    assert_eq!(page["limit"], 2);
}

// ---------- FR-12 proxy 代理缓存：cache-miss → hit 端到端（真实上游 + 真实 HttpUpstream） ----------

/// 启动一个本地 mock 上游服务（真实 TCP 监听，走 reqwest/HttpUpstream 真链路）。
///
/// 返回 (上游基址 `http://127.0.0.1:PORT`, 命中计数器)。计数器记录上游被实际拉取的次数，
/// 供断言"缓存命中不回源""删缓存后可重拉"。服务返回固定内容，路径不关心。
async fn 启动_mock_上游(
    content: &'static [u8],
) -> (String, Arc<std::sync::atomic::AtomicUsize>) {
    use std::sync::atomic::Ordering;

    let calls = Arc::new(std::sync::atomic::AtomicUsize::new(0));
    let calls_in_handler = calls.clone();
    // 任意路径都返回固定内容，并把命中计数 +1
    let app = Router::new().fallback(move || {
        let calls = calls_in_handler.clone();
        async move {
            calls.fetch_add(1, Ordering::SeqCst);
            content
        }
    });

    // 绑定到本机 ephemeral 端口，取回真实地址
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    tokio::spawn(async move {
        axum::serve(listener, app).await.unwrap();
    });
    (format!("http://{addr}"), calls)
}

#[tokio::test]
async fn proxy_cache_miss_回源后命中再删缓存可重拉() {
    use std::sync::atomic::Ordering;

    let (上游基址, 上游命中) = 启动_mock_上游(b"from-upstream").await;
    let fx = Fixture::new().await;
    let writer = fx.seed_user("writer", "pw", Role::User).await;
    // 公开 proxy 仓库，匿名亦可读
    let rid = fx
        .seed_raw_proxy_repo("mirror", Visibility::Public, &上游基址)
        .await;
    // 删缓存需写权限：给 writer 授写
    fx.seed_acl(&rid, &writer, Permission::Write).await;
    let auth = format!("Bearer {}", fx.login_token("writer", "pw").await);

    // ① cache-miss：匿名 GET 触发回源，内容来自上游
    let (status, bytes) =
        send_bytes(fx.router(), empty_req("GET", "/mirror/lib/x.bin", None)).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(bytes, b"from-upstream");
    assert_eq!(上游命中.load(Ordering::SeqCst), 1, "首次应回源一次");

    // ② cache-hit：再取命中本地缓存，不再回源（计数仍为 1）
    let (status, bytes) =
        send_bytes(fx.router(), empty_req("GET", "/mirror/lib/x.bin", None)).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(bytes, b"from-upstream");
    assert_eq!(上游命中.load(Ordering::SeqCst), 1, "命中缓存不应再回源");

    // ③ 删缓存（写权限）：proxy 删本地缓存，下次可重拉
    let resp = fx
        .router()
        .oneshot(empty_req("DELETE", "/mirror/lib/x.bin", Some(&auth)))
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::NO_CONTENT);

    // ④ 删后再取：重新回源（计数 +1 → 2）
    let (status, bytes) =
        send_bytes(fx.router(), empty_req("GET", "/mirror/lib/x.bin", None)).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(bytes, b"from-upstream");
    assert_eq!(上游命中.load(Ordering::SeqCst), 2, "删缓存后应可重新回源");
}

#[tokio::test]
async fn proxy_上游不可用回退_502_且不缓存() {
    // 指向一个不存在的上游地址（无人监听），回源应失败回退为 502，且不写缓存
    let fx = Fixture::new().await;
    // 127.0.0.1:1 几乎不可能有服务监听，触发连接失败
    let rid = fx
        .seed_raw_proxy_repo("dead", Visibility::Public, "http://127.0.0.1:1")
        .await;

    let (status, _) = send(fx.router(), empty_req("GET", "/dead/x.bin", None)).await;
    assert_eq!(status, StatusCode::BAD_GATEWAY);
    // 上游失败不应写入任何缓存索引
    assert!(fx
        .state
        .meta
        .get_artifact(&rid, "x.bin")
        .await
        .unwrap()
        .is_none());
}
