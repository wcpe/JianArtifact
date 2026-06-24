//! 用户组/团队与组级 ACL 的 HTTP 集成测试（FR-49 / ADR-0007）。
//!
//! 穷举组授权对鉴权矩阵的影响（#1 高风险区）：用户经组获得权限、退组后失权、
//! 组动作蕴含（read < write < delete < admin）、组 ACL 与直接 ACL 并集、组管理端点
//! 的 Admin-only 与 404/409 边界，并三身份通道（Bearer-JWT / Bearer-Token / Basic）
//! 各走一遍经组继承的私有读判定，确保任一通道不绕过组授权判定。

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
use jianartifact::meta::{MetaStore, Permission, Role, Visibility};
use jianartifact::proxy::HttpUpstream;
use jianartifact::storage::{BlobBackend, LocalFsStore};

/// 测试夹具：走真实 SQLite 文件，验证迁移、外键级联与组授权链路。
struct Fixture {
    state: AppState,
    _dir: tempfile::TempDir,
}

impl Fixture {
    async fn new() -> Self {
        let dir = tempfile::tempdir().unwrap();
        let meta = MetaStore::open(&dir.path().join("test.db")).await.unwrap();
        let store = BlobBackend::Fs(LocalFsStore::new(dir.path().join("blobs")).await.unwrap());
        let jwt = JwtSigner::from_secret(b"group-authz-secret-32-bytes-xxxxx", 3600);
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

    async fn seed_repo(&self, name: &str, visibility: Visibility) -> String {
        self.state
            .meta
            .create_repository(jianartifact::meta::NewRepository {
                name,
                format: "raw",
                r#type: jianartifact::meta::RepoType::Hosted,
                visibility,
                upstream_url: None,
                upstream_auth_ref: None,
            })
            .await
            .unwrap()
    }

    async fn seed_group(&self, name: &str) -> String {
        self.state.meta.create_group(name).await.unwrap()
    }

    async fn seed_member(&self, group_id: &str, user_id: &str) {
        self.state
            .meta
            .add_group_member(group_id, user_id)
            .await
            .unwrap();
    }

    async fn seed_group_acl(&self, repo_id: &str, group_id: &str, permission: Permission) {
        self.state
            .meta
            .create_group_acl(repo_id, group_id, permission)
            .await
            .unwrap();
    }
}

async fn send(router: Router, req: Request<Body>) -> (StatusCode, Value) {
    let resp = router.oneshot(req).await.unwrap();
    let status = resp.status();
    let bytes = resp.into_body().collect().await.unwrap().to_bytes();
    let body = serde_json::from_slice(&bytes).unwrap_or(Value::Null);
    (status, body)
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

// ---------- FR-49 用户组经组继承权限：私有仓库读浏览 ----------

#[tokio::test]
async fn 用户经组对私有仓库获得读权限() {
    let fx = Fixture::new().await;
    let member = fx.seed_user("member", "pw", Role::User).await;
    fx.seed_user("outsider", "pw", Role::User).await;
    let gid = fx.seed_group("readers").await;
    let rid = fx.seed_repo("priv", Visibility::Private).await;
    // 组被授读权限，member 是组成员，outsider 不是
    fx.seed_member(&gid, &member).await;
    fx.seed_group_acl(&rid, &gid, Permission::Read).await;

    // 成员经组获得读 → 200
    let member_auth = format!("Bearer {}", login_token(&fx, "member", "pw").await);
    let (status, _) = send(
        fx.router(),
        empty_req(
            "GET",
            &format!("/api/v1/repositories/{rid}/artifacts"),
            Some(&member_auth),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::OK, "组成员经组继承读权限应可浏览");

    // 非成员 → 404（私有对无权一律隐藏存在性）
    let out_auth = format!("Bearer {}", login_token(&fx, "outsider", "pw").await);
    let (status, _) = send(
        fx.router(),
        empty_req(
            "GET",
            &format!("/api/v1/repositories/{rid}/artifacts"),
            Some(&out_auth),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::NOT_FOUND, "非组成员应 404");
}

#[tokio::test]
async fn 退组后失去经组继承的权限() {
    let fx = Fixture::new().await;
    let member = fx.seed_user("member", "pw", Role::User).await;
    let gid = fx.seed_group("readers").await;
    let rid = fx.seed_repo("priv", Visibility::Private).await;
    fx.seed_member(&gid, &member).await;
    fx.seed_group_acl(&rid, &gid, Permission::Read).await;
    let uri = format!("/api/v1/repositories/{rid}/artifacts");
    let member_auth = format!("Bearer {}", login_token(&fx, "member", "pw").await);

    // 在组内：可读
    let (status, _) = send(fx.router(), empty_req("GET", &uri, Some(&member_auth))).await;
    assert_eq!(status, StatusCode::OK);

    // 退组后即失权 → 404
    fx.state
        .meta
        .remove_group_member(&gid, &member)
        .await
        .unwrap();
    let (status, _) = send(fx.router(), empty_req("GET", &uri, Some(&member_auth))).await;
    assert_eq!(status, StatusCode::NOT_FOUND, "退组后应失去读权限");
}

#[tokio::test]
async fn 组动作蕴含_写蕴含读_仅读不得越权写() {
    let fx = Fixture::new().await;
    let writer = fx.seed_user("writer", "pw", Role::User).await;
    let reader = fx.seed_user("reader", "pw", Role::User).await;
    let gw = fx.seed_group("writers").await;
    let gr = fx.seed_group("readers").await;
    let rid = fx.seed_repo("priv", Visibility::Private).await;
    fx.seed_member(&gw, &writer).await;
    fx.seed_member(&gr, &reader).await;
    // writers 组被授 write（蕴含读），readers 组被授 read
    fx.seed_group_acl(&rid, &gw, Permission::Write).await;
    fx.seed_group_acl(&rid, &gr, Permission::Read).await;
    let detail_uri = format!("/api/v1/repositories/{rid}");

    // writer 经组得写（蕴含读）：详情可见
    let writer_auth = format!("Bearer {}", login_token(&fx, "writer", "pw").await);
    let (status, _) = send(
        fx.router(),
        empty_req("GET", &detail_uri, Some(&writer_auth)),
    )
    .await;
    assert_eq!(status, StatusCode::OK, "写蕴含读，详情应可见");

    // writer 删除制品（需写权限）：制品不存在但鉴权先过——返回 404（仓库可读可写，制品不存在）
    let (status, _) = send(
        fx.router(),
        empty_req(
            "DELETE",
            &format!("/api/v1/repositories/{rid}/artifacts/无此制品.txt"),
            Some(&writer_auth),
        ),
    )
    .await;
    assert_eq!(
        status,
        StatusCode::NOT_FOUND,
        "有写权限但制品不存在应 404（非 403）"
    );

    // reader 经组仅得读：删除制品应被 403（有读无写不得越权写）
    let reader_auth = format!("Bearer {}", login_token(&fx, "reader", "pw").await);
    let (status, _) = send(
        fx.router(),
        empty_req(
            "DELETE",
            &format!("/api/v1/repositories/{rid}/artifacts/无此制品.txt"),
            Some(&reader_auth),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::FORBIDDEN, "仅读经组不得越权写");
}

#[tokio::test]
async fn 直接acl与组acl取并集() {
    // 用户直接被授 read，又经组被授 write：并集后应可写（取较高动作）。
    let fx = Fixture::new().await;
    let user = fx.seed_user("u", "pw", Role::User).await;
    let gid = fx.seed_group("g").await;
    let rid = fx.seed_repo("priv", Visibility::Private).await;
    fx.seed_member(&gid, &user).await;
    // 直接 ACL 给 read，组 ACL 给 write
    fx.state
        .meta
        .create_acl(&rid, &user, Permission::Read)
        .await
        .unwrap();
    fx.seed_group_acl(&rid, &gid, Permission::Write).await;

    // 删除制品（需写）：并集得写，制品不存在 → 404 而非 403
    let auth = format!("Bearer {}", login_token(&fx, "u", "pw").await);
    let (status, _) = send(
        fx.router(),
        empty_req(
            "DELETE",
            &format!("/api/v1/repositories/{rid}/artifacts/无此制品.txt"),
            Some(&auth),
        ),
    )
    .await;
    assert_eq!(
        status,
        StatusCode::NOT_FOUND,
        "直接读 ∪ 组写 应得写权限（制品不存在故 404）"
    );
}

#[tokio::test]
async fn 经组继承使私有仓库出现在列表中() {
    let fx = Fixture::new().await;
    let member = fx.seed_user("member", "pw", Role::User).await;
    let gid = fx.seed_group("g").await;
    let _pub_id = fx.seed_repo("pub", Visibility::Public).await;
    let priv_g = fx.seed_repo("priv-g", Visibility::Private).await;
    let _priv_other = fx.seed_repo("priv-other", Visibility::Private).await;
    fx.seed_member(&gid, &member).await;
    // 组在 priv-g 被授读
    fx.seed_group_acl(&priv_g, &gid, Permission::Read).await;

    // member 列表应含 public + 经组可读的 priv-g，不含 priv-other
    let auth = format!("Bearer {}", login_token(&fx, "member", "pw").await);
    let (status, list) = send(
        fx.router(),
        empty_req("GET", "/api/v1/repositories", Some(&auth)),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    let mut names: Vec<&str> = list
        .as_array()
        .unwrap()
        .iter()
        .map(|r| r["name"].as_str().unwrap())
        .collect();
    names.sort();
    assert_eq!(names, vec!["priv-g", "pub"]);
}

// ---------- 三身份通道经组继承读判定一致（避免某通道绕过组授权） ----------

#[tokio::test]
async fn 三身份通道对经组继承读判定一致() {
    let fx = Fixture::new().await;
    let reader = fx.seed_user("reader", "pw", Role::User).await;
    let gid = fx.seed_group("g").await;
    let rid = fx.seed_repo("priv", Visibility::Private).await;
    fx.seed_member(&gid, &reader).await;
    fx.seed_group_acl(&rid, &gid, Permission::Read).await;
    let uri = format!("/api/v1/repositories/{rid}");

    // 通道一：Bearer-JWT
    let jwt = login_token(&fx, "reader", "pw").await;
    let (status, _) = send(
        fx.router(),
        empty_req("GET", &uri, Some(&format!("Bearer {jwt}"))),
    )
    .await;
    assert_eq!(status, StatusCode::OK, "JWT 通道经组继承应可读");

    // 通道二：Bearer API Token
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
    let (status, _) = send(
        fx.router(),
        empty_req("GET", &uri, Some(&format!("Bearer {plaintext}"))),
    )
    .await;
    assert_eq!(status, StatusCode::OK, "API Token 通道经组继承应可读");

    // 通道三：Basic
    let basic = format!("Basic {}", STANDARD.encode("reader:pw"));
    let (status, _) = send(fx.router(), empty_req("GET", &uri, Some(&basic))).await;
    assert_eq!(status, StatusCode::OK, "Basic 通道经组继承应可读");
}

// ---------- FR-49 组管理端点：Admin-only 与边界 ----------

#[tokio::test]
async fn 管理员可建组加移成员授撤组acl() {
    let fx = Fixture::new().await;
    fx.seed_user("admin", "pw", Role::Admin).await;
    let target = fx.seed_user("dev", "pw", Role::User).await;
    let auth = format!("Bearer {}", login_token(&fx, "admin", "pw").await);
    let rid = fx.seed_repo("r", Visibility::Private).await;

    // 建组
    let (status, group) = send(
        fx.router(),
        json_req(
            "POST",
            "/api/v1/groups",
            Some(&auth),
            json!({ "name": "team" }),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::CREATED);
    assert_eq!(group["name"], "team");
    let gid = group["id"].as_str().unwrap().to_string();

    // 重名建组 → 409
    let (status, body) = send(
        fx.router(),
        json_req(
            "POST",
            "/api/v1/groups",
            Some(&auth),
            json!({ "name": "team" }),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::CONFLICT);
    assert_eq!(body["error"]["code"], "conflict");

    // 加成员
    let (status, _) = send(
        fx.router(),
        json_req(
            "POST",
            &format!("/api/v1/groups/{gid}/members"),
            Some(&auth),
            json!({ "user_id": target }),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::CREATED);

    // 重复加成员 → 409
    let (status, _) = send(
        fx.router(),
        json_req(
            "POST",
            &format!("/api/v1/groups/{gid}/members"),
            Some(&auth),
            json!({ "user_id": target }),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::CONFLICT);

    // 列成员含一条
    let (status, members) = send(
        fx.router(),
        empty_req("GET", &format!("/api/v1/groups/{gid}/members"), Some(&auth)),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(members.as_array().unwrap().len(), 1);
    assert_eq!(members[0]["username"], "dev");

    // 对组授仓库 ACL
    let (status, acl) = send(
        fx.router(),
        json_req(
            "POST",
            &format!("/api/v1/repositories/{rid}/group-acl"),
            Some(&auth),
            json!({ "group_id": gid, "permission": "write" }),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::CREATED);
    assert_eq!(acl["permission"], "write");
    let acl_id = acl["id"].as_str().unwrap().to_string();

    // 重复授同类 → 409
    let (status, _) = send(
        fx.router(),
        json_req(
            "POST",
            &format!("/api/v1/repositories/{rid}/group-acl"),
            Some(&auth),
            json!({ "group_id": gid, "permission": "write" }),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::CONFLICT);

    // 列组 ACL 含一条
    let (_, list) = send(
        fx.router(),
        empty_req(
            "GET",
            &format!("/api/v1/repositories/{rid}/group-acl"),
            Some(&auth),
        ),
    )
    .await;
    assert_eq!(list.as_array().unwrap().len(), 1);

    // 撤组 ACL
    let (status, _) = send(
        fx.router(),
        empty_req(
            "DELETE",
            &format!("/api/v1/repositories/{rid}/group-acl/{acl_id}"),
            Some(&auth),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::OK);

    // 移成员
    let (status, _) = send(
        fx.router(),
        empty_req(
            "DELETE",
            &format!("/api/v1/groups/{gid}/members/{target}"),
            Some(&auth),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::OK);

    // 删组
    let (status, _) = send(
        fx.router(),
        empty_req("DELETE", &format!("/api/v1/groups/{gid}"), Some(&auth)),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    let (status, _) = send(
        fx.router(),
        empty_req("GET", &format!("/api/v1/groups/{gid}"), Some(&auth)),
    )
    .await;
    assert_eq!(status, StatusCode::NOT_FOUND);
}

#[tokio::test]
async fn 组端点对不存在资源_404() {
    let fx = Fixture::new().await;
    fx.seed_user("admin", "pw", Role::Admin).await;
    let auth = format!("Bearer {}", login_token(&fx, "admin", "pw").await);
    let gid = fx.seed_group("g").await;
    let rid = fx.seed_repo("r", Visibility::Private).await;

    // 加不存在用户为成员 → 404
    let (status, _) = send(
        fx.router(),
        json_req(
            "POST",
            &format!("/api/v1/groups/{gid}/members"),
            Some(&auth),
            json!({ "user_id": "无此用户" }),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::NOT_FOUND);

    // 对不存在组授仓库 ACL → 404
    let (status, _) = send(
        fx.router(),
        json_req(
            "POST",
            &format!("/api/v1/repositories/{rid}/group-acl"),
            Some(&auth),
            json!({ "group_id": "无此组", "permission": "read" }),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::NOT_FOUND);

    // 移出本不在组内的用户 → 404
    let (status, _) = send(
        fx.router(),
        empty_req(
            "DELETE",
            &format!("/api/v1/groups/{gid}/members/无此用户"),
            Some(&auth),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::NOT_FOUND);
}

#[tokio::test]
async fn 非管理员访问组端点_403_匿名_401() {
    let fx = Fixture::new().await;
    fx.seed_user("user", "pw", Role::User).await;
    let user = format!("Bearer {}", login_token(&fx, "user", "pw").await);
    let gid = fx.seed_group("g").await;
    let rid = fx.seed_repo("r", Visibility::Private).await;

    // 普通用户列组 → 403
    let (status, _) = send(fx.router(), empty_req("GET", "/api/v1/groups", Some(&user))).await;
    assert_eq!(status, StatusCode::FORBIDDEN);

    // 普通用户对组授 ACL → 403
    let (status, _) = send(
        fx.router(),
        json_req(
            "POST",
            &format!("/api/v1/repositories/{rid}/group-acl"),
            Some(&user),
            json!({ "group_id": gid, "permission": "read" }),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::FORBIDDEN);

    // 匿名列组 → 401
    let (status, _) = send(fx.router(), empty_req("GET", "/api/v1/groups", None)).await;
    assert_eq!(status, StatusCode::UNAUTHORIZED);
}

#[tokio::test]
async fn 删组后经组继承的权限即失效() {
    // 删组应级联清成员与组 ACL，原成员立即失去经组继承的读权限。
    let fx = Fixture::new().await;
    let member = fx.seed_user("member", "pw", Role::User).await;
    let gid = fx.seed_group("g").await;
    let rid = fx.seed_repo("priv", Visibility::Private).await;
    fx.seed_member(&gid, &member).await;
    fx.seed_group_acl(&rid, &gid, Permission::Read).await;
    let uri = format!("/api/v1/repositories/{rid}/artifacts");
    let auth = format!("Bearer {}", login_token(&fx, "member", "pw").await);

    let (status, _) = send(fx.router(), empty_req("GET", &uri, Some(&auth))).await;
    assert_eq!(status, StatusCode::OK);

    // 删组后即失权
    fx.state.meta.delete_group(&gid).await.unwrap();
    let (status, _) = send(fx.router(), empty_req("GET", &uri, Some(&auth))).await;
    assert_eq!(status, StatusCode::NOT_FOUND, "删组后原成员应失权");
}
