//! Web 会话 JWT（ADR-0011，HS256）。
//!
//! 签名密钥的真源是 data_dir 下的 `.jwt_secret` 文件：无则生成高熵随机密钥写入，
//! 有则读取复用。密钥绝不入库、绝不进日志，文件位于已被 `.gitignore` 排除的数据目录。
//! SPA 把 JWT 放在 `Authorization: Bearer` 头里（不走 Cookie），天然规避 CSRF。

use std::path::{Path, PathBuf};

use jsonwebtoken::{decode, encode, Algorithm, DecodingKey, EncodingKey, Header, Validation};
use serde::{Deserialize, Serialize};

use crate::meta::Role;

/// 密钥文件名（位于数据目录下）。
const SECRET_FILE_NAME: &str = ".jwt_secret";
/// 生成的 HS256 密钥字节数（256 位高熵）。
const SECRET_LEN: usize = 32;
/// docker 范围令牌默认有效期（秒）：取较短值，降低令牌泄露风险。
pub const DOCKER_TOKEN_TTL_SECS: u64 = 300;

/// JWT 相关错误。
#[derive(Debug, thiserror::Error)]
pub enum JwtError {
    /// 密钥文件读写失败。
    #[error("JWT 密钥读写失败: {0}")]
    Io(#[from] std::io::Error),
    /// 签发 / 校验失败（含过期、签名不符、格式非法）。
    #[error("JWT 签发或校验失败: {0}")]
    Jwt(#[from] jsonwebtoken::errors::Error),
    /// 密钥文件内容异常（如长度不足）。
    #[error("JWT 密钥内容异常: {0}")]
    Secret(String),
}

/// JWT 载荷（claims）。仅放非敏感的最小身份信息。
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct JwtClaims {
    /// subject：用户主键。
    pub sub: String,
    /// 用户名。
    pub username: String,
    /// 全局角色字符串（admin | user）。
    pub role: String,
    /// 签发时间（Unix 秒）。
    pub iat: u64,
    /// 过期时间（Unix 秒）。
    pub exp: u64,
}

/// docker 范围令牌中的单条访问授权（对应 registry v2 token 的 `access` 项）。
///
/// 描述"对哪个资源（仓库镜像名）授予哪些动作"。`r#type` 在 registry v2 里固定为
/// `repository`；`name` 为 docker 的 `{仓库}/{镜像}` 名；`actions` 取 `pull` / `push`。
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct DockerAccess {
    /// 资源类型，registry v2 中固定为 `repository`。
    pub r#type: String,
    /// 资源名（docker 的 `{仓库}/{镜像}`）。
    pub name: String,
    /// 已授予的动作集合（`pull` / `push`）。
    pub actions: Vec<String>,
}

/// docker 范围令牌的载荷（claims）。
///
/// 仅放非敏感信息：主体（用户名或 `anonymous`）、签发 / 过期时间与已授予的访问集合。
/// 不含口令 / 凭据，令牌仅在签发时返回、不入库、不进日志。
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct DockerTokenClaims {
    /// subject：用户名，匿名为 `anonymous`。
    pub sub: String,
    /// 签发时间（Unix 秒）。
    pub iat: u64,
    /// 过期时间（Unix 秒）。
    pub exp: u64,
    /// 该令牌已授予的访问集合（按 scope 逐项授权）。
    pub access: Vec<DockerAccess>,
}

/// JWT 签名器：持有编解码密钥与会话有效期。
#[derive(Clone)]
pub struct JwtSigner {
    /// HS256 编码密钥。
    encoding: EncodingKey,
    /// HS256 解码密钥。
    decoding: DecodingKey,
    /// 会话有效期（秒）。
    ttl_secs: u64,
}

impl std::fmt::Debug for JwtSigner {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        // 绝不在调试输出中泄露密钥本体
        f.debug_struct("JwtSigner")
            .field("ttl_secs", &self.ttl_secs)
            .finish_non_exhaustive()
    }
}

impl JwtSigner {
    /// 从数据目录加载或初始化签名密钥并构造签名器。
    ///
    /// `data_dir/.jwt_secret` 不存在则生成 256 位随机密钥写入（尽量收紧权限）；
    /// 已存在则读取复用，保证重启后既有会话仍可校验。
    pub fn from_data_dir(data_dir: &Path, ttl_secs: u64) -> Result<Self, JwtError> {
        let secret = load_or_create_secret(&secret_path(data_dir))?;
        Ok(Self::from_secret(&secret, ttl_secs))
    }

    /// 基于给定密钥字节构造签名器（供测试与内部复用）。
    pub fn from_secret(secret: &[u8], ttl_secs: u64) -> Self {
        Self {
            encoding: EncodingKey::from_secret(secret),
            decoding: DecodingKey::from_secret(secret),
            ttl_secs,
        }
    }

    /// 会话有效期（秒）。
    pub fn ttl_secs(&self) -> u64 {
        self.ttl_secs
    }

    /// 为用户签发一枚有限有效期的 JWT。
    pub fn issue(&self, user_id: &str, username: &str, role: Role) -> Result<String, JwtError> {
        let now = now_unix();
        let claims = JwtClaims {
            sub: user_id.to_string(),
            username: username.to_string(),
            role: role.as_str().to_string(),
            iat: now,
            exp: now + self.ttl_secs,
        };
        let token = encode(&Header::new(Algorithm::HS256), &claims, &self.encoding)?;
        Ok(token)
    }

    /// 校验 JWT 并返回其 claims；签名不符 / 过期 / 格式非法均返回 Err。
    pub fn verify(&self, token: &str) -> Result<JwtClaims, JwtError> {
        // 显式要求 HS256，拒绝算法混淆；leeway 置 0 使过期判定精确（不放宽容差）
        let mut validation = Validation::new(Algorithm::HS256);
        validation.leeway = 0;
        let data = decode::<JwtClaims>(token, &self.decoding, &validation)?;
        Ok(data.claims)
    }

    /// 签发一枚 docker 范围令牌（复用同一 HS256 密钥）。
    ///
    /// `subject` 为用户名或 `anonymous`，`access` 为各 scope 授权后的访问集合，
    /// `ttl_secs` 为有效期（建议取较短值，见 [`DOCKER_TOKEN_TTL_SECS`]）。
    pub fn issue_docker_token(
        &self,
        subject: &str,
        access: Vec<DockerAccess>,
        ttl_secs: u64,
    ) -> Result<String, JwtError> {
        let now = now_unix();
        let claims = DockerTokenClaims {
            sub: subject.to_string(),
            iat: now,
            exp: now + ttl_secs,
            access,
        };
        let token = encode(&Header::new(Algorithm::HS256), &claims, &self.encoding)?;
        Ok(token)
    }

    /// 校验 docker 范围令牌并返回其 claims；签名不符 / 过期 / 格式非法均返回 Err。
    pub fn verify_docker_token(&self, token: &str) -> Result<DockerTokenClaims, JwtError> {
        // 与会话 JWT 同样：显式 HS256、leeway 置 0 精确判过期（默认要求并校验 exp）
        let mut validation = Validation::new(Algorithm::HS256);
        validation.leeway = 0;
        let data = decode::<DockerTokenClaims>(token, &self.decoding, &validation)?;
        Ok(data.claims)
    }
}

/// 计算密钥文件路径。
fn secret_path(data_dir: &Path) -> PathBuf {
    data_dir.join(SECRET_FILE_NAME)
}

/// 读取已有密钥，或生成新密钥并写入文件（尽量收紧权限）。
fn load_or_create_secret(path: &Path) -> Result<Vec<u8>, JwtError> {
    if path.exists() {
        let secret = std::fs::read(path)?;
        if secret.len() < SECRET_LEN {
            return Err(JwtError::Secret(format!(
                "密钥文件长度不足（{} < {}）",
                secret.len(),
                SECRET_LEN
            )));
        }
        return Ok(secret);
    }
    let secret = generate_secret();
    write_secret(path, &secret)?;
    Ok(secret)
}

/// 生成高熵随机密钥字节。
fn generate_secret() -> Vec<u8> {
    use rand::RngCore;
    let mut secret = vec![0u8; SECRET_LEN];
    rand::thread_rng().fill_bytes(&mut secret);
    secret
}

/// 写入密钥文件，并在类 Unix 平台收紧为属主只读写（0600）。
fn write_secret(path: &Path, secret: &[u8]) -> Result<(), JwtError> {
    std::fs::write(path, secret)?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        // 仅属主可读写，避免同机其他用户读取签名密钥
        std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o600))?;
    }
    Ok(())
}

/// 当前 Unix 时间（秒）。系统时间早于纪元时回退为 0（不影响 TTL 相对计算）。
fn now_unix() -> u64 {
    use std::time::{SystemTime, UNIX_EPOCH};
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn 签发后可校验出原始_claims() {
        let signer = JwtSigner::from_secret(b"test-secret-32-bytes-xxxxxxxxxxxx", 3600);
        let token = signer.issue("uid-1", "alice", Role::Admin).unwrap();
        let claims = signer.verify(&token).unwrap();
        assert_eq!(claims.sub, "uid-1");
        assert_eq!(claims.username, "alice");
        assert_eq!(claims.role, "admin");
        assert!(claims.exp > claims.iat);
    }

    #[test]
    fn 不同密钥签发的_token_互不通过() {
        let a = JwtSigner::from_secret(b"secret-a-xxxxxxxxxxxxxxxxxxxxxxxx", 3600);
        let b = JwtSigner::from_secret(b"secret-b-yyyyyyyyyyyyyyyyyyyyyyyy", 3600);
        let token = a.issue("uid", "u", Role::User).unwrap();
        // b 的密钥不同，校验签名应失败
        assert!(b.verify(&token).is_err());
    }

    #[test]
    fn 过期的_token_校验失败() {
        // TTL = 0 使签发即过期；jsonwebtoken 默认含少量 leeway，故等待越过窗口
        let signer = JwtSigner::from_secret(b"secret-expire-xxxxxxxxxxxxxxxxxxx", 0);
        let token = signer.issue("uid", "u", Role::User).unwrap();
        std::thread::sleep(std::time::Duration::from_secs(2));
        let err = signer.verify(&token).unwrap_err();
        assert!(matches!(err, JwtError::Jwt(_)));
    }

    #[test]
    fn 篡改的_token_校验失败() {
        let signer = JwtSigner::from_secret(b"secret-tamper-xxxxxxxxxxxxxxxxxxx", 3600);
        let mut token = signer.issue("uid", "u", Role::User).unwrap();
        token.push('x');
        assert!(signer.verify(&token).is_err());
    }

    #[test]
    fn 密钥文件不存在则生成_存在则复用() {
        let dir = tempfile::tempdir().unwrap();
        let p = secret_path(dir.path());
        assert!(!p.exists());
        let s1 = load_or_create_secret(&p).unwrap();
        assert!(p.exists());
        assert_eq!(s1.len(), SECRET_LEN);
        // 再次加载应得到完全相同的密钥（复用而非重新生成）
        let s2 = load_or_create_secret(&p).unwrap();
        assert_eq!(s1, s2);
    }

    #[test]
    fn docker_令牌签发后可校验出原始_access() {
        let signer = JwtSigner::from_secret(b"docker-secret-32-bytes-xxxxxxxxxx", 3600);
        let access = vec![DockerAccess {
            r#type: "repository".to_string(),
            name: "hub/app".to_string(),
            actions: vec!["pull".to_string(), "push".to_string()],
        }];
        let token = signer
            .issue_docker_token("alice", access.clone(), DOCKER_TOKEN_TTL_SECS)
            .unwrap();
        let claims = signer.verify_docker_token(&token).unwrap();
        assert_eq!(claims.sub, "alice");
        assert_eq!(claims.access, access);
        assert!(claims.exp > claims.iat);
    }

    #[test]
    fn 过期的_docker_令牌校验失败() {
        let signer = JwtSigner::from_secret(b"docker-expire-xxxxxxxxxxxxxxxxxxx", 3600);
        // TTL = 0 使签发即过期；默认 leeway 已被置 0，等待越过窗口
        let token = signer
            .issue_docker_token("u", Vec::new(), 0)
            .unwrap();
        std::thread::sleep(std::time::Duration::from_secs(2));
        assert!(signer.verify_docker_token(&token).is_err());
    }

    #[test]
    fn 伪造或他密钥签发的_docker_令牌校验失败() {
        let a = JwtSigner::from_secret(b"docker-key-a-xxxxxxxxxxxxxxxxxxxx", 3600);
        let b = JwtSigner::from_secret(b"docker-key-b-yyyyyyyyyyyyyyyyyyyy", 3600);
        let token = a.issue_docker_token("u", Vec::new(), 300).unwrap();
        // 他密钥无法校验通过（签名不符）
        assert!(b.verify_docker_token(&token).is_err());
        // 篡改令牌亦失败
        let mut tampered = token;
        tampered.push('x');
        assert!(a.verify_docker_token(&tampered).is_err());
    }

    #[test]
    fn 会话_jwt_与_docker_令牌互不串味() {
        let signer = JwtSigner::from_secret(b"cross-secret-xxxxxxxxxxxxxxxxxxxx", 3600);
        // 会话 JWT 不应被当作 docker 令牌解析成功（claims 结构不含 access 必备字段）
        let session = signer.issue("uid", "u", Role::User).unwrap();
        assert!(signer.verify_docker_token(&session).is_err());
        // docker 令牌也不应被当作会话 JWT（缺 username / role）
        let docker = signer.issue_docker_token("u", Vec::new(), 300).unwrap();
        assert!(signer.verify(&docker).is_err());
    }

    #[test]
    fn 复用同一密钥文件的签名器跨实例互通() {
        let dir = tempfile::tempdir().unwrap();
        let signer1 = JwtSigner::from_data_dir(dir.path(), 3600).unwrap();
        let token = signer1.issue("uid", "u", Role::User).unwrap();
        // 模拟重启：用同一数据目录再造签名器，应能校验既有 token
        let signer2 = JwtSigner::from_data_dir(dir.path(), 3600).unwrap();
        assert!(signer2.verify(&token).is_ok());
    }
}
