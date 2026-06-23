//! Maven 格式的 HTTP 集成测试（FR-14/61/68）。
//!
//! 覆盖：Maven 布局直传（deploy）/ 下载（resolve）端到端、release 不可覆盖（409）、
//! SNAPSHOT 可覆盖（200）、maven-metadata.xml 允许更新、校验和 sidecar 可读且与制品摘要一致、
//! proxy cache-miss → hit（本地 mock 上游走真实 HttpUpstream 链路）、写授权边界。

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
use jianartifact::storage::LocalFsStore;

/// 测试夹具。
struct Fixture {
    state: AppState,
    _dir: tempfile::TempDir,
}

impl Fixture {
    async fn new() -> Self {
        let dir = tempfile::tempdir().unwrap();
        let meta = MetaStore::open(&dir.path().join("test.db")).await.unwrap();
        let store = LocalFsStore::new(dir.path().join("blobs")).await.unwrap();
        let jwt = JwtSigner::from_secret(b"maven-secret-32-bytes-xxxxxxxxxxx", 3600);
        let upstream = HttpUpstream::new(std::time::Duration::from_secs(60)).unwrap();
        let artifacts = Arc::new(ArtifactService::new(store.clone(), meta.clone(), upstream));
        let mut config = Config::default();
        config.server.public_base_url = Some("http://localhost:8080".to_string());
        let state = AppState {
            config: Arc::new(config),
            meta,
            store,
            jwt,
            login_guard: Arc::new(LoginGuard::new(50, 900)),
            artifacts,
            formats: Arc::new(FormatRegistry::with_builtin()),
            docker: None,
        };
        Self { state, _dir: dir }
    }

    fn router(&self) -> Router {
        build_router(self.state.clone())
    }

    async fn seed_user(&self, username: &str, password: &str, role: Role) -> String {
        let hash = auth::hash_password(password).unwrap();
        self.state.meta.create_user(username, &hash, role).await.unwrap()
    }

    /// 建一个 maven hosted 仓库，返回 id。
    async fn seed_maven_repo(&self, name: &str, visibility: Visibility) -> String {
        self.state
            .meta
            .create_repository(NewRepository {
                name,
                format: "maven",
                r#type: RepoType::Hosted,
                visibility,
                upstream_url: None,
                upstream_auth_ref: None,
            })
            .await
            .unwrap()
    }

    /// 建一个 maven proxy 仓库（指向给定上游基址），返回 id。
    async fn seed_maven_proxy_repo(
        &self,
        name: &str,
        visibility: Visibility,
        upstream_url: &str,
    ) -> String {
        self.state
            .meta
            .create_repository(NewRepository {
                name,
                format: "maven",
                r#type: RepoType::Proxy,
                visibility,
                upstream_url: Some(upstream_url),
                upstream_auth_ref: None,
            })
            .await
            .unwrap()
    }

    async fn seed_acl(&self, repo_id: &str, user_id: &str, permission: Permission) {
        self.state.meta.create_acl(repo_id, user_id, permission).await.unwrap();
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

async fn send(router: Router, req: Request<Body>) -> (StatusCode, Value) {
    let resp = router.oneshot(req).await.unwrap();
    let status = resp.status();
    let bytes = resp.into_body().collect().await.unwrap().to_bytes();
    let body = serde_json::from_slice(&bytes).unwrap_or(Value::Null);
    (status, body)
}

async fn send_bytes(router: Router, req: Request<Body>) -> (StatusCode, Vec<u8>) {
    let resp = router.oneshot(req).await.unwrap();
    let status = resp.status();
    let bytes = resp.into_body().collect().await.unwrap().to_bytes().to_vec();
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

// ---------- FR-14 Maven deploy / resolve 端到端 ----------

#[tokio::test]
async fn maven_release_deploy_后可解析下载且内容一致() {
    let fx = Fixture::new().await;
    let writer = fx.seed_user("writer", "pw", Role::User).await;
    let rid = fx.seed_maven_repo("maven-hosted", Visibility::Public).await;
    fx.seed_acl(&rid, &writer, Permission::Write).await;
    let auth = format!("Bearer {}", fx.login_token("writer", "pw").await);

    // deploy：PUT jar + pom 到 Maven 布局路径
    let jar_path = "/maven-hosted/com/example/lib/1.0/lib-1.0.jar";
    let (status, _) = send(
        fx.router(),
        raw_req("PUT", jar_path, Some(&auth), b"jar-bytes".to_vec()),
    )
    .await;
    assert_eq!(status, StatusCode::CREATED);

    let pom = b"<project><modelVersion>4.0.0</modelVersion></project>".to_vec();
    let (status, _) = send(
        fx.router(),
        raw_req(
            "PUT",
            "/maven-hosted/com/example/lib/1.0/lib-1.0.pom",
            Some(&auth),
            pom.clone(),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::CREATED);

    // resolve：匿名 GET（public 仓库）应取回原始字节
    let (status, bytes) = send_bytes(fx.router(), empty_req("GET", jar_path, None)).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(bytes, b"jar-bytes");
}

#[tokio::test]
async fn maven_release_重复_deploy_返回_409() {
    let fx = Fixture::new().await;
    let writer = fx.seed_user("writer", "pw", Role::User).await;
    let rid = fx.seed_maven_repo("maven-hosted", Visibility::Public).await;
    fx.seed_acl(&rid, &writer, Permission::Write).await;
    let auth = format!("Bearer {}", fx.login_token("writer", "pw").await);

    let path = "/maven-hosted/com/example/lib/1.0/lib-1.0.jar";
    let (s1, _) = send(
        fx.router(),
        raw_req("PUT", path, Some(&auth), b"v1".to_vec()),
    )
    .await;
    assert_eq!(s1, StatusCode::CREATED);

    // 同 GAV release 重复部署 → 409 Conflict（不可覆盖）
    let (s2, _) = send(
        fx.router(),
        raw_req("PUT", path, Some(&auth), b"v2".to_vec()),
    )
    .await;
    assert_eq!(s2, StatusCode::CONFLICT);

    // 原内容未被改动
    let (_, bytes) = send_bytes(fx.router(), empty_req("GET", path, None)).await;
    assert_eq!(bytes, b"v1");
}

#[tokio::test]
async fn maven_snapshot_可重复_deploy_覆盖() {
    let fx = Fixture::new().await;
    let writer = fx.seed_user("writer", "pw", Role::User).await;
    let rid = fx.seed_maven_repo("maven-hosted", Visibility::Public).await;
    fx.seed_acl(&rid, &writer, Permission::Write).await;
    let auth = format!("Bearer {}", fx.login_token("writer", "pw").await);

    let path = "/maven-hosted/com/example/lib/1.0-SNAPSHOT/lib-1.0-SNAPSHOT.jar";
    let (s1, _) = send(
        fx.router(),
        raw_req("PUT", path, Some(&auth), b"snap-1".to_vec()),
    )
    .await;
    assert_eq!(s1, StatusCode::CREATED);

    // SNAPSHOT 覆盖允许 → 200
    let (s2, _) = send(
        fx.router(),
        raw_req("PUT", path, Some(&auth), b"snap-2-new".to_vec()),
    )
    .await;
    assert_eq!(s2, StatusCode::OK);

    let (_, bytes) = send_bytes(fx.router(), empty_req("GET", path, None)).await;
    assert_eq!(bytes, b"snap-2-new");
}

#[tokio::test]
async fn maven_metadata_允许更新() {
    let fx = Fixture::new().await;
    let writer = fx.seed_user("writer", "pw", Role::User).await;
    let rid = fx.seed_maven_repo("maven-hosted", Visibility::Public).await;
    fx.seed_acl(&rid, &writer, Permission::Write).await;
    let auth = format!("Bearer {}", fx.login_token("writer", "pw").await);

    let path = "/maven-hosted/com/example/lib/maven-metadata.xml";
    let (s1, _) = send(
        fx.router(),
        raw_req("PUT", path, Some(&auth), b"<metadata>v1</metadata>".to_vec()),
    )
    .await;
    assert_eq!(s1, StatusCode::CREATED);

    // maven-metadata.xml 允许更新 → 200
    let (s2, _) = send(
        fx.router(),
        raw_req("PUT", path, Some(&auth), b"<metadata>v2</metadata>".to_vec()),
    )
    .await;
    assert_eq!(s2, StatusCode::OK);
}

#[tokio::test]
async fn maven_sidecar_校验和与制品摘要一致() {
    let fx = Fixture::new().await;
    let writer = fx.seed_user("writer", "pw", Role::User).await;
    let rid = fx.seed_maven_repo("maven-hosted", Visibility::Public).await;
    fx.seed_acl(&rid, &writer, Permission::Write).await;
    let auth = format!("Bearer {}", fx.login_token("writer", "pw").await);

    // 部署 jar 主文件
    let jar = b"jar-content".to_vec();
    let jar_path = "/maven-hosted/com/example/lib/1.0/lib-1.0.jar";
    send(
        fx.router(),
        raw_req("PUT", jar_path, Some(&auth), jar.clone()),
    )
    .await;

    // 客户端独立计算 sha1，作为 sidecar 内容上传（模拟 mvn 行为）
    let sha1 = {
        use sha1::Digest;
        let mut h = sha1::Sha1::new();
        h.update(&jar);
        format!("{:x}", h.finalize())
    };
    let (s1, _) = send(
        fx.router(),
        raw_req(
            "PUT",
            "/maven-hosted/com/example/lib/1.0/lib-1.0.jar.sha1",
            Some(&auth),
            sha1.clone().into_bytes(),
        ),
    )
    .await;
    assert_eq!(s1, StatusCode::CREATED);

    // GET sidecar 取回，内容应即客户端计算的 sha1
    let (status, bytes) = send_bytes(
        fx.router(),
        empty_req("GET", "/maven-hosted/com/example/lib/1.0/lib-1.0.jar.sha1", None),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(String::from_utf8(bytes).unwrap(), sha1);

    // 制品详情里 jar 的 sha1 应与 sidecar 一致（服务端边写边算）
    let (status, detail) = send(
        fx.router(),
        empty_req(
            "GET",
            &format!("/api/v1/repositories/{rid}/artifacts/com/example/lib/1.0/lib-1.0.jar"),
            None,
        ),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(detail["checksums"]["sha1"], sha1);
    assert_eq!(detail["format"], "maven");
}

// ---------- FR-68 制品详情：Maven 使用片段 ----------

#[tokio::test]
async fn maven_制品详情含依赖坐标片段() {
    let fx = Fixture::new().await;
    let writer = fx.seed_user("writer", "pw", Role::User).await;
    let rid = fx.seed_maven_repo("maven-hosted", Visibility::Public).await;
    fx.seed_acl(&rid, &writer, Permission::Write).await;
    let auth = format!("Bearer {}", fx.login_token("writer", "pw").await);

    send(
        fx.router(),
        raw_req(
            "PUT",
            "/maven-hosted/com/example/foo/1.2.3/foo-1.2.3.jar",
            Some(&auth),
            b"x".to_vec(),
        ),
    )
    .await;

    let (status, detail) = send(
        fx.router(),
        empty_req(
            "GET",
            &format!("/api/v1/repositories/{rid}/artifacts/com/example/foo/1.2.3/foo-1.2.3.jar"),
            None,
        ),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    let usage = detail["usage"].as_array().unwrap();
    // 依赖坐标片段含 GAV
    assert!(usage.iter().any(|u| {
        let c = u["content"].as_str().unwrap();
        c.contains("<groupId>com.example</groupId>")
            && c.contains("<artifactId>foo</artifactId>")
            && c.contains("<version>1.2.3</version>")
    }));
    // 仓库接入片段含本仓库 URL
    assert!(usage
        .iter()
        .any(|u| u["content"].as_str().unwrap().contains("http://localhost:8080/maven-hosted")));
}

// ---------- FR-09 写授权边界（Maven 端点） ----------

#[tokio::test]
async fn maven_无写权限_deploy_被拒_403() {
    let fx = Fixture::new().await;
    fx.seed_maven_repo("pub-maven", Visibility::Public).await;
    // 匿名对 public 有读无写 → 403
    let (status, _) = send(
        fx.router(),
        raw_req(
            "PUT",
            "/pub-maven/com/example/lib/1.0/lib-1.0.jar",
            None,
            b"data".to_vec(),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::FORBIDDEN);
}

#[tokio::test]
async fn maven_私有仓库对无权者_404_隐藏存在性() {
    let fx = Fixture::new().await;
    fx.seed_maven_repo("secret-maven", Visibility::Private).await;
    // 匿名读 private → 404（不泄露存在）
    let (status, _) = send(
        fx.router(),
        empty_req("GET", "/secret-maven/com/example/lib/1.0/lib-1.0.jar", None),
    )
    .await;
    assert_eq!(status, StatusCode::NOT_FOUND);
}

// ---------- FR-12 Maven proxy 代理缓存：cache-miss → hit（真实 HttpUpstream 链路） ----------

/// 启动一个本地 mock 上游服务，按请求路径返回固定内容并记录命中次数。
async fn 启动_mock_上游(content: &'static [u8]) -> (String, Arc<std::sync::atomic::AtomicUsize>) {
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
async fn maven_proxy_cache_miss_回源后命中不再回源() {
    use std::sync::atomic::Ordering;

    let (上游基址, 上游命中) = 启动_mock_上游(b"upstream-jar").await;
    let fx = Fixture::new().await;
    fx.seed_maven_proxy_repo("central-mirror", Visibility::Public, &上游基址)
        .await;

    // ① cache-miss：匿名 GET 触发回源（按 Maven 布局路径）
    let path = "/central-mirror/org/apache/commons/commons-lang3/3.12.0/commons-lang3-3.12.0.jar";
    let (status, bytes) = send_bytes(fx.router(), empty_req("GET", path, None)).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(bytes, b"upstream-jar");
    assert_eq!(上游命中.load(Ordering::SeqCst), 1, "首次应回源一次");

    // ② cache-hit：再取命中本地缓存，不再回源
    let (status, bytes) = send_bytes(fx.router(), empty_req("GET", path, None)).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(bytes, b"upstream-jar");
    assert_eq!(上游命中.load(Ordering::SeqCst), 1, "命中缓存不应再回源");
}
