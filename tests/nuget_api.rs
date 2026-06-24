//! NuGet v3 格式的 HTTP 集成测试（FR-29）。
//!
//! 覆盖：`nuget push`（multipart 含 .nupkg）发布 → 服务索引 / 扁平容器版本列表 / .nupkg / .nuspec
//! 下载端到端（hosted），字节一致与四校验和（FR-69），版本不可变（重复 push 同版本 409，FR-61），
//! 写授权边界（无写权 403）、private 对无权读 404，以及 proxy cache-miss 回源缓存（.nupkg）、
//! 服务索引回源重写、版本列表回源（真实 mock 上游走 HttpUpstream）。
//!
//! 鉴权与制品机理复用既有层，本文件只验 NuGet 协议适配的正确性。

use std::io::Write;
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
        let jwt = JwtSigner::from_secret(b"nuget-secret-32-bytes-xxxxxxxxxxx", 3600);
        let upstream = HttpUpstream::new(std::time::Duration::from_secs(60)).unwrap();
        let artifacts = Arc::new(ArtifactService::new(store.clone(), meta.clone(), upstream));
        let mut config = Config::default();
        // 固定对外地址，便于断言服务索引 @id 与使用片段
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

    /// 建一个 NuGet hosted 仓库，返回 id。
    async fn seed_nuget_repo(&self, name: &str, visibility: Visibility) -> String {
        self.state
            .meta
            .create_repository(NewRepository {
                name,
                format: "nuget",
                r#type: RepoType::Hosted,
                visibility,
                upstream_url: None,
                upstream_auth_ref: None,
            })
            .await
            .unwrap()
    }

    /// 建一个 NuGet proxy 仓库（上游基址为上游服务根，如 mock 服务的 http://addr），返回 id。
    async fn seed_nuget_proxy_repo(
        &self,
        name: &str,
        visibility: Visibility,
        upstream_url: &str,
    ) -> String {
        self.state
            .meta
            .create_repository(NewRepository {
                name,
                format: "nuget",
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

/// 构造一份最小 .nupkg（zip，内含根级 `{id}.nuspec` 与一个伪 DLL）。
fn build_nupkg(id: &str, version: &str) -> Vec<u8> {
    use zip::write::SimpleFileOptions;
    use zip::ZipWriter;
    let nuspec = format!(
        r#"<?xml version="1.0"?>
<package xmlns="http://schemas.microsoft.com/packaging/2013/05/nuspec.xsd">
  <metadata>
    <id>{id}</id>
    <version>{version}</version>
    <authors>tester</authors>
    <description>测试包</description>
  </metadata>
</package>"#
    );
    let mut buf = Vec::new();
    {
        let cursor = std::io::Cursor::new(&mut buf);
        let mut zip = ZipWriter::new(cursor);
        let opts =
            SimpleFileOptions::default().compression_method(zip::CompressionMethod::Deflated);
        zip.start_file(format!("{id}.nuspec"), opts).unwrap();
        zip.write_all(nuspec.as_bytes()).unwrap();
        zip.start_file("lib/net8.0/Sample.dll", opts).unwrap();
        zip.write_all(b"FAKE-DLL-BYTES").unwrap();
        zip.finish().unwrap();
    }
    buf
}

/// 用单一文件字段构造 `nuget push` 的 multipart/form-data 体，返回 (content-type, body)。
fn build_push_multipart(nupkg: &[u8]) -> (String, Vec<u8>) {
    let boundary = "----jianartifact-test-boundary";
    let mut body = Vec::new();
    body.extend_from_slice(format!("--{boundary}\r\n").as_bytes());
    body.extend_from_slice(
        b"content-disposition: form-data; name=\"package\"; filename=\"package.nupkg\"\r\n",
    );
    body.extend_from_slice(b"content-type: application/octet-stream\r\n\r\n");
    body.extend_from_slice(nupkg);
    body.extend_from_slice(format!("\r\n--{boundary}--\r\n").as_bytes());
    let content_type = format!("multipart/form-data; boundary={boundary}");
    (content_type, body)
}

/// 发起一次 `nuget push`（PUT /{repo}/v3/package）。
fn push_req(repo: &str, nupkg: &[u8], auth: Option<&str>) -> Request<Body> {
    let (content_type, body) = build_push_multipart(nupkg);
    let mut builder = Request::builder()
        .method("PUT")
        .uri(format!("/{repo}/v3/package"))
        .header("content-type", content_type);
    if let Some(a) = auth {
        builder = builder.header("authorization", a);
    }
    builder.body(Body::from(body)).unwrap()
}

/// 计算字节的 sha256 十六进制（与下载响应头 / 独立计算对账）。
fn sha256_hex(bytes: &[u8]) -> String {
    use sha2::{Digest, Sha256};
    let mut h = Sha256::new();
    h.update(bytes);
    format!("{:x}", h.finalize())
}

// ---------- FR-29 hosted：push → 服务索引 / 版本列表 / .nupkg / .nuspec 端到端 ----------

#[tokio::test]
async fn nuget_push_后取服务索引_版本列表_nupkg_端到端() {
    let fx = Fixture::new().await;
    let writer = fx.seed_user("dev", "pw", Role::User).await;
    let rid = fx.seed_nuget_repo("nuget-hosted", Visibility::Public).await;
    fx.seed_acl(&rid, &writer, Permission::Write).await;
    let auth = format!("Bearer {}", fx.login_token("dev", "pw").await);

    // id 含大写，验证存储与下载均按小写规范化命中
    let nupkg = build_nupkg("Sample.Pkg", "1.2.3");

    // PUT /{repo}/v3/package 发布（nuget push）
    let (status, body) = send(fx.router(), push_req("nuget-hosted", &nupkg, Some(&auth))).await;
    assert_eq!(status, StatusCode::CREATED, "push 应 201: {body}");

    // 服务索引：列出扁平容器与发布端点，@id 指向本仓库
    let (status, idx) = send(
        fx.router(),
        empty_req("GET", "/nuget-hosted/v3/index.json", None),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(idx["version"], "3.0.0");
    let resources = idx["resources"].as_array().unwrap();
    let flat = resources
        .iter()
        .find(|r| r["@type"] == "PackageBaseAddress/3.0.0")
        .unwrap();
    assert_eq!(
        flat["@id"],
        "http://localhost:8080/nuget-hosted/v3-flatcontainer/"
    );

    // 扁平容器版本列表（id 小写）：动态汇总
    let (status, versions) = send(
        fx.router(),
        empty_req(
            "GET",
            "/nuget-hosted/v3-flatcontainer/sample.pkg/index.json",
            None,
        ),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(versions["versions"], json!(["1.2.3"]));

    // 下载 .nupkg（小写路径）：字节一致、四校验和头正确
    let (status, bytes) = send_bytes(
        fx.router(),
        empty_req(
            "GET",
            "/nuget-hosted/v3-flatcontainer/sample.pkg/1.2.3/sample.pkg.1.2.3.nupkg",
            None,
        ),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(bytes, nupkg, "下载 .nupkg 字节应与上传一致");

    // 下载 .nuspec：可取到发布时提取并落盘的清单，含 id/version
    let (status, nuspec) = send_bytes(
        fx.router(),
        empty_req(
            "GET",
            "/nuget-hosted/v3-flatcontainer/sample.pkg/1.2.3/sample.pkg.nuspec",
            None,
        ),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    let nuspec_text = String::from_utf8(nuspec).unwrap();
    assert!(nuspec_text.contains("<id>Sample.Pkg</id>"));
    assert!(nuspec_text.contains("<version>1.2.3</version>"));
}

#[tokio::test]
async fn nuget_下载_nupkg_校验和头与独立计算一致() {
    let fx = Fixture::new().await;
    let writer = fx.seed_user("dev", "pw", Role::User).await;
    let rid = fx.seed_nuget_repo("nuget-hosted", Visibility::Public).await;
    fx.seed_acl(&rid, &writer, Permission::Write).await;
    let auth = format!("Bearer {}", fx.login_token("dev", "pw").await);

    let nupkg = build_nupkg("Hash.Pkg", "9.9.9");
    let (status, _) = send(fx.router(), push_req("nuget-hosted", &nupkg, Some(&auth))).await;
    assert_eq!(status, StatusCode::CREATED);

    // 取下载响应的 sha256 头与独立计算对账（FR-69）
    let resp = fx
        .router()
        .oneshot(empty_req(
            "GET",
            "/nuget-hosted/v3-flatcontainer/hash.pkg/9.9.9/hash.pkg.9.9.9.nupkg",
            None,
        ))
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::OK);
    let sha_header = resp
        .headers()
        .get("x-checksum-sha256")
        .and_then(|v| v.to_str().ok())
        .unwrap()
        .to_string();
    assert_eq!(sha_header, sha256_hex(&nupkg));
}

#[tokio::test]
async fn nuget_两个版本都进版本列表() {
    let fx = Fixture::new().await;
    let writer = fx.seed_user("dev", "pw", Role::User).await;
    let rid = fx.seed_nuget_repo("nuget-hosted", Visibility::Public).await;
    fx.seed_acl(&rid, &writer, Permission::Write).await;
    let auth = format!("Bearer {}", fx.login_token("dev", "pw").await);

    for ver in ["1.0.0", "2.0.0"] {
        let nupkg = build_nupkg("Multi.Pkg", ver);
        let (status, _) = send(fx.router(), push_req("nuget-hosted", &nupkg, Some(&auth))).await;
        assert_eq!(status, StatusCode::CREATED, "版本 {ver} push 应 201");
    }

    let (status, versions) = send(
        fx.router(),
        empty_req(
            "GET",
            "/nuget-hosted/v3-flatcontainer/multi.pkg/index.json",
            None,
        ),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    // 升序、去重
    assert_eq!(versions["versions"], json!(["1.0.0", "2.0.0"]));
}

// ---------- FR-61 版本不可变：重复 push 同版本 409 ----------

#[tokio::test]
async fn nuget_重复_push_同版本返回_409() {
    let fx = Fixture::new().await;
    let writer = fx.seed_user("dev", "pw", Role::User).await;
    let rid = fx.seed_nuget_repo("nuget-hosted", Visibility::Public).await;
    fx.seed_acl(&rid, &writer, Permission::Write).await;
    let auth = format!("Bearer {}", fx.login_token("dev", "pw").await);

    let nupkg = build_nupkg("Dup.Pkg", "1.0.0");
    let (s1, _) = send(fx.router(), push_req("nuget-hosted", &nupkg, Some(&auth))).await;
    assert_eq!(s1, StatusCode::CREATED);

    // 再次 push 同 id+version → 409
    let (s2, err) = send(fx.router(), push_req("nuget-hosted", &nupkg, Some(&auth))).await;
    assert_eq!(s2, StatusCode::CONFLICT);
    assert_eq!(err["error"]["code"], "conflict");
}

// ---------- 非法包：缺 .nuspec / 非 zip → 400 ----------

#[tokio::test]
async fn nuget_push_非法包返回_400() {
    let fx = Fixture::new().await;
    let writer = fx.seed_user("dev", "pw", Role::User).await;
    let rid = fx.seed_nuget_repo("nuget-hosted", Visibility::Public).await;
    fx.seed_acl(&rid, &writer, Permission::Write).await;
    let auth = format!("Bearer {}", fx.login_token("dev", "pw").await);

    // 非 zip 字节
    let (status, err) = send(
        fx.router(),
        push_req("nuget-hosted", b"not-a-zip-file", Some(&auth)),
    )
    .await;
    assert_eq!(status, StatusCode::BAD_REQUEST);
    assert_eq!(err["error"]["code"], "bad_request");
}

// ---------- FR-09 写授权边界 ----------

#[tokio::test]
async fn nuget_无写权限_push_被拒_403() {
    let fx = Fixture::new().await;
    // public NuGet 仓库：匿名可读但无写 → push 应 403
    fx.seed_nuget_repo("nuget-pub", Visibility::Public).await;
    let nupkg = build_nupkg("No.Auth", "1.0.0");
    let (status, _) = send(fx.router(), push_req("nuget-pub", &nupkg, None)).await;
    assert_eq!(status, StatusCode::FORBIDDEN);
}

#[tokio::test]
async fn nuget_私有仓库对无权者读_404_隐藏存在性() {
    let fx = Fixture::new().await;
    fx.seed_nuget_repo("nuget-secret", Visibility::Private)
        .await;
    // 匿名读 private 服务索引 → 404（不泄露存在）
    let (status, _) = send(
        fx.router(),
        empty_req("GET", "/nuget-secret/v3/index.json", None),
    )
    .await;
    assert_eq!(status, StatusCode::NOT_FOUND);
    // 匿名读 private 版本列表 → 404
    let (status, _) = send(
        fx.router(),
        empty_req("GET", "/nuget-secret/v3-flatcontainer/x/index.json", None),
    )
    .await;
    assert_eq!(status, StatusCode::NOT_FOUND);
}

// ---------- 不存在的包版本列表 → 404 ----------

#[tokio::test]
async fn nuget_未发布包版本列表_404() {
    let fx = Fixture::new().await;
    fx.seed_nuget_repo("nuget-hosted", Visibility::Public).await;
    let (status, _) = send(
        fx.router(),
        empty_req(
            "GET",
            "/nuget-hosted/v3-flatcontainer/nope/index.json",
            None,
        ),
    )
    .await;
    assert_eq!(status, StatusCode::NOT_FOUND);
}

// ---------- FR-12 proxy：服务索引回源重写 + .nupkg cache-miss→hit + 版本列表回源 ----------

/// 启动一个 mock NuGet v3 上游：服务索引、扁平容器版本列表与 .nupkg，记录命中次数。
///
/// 返回 (上游服务根, 服务索引命中计数, .nupkg 命中计数)。服务索引的 PackageBaseAddress @id
/// 指向上游自身，服务端代理后应重写为本仓库。
async fn 启动_mock_nuget(
    id_lower: &'static str,
    version: &'static str,
    nupkg: &'static [u8],
) -> (
    String,
    Arc<std::sync::atomic::AtomicUsize>,
    Arc<std::sync::atomic::AtomicUsize>,
) {
    use axum::routing::get;
    use std::sync::atomic::{AtomicUsize, Ordering};

    let index_calls = Arc::new(AtomicUsize::new(0));
    let nupkg_calls = Arc::new(AtomicUsize::new(0));

    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    let base = format!("http://{addr}");

    // 上游服务索引：PackageBaseAddress @id 指向上游自身扁平容器
    let service_index = json!({
        "version": "3.0.0",
        "resources": [
            {
                "@id": format!("{base}/v3-flatcontainer/"),
                "@type": "PackageBaseAddress/3.0.0"
            },
            {
                "@id": format!("{base}/v3/registration5-gz-semver2/"),
                "@type": "RegistrationsBaseUrl/Versioned"
            }
        ]
    })
    .to_string();
    let versions_doc = json!({ "versions": [version] }).to_string();

    let ic = index_calls.clone();
    let nc = nupkg_calls.clone();
    let si = service_index.clone();
    let vd = versions_doc.clone();
    let nupkg_path = format!("/v3-flatcontainer/{id_lower}/{version}/{id_lower}.{version}.nupkg");
    let versions_path = format!("/v3-flatcontainer/{id_lower}/index.json");

    let app = Router::new()
        .route(
            "/v3/index.json",
            get(move || {
                let ic = ic.clone();
                let si = si.clone();
                async move {
                    ic.fetch_add(1, Ordering::SeqCst);
                    ([(axum::http::header::CONTENT_TYPE, "application/json")], si)
                }
            }),
        )
        .route(
            &versions_path,
            get(move || {
                let vd = vd.clone();
                async move { ([(axum::http::header::CONTENT_TYPE, "application/json")], vd) }
            }),
        )
        .route(
            &nupkg_path,
            get(move || {
                let nc = nc.clone();
                async move {
                    nc.fetch_add(1, Ordering::SeqCst);
                    nupkg
                }
            }),
        );

    tokio::spawn(async move {
        axum::serve(listener, app).await.unwrap();
    });
    (base, index_calls, nupkg_calls)
}

#[tokio::test]
async fn nuget_proxy_服务索引回源重写_nupkg_缓存_版本列表回源() {
    use std::sync::atomic::Ordering;

    let nupkg: &'static [u8] = b"upstream-nupkg-bytes-zip-payload";
    let (上游根, index_calls, nupkg_calls) = 启动_mock_nuget("mirror.pkg", "3.1.4", nupkg).await;
    let fx = Fixture::new().await;
    fx.seed_nuget_proxy_repo("nuget-mirror", Visibility::Public, &上游根)
        .await;

    // ① 服务索引：回源 + 把 PackageBaseAddress @id 重写为本仓库
    let (status, idx) = send(
        fx.router(),
        empty_req("GET", "/nuget-mirror/v3/index.json", None),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    let resources = idx["resources"].as_array().unwrap();
    let flat = resources
        .iter()
        .find(|r| r["@type"] == "PackageBaseAddress/3.0.0")
        .unwrap();
    assert_eq!(
        flat["@id"],
        "http://localhost:8080/nuget-mirror/v3-flatcontainer/"
    );
    // 未实现的资源保持上游原值
    assert!(resources
        .iter()
        .any(|r| r["@type"] == "RegistrationsBaseUrl/Versioned"));
    assert_eq!(index_calls.load(Ordering::SeqCst), 1, "应回源一次服务索引");

    // ② 版本列表：回源透传
    let (status, versions) = send(
        fx.router(),
        empty_req(
            "GET",
            "/nuget-mirror/v3-flatcontainer/mirror.pkg/index.json",
            None,
        ),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(versions["versions"], json!(["3.1.4"]));

    // ③ .nupkg cache-miss：回源一次
    let url = "/nuget-mirror/v3-flatcontainer/mirror.pkg/3.1.4/mirror.pkg.3.1.4.nupkg";
    let (status, bytes) = send_bytes(fx.router(), empty_req("GET", url, None)).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(bytes, nupkg);
    assert_eq!(
        nupkg_calls.load(Ordering::SeqCst),
        1,
        "首次应回源一次 .nupkg"
    );

    // ④ .nupkg cache-hit：命中缓存不再回源
    let (status, bytes) = send_bytes(fx.router(), empty_req("GET", url, None)).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(bytes, nupkg);
    assert_eq!(
        nupkg_calls.load(Ordering::SeqCst),
        1,
        "命中缓存不应再回源 .nupkg"
    );
}
