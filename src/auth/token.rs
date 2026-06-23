//! API Token 的生成、哈希与比对（ADR-0003）。
//!
//! Token 是高熵随机串，带可辨识前缀 `jna_`，仅在签发时明文返回一次；
//! DB 只存其 sha256 哈希。校验走稳定哈希后等值匹配，并用定长比较避免计时侧信道。
//! 注意：与口令不同，Token 本身已是高熵随机串，无需 argon2 慢哈希，sha256 足够且快。

use sha2::{Digest, Sha256};
use subtle::ConstantTimeEq;

/// API Token 明文前缀，便于在日志 / 配置中辨识（注意：明文本身绝不入日志）。
pub const TOKEN_PREFIX: &str = "jna_";
/// 随机部分的字节数（256 位高熵），以 base32 无歧义字符表达为明文。
const TOKEN_RANDOM_BYTES: usize = 32;

/// 生成一枚新的 API Token 明文（含 `jna_` 前缀）。仅在签发时返回一次。
pub fn generate_api_token() -> String {
    use rand::RngCore;
    let mut raw = [0u8; TOKEN_RANDOM_BYTES];
    rand::thread_rng().fill_bytes(&mut raw);
    format!("{TOKEN_PREFIX}{}", encode_token_body(&raw))
}

/// 计算 Token 的 sha256 哈希（小写十六进制），用于入库与比对。
///
/// 对完整明文（含前缀）取哈希，保证校验时算法一致。
pub fn hash_api_token(token: &str) -> String {
    let mut hasher = Sha256::new();
    hasher.update(token.as_bytes());
    hex_encode(&hasher.finalize())
}

/// 定长比较两个哈希串是否相等，避免计时侧信道。
///
/// 长度不同直接判否（已天然不同）；长度相同时逐字节定长比较。
pub fn verify_api_token(presented_hash: &str, stored_hash: &str) -> bool {
    let a = presented_hash.as_bytes();
    let b = stored_hash.as_bytes();
    if a.len() != b.len() {
        return false;
    }
    a.ct_eq(b).into()
}

/// 把随机字节编码为无歧义的小写 base32 风格字符集字符串。
fn encode_token_body(bytes: &[u8]) -> String {
    // 去除易混淆字符（0/o、1/l/i）的字符集，便于人工誊抄
    const CHARSET: &[u8] = b"abcdefghjkmnpqrstuvwxyz23456789";
    // 每字节映射到字符集，足够承载随机性（非严格 base32，仅作明文编码）
    bytes
        .iter()
        .map(|b| CHARSET[(*b as usize) % CHARSET.len()] as char)
        .collect()
}

/// 把字节切片编码为小写十六进制字符串。
fn hex_encode(bytes: &[u8]) -> String {
    use std::fmt::Write;
    let mut s = String::with_capacity(bytes.len() * 2);
    for b in bytes {
        let _ = write!(s, "{:02x}", b);
    }
    s
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn 生成的_token_带前缀且足够长() {
        let t = generate_api_token();
        assert!(t.starts_with(TOKEN_PREFIX));
        assert!(t.len() > TOKEN_PREFIX.len() + 20);
    }

    #[test]
    fn 两次生成的_token_不同() {
        assert_ne!(generate_api_token(), generate_api_token());
    }

    #[test]
    fn 哈希稳定且非明文() {
        let t = generate_api_token();
        let h1 = hash_api_token(&t);
        let h2 = hash_api_token(&t);
        assert_eq!(h1, h2);
        assert_ne!(h1, t);
        // sha256 十六进制固定 64 字符
        assert_eq!(h1.len(), 64);
    }

    #[test]
    fn 定长比较命中与不命中() {
        let t = generate_api_token();
        let h = hash_api_token(&t);
        assert!(verify_api_token(&h, &h));
        // 篡改一位即不相等
        let mut wrong = h.clone();
        wrong.replace_range(0..1, "x");
        assert!(!verify_api_token(&wrong, &h));
        // 长度不同直接判否
        assert!(!verify_api_token("短", &h));
    }
}
