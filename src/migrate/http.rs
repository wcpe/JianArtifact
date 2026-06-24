//! reqwest 实现的 Nexus REST 客户端（FR-36）：纯 rustls TLS，按 base URL 拉取仓库列表。
//!
//! 守 SECURITY：仅 rustls 校验源系统 HTTPS 证书，不引 native-tls / openssl。
//! 凭据经 HTTP Basic Auth 注入请求头，绝不写入日志 / 错误信息。

use super::{MigrateError, NexusClient, NexusCredential, NEXUS_REPOSITORIES_PATH};

/// 基于 reqwest 的 Nexus REST 客户端。内部持有复用连接池的 `reqwest::Client`。
#[derive(Debug, Clone)]
pub struct HttpNexusClient {
    /// 复用的 HTTP 客户端（连接池、超时已配置）。
    client: reqwest::Client,
}

impl HttpNexusClient {
    /// 构造客户端，设定整体请求超时（避免慢速源系统拖垮请求线程）。
    ///
    /// 超时来自配置，不硬编码；构造失败（如 TLS 后端初始化异常）冒泡给调用方。
    pub fn new(request_timeout: std::time::Duration) -> Result<Self, MigrateError> {
        let client = reqwest::Client::builder()
            .timeout(request_timeout)
            .build()
            .map_err(|e| MigrateError::Transport(e.to_string()))?;
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
}
