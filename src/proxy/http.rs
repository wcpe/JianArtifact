//! reqwest 上游实现（FR-12）：纯 rustls TLS，流式响应体，按上游基址 + 相对路径拉取。
//!
//! 守 SECURITY：仅 rustls 校验上游 HTTPS 证书，不引 native-tls / openssl；
//! 响应体以流式暴露为 `AsyncRead`，大文件不整体载入内存。

use futures_util::TryStreamExt;
use tokio_util::io::StreamReader;

use super::{Upstream, UpstreamBody, UpstreamError};

/// 基于 reqwest 的上游客户端。内部持有复用连接池的 `reqwest::Client`。
#[derive(Debug, Clone)]
pub struct HttpUpstream {
    /// 复用的 HTTP 客户端（连接池、超时已配置）。
    client: reqwest::Client,
}

impl HttpUpstream {
    /// 构造上游客户端，设定整体请求超时（避免慢速上游拖垮代理）。
    ///
    /// 超时来自配置，不硬编码；构造失败（如 TLS 后端初始化异常）冒泡给调用方。
    pub fn new(request_timeout: std::time::Duration) -> Result<Self, UpstreamError> {
        let client = reqwest::Client::builder()
            .timeout(request_timeout)
            .build()
            .map_err(|e| UpstreamError::Transport(e.to_string()))?;
        Ok(Self { client })
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

        let resp = self
            .client
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
