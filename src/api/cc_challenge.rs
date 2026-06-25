//! CC（HTTP 洪水）挑战——工作量证明 PoW（FR-54，ADR-0008 的「CC 挑战」部分）。
//!
//! 对疑似 CC 攻击的来源下发一个**工作量证明（Proof of Work）挑战**：客户端须找到一个 `nonce`，
//! 使 `sha256(challenge_token + ":" + nonce)` 的二进制前导零位数达到配置难度，解出后带证明重试
//! 方放行。难度可配置，难度越高客户端算力开销越大、攻击者刷流成本越高，而正常单次请求成本可忽略。
//!
//! **服务端无状态校验**：挑战令牌由服务端用 HMAC-SHA256（密钥经 Web 会话 JWT 真源密钥域分隔派生，
//! 不暴露其本体、与会话 JWT 不串味）签名其载荷（绑定来源 IP + 签发时刻 + 难度），不在服务端存大量
//! 挑战态；校验时重算 HMAC 比对、查
//! 过期、查 PoW 是否达难度即可。一次性弱保证由「短过期 + 绑定来源 IP」承担（无状态实现不追求严格
//! 单次，符合「简单优先」；如需严格一次性可后续引入有界内存登记，当前不预留）。
//!
//! 触发策略（关键的误杀防范，对齐 testing-and-quality §2.7）：
//! - **默认关闭**：正常包管理器 CLI（mvn / npm / docker / curl）**不会解 PoW**，无差别拦截会直接
//!   打断正常拉取。故默认关闭，启用与否由运维显式承担。
//! - **已认证豁免**：默认对已认证（Bearer / Basic / 会话）请求豁免——CLI 通常带凭据，豁免使其
//!   不受挑战影响；挑战只面向匿名可疑流量。
//! - **仅匿名触发**：启用且未豁免时，仅对**匿名**请求要求 PoW 证明；带合法证明即放行。
//!
//! 设计要点：
//! - **热路径低开销**：未启用时直接放行、零开销；启用时仅对匿名请求做一次 HMAC + 一次 SHA256
//!   校验（无锁、无 IO、无 DB）；正常已认证请求走豁免快路径。
//! - **防绕过**：挑战绑定**连接级来源 IP**（取 `ConnectInfo`，**不采信 XFF**），换 IP 的证明
//!   不可复用；HMAC 签名防伪造挑战；过期证明被拒。
//! - **配置即时生效**：开关 / 难度 / 过期 / 豁免从 `AppState.config` 读取，配置热替换后下个请求即按新值判定。

use std::net::SocketAddr;
use std::time::{SystemTime, UNIX_EPOCH};

use axum::{
    extract::{ConnectInfo, Request, State},
    http::{HeaderMap, StatusCode},
    middleware::Next,
    response::{IntoResponse, Response},
    Json,
};
use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use base64::Engine;
use serde_json::json;
use sha2::{Digest, Sha256};
use subtle::ConstantTimeEq;

use crate::auth::AuthIdentity;
use crate::metrics_keys as keys;

use super::alerts::ProtectionDimension;
use super::AppState;

/// 客户端提交 PoW 解的请求头：值形如 `<challenge_token>:<nonce>`。
const SOLUTION_HEADER: &str = "x-cc-solution";
/// 难度上限（前导零位数）：防误配过高难度把客户端卡死；64 位足够苛刻且仍可解。
const MAX_DIFFICULTY_BITS: u32 = 64;

/// CC 挑战签名器：持有 HMAC 密钥，负责无状态地签发与校验 PoW 挑战令牌。
///
/// 密钥由调用方传入（生产用会话 JWT 真源密钥域分隔派生的子密钥），随 `AppState` 经 `Arc` 共享。
/// 不持有任何挑战态——签发即把绑定信息编码进令牌并 HMAC 签名，校验时重算比对，故线程安全且无锁。
pub struct CcChallenger {
    /// HMAC-SHA256 密钥（不在 Debug 中泄露）。
    secret: Vec<u8>,
}

impl std::fmt::Debug for CcChallenger {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        // 绝不在调试输出中泄露密钥本体
        f.debug_struct("CcChallenger").finish_non_exhaustive()
    }
}

/// 校验失败原因（供中间件区分应答，便于客户端理解为何被拒）。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum VerifyError {
    /// 证明头缺失或格式非法（缺 nonce / 分隔符）。
    Malformed,
    /// 挑战令牌签名不符（伪造 / 篡改）。
    BadSignature,
    /// 挑战令牌已过期。
    Expired,
    /// 挑战令牌绑定的来源 IP 与当前连接 IP 不一致（换 IP 复用证明）。
    IpMismatch,
    /// PoW 未达难度（nonce 不满足前导零位数）。
    InsufficientWork,
}

impl CcChallenger {
    /// 用给定密钥构造签名器（生产传入由会话 JWT 真源密钥域分隔派生的子密钥）。
    pub fn new(secret: &[u8]) -> Self {
        Self {
            secret: secret.to_vec(),
        }
    }

    /// 为某来源 IP 在某难度下签发一个挑战令牌（无状态）。
    ///
    /// 令牌载荷 = `ip|issued_at|difficulty`，附 HMAC 签名；整体 base64url 编码为不含分隔歧义的串。
    /// 令牌本身公开可见（无机密），其不可伪造性由 HMAC 保证。
    fn issue(&self, ip: &str, difficulty: u32, now: u64) -> String {
        let payload = format!("{ip}|{now}|{difficulty}");
        let sig = self.sign(payload.as_bytes());
        let token = format!("{}.{}", b64(payload.as_bytes()), b64(&sig));
        token
    }

    /// 校验一个挑战令牌 + nonce 的 PoW 解：签名、过期、来源 IP 绑定、难度四关全过方返回 Ok。
    ///
    /// `now` 由调用方传入便于测试可控时钟。
    fn verify(
        &self,
        token: &str,
        nonce: &str,
        client_ip: &str,
        max_age_secs: u64,
        now: u64,
    ) -> Result<(), VerifyError> {
        // 令牌结构：base64url(payload).base64url(sig)
        let (payload_b64, sig_b64) = token.split_once('.').ok_or(VerifyError::Malformed)?;
        let payload = unb64(payload_b64).ok_or(VerifyError::Malformed)?;
        let sig = unb64(sig_b64).ok_or(VerifyError::Malformed)?;

        // —— 关 1：HMAC 签名比对（常量时间，防时序侧信道与伪造）——
        let expect = self.sign(&payload);
        if expect.ct_eq(&sig).unwrap_u8() != 1 {
            return Err(VerifyError::BadSignature);
        }

        // 解析载荷 ip|issued_at|difficulty
        let payload_str = std::str::from_utf8(&payload).map_err(|_| VerifyError::Malformed)?;
        let mut parts = payload_str.split('|');
        let bound_ip = parts.next().ok_or(VerifyError::Malformed)?;
        let issued_at: u64 = parts
            .next()
            .and_then(|s| s.parse().ok())
            .ok_or(VerifyError::Malformed)?;
        let difficulty: u32 = parts
            .next()
            .and_then(|s| s.parse().ok())
            .ok_or(VerifyError::Malformed)?;

        // —— 关 2：过期（now < issued_at 视作时钟回拨，按未过期容忍；超出 max_age 即拒）——
        if now.saturating_sub(issued_at) > max_age_secs {
            return Err(VerifyError::Expired);
        }
        // —— 关 3：来源 IP 绑定（换 IP 复用证明不放行，防绕过）——
        if bound_ip != client_ip {
            return Err(VerifyError::IpMismatch);
        }
        // —— 关 4：PoW 难度（sha256(token:nonce) 前导零位数 ≥ difficulty）——
        if !meets_difficulty(token, nonce, difficulty) {
            return Err(VerifyError::InsufficientWork);
        }
        Ok(())
    }

    /// 计算 HMAC-SHA256（复用 auth 层的统一实现，避免重复构造）。
    fn sign(&self, msg: &[u8]) -> [u8; 32] {
        crate::auth::hmac_sha256(&self.secret, msg)
    }
}

/// 判定 `sha256(token + ":" + nonce)` 的二进制前导零位数是否达到 `difficulty`。
///
/// 纯函数，便于穷举测试；难度即「前导零比特数」，与常见 PoW（hashcash）一致。
fn meets_difficulty(token: &str, nonce: &str, difficulty: u32) -> bool {
    // 难度 0 视作不要求工作量（任何 nonce 均满足，便于测试与最低难度退化）
    if difficulty == 0 {
        return true;
    }
    let mut hasher = Sha256::new();
    hasher.update(token.as_bytes());
    hasher.update(b":");
    hasher.update(nonce.as_bytes());
    let digest = hasher.finalize();
    leading_zero_bits(&digest) >= difficulty
}

/// 计算字节序列的二进制前导零位数（从最高位起连续的 0 比特数）。
fn leading_zero_bits(bytes: &[u8]) -> u32 {
    let mut count = 0u32;
    for &b in bytes {
        if b == 0 {
            count += 8;
        } else {
            count += b.leading_zeros();
            break;
        }
    }
    count
}

/// base64url 无填充编码。
fn b64(bytes: &[u8]) -> String {
    URL_SAFE_NO_PAD.encode(bytes)
}

/// base64url 无填充解码；非法输入返回 None。
fn unb64(s: &str) -> Option<Vec<u8>> {
    URL_SAFE_NO_PAD.decode(s).ok()
}

/// 当前 Unix 时间（秒）；系统时间早于纪元时回退 0。
fn now_unix() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0)
}

/// CC 挑战中间件：仅对匿名可疑流量要求 PoW 工作量证明，已认证客户端默认豁免。
///
/// 顺序：未启用 → 直接放行（零开销）；已认证且配置豁免 → 放行；匿名请求 → 校验证明头，
/// 带合法证明即放行，否则签发新挑战并返回 `429`（带挑战参数）要求客户端解出后重试。
/// 取不到连接 IP（如单元测试未注入 `ConnectInfo`）时不挑战、直接放行，避免误伤无 IP 上下文。
pub async fn cc_challenge_layer(
    State(state): State<AppState>,
    request: Request,
    next: Next,
) -> Response {
    let cfg = &state.config.protection.cc_challenge;
    if !cfg.enabled {
        return next.run(request).await;
    }

    // 已认证（Bearer / Basic / 会话）且配置豁免：放行——避免误伤带凭据的包管理器 CLI
    let is_authenticated = matches!(
        request.extensions().get::<AuthIdentity>(),
        Some(AuthIdentity::Authenticated(_))
    );
    if is_authenticated && cfg.exempt_authenticated {
        return next.run(request).await;
    }

    // 取连接级来源 IP（不采信 XFF）；缺失则不挑战、直接放行
    let Some(client_ip) = client_ip(&request) else {
        return next.run(request).await;
    };

    let challenger = &state.cc_challenger;
    let difficulty = cfg.difficulty.min(MAX_DIFFICULTY_BITS);
    let max_age = cfg.ttl_secs.max(1);
    let now = now_unix();

    // 带证明头：校验通过即放行，否则按原因重新下发挑战
    if let Some((token, nonce)) = parse_solution(request.headers()) {
        match challenger.verify(&token, &nonce, &client_ip, max_age, now) {
            Ok(()) => return next.run(request).await,
            Err(_) => {
                // 证明无效（伪造 / 过期 / 换 IP / 工作量不足）：计失败 + 告警评估，下发新挑战要求重新解
                record_failure(&state);
                let new_token = challenger.issue(&client_ip, difficulty, now);
                record_issued();
                return challenge_required(&new_token, difficulty, max_age);
            }
        }
    }

    // 无证明头：签发挑战，要求客户端解出后带证明重试
    let token = challenger.issue(&client_ip, difficulty, now);
    record_issued();
    challenge_required(&token, difficulty, max_age)
}

/// 累加 CC 挑战下发计数（FR-56）。metrics 未启用时宏为 no-op；热路径只做原子累加。
fn record_issued() {
    metrics::counter!(keys::CC_CHALLENGE_ISSUED_TOTAL).increment(1);
}

/// 累加 CC 挑战失败计数并做告警评估（FR-56）。
///
/// 失败指带证明但校验未过（工作量不足 / 过期 / 伪造 / 换 IP 复用）；按 `CcChallenge` 维度累加窗内
/// 计数，达阈值即告警。热路径只做原子累加 + 一次内存计数，不做 IO。
fn record_failure(state: &AppState) {
    metrics::counter!(keys::CC_CHALLENGE_FAILED_TOTAL).increment(1);
    state.alert_engine.record(
        ProtectionDimension::CcChallenge,
        &state.config.protection.alerts,
        std::time::Instant::now(),
    );
}

/// 取连接级来源 IP（由 `into_make_service_with_connect_info` 注入）；缺失返回 None。
///
/// 只认连接对端地址，**不读 `X-Forwarded-For` 等可伪造头**，确保换 IP 的证明不可复用。
fn client_ip(request: &Request) -> Option<String> {
    request
        .extensions()
        .get::<ConnectInfo<SocketAddr>>()
        .map(|ci| ci.0.ip().to_string())
}

/// 从请求头解析客户端提交的 PoW 解：`X-CC-Solution: <challenge_token>:<nonce>`。
///
/// 返回 `(challenge_token, nonce)`；头缺失或缺少分隔符返回 None。挑战令牌内已无 `:`
/// （仅含 base64url 字符与一个 `.`），故按**最后一个** `:` 切分出 nonce。
fn parse_solution(headers: &HeaderMap) -> Option<(String, String)> {
    let raw = headers.get(SOLUTION_HEADER)?.to_str().ok()?;
    let (token, nonce) = raw.rsplit_once(':')?;
    if token.is_empty() || nonce.is_empty() {
        return None;
    }
    Some((token.to_string(), nonce.to_string()))
}

/// 构造「需完成 CC 挑战」的 429 响应：携带挑战令牌、难度、过期，指示客户端解出 PoW 后带证明重试。
fn challenge_required(token: &str, difficulty: u32, ttl_secs: u64) -> Response {
    let body = Json(json!({
        "error": {
            "code": "cc_challenge_required",
            "message": "需完成工作量证明（PoW）挑战后重试",
        },
        "challenge": {
            // 客户端须找到 nonce 使 sha256(token + ":" + nonce) 前导零位 ≥ difficulty，
            // 然后以请求头 X-CC-Solution: <token>:<nonce> 重发原请求。
            "type": "pow_sha256_leading_zero_bits",
            "token": token,
            "difficulty": difficulty,
            "ttl_secs": ttl_secs,
            "solution_header": SOLUTION_HEADER,
        }
    }));
    (StatusCode::TOO_MANY_REQUESTS, body).into_response()
}

#[cfg(test)]
mod tests {
    use super::*;

    /// 暴力求解满足难度的 nonce（仅测试用，难度取小值）。
    fn solve(token: &str, difficulty: u32) -> String {
        for n in 0u64.. {
            let nonce = n.to_string();
            if meets_difficulty(token, &nonce, difficulty) {
                return nonce;
            }
        }
        unreachable!()
    }

    fn challenger() -> CcChallenger {
        CcChallenger::new(b"cc-secret-32-bytes-xxxxxxxxxxxxxx")
    }

    #[test]
    fn 前导零位数计算正确() {
        assert_eq!(leading_zero_bits(&[0x00, 0x00, 0xFF]), 16);
        assert_eq!(leading_zero_bits(&[0x0F, 0xFF]), 4);
        assert_eq!(leading_zero_bits(&[0xFF]), 0);
        assert_eq!(leading_zero_bits(&[0x00, 0x01]), 15);
    }

    #[test]
    fn 难度为零任何nonce均满足() {
        assert!(meets_difficulty("tok", "anything", 0));
    }

    #[test]
    fn 正确解出后校验通过() {
        let c = challenger();
        let now = 1_000;
        // 用低难度便于快速求解
        let difficulty = 8;
        let token = c.issue("1.1.1.1", difficulty, now);
        let nonce = solve(&token, difficulty);
        assert!(c.verify(&token, &nonce, "1.1.1.1", 300, now).is_ok());
    }

    #[test]
    fn 错误nonce被拒工作量不足() {
        let c = challenger();
        let now = 1_000;
        let difficulty = 16; // 较高难度，几乎任意小 nonce 都不满足
        let token = c.issue("1.1.1.1", difficulty, now);
        // 用一个大概率不满足 16 位前导零的 nonce
        let err = c.verify(&token, "0", "1.1.1.1", 300, now).unwrap_err();
        // 该 nonce 不满足难度（极小概率满足时换断言也无妨，这里固定 nonce 已验证不满足）
        assert_eq!(err, VerifyError::InsufficientWork);
    }

    #[test]
    fn 伪造或篡改令牌被拒() {
        let c = challenger();
        let now = 1_000;
        let token = c.issue("1.1.1.1", 8, now);
        // 篡改签名段
        let mut tampered = token.clone();
        tampered.push('x');
        let err = c.verify(&tampered, "0", "1.1.1.1", 300, now).unwrap_err();
        assert!(matches!(
            err,
            VerifyError::BadSignature | VerifyError::Malformed
        ));
        // 他密钥签发的令牌本机校验不过
        let other = CcChallenger::new(b"other-secret-yyyyyyyyyyyyyyyyyyyy");
        let foreign = other.issue("1.1.1.1", 8, now);
        let nonce = solve(&foreign, 8);
        assert_eq!(
            c.verify(&foreign, &nonce, "1.1.1.1", 300, now).unwrap_err(),
            VerifyError::BadSignature
        );
    }

    #[test]
    fn 过期令牌被拒() {
        let c = challenger();
        let issued = 1_000;
        let difficulty = 8;
        let token = c.issue("1.1.1.1", difficulty, issued);
        let nonce = solve(&token, difficulty);
        // 超过 max_age 后校验：过期
        let later = issued + 301;
        assert_eq!(
            c.verify(&token, &nonce, "1.1.1.1", 300, later).unwrap_err(),
            VerifyError::Expired
        );
    }

    #[test]
    fn 换ip复用证明被拒() {
        let c = challenger();
        let now = 1_000;
        let difficulty = 8;
        // 绑定 1.1.1.1 的令牌
        let token = c.issue("1.1.1.1", difficulty, now);
        let nonce = solve(&token, difficulty);
        // 换到另一个 IP 提交同一证明：来源 IP 不符被拒（防绕过）
        assert_eq!(
            c.verify(&token, &nonce, "2.2.2.2", 300, now).unwrap_err(),
            VerifyError::IpMismatch
        );
    }

    #[test]
    fn 证明头解析按最后一个冒号切分() {
        let mut headers = HeaderMap::new();
        headers.insert(SOLUTION_HEADER, "abc.def:12345".parse().unwrap());
        let (token, nonce) = parse_solution(&headers).unwrap();
        assert_eq!(token, "abc.def");
        assert_eq!(nonce, "12345");
    }

    #[test]
    fn 证明头缺失或残缺返回none() {
        let headers = HeaderMap::new();
        assert!(parse_solution(&headers).is_none());
        let mut h = HeaderMap::new();
        // 缺 nonce
        h.insert(SOLUTION_HEADER, "abc.def:".parse().unwrap());
        assert!(parse_solution(&h).is_none());
        // 缺分隔符
        let mut h2 = HeaderMap::new();
        h2.insert(SOLUTION_HEADER, "abcdef".parse().unwrap());
        assert!(parse_solution(&h2).is_none());
    }
}

// ============ 中间件端到端测试（经真实路由）============
#[cfg(test)]
mod middleware_tests {
    use super::super::tests::测试用状态;
    use super::super::{build_router, AppState};
    use super::{meets_difficulty, CcChallenger};
    use axum::body::Body;
    use axum::extract::ConnectInfo;
    use axum::http::{Request, StatusCode};
    use http_body_util::BodyExt;
    use std::net::SocketAddr;
    use std::sync::Arc;
    use tower::ServiceExt;

    /// 以给定 CC 挑战配置定制测试状态。
    async fn cc状态(
        enabled: bool,
        difficulty: u32,
        exempt_authenticated: bool,
    ) -> (AppState, tempfile::TempDir) {
        let (mut state, dir) = 测试用状态().await;
        let mut cfg = (*state.config).clone();
        cfg.protection.cc_challenge.enabled = enabled;
        cfg.protection.cc_challenge.difficulty = difficulty;
        cfg.protection.cc_challenge.ttl_secs = 300;
        cfg.protection.cc_challenge.exempt_authenticated = exempt_authenticated;
        state.config = Arc::new(cfg);
        // 用固定密钥的挑战器，确保签发 / 校验跨请求一致
        state.cc_challenger = Arc::new(CcChallenger::new(b"cc-mw-secret-32-bytes-xxxxxxxxxx"));
        (state, dir)
    }

    /// 暴力求解满足难度的 nonce（测试用，难度取小值）。
    fn solve(token: &str, difficulty: u32) -> String {
        for n in 0u64.. {
            let nonce = n.to_string();
            if meets_difficulty(token, &nonce, difficulty) {
                return nonce;
            }
        }
        unreachable!()
    }

    /// 用指定连接 IP 发 GET /health，可选附带证明头；返回 (状态码, 响应体 JSON)。
    async fn 打(
        app: axum::Router,
        ip: &str,
        solution: Option<&str>,
    ) -> (StatusCode, serde_json::Value) {
        let addr: SocketAddr = format!("{ip}:50000").parse().unwrap();
        let mut builder = Request::builder().uri("/health");
        if let Some(s) = solution {
            builder = builder.header("x-cc-solution", s);
        }
        let mut req = builder.body(Body::empty()).unwrap();
        req.extensions_mut().insert(ConnectInfo(addr));
        let resp = app.oneshot(req).await.unwrap();
        let status = resp.status();
        let bytes = resp.into_body().collect().await.unwrap().to_bytes();
        let json = serde_json::from_slice(&bytes).unwrap_or(serde_json::Value::Null);
        (status, json)
    }

    #[tokio::test]
    async fn 未启用时匿名请求正常放行() {
        // 关闭挑战：匿名请求直接放行（防误杀基线，正常 CLI 不受影响）
        let (state, _dir) = cc状态(false, 8, true).await;
        let app = build_router(state);
        let (st, _) = 打(app, "1.1.1.1", None).await;
        assert_eq!(st, StatusCode::OK);
    }

    #[tokio::test]
    async fn 启用后匿名无证明被下发挑战_429() {
        let (state, _dir) = cc状态(true, 8, true).await;
        let app = build_router(state);
        let (st, body) = 打(app, "1.1.1.1", None).await;
        assert_eq!(st, StatusCode::TOO_MANY_REQUESTS);
        // 响应体应含挑战参数（令牌 + 难度），供客户端求解
        assert_eq!(body["error"]["code"], "cc_challenge_required");
        assert_eq!(body["challenge"]["difficulty"], 8);
        assert!(body["challenge"]["token"].is_string());
    }

    #[tokio::test]
    async fn 解出pow后带证明放行() {
        let difficulty = 8;
        let (state, _dir) = cc状态(true, difficulty, true).await;
        let app = build_router(state);
        // 第一次拿到挑战令牌
        let (st, body) = 打(app.clone(), "1.1.1.1", None).await;
        assert_eq!(st, StatusCode::TOO_MANY_REQUESTS);
        let token = body["challenge"]["token"].as_str().unwrap().to_string();
        // 求解并带证明重试：放行
        let nonce = solve(&token, difficulty);
        let solution = format!("{token}:{nonce}");
        let (st2, _) = 打(app, "1.1.1.1", Some(&solution)).await;
        assert_eq!(st2, StatusCode::OK);
    }

    #[tokio::test]
    async fn 错误证明被拒并重新下发挑战() {
        let (state, _dir) = cc状态(true, 16, true).await;
        let app = build_router(state);
        let (_, body) = 打(app.clone(), "1.1.1.1", None).await;
        let token = body["challenge"]["token"].as_str().unwrap().to_string();
        // 带一个工作量不足的 nonce：仍被拒 429，并重新下发挑战
        let bad = format!("{token}:0");
        let (st, body2) = 打(app, "1.1.1.1", Some(&bad)).await;
        assert_eq!(st, StatusCode::TOO_MANY_REQUESTS);
        assert_eq!(body2["error"]["code"], "cc_challenge_required");
    }

    #[tokio::test]
    async fn 换ip复用证明被拒() {
        let difficulty = 8;
        let (state, _dir) = cc状态(true, difficulty, true).await;
        let app = build_router(state);
        // 在 1.1.1.1 上拿到挑战并解出
        let (_, body) = 打(app.clone(), "1.1.1.1", None).await;
        let token = body["challenge"]["token"].as_str().unwrap().to_string();
        let nonce = solve(&token, difficulty);
        let solution = format!("{token}:{nonce}");
        // 换到 2.2.2.2 提交同一证明：来源 IP 不符，仍被拒 429（防绕过）
        let (st, _) = 打(app, "2.2.2.2", Some(&solution)).await;
        assert_eq!(st, StatusCode::TOO_MANY_REQUESTS);
    }

    #[tokio::test]
    async fn 已认证请求豁免不被挑战() {
        // 启用挑战且豁免已认证：带合法 Bearer 会话的请求不被挑战、正常放行（不误伤 CLI）
        use crate::auth::hash_password;
        use crate::meta::Role;
        let (state, _dir) = cc状态(true, 8, true).await;
        let uid = state
            .meta
            .create_user("cc-user", &hash_password("pw").unwrap(), Role::User)
            .await
            .unwrap();
        let token = state.jwt.issue(&uid, "cc-user", Role::User).unwrap();
        let app = build_router(state);
        let addr: SocketAddr = "1.1.1.1:50000".parse().unwrap();
        let mut req = Request::builder()
            .uri("/api/v1/me")
            .header("Authorization", format!("Bearer {token}"))
            .body(Body::empty())
            .unwrap();
        req.extensions_mut().insert(ConnectInfo(addr));
        let st = app.oneshot(req).await.unwrap().status();
        // 已认证豁免：不被 CC 挑战拦截（/me 正常返回 200，而非 429）
        assert_eq!(st, StatusCode::OK);
    }

    #[tokio::test]
    async fn 不豁免已认证时也挑战() {
        // exempt_authenticated=false：即便已认证也要解 PoW（验证开关生效）
        use crate::auth::hash_password;
        use crate::meta::Role;
        let (state, _dir) = cc状态(true, 8, false).await;
        let uid = state
            .meta
            .create_user("cc-user2", &hash_password("pw").unwrap(), Role::User)
            .await
            .unwrap();
        let token = state.jwt.issue(&uid, "cc-user2", Role::User).unwrap();
        let app = build_router(state);
        let addr: SocketAddr = "1.1.1.1:50000".parse().unwrap();
        let mut req = Request::builder()
            .uri("/api/v1/me")
            .header("Authorization", format!("Bearer {token}"))
            .body(Body::empty())
            .unwrap();
        req.extensions_mut().insert(ConnectInfo(addr));
        let st = app.oneshot(req).await.unwrap().status();
        assert_eq!(st, StatusCode::TOO_MANY_REQUESTS);
    }
}
