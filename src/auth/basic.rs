//! Basic Auth 凭据解析（ADR-0003）。
//!
//! 解析 `Authorization: Basic base64(user:secret)`。secret 既可是用户口令（argon2 校验），
//! 也可是 API Token（哈希校验），由调用方按 secret 形态分派，兼容包管理器 CLI。

use base64::engine::general_purpose::STANDARD;
use base64::Engine;

/// 解析出的 Basic Auth 凭据（用户名与密文，密文可为口令或 Token）。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct BasicCredentials {
    /// 用户名。
    pub username: String,
    /// 密文：用户口令或 API Token。
    pub secret: String,
}

/// 从 `Authorization` 头值解析 Basic 凭据；非 Basic 方案或格式非法返回 None。
///
/// 入参为完整头值（如 `Basic dXNlcjpwYXNz`）。不区分大小写匹配 `Basic ` 方案前缀。
pub fn parse_basic_auth(header_value: &str) -> Option<BasicCredentials> {
    let rest = strip_scheme_prefix(header_value, "Basic ")?;
    let decoded = STANDARD.decode(rest.trim()).ok()?;
    let text = String::from_utf8(decoded).ok()?;
    // 仅在首个冒号处分割：口令中允许包含冒号
    let (username, secret) = text.split_once(':')?;
    Some(BasicCredentials {
        username: username.to_string(),
        secret: secret.to_string(),
    })
}

/// 大小写不敏感地剥离方案前缀（如 `Basic ` / `Bearer `），返回其后内容。
///
/// 用 `get(..prefix_len)` 取前缀，前缀长度落在多字节字符内部时返回 None（避免 `split_at`
/// 在非字符边界 panic）；方案前缀本身为 ASCII，正常请求不受影响。
pub fn strip_scheme_prefix<'a>(header_value: &'a str, scheme_with_space: &str) -> Option<&'a str> {
    let prefix_len = scheme_with_space.len();
    let head = header_value.get(..prefix_len)?;
    if head.eq_ignore_ascii_case(scheme_with_space) {
        // 前缀为 ASCII，命中后 prefix_len 必为字符边界，切片安全
        Some(&header_value[prefix_len..])
    } else {
        None
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// 构造 `Basic <base64>` 头值。
    fn basic_header(user: &str, secret: &str) -> String {
        let encoded = STANDARD.encode(format!("{user}:{secret}"));
        format!("Basic {encoded}")
    }

    #[test]
    fn 解析合法_basic_凭据() {
        let h = basic_header("alice", "p@ss:word");
        let creds = parse_basic_auth(&h).unwrap();
        assert_eq!(creds.username, "alice");
        // 口令中的冒号应被保留（只在首个冒号处分割）
        assert_eq!(creds.secret, "p@ss:word");
    }

    #[test]
    fn 方案前缀大小写不敏感() {
        let encoded = STANDARD.encode("bob:secret");
        let creds = parse_basic_auth(&format!("basic {encoded}")).unwrap();
        assert_eq!(creds.username, "bob");
    }

    #[test]
    fn 非_basic_方案返回_none() {
        assert!(parse_basic_auth("Bearer abc").is_none());
        assert!(parse_basic_auth("Basic").is_none());
    }

    #[test]
    fn 非法_base64_返回_none() {
        assert!(parse_basic_auth("Basic !!!不是base64").is_none());
    }

    #[test]
    fn 缺少冒号分隔返回_none() {
        let encoded = STANDARD.encode("没有冒号");
        assert!(parse_basic_auth(&format!("Basic {encoded}")).is_none());
    }

    #[test]
    fn 剥离方案前缀() {
        assert_eq!(strip_scheme_prefix("Bearer xyz", "Bearer "), Some("xyz"));
        assert_eq!(strip_scheme_prefix("bearer xyz", "Bearer "), Some("xyz"));
        assert_eq!(strip_scheme_prefix("Basic xyz", "Bearer "), None);
    }

    #[test]
    fn 前缀长度落在多字节字符内不_panic() {
        // 头值短于前缀且首字符多字节：get(..7) 落在字符内部，应返回 None 而非 panic
        assert_eq!(strip_scheme_prefix("不是有效令牌", "Bearer "), None);
        assert_eq!(strip_scheme_prefix("中", "Basic "), None);
        assert_eq!(strip_scheme_prefix("", "Bearer "), None);
    }
}
