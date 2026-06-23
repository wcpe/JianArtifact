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

use crate::auth::{self, AuthIdentity, AuthUser};
use crate::meta::{MetaStore, Role};

use super::AppState;

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

    let identity = match header {
        Some(value) => resolve_identity(&state.meta, &state.jwt, &value).await,
        None => AuthIdentity::Anonymous,
    };

    request.extensions_mut().insert(identity);
    next.run(request).await
}

/// 从 `Authorization` 头值解析身份；任何无效 / 缺失凭据都回退为匿名。
///
/// 解析顺序：Bearer → 先按 JWT 校验，失败再按 API Token 校验；Basic → 口令或 Token。
/// 命中的用户若已禁用，则不授予身份（按匿名处理），避免禁用账户继续访问。
pub async fn resolve_identity(
    meta: &MetaStore,
    jwt: &auth::JwtSigner,
    header_value: &str,
) -> AuthIdentity {
    if let Some(rest) = auth::basic::strip_scheme_prefix(header_value, "Bearer ") {
        return resolve_bearer(meta, jwt, rest.trim()).await;
    }
    if auth::basic::strip_scheme_prefix(header_value, "Basic ").is_some() {
        if let Some(creds) = auth::parse_basic_auth(header_value) {
            return resolve_basic(meta, &creds.username, &creds.secret).await;
        }
    }
    AuthIdentity::Anonymous
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

/// 解析 Basic：secret 先按用户口令（argon2）校验，失败再按 API Token 校验。
async fn resolve_basic(meta: &MetaStore, username: &str, secret: &str) -> AuthIdentity {
    // 路径一：用户名 + 口令
    if let Ok(Some(user)) = meta.get_user_by_username(username).await {
        if user.disabled == 0 && auth::verify_password(secret, &user.password_hash) {
            return authenticated(user.id, user.username, &user.role);
        }
    }
    // 路径二：secret 作为 API Token（兼容包管理器把 Token 当密码填）
    resolve_api_token(meta, secret).await
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

        let id = resolve_identity(&meta, &jwt, &format!("Bearer {token}")).await;
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

        let id = resolve_identity(&meta, &jwt, &format!("Bearer {token}")).await;
        assert_eq!(id.user().unwrap().username, "dev");
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

        let id = resolve_identity(&meta, &jwt, &header).await;
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

        let id = resolve_identity(&meta, &jwt, &header).await;
        assert_eq!(id.user().unwrap().username, "bob");
    }

    #[tokio::test]
    async fn 无凭据与错误凭据均为匿名() {
        let meta = 新建库().await;
        let jwt = 测试签名器();
        assert_eq!(
            resolve_identity(&meta, &jwt, "Bearer 不是有效令牌").await,
            AuthIdentity::Anonymous
        );
        assert_eq!(
            resolve_identity(&meta, &jwt, "Basic !!!非法").await,
            AuthIdentity::Anonymous
        );
        assert_eq!(
            resolve_identity(&meta, &jwt, "Unknown scheme").await,
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
            resolve_identity(&meta, &jwt, &header).await,
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
            resolve_identity(&meta, &jwt, &format!("Bearer {token}")).await,
            AuthIdentity::Anonymous
        );
    }
}
