//! reqwest 实现的 Nexus REST 客户端（FR-36）：纯 rustls TLS，按 base URL 拉取仓库列表。
//!
//! 守 SECURITY：仅 rustls 校验源系统 HTTPS 证书，不引 native-tls / openssl。
//! 凭据经 HTTP Basic Auth 注入请求头，绝不写入日志 / 错误信息。

use futures_util::TryStreamExt;
use tokio_util::io::StreamReader;

use super::{
    MigrateError, NexusClient, NexusCredential, NEXUS_COMPONENTS_PATH, NEXUS_REPOSITORIES_PATH,
};

/// 基于 reqwest 的 Nexus REST 客户端。内部持有复用连接池的 `reqwest::Client`。
#[derive(Debug, Clone)]
pub struct HttpNexusClient {
    /// 复用的 HTTP 客户端（连接池、超时已配置）。
    client: reqwest::Client,
}

impl HttpNexusClient {
    /// 构造客户端，设定整体请求超时（避免慢速源系统拖垮请求线程）。
    ///
    /// 不注入出站代理（等价空代理配置，保持既有行为）；超时来自配置，不硬编码。
    /// 需注入 `[network.proxy]` 出站代理时改用 [`HttpNexusClient::with_network`]。
    pub fn new(request_timeout: std::time::Duration) -> Result<Self, MigrateError> {
        Self::with_network(
            request_timeout,
            &crate::config::NetworkProxyConfig::default(),
        )
    }

    /// 按出站代理配置构造 Nexus 客户端（FR-84，ADR-0020）。
    ///
    /// 经统一出站客户端 helper 注入 `[network.proxy]` 代理与既有超时 / rustls / stream 特性；
    /// 构造失败冒泡给调用方，错误信息不含代理凭据。
    pub fn with_network(
        request_timeout: std::time::Duration,
        proxy: &crate::config::NetworkProxyConfig,
    ) -> Result<Self, MigrateError> {
        let client = crate::config::build_outbound_client(request_timeout, proxy)
            .map_err(MigrateError::Transport)?;
        Ok(Self { client })
    }
}

impl NexusClient for HttpNexusClient {
    async fn fetch_repositories(
        &self,
        base_url: &str,
        credential: Option<&NexusCredential>,
    ) -> Result<String, MigrateError> {
        // 拼 URL：去 base 尾斜杠，避免双斜杠或缺斜杠
        let url = format!(
            "{}/{}",
            base_url.trim_end_matches('/'),
            NEXUS_REPOSITORIES_PATH
        );

        let mut req = self.client.get(&url);
        // 带凭据时以 Basic Auth 注入；凭据不进日志
        if let Some(c) = credential {
            req = req.basic_auth(&c.username, Some(&c.password));
        }

        let resp = req
            .send()
            .await
            .map_err(|e| MigrateError::Transport(e.to_string()))?;

        // 非 2xx 一律按源系统错误处理（如 401 鉴权失败 / 5xx）
        if !resp.status().is_success() {
            return Err(MigrateError::Status(resp.status().as_u16()));
        }

        // 仓库列表为有限规模的 JSON 元数据，整体读为文本后交纯函数解析
        resp.text()
            .await
            .map_err(|e| MigrateError::Transport(e.to_string()))
    }

    async fn fetch_components(
        &self,
        base_url: &str,
        repository: &str,
        continuation_token: Option<&str>,
        credential: Option<&NexusCredential>,
    ) -> Result<String, MigrateError> {
        // 拼 `{base}/service/rest/v1/components?repository=X[&continuationToken=T]`
        let mut url = reqwest::Url::parse(&format!(
            "{}/{}",
            base_url.trim_end_matches('/'),
            NEXUS_COMPONENTS_PATH
        ))
        .map_err(|e| MigrateError::Invalid(e.to_string()))?;
        {
            let mut q = url.query_pairs_mut();
            q.append_pair("repository", repository);
            if let Some(t) = continuation_token {
                q.append_pair("continuationToken", t);
            }
        }

        let mut req = self.client.get(url);
        if let Some(c) = credential {
            req = req.basic_auth(&c.username, Some(&c.password));
        }
        let resp = req
            .send()
            .await
            .map_err(|e| MigrateError::Transport(e.to_string()))?;
        if !resp.status().is_success() {
            return Err(MigrateError::Status(resp.status().as_u16()));
        }
        resp.text()
            .await
            .map_err(|e| MigrateError::Transport(e.to_string()))
    }

    async fn download_asset(
        &self,
        download_url: &str,
        credential: Option<&NexusCredential>,
    ) -> Result<Box<dyn tokio::io::AsyncRead + Send + Unpin>, MigrateError> {
        let mut req = self.client.get(download_url);
        if let Some(c) = credential {
            req = req.basic_auth(&c.username, Some(&c.password));
        }
        let resp = req
            .send()
            .await
            .map_err(|e| MigrateError::Transport(e.to_string()))?;
        if !resp.status().is_success() {
            return Err(MigrateError::Status(resp.status().as_u16()));
        }
        // 响应字节流适配为 AsyncRead：流式下载，不整体载入内存
        let stream = resp
            .bytes_stream()
            .map_err(|e| std::io::Error::other(e.to_string()));
        Ok(Box::new(StreamReader::new(stream)))
    }
}
