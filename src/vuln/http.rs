//! reqwest 镜像下载实现（FR-70，ADR-0012）：纯 rustls TLS，流式落盘。
//!
//! 据数据源基址拼 `{base}/{ecosystem}/all.zip` 下载公开漏洞数据集整体镜像到本地文件。
//! 流式写盘（不整体载入内存）；下载的是公开数据集整包，**绝不携带本机制品坐标**（守隐私红线）。

use std::path::Path;
use std::sync::Arc;

use futures_util::StreamExt;
use tokio::io::AsyncWriteExt;

use crate::config::NetworkState;

use super::{MirrorSource, VulnError};

/// 基于 reqwest 的镜像下载器。
///
/// 不再持有启动期固化的 client，改持出站网络热替换槽 [`NetworkState`]（FR-88，ADR-0022）：
/// 后台周期刷新每次下载经 `network.client()` 取**当前** client，故运行时 PATCH 改代理后下次刷新即用新代理。
#[derive(Clone)]
pub struct HttpMirrorSource {
    /// 出站网络热替换槽（含当前 client，随 PATCH 即时换代理）。
    network: Arc<NetworkState>,
    /// 数据源基址（按生态在其下取 `{ecosystem}/all.zip`）。
    base_url: String,
}

impl HttpMirrorSource {
    /// 持出站网络热替换槽构造镜像下载器（FR-88，ADR-0022）。
    ///
    /// 下载时经 `network.client()` 取当前 client；代理 / 超时 / rustls / stream 特性由槽统一注入。
    pub fn with_network_state(base_url: String, network: Arc<NetworkState>) -> Self {
        Self { network, base_url }
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

        // 从热替换槽取当前 client（读锁极短、锁外发请求），运行时换代理后下次刷新即用新 client
        let resp = self
            .network
            .client()
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
