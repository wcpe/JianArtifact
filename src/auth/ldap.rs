//! LDAP bind 认证集成（FR-35 / ADR-0016）。
//!
//! 采用 **bind 校验**模式（ADR-0016 §4）：用配置的 `bind_dn` + bind 口令连接目录，
//! 按 `user_search_base` + 过滤模板查到用户 DN，再用该 DN + 用户提交的口令做一次
//! bind；bind 成功即认证通过，产出 [`AuthenticatedSubject`]，经既有
//! [`super::resolve_external_login`] JIT 映射得本地用户后照常签发会话/JWT。
//!
//! LDAP 仅参与口令型登录（Web 表单 / Basic Auth），经统一的口令型
//! [`AuthProvider::authenticate_password`] 接入；下游四通道与鉴权矩阵不变。
//!
//! TLS：连接走 LDAPS（`ldaps://`）或 StartTLS，由 rustls 提供；默认不接受明文
//! `ldap://`，除非运维显式在可信内网开启 `allow_insecure`。
//!
//! 凭据脱敏：bind 口令真源 env/配置，绝不入库 / 进日志 / 进 DB 明文；用户提交的口令
//! 仅用于一次 bind 校验、不留存、不进日志。

use ldap3::{LdapConnAsync, LdapConnSettings, Scope, SearchEntry};

use super::provider::{AuthProvider, AuthenticatedSubject, ProviderKind};
use super::AuthError;

/// 过滤模板中代表「用户提交用户名」的占位符。
const USERNAME_PLACEHOLDER: &str = "{username}";
/// 默认用户搜索过滤模板（按 `uid` 匹配，适配 OpenLDAP；AD 常用 `sAMAccountName`）。
const DEFAULT_USER_FILTER: &str = "(uid={username})";

/// LDAP 运行期配置（由应用配置 `[auth.ldap]` 装配）。
///
/// `bind_password` 是密钥：真源 env/配置，绝不入库 / 进日志。
#[derive(Clone)]
pub struct LdapSettings {
    /// 目录服务 URL（`ldaps://host:636` 或 `ldap://host:389`）。
    pub url: String,
    /// 搜索绑定 DN（服务账号），用于连接后先查用户 DN。
    pub bind_dn: String,
    /// 搜索绑定口令（敏感）；真源 env/配置，绝不入库 / 进日志。
    pub bind_password: String,
    /// 用户搜索基准 DN（如 `ou=people,dc=example,dc=org`）。
    pub user_search_base: String,
    /// 用户搜索过滤模板，含 `{username}` 占位符（如 `(uid={username})`）。
    pub user_filter: String,
    /// 取作建议用户名的属性名（如 `uid` / `cn` / `sAMAccountName`）；为空则回退提交的用户名。
    pub username_attr: String,
    /// 是否使用 StartTLS（在明文端口上协商升级 TLS）；为 false 且 URL 为 `ldaps://` 时走 LDAPS。
    pub starttls: bool,
    /// 是否允许明文 `ldap://`（无 TLS）：默认 false，仅运维在可信内网显式开启。
    pub allow_insecure: bool,
    /// 连接超时（秒）。
    pub conn_timeout_secs: u64,
}

impl std::fmt::Debug for LdapSettings {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        // 绝不在调试输出中泄露 bind_password
        f.debug_struct("LdapSettings")
            .field("url", &self.url)
            .field("bind_dn", &self.bind_dn)
            .field("bind_password", &"<已脱敏>")
            .field("user_search_base", &self.user_search_base)
            .field("user_filter", &self.user_filter)
            .field("username_attr", &self.username_attr)
            .field("starttls", &self.starttls)
            .field("allow_insecure", &self.allow_insecure)
            .field("conn_timeout_secs", &self.conn_timeout_secs)
            .finish()
    }
}

/// LDAP provider：持有配置，按口令型登录路径做 bind 校验。
///
/// 实现 [`AuthProvider`]，与本地、OIDC provider 并存于同一抽象；只回答「你是谁」，
/// 不做授权（仍由 `authz` 判定）。
#[derive(Debug)]
pub struct LdapProvider {
    settings: LdapSettings,
}

impl LdapProvider {
    /// 构造 provider。
    pub fn new(settings: LdapSettings) -> Self {
        Self { settings }
    }

    /// 连接目录：按配置应用连接超时与 StartTLS，返回可用的 `Ldap` 句柄。
    ///
    /// 连接驱动任务 spawn 到当前 tokio executor（ldap3 异步连接约定）。
    /// 明文 `ldap://` 在未显式 `allow_insecure` 时已由 [`Self::authenticate_password`] 前置拒绝。
    async fn connect(&self) -> Result<ldap3::Ldap, ldap3::LdapError> {
        let settings = LdapConnSettings::new()
            .set_conn_timeout(std::time::Duration::from_secs(
                self.settings.conn_timeout_secs,
            ))
            .set_starttls(self.settings.starttls);
        let (conn, ldap) = LdapConnAsync::with_settings(settings, &self.settings.url).await?;
        // 驱动连接 IO 的后台任务；连接结束即退出，错误仅记 WARN 不影响主流程。
        ldap3::drive!(conn);
        Ok(ldap)
    }

    /// 查用户 DN 与建议用户名：用服务账号 bind 后按过滤模板搜索唯一用户条目。
    ///
    /// 返回 `(用户 DN, 建议用户名)`；无匹配 / 多匹配 / 搜索失败均返回认证失败（不泄露细节）。
    async fn search_user(
        &self,
        ldap: &mut ldap3::Ldap,
        username: &str,
    ) -> Result<(String, String), AuthError> {
        // 服务账号 bind（仅用于搜索；口令绝不进日志）
        ldap.simple_bind(&self.settings.bind_dn, &self.settings.bind_password)
            .await
            .map_err(map_ldap_err)?
            .success()
            .map_err(|e| {
                tracing::warn!(错误 = %e, "LDAP 服务账号 bind 失败");
                AuthError::ExternalAuth
            })?;

        // 用归一后的过滤模板（空模板回退默认），再代入转义后的用户名
        let filter = build_user_filter(self.settings.normalized_filter(), username);
        let attrs = search_attrs(&self.settings.username_attr);
        let (entries, _res) = ldap
            .search(
                &self.settings.user_search_base,
                Scope::Subtree,
                &filter,
                attrs,
            )
            .await
            .map_err(map_ldap_err)?
            .success()
            .map_err(|e| {
                tracing::warn!(错误 = %e, "LDAP 用户搜索失败");
                AuthError::ExternalAuth
            })?;

        // 必须唯一命中：无匹配或多匹配都视为认证失败（避免歧义放行）
        let entry = unique_entry(entries).ok_or_else(|| {
            tracing::warn!("LDAP 用户搜索无唯一匹配，拒绝认证");
            AuthError::ExternalAuth
        })?;
        let entry = SearchEntry::construct(entry);
        Ok(subject_from_entry(
            entry,
            &self.settings.username_attr,
            username,
        ))
    }
}

impl AuthProvider for LdapProvider {
    fn kind(&self) -> ProviderKind {
        ProviderKind::Ldap
    }

    /// 口令型登录：① 校验 TLS 安全前置；② 服务账号搜出用户 DN；③ 用该 DN + 用户口令做
    /// 一次 bind，成功即认证通过，产出外部主体（subject = 用户 DN）。
    ///
    /// 任何环节失败统一返回 [`AuthError::ExternalAuth`]，不泄露「用户是否存在 / 哪步失败」。
    async fn authenticate_password(
        &self,
        username: &str,
        password: &str,
    ) -> Result<AuthenticatedSubject, AuthError> {
        // 空口令直接拒：LDAP 对空口令 bind 可能被视作匿名 bind 而「成功」，须前置挡掉
        if username.is_empty() || password.is_empty() {
            return Err(AuthError::ExternalAuth);
        }
        // TLS 安全前置：默认不接受明文 ldap://（除非运维显式 allow_insecure）
        ensure_secure_transport(
            &self.settings.url,
            self.settings.starttls,
            self.settings.allow_insecure,
        )?;

        // 第一次连接：服务账号搜出用户 DN 与建议用户名
        let mut ldap = self.connect().await.map_err(map_ldap_err)?;
        let (user_dn, preferred_username) = self.search_user(&mut ldap, username).await?;
        let _ = ldap.unbind().await;

        // 第二次连接：用用户 DN + 用户提交口令做 bind 校验（口令绝不进日志）
        let mut user_ldap = self.connect().await.map_err(map_ldap_err)?;
        let bind_ok = user_ldap
            .simple_bind(&user_dn, password)
            .await
            .map_err(map_ldap_err)?
            .success()
            .is_ok();
        let _ = user_ldap.unbind().await;

        if !bind_ok {
            tracing::warn!(用户名 = %username, "LDAP 用户 bind 校验失败：口令错误");
            return Err(AuthError::ExternalAuth);
        }

        tracing::info!(用户名 = %username, "LDAP bind 校验通过");
        Ok(AuthenticatedSubject {
            provider: ProviderKind::Ldap,
            // 外部稳定标识用用户 DN（与 provider kind 共同构成外部身份键）
            subject: user_dn,
            preferred_username,
        })
    }
}

/// 把 ldap3 错误统一收敛为外部认证失败（目录不可达 / 超时 / 协议错误等）。
///
/// 不向上抛 LDAP 内部细节，避免泄露目录拓扑；细节仅记 WARN 日志。
fn map_ldap_err(e: ldap3::LdapError) -> AuthError {
    tracing::warn!(错误 = %e, "LDAP 目录交互失败");
    AuthError::ExternalAuth
}

impl LdapSettings {
    /// 把可能为空的过滤模板归一为有效模板（空则用默认 `(uid={username})`）。
    pub fn normalized_filter(&self) -> &str {
        if self.user_filter.trim().is_empty() {
            DEFAULT_USER_FILTER
        } else {
            self.user_filter.as_str()
        }
    }
}

/// 校验传输层安全：默认不接受明文 `ldap://`，除非启用了 StartTLS 或运维显式 `allow_insecure`。
///
/// `ldaps://` 始终视为安全；`ldap://` 仅在 `starttls = true` 或 `allow_insecure = true` 时放行。
fn ensure_secure_transport(
    url: &str,
    starttls: bool,
    allow_insecure: bool,
) -> Result<(), AuthError> {
    let lower = url.trim().to_ascii_lowercase();
    let is_ldaps = lower.starts_with("ldaps://");
    if is_ldaps || starttls || allow_insecure {
        return Ok(());
    }
    tracing::warn!(
        "LDAP URL 为明文 ldap:// 且未启用 StartTLS / allow_insecure，按安全默认拒绝连接"
    );
    Err(AuthError::ExternalAuth)
}

/// 按 RFC 4515 转义用户名后代入过滤模板的 `{username}` 占位符。
///
/// 防 LDAP 过滤注入：把 `* ( ) \\ NUL` 等转义为 `\XX` 形式，再做模板替换。
fn build_user_filter(template: &str, username: &str) -> String {
    let escaped = escape_filter_value(username);
    template.replace(USERNAME_PLACEHOLDER, &escaped)
}

/// RFC 4515 过滤值转义：对特殊字符按 `\XX`（两位十六进制）编码，防注入。
fn escape_filter_value(value: &str) -> String {
    let mut out = String::with_capacity(value.len());
    for b in value.as_bytes() {
        match b {
            b'*' | b'(' | b')' | b'\\' | 0x00 => out.push_str(&format!("\\{b:02x}")),
            _ => out.push(*b as char),
        }
    }
    out
}

/// 计算搜索请求的返回属性列表：配置了用户名属性时只取该属性，否则不取属性（仅要 DN）。
fn search_attrs(username_attr: &str) -> Vec<&str> {
    if username_attr.trim().is_empty() {
        // 仅需 DN：用 "1.1" 显式表示不返回任何属性（RFC 4511）
        vec!["1.1"]
    } else {
        vec![username_attr]
    }
}

/// 从搜索结果中取唯一条目：恰好一个返回该条目，零个或多于一个返回 None。
fn unique_entry(mut entries: Vec<ldap3::ResultEntry>) -> Option<ldap3::ResultEntry> {
    if entries.len() == 1 {
        entries.pop()
    } else {
        None
    }
}

/// 由搜索到的条目组装 `(用户 DN, 建议用户名)`。
///
/// 用户 DN 取条目 `dn`（作外部稳定标识 subject）；建议用户名优先取配置属性的首值，
/// 缺失则回退用户提交的用户名。
fn subject_from_entry(
    entry: SearchEntry,
    username_attr: &str,
    submitted_username: &str,
) -> (String, String) {
    let preferred = entry
        .attrs
        .get(username_attr)
        .and_then(|vals| vals.first())
        .filter(|v| !v.is_empty())
        .cloned()
        .unwrap_or_else(|| submitted_username.to_string());
    (entry.dn, preferred)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::HashMap;

    #[test]
    fn 过滤模板替换占位符() {
        let f = build_user_filter("(uid={username})", "alice");
        assert_eq!(f, "(uid=alice)");
        // AD 风格属性同样可用
        let f = build_user_filter("(sAMAccountName={username})", "bob");
        assert_eq!(f, "(sAMAccountName=bob)");
    }

    #[test]
    fn 过滤值转义防注入() {
        // 注入用的通配符 / 括号 / 反斜杠须被转义为 \XX，避免改变过滤语义
        let f = build_user_filter("(uid={username})", "a*)(uid=*");
        assert_eq!(f, "(uid=a\\2a\\29\\28uid=\\2a)");
        // 反斜杠自身
        assert_eq!(escape_filter_value("a\\b"), "a\\5cb");
        // 普通用户名不受影响
        assert_eq!(escape_filter_value("normal.user-1"), "normal.user-1");
    }

    #[test]
    fn ldaps_始终视为安全() {
        assert!(ensure_secure_transport("ldaps://dir:636", false, false).is_ok());
        // 大小写不敏感
        assert!(ensure_secure_transport("LDAPS://dir:636", false, false).is_ok());
    }

    #[test]
    fn 明文_ldap_默认被拒() {
        let err = ensure_secure_transport("ldap://dir:389", false, false).unwrap_err();
        assert!(matches!(err, AuthError::ExternalAuth));
    }

    #[test]
    fn 明文_ldap_启用_starttls_或_allow_insecure_放行() {
        // StartTLS 升级后视为安全
        assert!(ensure_secure_transport("ldap://dir:389", true, false).is_ok());
        // 运维显式允许明文（可信内网）
        assert!(ensure_secure_transport("ldap://dir:389", false, true).is_ok());
    }

    #[test]
    fn 搜索属性按配置选择() {
        // 配置了用户名属性：只取该属性
        assert_eq!(search_attrs("uid"), vec!["uid"]);
        // 未配置：用 "1.1" 表示不返回任何属性（仅要 DN）
        assert_eq!(search_attrs(""), vec!["1.1"]);
        assert_eq!(search_attrs("   "), vec!["1.1"]);
    }

    #[test]
    fn 唯一条目判定() {
        // 工具函数：零条 / 多条均视为非唯一
        assert!(unique_entry(Vec::new()).is_none());
    }

    /// 造一个带指定 dn 与属性的搜索条目（绕过 ldap3 内部构造，直接填 SearchEntry）。
    fn 造条目(dn: &str, attrs: &[(&str, &str)]) -> SearchEntry {
        let mut map: HashMap<String, Vec<String>> = HashMap::new();
        for (k, v) in attrs {
            map.entry(k.to_string()).or_default().push(v.to_string());
        }
        SearchEntry {
            dn: dn.to_string(),
            attrs: map,
            bin_attrs: HashMap::new(),
        }
    }

    #[test]
    fn 主体取_dn_与建议用户名() {
        let entry = 造条目("uid=alice,ou=people,dc=ex,dc=org", &[("uid", "alice")]);
        let (dn, preferred) = subject_from_entry(entry, "uid", "ALICE");
        assert_eq!(dn, "uid=alice,ou=people,dc=ex,dc=org");
        // 建议用户名取目录属性首值（而非用户提交的大小写）
        assert_eq!(preferred, "alice");
    }

    #[test]
    fn 缺属性时建议用户名回退提交值() {
        let entry = 造条目("uid=bob,dc=ex,dc=org", &[]);
        let (dn, preferred) = subject_from_entry(entry, "uid", "bob");
        assert_eq!(dn, "uid=bob,dc=ex,dc=org");
        assert_eq!(preferred, "bob");
    }

    #[test]
    fn 默认过滤模板归一() {
        let s = base_settings("");
        assert_eq!(s.normalized_filter(), "(uid={username})");
        let s = base_settings("(cn={username})");
        assert_eq!(s.normalized_filter(), "(cn={username})");
    }

    #[test]
    fn debug_脱敏_bind_口令() {
        let s = base_settings("(uid={username})");
        let dbg = format!("{s:?}");
        assert!(dbg.contains("<已脱敏>"));
        // 绝不出现明文口令
        assert!(!dbg.contains("super-secret-bind-pw"));
    }

    #[test]
    fn provider_类别为_ldap() {
        let p = LdapProvider::new(base_settings("(uid={username})"));
        assert_eq!(p.kind(), ProviderKind::Ldap);
    }

    #[tokio::test]
    async fn 空用户名或空口令直接拒绝() {
        let p = LdapProvider::new(base_settings("(uid={username})"));
        assert!(matches!(
            p.authenticate_password("", "pw").await.unwrap_err(),
            AuthError::ExternalAuth
        ));
        assert!(matches!(
            p.authenticate_password("alice", "").await.unwrap_err(),
            AuthError::ExternalAuth
        ));
    }

    #[tokio::test]
    async fn 明文_ldap_默认拒绝连接() {
        // url 为明文 ldap:// 且未放行：authenticate_password 应在连接前即拒绝
        let mut s = base_settings("(uid={username})");
        s.url = "ldap://127.0.0.1:1/".to_string();
        let p = LdapProvider::new(s);
        let err = p.authenticate_password("alice", "pw").await.unwrap_err();
        assert!(matches!(err, AuthError::ExternalAuth));
    }

    /// 造一份基础测试配置（bind 口令为占位测试值）。
    fn base_settings(filter: &str) -> LdapSettings {
        LdapSettings {
            url: "ldaps://dir.example:636".to_string(),
            bind_dn: "cn=svc,dc=ex,dc=org".to_string(),
            bind_password: "super-secret-bind-pw".to_string(),
            user_search_base: "ou=people,dc=ex,dc=org".to_string(),
            user_filter: filter.to_string(),
            username_attr: "uid".to_string(),
            starttls: false,
            allow_insecure: false,
            conn_timeout_secs: 10,
        }
    }
}
