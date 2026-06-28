//! 代理凭据落库加密（ADR-0030）：XChaCha20-Poly1305 AEAD 的纯函数封装。
//!
//! 仅用于把网络代理密码以**密文**落 `app_settings`（明文密码绝不入库，守红线 1）。
//! 加密密钥经 `JwtSigner::derive_key(b"proxy-credential-encryption-v1")` 派生（域分隔），
//! 真源是数据目录的 `.jwt_secret` 文件，密钥本体绝不入库 / 不进日志（守红线 2）。
//!
//! 算法选型（ADR-0030）：XChaCha20-Poly1305（192 位随机 nonce，无 AES-GCM 96 位生日界顾虑），
//! RustCrypto 纯 Rust，与项目既有加密生态（argon2 / sha2）一致，不用 ring 作直接加密 API。

use base64::{engine::general_purpose::STANDARD, Engine};
use chacha20poly1305::{
    aead::{Aead, KeyInit},
    XChaCha20Poly1305, XNonce,
};

/// XChaCha20-Poly1305 的 nonce 长度（192 位 = 24 字节）。
const NONCE_LEN: usize = 24;

/// 代理凭据加密子密钥的派生域（ADR-0030）：经 `JwtSigner::derive_key` 域分隔派生 32 字节子密钥。
///
/// 与会话 JWT、CC 挑战等其它域互不串味；勿改动此常量，否则已落库密文将无法解密。
pub const PROXY_KEY_DOMAIN: &[u8] = b"proxy-credential-encryption-v1";

/// 用 32 字节密钥加密明文，返回 `base64(nonce ‖ ciphertext)`。
///
/// 每次加密用一枚 192 位随机 nonce（XChaCha20 的扩展 nonce 空间足够大，随机选取碰撞概率可忽略），
/// 把 `nonce ‖ ciphertext` 整体 base64 编码为可落库字符串。AEAD 同时提供机密性与完整性
/// （Poly1305 标签随密文，防篡改 / 截断）。`key` 为派生子密钥（绝不是 JWT 密钥本体）。
pub fn encrypt_secret(key: &[u8; 32], plaintext: &str) -> String {
    let cipher = XChaCha20Poly1305::new(key.into());
    // 192 位随机 nonce：用项目既有 rand，避免为编码 / 随机另引 crate
    let mut nonce_bytes = [0u8; NONCE_LEN];
    {
        use rand::RngCore;
        rand::thread_rng().fill_bytes(&mut nonce_bytes);
    }
    let nonce = XNonce::from_slice(&nonce_bytes);
    // 明文加密失败在本场景几乎不可能（仅极端内存不足），按 AEAD 约定回落空密文交解密侧判失败；
    // 不 panic、不泄露明文。正常路径恒成功。
    let ciphertext = cipher
        .encrypt(nonce, plaintext.as_bytes())
        .unwrap_or_default();
    // 拼 nonce ‖ ciphertext 再整体 base64
    let mut blob = Vec::with_capacity(NONCE_LEN + ciphertext.len());
    blob.extend_from_slice(&nonce_bytes);
    blob.extend_from_slice(&ciphertext);
    STANDARD.encode(blob)
}

/// 用 32 字节密钥解密 [`encrypt_secret`] 产出的 `base64(nonce ‖ ciphertext)`，返回明文。
///
/// 解析失败（非法 base64 / 长度不足 / 认证标签不符 / 错密钥）一律返回 `None`、不 panic——
/// 启动恢复侧据此降级为「无密码」并记 WARN，不阻断启动。
pub fn decrypt_secret(key: &[u8; 32], blob: &str) -> Option<String> {
    let raw = STANDARD.decode(blob.trim()).ok()?;
    // 至少要容下 nonce（密文可为空，但 AEAD 标签 16 字节，故实际更长；此处只做下界保护）
    if raw.len() < NONCE_LEN {
        return None;
    }
    let (nonce_bytes, ciphertext) = raw.split_at(NONCE_LEN);
    let cipher = XChaCha20Poly1305::new(key.into());
    let nonce = XNonce::from_slice(nonce_bytes);
    let plaintext = cipher.decrypt(nonce, ciphertext).ok()?;
    String::from_utf8(plaintext).ok()
}

#[cfg(test)]
mod tests {
    use super::*;

    /// 便捷：构造一把确定性测试密钥。
    fn 密钥(seed: u8) -> [u8; 32] {
        [seed; 32]
    }

    #[test]
    fn 加密解密往返还原明文() {
        let key = 密钥(1);
        let plain = "supersecretpw";
        let blob = encrypt_secret(&key, plain);
        assert_eq!(decrypt_secret(&key, &blob).as_deref(), Some(plain));
    }

    #[test]
    fn 密文不含明文子串() {
        let key = 密钥(2);
        let plain = "p@ss:w/ord-明文";
        let blob = encrypt_secret(&key, plain);
        // base64 密文中不得出现明文任何片段（机密性）
        assert!(!blob.contains("p@ss"), "密文不得含明文：{blob}");
        assert!(!blob.contains(plain), "密文不得含明文：{blob}");
    }

    #[test]
    fn 错密钥解不开返回_none() {
        let plain = "secret";
        let blob = encrypt_secret(&密钥(3), plain);
        // 换一把密钥解密：AEAD 认证失败 → None（不 panic、不误还原）
        assert_eq!(decrypt_secret(&密钥(4), &blob), None);
    }

    #[test]
    fn 同一明文两次加密_nonce_随机致密文不同() {
        let key = 密钥(5);
        let a = encrypt_secret(&key, "same");
        let b = encrypt_secret(&key, "same");
        // 随机 nonce 使两次密文不同（避免确定性加密的可关联性）
        assert_ne!(a, b, "随机 nonce 应使同明文密文不同");
        // 但都能正确解回同一明文
        assert_eq!(decrypt_secret(&key, &a).as_deref(), Some("same"));
        assert_eq!(decrypt_secret(&key, &b).as_deref(), Some("same"));
    }

    #[test]
    fn 非法_base64_解密返回_none() {
        assert_eq!(decrypt_secret(&密钥(6), "@@@不是 base64@@@"), None);
    }

    #[test]
    fn 长度不足_解密返回_none() {
        // 合法 base64 但长度短于 nonce → None
        let short = STANDARD.encode([0u8; 4]);
        assert_eq!(decrypt_secret(&密钥(7), &short), None);
    }

    #[test]
    fn 篡改密文_认证失败返回_none() {
        let key = 密钥(8);
        let blob = encrypt_secret(&key, "tamper-me");
        let mut raw = STANDARD.decode(&blob).unwrap();
        // 翻转最后一字节（落在 Poly1305 标签或密文上）→ 认证失败
        let last = raw.len() - 1;
        raw[last] ^= 0xff;
        let tampered = STANDARD.encode(raw);
        assert_eq!(decrypt_secret(&key, &tampered), None);
    }

    #[test]
    fn 空明文可往返() {
        let key = 密钥(9);
        let blob = encrypt_secret(&key, "");
        assert_eq!(decrypt_secret(&key, &blob).as_deref(), Some(""));
    }
}
