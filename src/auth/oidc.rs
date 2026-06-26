//! OIDC 授权码流 + PKCE（FR-34 / ADR-0016）。
//!
//! 仅用于 Web 控制台登录：`/auth/oidc/login` 生成 `state` + PKCE + `nonce` 并重定向 IdP；
//! `/auth/oidc/callback` 校验 `state`、用 `code` 换 ID Token、**校验签名（JWKS）/ `iss` /
//! `aud` / `exp` / `nonce`**，解析外部身份（`sub` + 用户名）产出 [`AuthenticatedSubject`]。
//!
//! 凭据脱敏：`client_secret`、ID Token 等绝不进日志 / 错误响应 / DB 明文。
//! 网络 IO 走既有 reqwest（纯 rustls）；JSON 用 `bytes()` + serde_json 解析，不引新依赖。

use std::collections::HashMap;

use jsonwebtoken::{decode, decode_header, Algorithm, DecodingKey, Validation};
use serde::Deserialize;
use sha2::{Digest, Sha256};

use super::provider::{AuthenticatedSubject, ProviderKind};

/// PKCE `code_verifier` 字节数（RFC 7636 建议 43~128 字符；32 字节 base64url ≈ 43 字符）。
const PKCE_VERIFIER_BYTES: usize = 32;
/// `state` / `nonce` 随机字节数（防 CSRF / 重放，128 位高熵足够）。
const STATE_NONCE_BYTES: usize = 16;

/// OIDC 运行期配置（由应用配置 `[auth.oidc]` 装配；`client_secret` 真源 env/配置，不入库）。
#[derive(Debug, Clone)]
pub struct OidcSettings {
    /// IdP 签发者标识（issuer），同时用作 discovery 基址与 ID Token `iss` 校验值。
    pub issuer: String,
    /// 客户端 ID。
    pub client_id: String,
    /// 客户端密钥（敏感）；真源 env/配置，绝不入库 / 进日志。
    pub client_secret: String,
    /// 回调地址（须与 IdP 注册的 redirect_uri 完全一致）。
    pub redirect_uri: String,
    /// 是否即时开通（JIT）：默认关闭，无对应本地用户则拒登录（守 ADR-0010）。
    pub auto_provision: bool,
}

/// OIDC 相关错误。对外一律收敛为「外部认证失败」，不泄露 IdP 内部细节与凭据。
#[derive(Debug, thiserror::Error)]
pub enum OidcError {
    /// discovery / JWKS / token 端点网络或解析失败（含超时）。
    #[error("外部认证失败：与 IdP 交互出错")]
    Upstream,
    /// ID Token 签名 / 声明（iss/aud/exp/nonce）校验失败。
    #[error("外部认证失败：ID Token 校验未通过")]
    InvalidIdToken,
}

/// OIDC discovery 文档中本流程所需的端点（`/.well-known/openid-configuration`）。
#[derive(Debug, Clone, Deserialize)]
pub struct Discovery {
    /// 授权端点（重定向用户至此登录授权）。
    pub authorization_endpoint: String,
    /// token 端点（用授权码换 ID Token）。
    pub token_endpoint: String,
    /// JWKS 端点（取 IdP 公钥校验 ID Token 签名）。
    pub jwks_uri: String,
}

/// 单个 JWK（仅支持 RSA：OIDC ID Token 主流签名算法 RS256）。
#[derive(Debug, Clone, Deserialize)]
pub struct Jwk {
    /// 密钥 ID，与 ID Token header 的 `kid` 匹配。
    pub kid: Option<String>,
    /// RSA 模数（base64url）。
    pub n: Option<String>,
    /// RSA 公开指数（base64url）。
    pub e: Option<String>,
    /// 密钥类型（仅处理 `RSA`）。
    pub kty: String,
}

/// JWKS 文档。
#[derive(Debug, Clone, Deserialize)]
pub struct Jwks {
    /// 公钥集合。
    pub keys: Vec<Jwk>,
}

/// 授权码换 token 的响应（仅取 `id_token`；ID Token 自身已携带身份声明）。
#[derive(Debug, Deserialize)]
struct TokenResponse {
    /// ID Token（JWT）。脱敏：绝不进日志。
    id_token: String,
}

/// ID Token 声明（仅取本流程所需字段）。
#[derive(Debug, Deserialize)]
struct IdTokenClaims {
    /// 签发者，须等于配置 issuer。
    iss: String,
    /// 受众，须包含本客户端 ID。
    aud: Audience,
    /// 外部稳定标识。
    sub: String,
    /// 过期时间（Unix 秒），由 jsonwebtoken 校验。
    #[allow(dead_code)]
    exp: u64,
    /// 防重放随机串，须等于登录时下发的 nonce。
    nonce: Option<String>,
    /// 建议用户名（可选）。
    preferred_username: Option<String>,
    /// 邮箱（可选，作用户名兜底）。
    email: Option<String>,
}

/// `aud` 可能是单串或字符串数组，统一解析。
#[derive(Debug, Deserialize)]
#[serde(untagged)]
enum Audience {
    /// 单个受众。
    One(String),
    /// 多个受众。
    Many(Vec<String>),
}

impl Audience {
    /// 是否包含目标客户端 ID。
    fn contains(&self, client_id: &str) -> bool {
        match self {
            Audience::One(a) => a == client_id,
            Audience::Many(list) => list.iter().any(|a| a == client_id),
        }
    }
}

/// 一次 OIDC 登录流程的服务端短期状态（绑定一次性，回调时按 `state` 取出并消费）。
#[derive(Debug, Clone)]
pub struct FlowState {
    /// PKCE code_verifier（换码时回传 IdP）。
    pub code_verifier: String,
    /// 防重放 nonce（须与 ID Token 的 nonce 一致）。
    pub nonce: String,
}

/// OIDC provider：持有配置与 HTTP 客户端，按需拉取并缓存 discovery / JWKS。
///
/// 不实现口令型 [`super::provider::AuthProvider`]（OIDC 走授权码流），单独暴露
/// 授权 URL 构造与回调换码 / 校验能力。
pub struct OidcProvider {
    settings: OidcSettings,
    /// 出站网络热替换槽（含当前 client，随 PATCH 即时换代理；FR-88，ADR-0022）。
    network: std::sync::Arc<crate::config::NetworkState>,
    /// discovery + JWKS 缓存（首次使用拉取；本批简化为进程内一次性缓存，按需可扩 TTL 刷新）。
    cache: tokio::sync::OnceCell<(Discovery, Jwks)>,
}

impl OidcProvider {
    /// 构造 provider；持出站网络热替换槽，出站时取当前 client（纯 rustls，与既有上游一致）。
    pub fn new(
        settings: OidcSettings,
        network: std::sync::Arc<crate::config::NetworkState>,
    ) -> Self {
        Self {
            settings,
            network,
            cache: tokio::sync::OnceCell::new(),
        }
    }

    /// provider 类别。
    pub fn kind(&self) -> ProviderKind {
        ProviderKind::Oidc
    }

    /// 是否启用 JIT 即时开通。
    pub fn auto_provision(&self) -> bool {
        self.settings.auto_provision
    }

    /// 生成一次登录流程的随机参数（state + PKCE + nonce）与跳转到 IdP 的授权 URL。
    ///
    /// 返回 `(授权 URL, state, FlowState)`：调用方按 `state` 暂存 `FlowState`，回调时取回。
    pub async fn begin_login(&self) -> Result<(String, String, FlowState), OidcError> {
        let (discovery, _) = self.discovery_and_jwks().await?;
        let state = random_b64url(STATE_NONCE_BYTES);
        let nonce = random_b64url(STATE_NONCE_BYTES);
        let code_verifier = random_b64url(PKCE_VERIFIER_BYTES);
        let code_challenge = pkce_challenge(&code_verifier);
        let url = build_authorize_url(
            &discovery.authorization_endpoint,
            &self.settings.client_id,
            &self.settings.redirect_uri,
            &state,
            &nonce,
            &code_challenge,
        );
        Ok((
            url,
            state,
            FlowState {
                code_verifier,
                nonce,
            },
        ))
    }

    /// 回调换码：用授权码 + PKCE verifier 换 ID Token，校验签名与声明，产出外部主体。
    ///
    /// `flow` 为登录时按 `state` 暂存并已取回的流程状态（state 校验由调用方在取回时完成）。
    pub async fn complete_login(
        &self,
        code: &str,
        flow: &FlowState,
    ) -> Result<AuthenticatedSubject, OidcError> {
        let (discovery, jwks) = self.discovery_and_jwks().await?;
        let id_token = self
            .exchange_code(&discovery.token_endpoint, code, &flow.code_verifier)
            .await?;
        let claims = verify_id_token(
            &id_token,
            jwks,
            &self.settings.issuer,
            &self.settings.client_id,
            &flow.nonce,
        )?;
        Ok(subject_from_claims(claims))
    }

    /// 拉取并缓存 discovery 与 JWKS（首次使用时拉取）。
    async fn discovery_and_jwks(&self) -> Result<&(Discovery, Jwks), OidcError> {
        self.cache
            .get_or_try_init(|| async {
                let discovery = self.fetch_discovery().await?;
                let jwks = self.fetch_jwks(&discovery.jwks_uri).await?;
                Ok((discovery, jwks))
            })
            .await
    }

    /// 拉取 OIDC discovery 文档。
    async fn fetch_discovery(&self) -> Result<Discovery, OidcError> {
        let url = discovery_url(&self.settings.issuer);
        self.get_json(&url).await
    }

    /// 拉取 JWKS。
    async fn fetch_jwks(&self, jwks_uri: &str) -> Result<Jwks, OidcError> {
        self.get_json(jwks_uri).await
    }

    /// GET 并按 JSON 解析（用 bytes + serde_json，不依赖 reqwest 的 json 特性）。
    async fn get_json<T: serde::de::DeserializeOwned>(&self, url: &str) -> Result<T, OidcError> {
        // 从热替换槽取当前 client（读锁极短、锁外发请求）
        let resp = self.network.client().get(url).send().await.map_err(|e| {
            tracing::warn!(错误 = %e, "OIDC 请求 IdP 端点失败");
            OidcError::Upstream
        })?;
        if !resp.status().is_success() {
            tracing::warn!(状态码 = %resp.status(), "OIDC IdP 端点返回非成功状态");
            return Err(OidcError::Upstream);
        }
        let bytes = resp.bytes().await.map_err(|_| OidcError::Upstream)?;
        serde_json::from_slice(&bytes).map_err(|e| {
            tracing::warn!(错误 = %e, "OIDC 解析 IdP 响应失败");
            OidcError::Upstream
        })
    }

    /// 用授权码 + PKCE verifier 向 token 端点换取 ID Token。
    async fn exchange_code(
        &self,
        token_endpoint: &str,
        code: &str,
        code_verifier: &str,
    ) -> Result<String, OidcError> {
        // 授权码授权类型（RFC 6749 §4.1.3）+ PKCE verifier（RFC 7636）；client_secret 经
        // 表单提交（绝不进日志）。client_secret_post 客户端认证方式，主流 IdP 通用。
        let mut form: HashMap<&str, &str> = HashMap::new();
        form.insert("grant_type", "authorization_code");
        form.insert("code", code);
        form.insert("redirect_uri", &self.settings.redirect_uri);
        form.insert("client_id", &self.settings.client_id);
        form.insert("client_secret", &self.settings.client_secret);
        form.insert("code_verifier", code_verifier);

        let resp = self
            .network
            .client()
            .post(token_endpoint)
            .form(&form)
            .send()
            .await
            .map_err(|e| {
                tracing::warn!(错误 = %e, "OIDC token 端点换码请求失败");
                OidcError::Upstream
            })?;
        if !resp.status().is_success() {
            // 不回显 IdP 错误体，避免泄露内部细节
            tracing::warn!(状态码 = %resp.status(), "OIDC token 端点返回非成功状态");
            return Err(OidcError::Upstream);
        }
        let bytes = resp.bytes().await.map_err(|_| OidcError::Upstream)?;
        let token: TokenResponse = serde_json::from_slice(&bytes).map_err(|e| {
            tracing::warn!(错误 = %e, "OIDC 解析 token 响应失败");
            OidcError::Upstream
        })?;
        Ok(token.id_token)
    }
}

/// 由 issuer 推出 discovery 文档 URL（拼 `/.well-known/openid-configuration`，去重斜杠）。
fn discovery_url(issuer: &str) -> String {
    let base = issuer.trim_end_matches('/');
    format!("{base}/.well-known/openid-configuration")
}

/// 构造跳转到 IdP 授权端点的 URL（授权码流 + PKCE + nonce）。
fn build_authorize_url(
    authorization_endpoint: &str,
    client_id: &str,
    redirect_uri: &str,
    state: &str,
    nonce: &str,
    code_challenge: &str,
) -> String {
    // 固定请求 openid + profile + email 范围；S256 PKCE 方法。
    let query = [
        ("response_type", "code"),
        ("client_id", client_id),
        ("redirect_uri", redirect_uri),
        ("scope", "openid profile email"),
        ("state", state),
        ("nonce", nonce),
        ("code_challenge", code_challenge),
        ("code_challenge_method", "S256"),
    ]
    .iter()
    .map(|(k, v)| format!("{}={}", k, urlencode(v)))
    .collect::<Vec<_>>()
    .join("&");
    let sep = if authorization_endpoint.contains('?') {
        '&'
    } else {
        '?'
    };
    format!("{authorization_endpoint}{sep}{query}")
}

/// 校验 ID Token：签名（JWKS RSA）+ iss + aud + exp + nonce。任一不符即拒。
fn verify_id_token(
    id_token: &str,
    jwks: &Jwks,
    issuer: &str,
    client_id: &str,
    expected_nonce: &str,
) -> Result<IdTokenClaims, OidcError> {
    // 取 header 的 kid 选对应公钥；同时锁定 RS256（拒绝算法混淆，绝不接受 none / HS*）
    let header = decode_header(id_token).map_err(|_| OidcError::InvalidIdToken)?;
    if header.alg != Algorithm::RS256 {
        tracing::warn!(算法 = ?header.alg, "OIDC ID Token 签名算法非 RS256，拒绝");
        return Err(OidcError::InvalidIdToken);
    }
    let jwk = select_rsa_jwk(jwks, header.kid.as_deref()).ok_or(OidcError::InvalidIdToken)?;
    let (n, e) = match (&jwk.n, &jwk.e) {
        (Some(n), Some(e)) => (n, e),
        _ => return Err(OidcError::InvalidIdToken),
    };
    let key = DecodingKey::from_rsa_components(n, e).map_err(|_| OidcError::InvalidIdToken)?;

    let mut validation = Validation::new(Algorithm::RS256);
    validation.leeway = 0;
    // iss / aud 交由 jsonwebtoken 校验（更严谨且常量时间）；同时我们再显式核对一遍 aud。
    validation.set_issuer(&[issuer]);
    validation.set_audience(&[client_id]);
    let data = decode::<IdTokenClaims>(id_token, &key, &validation).map_err(|e| {
        tracing::warn!(错误 = %e, "OIDC ID Token 签名或声明校验失败");
        OidcError::InvalidIdToken
    })?;
    let claims = data.claims;

    // 显式再核对 iss / aud（防御性，不依赖单一校验路径）
    if claims.iss != issuer || !claims.aud.contains(client_id) {
        return Err(OidcError::InvalidIdToken);
    }
    // nonce 防重放：必须存在且与登录时下发的一致
    match &claims.nonce {
        Some(n) if n == expected_nonce => {}
        _ => {
            tracing::warn!("OIDC ID Token nonce 缺失或不匹配，拒绝（防重放）");
            return Err(OidcError::InvalidIdToken);
        }
    }
    Ok(claims)
}

/// 选取与 `kid` 匹配的 RSA 公钥；无 `kid` 时回退唯一的 RSA 键。
fn select_rsa_jwk<'a>(jwks: &'a Jwks, kid: Option<&str>) -> Option<&'a Jwk> {
    let rsa_keys = jwks.keys.iter().filter(|k| k.kty == "RSA");
    match kid {
        Some(kid) => rsa_keys.clone().find(|k| k.kid.as_deref() == Some(kid)),
        None => {
            let mut iter = rsa_keys.clone();
            let first = iter.next();
            // 多键且 token 未给 kid：无法确定用哪把，拒绝（避免错配）
            if iter.next().is_some() {
                None
            } else {
                first
            }
        }
    }
}

/// 由 ID Token 声明组装外部主体；用户名优先 preferred_username，其次 email，最后 sub。
fn subject_from_claims(claims: IdTokenClaims) -> AuthenticatedSubject {
    let preferred_username = claims
        .preferred_username
        .filter(|s| !s.is_empty())
        .or(claims.email.filter(|s| !s.is_empty()))
        .unwrap_or_else(|| claims.sub.clone());
    AuthenticatedSubject {
        provider: ProviderKind::Oidc,
        subject: claims.sub,
        preferred_username,
    }
}

/// 计算 PKCE `code_challenge` = base64url(sha256(code_verifier))（S256 方法）。
fn pkce_challenge(code_verifier: &str) -> String {
    let digest = Sha256::digest(code_verifier.as_bytes());
    b64url_encode(&digest)
}

/// 生成 `len` 字节高熵随机数并 base64url（无填充）编码为串。
fn random_b64url(len: usize) -> String {
    use rand::RngCore;
    let mut buf = vec![0u8; len];
    rand::thread_rng().fill_bytes(&mut buf);
    b64url_encode(&buf)
}

/// base64url 无填充编码。
fn b64url_encode(bytes: &[u8]) -> String {
    use base64::engine::general_purpose::URL_SAFE_NO_PAD;
    use base64::Engine;
    URL_SAFE_NO_PAD.encode(bytes)
}

/// 最小 URL 百分号编码：只对非 unreserved 字符编码（query 参数值用，足够覆盖本流程取值）。
fn urlencode(s: &str) -> String {
    let mut out = String::with_capacity(s.len());
    for b in s.bytes() {
        match b {
            // RFC 3986 unreserved
            b'A'..=b'Z' | b'a'..=b'z' | b'0'..=b'9' | b'-' | b'_' | b'.' | b'~' => {
                out.push(b as char)
            }
            _ => out.push_str(&format!("%{b:02X}")),
        }
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;
    use jsonwebtoken::{encode, EncodingKey, Header};

    /// 固定测试用 RSA 私钥（PKCS#1 DER），与下方 JWKS 的 n/e 配对。
    /// 由 `cryptography` 一次性生成的纯测试向量，非生产密钥；二进制资产不含注释。
    const TEST_RSA_PKCS1_DER: &[u8] = include_bytes!("testdata/oidc_test_rsa_pkcs1.der");
    /// 上述私钥对应公钥的 base64url 模数 `n`。
    const TEST_JWK_N: &str = "tJOCUVcE473ahzlFWSRD7_vj6ZMHRKKCXyUWlVQqJx5O2yYu1ffXVBnU4nYzTTCzVqN0-3h97SFDk56lDXL5qSQK9yDQdC1ppflEdCs7T-73rpQHoAUvnGgQFEmTFGhJDbV7LXMg-3NoZoWodQ5WJwUCTevjG3xhgfSO69Z_0vEEVtWuBRpt4HaeBTOEGhhTbheVEOkIZ7ZYPEpkAL8vJrpz-waiOMWi-3gj5RK1tzy6vSJ_9GF8JqxQpr_Fx1nd95Lu8WmO6ZRz5-SGJkW3t1m9H9dVg9oPU3MMDTsxGZ7c45Yc3EO9oqhmHnD1QK1DxHx9XQJKwAZphprzCfuBew";
    /// 上述公钥的 base64url 指数 `e`（65537）。
    const TEST_JWK_E: &str = "AQAB";

    /// 据固定测试密钥造编码器与单键 JWKS（kid = "test-kid"）。
    fn 测试密钥与_jwks() -> (EncodingKey, Jwks) {
        let encoding = EncodingKey::from_rsa_der(TEST_RSA_PKCS1_DER);
        let jwks = Jwks {
            keys: vec![Jwk {
                kid: Some("test-kid".to_string()),
                n: Some(TEST_JWK_N.to_string()),
                e: Some(TEST_JWK_E.to_string()),
                kty: "RSA".to_string(),
            }],
        };
        (encoding, jwks)
    }

    /// 用给定声明签发一枚 RS256 ID Token（kid = "test-kid"）。
    fn 签发_id_token(
        encoding: &EncodingKey,
        iss: &str,
        aud: &str,
        sub: &str,
        nonce: Option<&str>,
        exp_offset: i64,
    ) -> String {
        let now = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_secs() as i64;
        let claims = serde_json::json!({
            "iss": iss,
            "aud": aud,
            "sub": sub,
            "exp": now + exp_offset,
            "iat": now,
            "nonce": nonce,
            "preferred_username": "alice",
        });
        let mut header = Header::new(Algorithm::RS256);
        header.kid = Some("test-kid".to_string());
        encode(&header, &claims, encoding).unwrap()
    }

    #[test]
    fn pkce_challenge_为_verifier_的_s256_base64url() {
        // RFC 7636 附录 B 示例向量
        let verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk";
        let challenge = pkce_challenge(verifier);
        assert_eq!(challenge, "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM");
    }

    #[test]
    fn 授权_url_含必要参数与_pkce() {
        let url = build_authorize_url(
            "https://idp.example/auth",
            "client-1",
            "https://app/callback",
            "st4te",
            "n0nce",
            "chall",
        );
        assert!(url.starts_with("https://idp.example/auth?"));
        assert!(url.contains("response_type=code"));
        assert!(url.contains("client_id=client-1"));
        assert!(url.contains("code_challenge=chall"));
        assert!(url.contains("code_challenge_method=S256"));
        assert!(url.contains("state=st4te"));
        assert!(url.contains("nonce=n0nce"));
        // redirect_uri 被百分号编码
        assert!(url.contains("redirect_uri=https%3A%2F%2Fapp%2Fcallback"));
    }

    #[test]
    fn discovery_url_拼接去重斜杠() {
        assert_eq!(
            discovery_url("https://idp.example"),
            "https://idp.example/.well-known/openid-configuration"
        );
        assert_eq!(
            discovery_url("https://idp.example/"),
            "https://idp.example/.well-known/openid-configuration"
        );
    }

    #[test]
    fn id_token_合法时校验通过并解析出_sub() {
        let (enc, jwks) = 测试密钥与_jwks();
        let token = 签发_id_token(
            &enc,
            "https://idp",
            "client-1",
            "user-123",
            Some("the-nonce"),
            3600,
        );
        let claims =
            verify_id_token(&token, &jwks, "https://idp", "client-1", "the-nonce").unwrap();
        assert_eq!(claims.sub, "user-123");
        let subject = subject_from_claims(claims);
        assert_eq!(subject.subject, "user-123");
        assert_eq!(subject.preferred_username, "alice");
        assert_eq!(subject.provider, ProviderKind::Oidc);
    }

    #[test]
    fn id_token_nonce_不匹配被拒_防重放() {
        let (enc, jwks) = 测试密钥与_jwks();
        let token = 签发_id_token(
            &enc,
            "https://idp",
            "client-1",
            "u",
            Some("real-nonce"),
            3600,
        );
        // 期望的 nonce 与 token 内不一致
        let err =
            verify_id_token(&token, &jwks, "https://idp", "client-1", "other-nonce").unwrap_err();
        assert!(matches!(err, OidcError::InvalidIdToken));
    }

    #[test]
    fn id_token_缺_nonce_被拒() {
        let (enc, jwks) = 测试密钥与_jwks();
        let token = 签发_id_token(&enc, "https://idp", "client-1", "u", None, 3600);
        let err = verify_id_token(&token, &jwks, "https://idp", "client-1", "any").unwrap_err();
        assert!(matches!(err, OidcError::InvalidIdToken));
    }

    #[test]
    fn id_token_错误_issuer_被拒() {
        let (enc, jwks) = 测试密钥与_jwks();
        let token = 签发_id_token(&enc, "https://evil", "client-1", "u", Some("n"), 3600);
        let err = verify_id_token(&token, &jwks, "https://idp", "client-1", "n").unwrap_err();
        assert!(matches!(err, OidcError::InvalidIdToken));
    }

    #[test]
    fn id_token_错误_audience_被拒() {
        let (enc, jwks) = 测试密钥与_jwks();
        let token = 签发_id_token(&enc, "https://idp", "other-client", "u", Some("n"), 3600);
        let err = verify_id_token(&token, &jwks, "https://idp", "client-1", "n").unwrap_err();
        assert!(matches!(err, OidcError::InvalidIdToken));
    }

    #[test]
    fn id_token_过期被拒() {
        let (enc, jwks) = 测试密钥与_jwks();
        // exp 在过去
        let token = 签发_id_token(&enc, "https://idp", "client-1", "u", Some("n"), -3600);
        let err = verify_id_token(&token, &jwks, "https://idp", "client-1", "n").unwrap_err();
        assert!(matches!(err, OidcError::InvalidIdToken));
    }

    #[test]
    fn id_token_被他密钥签名时签名校验失败() {
        let (_enc, jwks) = 测试密钥与_jwks();
        // 用另一份固定密钥签发（kid 仍标 test-kid），但用第一份 JWKS 校验：签名应不符
        const OTHER_DER: &[u8] = include_bytes!("testdata/oidc_test_rsa_pkcs1_other.der");
        let enc2 = EncodingKey::from_rsa_der(OTHER_DER);
        let token = 签发_id_token(&enc2, "https://idp", "client-1", "u", Some("n"), 3600);
        let err = verify_id_token(&token, &jwks, "https://idp", "client-1", "n").unwrap_err();
        assert!(matches!(err, OidcError::InvalidIdToken));
    }

    #[test]
    fn 用户名兜底_preferred_then_email_then_sub() {
        // preferred_username 缺失走 email
        let c = IdTokenClaims {
            iss: "i".into(),
            aud: Audience::One("a".into()),
            sub: "s".into(),
            exp: 0,
            nonce: None,
            preferred_username: None,
            email: Some("u@e.com".into()),
        };
        assert_eq!(subject_from_claims(c).preferred_username, "u@e.com");
        // 都缺走 sub
        let c = IdTokenClaims {
            iss: "i".into(),
            aud: Audience::One("a".into()),
            sub: "the-sub".into(),
            exp: 0,
            nonce: None,
            preferred_username: None,
            email: None,
        };
        assert_eq!(subject_from_claims(c).preferred_username, "the-sub");
    }

    #[test]
    fn 多_rsa_键且无_kid_时拒绝错配() {
        let jwks = Jwks {
            keys: vec![
                Jwk {
                    kid: Some("a".into()),
                    n: Some("x".into()),
                    e: Some("AQAB".into()),
                    kty: "RSA".into(),
                },
                Jwk {
                    kid: Some("b".into()),
                    n: Some("y".into()),
                    e: Some("AQAB".into()),
                    kty: "RSA".into(),
                },
            ],
        };
        assert!(select_rsa_jwk(&jwks, None).is_none());
        assert!(select_rsa_jwk(&jwks, Some("a")).is_some());
        assert!(select_rsa_jwk(&jwks, Some("z")).is_none());
    }
}
