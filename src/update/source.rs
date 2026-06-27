//! Release 来源抽象与 GitHub 生产实现（FR-85，ADR-0021；FR-89 加更新通道）。
//!
//! `ReleaseSource` 抽象「按通道取 Release 元数据 + 流式下载资产」，便于测试注入 fake 源不触网。
//! 生产实现 `GithubReleaseSource` 经统一出站客户端（FR-84 / ADR-0020，honor 代理）请求 GitHub API：
//! `stable` 通道走 `/releases/latest`（只认稳定版）、`prerelease` 通道走 `/releases`（取最新含预发布一条）。

use serde::Serialize;
use tokio::io::AsyncRead;

use crate::config::UpdateChannel;

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
    /// 最新版本号：优先 `tag_name`（去前导 `v`），但 prerelease 滚动发布的 `tag_name` 是固定标签
    /// `dev`（非版本串，见 FR-86 release.yml），此时回退到 release 标题 `name`（内嵌完整 dev 版本串，
    /// 如 `0.4.0-dev.5.<sha>`）。正式版 `tag_name=vX.Y.Z` 仍走 tag。
    pub fn version(&self) -> String {
        let tag = self.tag_name.trim();
        let tag_ver = tag.strip_prefix('v').unwrap_or(tag);
        // tag 不是合法版本串（如滚动标签 `dev`）时，回退到 name；name 也无效则原样返回 tag 值
        if super::parse_version(tag_ver).is_ok() {
            return tag_ver.to_string();
        }
        let name = self.name.trim();
        let name_ver = name.strip_prefix('v').unwrap_or(name);
        if super::parse_version(name_ver).is_ok() {
            return name_ver.to_string();
        }
        tag_ver.to_string()
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

/// Release 来源抽象：按通道取 Release + 流式下载资产。
pub trait ReleaseSource {
    /// 按通道取配置仓库的 Release 元数据。
    ///
    /// `Stable` 取最新稳定版（`/releases/latest`）；`Prerelease` 取最新一条非 draft 的 release
    /// （含预发布，`/releases` 列表首条）。
    fn fetch_latest_release(
        &self,
        channel: UpdateChannel,
    ) -> impl std::future::Future<Output = Result<Release, UpdateError>> + Send;

    /// 流式下载资产（不整体载入内存），返回装箱异步读句柄。
    fn download_asset(
        &self,
        url: &str,
    ) -> impl std::future::Future<Output = Result<Box<dyn AsyncRead + Send + Unpin>, UpdateError>> + Send;
}

/// GitHub Release 生产来源（出站经热替换槽 [`NetworkState`] 取当前 client，FR-88 / ADR-0022）。
#[derive(Clone)]
pub struct GithubReleaseSource {
    /// 出站网络热替换槽（含当前 client，随 PATCH 即时换代理）。
    network: std::sync::Arc<crate::config::NetworkState>,
    /// GitHub API 基址（默认 `https://api.github.com`，可配）。
    api_base_url: String,
    /// 仓库源（`owner/repo`）。
    repo: String,
    /// 可选访问 token（私有仓库；真源 env，绝不进日志 / 错误）。
    token: Option<String>,
    /// 资产下载整体超时（按 `[update] download_timeout_secs`，可能远大于出站 client 默认超时）。
    ///
    /// 出站 client 取自共享热替换槽（其超时为上游回源口径）；自更新下载大资产需更长超时，故按请求级
    /// `RequestBuilder::timeout` 注入本值覆盖 client 超时，保持 ADR-0021 的下载超时语义。
    download_timeout: std::time::Duration,
}

impl GithubReleaseSource {
    /// 持出站网络热替换槽构造 GitHub 来源（FR-88，ADR-0022）。
    ///
    /// 出站时经 `network.client()` 取当前 client（代理 / rustls / stream 由槽统一注入）；下载超时按
    /// `download_timeout` 请求级注入。token 仅注入请求头，绝不进日志 / 错误。
    pub fn with_network_state(
        network: std::sync::Arc<crate::config::NetworkState>,
        api_base_url: String,
        repo: String,
        token: Option<String>,
        download_timeout: std::time::Duration,
    ) -> Self {
        Self {
            network,
            api_base_url,
            repo,
            token,
            download_timeout,
        }
    }

    /// 给请求注入 GitHub API 必需的 `User-Agent`、可选 `Authorization` 与请求级下载超时。
    fn with_headers(&self, req: reqwest::RequestBuilder) -> reqwest::RequestBuilder {
        // GitHub API 要求带 User-Agent；缺失会被拒。请求级超时覆盖 client 超时，保持下载超时语义
        let mut req = req
            .timeout(self.download_timeout)
            .header(reqwest::header::USER_AGENT, "JianArtifact-Updater");
        if let Some(token) = &self.token {
            req = req.header(reqwest::header::AUTHORIZATION, format!("Bearer {token}"));
        }
        req
    }
}

impl ReleaseSource for GithubReleaseSource {
    async fn fetch_latest_release(&self, channel: UpdateChannel) -> Result<Release, UpdateError> {
        // 通道决定端点：stable 取单条最新稳定版，prerelease 取列表后选最新非 draft 一条
        let url = match channel {
            UpdateChannel::Stable => format!(
                "{}/repos/{}/releases/latest",
                self.api_base_url.trim_end_matches('/'),
                self.repo
            ),
            UpdateChannel::Prerelease => format!(
                "{}/repos/{}/releases",
                self.api_base_url.trim_end_matches('/'),
                self.repo
            ),
        };
        // 从热替换槽取当前 client（读锁极短、锁外发请求）
        let req = self.network.client().get(&url);
        let resp = self
            .with_headers(req)
            .send()
            .await
            .map_err(|e| UpdateError::Upstream(e.to_string()))?;
        // stable 通道下 `/releases/latest` 对「仓库尚无正式发布（只有预发布 / 草稿）」返 404——
        // 这不是上游故障，给清晰提示而非笼统「拉取失败」；提示改用 prerelease 通道或发正式版。
        if resp.status() == reqwest::StatusCode::NOT_FOUND && channel == UpdateChannel::Stable {
            return Err(UpdateError::NoUpdate(
                "仓库尚无正式发布版本，暂无可用更新（如需拉取开发版，请将更新通道切换为 prerelease）"
                    .to_string(),
            ));
        }
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
        // stable 解析单对象；prerelease 解析数组、取最新非 draft 一条
        match channel {
            UpdateChannel::Stable => parse_release(&body),
            UpdateChannel::Prerelease => parse_release_list(&body),
        }
    }

    async fn download_asset(
        &self,
        url: &str,
    ) -> Result<Box<dyn AsyncRead + Send + Unpin>, UpdateError> {
        use futures_util::TryStreamExt;
        use tokio_util::io::StreamReader;

        let req = self.network.client().get(url);
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

/// GitHub Release 原始 JSON 形态（只取所需字段，其余忽略）。
#[derive(serde::Deserialize)]
struct RawRelease {
    tag_name: String,
    #[serde(default)]
    name: Option<String>,
    #[serde(default)]
    body: Option<String>,
    /// 是否为草稿（FR-89：prerelease 通道跳过草稿，草稿无可下载资产）。
    #[serde(default)]
    draft: bool,
    #[serde(default)]
    assets: Vec<RawAsset>,
}

/// GitHub Release 资产原始 JSON 形态。
#[derive(serde::Deserialize)]
struct RawAsset {
    name: String,
    browser_download_url: String,
}

impl From<RawRelease> for Release {
    fn from(raw: RawRelease) -> Self {
        Release {
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
        }
    }
}

/// 解析 GitHub `releases/latest` 响应 JSON 为 [`Release`]（纯函数，可测）。
///
/// 只取 `tag_name` / `name` / `body` / `assets[].name` / `assets[].browser_download_url`；
/// 其余字段忽略。缺 `tag_name` 即报 [`UpdateError::Parse`]。
pub(crate) fn parse_release(body: &str) -> Result<Release, UpdateError> {
    let raw: RawRelease =
        serde_json::from_str(body).map_err(|e| UpdateError::Parse(e.to_string()))?;
    Ok(raw.into())
}

/// 解析 GitHub `releases` 列表响应 JSON，取最新一条非 draft 的 release（含预发布；纯函数，可测）。
///
/// GitHub 列表按发布时间倒序，故首个非 draft 即「最新」。draft 无可下载资产、跳过；列表为空或
/// 全为 draft 时报 [`UpdateError::Upstream`]（无可用 release）。`prerelease` 字段当前不参与筛选——
/// prerelease 通道意在「含预发布的最新一条」，稳定版与预发布版皆可被选中，由后续版本比较决定是否升级。
pub(crate) fn parse_release_list(body: &str) -> Result<Release, UpdateError> {
    let raw_list: Vec<RawRelease> =
        serde_json::from_str(body).map_err(|e| UpdateError::Parse(e.to_string()))?;
    raw_list
        .into_iter()
        .find(|r| !r.draft)
        .map(Release::from)
        .ok_or_else(|| {
            UpdateError::Upstream("仓库无可用 release（列表为空或全为草稿）".to_string())
        })
}
