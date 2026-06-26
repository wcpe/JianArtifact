//! reqwest 上游实现（FR-12）：纯 rustls TLS，流式响应体，按上游基址 + 相对路径拉取。
//!
//! 守 SECURITY：仅 rustls 校验上游 HTTPS 证书，不引 native-tls / openssl；
//! 响应体以流式暴露为 `AsyncRead`，大文件不整体载入内存。

use std::sync::Arc;

use futures_util::TryStreamExt;
use tokio_util::io::StreamReader;

use crate::config::NetworkState;

use super::{Upstream, UpstreamBody, UpstreamError};

/// 基于 reqwest 的上游客户端。
///
/// 不再持有启动期固化的 client，改持出站网络热替换槽 [`NetworkState`]（FR-88，ADR-0022）：
/// 每次回源经 `network.client()` 取当前 client（含运行时 PATCH 后的新代理），下个请求即用新代理。
#[derive(Clone)]
pub struct HttpUpstream {
    /// 出站网络热替换槽（含当前 client，随 PATCH 即时换代理）。
    network: Arc<NetworkState>,
}

impl HttpUpstream {
    /// 持出站网络热替换槽构造上游客户端（FR-88，ADR-0022）。
    ///
    /// 回源时经 `network.client()` 取当前出站 client；代理 / 超时 / rustls / stream 特性由槽统一注入。
    /// 生产装配应传入随 `AppState` 共享的同一槽，方能在设置页 PATCH 改代理后即时生效。
    pub fn with_network_state(network: Arc<NetworkState>) -> Self {
        Self { network }
    }

    /// 便捷构造：以默认空代理 + 给定超时建一个**独立**出站网络槽（不接共享热替换槽）。
    ///
    /// 仅用于测试 / 无需热替换的场景（如 proxy 回源在单测里不验代理热替换）。构造失败冒泡。
    pub fn new(request_timeout: std::time::Duration) -> Result<Self, UpstreamError> {
        let network = NetworkState::new(
            crate::config::NetworkProxyConfig::default(),
            request_timeout,
        )
        .map_err(UpstreamError::Transport)?;
        Ok(Self {
            network: Arc::new(network),
        })
    }
}

impl Upstream for HttpUpstream {
    async fn fetch(&self, base_url: &str, rel_path: &str) -> Result<UpstreamBody, UpstreamError> {
        // 拼 URL：去 base 尾斜杠、去 rel 首斜杠，避免双斜杠或缺斜杠
        let url = format!(
            "{}/{}",
            base_url.trim_end_matches('/'),
            rel_path.trim_start_matches('/')
        );

        // 从热替换槽取当前 client（读锁极短、锁外发请求），运行时换代理后即用新 client
        let resp = self
            .network
            .client()
            .get(&url)
            .send()
            .await
            .map_err(|e| UpstreamError::Transport(e.to_string()))?;

        // 非 2xx 一律按上游错误处理，绝不把错误体当制品缓存
        if !resp.status().is_success() {
            return Err(UpstreamError::Status(resp.status().as_u16()));
        }

        // 把响应字节流适配为 AsyncRead：流式读取，不整体载入内存
        let stream = resp
            .bytes_stream()
            .map_err(|e| std::io::Error::other(e.to_string()));
        let reader = StreamReader::new(stream);
        Ok(Box::new(reader))
    }
}
