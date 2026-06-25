//! 身份解析中间件：统一识别 Bearer(JWT / API Token) / Basic / 无凭据，
//! 解析出 `AuthIdentity` 注入请求扩展，供后续 handler 与 authz 批次使用。
//!
//! 本中间件只解析“是谁”（认证），不做仓库读写判定（鉴权属 authz 批次）。
//! 凭据无效不在此直接拒绝，而是按匿名注入——是否要求认证由各 handler 决定，
//! 以便公开资源对匿名放行、私有资源在鉴权层按既定语义拒绝。

use axum::{
    extract::{Request, State},
    http::header::AUTHORIZATION,
    middleware::Next,
    response::Response,
};

use std::sync::Arc;

use crate::auth::{self, AuthIdentity, AuthUser, LdapProvider};
use crate::meta::{MetaStore, Role};

use super::AppState;

/// NuGet 规范 api-key 请求头名：`dotnet nuget push` 经此头携带 API Token（不发 `Authorization`）。
const NUGET_API_KEY_HEADER: &str = "X-NuGet-ApiKey";

/// LDAP 登录上下文：Basic Auth 口令通道本地校验失败后委托 LDAP bind 校验所需的最小依赖。
///
/// 仅在配置了 `[auth.ldap]` 时构造；持有 provider 与 JIT 开关（守 ADR-0010 默认关）。
pub struct LdapAuthContext {
    /// LDAP 认证 provider。
    pub provider: Arc<LdapProvider>,
    /// 是否即时开通（JIT）：默认关闭，无对应本地用户则拒登录。
    pub auto_provision: bool,
}

impl LdapAuthContext {
    /// 从应用状态装配 LDAP 上下文；未配置 LDAP 时返回 None。
    pub fn from_state(state: &AppState) -> Option<Self> {
        let provider = state.ldap.clone()?;
        let auto_provision = state
            .config
            .auth
            .ldap
            .as_ref()
            .map(|c| c.auto_provision)
            .unwrap_or(false);
        Some(Self {
            provider,
            auto_provision,
        })
    }
}

/// axum 中间件入口：解析身份后注入扩展并放行。
pub async fn identity_layer(
    State(state): State<AppState>,
    mut request: Request,
    next: Next,
) -> Response {
    let header = request
        .headers()
        .get(AUTHORIZATION)
        .and_then(|v| v.to_str().ok())
        .map(str::to_owned);

    let ldap = LdapAuthContext::from_state(&state);
    let mut identity = match header {
        Some(value) => resolve_identity(&state.meta, &state.jwt, &value, ldap.as_ref()).await,
        None => AuthIdentity::Anonymous,
    };

    // NuGet 规范 api-key 头：`dotnet nuget push -k <token>` 仅经 `X-NuGet-ApiKey` 头携带凭据、
    // 不发 `Authorization`。当 `Authorization` 未解析出身份时，回退按 API Token 校验该头值，
    // 使 dotnet 客户端原生互通；非法 key 仍回退匿名、不绕过鉴权。
    if !identity.is_authenticated() {
        if let Some(key) = request
            .headers()
            .get(NUGET_API_KEY_HEADER)
            .and_then(|v| v.to_str().ok())
            .map(str::trim)
            .filter(|k| !k.is_empty())
        {
            identity = resolve_api_token(&state.meta, key).await;
        }
    }

    request.extensions_mut().insert(identity);
    next.run(request).await
}

/// 从 `Authorization` 头值解析身份；任何无效 / 缺失凭据都回退为匿名。
///
/// 解析顺序：Bearer → 先按 JWT 校验，失败再按 API Token 校验；Basic → 口令或 Token；
/// 无识别 scheme 前缀时，按 Cargo registry 约定把整个头值当作裸 API Token 校验。
/// 命中的用户若已禁用，则不授予身份（按匿名处理），避免禁用账户继续访问。
pub async fn resolve_identity(
    meta: &MetaStore,
    jwt: &auth::JwtSigner,
    header_value: &str,
    ldap: Option<&LdapAuthContext>,
) -> AuthIdentity {
    if let Some(rest) = auth::basic::strip_scheme_prefix(header_value, "Bearer ") {
        return resolve_bearer(meta, jwt, rest.trim()).await;
    }
    if auth::basic::strip_scheme_prefix(header_value, "Basic ").is_some() {
        if let Some(creds) = auth::parse_basic_auth(header_value) {
            return resolve_basic(meta, &creds.username, &creds.secret, ldap).await;
        }
        return AuthIdentity::Anonymous;
    }
    // 无 Bearer / Basic scheme：Cargo registry 客户端把 API Token 裸放进 Authorization 头
    // （`Authorization: <token>`，无 scheme 前缀），按 API Token 校验；非法则回退匿名。
    let raw = header_value.trim();
    if raw.is_empty() {
        return AuthIdentity::Anonymous;
    }
    resolve_api_token(meta, raw).await
}

/// 解析 Bearer：先试 JWT（会话），失败再试 API Token。
async fn resolve_bearer(meta: &MetaStore, jwt: &auth::JwtSigner, raw: &str) -> AuthIdentity {
    // 路径一：JWT 会话凭据
    if let Ok(claims) = jwt.verify(raw) {
        // JWT 内含角色，但仍需确认用户存在且未禁用（吊销 / 禁用即时生效）
        if let Ok(Some(user)) = meta.get_user_by_id(&claims.sub).await {
            if user.disabled == 0 {
                return authenticated(user.id, user.username, &user.role);
            }
        }
        return AuthIdentity::Anonymous;
    }
    // 路径二：API Token（哈希比对）
    resolve_api_token(meta, raw).await
}

/// 解析 API Token：按哈希查未吊销 Token 的所属身份。
async fn resolve_api_token(meta: &MetaStore, raw: &str) -> AuthIdentity {
    let hash = auth::hash_api_token(raw);
    match meta.get_token_identity_by_hash(&hash).await {
        Ok(Some(ident)) if ident.disabled == 0 => {
            // 命中即更新最近使用时间（失败不阻断鉴权，仅记日志）
            if let Err(e) = meta.touch_token_last_used(&ident.token_id).await {
                tracing::warn!(错误 = %e, "更新 Token 最近使用时间失败");
            }
            authenticated(ident.user_id, ident.username, &ident.role)
        }
        _ => AuthIdentity::Anonymous,
    }
}

/// 解析 Basic：secret 先按用户口令（argon2）校验，失败再按 API Token 校验，
/// 最后（配置了 LDAP 时）委托 LDAP bind 校验。
async fn resolve_basic(
    meta: &MetaStore,
    username: &str,
    secret: &str,
    ldap: Option<&LdapAuthContext>,
) -> AuthIdentity {
    // 路径一：用户名 + 本地口令
    if let Ok(Some(user)) = meta.get_user_by_username(username).await {
        if user.disabled == 0 && auth::verify_password(secret, &user.password_hash) {
            return authenticated(user.id, user.username, &user.role);
        }
    }
    // 路径二：secret 作为 API Token（兼容包管理器把 Token 当密码填）
    let token_identity = resolve_api_token(meta, secret).await;
    if token_identity.is_authenticated() {
        return token_identity;
    }
    // 路径三：LDAP bind 校验（FR-35 / ADR-0016）——仅在配置了 LDAP 时尝试；
    // 成功经既有 JIT 映射得本地用户，失败 / 被拒回退匿名（不泄露细节）。
    resolve_ldap(meta, username, secret, ldap).await
}

/// 经 LDAP provider 做 Basic Auth 口令校验：未配置 LDAP 直接匿名；
/// 配置了则做 bind + JIT 映射，成功授予身份（拒绝已禁用账户），失败回退匿名。
async fn resolve_ldap(
    meta: &MetaStore,
    username: &str,
    secret: &str,
    ldap: Option<&LdapAuthContext>,
) -> AuthIdentity {
    let Some(ctx) = ldap else {
        return AuthIdentity::Anonymous;
    };
    match auth::ldap_login(meta, &ctx.provider, username, secret, ctx.auto_provision).await {
        Ok(user) if user.disabled == 0 => authenticated(user.id, user.username, &user.role),
        Ok(_) => AuthIdentity::Anonymous,
        Err(e) => {
            tracing::warn!(用户名 = %username, 原因 = %e, "LDAP Basic Auth 校验失败");
            AuthIdentity::Anonymous
        }
    }
}

/// 组装已认证身份。
fn authenticated(user_id: String, username: String, role_str: &str) -> AuthIdentity {
    AuthIdentity::Authenticated(AuthUser {
        user_id,
        username,
        role: Role::from_db_str(role_str),
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::auth::{generate_api_token, hash_api_token, hash_password, JwtSigner};

    async fn 新建库() -> MetaStore {
        MetaStore::open_in_memory().await.unwrap()
    }

    fn 测试签名器() -> JwtSigner {
        JwtSigner::from_secret(b"identity-test-secret-xxxxxxxxxxxx", 3600)
    }

    #[tokio::test]
    async fn bearer_jwt_通道解析身份() {
        let meta = 新建库().await;
        let jwt = 测试签名器();
        let uid = meta
            .create_user("alice", &hash_password("pw").unwrap(), Role::Admin)
            .await
            .unwrap();
        let token = jwt.issue(&uid, "alice", Role::Admin).unwrap();

        let id = resolve_identity(&meta, &jwt, &format!("Bearer {token}"), None).await;
        let user = id.user().unwrap();
        assert_eq!(user.username, "alice");
        assert_eq!(user.role, Role::Admin);
    }

    #[tokio::test]
    async fn bearer_api_token_通道解析身份() {
        let meta = 新建库().await;
        let jwt = 测试签名器();
        let uid = meta
            .create_user("dev", &hash_password("pw").unwrap(), Role::User)
            .await
            .unwrap();
        let token = generate_api_token();
        meta.create_token(&uid, "ci", &hash_api_token(&token))
            .await
            .unwrap();

        let id = resolve_identity(&meta, &jwt, &format!("Bearer {token}"), None).await;
        assert_eq!(id.user().unwrap().username, "dev");
    }

    #[tokio::test]
    async fn 裸_token_通道解析身份() {
        // Cargo registry 把 API Token 裸放进 Authorization 头（无 Bearer/Basic 前缀）
        let meta = 新建库().await;
        let jwt = 测试签名器();
        let uid = meta
            .create_user("dev", &hash_password("pw").unwrap(), Role::User)
            .await
            .unwrap();
        let token = generate_api_token();
        meta.create_token(&uid, "cargo", &hash_api_token(&token))
            .await
            .unwrap();

        // 裸 Token（无 scheme 前缀）应解析出对应身份
        let id = resolve_identity(&meta, &jwt, &token, None).await;
        assert_eq!(id.user().unwrap().username, "dev");
        // 非法裸 Token 回退匿名
        assert_eq!(
            resolve_identity(&meta, &jwt, "不是有效的裸令牌", None).await,
            AuthIdentity::Anonymous
        );
    }

    #[tokio::test]
    async fn basic_口令通道解析身份() {
        use base64::{engine::general_purpose::STANDARD, Engine};
        let meta = 新建库().await;
        let jwt = 测试签名器();
        meta.create_user("bob", &hash_password("s3cret").unwrap(), Role::User)
            .await
            .unwrap();
        let header = format!("Basic {}", STANDARD.encode("bob:s3cret"));

        let id = resolve_identity(&meta, &jwt, &header, None).await;
        assert_eq!(id.user().unwrap().username, "bob");
    }

    #[tokio::test]
    async fn basic_token_通道解析身份() {
        use base64::{engine::general_purpose::STANDARD, Engine};
        let meta = 新建库().await;
        let jwt = 测试签名器();
        let uid = meta
            .create_user("bob", &hash_password("pw").unwrap(), Role::User)
            .await
            .unwrap();
        let token = generate_api_token();
        meta.create_token(&uid, "ci", &hash_api_token(&token))
            .await
            .unwrap();
        // 包管理器常把 Token 当作 Basic 的密码字段
        let header = format!("Basic {}", STANDARD.encode(format!("bob:{token}")));

        let id = resolve_identity(&meta, &jwt, &header, None).await;
        assert_eq!(id.user().unwrap().username, "bob");
    }

    #[tokio::test]
    async fn 无凭据与错误凭据均为匿名() {
        let meta = 新建库().await;
        let jwt = 测试签名器();
        assert_eq!(
            resolve_identity(&meta, &jwt, "Bearer 不是有效令牌", None).await,
            AuthIdentity::Anonymous
        );
        assert_eq!(
            resolve_identity(&meta, &jwt, "Basic !!!非法", None).await,
            AuthIdentity::Anonymous
        );
        assert_eq!(
            resolve_identity(&meta, &jwt, "Unknown scheme", None).await,
            AuthIdentity::Anonymous
        );
    }

    #[tokio::test]
    async fn 禁用用户的口令不授予身份() {
        use base64::{engine::general_purpose::STANDARD, Engine};
        let meta = 新建库().await;
        let jwt = 测试签名器();
        let uid = meta
            .create_user("bob", &hash_password("pw").unwrap(), Role::User)
            .await
            .unwrap();
        meta.update_user(&uid, None, Some(true)).await.unwrap();
        let header = format!("Basic {}", STANDARD.encode("bob:pw"));
        // 禁用账户即便口令正确也不授予身份
        assert_eq!(
            resolve_identity(&meta, &jwt, &header, None).await,
            AuthIdentity::Anonymous
        );
    }

    #[tokio::test]
    async fn 吊销的_token_不授予身份() {
        let meta = 新建库().await;
        let jwt = 测试签名器();
        let uid = meta
            .create_user("dev", &hash_password("pw").unwrap(), Role::User)
            .await
            .unwrap();
        let token = generate_api_token();
        let tid = meta
            .create_token(&uid, "ci", &hash_api_token(&token))
            .await
            .unwrap();
        meta.revoke_token(&tid).await.unwrap();
        assert_eq!(
            resolve_identity(&meta, &jwt, &format!("Bearer {token}"), None).await,
            AuthIdentity::Anonymous
        );
    }
}
