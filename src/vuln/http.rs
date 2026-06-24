//! reqwest 镜像下载实现（FR-70，ADR-0012）：纯 rustls TLS，流式落盘。
//!
//! 据数据源基址拼 `{base}/{ecosystem}/all.zip` 下载公开漏洞数据集整体镜像到本地文件。
//! 流式写盘（不整体载入内存）；下载的是公开数据集整包，**绝不携带本机制品坐标**（守隐私红线）。

use std::path::Path;

use futures_util::StreamExt;
use tokio::io::AsyncWriteExt;

use super::{MirrorSource, VulnError};

/// 基于 reqwest 的镜像下载器。内部持有复用连接池的 `reqwest::Client`。
#[derive(Debug, Clone)]
pub struct HttpMirrorSource {
    /// 复用的 HTTP 客户端（超时已配置）。
    client: reqwest::Client,
    /// 数据源基址（按生态在其下取 `{ecosystem}/all.zip`）。
    base_url: String,
}

impl HttpMirrorSource {
    /// 构造下载器，设定单次下载整体超时（按生态 all.zip 可能较大）。
    pub fn new(base_url: String, download_timeout: std::time::Duration) -> Result<Self, VulnError> {
        let client = reqwest::Client::builder()
            .timeout(download_timeout)
            .build()
            .map_err(|e| VulnError::Download(e.to_string()))?;
        Ok(Self { client, base_url })
    }
}

impl MirrorSource for HttpMirrorSource {
    async fn download(&self, ecosystem: &str, dest: &Path) -> Result<(), VulnError> {
        // 拼下载 URL：公开数据集按生态提供整包，URL 只含生态名（公开坐标），不含本机制品坐标
        let url = format!(
            "{}/{}/all.zip",
            self.base_url.trim_end_matches('/'),
            ecosystem
        );

        let resp = self
            .client
            .get(&url)
            .send()
            .await
            .map_err(|e| VulnError::Download(e.to_string()))?;

        if !resp.status().is_success() {
            return Err(VulnError::Download(format!(
                "上游返回状态 {}",
                resp.status().as_u16()
            )));
        }

        // 流式写盘：边收边写，大镜像不整体载入内存
        let mut file = tokio::fs::File::create(dest).await?;
        let mut stream = resp.bytes_stream();
        while let Some(chunk) = stream.next().await {
            let chunk = chunk.map_err(|e| VulnError::Download(e.to_string()))?;
            file.write_all(&chunk).await?;
        }
        file.flush().await?;
        Ok(())
    }
}
