//! 慢速攻击（slowloris）超时与通用请求体大小限制中间件（FR-52，ADR-0008 的「慢速攻击超时 +
//! 请求体大小限制」部分）。
//!
//! 仅做应用层（L7）防护：
//! - **慢速 drip 超时**：把请求体包成带超时的数据流——等待首个数据块超过 `header_timeout`、或相邻
//!   数据块间隔超过 `body_read_timeout` 即判为慢速连接并断开，避免连接长期占用。**这是「块间空闲
//!   超时」而非「整体超时」**：只要客户端持续有数据到达就不触发，因此对正常大文件流式上传（mvn
//!   deploy 大 jar、docker push 大层）友好，只惩罚长时间不发数据的 slowloris。
//! - **通用请求体大小上限**：对**所有请求**的请求体设可配置上限（`max_body_bytes`），超限返回
//!   `413`。区别于 `limits.max_artifact_size`（仅约束制品上传体），本项是兜底上限；带 `Content-Length`
//!   时在进入业务前即拒（不读体），分块传输则边读边计、超限即断开。
//!
//! 设计要点（对齐 testing-and-quality §2.7）：
//! - **不误杀正常流式上传**：默认关闭；超时按「块间空闲」判定而非整体时长；通用体上限默认 0（不启用），
//!   启用时应设得高于预期最大制品体。正常大制品持续发数据不会触发空闲超时。
//! - **热路径低开销**：未启用时直接放行、零包裹开销；启用时仅给请求体套一层流式计时 / 计数包装，
//!   逐块惰性处理，不缓冲整个体、不整体载入内存。
//! - **L3/L4 不在此实现**：体积型攻击交前置反向代理 / CDN / WAF；本中间件只在应用层切断慢速连接、
//!   兜底超大体。
//! - **配置即时生效**：超时档位与体上限从 `AppState.config` 读取，配置热替换后下个请求即按新值判定。

use std::time::Duration;

use axum::{
    body::{Body, Bytes},
    extract::{Request, State},
    http::{header::CONTENT_LENGTH, StatusCode},
    middleware::Next,
    response::{IntoResponse, Response},
    Json,
};
use futures_util::StreamExt;
use serde_json::json;

use super::AppState;

/// 慢速攻击防护中间件：置于身份解析 / 限流之外（更靠近连接侧），在读取请求体前介入。
///
/// 未启用（`enabled=false`）时直接放行、零开销。启用时：先按 `Content-Length` 做一次廉价的超大体
/// 预拒（带长度时不读体即返回 413）；再把请求体包成带「首块等待超时 + 块间空闲超时 + 累计字节上限」
/// 的数据流，慢速 drip 触发超时即断流、分块体超限即断流，正常持续发数据的上传不受影响。
pub async fn slowloris_layer(
    State(state): State<AppState>,
    request: Request,
    next: Next,
) -> Response {
    let cfg = &state.config.protection.slowloris;
    if !cfg.enabled {
        return next.run(request).await;
    }

    let max_body_bytes = cfg.max_body_bytes;
    // —— 廉价预拒：带 Content-Length 且超过通用上限，进入业务前即拒 413，不读体 ——
    if max_body_bytes > 0 {
        if let Some(len) = content_length(&request) {
            if len > max_body_bytes {
                return payload_too_large();
            }
        }
    }

    let header_timeout = Duration::from_secs(cfg.header_timeout_secs.max(1));
    let body_read_timeout = Duration::from_secs(cfg.body_read_timeout_secs.max(1));

    // 拆出请求体，套一层带超时与字节上限的流式包装后重组请求；正常上传逐块透传、不缓冲整体。
    let (parts, body) = request.into_parts();
    let guarded = guard_body_stream(body, header_timeout, body_read_timeout, max_body_bytes);
    let request = Request::from_parts(parts, Body::from_stream(guarded));

    next.run(request).await
}

/// 把请求体的数据流包成带超时与累计字节上限的流：首块用 `header_timeout` 计时、后续块用
/// `body_read_timeout` 计时，累计字节超过 `max_body_bytes`（>0 时）即以错误终止。
///
/// 用 [`futures_util::stream::unfold`] 线性串接状态（流 + 是否首块 + 已读字节），逐块惰性处理；
/// 任一超时 / 超限以 `io::Error` 结束流，axum 据此把请求体读取标记为失败、断开连接，不缓冲整个体。
fn guard_body_stream(
    body: Body,
    header_timeout: Duration,
    body_read_timeout: Duration,
    max_body_bytes: u64,
) -> impl futures_util::Stream<Item = Result<Bytes, std::io::Error>> {
    // 状态：底层数据流、是否仍在等首块（决定用哪个超时）、已累计读取字节数、是否已终止。
    // `done` 置位后立即结束本流，避免在超时 / 超限后继续 poll 慢速底层流（否则会一直阻塞）。
    let init = (body.into_data_stream(), true, 0u64, false);
    futures_util::stream::unfold(init, move |(mut stream, is_first, read, done)| async move {
        // 已终止：不再 poll 底层流，直接结束（防超时后继续阻塞在慢速连接上）
        if done {
            return None;
        }
        // 首块等待用 header_timeout，后续块间用 body_read_timeout，区分「慢起始」与「慢 drip」。
        let timeout = if is_first {
            header_timeout
        } else {
            body_read_timeout
        };
        match tokio::time::timeout(timeout, stream.next()).await {
            // 在超时窗内收到一个数据块
            Ok(Some(Ok(chunk))) => {
                let read = read.saturating_add(chunk.len() as u64);
                // 通用体上限（>0 时启用）：累计超限即以错误终止流（分块体的兜底拦截）
                if max_body_bytes > 0 && read > max_body_bytes {
                    return Some((
                        Err(std::io::Error::other(BodyLimitExceeded)),
                        (stream, false, read, true),
                    ));
                }
                Some((Ok(chunk), (stream, false, read, false)))
            }
            // 底层流的读取错误，原样透传后终止（连接中断 / 截断等）
            Ok(Some(Err(e))) => Some((Err(std::io::Error::other(e)), (stream, false, read, true))),
            // 底层流正常结束：终止本流
            Ok(None) => None,
            // 超时：判为慢速连接，以错误终止流并断开（slowloris / 慢速 POST 防护）
            Err(_) => Some((
                Err(std::io::Error::new(
                    std::io::ErrorKind::TimedOut,
                    SlowRequestTimeout,
                )),
                (stream, false, read, true),
            )),
        }
    })
}

/// 读请求的 `Content-Length` 头（合法 `u64` 时返回）；缺失 / 非法返回 None。
fn content_length(request: &Request) -> Option<u64> {
    request
        .headers()
        .get(CONTENT_LENGTH)
        .and_then(|v| v.to_str().ok())
        .and_then(|s| s.trim().parse::<u64>().ok())
}

/// 通用请求体超过大小上限的错误载荷（供日志 / 流错误识别）。
#[derive(Debug)]
struct BodyLimitExceeded;

impl std::fmt::Display for BodyLimitExceeded {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "请求体超过通用大小上限")
    }
}

impl std::error::Error for BodyLimitExceeded {}

/// 慢速请求超时的错误载荷（供日志 / 流错误识别）。
#[derive(Debug)]
struct SlowRequestTimeout;

impl std::fmt::Display for SlowRequestTimeout {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "请求体读取超时（疑似慢速攻击）")
    }
}

impl std::error::Error for SlowRequestTimeout {}

/// 构造 413 响应：统一错误体，提示请求体过大。
fn payload_too_large() -> Response {
    let body = Json(json!({
        "error": {
            "code": "payload_too_large",
            "message": "请求体超过大小上限",
        }
    }));
    (StatusCode::PAYLOAD_TOO_LARGE, body).into_response()
}

#[cfg(test)]
mod tests {
    use super::*;
    use axum::http::Request as HttpRequest;

    /// 带 Content-Length 且超上限时，content_length 能正确解析，供预拒判定。
    #[test]
    fn content_length_解析合法长度() {
        let req = HttpRequest::builder()
            .header(CONTENT_LENGTH, "1024")
            .body(Body::empty())
            .unwrap();
        assert_eq!(content_length(&req), Some(1024));
    }

    /// 缺失或非法 Content-Length 返回 None（不误判、回落到流式计数）。
    #[test]
    fn content_length_缺失或非法返回_none() {
        let req = HttpRequest::builder().body(Body::empty()).unwrap();
        assert_eq!(content_length(&req), None);
        let req = HttpRequest::builder()
            .header(CONTENT_LENGTH, "abc")
            .body(Body::empty())
            .unwrap();
        assert_eq!(content_length(&req), None);
    }

    /// 正常持续到达的数据块全部透传、字节不变（不误杀正常流式上传）。
    #[tokio::test]
    async fn 正常数据流逐块透传不超时不超限() {
        // 用一个立即给出多块、随后结束的流构造请求体
        let chunks: Vec<Result<Bytes, std::io::Error>> = vec![
            Ok(Bytes::from_static(b"hello")),
            Ok(Bytes::from_static(b"world")),
        ];
        let body = Body::from_stream(futures_util::stream::iter(chunks));
        // 超时给足、上限设大：应全部透传
        let guarded =
            guard_body_stream(body, Duration::from_secs(10), Duration::from_secs(10), 1024);
        let collected: Vec<_> = guarded.collect().await;
        let total: usize = collected
            .iter()
            .map(|r| r.as_ref().map(|b| b.len()).unwrap_or(0))
            .sum();
        assert!(collected.iter().all(|r| r.is_ok()), "正常块不应出错");
        assert_eq!(total, 10, "字节应原样透传");
    }

    /// 累计字节超过通用上限时以错误终止流（分块体超限的兜底拦截）。
    #[tokio::test]
    async fn 分块体超通用上限以错误终止() {
        let chunks: Vec<Result<Bytes, std::io::Error>> = vec![
            Ok(Bytes::from_static(b"aaaa")), // 4
            Ok(Bytes::from_static(b"bbbb")), // 累计 8，超上限 5
        ];
        let body = Body::from_stream(futures_util::stream::iter(chunks));
        let guarded = guard_body_stream(body, Duration::from_secs(10), Duration::from_secs(10), 5);
        let collected: Vec<_> = guarded.collect().await;
        // 至少出现一次错误（越限块），证明被拦截
        assert!(
            collected.iter().any(|r| r.is_err()),
            "累计超上限应以错误终止"
        );
    }

    /// 上限为 0（不启用通用体上限）时，任意大小的分块体都不因体上限被拦截。
    #[tokio::test]
    async fn 上限为零不限制体大小() {
        let chunks: Vec<Result<Bytes, std::io::Error>> = (0..50)
            .map(|_| Ok(Bytes::from_static(b"xxxxxxxx")))
            .collect();
        let body = Body::from_stream(futures_util::stream::iter(chunks));
        let guarded = guard_body_stream(body, Duration::from_secs(10), Duration::from_secs(10), 0);
        let collected: Vec<_> = guarded.collect().await;
        assert!(
            collected.iter().all(|r| r.is_ok()),
            "上限为 0 时不应因体大小拦截"
        );
    }

    /// 慢速 drip：相邻数据块间隔超过空闲超时即以超时错误终止流（slowloris 防护核心）。
    #[tokio::test]
    async fn 慢速drip块间超时被断() {
        // 构造一个第一块立即到、第二块迟迟不到（远超空闲超时）的流
        let slow = futures_util::stream::once(async {
            Ok::<Bytes, std::io::Error>(Bytes::from_static(b"first"))
        })
        .chain(futures_util::stream::once(async {
            // 第二块前睡很久，模拟 drip 停顿
            tokio::time::sleep(Duration::from_secs(60)).await;
            Ok(Bytes::from_static(b"late"))
        }));
        let body = Body::from_stream(slow);
        // 首块超时给足、块间空闲超时设很短：第二块等待将超时
        let guarded =
            guard_body_stream(body, Duration::from_secs(10), Duration::from_millis(50), 0);
        let collected: Vec<_> = guarded.collect().await;
        // 第一块应透传，随后因块间空闲超时出现一次错误
        assert!(
            collected.first().map(|r| r.is_ok()).unwrap_or(false),
            "首块应正常透传"
        );
        assert!(
            collected.iter().any(|r| r.is_err()),
            "块间空闲超时应以错误终止（慢速攻击被断）"
        );
    }

    /// 慢起始：发完头后迟迟不发首块，等待超过首块超时即以超时错误终止（慢速起始防护）。
    #[tokio::test]
    async fn 慢起始首块超时被断() {
        let slow = futures_util::stream::once(async {
            tokio::time::sleep(Duration::from_secs(60)).await;
            Ok::<Bytes, std::io::Error>(Bytes::from_static(b"first"))
        });
        let body = Body::from_stream(slow);
        // 首块超时很短：首块等待将超时
        let guarded =
            guard_body_stream(body, Duration::from_millis(50), Duration::from_secs(10), 0);
        let collected: Vec<_> = guarded.collect().await;
        assert!(
            collected.iter().any(|r| r.is_err()),
            "首块等待超时应以错误终止（慢起始被断）"
        );
    }
}

// ============ 中间件端到端测试（经真实路由）============
#[cfg(test)]
mod middleware_tests {
    use super::super::tests::测试用状态;
    use super::super::{build_router, AppState};
    use axum::body::Body;
    use axum::http::{Request, StatusCode};
    use std::sync::Arc;
    use tower::ServiceExt;

    /// 以给定慢速攻击防护配置定制测试状态。
    async fn 慢速防护状态(
        enabled: bool,
        max_body_bytes: u64,
    ) -> (AppState, tempfile::TempDir) {
        let (mut state, dir) = 测试用状态().await;
        let mut cfg = (*state.config).clone();
        cfg.protection.slowloris.enabled = enabled;
        cfg.protection.slowloris.max_body_bytes = max_body_bytes;
        // 超时给足，隔离观察体上限维度（超时维度由模块单元测试覆盖）
        cfg.protection.slowloris.header_timeout_secs = 30;
        cfg.protection.slowloris.body_read_timeout_secs = 30;
        state.config = Arc::new(cfg);
        (state, dir)
    }

    /// 未启用时：即便请求体很大也照常进入业务、不被防护拦截（防误杀基线）。
    #[tokio::test]
    async fn 未启用时大体请求正常放行() {
        let (state, _dir) = 慢速防护状态(false, 0).await;
        let app = build_router(state);
        // 带超大 Content-Length 也不应被拦（未启用）
        let req = Request::builder()
            .uri("/health")
            .header("content-length", "999999999")
            .body(Body::from(vec![0u8; 1024]))
            .unwrap();
        let st = app.oneshot(req).await.unwrap().status();
        assert_eq!(st, StatusCode::OK);
    }

    /// 启用 + 带 Content-Length 且超通用上限：进入业务前即返回 413（廉价预拒，不读体）。
    #[tokio::test]
    async fn 超大content_length进入业务前拒413() {
        let (state, _dir) = 慢速防护状态(true, 1024).await;
        let app = build_router(state);
        let req = Request::builder()
            .uri("/health")
            .header("content-length", "2048")
            .body(Body::from(vec![0u8; 2048]))
            .unwrap();
        let st = app.oneshot(req).await.unwrap().status();
        assert_eq!(st, StatusCode::PAYLOAD_TOO_LARGE);
    }

    /// 启用但请求体在上限内：正常放行进入业务（防误杀）。
    #[tokio::test]
    async fn 上限内请求正常放行() {
        let (state, _dir) = 慢速防护状态(true, 4096).await;
        let app = build_router(state);
        let req = Request::builder()
            .uri("/health")
            .header("content-length", "16")
            .body(Body::from(vec![0u8; 16]))
            .unwrap();
        let st = app.oneshot(req).await.unwrap().status();
        assert_eq!(st, StatusCode::OK);
    }

    /// 启用但通用上限为 0（不启用体上限）：任意 Content-Length 都不被预拒（仅做慢速超时）。
    #[tokio::test]
    async fn 通用上限为零不预拒大体() {
        let (state, _dir) = 慢速防护状态(true, 0).await;
        let app = build_router(state);
        let req = Request::builder()
            .uri("/health")
            .header("content-length", "999999999")
            .body(Body::empty())
            .unwrap();
        let st = app.oneshot(req).await.unwrap().status();
        // 上限为 0：不因体大小预拒；/health 无体、超时不触发，正常 200
        assert_eq!(st, StatusCode::OK);
    }
}
