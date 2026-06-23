//! Docker Registry v2 / OCI Distribution 的 HTTP 集成测试（FR-16）。
//!
//! 覆盖：版本检查（含 Docker-Distribution-Api-Version 头）、blob 上传状态机
//! （POST→PATCH→PUT 分块与 POST?digest 单体）、blob HEAD/GET、manifest PUT/HEAD/GET
//! （按 tag 与 digest，正确 Content-Type 与 Docker-Content-Digest 头）、同 tag 覆盖、
//! digest 校验失败，以及鉴权边界（匿名写 401 + WWW-Authenticate、私有仓库对匿名 / 无权
//! 已认证用户的存在性隐藏）。
//!
//! 走 `tower::ServiceExt::oneshot` 直驱 router，断言状态码、响应头与字节内容。

use std::sync::Arc;

use axum::body::Body;
use axum::http::{header, Request, StatusCode};
use axum::Router;
use base64::{engine::general_purpose::STANDARD, Engine};
use digest::Digest;
use http_body_util::BodyExt;
use serde_json::Value;
use tower::ServiceExt;

use jianartifact::api::{build_router, AppState};
use jianartifact::auth::{self, JwtSigner, LoginGuard};
use jianartifact::config::Config;
use jianartifact::format::{ArtifactService, DockerRegistry, FormatRegistry};
use jianartifact::meta::{MetaStore, NewRepository, Permission, RepoType, Role, Visibility};
use jianartifact::proxy::HttpUpstream;
use jianartifact::storage::LocalFsStore;

/// schema2 manifest 媒体类型。
const MANIFEST_V2: &str = "application/vnd.docker.distribution.manifest.v2+json";

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
        let jwt = JwtSigner::from_secret(b"docker-secret-32-bytes-xxxxxxxxxx", 3600);
        let upstream = HttpUpstream::new(std::time::Duration::from_secs(60)).unwrap();
        let artifacts = Arc::new(ArtifactService::new(store.clone(), meta.clone(), upstream));
        let docker = Arc::new(
            DockerRegistry::new(store.clone(), meta.clone(), dir.path().join("uploads"), None)
                .await
                .unwrap(),
        );
        let mut config = Config::default();
        // 固定对外地址，便于断言 Location 头
        config.server.public_base_url = Some("http://127.0.0.1:18161".to_string());
        let state = AppState {
            config: Arc::new(config),
            meta,
            store,
            jwt,
            login_guard: Arc::new(LoginGuard::new(50, 900)),
            artifacts,
            formats: Arc::new(FormatRegistry::with_builtin()),
            docker: Some(docker),
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

    /// 建一个 docker hosted 仓库，返回 id。
    async fn seed_docker_repo(&self, name: &str, visibility: Visibility) -> String {
        self.state
            .meta
            .create_repository(NewRepository {
                name,
                format: "docker",
                r#type: RepoType::Hosted,
                visibility,
                upstream_url: None,
                upstream_auth_ref: None,
            })
            .await
            .unwrap()
    }

    async fn seed_acl(&self, repo_id: &str, user_id: &str, permission: Permission) {
        self.state.meta.create_acl(repo_id, user_id, permission).await.unwrap();
    }
}

/// 以 Basic 凭据组装 Authorization 头（docker 客户端登录后即用此通道）。
fn basic(username: &str, password: &str) -> String {
    format!("Basic {}", STANDARD.encode(format!("{username}:{password}")))
}

/// 以 Bearer 令牌组装 Authorization 头。
fn bearer(token: &str) -> String {
    format!("Bearer {token}")
}

/// 请求令牌端点换取范围令牌，返回响应体 JSON。
async fn fetch_token(fx: &Fixture, auth: Option<&str>, scope: &str) -> serde_json::Value {
    let uri = format!("/v2/token?service=jianartifact&scope={scope}");
    let resp = fx
        .router()
        .oneshot(req("GET", &uri, auth, None, Vec::new()))
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::OK, "令牌端点应 200");
    serde_json::from_slice(&body_bytes(resp).await).unwrap()
}

/// 算内容 sha256 并拼成 docker digest（`sha256:{hex}`）。
fn digest_of(data: &[u8]) -> String {
    let mut h = sha2::Sha256::new();
    h.update(data);
    format!("sha256:{:x}", h.finalize())
}

/// 构造带可选 Authorization 与 Content-Type 的请求。
fn req(
    method: &str,
    uri: &str,
    auth: Option<&str>,
    content_type: Option<&str>,
    body: Vec<u8>,
) -> Request<Body> {
    let mut builder = Request::builder().method(method).uri(uri);
    if let Some(a) = auth {
        builder = builder.header(header::AUTHORIZATION, a);
    }
    if let Some(ct) = content_type {
        builder = builder.header(header::CONTENT_TYPE, ct);
    }
    builder.body(Body::from(body)).unwrap()
}

/// 取响应中某头的字符串值。
fn header_str(resp: &axum::response::Response, name: &str) -> Option<String> {
    resp.headers()
        .get(name)
        .and_then(|v| v.to_str().ok())
        .map(str::to_string)
}

/// 收集响应体字节。
async fn body_bytes(resp: axum::response::Response) -> Vec<u8> {
    resp.into_body().collect().await.unwrap().to_bytes().to_vec()
}

/// 端到端把一段 blob 经 POST→PATCH→PUT 状态机推上去，返回最终 digest。
async fn push_blob(fx: &Fixture, name: &str, auth: &str, content: &[u8]) -> String {
    let digest = digest_of(content);

    // POST 启动上传
    let post = fx
        .router()
        .oneshot(req(
            "POST",
            &format!("/v2/{name}/blobs/uploads/"),
            Some(auth),
            None,
            Vec::new(),
        ))
        .await
        .unwrap();
    assert_eq!(post.status(), StatusCode::ACCEPTED, "POST 启动上传应 202");
    let location = header_str(&post, "location").expect("启动上传须带 Location");

    // PATCH 追加全部字节（用返回的 Location 作为续传地址）
    let patch = fx
        .router()
        .oneshot(req("PATCH", &location, Some(auth), None, content.to_vec()))
        .await
        .unwrap();
    assert_eq!(patch.status(), StatusCode::ACCEPTED, "PATCH 追加应 202");

    // PUT 完成（带 digest 查询参数）
    let put = fx
        .router()
        .oneshot(req(
            "PUT",
            &format!("{location}?digest={digest}"),
            Some(auth),
            None,
            Vec::new(),
        ))
        .await
        .unwrap();
    assert_eq!(put.status(), StatusCode::CREATED, "PUT 完成应 201");
    assert_eq!(
        header_str(&put, "docker-content-digest").as_deref(),
        Some(digest.as_str()),
        "完成上传应回 Docker-Content-Digest"
    );
    digest
}

// ---------- 版本检查 ----------

#[tokio::test]
async fn v2_版本检查_无凭据_401_bearer_带凭据_200() {
    let fx = Fixture::new().await;
    fx.seed_user("admin", "pw", Role::Admin).await;

    // 无凭据：发起认证发现，返回 401 + Bearer 质询（不带 scope）
    let anon = fx
        .router()
        .oneshot(req("GET", "/v2/", None, None, Vec::new()))
        .await
        .unwrap();
    assert_eq!(anon.status(), StatusCode::UNAUTHORIZED);
    let www = header_str(&anon, "www-authenticate").expect("应带 WWW-Authenticate");
    assert!(www.starts_with("Bearer "), "应为 Bearer 质询: {www}");
    assert!(www.contains("/v2/token"), "realm 应指向令牌端点: {www}");

    // 带凭据：返回 200 与版本头（探活成功）
    let authed = fx
        .router()
        .oneshot(req("GET", "/v2/", Some(&basic("admin", "pw")), None, Vec::new()))
        .await
        .unwrap();
    assert_eq!(authed.status(), StatusCode::OK);
    assert_eq!(
        header_str(&authed, "docker-distribution-api-version").as_deref(),
        Some("registry/2.0")
    );
}

// ---------- blob 上传状态机（POST→PATCH→PUT）与读回 ----------

#[tokio::test]
async fn blob_分块上传后_head_get_可读回内容一致() {
    let fx = Fixture::new().await;
    fx.seed_user("admin", "pw", Role::Admin).await;
    fx.seed_docker_repo("hub", Visibility::Public).await;
    let auth = basic("admin", "pw");
    let content = b"docker-layer-bytes-stream";

    let digest = push_blob(&fx, "hub/app", &auth, content).await;

    // HEAD 存在性 + Content-Length
    let head = fx
        .router()
        .oneshot(req(
            "HEAD",
            &format!("/v2/hub/app/blobs/{digest}"),
            None,
            None,
            Vec::new(),
        ))
        .await
        .unwrap();
    assert_eq!(head.status(), StatusCode::OK);
    assert_eq!(
        header_str(&head, "content-length").as_deref(),
        Some(content.len().to_string().as_str())
    );

    // GET 读回内容一致 + digest 头
    let get = fx
        .router()
        .oneshot(req(
            "GET",
            &format!("/v2/hub/app/blobs/{digest}"),
            None,
            None,
            Vec::new(),
        ))
        .await
        .unwrap();
    assert_eq!(get.status(), StatusCode::OK);
    assert_eq!(
        header_str(&get, "docker-content-digest").as_deref(),
        Some(digest.as_str())
    );
    assert_eq!(body_bytes(get).await, content);
}

#[tokio::test]
async fn blob_单体上传_post_带_digest_直接完成() {
    let fx = Fixture::new().await;
    fx.seed_user("admin", "pw", Role::Admin).await;
    fx.seed_docker_repo("hub", Visibility::Public).await;
    let auth = basic("admin", "pw");
    let content = b"single-shot-via-post";
    let digest = digest_of(content);

    // POST 直接携带 digest 与 body（单体上传）
    let resp = fx
        .router()
        .oneshot(req(
            "POST",
            &format!("/v2/hub/app/blobs/uploads/?digest={digest}"),
            Some(&auth),
            None,
            content.to_vec(),
        ))
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::CREATED, "单体上传应直接 201");
    assert_eq!(
        header_str(&resp, "docker-content-digest").as_deref(),
        Some(digest.as_str())
    );

    // 可读回
    let head = fx
        .router()
        .oneshot(req(
            "HEAD",
            &format!("/v2/hub/app/blobs/{digest}"),
            None,
            None,
            Vec::new(),
        ))
        .await
        .unwrap();
    assert_eq!(head.status(), StatusCode::OK);
}

#[tokio::test]
async fn blob_put_完成时_digest_不符返回_400() {
    let fx = Fixture::new().await;
    fx.seed_user("admin", "pw", Role::Admin).await;
    fx.seed_docker_repo("hub", Visibility::Public).await;
    let auth = basic("admin", "pw");

    let post = fx
        .router()
        .oneshot(req(
            "POST",
            "/v2/hub/app/blobs/uploads/",
            Some(&auth),
            None,
            Vec::new(),
        ))
        .await
        .unwrap();
    let location = header_str(&post, "location").unwrap();
    fx.router()
        .oneshot(req("PATCH", &location, Some(&auth), None, b"real".to_vec()))
        .await
        .unwrap();

    // 故意给错 digest
    let wrong = format!("sha256:{}", "0".repeat(64));
    let put = fx
        .router()
        .oneshot(req(
            "PUT",
            &format!("{location}?digest={wrong}"),
            Some(&auth),
            None,
            Vec::new(),
        ))
        .await
        .unwrap();
    assert_eq!(put.status(), StatusCode::BAD_REQUEST);
    // 错 digest 不应可读
    let head = fx
        .router()
        .oneshot(req(
            "HEAD",
            &format!("/v2/hub/app/blobs/{wrong}"),
            None,
            None,
            Vec::new(),
        ))
        .await
        .unwrap();
    assert_eq!(head.status(), StatusCode::NOT_FOUND);
}

// ---------- manifest 存取与 tag 覆盖 ----------

#[tokio::test]
async fn manifest_按_tag_写入再按_tag_与_digest_读回() {
    let fx = Fixture::new().await;
    fx.seed_user("admin", "pw", Role::Admin).await;
    fx.seed_docker_repo("hub", Visibility::Public).await;
    let auth = basic("admin", "pw");
    let manifest = br#"{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json"}"#;
    let expected = digest_of(manifest);

    // PUT manifest（按 tag）
    let put = fx
        .router()
        .oneshot(req(
            "PUT",
            "/v2/hub/app/manifests/1.0",
            Some(&auth),
            Some(MANIFEST_V2),
            manifest.to_vec(),
        ))
        .await
        .unwrap();
    assert_eq!(put.status(), StatusCode::CREATED);
    assert_eq!(
        header_str(&put, "docker-content-digest").as_deref(),
        Some(expected.as_str())
    );

    // GET 按 tag 读回：正确 Content-Type 与 digest 头、字节一致
    let by_tag = fx
        .router()
        .oneshot(req("GET", "/v2/hub/app/manifests/1.0", None, None, Vec::new()))
        .await
        .unwrap();
    assert_eq!(by_tag.status(), StatusCode::OK);
    assert_eq!(header_str(&by_tag, "content-type").as_deref(), Some(MANIFEST_V2));
    assert_eq!(
        header_str(&by_tag, "docker-content-digest").as_deref(),
        Some(expected.as_str())
    );
    assert_eq!(body_bytes(by_tag).await, manifest);

    // HEAD 按 digest 读回
    let by_digest = fx
        .router()
        .oneshot(req(
            "HEAD",
            &format!("/v2/hub/app/manifests/{expected}"),
            None,
            None,
            Vec::new(),
        ))
        .await
        .unwrap();
    assert_eq!(by_digest.status(), StatusCode::OK);
    assert_eq!(
        header_str(&by_digest, "docker-content-digest").as_deref(),
        Some(expected.as_str())
    );
}

#[tokio::test]
async fn tags_list_列出镜像全部_tag_未知镜像_404() {
    let fx = Fixture::new().await;
    fx.seed_user("admin", "pw", Role::Admin).await;
    fx.seed_docker_repo("hub", Visibility::Public).await;
    let auth = basic("admin", "pw");
    let manifest = br#"{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json"}"#;
    // 写两个 tag（乱序写入，验证列表按字典序）
    for tag in ["2.0", "1.0"] {
        let put = fx
            .router()
            .oneshot(req(
                "PUT",
                &format!("/v2/hub/app/manifests/{tag}"),
                Some(&auth),
                Some(MANIFEST_V2),
                manifest.to_vec(),
            ))
            .await
            .unwrap();
        assert_eq!(put.status(), StatusCode::CREATED);
    }

    // GET tags/list：返回该镜像全部 tag（字典序），带版本头
    let list = fx
        .router()
        .oneshot(req("GET", "/v2/hub/app/tags/list", None, None, Vec::new()))
        .await
        .unwrap();
    assert_eq!(list.status(), StatusCode::OK);
    assert_eq!(
        header_str(&list, "docker-distribution-api-version").as_deref(),
        Some("registry/2.0")
    );
    let body: serde_json::Value = serde_json::from_slice(&body_bytes(list).await).unwrap();
    assert_eq!(body["name"], "hub/app");
    assert_eq!(body["tags"], serde_json::json!(["1.0", "2.0"]));

    // 同仓库未知镜像 → 404 NAME_UNKNOWN（无任何 tag）
    let none = fx
        .router()
        .oneshot(req("GET", "/v2/hub/nope/tags/list", None, None, Vec::new()))
        .await
        .unwrap();
    assert_eq!(none.status(), StatusCode::NOT_FOUND);
}

#[tokio::test]
async fn manifest_同_tag_可覆盖指向新内容() {
    let fx = Fixture::new().await;
    fx.seed_user("admin", "pw", Role::Admin).await;
    fx.seed_docker_repo("hub", Visibility::Public).await;
    let auth = basic("admin", "pw");
    let m1 = br#"{"schemaVersion":2,"v":1}"#;
    let m2 = br#"{"schemaVersion":2,"v":2,"extra":"data"}"#;

    for m in [&m1[..], &m2[..]] {
        let put = fx
            .router()
            .oneshot(req(
                "PUT",
                "/v2/hub/app/manifests/latest",
                Some(&auth),
                Some(MANIFEST_V2),
                m.to_vec(),
            ))
            .await
            .unwrap();
        assert_eq!(put.status(), StatusCode::CREATED);
    }

    // latest 现指向 m2
    let now = fx
        .router()
        .oneshot(req("GET", "/v2/hub/app/manifests/latest", None, None, Vec::new()))
        .await
        .unwrap();
    assert_eq!(now.status(), StatusCode::OK);
    assert_eq!(body_bytes(now).await, m2);

    // 旧内容仍可按其 digest 取得
    let old = fx
        .router()
        .oneshot(req(
            "GET",
            &format!("/v2/hub/app/manifests/{}", digest_of(m1)),
            None,
            None,
            Vec::new(),
        ))
        .await
        .unwrap();
    assert_eq!(old.status(), StatusCode::OK);
    assert_eq!(body_bytes(old).await, m1);
}

#[tokio::test]
async fn 读不存在的_manifest_返回_404_manifest_unknown() {
    let fx = Fixture::new().await;
    fx.seed_docker_repo("hub", Visibility::Public).await;
    let resp = fx
        .router()
        .oneshot(req("GET", "/v2/hub/app/manifests/no-such-tag", None, None, Vec::new()))
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::NOT_FOUND);
    let body: Value = serde_json::from_slice(&body_bytes(resp).await).unwrap_or(Value::Null);
    assert_eq!(body["errors"][0]["code"], "MANIFEST_UNKNOWN");
}

// ---------- 鉴权边界（§2.1 高风险） ----------

#[tokio::test]
async fn 匿名写_manifest_返回_401_带_www_authenticate() {
    let fx = Fixture::new().await;
    fx.seed_docker_repo("hub", Visibility::Public).await;
    let resp = fx
        .router()
        .oneshot(req(
            "PUT",
            "/v2/hub/app/manifests/1.0",
            None,
            Some(MANIFEST_V2),
            b"{}".to_vec(),
        ))
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::UNAUTHORIZED);
    let www = header_str(&resp, "www-authenticate").expect("401 须带 WWW-Authenticate");
    // Bearer 令牌质询：含 realm（指向令牌端点）、service 与 scope（写需 pull,push）
    assert!(www.starts_with("Bearer "), "应为 Bearer 质询: {www}");
    assert!(www.contains("/v2/token"), "realm 应指向令牌端点: {www}");
    assert!(www.contains("service=\"jianartifact\""), "应含 service: {www}");
    assert!(
        www.contains("scope=\"repository:hub/app:pull,push\""),
        "写操作 scope 应含 pull,push: {www}"
    );
}

#[tokio::test]
async fn 匿名启动上传返回_401_带_www_authenticate() {
    let fx = Fixture::new().await;
    fx.seed_docker_repo("hub", Visibility::Public).await;
    let resp = fx
        .router()
        .oneshot(req(
            "POST",
            "/v2/hub/app/blobs/uploads/",
            None,
            None,
            Vec::new(),
        ))
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::UNAUTHORIZED);
    let www = header_str(&resp, "www-authenticate").expect("401 须带 WWW-Authenticate");
    assert!(www.starts_with("Bearer "), "应为 Bearer 质询: {www}");
}

#[tokio::test]
async fn 私有仓库匿名读_manifest_返回_401_隐藏存在性() {
    let fx = Fixture::new().await;
    fx.seed_user("admin", "pw", Role::Admin).await;
    fx.seed_docker_repo("priv", Visibility::Private).await;
    let auth = basic("admin", "pw");
    // 管理员先写一个 manifest
    fx.router()
        .oneshot(req(
            "PUT",
            "/v2/priv/app/manifests/1.0",
            Some(&auth),
            Some(MANIFEST_V2),
            b"{}".to_vec(),
        ))
        .await
        .unwrap();

    // 匿名读：私有仓库 → 401 引导认证（不暴露 404/200 区分存在性）
    let resp = fx
        .router()
        .oneshot(req("GET", "/v2/priv/app/manifests/1.0", None, None, Vec::new()))
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::UNAUTHORIZED);
    let www = header_str(&resp, "www-authenticate").expect("401 须带 WWW-Authenticate");
    // 读操作 Bearer 质询 scope 仅需 pull
    assert!(www.starts_with("Bearer "), "应为 Bearer 质询: {www}");
    assert!(
        www.contains("scope=\"repository:priv/app:pull\""),
        "读操作 scope 应仅含 pull: {www}"
    );
}

#[tokio::test]
async fn 私有仓库无_acl_已认证用户读_404_隐藏存在性() {
    let fx = Fixture::new().await;
    fx.seed_user("admin", "pw", Role::Admin).await;
    fx.seed_user("bob", "pw", Role::User).await;
    fx.seed_docker_repo("priv", Visibility::Private).await;
    let admin_auth = basic("admin", "pw");
    fx.router()
        .oneshot(req(
            "PUT",
            "/v2/priv/app/manifests/1.0",
            Some(&admin_auth),
            Some(MANIFEST_V2),
            b"{}".to_vec(),
        ))
        .await
        .unwrap();

    // 已认证但无 ACL 的普通用户读私有仓库 → 404（隐藏存在性，非 403）
    let bob_auth = basic("bob", "pw");
    let resp = fx
        .router()
        .oneshot(req(
            "GET",
            "/v2/priv/app/manifests/1.0",
            Some(&bob_auth),
            None,
            Vec::new(),
        ))
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::NOT_FOUND);
}

#[tokio::test]
async fn 有读无写_push_manifest_返回_403() {
    let fx = Fixture::new().await;
    fx.seed_user("admin", "pw", Role::Admin).await;
    let reader = fx.seed_user("reader", "pw", Role::User).await;
    let rid = fx.seed_docker_repo("priv", Visibility::Private).await;
    fx.seed_acl(&rid, &reader, Permission::Read).await;

    // 有读无写的用户写 manifest → 403
    let auth = basic("reader", "pw");
    let resp = fx
        .router()
        .oneshot(req(
            "PUT",
            "/v2/priv/app/manifests/1.0",
            Some(&auth),
            Some(MANIFEST_V2),
            b"{}".to_vec(),
        ))
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::FORBIDDEN);
}

#[tokio::test]
async fn 私有仓库读_acl_用户可拉取_manifest() {
    let fx = Fixture::new().await;
    fx.seed_user("admin", "pw", Role::Admin).await;
    let reader = fx.seed_user("reader", "pw", Role::User).await;
    let rid = fx.seed_docker_repo("priv", Visibility::Private).await;
    fx.seed_acl(&rid, &reader, Permission::Read).await;
    let admin_auth = basic("admin", "pw");
    let manifest = br#"{"schemaVersion":2}"#;
    fx.router()
        .oneshot(req(
            "PUT",
            "/v2/priv/app/manifests/1.0",
            Some(&admin_auth),
            Some(MANIFEST_V2),
            manifest.to_vec(),
        ))
        .await
        .unwrap();

    // 有读 ACL 的用户可读回
    let auth = basic("reader", "pw");
    let resp = fx
        .router()
        .oneshot(req(
            "GET",
            "/v2/priv/app/manifests/1.0",
            Some(&auth),
            None,
            Vec::new(),
        ))
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::OK);
    assert_eq!(body_bytes(resp).await, manifest);
}

// ---------- 完整 push/pull 时序（blob + manifest 引用） ----------

#[tokio::test]
async fn 完整推拉时序_blob_与引用其的_manifest() {
    let fx = Fixture::new().await;
    fx.seed_user("admin", "pw", Role::Admin).await;
    fx.seed_docker_repo("hub", Visibility::Public).await;
    let auth = basic("admin", "pw");

    // 1) 推 config blob 与 layer blob
    let config_blob = b"{\"architecture\":\"amd64\",\"os\":\"linux\"}";
    let layer_blob = b"fake-layer-tar-bytes";
    let config_digest = push_blob(&fx, "hub/test", &auth, config_blob).await;
    let layer_digest = push_blob(&fx, "hub/test", &auth, layer_blob).await;

    // 2) 推引用上述 blob 的 manifest
    let manifest = format!(
        r#"{{"schemaVersion":2,"mediaType":"{MANIFEST_V2}","config":{{"mediaType":"application/vnd.docker.container.image.v1+json","size":{},"digest":"{config_digest}"}},"layers":[{{"mediaType":"application/vnd.docker.image.rootfs.diff.tar.gzip","size":{},"digest":"{layer_digest}"}}]}}"#,
        config_blob.len(),
        layer_blob.len()
    );
    let put = fx
        .router()
        .oneshot(req(
            "PUT",
            "/v2/hub/test/manifests/1",
            Some(&auth),
            Some(MANIFEST_V2),
            manifest.clone().into_bytes(),
        ))
        .await
        .unwrap();
    assert_eq!(put.status(), StatusCode::CREATED);

    // 3) 模拟 pull：拉 manifest，再据其引用拉 blob，校验存在
    let m = fx
        .router()
        .oneshot(req("GET", "/v2/hub/test/manifests/1", None, None, Vec::new()))
        .await
        .unwrap();
    assert_eq!(m.status(), StatusCode::OK);
    for d in [&config_digest, &layer_digest] {
        let head = fx
            .router()
            .oneshot(req(
                "HEAD",
                &format!("/v2/hub/test/blobs/{d}"),
                None,
                None,
                Vec::new(),
            ))
            .await
            .unwrap();
        assert_eq!(head.status(), StatusCode::OK, "manifest 引用的 blob 应可拉取: {d}");
    }
}

// ---------- Bearer 令牌端点与令牌鉴权（§2.1 / §2.6） ----------

#[tokio::test]
async fn 令牌端点_admin_请求_pull_push_可推送() {
    let fx = Fixture::new().await;
    fx.seed_user("admin", "pw", Role::Admin).await;
    fx.seed_docker_repo("hub", Visibility::Public).await;

    // admin 带 Basic 请求 repository:hub/app:pull,push → 200，令牌可推送
    let json = fetch_token(&fx, Some(&basic("admin", "pw")), "repository:hub/app:pull,push").await;
    let token = json["token"].as_str().expect("应含 token");
    assert_eq!(json["access_token"], token, "access_token 应与 token 一致");
    assert!(json["expires_in"].as_u64().unwrap() > 0);

    // 用同一密钥校验令牌：access 应含 pull 与 push（管理员对 public 仓库读写皆放行）
    let signer = JwtSigner::from_secret(b"docker-secret-32-bytes-xxxxxxxxxx", 3600);
    let claims = signer.verify_docker_token(token).expect("令牌应可校验");
    assert_eq!(claims.sub, "admin");
    let app = claims
        .access
        .iter()
        .find(|a| a.name == "hub/app")
        .expect("应含 hub/app 授权");
    assert!(app.actions.contains(&"pull".to_string()), "应授予 pull");
    assert!(app.actions.contains(&"push".to_string()), "应授予 push");

    // 用该 Bearer 令牌推 manifest → 201（经令牌流，无需再带 Basic）
    let manifest = br#"{"schemaVersion":2}"#;
    let put = fx
        .router()
        .oneshot(req(
            "PUT",
            "/v2/hub/app/manifests/1.0",
            Some(&bearer(token)),
            Some(MANIFEST_V2),
            manifest.to_vec(),
        ))
        .await
        .unwrap();
    assert_eq!(put.status(), StatusCode::CREATED, "Bearer 令牌推送应 201");
}

#[tokio::test]
async fn 令牌端点_无写权限用户的令牌不能推送() {
    let fx = Fixture::new().await;
    fx.seed_user("admin", "pw", Role::Admin).await;
    let reader = fx.seed_user("reader", "pw", Role::User).await;
    let rid = fx.seed_docker_repo("priv", Visibility::Private).await;
    fx.seed_acl(&rid, &reader, Permission::Read).await;

    // 仅读 ACL 用户请求 pull,push：令牌只应授予 pull（push 被拒，不入令牌）
    let json = fetch_token(&fx, Some(&basic("reader", "pw")), "repository:priv/app:pull,push").await;
    let token = json["token"].as_str().unwrap();
    let signer = JwtSigner::from_secret(b"docker-secret-32-bytes-xxxxxxxxxx", 3600);
    let claims = signer.verify_docker_token(token).unwrap();
    let app = claims.access.iter().find(|a| a.name == "priv/app").unwrap();
    assert!(app.actions.contains(&"pull".to_string()), "应授予 pull");
    assert!(!app.actions.contains(&"push".to_string()), "不应授予 push");

    // 该令牌推送 manifest → 403（令牌不含 push，回退既有 identity 也无写）
    let put = fx
        .router()
        .oneshot(req(
            "PUT",
            "/v2/priv/app/manifests/1.0",
            Some(&bearer(token)),
            Some(MANIFEST_V2),
            br#"{"schemaVersion":2}"#.to_vec(),
        ))
        .await
        .unwrap();
    assert_eq!(put.status(), StatusCode::FORBIDDEN, "无 push 授予的令牌不得推送");

    // 但该令牌含 pull：可读（管理员先写一个）
    let admin_auth = basic("admin", "pw");
    fx.router()
        .oneshot(req(
            "PUT",
            "/v2/priv/app/manifests/1.0",
            Some(&admin_auth),
            Some(MANIFEST_V2),
            br#"{"schemaVersion":2}"#.to_vec(),
        ))
        .await
        .unwrap();
    let get = fx
        .router()
        .oneshot(req(
            "GET",
            "/v2/priv/app/manifests/1.0",
            Some(&bearer(token)),
            None,
            Vec::new(),
        ))
        .await
        .unwrap();
    assert_eq!(get.status(), StatusCode::OK, "含 pull 授予的令牌应可读");
}

#[tokio::test]
async fn 令牌端点_匿名请求_public_仓库可拉取() {
    let fx = Fixture::new().await;
    fx.seed_user("admin", "pw", Role::Admin).await;
    fx.seed_docker_repo("hub", Visibility::Public).await;
    // 管理员先写 manifest
    let admin_auth = basic("admin", "pw");
    fx.router()
        .oneshot(req(
            "PUT",
            "/v2/hub/app/manifests/1.0",
            Some(&admin_auth),
            Some(MANIFEST_V2),
            br#"{"schemaVersion":2}"#.to_vec(),
        ))
        .await
        .unwrap();

    // 匿名（无 Authorization）请求 public 仓库 pull → 令牌端点 200，签发含 pull 的令牌
    let json = fetch_token(&fx, None, "repository:hub/app:pull").await;
    let token = json["token"].as_str().unwrap();
    let get = fx
        .router()
        .oneshot(req(
            "GET",
            "/v2/hub/app/manifests/1.0",
            Some(&bearer(token)),
            None,
            Vec::new(),
        ))
        .await
        .unwrap();
    assert_eq!(get.status(), StatusCode::OK, "匿名 public 读令牌应可拉取");
}

#[tokio::test]
async fn 令牌端点_错误_basic_凭据返回_401() {
    let fx = Fixture::new().await;
    fx.seed_user("admin", "pw", Role::Admin).await;
    fx.seed_docker_repo("hub", Visibility::Public).await;

    // 提供了 Basic 凭据但口令错误 → 401（不降级为匿名签空令牌）
    let resp = fx
        .router()
        .oneshot(req(
            "GET",
            "/v2/token?service=jianartifact&scope=repository:hub/app:pull,push",
            Some(&basic("admin", "wrong-pw")),
            None,
            Vec::new(),
        ))
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::UNAUTHORIZED);
    assert!(header_str(&resp, "www-authenticate").is_some());
}

#[tokio::test]
async fn 令牌_scope_不含目标仓库则推送被拒() {
    let fx = Fixture::new().await;
    fx.seed_user("admin", "pw", Role::Admin).await;
    fx.seed_docker_repo("hub", Visibility::Public).await;

    // 令牌只授予 hub/other，却用于推 hub/app
    let json = fetch_token(&fx, Some(&basic("admin", "pw")), "repository:hub/other:pull,push").await;
    let token = json["token"].as_str().unwrap();
    let put = fx
        .router()
        .oneshot(req(
            "PUT",
            "/v2/hub/app/manifests/1.0",
            Some(&bearer(token)),
            Some(MANIFEST_V2),
            br#"{"schemaVersion":2}"#.to_vec(),
        ))
        .await
        .unwrap();
    // 令牌已认证（sub=admin）但不覆盖 hub/app：按已认证语义 404 隐藏存在性（既无 push 也无 pull）
    assert_eq!(
        put.status(),
        StatusCode::NOT_FOUND,
        "scope 不含目标仓库的令牌不得推送，按已认证语义 404 隐藏"
    );
}

#[tokio::test]
async fn 伪造或过期_docker_令牌推送被拒() {
    let fx = Fixture::new().await;
    fx.seed_user("admin", "pw", Role::Admin).await;
    fx.seed_docker_repo("hub", Visibility::Public).await;

    // 伪造令牌（非本服务签发）作为 Bearer → 既非有效 docker 令牌、也非有效身份 → 匿名写 401
    let put = fx
        .router()
        .oneshot(req(
            "PUT",
            "/v2/hub/app/manifests/1.0",
            Some(&bearer("not-a-valid-token.payload.sig")),
            Some(MANIFEST_V2),
            br#"{"schemaVersion":2}"#.to_vec(),
        ))
        .await
        .unwrap();
    assert_eq!(put.status(), StatusCode::UNAUTHORIZED, "伪造令牌不得推送");
}

#[tokio::test]
async fn 匿名_public_读回归_仍_200() {
    let fx = Fixture::new().await;
    fx.seed_user("admin", "pw", Role::Admin).await;
    fx.seed_docker_repo("hub", Visibility::Public).await;
    let auth = basic("admin", "pw");
    let manifest = br#"{"schemaVersion":2}"#;
    fx.router()
        .oneshot(req(
            "PUT",
            "/v2/hub/app/manifests/1.0",
            Some(&auth),
            Some(MANIFEST_V2),
            manifest.to_vec(),
        ))
        .await
        .unwrap();

    // 无任何凭据、无令牌：public 仓库读仍 200（tokenless 拉取不被破坏）
    let get = fx
        .router()
        .oneshot(req("GET", "/v2/hub/app/manifests/1.0", None, None, Vec::new()))
        .await
        .unwrap();
    assert_eq!(get.status(), StatusCode::OK, "匿名 public 读应保持 200");
    assert_eq!(body_bytes(get).await, manifest);
}
