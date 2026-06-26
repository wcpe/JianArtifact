//! Release 来源抽象与 GitHub 生产实现（FR-85，ADR-0021）。
//!
//! `ReleaseSource` 抽象「取最新稳定 Release 元数据 + 流式下载资产」，便于测试注入 fake 源不触网。
//! 生产实现 `GithubReleaseSource` 经统一出站客户端（FR-84 / ADR-0020，honor 代理）请求 GitHub API。

use serde::Serialize;
use tokio::io::AsyncRead;

use super::UpdateError;

/// Release 元数据（仅取自更新所需字段）。
#[derive(Debug, Clone, Serialize, PartialEq, Eq)]
pub struct Release {
    /// 版本标签（如 `v0.4.0`）。
    pub tag_name: String,
    /// Release 标题。
    pub name: String,
    /// 发布说明正文。
    pub body: String,
    /// 资产列表。
    pub assets: Vec<ReleaseAsset>,
}

impl Release {
    /// 最新版本号：`tag_name` 去前导 `v`。
    pub fn version(&self) -> String {
        self.tag_name
            .trim()
            .strip_prefix('v')
            .unwrap_or_else(|| self.tag_name.trim())
            .to_string()
    }

    /// 按名精确匹配资产。
    pub fn find_asset(&self, name: &str) -> Option<&ReleaseAsset> {
        self.assets.iter().find(|a| a.name == name)
    }
}

/// Release 单个资产。
#[derive(Debug, Clone, Serialize, PartialEq, Eq)]
pub struct ReleaseAsset {
    /// 资产文件名。
    pub name: String,
    /// 浏览器直链下载地址。
    pub download_url: String,
}

/// Release 来源抽象：取最新稳定 Release + 流式下载资产。
pub trait ReleaseSource {
    /// 取配置仓库的最新稳定 Release 元数据。
    fn fetch_latest_release(
        &self,
    ) -> impl std::future::Future<Output = Result<Release, UpdateError>> + Send;

    /// 流式下载资产（不整体载入内存），返回装箱异步读句柄。
    fn download_asset(
        &self,
        url: &str,
    ) -> impl std::future::Future<Output = Result<Box<dyn AsyncRead + Send + Unpin>, UpdateError>> + Send;
}

/// GitHub Release 生产来源（经 `build_outbound_client` 注入代理与 rustls / stream 特性）。
#[derive(Debug, Clone)]
pub struct GithubReleaseSource {
    /// 复用的出站 HTTP 客户端（已注入 `[network.proxy]` 与超时）。
    client: reqwest::Client,
    /// GitHub API 基址（默认 `https://api.github.com`，可配）。
    api_base_url: String,
    /// 仓库源（`owner/repo`）。
    repo: String,
    /// 可选访问 token（私有仓库；真源 env，绝不进日志 / 错误）。
    token: Option<String>,
}

impl GithubReleaseSource {
    /// 据出站代理配置构造 GitHub 来源（FR-84，ADR-0020）。
    ///
    /// 经统一出站客户端 helper 注入代理与既有 rustls / stream / 超时特性；token 仅注入请求头，
    /// 绝不进日志 / 错误。构造失败冒泡给调用方（错误信息不含代理凭据）。
    pub fn new(
        timeout: std::time::Duration,
        proxy: &crate::config::NetworkProxyConfig,
        api_base_url: String,
        repo: String,
        token: Option<String>,
    ) -> Result<Self, UpdateError> {
        let client =
            crate::config::build_outbound_client(timeout, proxy).map_err(UpdateError::Upstream)?;
        Ok(Self {
            client,
            api_base_url,
            repo,
            token,
        })
    }

    /// 给请求注入 GitHub API 必需的 `User-Agent` 与可选 `Authorization`。
    fn with_headers(&self, req: reqwest::RequestBuilder) -> reqwest::RequestBuilder {
        // GitHub API 要求带 User-Agent；缺失会被拒
        let mut req = req.header(reqwest::header::USER_AGENT, "JianArtifact-Updater");
        if let Some(token) = &self.token {
            req = req.header(reqwest::header::AUTHORIZATION, format!("Bearer {token}"));
        }
        req
    }
}

impl ReleaseSource for GithubReleaseSource {
    async fn fetch_latest_release(&self) -> Result<Release, UpdateError> {
        let url = format!(
            "{}/repos/{}/releases/latest",
            self.api_base_url.trim_end_matches('/'),
            self.repo
        );
        let req = self.client.get(&url);
        let resp = self
            .with_headers(req)
            .send()
            .await
            .map_err(|e| UpdateError::Upstream(e.to_string()))?;
        if !resp.status().is_success() {
            return Err(UpdateError::Upstream(format!(
                "GitHub 返回状态 {}",
                resp.status().as_u16()
            )));
        }
        let body = resp
            .text()
            .await
            .map_err(|e| UpdateError::Upstream(e.to_string()))?;
        parse_release(&body)
    }

    async fn download_asset(
        &self,
        url: &str,
    ) -> Result<Box<dyn AsyncRead + Send + Unpin>, UpdateError> {
        use futures_util::TryStreamExt;
        use tokio_util::io::StreamReader;

        let req = self.client.get(url);
        let resp = self
            .with_headers(req)
            .send()
            .await
            .map_err(|e| UpdateError::Upstream(e.to_string()))?;
        if !resp.status().is_success() {
            return Err(UpdateError::Upstream(format!(
                "资产下载返回状态 {}",
                resp.status().as_u16()
            )));
        }
        // 响应字节流适配为 AsyncRead：流式下载，不整体载入内存
        let stream = resp
            .bytes_stream()
            .map_err(|e| std::io::Error::other(e.to_string()));
        Ok(Box::new(StreamReader::new(stream)))
    }
}

/// 解析 GitHub `releases/latest` 响应 JSON 为 [`Release`]（纯函数，可测）。
///
/// 只取 `tag_name` / `name` / `body` / `assets[].name` / `assets[].browser_download_url`；
/// 其余字段忽略。缺 `tag_name` 即报 [`UpdateError::Parse`]。
pub(crate) fn parse_release(body: &str) -> Result<Release, UpdateError> {
    #[derive(serde::Deserialize)]
    struct RawRelease {
        tag_name: String,
        #[serde(default)]
        name: Option<String>,
        #[serde(default)]
        body: Option<String>,
        #[serde(default)]
        assets: Vec<RawAsset>,
    }
    #[derive(serde::Deserialize)]
    struct RawAsset {
        name: String,
        browser_download_url: String,
    }
    let raw: RawRelease =
        serde_json::from_str(body).map_err(|e| UpdateError::Parse(e.to_string()))?;
    Ok(Release {
        name: raw.name.unwrap_or_else(|| raw.tag_name.clone()),
        tag_name: raw.tag_name,
        body: raw.body.unwrap_or_default(),
        assets: raw
            .assets
            .into_iter()
            .map(|a| ReleaseAsset {
                name: a.name,
                download_url: a.browser_download_url,
            })
            .collect(),
    })
}
