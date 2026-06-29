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
use jianartifact::storage::{BlobBackend, LocalFsStore};

/// 测试夹具。
struct Fixture {
    state: AppState,
    _dir: tempfile::TempDir,
}

impl Fixture {
    async fn new() -> Self {
        let dir = tempfile::tempdir().unwrap();
        let meta = MetaStore::open(&dir.path().join("test.db")).await.unwrap();
        let store = BlobBackend::Fs(LocalFsStore::new(dir.path().join("blobs")).await.unwrap());
        let jwt = JwtSigner::from_secret(b"maven-secret-32-bytes-xxxxxxxxxxx", 3600);
        let upstream = HttpUpstream::new(std::time::Duration::from_secs(60)).unwrap();
        let artifacts = Arc::new(ArtifactService::new(store.clone(), meta.clone(), upstream));
        let mut config = Config::default();
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
        raw_req(
            "PUT",
            path,
            Some(&auth),
            b"<metadata>v1</metadata>".to_vec(),
        ),
    )
    .await;
    assert_eq!(s1, StatusCode::CREATED);

    // maven-metadata.xml 允许更新 → 200
    let (s2, _) = send(
        fx.router(),
        raw_req(
            "PUT",
            path,
            Some(&auth),
            b"<metadata>v2</metadata>".to_vec(),
        ),
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
        empty_req(
            "GET",
            "/maven-hosted/com/example/lib/1.0/lib-1.0.jar.sha1",
            None,
        ),
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
    assert!(usage.iter().any(|u| u["content"]
        .as_str()
        .unwrap()
        .contains("http://localhost:8080/maven-hosted")));
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
    fx.seed_maven_repo("secret-maven", Visibility::Private)
        .await;
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

// ---------- FR-121 服务端权威 maven-metadata.xml + pom 三级兜底（ADR-0037） ----------

/// 构造一份最小 jar（zip）：可选在标准 META-INF/maven 路径放入内嵌 pom.xml。
fn build_jar(group_id: &str, artifact_id: &str, pom_xml: Option<&str>) -> Vec<u8> {
    use std::io::Write as _;
    use zip::write::SimpleFileOptions;
    use zip::ZipWriter;
    let mut buf = Vec::new();
    {
        let cursor = std::io::Cursor::new(&mut buf);
        let mut zip = ZipWriter::new(cursor);
        let opts =
            SimpleFileOptions::default().compression_method(zip::CompressionMethod::Deflated);
        zip.start_file("a/b/Demo.class", opts).unwrap();
        zip.write_all(b"CAFEBABE").unwrap();
        if let Some(xml) = pom_xml {
            let entry = format!("META-INF/maven/{group_id}/{artifact_id}/pom.xml");
            zip.start_file(entry, opts).unwrap();
            zip.write_all(xml.as_bytes()).unwrap();
        }
        zip.finish().unwrap();
    }
    buf
}

/// multipart 上传字段。
struct Part {
    name: String,
    filename: Option<String>,
    value: Vec<u8>,
}

fn text_part(name: &str, value: &str) -> Part {
    Part {
        name: name.to_string(),
        filename: None,
        value: value.as_bytes().to_vec(),
    }
}

fn file_part(name: &str, filename: &str, value: &[u8]) -> Part {
    Part {
        name: name.to_string(),
        filename: Some(filename.to_string()),
        value: value.to_vec(),
    }
}

/// 构造 multipart/form-data 上传请求，POST 到通用上传端点（FR-73）。
fn upload_req(repo_id: &str, auth: Option<&str>, parts: &[Part]) -> Request<Body> {
    let boundary = "----JianArtifactMavenBoundary";
    let mut body: Vec<u8> = Vec::new();
    for p in parts {
        body.extend_from_slice(format!("--{boundary}\r\n").as_bytes());
        match &p.filename {
            Some(fname) => {
                body.extend_from_slice(
                    format!(
                        "Content-Disposition: form-data; name=\"{}\"; filename=\"{}\"\r\n",
                        p.name, fname
                    )
                    .as_bytes(),
                );
                body.extend_from_slice(b"Content-Type: application/octet-stream\r\n\r\n");
            }
            None => {
                body.extend_from_slice(
                    format!(
                        "Content-Disposition: form-data; name=\"{}\"\r\n\r\n",
                        p.name
                    )
                    .as_bytes(),
                );
            }
        }
        body.extend_from_slice(&p.value);
        body.extend_from_slice(b"\r\n");
    }
    body.extend_from_slice(format!("--{boundary}--\r\n").as_bytes());

    let mut builder = Request::builder()
        .method("POST")
        .uri(format!("/api/v1/repositories/{repo_id}/upload"))
        .header(
            "content-type",
            format!("multipart/form-data; boundary={boundary}"),
        );
    if let Some(a) = auth {
        builder = builder.header("authorization", a);
    }
    builder.body(Body::from(body)).unwrap()
}

#[tokio::test]
async fn maven_deploy_jar_后服务端生成_metadata() {
    let fx = Fixture::new().await;
    let writer = fx.seed_user("writer", "pw", Role::User).await;
    let rid = fx.seed_maven_repo("maven-hosted", Visibility::Public).await;
    fx.seed_acl(&rid, &writer, Permission::Write).await;
    let auth = format!("Bearer {}", fx.login_token("writer", "pw").await);

    // mvn deploy 模拟：PUT 一个 release jar
    let (status, _) = send(
        fx.router(),
        raw_req(
            "PUT",
            "/maven-hosted/com/example/lib/1.0/lib-1.0.jar",
            Some(&auth),
            b"jar-bytes".to_vec(),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::CREATED);

    // 服务端权威生成 artifact 级 maven-metadata.xml，匿名可读（public）
    let (status, bytes) = send_bytes(
        fx.router(),
        empty_req(
            "GET",
            "/maven-hosted/com/example/lib/maven-metadata.xml",
            None,
        ),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    let xml = String::from_utf8(bytes).unwrap();
    assert!(
        xml.contains("<groupId>com.example</groupId>"),
        "metadata: {xml}"
    );
    assert!(xml.contains("<artifactId>lib</artifactId>"));
    assert!(xml.contains("<latest>1.0</latest>"));
    assert!(xml.contains("<release>1.0</release>"));
    assert!(xml.contains("<version>1.0</version>"));

    // metadata 的校验和 sidecar 一并生成、可读
    let (status, _) = send_bytes(
        fx.router(),
        empty_req(
            "GET",
            "/maven-hosted/com/example/lib/maven-metadata.xml.sha256",
            None,
        ),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
}

#[tokio::test]
async fn maven_deploy_多版本_metadata_聚合_latest_release() {
    let fx = Fixture::new().await;
    let writer = fx.seed_user("writer", "pw", Role::User).await;
    let rid = fx.seed_maven_repo("maven-hosted", Visibility::Public).await;
    fx.seed_acl(&rid, &writer, Permission::Write).await;
    let auth = format!("Bearer {}", fx.login_token("writer", "pw").await);

    for v in ["1.0", "2.0"] {
        let path = format!("/maven-hosted/com/example/lib/{v}/lib-{v}.jar");
        let (status, _) = send(
            fx.router(),
            raw_req("PUT", &path, Some(&auth), format!("jar-{v}").into_bytes()),
        )
        .await;
        assert_eq!(status, StatusCode::CREATED);
    }

    let (status, bytes) = send_bytes(
        fx.router(),
        empty_req(
            "GET",
            "/maven-hosted/com/example/lib/maven-metadata.xml",
            None,
        ),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    let xml = String::from_utf8(bytes).unwrap();
    // 两个版本都进聚合，latest / release 为最新部署的 2.0
    assert!(xml.contains("<version>1.0</version>"), "metadata: {xml}");
    assert!(xml.contains("<version>2.0</version>"));
    assert!(xml.contains("<latest>2.0</latest>"));
    assert!(xml.contains("<release>2.0</release>"));
}

#[tokio::test]
async fn maven_web上传jar_提取内嵌pom并生成metadata() {
    let fx = Fixture::new().await;
    let writer = fx.seed_user("writer", "pw", Role::User).await;
    let rid = fx.seed_maven_repo("maven-hosted", Visibility::Public).await;
    fx.seed_acl(&rid, &writer, Permission::Write).await;
    let auth = format!("Bearer {}", fx.login_token("writer", "pw").await);

    let pom = r#"<project><groupId>com.example</groupId><artifactId>weblib</artifactId><version>1.0</version><packaging>jar</packaging></project>"#;
    let jar = build_jar("com.example", "weblib", Some(pom));
    let parts = vec![
        text_part("group_id", "com.example"),
        text_part("artifact_id", "weblib"),
        text_part("version", "1.0"),
        file_part("file", "weblib-1.0.jar", &jar),
    ];
    let resp = fx
        .router()
        .oneshot(upload_req(&rid, Some(&auth), &parts))
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::CREATED);

    // 服务端据 jar 内嵌 pom 兜底生成 .pom，内容为内嵌 pom 原样字节
    let (status, bytes) = send_bytes(
        fx.router(),
        empty_req(
            "GET",
            "/maven-hosted/com/example/weblib/1.0/weblib-1.0.pom",
            None,
        ),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(bytes, pom.as_bytes(), "应原样落 jar 内嵌 pom");

    // 同时生成 artifact 级 metadata，含该版本
    let (status, mbytes) = send_bytes(
        fx.router(),
        empty_req(
            "GET",
            "/maven-hosted/com/example/weblib/maven-metadata.xml",
            None,
        ),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert!(String::from_utf8(mbytes)
        .unwrap()
        .contains("<version>1.0</version>"));
}

#[tokio::test]
async fn maven_web上传jar_无内嵌pom_生成最小pom() {
    let fx = Fixture::new().await;
    let writer = fx.seed_user("writer", "pw", Role::User).await;
    let rid = fx.seed_maven_repo("maven-hosted", Visibility::Public).await;
    fx.seed_acl(&rid, &writer, Permission::Write).await;
    let auth = format!("Bearer {}", fx.login_token("writer", "pw").await);

    let jar = build_jar("com.example", "plainlib", None);
    let parts = vec![
        text_part("group_id", "com.example"),
        text_part("artifact_id", "plainlib"),
        text_part("version", "2.5.0"),
        file_part("file", "plainlib-2.5.0.jar", &jar),
    ];
    let resp = fx
        .router()
        .oneshot(upload_req(&rid, Some(&auth), &parts))
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::CREATED);

    let (status, bytes) = send_bytes(
        fx.router(),
        empty_req(
            "GET",
            "/maven-hosted/com/example/plainlib/2.5.0/plainlib-2.5.0.pom",
            None,
        ),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    let pom = String::from_utf8(bytes).unwrap();
    assert!(
        pom.contains("<modelVersion>4.0.0</modelVersion>"),
        "最小 pom: {pom}"
    );
    assert!(pom.contains("<groupId>com.example</groupId>"));
    assert!(pom.contains("<artifactId>plainlib</artifactId>"));
    assert!(pom.contains("<version>2.5.0</version>"));
}

#[tokio::test]
async fn maven_deploy路径_client_pom_不被服务端兜底覆盖() {
    let fx = Fixture::new().await;
    let writer = fx.seed_user("writer", "pw", Role::User).await;
    let rid = fx.seed_maven_repo("maven-hosted", Visibility::Public).await;
    fx.seed_acl(&rid, &writer, Permission::Write).await;
    let auth = format!("Bearer {}", fx.login_token("writer", "pw").await);

    // mvn deploy 路径：先 PUT release jar（服务端不兜底 pom，client-priority）
    let (status, _) = send(
        fx.router(),
        raw_req(
            "PUT",
            "/maven-hosted/com/example/lib/1.0/lib-1.0.jar",
            Some(&auth),
            b"jar".to_vec(),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::CREATED);

    // deploy 路径不生成 pom：此时客户端尚未上传 pom，pom 应不存在（404）
    let (status, _) = send_bytes(
        fx.router(),
        empty_req("GET", "/maven-hosted/com/example/lib/1.0/lib-1.0.pom", None),
    )
    .await;
    assert_eq!(
        status,
        StatusCode::NOT_FOUND,
        "deploy 路径不应预生成 release pom"
    );

    // 客户端随后 PUT 自己的 release pom → 成功 201（未被服务端占位阻挡为 409）
    let client_pom = b"<project><client>own</client></project>".to_vec();
    let (status, _) = send(
        fx.router(),
        raw_req(
            "PUT",
            "/maven-hosted/com/example/lib/1.0/lib-1.0.pom",
            Some(&auth),
            client_pom.clone(),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::CREATED);

    // 客户端 pom 原样保留
    let (status, bytes) = send_bytes(
        fx.router(),
        empty_req("GET", "/maven-hosted/com/example/lib/1.0/lib-1.0.pom", None),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(bytes, client_pom);
}

// ---------- FR-122 完整 Maven 快照（时间戳唯一版本 + snapshot 级 metadata） ----------

/// 从 XML 中取首个 `<tag>...</tag>` 的内容（测试用简易提取）。
fn extract_tag(xml: &str, tag: &str) -> Option<String> {
    let open = format!("<{tag}>");
    let close = format!("</{tag}>");
    let start = xml.find(&open)? + open.len();
    let end = xml[start..].find(&close)? + start;
    Some(xml[start..end].to_string())
}

#[tokio::test]
async fn maven_web上传snapshot_生成时间戳版本与snapshot_metadata() {
    let fx = Fixture::new().await;
    let writer = fx.seed_user("writer", "pw", Role::User).await;
    let rid = fx.seed_maven_repo("maven-hosted", Visibility::Public).await;
    fx.seed_acl(&rid, &writer, Permission::Write).await;
    let auth = format!("Bearer {}", fx.login_token("writer", "pw").await);

    let jar = build_jar("com.example", "snaplib", None);
    let parts = vec![
        text_part("group_id", "com.example"),
        text_part("artifact_id", "snaplib"),
        text_part("version", "1.0-SNAPSHOT"),
        file_part("file", "snaplib-1.0-SNAPSHOT.jar", &jar),
    ];
    let resp = fx
        .router()
        .oneshot(upload_req(&rid, Some(&auth), &parts))
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::CREATED);

    // 快照级 maven-metadata.xml 生成，含 snapshot/timestamp/buildNumber/snapshotVersions
    let (status, bytes) = send_bytes(
        fx.router(),
        empty_req(
            "GET",
            "/maven-hosted/com/example/snaplib/1.0-SNAPSHOT/maven-metadata.xml",
            None,
        ),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    let xml = String::from_utf8(bytes).unwrap();
    assert!(
        xml.contains("<version>1.0-SNAPSHOT</version>"),
        "snapshot meta: {xml}"
    );
    assert!(xml.contains("<buildNumber>1</buildNumber>"));
    assert!(xml.contains("<snapshotVersions>"));
    let value = extract_tag(&xml, "value").expect("应有 snapshotVersion value");
    assert!(value.starts_with("1.0-"), "value 形如 1.0-<ts>-1: {value}");
    assert!(value.ends_with("-1"));

    // 时间戳唯一构件按 value 落库、可下载、字节与上传一致
    let ts_jar = format!("/maven-hosted/com/example/snaplib/1.0-SNAPSHOT/snaplib-{value}.jar");
    let (status, jbytes) = send_bytes(fx.router(), empty_req("GET", &ts_jar, None)).await;
    assert_eq!(status, StatusCode::OK, "时间戳构件应可下载: {ts_jar}");
    assert_eq!(jbytes, jar);

    // 同名 pom 也按时间戳唯一名生成（最小 pom）
    let ts_pom = format!("/maven-hosted/com/example/snaplib/1.0-SNAPSHOT/snaplib-{value}.pom");
    let (status, pbytes) = send_bytes(fx.router(), empty_req("GET", &ts_pom, None)).await;
    assert_eq!(status, StatusCode::OK, "时间戳 pom 应生成: {ts_pom}");
    assert!(String::from_utf8(pbytes)
        .unwrap()
        .contains("<version>1.0-SNAPSHOT</version>"));

    // artifact 级 metadata 把 1.0-SNAPSHOT 列为一个版本
    let (status, abytes) = send_bytes(
        fx.router(),
        empty_req(
            "GET",
            "/maven-hosted/com/example/snaplib/maven-metadata.xml",
            None,
        ),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert!(String::from_utf8(abytes)
        .unwrap()
        .contains("<version>1.0-SNAPSHOT</version>"));
}

#[tokio::test]
async fn maven_web上传snapshot两次_构建号递增() {
    let fx = Fixture::new().await;
    let writer = fx.seed_user("writer", "pw", Role::User).await;
    let rid = fx.seed_maven_repo("maven-hosted", Visibility::Public).await;
    fx.seed_acl(&rid, &writer, Permission::Write).await;
    let auth = format!("Bearer {}", fx.login_token("writer", "pw").await);

    let jar = build_jar("com.example", "snaplib", None);
    for _ in 0..2 {
        let parts = vec![
            text_part("group_id", "com.example"),
            text_part("artifact_id", "snaplib"),
            text_part("version", "2.0-SNAPSHOT"),
            file_part("file", "snaplib-2.0-SNAPSHOT.jar", &jar),
        ];
        let resp = fx
            .router()
            .oneshot(upload_req(&rid, Some(&auth), &parts))
            .await
            .unwrap();
        assert_eq!(resp.status(), StatusCode::CREATED);
    }

    let (status, bytes) = send_bytes(
        fx.router(),
        empty_req(
            "GET",
            "/maven-hosted/com/example/snaplib/2.0-SNAPSHOT/maven-metadata.xml",
            None,
        ),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    let xml = String::from_utf8(bytes).unwrap();
    // 第二次上传后最新构建号为 2
    assert!(
        xml.contains("<buildNumber>2</buildNumber>"),
        "应递增到 2: {xml}"
    );
    let value = extract_tag(&xml, "value").unwrap();
    assert!(value.ends_with("-2"), "最新 value 构建号应为 2: {value}");
}

#[tokio::test]
async fn maven_deploy时间戳snapshot_服务端重生成snapshot_metadata() {
    let fx = Fixture::new().await;
    let writer = fx.seed_user("writer", "pw", Role::User).await;
    let rid = fx.seed_maven_repo("maven-hosted", Visibility::Public).await;
    fx.seed_acl(&rid, &writer, Permission::Write).await;
    let auth = format!("Bearer {}", fx.login_token("writer", "pw").await);

    // mvn deploy 模拟：客户端 PUT 时间戳唯一构件（含 jar + pom）
    for ext in ["jar", "pom"] {
        let path =
            format!("/maven-hosted/com/example/dlib/1.0-SNAPSHOT/dlib-1.0-20260629.120000-1.{ext}");
        let (status, _) = send(
            fx.router(),
            raw_req(
                "PUT",
                &path,
                Some(&auth),
                format!("data-{ext}").into_bytes(),
            ),
        )
        .await;
        assert_eq!(status, StatusCode::CREATED);
    }

    // 服务端据目录时间戳构建权威重生成快照级 metadata
    let (status, bytes) = send_bytes(
        fx.router(),
        empty_req(
            "GET",
            "/maven-hosted/com/example/dlib/1.0-SNAPSHOT/maven-metadata.xml",
            None,
        ),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    let xml = String::from_utf8(bytes).unwrap();
    assert!(
        xml.contains("<timestamp>20260629.120000</timestamp>"),
        "{xml}"
    );
    assert!(xml.contains("<buildNumber>1</buildNumber>"));
    assert!(xml.contains("<value>1.0-20260629.120000-1</value>"));
    assert!(xml.contains("<extension>jar</extension>"));
    assert!(xml.contains("<extension>pom</extension>"));
}

// ---------- FR-123 Web 上传页 Maven 适配（坐标自动回填 + 可选 pom 多文件） ----------

#[tokio::test]
async fn maven_web上传jar_坐标留空时从内嵌pom自动识别() {
    let fx = Fixture::new().await;
    let writer = fx.seed_user("writer", "pw", Role::User).await;
    let rid = fx.seed_maven_repo("maven-hosted", Visibility::Public).await;
    fx.seed_acl(&rid, &writer, Permission::Write).await;
    let auth = format!("Bearer {}", fx.login_token("writer", "pw").await);

    let pom = r#"<project><groupId>com.example</groupId><artifactId>autolib</artifactId><version>3.0</version></project>"#;
    let jar = build_jar("com.example", "autolib", Some(pom));
    // 仅上传文件，不带 group_id / artifact_id / version 表单字段
    let parts = vec![file_part("file", "autolib-3.0.jar", &jar)];
    let resp = fx
        .router()
        .oneshot(upload_req(&rid, Some(&auth), &parts))
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::CREATED);

    // 服务端据 jar 内嵌 pom 自动识别坐标，制品落于正确 GAV 路径
    let (status, jbytes) = send_bytes(
        fx.router(),
        empty_req(
            "GET",
            "/maven-hosted/com/example/autolib/3.0/autolib-3.0.jar",
            None,
        ),
    )
    .await;
    assert_eq!(status, StatusCode::OK, "应据内嵌 pom 落于 GAV 路径");
    assert_eq!(jbytes, jar);

    // artifact 级 metadata 也据自动识别坐标生成
    let (status, mbytes) = send_bytes(
        fx.router(),
        empty_req(
            "GET",
            "/maven-hosted/com/example/autolib/maven-metadata.xml",
            None,
        ),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert!(String::from_utf8(mbytes)
        .unwrap()
        .contains("<version>3.0</version>"));
}

#[tokio::test]
async fn maven_web上传jar无坐标无内嵌pom_返回400() {
    let fx = Fixture::new().await;
    let writer = fx.seed_user("writer", "pw", Role::User).await;
    let rid = fx.seed_maven_repo("maven-hosted", Visibility::Public).await;
    fx.seed_acl(&rid, &writer, Permission::Write).await;
    let auth = format!("Bearer {}", fx.login_token("writer", "pw").await);

    // 无内嵌 pom 的 jar + 不填坐标 → 无法识别坐标 → 400
    let jar = build_jar("com.example", "x", None);
    let parts = vec![file_part("file", "mystery.jar", &jar)];
    let resp = fx
        .router()
        .oneshot(upload_req(&rid, Some(&auth), &parts))
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::BAD_REQUEST);
}

#[tokio::test]
async fn maven_web上传jar附用户pom_采用用户pom不被覆盖() {
    let fx = Fixture::new().await;
    let writer = fx.seed_user("writer", "pw", Role::User).await;
    let rid = fx.seed_maven_repo("maven-hosted", Visibility::Public).await;
    fx.seed_acl(&rid, &writer, Permission::Write).await;
    let auth = format!("Bearer {}", fx.login_token("writer", "pw").await);

    // 无内嵌 pom 的 jar + 用户显式附带的 pom 文件 + 手填坐标
    let jar = build_jar("com.example", "withpom", None);
    let user_pom =
        b"<project><modelVersion>4.0.0</modelVersion><description>user pom</description></project>"
            .to_vec();
    let parts = vec![
        text_part("group_id", "com.example"),
        text_part("artifact_id", "withpom"),
        text_part("version", "1.0"),
        file_part("file", "withpom-1.0.jar", &jar),
        file_part("pom", "withpom-1.0.pom", &user_pom),
    ];
    let resp = fx
        .router()
        .oneshot(upload_req(&rid, Some(&auth), &parts))
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::CREATED);

    // pom 为用户上传内容（client-priority），不被服务端最小 pom 覆盖
    let (status, bytes) = send_bytes(
        fx.router(),
        empty_req(
            "GET",
            "/maven-hosted/com/example/withpom/1.0/withpom-1.0.pom",
            None,
        ),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(bytes, user_pom, "应保留用户上传 pom");
}
