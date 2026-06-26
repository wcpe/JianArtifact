//! 制品库迁移（ADR-0006）：**Nexus OSS 迁移入口**——在线 REST API + 离线 blob store 双入口。
//!
//! 本模块覆盖迁移的**发现 / 入口**与 **proxy 仓库搬运**：
//! - **在线 REST 入口**（FR-36）：源 Nexus 仍在线时，经其 REST API 枚举可迁移仓库列表与
//!   基本元数据（仓库名 / 格式 / 类型 / 上游地址），供用户预览（见 [`http`]）。
//! - **离线 blob store 入口**（FR-37）：源 Nexus 已下线、只剩其文件型 blob store 目录时，
//!   从该本地目录解析磁盘布局，按 repo 枚举可迁移的 blob 及基本元数据（见 [`offline`]）。
//! - **proxy 仓库配置 + 缓存制品搬运**（FR-38）：据在线枚举的 proxy 仓库配置在本系统建仓，
//!   并把离线 blob store 中该仓库的缓存制品本体经既有制品机理搬运入缓存（见 [`proxy`]）。
//! - **hosted 仓库配置 + 完整制品搬运**（FR-39）：据在线枚举的 hosted 仓库配置在本系统建
//!   hosted 仓库，并把离线 blob store 中该仓库的全部制品本体经既有制品机理搬运为正常 hosted
//!   制品（`cached = false`，按各格式覆盖 / 不可变策略处理重复），见 [`hosted`]。至此 Nexus
//!   迁移框架（FR-36/37/38/39）完整。
//!
//! 关键约束：
//! - 凭据真源在 env / 配置，DB 仅存引用（`auth_ref`），凭据绝不入库、不进日志。
//! - REST 交互经 [`NexusClient`] trait 抽象，生产实现 [`HttpNexusClient`] 复用 reqwest
//!   纯 rustls；测试可注入 mock 覆盖响应解析与错误 / 超时分支。
//! - 离线入口纯文件系统读取、不依赖外部服务；解析逻辑做成无副作用纯函数便于穷举测试。
//! - 依赖方向：本模块仅依赖 `config` 级以下，不反向依赖上层；api 层薄编排调用之。

mod hosted;
mod http;
mod offline;
mod online;
mod proxy;

pub use hosted::{migrate_hosted_repositories, HostedMigrationReport, HostedRepoMigrationOutcome};
pub use http::HttpNexusClient;
pub use offline::{
    enumerate_blob_entries, enumerate_blob_store, OfflineBlobEntry, OfflineBlobSummary,
    OfflineRepoSummary,
};
pub use online::{
    migrate_online_repositories, migrate_online_with_progress, JobControl, OnlineMigrationReport,
    OnlinePullPhase, OnlinePullProgress, OnlinePullSelection, OnlineRepoMigrationOutcome,
};
pub use proxy::{migrate_proxy_repositories, ProxyMigrationReport, RepoMigrationOutcome};

/// 把 Nexus 原样格式名映射为本系统已实现的格式名；无法映射（未实现格式）返回 None。
///
/// 仅映射本系统当前已实现的格式（与 `repo` 模块受支持格式集合一致）；其余 Nexus 格式
/// （如 `rubygems` / `conan` 等后续阶段格式）不映射、对应仓库整体跳过，不越界建仓。
/// proxy（FR-38）与 hosted（FR-39）搬运共用此映射，保证格式判定一致。
pub(crate) fn map_nexus_format(nexus_format: &str) -> Option<&'static str> {
    match nexus_format.to_ascii_lowercase().as_str() {
        // Nexus 的 Maven 格式标识为 maven2
        "maven2" | "maven" => Some("maven"),
        "npm" => Some("npm"),
        "docker" => Some("docker"),
        "nuget" => Some("nuget"),
        "pypi" => Some("pypi"),
        "go" => Some("go"),
        "cargo" => Some("cargo"),
        // Nexus 的 Raw 格式标识为 raw
        "raw" => Some("raw"),
        _ => None,
    }
}

/// 把 Nexus blob 逻辑名归一化为本仓库内制品路径：去首部斜杠（Nexus blob-name 多以 `/` 起头）。
///
/// 归一化后交由对应 [`crate::format::Format::parse_path`] 做合法性校验（拒目录穿越 / 空路径）。
/// proxy 与 hosted 搬运共用此归一化逻辑。
pub(crate) fn normalize_blob_path(blob_name: &str) -> &str {
    blob_name.trim_start_matches('/')
}

/// Nexus 仓库列表 REST 端点（相对其 base URL）。
const NEXUS_REPOSITORIES_PATH: &str = "service/rest/v1/repositories";

/// Nexus 制品（components）REST 端点（相对其 base URL，按 `?repository=X` 分页枚举）。
const NEXUS_COMPONENTS_PATH: &str = "service/rest/v1/components";

/// 解析凭据引用时的 env 前缀。`auth_ref` 为 `<NAME>`，
/// 真值取 `JIANARTIFACT_MIGRATE_<NAME>_USERNAME` / `JIANARTIFACT_MIGRATE_<NAME>_PASSWORD`，
/// 与 `docs/CONFIG.md` 的上游凭据约定同款（凭据不入库、不进日志）。
const ENV_PREFIX: &str = "JIANARTIFACT_MIGRATE_";

/// 迁移入口错误。
#[derive(Debug, thiserror::Error)]
pub enum MigrateError {
    /// 入参不合法（如 base URL 为空）。
    #[error("{0}")]
    Invalid(String),
    /// 凭据引用对应的 env 真值缺失。
    #[error("凭据引用未在环境变量中配置: {0}")]
    MissingCredential(String),
    /// 源系统返回非成功状态（如 401 鉴权失败、404、5xx）。
    #[error("源系统返回错误状态: {0}")]
    Status(u16),
    /// 源系统不可用 / 超时 / 传输失败。
    #[error("源系统请求失败: {0}")]
    Transport(String),
    /// 源系统响应体解析失败（非预期 JSON 结构）。
    #[error("源系统响应解析失败: {0}")]
    Parse(String),
}

/// 从源 Nexus 枚举出的单个仓库的基本元数据（迁移预览项）。
///
/// 仅承载迁移发现所需的基本信息；不含任何凭据。`upstream_url` 仅 proxy 仓库有值
/// （取自 Nexus 的 `attributes.proxy.remoteUrl`），hosted 仓库为 None。
#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize)]
pub struct NexusRepoSummary {
    /// 仓库名。
    pub name: String,
    /// 源系统中的格式标识（Nexus 原样值，如 `maven2` / `npm` / `docker`）。
    pub format: String,
    /// 仓库类型（`hosted` / `proxy` / `group`，Nexus 原样值）。
    pub r#type: String,
    /// proxy 仓库的上游地址（hosted / group 为 None）。
    pub upstream_url: Option<String>,
}

/// 连接源 Nexus 时使用的凭据（用户名 + 口令 / token）。真源在 env，绝不入库、不进日志。
#[derive(Clone)]
pub struct NexusCredential {
    /// 用户名。
    pub username: String,
    /// 口令或 token。
    pub password: String,
}

// 手写 Debug，避免凭据明文随 derive 泄漏进日志 / 错误。
impl std::fmt::Debug for NexusCredential {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("NexusCredential")
            .field("username", &self.username)
            .field("password", &"<已脱敏>")
            .finish()
    }
}

/// Nexus REST 交互抽象：据 base URL 与可选凭据枚举仓库列表。
///
/// 生产实现 [`HttpNexusClient`] 走 reqwest；测试可注入 mock 以覆盖解析与错误分支。
#[allow(async_fn_in_trait)]
pub trait NexusClient: Send + Sync {
    /// 拉取源 Nexus 的仓库列表 REST 响应原文（JSON 文本）。
    ///
    /// 成功返回响应体文本；非 2xx / 传输失败返回对应 [`MigrateError`]。
    async fn fetch_repositories(
        &self,
        base_url: &str,
        credential: Option<&NexusCredential>,
    ) -> Result<String, MigrateError>;

    /// 分页拉取某仓库的 components REST 响应原文（JSON 文本）。
    ///
    /// `continuation_token` 为上一页返回的续页令牌（首页传 `None`）；非 2xx / 传输失败
    /// 返回对应 [`MigrateError`]。供在线拉取迁移枚举某仓库的全部 asset。
    async fn fetch_components(
        &self,
        base_url: &str,
        repository: &str,
        continuation_token: Option<&str>,
        credential: Option<&NexusCredential>,
    ) -> Result<String, MigrateError>;

    /// 流式下载单个 asset，返回装箱的异步读句柄（不整体载入内存）。
    ///
    /// 供在线拉取迁移逐 asset 下载后喂给既有 `ingest_hosted`；非 2xx / 传输失败返回
    /// 对应 [`MigrateError`]。
    async fn download_asset(
        &self,
        download_url: &str,
        credential: Option<&NexusCredential>,
    ) -> Result<Box<dyn tokio::io::AsyncRead + Send + Unpin>, MigrateError>;
}

/// 据 `auth_ref` 从环境变量解析 Nexus 访问凭据。
///
/// `auth_ref` 为 `<NAME>`，真值取 `JIANARTIFACT_MIGRATE_<NAME>_USERNAME` /
/// `JIANARTIFACT_MIGRATE_<NAME>_PASSWORD`。仅当二者都存在时返回凭据；任一缺失即报
/// [`MigrateError::MissingCredential`]，避免半截凭据。凭据真值绝不写入日志 / 错误信息。
pub fn resolve_credential(auth_ref: &str) -> Result<NexusCredential, MigrateError> {
    let key = auth_ref.to_ascii_uppercase();
    let username = std::env::var(format!("{ENV_PREFIX}{key}_USERNAME"))
        .map_err(|_| MigrateError::MissingCredential(auth_ref.to_string()))?;
    let password = std::env::var(format!("{ENV_PREFIX}{key}_PASSWORD"))
        .map_err(|_| MigrateError::MissingCredential(auth_ref.to_string()))?;
    Ok(NexusCredential { username, password })
}

/// 解析 Nexus 仓库列表 REST 响应文本为仓库摘要列表（纯函数，便于穷举测试）。
///
/// 只取迁移发现所需字段（name / format / type / proxy.remoteUrl），忽略其余字段；
/// 顶层须为 JSON 数组，每项须含 name / format / type，否则报 [`MigrateError::Parse`]。
pub fn parse_repositories(body: &str) -> Result<Vec<NexusRepoSummary>, MigrateError> {
    // 仅声明需要的字段，Nexus 多出的字段（size 等）由 serde 忽略
    #[derive(serde::Deserialize)]
    struct RawRepo {
        name: String,
        format: String,
        r#type: String,
        #[serde(default)]
        attributes: RawAttributes,
    }
    #[derive(serde::Deserialize, Default)]
    struct RawAttributes {
        #[serde(default)]
        proxy: Option<RawProxy>,
    }
    #[derive(serde::Deserialize)]
    struct RawProxy {
        #[serde(rename = "remoteUrl")]
        remote_url: Option<String>,
    }

    let raw: Vec<RawRepo> =
        serde_json::from_str(body).map_err(|e| MigrateError::Parse(e.to_string()))?;

    Ok(raw
        .into_iter()
        .map(|r| NexusRepoSummary {
            name: r.name,
            format: r.format,
            r#type: r.r#type,
            // proxy 仓库的上游地址取自 attributes.proxy.remoteUrl；其余类型无此项
            upstream_url: r.attributes.proxy.and_then(|p| p.remote_url),
        })
        .collect())
}

/// 源 Nexus 某仓库的单个 asset（在线拉取的下载单元）。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct NexusAsset {
    /// 仓库内路径（即本系统落地路径），如 `com/foo/lib/1.0/lib-1.0.jar`。
    pub path: String,
    /// 该 asset 的 HTTP 下载地址（取自 Nexus 的 `downloadUrl`）。
    pub download_url: String,
    /// Nexus 报告的 sha256：用于流式落定后比对，保证文件字节一致；缺失则不比对。
    pub sha256: Option<String>,
}

/// components REST 单页解析结果。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ComponentsPage {
    /// 本页所有 component 展平后的 asset 列表。
    pub assets: Vec<NexusAsset>,
    /// 续页令牌；`None` 表示已是最后一页。
    pub continuation_token: Option<String>,
}

/// 解析 Nexus components REST 单页响应为 asset 列表与续页令牌（纯函数，便于穷举测试）。
///
/// 取 `items[].assets[]` 的 `path` / `downloadUrl` / `checksum.sha256`，与顶层
/// `continuationToken`；Nexus 多出的字段（id / format / blobCreated 等）由 serde 忽略。
/// 顶层须为含 `items` 数组的对象，否则报 [`MigrateError::Parse`]。
pub fn parse_components(body: &str) -> Result<ComponentsPage, MigrateError> {
    #[derive(serde::Deserialize)]
    struct RawPage {
        #[serde(default)]
        items: Vec<RawComponent>,
        #[serde(rename = "continuationToken")]
        continuation_token: Option<String>,
    }
    #[derive(serde::Deserialize)]
    struct RawComponent {
        #[serde(default)]
        assets: Vec<RawAsset>,
    }
    #[derive(serde::Deserialize)]
    struct RawAsset {
        path: String,
        #[serde(rename = "downloadUrl")]
        download_url: String,
        #[serde(default)]
        checksum: RawChecksum,
    }
    #[derive(serde::Deserialize, Default)]
    struct RawChecksum {
        #[serde(default)]
        sha256: Option<String>,
    }

    let raw: RawPage =
        serde_json::from_str(body).map_err(|e| MigrateError::Parse(e.to_string()))?;
    let assets = raw
        .items
        .into_iter()
        .flat_map(|c| c.assets)
        .map(|a| NexusAsset {
            path: a.path,
            download_url: a.download_url,
            sha256: a.checksum.sha256,
        })
        .collect();
    Ok(ComponentsPage {
        assets,
        continuation_token: raw.continuation_token,
    })
}

/// 连接在线 Nexus 并枚举可迁移仓库列表（迁移发现 / 入口步骤）。
///
/// `base_url` 为源 Nexus 基址；`auth_ref` 给定时从 env 解析凭据（匿名访问可不给）。
/// 仅做连接 + 枚举 + 解析，不搬运任何制品。凭据不入库、不进日志。
pub async fn discover_repositories<C: NexusClient>(
    client: &C,
    base_url: &str,
    auth_ref: Option<&str>,
) -> Result<Vec<NexusRepoSummary>, MigrateError> {
    let base_url = base_url.trim();
    if base_url.is_empty() {
        return Err(MigrateError::Invalid(
            "源系统 base URL 不能为空".to_string(),
        ));
    }

    let credential = match auth_ref {
        Some(r) if !r.is_empty() => Some(resolve_credential(r)?),
        _ => None,
    };

    // 凭据仅传给客户端用于鉴权，绝不进日志：此处只记基址与是否带凭据
    tracing::info!(
        源基址 = %base_url,
        带凭据 = credential.is_some(),
        "开始枚举源 Nexus 仓库列表"
    );

    let body = client
        .fetch_repositories(base_url, credential.as_ref())
        .await?;
    let repos = parse_repositories(&body)?;
    tracing::info!(仓库数 = repos.len(), "源 Nexus 仓库枚举完成");
    Ok(repos)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::Mutex;

    /// 在隔离的环境变量上下文中执行闭包，避免测试间互相污染（进程级全局状态需串行化）。
    fn with_env_vars<F: FnOnce()>(vars: &[(&str, &str)], f: F) {
        static ENV_LOCK: Mutex<()> = Mutex::new(());
        let _guard = ENV_LOCK.lock().unwrap_or_else(|e| e.into_inner());
        for (k, v) in vars {
            std::env::set_var(k, v);
        }
        f();
        for (k, _) in vars {
            std::env::remove_var(k);
        }
    }

    /// 计数 mock 客户端：记录被调用次数与收到的凭据，返回预置响应或错误。
    struct MockClient {
        response: Result<String, MigrateError>,
        /// 记录最近一次调用收到的凭据（用户名），供断言凭据被正确透传。
        seen_username: Mutex<Option<String>>,
    }

    impl MockClient {
        fn ok(body: &str) -> Self {
            Self {
                response: Ok(body.to_string()),
                seen_username: Mutex::new(None),
            }
        }
        fn err(e: MigrateError) -> Self {
            Self {
                response: Err(e),
                seen_username: Mutex::new(None),
            }
        }
    }

    impl NexusClient for MockClient {
        async fn fetch_repositories(
            &self,
            _base_url: &str,
            credential: Option<&NexusCredential>,
        ) -> Result<String, MigrateError> {
            *self.seen_username.lock().unwrap() = credential.map(|c| c.username.clone());
            match &self.response {
                Ok(body) => Ok(body.clone()),
                Err(MigrateError::Status(s)) => Err(MigrateError::Status(*s)),
                Err(MigrateError::Transport(m)) => Err(MigrateError::Transport(m.clone())),
                Err(MigrateError::Parse(m)) => Err(MigrateError::Parse(m.clone())),
                Err(MigrateError::Invalid(m)) => Err(MigrateError::Invalid(m.clone())),
                Err(MigrateError::MissingCredential(m)) => {
                    Err(MigrateError::MissingCredential(m.clone()))
                }
            }
        }

        // 本 mock 仅用于 discover_repositories 用例，不触达在线拉取路径。
        async fn fetch_components(
            &self,
            _base_url: &str,
            _repository: &str,
            _continuation_token: Option<&str>,
            _credential: Option<&NexusCredential>,
        ) -> Result<String, MigrateError> {
            unimplemented!("discover 用例不调用 fetch_components")
        }

        async fn download_asset(
            &self,
            _download_url: &str,
            _credential: Option<&NexusCredential>,
        ) -> Result<Box<dyn tokio::io::AsyncRead + Send + Unpin>, MigrateError> {
            unimplemented!("discover 用例不调用 download_asset")
        }
    }

    /// Nexus 文档示例响应：一个 proxy（带 remoteUrl）与一个 hosted（空 attributes）。
    const SAMPLE: &str = r#"[
        {
            "name": "nuget.org-proxy",
            "format": "nuget",
            "type": "proxy",
            "url": "http://localhost:8081/repository/nuget.org-proxy",
            "size": 495186797,
            "attributes": { "proxy": { "remoteUrl": "https://www.nuget.org/api/v2/" } }
        },
        {
            "name": "maven-releases",
            "format": "maven2",
            "type": "hosted",
            "url": "http://localhost:8081/repository/maven-releases",
            "size": 385809438,
            "attributes": {}
        }
    ]"#;

    #[test]
    fn 映射格式仅认已实现格式() {
        assert_eq!(map_nexus_format("maven2"), Some("maven"));
        assert_eq!(map_nexus_format("NPM"), Some("npm"));
        assert_eq!(map_nexus_format("raw"), Some("raw"));
        // 未实现格式不映射
        assert_eq!(map_nexus_format("rubygems"), None);
        assert_eq!(map_nexus_format("conan"), None);
    }

    #[test]
    fn 归一化_blob_路径去首斜杠() {
        assert_eq!(normalize_blob_path("/a/b.jar"), "a/b.jar");
        assert_eq!(normalize_blob_path("a/b.jar"), "a/b.jar");
    }

    #[test]
    fn 解析仓库列表取基本元数据并区分_proxy_上游() {
        let repos = parse_repositories(SAMPLE).unwrap();
        assert_eq!(repos.len(), 2);
        assert_eq!(
            repos[0],
            NexusRepoSummary {
                name: "nuget.org-proxy".to_string(),
                format: "nuget".to_string(),
                r#type: "proxy".to_string(),
                upstream_url: Some("https://www.nuget.org/api/v2/".to_string()),
            }
        );
        // hosted 仓库无上游地址
        assert_eq!(repos[1].name, "maven-releases");
        assert_eq!(repos[1].r#type, "hosted");
        assert_eq!(repos[1].upstream_url, None);
    }

    #[test]
    fn 解析空数组得空列表() {
        assert!(parse_repositories("[]").unwrap().is_empty());
    }

    #[test]
    fn 解析非数组与缺字段报解析错误() {
        // 顶层非数组
        assert!(matches!(
            parse_repositories(r#"{"name":"x"}"#),
            Err(MigrateError::Parse(_))
        ));
        // 缺 name 字段
        assert!(matches!(
            parse_repositories(r#"[{"format":"npm","type":"hosted"}]"#),
            Err(MigrateError::Parse(_))
        ));
        // 完全非法 JSON
        assert!(matches!(
            parse_repositories("not json"),
            Err(MigrateError::Parse(_))
        ));
    }

    #[test]
    fn 解析_components_页取_asset_与续页令牌() {
        let body = r#"{
            "items": [
                { "assets": [
                    { "path": "com/foo/lib/1.0/lib-1.0.jar", "downloadUrl": "https://nx/repository/r/com/foo/lib/1.0/lib-1.0.jar", "checksum": { "sha256": "abc", "sha1": "x" } },
                    { "path": "com/foo/lib/1.0/lib-1.0.jar.sha1", "downloadUrl": "https://nx/repository/r/com/foo/lib/1.0/lib-1.0.jar.sha1", "checksum": { "sha256": "def" } }
                ]},
                { "assets": [
                    { "path": "com/foo/lib/1.0/lib-1.0.pom", "downloadUrl": "https://nx/p/lib-1.0.pom", "checksum": {} }
                ]}
            ],
            "continuationToken": "tok123"
        }"#;
        let page = parse_components(body).unwrap();
        assert_eq!(page.continuation_token.as_deref(), Some("tok123"));
        // 跨 component 展平所有 asset
        assert_eq!(page.assets.len(), 3);
        assert_eq!(page.assets[0].path, "com/foo/lib/1.0/lib-1.0.jar");
        assert_eq!(
            page.assets[0].download_url,
            "https://nx/repository/r/com/foo/lib/1.0/lib-1.0.jar"
        );
        assert_eq!(page.assets[0].sha256.as_deref(), Some("abc"));
        // sidecar 作为独立 asset 一并枚举
        assert_eq!(page.assets[1].path, "com/foo/lib/1.0/lib-1.0.jar.sha1");
        // checksum 缺 sha256 时为 None（不比对）
        assert_eq!(page.assets[2].sha256, None);
    }

    #[test]
    fn 解析_components_末页续页令牌为_none() {
        let page =
            parse_components(r#"{ "items": [ { "assets": [] } ], "continuationToken": null }"#)
                .unwrap();
        assert!(page.assets.is_empty());
        assert_eq!(page.continuation_token, None);
        // 缺 continuationToken 字段也视为末页
        let page2 = parse_components(r#"{ "items": [] }"#).unwrap();
        assert!(page2.continuation_token.is_none());
    }

    #[test]
    fn 解析_components_非法_json_报解析错误() {
        assert!(matches!(
            parse_components("not json"),
            Err(MigrateError::Parse(_))
        ));
        // asset 缺 path 字段
        assert!(matches!(
            parse_components(r#"{"items":[{"assets":[{"downloadUrl":"u"}]}]}"#),
            Err(MigrateError::Parse(_))
        ));
    }

    #[test]
    fn 凭据引用从环境变量解析且二者齐备() {
        with_env_vars(
            &[
                ("JIANARTIFACT_MIGRATE_PROD_USERNAME", "admin"),
                ("JIANARTIFACT_MIGRATE_PROD_PASSWORD", "s3cr3t"),
            ],
            || {
                let c = resolve_credential("prod").unwrap();
                assert_eq!(c.username, "admin");
                assert_eq!(c.password, "s3cr3t");
            },
        );
    }

    #[test]
    fn 凭据任一缺失报缺失凭据错误() {
        with_env_vars(&[("JIANARTIFACT_MIGRATE_PROD_USERNAME", "admin")], || {
            // 仅有用户名、缺口令
            assert!(matches!(
                resolve_credential("prod"),
                Err(MigrateError::MissingCredential(_))
            ));
        });
        // 完全未配置
        with_env_vars(&[], || {
            assert!(matches!(
                resolve_credential("none"),
                Err(MigrateError::MissingCredential(_))
            ));
        });
    }

    #[test]
    fn 凭据_debug_脱敏不泄漏口令() {
        let c = NexusCredential {
            username: "admin".to_string(),
            password: "s3cr3t".to_string(),
        };
        let dbg = format!("{c:?}");
        assert!(dbg.contains("admin"));
        // 口令明文绝不出现在 Debug 输出中
        assert!(!dbg.contains("s3cr3t"));
        assert!(dbg.contains("已脱敏"));
    }

    #[tokio::test]
    async fn 枚举成功返回仓库摘要() {
        let client = MockClient::ok(SAMPLE);
        let repos = discover_repositories(&client, "https://nexus.example", None)
            .await
            .unwrap();
        assert_eq!(repos.len(), 2);
        // 未给 auth_ref 时应以匿名调用（mock 未收到凭据）
        assert_eq!(*client.seen_username.lock().unwrap(), None);
    }

    // 该用例需在同一线程内先设 env 再跑异步，故用同步 `#[test]` 自建运行时，
    // 避免在 `#[tokio::test]` 的运行时内再 block_on（会触发"运行时内启动运行时"panic）。
    #[test]
    fn 枚举携带凭据时透传给客户端() {
        with_env_vars(
            &[
                ("JIANARTIFACT_MIGRATE_PROD_USERNAME", "admin"),
                ("JIANARTIFACT_MIGRATE_PROD_PASSWORD", "s3cr3t"),
            ],
            || {
                let rt = tokio::runtime::Builder::new_current_thread()
                    .build()
                    .unwrap();
                rt.block_on(async {
                    let client = MockClient::ok(SAMPLE);
                    let repos =
                        discover_repositories(&client, "https://nexus.example", Some("prod"))
                            .await
                            .unwrap();
                    assert_eq!(repos.len(), 2);
                    // 凭据被解析并透传给客户端用于鉴权
                    assert_eq!(
                        *client.seen_username.lock().unwrap(),
                        Some("admin".to_string())
                    );
                });
            },
        );
    }

    #[tokio::test]
    async fn 空_base_url_被拒() {
        let client = MockClient::ok(SAMPLE);
        assert!(matches!(
            discover_repositories(&client, "   ", None).await,
            Err(MigrateError::Invalid(_))
        ));
    }

    // 同上：同步 `#[test]` 自建运行时，先清 env 再跑异步，避免运行时嵌套。
    #[test]
    fn 凭据引用缺失时枚举报错且不调用客户端() {
        with_env_vars(&[], || {
            let rt = tokio::runtime::Builder::new_current_thread()
                .build()
                .unwrap();
            rt.block_on(async {
                let client = MockClient::ok(SAMPLE);
                let err =
                    discover_repositories(&client, "https://nexus.example", Some("absent")).await;
                assert!(matches!(err, Err(MigrateError::MissingCredential(_))));
                // 凭据缺失应在调用源系统前短路，客户端未被触达
                assert_eq!(*client.seen_username.lock().unwrap(), None);
            });
        });
    }

    #[tokio::test]
    async fn 源系统返回错误状态向上冒泡() {
        let client = MockClient::err(MigrateError::Status(401));
        let err = discover_repositories(&client, "https://nexus.example", None).await;
        assert!(matches!(err, Err(MigrateError::Status(401))));
    }

    #[tokio::test]
    async fn 源系统传输失败向上冒泡() {
        let client = MockClient::err(MigrateError::Transport("超时".to_string()));
        let err = discover_repositories(&client, "https://nexus.example", None).await;
        assert!(matches!(err, Err(MigrateError::Transport(_))));
    }
}
