//! reqwest 实现的 Nexus REST 客户端（FR-36）：纯 rustls TLS，按 base URL 拉取仓库列表。
//!
//! 守 SECURITY：仅 rustls 校验源系统 HTTPS 证书，不引 native-tls / openssl。
//! 凭据经 HTTP Basic Auth 注入请求头，绝不写入日志 / 错误信息。

use std::sync::Arc;

use futures_util::TryStreamExt;
use tokio_util::io::StreamReader;

use crate::config::NetworkState;

use super::{
    MigrateError, NexusClient, NexusCredential, NEXUS_COMPONENTS_PATH, NEXUS_REPOSITORIES_PATH,
};

/// 基于 reqwest 的 Nexus REST 客户端。
///
/// 不再持有启动期固化的 client，改持出站网络热替换槽 [`NetworkState`]（FR-88，ADR-0022）：
/// 每次出站经 `network.client()` 取当前 client（含运行时 PATCH 后的新代理）。
#[derive(Clone)]
pub struct HttpNexusClient {
    /// 出站网络热替换槽（含当前 client，随 PATCH 即时换代理）。
    network: Arc<NetworkState>,
}

impl HttpNexusClient {
    /// 持出站网络热替换槽构造 Nexus 客户端（FR-88，ADR-0022）。
    ///
    /// 出站时经 `network.client()` 取当前 client；代理 / 超时 / rustls / stream 特性由槽统一注入。
    pub fn with_network_state(network: Arc<NetworkState>) -> Self {
        Self { network }
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

        // 从热替换槽取当前 client（读锁极短、锁外发请求）
        let mut req = self.network.client().get(&url);
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

        let mut req = self.network.client().get(url);
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
        let mut req = self.network.client().get(download_url);
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
