//! 认证 provider 抽象（FR-34 / ADR-0016）。
//!
//! 把「一次外部认证」收敛为一个统一结果 [`AuthenticatedSubject`]：只回答「你是谁」
//! （证明外部身份），**不负责授权**——能否读写某仓库仍由 `authz` 按 ADR-0004 判定。
//!
//! provider 只是「身份从哪来」的扩展点：本地用户名/密码始终启用（默认 provider），
//! OIDC 经浏览器授权码流接入（见 [`super::oidc`]），后续 LDAP（FR-35）经口令型
//! [`AuthProvider::authenticate_password`] 接入同一抽象——本批不实现 LDAP，仅留接口。

/// 认证 provider 类别。以小写字符串落配置与外部身份键，避免魔法字符串散落。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ProviderKind {
    /// 本地用户名/密码（argon2），默认且始终启用。
    Local,
    /// OIDC 授权码流（FR-34）。
    Oidc,
    /// LDAP bind 校验（FR-35，本批未实现，仅占位于抽象内）。
    Ldap,
}

impl ProviderKind {
    /// 转为入库 / 配置用的稳定字符串。
    pub fn as_str(self) -> &'static str {
        match self {
            ProviderKind::Local => "local",
            ProviderKind::Oidc => "oidc",
            ProviderKind::Ldap => "ldap",
        }
    }
}

/// 已认证的外部主体：provider 证明的「外部身份」，尚未映射到本地用户。
///
/// 仅含非敏感的身份标识与展示信息，**绝不含任何外部凭据**（口令 / token / client_secret）。
/// 由「外部身份 → 本地用户」映射（见 [`super::resolve_external_login`]）落到本地账号后，
/// 才照常签发既有本地会话/JWT。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct AuthenticatedSubject {
    /// 产出该主体的 provider 类别。
    pub provider: ProviderKind,
    /// 外部稳定标识（OIDC `sub` / LDAP DN 等），与 provider 类别共同构成外部身份键。
    pub subject: String,
    /// 建议用户名（来自 IdP 的 `preferred_username` / `email` / `sub`），JIT 开通时取用。
    pub preferred_username: String,
}

/// 认证 provider：把「凭证表单 / 外部回调」解析为已认证的外部主体（或失败）。
///
/// 口令型 provider（本地、后续 LDAP）实现 [`authenticate_password`]；OIDC 走浏览器
/// 授权码流、不套口令法，由独立回调编排产出 [`AuthenticatedSubject`]（见 `super::oidc`）。
///
/// [`authenticate_password`]: AuthProvider::authenticate_password
#[allow(async_fn_in_trait)]
pub trait AuthProvider: Send + Sync {
    /// provider 类别标识，用于配置与外部身份键。
    fn kind(&self) -> ProviderKind;

    /// 用口令型凭据认证（本地、LDAP bind 走此路径）；OIDC 不实现此法。
    ///
    /// 成功产出 [`AuthenticatedSubject`]；凭据无效 / provider 不支持口令认证返回 Err。
    async fn authenticate_password(
        &self,
        username: &str,
        password: &str,
    ) -> Result<AuthenticatedSubject, super::AuthError>;
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn provider_类别字符串稳定() {
        assert_eq!(ProviderKind::Local.as_str(), "local");
        assert_eq!(ProviderKind::Oidc.as_str(), "oidc");
        assert_eq!(ProviderKind::Ldap.as_str(), "ldap");
    }
}
