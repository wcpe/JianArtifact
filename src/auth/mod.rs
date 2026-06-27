//! 认证模块：口令哈希、首启管理员引导、JWT 会话、API Token、Basic Auth 与登录防护
//! （ADR-0003、ADR-0010、ADR-0011）。
//!
//! 鉴权（对仓库读写的判定）属后续批次的 `authz` 模块，本模块只解析“是谁”。

use argon2::password_hash::{
    rand_core::OsRng, PasswordHash, PasswordHasher, PasswordVerifier, SaltString,
};
use argon2::Argon2;

use crate::meta::{MetaError, MetaStore, Role, UserRecord};

pub mod basic;
pub mod jwt;
pub mod ldap;
pub mod lockout;
pub mod oidc;
pub mod provider;
pub mod token;

pub use basic::{parse_basic_auth, BasicCredentials};
pub(crate) use jwt::hmac_sha256;
pub use jwt::{
    DockerAccess, DockerTokenClaims, JwtClaims, JwtError, JwtSigner, DOCKER_TOKEN_TTL_SECS,
};
pub use ldap::{LdapProvider, LdapSettings};
pub use lockout::{LockoutError, LoginGuard};
pub use oidc::{OidcError, OidcProvider, OidcSettings};
pub use provider::{AuthProvider, AuthenticatedSubject, ProviderKind};
pub use token::{generate_api_token, hash_api_token, verify_api_token, TOKEN_PREFIX};

/// 已解析的调用方身份，由认证中间件注入请求扩展，供后续鉴权使用。
///
/// 本模块只回答“是谁”（认证）；“能否读写某仓库”（鉴权）在 `authz` 批次判定。
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum AuthIdentity {
    /// 匿名访客：未携带任何有效凭据。
    Anonymous,
    /// 已认证用户。
    Authenticated(AuthUser),
}

/// 已认证用户的最小身份画像（不含口令 / 凭据等敏感项）。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct AuthUser {
    /// 用户主键。
    pub user_id: String,
    /// 用户名。
    pub username: String,
    /// 全局角色。
    pub role: Role,
}

impl AuthIdentity {
    /// 是否为已认证用户。
    pub fn is_authenticated(&self) -> bool {
        matches!(self, AuthIdentity::Authenticated(_))
    }

    /// 取已认证用户引用；匿名时返回 None。
    pub fn user(&self) -> Option<&AuthUser> {
        match self {
            AuthIdentity::Authenticated(u) => Some(u),
            AuthIdentity::Anonymous => None,
        }
    }

    /// 是否为管理员。
    pub fn is_admin(&self) -> bool {
        matches!(
            self,
            AuthIdentity::Authenticated(AuthUser {
                role: Role::Admin,
                ..
            })
        )
    }
}

/// 首启引导默认管理员用户名（未经 env 指定时使用）。
const DEFAULT_ADMIN_USERNAME: &str = "admin";
/// 随机口令长度（字符数）。
const RANDOM_PASSWORD_LEN: usize = 24;
/// 引导用环境变量：管理员用户名。
const ENV_ADMIN_USERNAME: &str = "JIANARTIFACT_ADMIN_USERNAME";
/// 引导用环境变量：管理员口令。
const ENV_ADMIN_PASSWORD: &str = "JIANARTIFACT_ADMIN_PASSWORD";

/// 认证相关错误。
#[derive(Debug, thiserror::Error)]
pub enum AuthError {
    /// 口令哈希计算失败。
    #[error("口令哈希失败: {0}")]
    Hash(String),
    /// 元数据访问失败。
    #[error(transparent)]
    Meta(#[from] MetaError),
    /// 外部 provider（LDAP 等）认证失败：凭据无效 / 目录不可达 / 协议错误等。
    ///
    /// 统一收敛，不区分「用户不存在 / 口令错误 / 目录故障」，避免泄露目录存在性与拓扑。
    #[error("外部认证失败")]
    ExternalAuth,
}

/// 用 argon2 计算口令哈希（含随机盐），返回 PHC 字符串。
///
/// 绝不存明文：调用方应只把返回的哈希入库。
pub fn hash_password(password: &str) -> Result<String, AuthError> {
    let salt = SaltString::generate(&mut OsRng);
    let argon2 = Argon2::default();
    argon2
        .hash_password(password.as_bytes(), &salt)
        .map(|h| h.to_string())
        .map_err(|e| AuthError::Hash(e.to_string()))
}

/// 校验明文口令是否匹配给定的 argon2 PHC 哈希。
///
/// 哈希格式非法或不匹配均返回 false（不区分，避免泄露细节）。
pub fn verify_password(password: &str, password_hash: &str) -> bool {
    match PasswordHash::new(password_hash) {
        Ok(parsed) => Argon2::default()
            .verify_password(password.as_bytes(), &parsed)
            .is_ok(),
        Err(_) => false,
    }
}

/// 首启引导的结果，供启动流程决定是否打印随机口令提示。
#[derive(Debug)]
pub enum BootstrapOutcome {
    /// 库中已有用户，未做任何引导。
    AlreadyInitialized,
    /// 经环境变量创建了首个管理员。
    CreatedFromEnv {
        /// 创建的管理员用户名。
        username: String,
    },
    /// 用随机口令创建了首个管理员，需要把口令提示给运维（仅首启一次）。
    CreatedWithRandomPassword {
        /// 默认管理员用户名。
        username: String,
        /// 一次性随机口令明文（仅用于首启在标准输出提示运维，不进运行日志、不入库、不持久化）。
        password: String,
    },
}

/// 首个管理员引导：仅当库中无任何用户时触发（ADR-0010）。
///
/// 顺序：① env 提供用户名+口令则据此建 Admin；② 否则生成随机强口令建默认管理员。
/// 口令一律 argon2 哈希入库，绝不存明文。系统始终不开放公开自助注册。
pub async fn bootstrap_admin(meta: &MetaStore) -> Result<BootstrapOutcome, AuthError> {
    if meta.count_users().await? > 0 {
        return Ok(BootstrapOutcome::AlreadyInitialized);
    }

    // 路径一：环境变量同时提供用户名与口令
    let env_username = std::env::var(ENV_ADMIN_USERNAME)
        .ok()
        .filter(|s| !s.is_empty());
    let env_password = std::env::var(ENV_ADMIN_PASSWORD)
        .ok()
        .filter(|s| !s.is_empty());

    if let (Some(username), Some(password)) = (env_username, env_password) {
        let hash = hash_password(&password)?;
        meta.create_user(&username, &hash, Role::Admin).await?;
        return Ok(BootstrapOutcome::CreatedFromEnv { username });
    }

    // 路径二：随机口令兜底，不设固定默认口令
    let password = generate_random_password(RANDOM_PASSWORD_LEN);
    let hash = hash_password(&password)?;
    meta.create_user(DEFAULT_ADMIN_USERNAME, &hash, Role::Admin)
        .await?;
    Ok(BootstrapOutcome::CreatedWithRandomPassword {
        username: DEFAULT_ADMIN_USERNAME.to_string(),
        password,
    })
}

/// 外部认证（OIDC）映射到本地用户时的拒绝原因（守 ADR-0010 不自助注册红线）。
#[derive(Debug, thiserror::Error)]
pub enum ExternalLoginError {
    /// 外部认证成功但无对应本地用户、且 JIT 关闭：拒绝登录（最严默认）。
    #[error("外部身份未绑定本地用户，且即时开通未开启")]
    NoLocalUser,
    /// 命中的本地用户已被禁用：拒绝登录。
    #[error("绑定的本地用户已被禁用")]
    Disabled,
    /// 元数据访问失败。
    #[error(transparent)]
    Meta(#[from] MetaError),
}

/// 把已认证的外部主体映射到本地用户（FR-34 / ADR-0016，守 ADR-0010）。
///
/// 流程：① 按外部身份键（provider + sub）查已绑定本地用户，命中即复用（未禁用）；
/// ② 未命中且 `auto_provision = false` → 拒绝（最严默认，等价「仅管理员预置账号可登录」）；
/// ③ 未命中且 `auto_provision = true` → 即时开通，**默认角色固定为最低权限 `User`**，
///    绝不自动授予 `Admin`；用户名取建议名，冲突时追加短后缀避重。
///
/// 该函数只做「外部身份 → 本地用户」映射，不签发会话（签发由调用方照常走既有 JWT）。
pub async fn resolve_external_login(
    meta: &MetaStore,
    subject: &AuthenticatedSubject,
    auto_provision: bool,
) -> Result<UserRecord, ExternalLoginError> {
    let idp = subject.provider.as_str();
    // ① 已绑定本地用户：复用（拒绝已禁用账户）
    if let Some(user) = meta
        .get_user_by_external_identity(idp, &subject.subject)
        .await?
    {
        if user.disabled != 0 {
            return Err(ExternalLoginError::Disabled);
        }
        return Ok(user);
    }

    // ② JIT 关闭：拒绝（守不自助注册红线）
    if !auto_provision {
        return Err(ExternalLoginError::NoLocalUser);
    }

    // ③ JIT 开通：默认角色 User，绝不自动 Admin
    let username = provision_username(meta, &subject.preferred_username).await?;
    let id = meta
        .create_external_user(&username, Role::User, idp, &subject.subject)
        .await?;
    // 回读刚建的用户记录返回（含本地主键 / 角色）
    let user = meta
        .get_user_by_id(&id)
        .await?
        .ok_or(ExternalLoginError::NoLocalUser)?;
    Ok(user)
}

/// 经 LDAP provider 做口令型登录并映射到本地用户（FR-35 / ADR-0016）。
///
/// 流程：① 调用 provider 的 bind 校验（[`LdapProvider::authenticate_password`]）证明外部身份；
/// ② 经既有 [`resolve_external_login`] 把外部主体映射到本地用户（守 ADR-0010：JIT 默认关、
/// 默认角色 User）。校验失败 / 映射拒绝统一返回 [`ExternalLoginError`]，调用方据此回 401。
///
/// 该函数只产出本地用户记录，不签发会话（签发由调用方照常走既有 JWT）。
pub async fn ldap_login(
    meta: &MetaStore,
    provider: &LdapProvider,
    username: &str,
    password: &str,
    auto_provision: bool,
) -> Result<UserRecord, ExternalLoginError> {
    let subject = provider
        .authenticate_password(username, password)
        .await
        .map_err(|_| ExternalLoginError::NoLocalUser)?;
    resolve_external_login(meta, &subject, auto_provision).await
}

/// 为 JIT 开通挑一个可用用户名：建议名可用则用之，已被占用则追加短随机后缀避重。
///
/// 不无限重试：单次冲突即追加 6 位随机后缀，几乎必然唯一；仍冲突由唯一约束兜底报错。
async fn provision_username(
    meta: &MetaStore,
    preferred: &str,
) -> Result<String, ExternalLoginError> {
    let base = if preferred.trim().is_empty() {
        "oidc-user"
    } else {
        preferred.trim()
    };
    if meta.get_user_by_username(base).await?.is_none() {
        return Ok(base.to_string());
    }
    let suffix = generate_random_password(6).to_lowercase();
    Ok(format!("{base}-{suffix}"))
}

/// 生成高熵随机口令：从无歧义字符集中均匀采样。
fn generate_random_password(len: usize) -> String {
    use rand::Rng;
    // 去除易混淆字符（0/O、1/l/I）的字符集，便于人工誊抄
    const CHARSET: &[u8] = b"ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnpqrstuvwxyz23456789";
    let mut rng = rand::thread_rng();
    (0..len)
        .map(|_| {
            let idx = rng.gen_range(0..CHARSET.len());
            CHARSET[idx] as char
        })
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;
    use tokio::sync::Mutex;

    /// 串行化所有触碰引导环境变量的异步测试。
    ///
    /// 进程级环境变量是全局可变状态，多测试并发设置会串味；用异步互斥锁把
    /// “设置 env → 执行引导 → 清理 env”整段串行化，跨 await 持锁。
    static ENV_LOCK: Mutex<()> = Mutex::const_new(());

    /// 在受控引导环境变量下执行 bootstrap_admin，并在结束后清理环境。
    async fn bootstrap_with_env(
        username: Option<&str>,
        password: Option<&str>,
        meta: &MetaStore,
    ) -> BootstrapOutcome {
        let _guard = ENV_LOCK.lock().await;
        match username {
            Some(v) => std::env::set_var(ENV_ADMIN_USERNAME, v),
            None => std::env::remove_var(ENV_ADMIN_USERNAME),
        }
        match password {
            Some(v) => std::env::set_var(ENV_ADMIN_PASSWORD, v),
            None => std::env::remove_var(ENV_ADMIN_PASSWORD),
        }
        let outcome = bootstrap_admin(meta).await.unwrap();
        std::env::remove_var(ENV_ADMIN_USERNAME);
        std::env::remove_var(ENV_ADMIN_PASSWORD);
        outcome
    }

    #[test]
    fn 哈希与校验往返成功且哈希非明文() {
        let hash = hash_password("正确口令-S3cret").unwrap();
        assert_ne!(hash, "正确口令-S3cret");
        assert!(hash.starts_with("$argon2"));
        assert!(verify_password("正确口令-S3cret", &hash));
        assert!(!verify_password("错误口令", &hash));
    }

    #[test]
    fn 校验非法哈希格式返回_false() {
        assert!(!verify_password("任意口令", "不是合法的-PHC-串"));
    }

    #[test]
    fn 同一口令两次哈希因随机盐而不同() {
        let h1 = hash_password("same").unwrap();
        let h2 = hash_password("same").unwrap();
        assert_ne!(h1, h2);
        // 但都能校验通过
        assert!(verify_password("same", &h1));
        assert!(verify_password("same", &h2));
    }

    #[tokio::test]
    async fn 空库经_env_创建管理员() {
        let meta = MetaStore::open_in_memory().await.unwrap();
        let outcome = bootstrap_with_env(Some("ops"), Some("EnvP@ss123"), &meta).await;

        match outcome {
            BootstrapOutcome::CreatedFromEnv { username } => assert_eq!(username, "ops"),
            other => panic!("预期 CreatedFromEnv，实际 {other:?}"),
        }
        assert_eq!(meta.count_users().await.unwrap(), 1);
        let user = meta.get_user_by_username("ops").await.unwrap().unwrap();
        assert_eq!(user.role, "admin");
        // 入库的是哈希而非明文
        assert_ne!(user.password_hash, "EnvP@ss123");
        assert!(verify_password("EnvP@ss123", &user.password_hash));
    }

    #[tokio::test]
    async fn 第二次引导不重复创建() {
        let meta = MetaStore::open_in_memory().await.unwrap();
        let _ = bootstrap_with_env(Some("ops"), Some("EnvP@ss123"), &meta).await;
        // 已有用户后再次引导应是 AlreadyInitialized，且计数不变
        let second = bootstrap_with_env(Some("ops2"), Some("Another1"), &meta).await;
        assert!(matches!(second, BootstrapOutcome::AlreadyInitialized));
        assert_eq!(meta.count_users().await.unwrap(), 1);
    }

    #[tokio::test]
    async fn env_缺失时走随机口令路径() {
        let meta = MetaStore::open_in_memory().await.unwrap();
        let outcome = bootstrap_with_env(None, None, &meta).await;
        match outcome {
            BootstrapOutcome::CreatedWithRandomPassword { username, password } => {
                assert_eq!(username, DEFAULT_ADMIN_USERNAME);
                assert_eq!(password.len(), RANDOM_PASSWORD_LEN);
                // 随机口令能登录默认管理员
                let user = meta
                    .get_user_by_username(DEFAULT_ADMIN_USERNAME)
                    .await
                    .unwrap()
                    .unwrap();
                assert!(verify_password(&password, &user.password_hash));
            }
            other => panic!("预期 CreatedWithRandomPassword，实际 {other:?}"),
        }
    }

    #[tokio::test]
    async fn env_仅给用户名缺口令则走随机口令路径() {
        let meta = MetaStore::open_in_memory().await.unwrap();
        let outcome = bootstrap_with_env(Some("ops"), None, &meta).await;
        // 用户名与口令须同时提供才走 env 路径，否则兜底随机口令
        assert!(matches!(
            outcome,
            BootstrapOutcome::CreatedWithRandomPassword { .. }
        ));
    }

    // ===== FR-34 外部认证 → 本地用户映射（守 ADR-0010）=====

    /// 构造一个 OIDC 外部主体。
    fn oidc_subject(sub: &str, preferred: &str) -> AuthenticatedSubject {
        AuthenticatedSubject {
            provider: ProviderKind::Oidc,
            subject: sub.to_string(),
            preferred_username: preferred.to_string(),
        }
    }

    #[tokio::test]
    async fn jit_关闭且无绑定用户时拒绝登录() {
        let meta = MetaStore::open_in_memory().await.unwrap();
        let subject = oidc_subject("ext-sub-1", "alice");
        // auto_provision = false：无对应本地用户一律拒绝（最严默认，守不自助注册红线）
        let err = resolve_external_login(&meta, &subject, false)
            .await
            .unwrap_err();
        assert!(matches!(err, ExternalLoginError::NoLocalUser));
        // 拒绝后不得静默建用户
        assert_eq!(meta.count_users().await.unwrap(), 0);
    }

    #[tokio::test]
    async fn jit_关闭但已预置绑定用户时复用登录() {
        let meta = MetaStore::open_in_memory().await.unwrap();
        // 管理员预置外部用户并绑定外部身份
        let uid = meta
            .create_external_user("alice", Role::User, "oidc", "ext-sub-1")
            .await
            .unwrap();
        let subject = oidc_subject("ext-sub-1", "alice-from-idp");
        let user = resolve_external_login(&meta, &subject, false)
            .await
            .unwrap();
        assert_eq!(user.id, uid);
        assert_eq!(user.role, "user");
        // 复用既有用户，不新增
        assert_eq!(meta.count_users().await.unwrap(), 1);
    }

    #[tokio::test]
    async fn jit_开启时即时开通且默认角色_user_绝不_admin() {
        let meta = MetaStore::open_in_memory().await.unwrap();
        let subject = oidc_subject("ext-sub-2", "bob");
        let user = resolve_external_login(&meta, &subject, true).await.unwrap();
        // JIT 开通默认角色固定为最低权限 User，绝不自动 Admin
        assert_eq!(user.role, "user");
        assert_ne!(user.role, "admin");
        assert_eq!(user.username, "bob");
        assert_eq!(user.external_idp.as_deref(), Some("oidc"));
        assert_eq!(user.external_subject.as_deref(), Some("ext-sub-2"));
        assert_eq!(meta.count_users().await.unwrap(), 1);

        // 再次同一外部身份登录复用同一本地用户（不重复建号）
        let again = resolve_external_login(&meta, &subject, true).await.unwrap();
        assert_eq!(again.id, user.id);
        assert_eq!(meta.count_users().await.unwrap(), 1);
    }

    #[tokio::test]
    async fn jit_开通用户名冲突时追加后缀避重() {
        let meta = MetaStore::open_in_memory().await.unwrap();
        // 先占用本地用户名 "carol"
        meta.create_user("carol", "h", Role::Admin).await.unwrap();
        let subject = oidc_subject("ext-sub-3", "carol");
        let user = resolve_external_login(&meta, &subject, true).await.unwrap();
        // 不得复用本地同名管理员账号；新建用户名带后缀且角色为 User
        assert_ne!(user.username, "carol");
        assert!(user.username.starts_with("carol-"));
        assert_eq!(user.role, "user");
    }

    #[tokio::test]
    async fn 绑定的本地用户被禁用时拒绝登录() {
        let meta = MetaStore::open_in_memory().await.unwrap();
        let uid = meta
            .create_external_user("dave", Role::User, "oidc", "ext-sub-4")
            .await
            .unwrap();
        meta.update_user(&uid, None, Some(true)).await.unwrap();
        let subject = oidc_subject("ext-sub-4", "dave");
        let err = resolve_external_login(&meta, &subject, true)
            .await
            .unwrap_err();
        assert!(matches!(err, ExternalLoginError::Disabled));
    }

    #[tokio::test]
    async fn 外部用户不能经本地口令登录() {
        // 外部用户口令哈希为占位串，任何明文口令校验均不通过
        let meta = MetaStore::open_in_memory().await.unwrap();
        meta.create_external_user("erin", Role::User, "oidc", "ext-sub-5")
            .await
            .unwrap();
        let user = meta.get_user_by_username("erin").await.unwrap().unwrap();
        assert!(!verify_password("任何口令", &user.password_hash));
        assert!(!verify_password("", &user.password_hash));
    }
}
