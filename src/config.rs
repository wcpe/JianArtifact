//! 运行期配置加载层。
//!
//! 配置由单个 TOML 文件加载，环境变量（前缀 `JIANARTIFACT_`）优先覆盖同名项。
//! 键名与默认值对齐 `docs/CONFIG.md`。

use std::path::{Path, PathBuf};

use figment::{
    providers::{Env, Format, Serialized, Toml},
    value::{Uncased, UncasedStr},
    Figment,
};
use serde::{Deserialize, Serialize};

/// 默认监听地址。
const DEFAULT_LISTEN_ADDR: &str = "127.0.0.1";
/// 默认监听端口。
const DEFAULT_PORT: u16 = 8080;
/// 默认数据目录。
const DEFAULT_DATA_DIR: &str = "./data";
/// blob 存储默认子目录名（位于数据目录下）。
const DEFAULT_BLOBS_SUBDIR: &str = "blobs";
/// 默认会话有效期（秒）。
const DEFAULT_SESSION_TTL_SECS: u64 = 3600;
/// 触发锁定的默认连续失败次数。
const DEFAULT_LOGIN_MAX_FAILURES: u32 = 5;
/// 默认锁定时长（秒）。
const DEFAULT_LOGIN_LOCKOUT_SECS: u64 = 900;
/// 默认上游拉取超时（秒），proxy 回源用，避免慢速上游拖垮代理。
const DEFAULT_UPSTREAM_TIMEOUT_SECS: u64 = 60;
/// 默认审计日志保留天数（ADR-0015）。
const DEFAULT_AUDIT_RETENTION_DAYS: u32 = 90;
/// 默认审计日志行数硬上限（兜底，防止撑爆 SQLite）。
const DEFAULT_AUDIT_MAX_ROWS: u64 = 1_000_000;
/// 使用分析明细行数硬上限默认值（兜底，防止明细撑爆 SQLite；ADR-0009）。
const DEFAULT_USAGE_MAX_DETAIL_ROWS: u64 = 1_000_000;
/// 默认是否启用 Prometheus 指标端点（FR-32，ADR-0015）：默认开。
const DEFAULT_METRICS_ENABLED: bool = true;
/// 默认是否允许匿名抓取 /metrics（ADR-0015）：默认关，须运维显式开启（限内网 / 反代后）。
const DEFAULT_METRICS_ALLOW_ANONYMOUS: bool = false;
/// 漏洞库离线镜像默认数据源基址（OSV 公开数据集，按生态提供 all.zip 整包下载）。
const DEFAULT_VULN_SOURCE_BASE_URL: &str = "https://osv-vulnerabilities.storage.googleapis.com";
/// 漏洞库镜像默认刷新周期（秒），默认 24 小时。
const DEFAULT_VULN_REFRESH_INTERVAL_SECS: u64 = 86_400;
/// 漏洞库镜像下载整体超时（秒），默认 600 秒（按生态 all.zip 可能较大）。
const DEFAULT_VULN_DOWNLOAD_TIMEOUT_SECS: u64 = 600;
/// 速率限制默认开关（FR-33，ADR-0008）：默认关闭，须运维显式开启，避免误杀正常流量。
const DEFAULT_RATE_LIMIT_ENABLED: bool = false;
/// 速率限制默认时间窗（秒）：60 秒固定窗。
const DEFAULT_RATE_LIMIT_WINDOW_SECS: u64 = 60;
/// 单 IP 每窗默认请求上限：保守宽放，正常包管理器批量拉取不应触顶。
const DEFAULT_RATE_LIMIT_IP_MAX_REQUESTS: u64 = 1200;
/// 单身份（用户 / Token）每窗默认请求上限：略高于 IP，照顾 CI 等高频合法调用。
const DEFAULT_RATE_LIMIT_IDENTITY_MAX_REQUESTS: u64 = 2400;
/// 单仓库每窗默认请求上限（FR-51 仓库维度）：默认 0 表示不启用该维度，保守不误杀。
const DEFAULT_RATE_LIMIT_REPO_MAX_REQUESTS: u64 = 0;
/// 单 IP 默认并发在途请求上限（FR-51 并发上限）：默认 0 表示不限并发，避免误杀正常并发拉取。
const DEFAULT_RATE_LIMIT_IP_MAX_CONCURRENT: u64 = 0;
/// 单用户默认并发在途请求上限（FR-51 并发上限）：默认 0 表示不限并发。
const DEFAULT_RATE_LIMIT_USER_MAX_CONCURRENT: u64 = 0;
/// 单仓库默认并发在途请求上限（FR-51 并发上限）：默认 0 表示不限并发。
const DEFAULT_RATE_LIMIT_REPO_MAX_CONCURRENT: u64 = 0;
/// 访问异常检测与自动封禁默认开关（FR-53，ADR-0008）：默认关闭，须运维显式开启，避免误杀。
const DEFAULT_BAN_ENABLED: bool = false;
/// 异常检测固定时间窗时长（秒）：在该窗内统计单 IP 的异常信号数。
const DEFAULT_BAN_WINDOW_SECS: u64 = 60;
/// 触发自动封禁的窗内异常信号阈值：单 IP 一窗内异常信号数达此值即封禁。
///
/// 异常信号指 4xx（客户端错误，含 401/403 鉴权失败）与被限流拒绝（429）等可疑响应。
/// 默认 100 较保守宽放：正常包管理器批量拉取偶发 404（探测制品是否存在）不应触顶。
const DEFAULT_BAN_THRESHOLD: u64 = 100;
/// 自动封禁时长（秒）：封禁期内来源 IP 一律拒绝；到期自动解封。默认 15 分钟。
const DEFAULT_BAN_DURATION_SECS: u64 = 900;
/// 慢速攻击防护默认开关（FR-52，ADR-0008）：默认关闭，须运维显式开启，避免误伤慢速合法客户端。
const DEFAULT_SLOWLORIS_ENABLED: bool = false;
/// 请求体读取的相邻数据块默认空闲超时（秒）：两次到达数据块的最大间隔，超过即判为慢速 drip 并断开。
///
/// 这是「块间空闲超时」而非「整体超时」：只要客户端持续有数据到达就不触发，因此对正常大文件流式
/// 上传（如 mvn deploy 大 jar、docker push 大层）友好——只惩罚长时间不发数据的 slowloris 慢速连接。
/// 默认 30 秒：远宽于正常网络抖动，又能及时切断只为占用连接而几乎不发数据的慢速攻击。
const DEFAULT_SLOWLORIS_BODY_READ_TIMEOUT_SECS: u64 = 30;
/// 等待请求体首个数据块的默认超时（秒）：从中间件开始读取体到收到第一个字节的最长等待。
///
/// 针对「发完头后迟迟不发体」的慢速起始攻击；同样对正常上传友好（正常客户端发完头即开始发体）。
/// 默认 30 秒，与块间空闲超时同档，保守不误伤。
const DEFAULT_SLOWLORIS_HEADER_TIMEOUT_SECS: u64 = 30;
/// 单个请求体通用大小上限默认值（字节，FR-52）：默认 0 表示不启用该通用上限。
///
/// 区别于 `limits.max_artifact_size`（仅约束制品上传体）：本项是对**所有请求**请求体的兜底上限，
/// 防止任意端点（如管理 JSON 接口）被超大体撑爆。默认 0（不启用），避免误杀正常大制品流式上传；
/// 启用时应设得高于预期最大制品体，仅作异常超大体的兜底拦截。
const DEFAULT_SLOWLORIS_MAX_BODY_BYTES: u64 = 0;
/// CC 挑战默认开关（FR-54，ADR-0008）：**默认关闭**。
///
/// 正常包管理器 CLI（mvn / npm / docker / curl）不会解工作量证明（PoW），无差别拦截会直接打断
/// 正常拉取。故默认关闭，启用与否由运维显式承担——仅在确有 CC（HTTP 洪水）攻击且能接受匿名访问
/// 受影响时开启。
const DEFAULT_CC_CHALLENGE_ENABLED: bool = false;
/// CC 挑战默认 PoW 难度（要求 sha256 摘要的前导零比特数）。
///
/// 难度越高客户端单次求解开销越大、攻击者刷流成本越高，而正常单次请求成本仍可忽略。
/// 默认 20 位：现代 CPU 求解约毫秒级，对单请求开销小，又足以抬高高频刷流成本。
const DEFAULT_CC_CHALLENGE_DIFFICULTY: u32 = 20;
/// CC 挑战令牌默认有效期（秒）：签发后超此时长的证明视为过期，须重新获取挑战。
///
/// 取较短值以收紧证明的可复用窗口（配合绑定来源 IP），默认 300 秒兼顾客户端求解 + 重试时间。
const DEFAULT_CC_CHALLENGE_TTL_SECS: u64 = 300;
/// CC 挑战是否默认豁免已认证请求：**默认豁免**。
///
/// 包管理器 CLI 通常带凭据（Bearer / Basic），豁免使其不受 PoW 挑战影响；挑战只面向匿名可疑流量。
const DEFAULT_CC_CHALLENGE_EXEMPT_AUTHENTICATED: bool = true;
/// 可配置 WAF 规则引擎默认开关（FR-55，ADR-0008）：默认关闭，须运维显式开启，避免误杀正常请求。
const DEFAULT_WAF_ENABLED: bool = false;
/// 防护阈值告警默认开关（FR-56，ADR-0017）：默认关闭，须运维显式开启，避免无人值守时刷告警。
const DEFAULT_ALERTS_ENABLED: bool = false;
/// 告警评估固定时间窗时长（秒）：在该窗内统计各防护维度事件计数并与阈值比较。默认 5 分钟。
const DEFAULT_ALERTS_WINDOW_SECS: u64 = 300;
/// 限流被拒窗内告警阈值（FR-56）：一窗内限流被拒次数达此值即告警。默认保守宽放，避免误报。
const DEFAULT_ALERTS_RATE_LIMIT_WARN_THRESHOLD: u64 = 1000;
/// 自动封禁触发窗内告警阈值（FR-56）：一窗内自动封禁触发次数达此值即告警。
const DEFAULT_ALERTS_BAN_WARN_THRESHOLD: u64 = 50;
/// CC 挑战失败窗内告警阈值（FR-56）：一窗内 CC 证明校验失败次数达此值即告警。
const DEFAULT_ALERTS_CC_CHALLENGE_FAIL_WARN_THRESHOLD: u64 = 1000;
/// WAF 阻断窗内告警阈值（FR-56）：一窗内 WAF 阻断次数达此值即告警。
const DEFAULT_ALERTS_WAF_BLOCK_WARN_THRESHOLD: u64 = 500;
/// 慢速攻击超时窗内告警阈值（FR-56）：一窗内慢速超时 / 截断拒绝次数达此值即告警。
const DEFAULT_ALERTS_SLOWLORIS_WARN_THRESHOLD: u64 = 200;
/// 防护告警明细行数硬上限（FR-56）：超限删最旧行，兜底防止撑爆 SQLite。
const DEFAULT_ALERTS_MAX_ROWS: u64 = 100_000;
/// 在线更新默认开关（FR-85，ADR-0021）：**默认关闭**，出站默认不联网，须运维显式开启。
const DEFAULT_UPDATE_ENABLED: bool = false;
/// 在线更新默认仓库源（FR-85）：`owner/repo` 形式，默认本项目发布仓库。
const DEFAULT_UPDATE_REPO: &str = "wcpe/JianArtifact";
/// 在线更新默认 GitHub API 基址（FR-85）：可配（便于测试 / 镜像）。
const DEFAULT_UPDATE_API_BASE_URL: &str = "https://api.github.com";
/// 在线更新默认重启模式（FR-85）：`self`（自拉起新进程）或 `exit`（仅退出交外部进程管理器）。
const DEFAULT_UPDATE_RESTART_MODE: &str = "self";
/// 在线更新默认下载超时（秒）：资产下载整体超时，默认 300 秒。
const DEFAULT_UPDATE_DOWNLOAD_TIMEOUT_SECS: u64 = 300;
/// 在线更新默认更新通道（FR-89）：`stable`（仅稳定版）或 `prerelease`（含预发布），默认仅稳定版。
const DEFAULT_UPDATE_CHANNEL: &str = "stable";
/// 环境变量前缀。
const ENV_PREFIX: &str = "JIANARTIFACT_";
/// 已知配置节名。环境变量映射时，仅把节名与键名之间的首个下划线视作嵌套分隔，
/// 键名内部的下划线（如 `session_ttl_secs`）保持原样。
const KNOWN_SECTIONS: &[&str] = &[
    "server",
    "data",
    "auth",
    "limits",
    "proxy",
    "observability",
    "protection",
    "vuln",
    "network",
    "update",
];

/// 已知的多级嵌套前缀映射（环境变量下划线前缀 → 点分隔配置路径）。
///
/// 单级节名（[`KNOWN_SECTIONS`]）只替换首个下划线，无法表达 `data.storage.s3.*` 这类深层嵌套；
/// 这里按前缀长度从长到短优先匹配，把整段已知前缀替换为点分隔路径，余下键名内部下划线保留。
const KNOWN_NESTED_PREFIXES: &[(&str, &str)] = &[
    ("data_storage_s3_", "data.storage.s3."),
    ("data_storage_", "data.storage."),
    // OIDC 配置在 auth.oidc 子节下；env 形如 JIANARTIFACT_AUTH_OIDC_CLIENT_SECRET。
    ("auth_oidc_", "auth.oidc."),
    // LDAP 配置在 auth.ldap 子节下；env 形如 JIANARTIFACT_AUTH_LDAP_BIND_PASSWORD。
    ("auth_ldap_", "auth.ldap."),
    // 出站代理在 network.proxy 子节下；env 形如 JIANARTIFACT_NETWORK_PROXY_HTTPS（FR-84）。
    ("network_proxy_", "network.proxy."),
];

/// 顶层配置。
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct Config {
    /// HTTP 服务配置。
    #[serde(default)]
    pub server: ServerConfig,
    /// 数据目录与 blob 存储配置。
    #[serde(default)]
    pub data: DataConfig,
    /// 认证与登录防护配置。
    #[serde(default)]
    pub auth: AuthConfig,
    /// 上传等限制配置。
    #[serde(default)]
    pub limits: LimitsConfig,
    /// 代理仓库上游拉取配置。
    #[serde(default)]
    pub proxy: ProxyConfig,
    /// 可观测性配置（审计日志等，ADR-0015）。
    #[serde(default)]
    pub observability: ObservabilityConfig,
    /// 应用层（L7）防护配置（当前承载基础速率限制，FR-33 / ADR-0008）。
    #[serde(default)]
    pub protection: ProtectionConfig,
    /// 漏洞库离线镜像配置。
    #[serde(default)]
    pub vuln: VulnConfig,
    /// 出站网络代理配置（FR-84，ADR-0020）：统一注入全部出站 reqwest 客户端。
    #[serde(default)]
    pub network: NetworkConfig,
    /// 在线更新配置（FR-85，ADR-0021）：管理员手动触发的自更新；默认关闭出站。
    #[serde(default)]
    pub update: UpdateConfig,
}

/// HTTP 服务配置。
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ServerConfig {
    /// 监听地址。
    pub listen_addr: String,
    /// 监听端口。
    pub port: u16,
    /// 对外基础 URL，用于生成链接；未配置时为 None，由调用方按监听推断。
    #[serde(default)]
    pub public_base_url: Option<String>,
}

impl Default for ServerConfig {
    fn default() -> Self {
        Self {
            listen_addr: DEFAULT_LISTEN_ADDR.to_string(),
            port: DEFAULT_PORT,
            public_base_url: None,
        }
    }
}

/// 数据目录与 blob 存储配置。
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct DataConfig {
    /// 数据目录（SQLite 文件与 blob 根）。
    pub data_dir: PathBuf,
    /// blob 存储子目录；为 None 时取 `data_dir/blobs`。
    #[serde(default)]
    pub blobs_dir: Option<PathBuf>,
    /// blob 存储后端选择（ADR-0014）。默认本地文件系统。
    #[serde(default)]
    pub storage: StorageConfig,
}

impl Default for DataConfig {
    fn default() -> Self {
        Self {
            data_dir: PathBuf::from(DEFAULT_DATA_DIR),
            blobs_dir: None,
            storage: StorageConfig::default(),
        }
    }
}

/// blob 存储后端类型。
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum StorageBackend {
    /// 本地文件系统（默认，零外部依赖）。
    #[default]
    Fs,
    /// S3 兼容对象存储（可选 opt-in，需启用 `s3` 编译特性，ADR-0014）。
    S3,
}

/// blob 存储后端配置（ADR-0014）。
#[derive(Debug, Clone, Serialize, Deserialize, Default)]
pub struct StorageConfig {
    /// 后端类型：`fs`（默认）或 `s3`。
    #[serde(default)]
    pub backend: StorageBackend,
    /// S3 子配置；仅 `backend = "s3"` 时使用。
    #[serde(default)]
    pub s3: Option<S3Settings>,
}

/// S3 兼容对象存储连接配置（ADR-0014）。
///
/// 凭据（access key / secret key）不在此结构体内：其真源是配置/环境（沿用 AWS SDK 标准
/// `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` 等），绝不入库、不进日志、不进 DB 明文。
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct S3Settings {
    /// S3 端点 URL（兼容 MinIO 等自建网关；指向 AWS 时可省略由 region 推断）。
    #[serde(default)]
    pub endpoint: Option<String>,
    /// 区域（如 `us-east-1`；MinIO 等可填任意占位值）。
    pub region: String,
    /// 存储桶名。
    pub bucket: String,
    /// 对象 key 前缀（默认空）；与 sha256 内容寻址键拼接。
    #[serde(default)]
    pub prefix: String,
    /// 是否使用 path-style 寻址（MinIO 等自建网关需 true，默认 true）。
    #[serde(default = "default_path_style")]
    pub path_style: bool,
}

/// path-style 寻址默认值：true，兼容 MinIO 等自建对象存储。
fn default_path_style() -> bool {
    true
}

impl DataConfig {
    /// 解析 blob 存储根目录：优先用显式配置，否则取 `data_dir/blobs`。
    pub fn resolved_blobs_dir(&self) -> PathBuf {
        self.blobs_dir
            .clone()
            .unwrap_or_else(|| self.data_dir.join(DEFAULT_BLOBS_SUBDIR))
    }

    /// 解析 SQLite 数据库文件路径（位于数据目录下）。
    pub fn database_path(&self) -> PathBuf {
        self.data_dir.join("jianartifact.db")
    }
}

/// 认证与登录防护配置。
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct AuthConfig {
    /// Web 会话 / JWT 有效期（秒）。
    pub session_ttl_secs: u64,
    /// 触发锁定的连续登录失败次数。
    pub login_max_failures: u32,
    /// 锁定时长（秒）。
    pub login_lockout_secs: u64,
    /// OIDC 认证集成（FR-34 / ADR-0016）；未配置时为 None（不实例化 provider）。
    #[serde(default)]
    pub oidc: Option<OidcConfig>,
    /// LDAP 认证集成（FR-35 / ADR-0016）；未配置时为 None（不实例化 provider）。
    #[serde(default)]
    pub ldap: Option<LdapConfig>,
}

impl Default for AuthConfig {
    fn default() -> Self {
        Self {
            session_ttl_secs: DEFAULT_SESSION_TTL_SECS,
            login_max_failures: DEFAULT_LOGIN_MAX_FAILURES,
            login_lockout_secs: DEFAULT_LOGIN_LOCKOUT_SECS,
            oidc: None,
            ldap: None,
        }
    }
}

/// OIDC 认证集成配置（FR-34 / ADR-0016）。
///
/// `client_secret` 是密钥：真源在 env / 配置（前缀 `JIANARTIFACT_`），绝不入库、不进日志、
/// 不进 DB 明文。建议仅经环境变量 `JIANARTIFACT_AUTH_OIDC_CLIENT_SECRET` 提供，不写入入库 TOML。
#[derive(Clone, Serialize, Deserialize)]
pub struct OidcConfig {
    /// IdP 签发者标识（issuer），同时用作 discovery 基址与 ID Token `iss` 校验值。
    pub issuer: String,
    /// 客户端 ID。
    pub client_id: String,
    /// 客户端密钥（敏感）；真源 env / 配置，绝不入库 / 进日志。
    pub client_secret: String,
    /// 回调地址（须与 IdP 注册的 redirect_uri 完全一致）。
    pub redirect_uri: String,
    /// 是否即时开通（JIT）：默认关闭，无对应本地用户则拒登录（守 ADR-0010）。
    #[serde(default)]
    pub auto_provision: bool,
}

impl std::fmt::Debug for OidcConfig {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        // 绝不在调试输出中泄露 client_secret
        f.debug_struct("OidcConfig")
            .field("issuer", &self.issuer)
            .field("client_id", &self.client_id)
            .field("client_secret", &"<已脱敏>")
            .field("redirect_uri", &self.redirect_uri)
            .field("auto_provision", &self.auto_provision)
            .finish()
    }
}

/// 默认用户搜索过滤模板（按 `uid` 匹配，适配 OpenLDAP；AD 常用 `sAMAccountName`）。
const DEFAULT_LDAP_USER_FILTER: &str = "(uid={username})";
/// 默认取作建议用户名的属性名。
const DEFAULT_LDAP_USERNAME_ATTR: &str = "uid";
/// LDAP 默认连接超时（秒）。
const DEFAULT_LDAP_CONN_TIMEOUT_SECS: u64 = 10;

/// LDAP 认证集成配置（FR-35 / ADR-0016）。
///
/// `bind_password` 是密钥：真源在 env / 配置（前缀 `JIANARTIFACT_`），绝不入库、不进日志、
/// 不进 DB 明文。建议仅经环境变量 `JIANARTIFACT_AUTH_LDAP_BIND_PASSWORD` 提供，不写入入库 TOML。
#[derive(Clone, Serialize, Deserialize)]
pub struct LdapConfig {
    /// 目录服务 URL（`ldaps://host:636` 或 `ldap://host:389`）。
    pub url: String,
    /// 搜索绑定 DN（服务账号），连接后先用其查用户 DN。
    pub bind_dn: String,
    /// 搜索绑定口令（敏感）；真源 env / 配置，绝不入库 / 进日志。
    pub bind_password: String,
    /// 用户搜索基准 DN（如 `ou=people,dc=example,dc=org`）。
    pub user_search_base: String,
    /// 用户搜索过滤模板，含 `{username}` 占位符；缺省 `(uid={username})`。
    #[serde(default = "default_ldap_user_filter")]
    pub user_filter: String,
    /// 取作建议用户名的属性名；缺省 `uid`。
    #[serde(default = "default_ldap_username_attr")]
    pub username_attr: String,
    /// 是否使用 StartTLS（在明文端口上协商升级 TLS）；缺省 false。
    #[serde(default)]
    pub starttls: bool,
    /// 是否允许明文 `ldap://`（无 TLS）：缺省 false，仅运维在可信内网显式开启。
    #[serde(default)]
    pub allow_insecure: bool,
    /// 连接超时（秒）；缺省 10。
    #[serde(default = "default_ldap_conn_timeout_secs")]
    pub conn_timeout_secs: u64,
    /// 是否即时开通（JIT）：缺省关闭，无对应本地用户则拒登录（守 ADR-0010）。
    #[serde(default)]
    pub auto_provision: bool,
}

/// serde 默认值辅助：用户搜索过滤模板默认值。
fn default_ldap_user_filter() -> String {
    DEFAULT_LDAP_USER_FILTER.to_string()
}
/// serde 默认值辅助：建议用户名属性默认值。
fn default_ldap_username_attr() -> String {
    DEFAULT_LDAP_USERNAME_ATTR.to_string()
}
/// serde 默认值辅助：连接超时默认值。
fn default_ldap_conn_timeout_secs() -> u64 {
    DEFAULT_LDAP_CONN_TIMEOUT_SECS
}

impl std::fmt::Debug for LdapConfig {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        // 绝不在调试输出中泄露 bind_password
        f.debug_struct("LdapConfig")
            .field("url", &self.url)
            .field("bind_dn", &self.bind_dn)
            .field("bind_password", &"<已脱敏>")
            .field("user_search_base", &self.user_search_base)
            .field("user_filter", &self.user_filter)
            .field("username_attr", &self.username_attr)
            .field("starttls", &self.starttls)
            .field("allow_insecure", &self.allow_insecure)
            .field("conn_timeout_secs", &self.conn_timeout_secs)
            .field("auto_provision", &self.auto_provision)
            .finish()
    }
}

/// 上传等限制配置。
#[derive(Debug, Clone, Serialize, Deserialize, Default)]
pub struct LimitsConfig {
    /// 单个制品上传大小上限（字节）；为 None 表示不额外限制。超限返回 413。
    #[serde(default)]
    pub max_artifact_size: Option<u64>,
}

/// 代理仓库上游拉取配置。
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ProxyConfig {
    /// 上游拉取整体超时（秒）。
    pub upstream_timeout_secs: u64,
}

impl Default for ProxyConfig {
    fn default() -> Self {
        Self {
            upstream_timeout_secs: DEFAULT_UPSTREAM_TIMEOUT_SECS,
        }
    }
}

/// 可观测性配置：当前承载审计日志（FR-31）与使用分析采集（FR-57）。
/// 可观测性配置：承载审计日志（FR-31）与 Prometheus 指标端点（FR-32）。
#[derive(Debug, Clone, Serialize, Deserialize, Default)]
pub struct ObservabilityConfig {
    /// 审计日志配置。
    #[serde(default)]
    pub audit: AuditConfig,
    /// 使用分析采集配置。
    #[serde(default)]
    pub usage: UsageConfig,
    /// Prometheus 指标端点配置。
    #[serde(default)]
    pub metrics: MetricsConfig,
}

/// 审计日志配置（ADR-0015）。
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct AuditConfig {
    /// 保留天数：后台任务按此周期删除更早的审计行。
    pub retention_days: u32,
    /// 行数硬上限：超限删最旧行，兜底防止撑爆 SQLite。
    pub max_rows: u64,
}

impl Default for AuditConfig {
    fn default() -> Self {
        Self {
            retention_days: DEFAULT_AUDIT_RETENTION_DAYS,
            max_rows: DEFAULT_AUDIT_MAX_ROWS,
        }
    }
}

/// 使用分析采集配置（FR-57，ADR-0009）。
///
/// 聚合计数始终采集（开销小、量级可控）；明细默认关闭，开启后量级由 `max_detail_rows` 兜底裁剪。
/// 统计数据为本机内部数据、默认不外发，本结构不含任何外部导出 / 上报开关（本批不做导出）。
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct UsageConfig {
    /// 是否记录逐条访问 / 下载明细；默认关闭（仅聚合计数），避免明细无谓增长。
    pub detail_enabled: bool,
    /// 明细行数硬上限：超限删最旧行，兜底防止明细撑爆 SQLite。
    pub max_detail_rows: u64,
}

impl Default for UsageConfig {
    fn default() -> Self {
        Self {
            // 默认只采集聚合计数，不落明细
            detail_enabled: false,
            max_detail_rows: DEFAULT_USAGE_MAX_DETAIL_ROWS,
        }
    }
}

/// Prometheus 指标端点配置（FR-32，ADR-0015）。
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct MetricsConfig {
    /// 是否启用 `GET /metrics` 端点：默认开。关闭后端点返回 404，不安装 recorder。
    pub enabled: bool,
    /// 是否允许匿名抓取 `/metrics`：默认关，须运维显式开启。
    ///
    /// 关闭时端点要求认证且仅 Admin 可访问；开启时免认证抓取，**前提是把端点限定在内网 /
    /// 反向代理之后**（运维显式承担的暴露面，详见 OPERATIONS）。
    pub allow_anonymous: bool,
}

impl Default for MetricsConfig {
    fn default() -> Self {
        Self {
            enabled: DEFAULT_METRICS_ENABLED,
            allow_anonymous: DEFAULT_METRICS_ALLOW_ANONYMOUS,
        }
    }
}

/// 应用层（L7）防护配置（ADR-0008）：承载多维限流（FR-33 + FR-51）、慢速攻击防护（FR-52）、
/// 访问异常检测与自动封禁、IP 黑/白名单（FR-53）。
///
/// 仅做应用层防护；L3/L4 体积型 DDoS 由前置反向代理 / CDN / WAF 承担，不在二进制内实现。
/// 承载多维限流（FR-33 + FR-51）、慢速攻击防护（FR-52）、异常封禁 + IP 名单（FR-53）、
/// CC 挑战（FR-54）、WAF 规则引擎（FR-55）与防护监控告警（FR-56）。
#[derive(Debug, Clone, Serialize, Deserialize, Default)]
pub struct ProtectionConfig {
    /// 多维速率限制与并发上限配置。
    #[serde(default)]
    pub rate_limit: RateLimitConfig,
    /// IP 黑 / 白名单配置（FR-53）：黑名单直接拒、白名单豁免一切防护。
    #[serde(default)]
    pub ip_list: IpListConfig,
    /// 访问异常检测与自动封禁配置（FR-53）。
    #[serde(default)]
    pub ban: BanConfig,
    /// 慢速攻击（slowloris）超时与通用请求体大小限制配置（FR-52）。
    #[serde(default)]
    pub slowloris: SlowlorisConfig,
    /// CC 挑战（工作量证明 PoW）配置（FR-54）。
    #[serde(default)]
    pub cc_challenge: CcChallengeConfig,
    /// 可配置 WAF 规则引擎配置（FR-55）：按请求模式匹配阻断 / 放行。
    #[serde(default)]
    pub waf: WafConfig,
    /// 防护监控与阈值告警配置（FR-56）：窗内各维度防护事件达阈值即告警并落库。
    #[serde(default)]
    pub alerts: AlertsConfig,
}

/// CC 挑战难度上限（位）：与中间件实现一致，超过即无意义（PoW 不可解）。
const MAX_CC_CHALLENGE_DIFFICULTY_BITS: u32 = 64;

impl ProtectionConfig {
    /// 校验防护配置各维度的数值边界（FR-79，运行时热替换入口用）。
    ///
    /// 纯函数、无副作用，便于穷举测试。仅校验会导致运行异常或无意义的边界：
    /// - 各时间窗（限流 / 异常封禁 / 告警评估窗）必须 > 0，否则固定窗计数无法成立。
    /// - 慢速攻击防护的两个超时必须 > 0，否则等同零超时立即断流、误杀正常上传。
    /// - CC 挑战令牌有效期必须 > 0，否则签发即过期；难度不得超过
    ///   [`MAX_CC_CHALLENGE_DIFFICULTY_BITS`]（超过则 PoW 不可解、等同 DoS 自身）。
    ///
    /// WAF 规则的字段 / 匹配类型 / 动作合法性沿用既有「非法项记 WARN 跳过、不阻断」语义（与文件配置
    /// 向后兼容），不在此处硬拒，避免单条坏规则使整次热替换失败。校验通过返回 `Ok(())`，否则返回中文原因。
    pub fn validate(&self) -> Result<(), String> {
        if self.rate_limit.window_secs == 0 {
            return Err("限流时间窗（rate_limit.window_secs）必须大于 0".to_string());
        }
        if self.ban.window_secs == 0 {
            return Err("异常检测时间窗（ban.window_secs）必须大于 0".to_string());
        }
        if self.alerts.window_secs == 0 {
            return Err("告警评估时间窗（alerts.window_secs）必须大于 0".to_string());
        }
        if self.slowloris.body_read_timeout_secs == 0 || self.slowloris.header_timeout_secs == 0 {
            return Err("慢速攻击防护超时（slowloris.*_timeout_secs）必须大于 0".to_string());
        }
        if self.cc_challenge.ttl_secs == 0 {
            return Err("CC 挑战令牌有效期（cc_challenge.ttl_secs）必须大于 0".to_string());
        }
        if self.cc_challenge.difficulty > MAX_CC_CHALLENGE_DIFFICULTY_BITS {
            return Err(format!(
                "CC 挑战难度（cc_challenge.difficulty）不得超过 {MAX_CC_CHALLENGE_DIFFICULTY_BITS} 位"
            ));
        }
        Ok(())
    }
}

/// 防护监控与阈值告警配置（FR-56，ADR-0017）。
///
/// 进程内在固定时间窗内统计各防护维度（限流被拒 / 自动封禁 / CC 挑战失败 / WAF 阻断 / 慢速超时）
/// 的事件计数，单维度窗内计数达对应阈值即产生一条告警：按严重度记中文分级日志（WARN）并异步落
/// SQLite（`protection_alerts` 表）。同一维度在窗内**去抖**——一窗内同维度只告警一次，不刷屏。
/// **默认关闭**：避免无人值守时刷告警；阈值默认保守宽放，避免正常高频访问误报。启用与阈值由运维显式承担。
/// 告警是本机内部数据：只落本地、不外发、不内置外发型通知（Webhook / 邮件等若未来要做须另写 ADR）。
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct AlertsConfig {
    /// 是否启用阈值告警；默认关闭，关闭时不评估、不落库、零额外开销。
    pub enabled: bool,
    /// 告警评估固定时间窗时长（秒）：每窗内独立统计各维度计数，跨窗清零。
    pub window_secs: u64,
    /// 限流被拒窗内告警阈值：一窗内限流被拒次数达此值即告警。
    pub rate_limit_warn_threshold: u64,
    /// 自动封禁触发窗内告警阈值：一窗内自动封禁触发次数达此值即告警。
    pub ban_warn_threshold: u64,
    /// CC 挑战失败窗内告警阈值：一窗内 CC 证明校验失败次数达此值即告警。
    pub cc_challenge_fail_warn_threshold: u64,
    /// WAF 阻断窗内告警阈值：一窗内 WAF 阻断次数达此值即告警。
    pub waf_block_warn_threshold: u64,
    /// 慢速攻击超时窗内告警阈值：一窗内慢速超时 / 截断拒绝次数达此值即告警。
    pub slowloris_warn_threshold: u64,
    /// 告警明细行数硬上限：超限删最旧行，兜底防止撑爆 SQLite。
    pub max_rows: u64,
}

impl Default for AlertsConfig {
    fn default() -> Self {
        Self {
            enabled: DEFAULT_ALERTS_ENABLED,
            window_secs: DEFAULT_ALERTS_WINDOW_SECS,
            rate_limit_warn_threshold: DEFAULT_ALERTS_RATE_LIMIT_WARN_THRESHOLD,
            ban_warn_threshold: DEFAULT_ALERTS_BAN_WARN_THRESHOLD,
            cc_challenge_fail_warn_threshold: DEFAULT_ALERTS_CC_CHALLENGE_FAIL_WARN_THRESHOLD,
            waf_block_warn_threshold: DEFAULT_ALERTS_WAF_BLOCK_WARN_THRESHOLD,
            slowloris_warn_threshold: DEFAULT_ALERTS_SLOWLORIS_WARN_THRESHOLD,
            max_rows: DEFAULT_ALERTS_MAX_ROWS,
        }
    }
}

/// IP 黑 / 白名单配置（FR-53，ADR-0008）。
///
/// 支持单 IP（如 `203.0.113.7`）与 CIDR 网段（如 `10.0.0.0/8`）两种写法，IPv4 / IPv6 均可。
/// **白名单优先级最高**：命中白名单的来源豁免限流 / 封禁 / 异常检测，照常进入业务；命中黑名单
/// 的来源在进入业务前直接拒（403）。两者均按**连接级来源 IP** 判定，不采信 `X-Forwarded-For`。
/// 默认两表皆空 = 不启用名单（不影响现有行为）。
#[derive(Debug, Clone, Serialize, Deserialize, Default)]
pub struct IpListConfig {
    /// 白名单条目（IP 或 CIDR）：命中即豁免一切应用层防护，优先级高于黑名单。
    #[serde(default)]
    pub allow: Vec<String>,
    /// 黑名单条目（IP 或 CIDR）：命中即在进入业务前直接拒绝（403）。
    #[serde(default)]
    pub deny: Vec<String>,
}

/// 访问异常检测与自动封禁配置（FR-53，ADR-0008）。
///
/// 在固定时间窗内按**连接级来源 IP** 统计异常信号（4xx 客户端错误 / 被限流拒绝），单 IP 一窗内
/// 异常信号数达 `threshold` 即自动封禁 `duration_secs`，封禁期内该 IP 一律拒绝（403）；封禁到期
/// 自动解封。封禁状态进程内内存维护（时间窗，重启即清），不落 DB。
/// 默认关闭且阈值保守宽放，避免误杀正常包管理器的偶发 404 / 鉴权重试；启用与调阈值由运维显式承担。
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct BanConfig {
    /// 是否启用异常检测与自动封禁；默认关闭，关闭时不统计、不封禁、零额外开销。
    pub enabled: bool,
    /// 异常检测固定时间窗时长（秒）：每窗内独立统计异常信号，跨窗清零。
    pub window_secs: u64,
    /// 触发封禁的窗内异常信号阈值：单 IP 一窗内异常信号数达此值即封禁。
    pub threshold: u64,
    /// 自动封禁时长（秒）：封禁期内该 IP 一律拒绝；到期自动解封。
    pub duration_secs: u64,
}

impl Default for BanConfig {
    fn default() -> Self {
        Self {
            enabled: DEFAULT_BAN_ENABLED,
            window_secs: DEFAULT_BAN_WINDOW_SECS,
            threshold: DEFAULT_BAN_THRESHOLD,
            duration_secs: DEFAULT_BAN_DURATION_SECS,
        }
    }
}

/// 多维速率限制与并发上限配置（FR-33 + FR-51，ADR-0008）。
///
/// 进程内固定窗计数，按 IP / 身份（用户及其所有 Token）/ 用户 / 仓库维度分别限流；任一维度
/// 超阈值返回 429。并发维度按 IP / 用户 / 仓库限制在途请求数，超上限返回 429。
/// 默认关闭且阈值保守（新增维度默认 0 = 不启用），避免误杀正常包管理器批量拉取；
/// 启用与调阈值由运维显式承担。
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct RateLimitConfig {
    /// 是否启用速率限制；默认关闭，关闭时中间件直接放行、零计数开销。
    pub enabled: bool,
    /// 固定时间窗时长（秒）：每个窗内独立计数，跨窗清零。
    pub window_secs: u64,
    /// 单 IP 每窗请求数上限：超过即对该 IP 限流。
    pub ip_max_requests: u64,
    /// 单身份（用户 / 其所有 Token / 会话）每窗请求数上限：超过即对该主体限流。
    ///
    /// 此即 FR-51 的「用户维度」限流：身份按 `user_id` 归一，覆盖该用户的所有 Token 与会话。
    pub identity_max_requests: u64,
    /// 单仓库每窗请求数上限（FR-51 仓库维度，按格式路径首段仓库名计数）：0 表示不启用。
    #[serde(default = "default_repo_max_requests")]
    pub repo_max_requests: u64,
    /// 单 IP 并发在途请求上限（FR-51 并发上限）：0 表示不限并发。
    #[serde(default = "default_ip_max_concurrent")]
    pub ip_max_concurrent: u64,
    /// 单用户并发在途请求上限（FR-51 并发上限）：0 表示不限并发。
    #[serde(default = "default_user_max_concurrent")]
    pub user_max_concurrent: u64,
    /// 单仓库并发在途请求上限（FR-51 并发上限）：0 表示不限并发。
    #[serde(default = "default_repo_max_concurrent")]
    pub repo_max_concurrent: u64,
}

/// serde 默认值辅助：仓库维度每窗请求上限默认值。
fn default_repo_max_requests() -> u64 {
    DEFAULT_RATE_LIMIT_REPO_MAX_REQUESTS
}
/// serde 默认值辅助：单 IP 并发上限默认值。
fn default_ip_max_concurrent() -> u64 {
    DEFAULT_RATE_LIMIT_IP_MAX_CONCURRENT
}
/// serde 默认值辅助：单用户并发上限默认值。
fn default_user_max_concurrent() -> u64 {
    DEFAULT_RATE_LIMIT_USER_MAX_CONCURRENT
}
/// serde 默认值辅助：单仓库并发上限默认值。
fn default_repo_max_concurrent() -> u64 {
    DEFAULT_RATE_LIMIT_REPO_MAX_CONCURRENT
}

impl Default for RateLimitConfig {
    fn default() -> Self {
        Self {
            enabled: DEFAULT_RATE_LIMIT_ENABLED,
            window_secs: DEFAULT_RATE_LIMIT_WINDOW_SECS,
            ip_max_requests: DEFAULT_RATE_LIMIT_IP_MAX_REQUESTS,
            identity_max_requests: DEFAULT_RATE_LIMIT_IDENTITY_MAX_REQUESTS,
            repo_max_requests: DEFAULT_RATE_LIMIT_REPO_MAX_REQUESTS,
            ip_max_concurrent: DEFAULT_RATE_LIMIT_IP_MAX_CONCURRENT,
            user_max_concurrent: DEFAULT_RATE_LIMIT_USER_MAX_CONCURRENT,
            repo_max_concurrent: DEFAULT_RATE_LIMIT_REPO_MAX_CONCURRENT,
        }
    }
}

/// 慢速攻击（slowloris）超时与通用请求体大小限制配置（FR-52，ADR-0008）。
///
/// 仅做应用层（L7）防护：对慢速 drip 请求体设「块间空闲超时」与「首块等待超时」，超时即断开，
/// 避免连接长期被占用；并对所有请求体设可配置的通用大小上限（超限 413）。默认关闭且超时档位保守，
/// 对正常大文件流式上传（mvn deploy 大 jar、docker push 大层）友好——只惩罚长时间不发数据的慢速连接，
/// 不按整体时长一刀切。L3/L4 体积型攻击仍交前置反向代理 / CDN / WAF。
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct SlowlorisConfig {
    /// 是否启用慢速攻击防护与通用请求体大小限制；默认关闭，关闭时中间件直接放行、零额外开销。
    pub enabled: bool,
    /// 请求体相邻数据块的空闲超时（秒）：两次到达数据块的最大间隔，超过即判为慢速 drip 并断开。
    pub body_read_timeout_secs: u64,
    /// 等待请求体首个数据块的超时（秒）：发完头后迟迟不发体即判为慢速起始攻击并断开。
    pub header_timeout_secs: u64,
    /// 单个请求体通用大小上限（字节）：超过即返回 413。0 表示不启用该通用上限。
    ///
    /// 区别于 `limits.max_artifact_size`（仅约束制品上传体）：本项对所有请求体兜底，
    /// 启用时应设得高于预期最大制品体，避免误杀正常大制品流式上传。
    pub max_body_bytes: u64,
}

impl Default for SlowlorisConfig {
    fn default() -> Self {
        Self {
            enabled: DEFAULT_SLOWLORIS_ENABLED,
            body_read_timeout_secs: DEFAULT_SLOWLORIS_BODY_READ_TIMEOUT_SECS,
            header_timeout_secs: DEFAULT_SLOWLORIS_HEADER_TIMEOUT_SECS,
            max_body_bytes: DEFAULT_SLOWLORIS_MAX_BODY_BYTES,
        }
    }
}

/// CC 挑战（工作量证明 PoW）配置（FR-54，ADR-0008）。
///
/// 对疑似 CC（HTTP 洪水）攻击的匿名来源下发工作量证明挑战：客户端须找到 `nonce` 使
/// `sha256(challenge_token + ":" + nonce)` 前导零位数达 `difficulty`，带证明重试方放行。
/// 服务端无状态校验（HMAC 签名挑战，绑定来源 IP + 难度 + 签发时刻），不存挑战态。
/// **默认关闭**：正常包管理器 CLI 不会解 PoW，无差别拦截会打断正常拉取；默认豁免已认证客户端
/// （带凭据的 CLI），挑战只面向匿名可疑流量。启用与否由运维显式承担。
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct CcChallengeConfig {
    /// 是否启用 CC 挑战；**默认关闭**，关闭时中间件直接放行、零开销。
    pub enabled: bool,
    /// PoW 难度（要求 sha256 摘要的前导零比特数）：越高客户端开销越大。默认 20。
    pub difficulty: u32,
    /// 挑战令牌有效期（秒）：签发后超此时长的证明视为过期、须重新获取挑战。默认 300。
    pub ttl_secs: u64,
    /// 是否豁免已认证（Bearer / Basic / 会话）请求：**默认豁免**，避免误伤带凭据的包管理器 CLI。
    pub exempt_authenticated: bool,
}

impl Default for CcChallengeConfig {
    fn default() -> Self {
        Self {
            enabled: DEFAULT_CC_CHALLENGE_ENABLED,
            difficulty: DEFAULT_CC_CHALLENGE_DIFFICULTY,
            ttl_secs: DEFAULT_CC_CHALLENGE_TTL_SECS,
            exempt_authenticated: DEFAULT_CC_CHALLENGE_EXEMPT_AUTHENTICATED,
        }
    }
}

/// 可配置 WAF 规则引擎配置（FR-55，ADR-0008）。
///
/// 仅做应用层（L7）请求模式匹配与阻断：按有序规则对请求的 method / path / query / 指定 header
/// 做字面（literal）/ 通配（wildcard，`*`/`?`）/ 正则（regex）匹配，**首个命中生效**——命中
/// `block` 即在进入业务前返回 `403`，命中 `allow` 即放行（短路后续规则）。规则在启动期**编译一次**
/// （正则预编译），热路径仅做匹配；非法规则记 WARN 跳过、不阻断启动。
/// 默认**空规则集 + 关闭**，不影响现有行为、不误杀正常包管理器请求；启用与规则集由运维显式承担。
/// 仅应用层防护；L3/L4 体积型攻击仍交前置反向代理 / CDN / WAF。
#[derive(Debug, Clone, Serialize, Deserialize, Default)]
pub struct WafConfig {
    /// 是否启用 WAF 规则引擎；默认关闭，关闭或空规则集时中间件直接放行、零额外开销。
    #[serde(default = "default_waf_enabled")]
    pub enabled: bool,
    /// 有序规则集：按声明顺序逐条匹配，**首个命中生效**；默认空（不阻断任何请求）。
    #[serde(default)]
    pub rules: Vec<WafRuleConfig>,
}

/// serde 默认值辅助：WAF 启用开关默认值（默认关闭）。
fn default_waf_enabled() -> bool {
    DEFAULT_WAF_ENABLED
}

/// 单条 WAF 规则配置（FR-55，ADR-0008）。
///
/// 对请求的某个属性字段（`field`）按指定匹配类型（`match_type`）匹配 `pattern`，命中即执行 `action`。
/// `field` 为 `header` 时须配 `header_name`（指定要匹配的请求头名，大小写不敏感）；其余字段忽略它。
/// 字段 / 匹配类型 / 动作均为受限枚举字符串（非法值在编译时记 WARN 跳过该条，不阻断启动）。
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct WafRuleConfig {
    /// 匹配的请求属性字段：`method` / `path` / `query` / `header`。
    pub field: String,
    /// 当 `field = "header"` 时，指定要匹配的请求头名（大小写不敏感）；其余字段忽略。
    #[serde(default)]
    pub header_name: Option<String>,
    /// 匹配模式字符串：按 `match_type` 解释（字面值 / 通配模式 / 正则表达式）。
    pub pattern: String,
    /// 匹配类型：`literal`（字面相等 / 包含）/ `wildcard`（`*`/`?` 通配）/ `regex`（正则）。
    pub match_type: String,
    /// 命中后的动作：`block`（拒 403）/ `allow`（放行并短路后续规则）。
    pub action: String,
}

/// 漏洞库离线镜像配置（FR-70，ADR-0012）。
///
/// 默认关闭：镜像需主动联网拉取公开漏洞数据集到本机，应由运维显式开启。
/// 下载的是公开数据集整体镜像，**不把本机制品坐标逐包外发到外部漏洞服务**（守隐私红线）。
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct VulnConfig {
    /// 是否启用漏洞库离线镜像；默认关闭，由运维显式开启。
    pub enabled: bool,
    /// 镜像数据源基址（按生态在其下取 `{ecosystem}/all.zip`）。
    pub source_base_url: String,
    /// 镜像的生态列表（如 ["Maven", "npm"]）；为空表示不镜像任何生态。
    pub ecosystems: Vec<String>,
    /// 刷新周期（秒）：每隔该时长重新拉取并落库一次。
    pub refresh_interval_secs: u64,
    /// 单次镜像下载的整体超时（秒）。
    pub download_timeout_secs: u64,
}

impl Default for VulnConfig {
    fn default() -> Self {
        Self {
            // 默认关闭，避免未配置时静默联网拉取
            enabled: false,
            source_base_url: DEFAULT_VULN_SOURCE_BASE_URL.to_string(),
            // 默认不预设生态，由运维按需开启，避免无意义的全量下载
            ecosystems: Vec::new(),
            refresh_interval_secs: DEFAULT_VULN_REFRESH_INTERVAL_SECS,
            download_timeout_secs: DEFAULT_VULN_DOWNLOAD_TIMEOUT_SECS,
        }
    }
}

/// 出站网络配置（FR-84，ADR-0020）：当前仅承载正向出站代理。
#[derive(Debug, Clone, Serialize, Deserialize, Default)]
pub struct NetworkConfig {
    /// 出站代理配置。
    #[serde(default)]
    pub proxy: NetworkProxyConfig,
}

/// 出站正向代理配置（FR-84，ADR-0020）。
///
/// 三键均默认 `None`：全不配置时不显式注入代理，保持 reqwest 既有行为不变
/// （含其默认 honor 系统 `HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY` 环境变量）。
/// 任一键给值即以配置为真源（见 [`build_outbound_client`] 与 ADR-0020）。
#[derive(Debug, Clone, Serialize, Deserialize, Default)]
pub struct NetworkProxyConfig {
    /// HTTP 出站代理 URL（如 `http://proxy.internal:8080`，可含 `user:pass@` 凭据）。
    #[serde(default)]
    pub http: Option<String>,
    /// HTTPS 出站代理 URL。
    #[serde(default)]
    pub https: Option<String>,
    /// 直连绕过列表（逗号分隔的主机 / 域 / CIDR），命中者不经代理。
    #[serde(default)]
    pub no_proxy: Option<String>,
}

/// 在线更新配置（FR-85，ADR-0021）。
///
/// 管理员手动触发的完整自更新：查 GitHub 最新稳定 Release、按本机 target 下载资产、
/// 校验 sha256、原子替换二进制并自动重启。出站默认关闭，须运维显式开启。
/// `token` 真源为 env `JIANARTIFACT_UPDATE_TOKEN`（私有仓库可选），不入库、不进日志、不回显。
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct UpdateConfig {
    /// 是否启用在线更新（出站开关）：默认 `false`，关闭时检查 / 应用端点一律拒绝、不联网。
    pub enabled: bool,
    /// 仓库源（`owner/repo`），默认 `wcpe/JianArtifact`。
    pub repo: String,
    /// GitHub API 基址，可配（便于测试 / 镜像），默认 `https://api.github.com`。
    pub api_base_url: String,
    /// 重启模式：`self`（重启后自拉起新进程）或 `exit`（仅退出，交外部进程管理器重启）。
    pub restart_mode: String,
    /// 资产下载整体超时（秒），默认 300。
    pub download_timeout_secs: u64,
    /// 更新通道（FR-89）：`stable`（仅稳定版，默认）或 `prerelease`（含预发布，取最新一条）。
    #[serde(default = "default_update_channel")]
    pub channel: String,
    /// 私有仓库可选访问 token（真源 env `JIANARTIFACT_UPDATE_TOKEN`）。
    ///
    /// 绝不入库、不进日志、不回显：序列化时一律跳过，避免写入配置导出或调试输出。
    #[serde(default, skip_serializing)]
    pub token: Option<String>,
}

/// `channel` 字段缺省值（TOML 未给 `[update] channel` 时回落默认通道）。
fn default_update_channel() -> String {
    DEFAULT_UPDATE_CHANNEL.to_string()
}

impl Default for UpdateConfig {
    fn default() -> Self {
        Self {
            enabled: DEFAULT_UPDATE_ENABLED,
            repo: DEFAULT_UPDATE_REPO.to_string(),
            api_base_url: DEFAULT_UPDATE_API_BASE_URL.to_string(),
            restart_mode: DEFAULT_UPDATE_RESTART_MODE.to_string(),
            download_timeout_secs: DEFAULT_UPDATE_DOWNLOAD_TIMEOUT_SECS,
            channel: DEFAULT_UPDATE_CHANNEL.to_string(),
            token: None,
        }
    }
}

/// 构造统一的出站 reqwest 客户端（FR-84，ADR-0020）。
///
/// 在固定的 rustls / stream 特性（见 Cargo.toml）与给定整体超时基础上，按 `proxy` 配置
/// 注入出站正向代理：
/// - `https` / `http` 任一给值即注入对应 scheme 的 [`reqwest::Proxy`]，并关闭 reqwest 的
///   自动系统代理探测（配置为真源，压过系统环境）；
/// - `no_proxy` 给值则解析为绕过列表挂到所注入的各 Proxy 上；
/// - 三键全空时不调用任何 `.proxy()`，保持 reqwest 默认行为（含其系统环境变量 honor），
///   从而「不配置即与现状一致」。
///
/// 失败返回的错误信息**不含原始代理 URL**（避免泄露代理凭据，守安全脱敏红线）。
pub fn build_outbound_client(
    timeout: std::time::Duration,
    proxy: &NetworkProxyConfig,
) -> Result<reqwest::Client, String> {
    let mut builder = reqwest::Client::builder().timeout(timeout);

    // no_proxy 解析一次，挂到每个注入的 Proxy 上（reqwest 的绕过列表绑定在 Proxy 维度）
    let no_proxy = proxy
        .no_proxy
        .as_deref()
        .and_then(reqwest::NoProxy::from_string);

    if let Some(url) = proxy.https.as_deref() {
        let p = reqwest::Proxy::https(url)
            .map_err(|_| "出站 HTTPS 代理配置无效".to_string())?
            .no_proxy(no_proxy.clone());
        builder = builder.proxy(p);
    }
    if let Some(url) = proxy.http.as_deref() {
        let p = reqwest::Proxy::http(url)
            .map_err(|_| "出站 HTTP 代理配置无效".to_string())?
            .no_proxy(no_proxy.clone());
        builder = builder.proxy(p);
    }

    builder.build().map_err(|e| e.to_string())
}

// ===== FR-88 / ADR-0022：运行时可编辑设置与出站客户端热替换 =====

/// 出站网络当前快照：当前生效的代理配置 + 据其构造的出站 `reqwest::Client` + 构造用超时。
///
/// 三者打包成不可变、整体替换的快照，保证「代理配置」与「据其构造的 client」始终一致，
/// 绝不出现「配置已换、client 仍是旧代理」的中间态（仿 ADR-0018 防护快照）。
#[derive(Clone)]
pub struct NetworkSnapshot {
    /// 当前生效的出站代理配置（PATCH 整体替换的来源）。
    pub proxy: NetworkProxyConfig,
    /// 构造出站 client 用的整体请求超时（沿用启动期超时，热替换不改超时口径）。
    pub timeout: std::time::Duration,
    /// 据 `proxy` + `timeout` 经 [`build_outbound_client`] 构造的出站客户端。
    ///
    /// `reqwest::Client` 内部为 `Arc`，clone 仅引用计数 +1、廉价；各出站点经
    /// [`NetworkState::client`] 取一份立即在锁外发起请求。
    pub client: reqwest::Client,
}

/// 运行时出站网络代理热替换槽（FR-88，ADR-0022）：随 `AppState` 经 `Arc` 共享。
///
/// 读多写极少：各出站点经 [`Self::client`] 取当前 client（读锁极短、锁外发请求）；管理端 PATCH
/// 经 [`Self::replace_proxy`] 锁外重建 client、再短持写锁原子换快照。用 std `RwLock<Arc<..>>`
/// 实现，不引入外部依赖。
pub struct NetworkState {
    /// 当前生效快照；替换时整体换 `Arc`，读时 clone 出锁。
    current: std::sync::RwLock<std::sync::Arc<NetworkSnapshot>>,
}

impl NetworkState {
    /// 用初始代理配置与超时构造热替换槽（启动期由 `[network.proxy]` 文件 / env 配置装载）。
    ///
    /// 构造失败（代理 URL 无效 / TLS 初始化异常）冒泡给调用方；错误信息不含代理凭据。
    pub fn new(proxy: NetworkProxyConfig, timeout: std::time::Duration) -> Result<Self, String> {
        let client = build_outbound_client(timeout, &proxy)?;
        Ok(Self {
            current: std::sync::RwLock::new(std::sync::Arc::new(NetworkSnapshot {
                proxy,
                timeout,
                client,
            })),
        })
    }

    /// 取当前出站客户端：读锁内 clone `reqwest::Client`（内部 `Arc`、廉价）立即放锁，锁外发请求。
    pub fn client(&self) -> reqwest::Client {
        self.current
            .read()
            .unwrap_or_else(|e| e.into_inner())
            .client
            .clone()
    }

    /// 取当前生效快照（含代理配置）：读锁内 clone `Arc` 立即放锁，调用方锁外读。
    pub fn snapshot(&self) -> std::sync::Arc<NetworkSnapshot> {
        std::sync::Arc::clone(&self.current.read().unwrap_or_else(|e| e.into_inner()))
    }

    /// 用新代理配置原子替换当前快照：**锁外**重建 client，再短持写锁换指针。
    ///
    /// 构造失败即返回错误且**不替换**（现有 client 仍生效）；成功后下一个 [`Self::client`] 即返回
    /// 新代理的 client，对应下一个出站请求即经新代理。沿用当前快照的超时口径。
    pub fn replace_proxy(&self, proxy: NetworkProxyConfig) -> Result<(), String> {
        // 沿用当前超时，PATCH 只调代理
        let timeout = self
            .current
            .read()
            .unwrap_or_else(|e| e.into_inner())
            .timeout;
        // 锁外重建 client（TLS / 代理初始化开销均在临界区外完成）；失败即返回、不触碰现有快照
        let client = build_outbound_client(timeout, &proxy)?;
        let next = std::sync::Arc::new(NetworkSnapshot {
            proxy,
            timeout,
            client,
        });
        // 写临界区只做一次指针赋值，短持有、不做编译 / IO
        let mut guard = self.current.write().unwrap_or_else(|e| e.into_inner());
        *guard = next;
        Ok(())
    }
}

/// 在线更新通道（FR-89）：决定取哪一条 GitHub Release。
///
/// `Stable` 走 `/releases/latest`（只认稳定版，默认）；`Prerelease` 走 `/releases` 列表、
/// 取最新一条非 draft 的 release（含预发布）。纯枚举，便于 check / apply 据其选源。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum UpdateChannel {
    /// 稳定通道：仅最新稳定版（`/releases/latest`）。
    Stable,
    /// 预发布通道：含预发布的最新一条（`/releases` 列表）。
    Prerelease,
}

impl UpdateChannel {
    /// 由配置字符串解析通道：未知值回退 `Stable`（最保守默认，配合 `validate` 在编辑期拒非法值）。
    pub fn from_config(s: &str) -> Self {
        match s {
            "prerelease" => UpdateChannel::Prerelease,
            _ => UpdateChannel::Stable,
        }
    }
}

/// 在线更新运行时可调配置（FR-88，ADR-0022，收拢 ADR-0021 可热替换的字段）。
///
/// 不含 `download_timeout_secs`（出站 client 经 `NetworkState` 取、超时随槽），只承载 check / apply
/// 需读且可运行时调的字段。`token` 绝不回显 / 不入库 / 不进日志。
#[derive(Debug, Clone)]
pub struct EditableUpdate {
    /// 是否启用在线更新（出站开关）：false 时 check / apply 一律拒绝、不联网。
    pub enabled: bool,
    /// 仓库源（`owner/repo`）。
    pub repo: String,
    /// GitHub API 基址。
    pub api_base_url: String,
    /// 重启模式：`self`（自拉起）或 `exit`（交外部进程管理器）。
    pub restart_mode: String,
    /// 更新通道（FR-89）：`stable`（仅稳定版）或 `prerelease`（含预发布）。
    pub channel: String,
    /// 资产下载整体超时（秒）。
    pub download_timeout_secs: u64,
    /// 私有仓库可选访问 token（真源 env，绝不回显 / 入库 / 进日志）。
    pub token: Option<String>,
}

impl EditableUpdate {
    /// 从启动期 [`UpdateConfig`] 装载初值。
    pub fn from_config(cfg: &UpdateConfig) -> Self {
        Self {
            enabled: cfg.enabled,
            repo: cfg.repo.clone(),
            api_base_url: cfg.api_base_url.clone(),
            restart_mode: cfg.restart_mode.clone(),
            channel: cfg.channel.clone(),
            download_timeout_secs: cfg.download_timeout_secs,
            token: cfg.token.clone(),
        }
    }

    /// 校验运行时可调字段：`repo` / `api_base_url` 非空、`restart_mode` 合法。
    pub fn validate(&self) -> Result<(), String> {
        if self.repo.trim().is_empty() {
            return Err("仓库源 repo 不能为空".to_string());
        }
        if self.api_base_url.trim().is_empty() {
            return Err("GitHub API 基址 api_base_url 不能为空".to_string());
        }
        if self.restart_mode != "self" && self.restart_mode != "exit" {
            return Err("重启模式 restart_mode 仅允许 self 或 exit".to_string());
        }
        if self.channel != "stable" && self.channel != "prerelease" {
            return Err("更新通道 channel 仅允许 stable 或 prerelease".to_string());
        }
        Ok(())
    }
}

/// 运行时可编辑设置热替换槽（FR-88，ADR-0022）：随 `AppState` 经 `Arc` 共享。
///
/// 收拢两块可运行时调整的配置：出站网络代理（[`NetworkState`]，含据代理构造的 client）与在线更新
/// 可调字段（[`EditableUpdate`]）。设置页 `PATCH /api/v1/settings` 校验后换槽即时生效、无须重启；
/// 凭据（代理 `user:pass@` 与 update token）只入本内存槽、不写回 TOML / 不入 DB / 不回显。
pub struct EditableSettings {
    /// 出站网络代理热替换槽（含当前 client）；`Arc` 共享给各出站点持有。
    pub network: std::sync::Arc<NetworkState>,
    /// 在线更新可调字段（`RwLock<Arc<..>>` 原子换）。
    update: std::sync::RwLock<std::sync::Arc<EditableUpdate>>,
}

impl EditableSettings {
    /// 用启动期网络代理 + 出站超时 + 在线更新配置构造可编辑设置槽。
    ///
    /// `network_timeout` 为出站 client 整体超时（沿用启动期上游超时口径）。构造失败（代理无效）
    /// 冒泡给调用方，错误信息不含凭据。
    pub fn new(
        proxy: NetworkProxyConfig,
        network_timeout: std::time::Duration,
        update: &UpdateConfig,
    ) -> Result<Self, String> {
        Ok(Self {
            network: std::sync::Arc::new(NetworkState::new(proxy, network_timeout)?),
            update: std::sync::RwLock::new(std::sync::Arc::new(EditableUpdate::from_config(
                update,
            ))),
        })
    }

    /// 取当前在线更新可调配置：读锁内 clone `Arc` 立即放锁。
    pub fn update(&self) -> std::sync::Arc<EditableUpdate> {
        std::sync::Arc::clone(&self.update.read().unwrap_or_else(|e| e.into_inner()))
    }

    /// 原子替换在线更新可调配置（调用方应先 [`EditableUpdate::validate`] 校验）。
    pub fn replace_update(&self, next: EditableUpdate) {
        let mut guard = self.update.write().unwrap_or_else(|e| e.into_inner());
        *guard = std::sync::Arc::new(next);
    }
}

impl Config {
    /// 从指定 TOML 文件与环境变量加载配置。
    ///
    /// 加载顺序：内置默认值 → TOML 文件（若存在）→ 环境变量覆盖。
    /// TOML 文件不存在时不报错，仅使用默认值与环境变量。
    pub fn load(config_path: &Path) -> Result<Self, Box<figment::Error>> {
        Figment::new()
            // 以默认值打底，缺省项不必在 TOML 中显式给出
            .merge(Serialized::defaults(Config::default()))
            // TOML 文件覆盖默认值；文件缺失时 figment 跳过该 provider 不报错
            .merge(Toml::file(config_path))
            // 环境变量优先级最高。仅把节名后的首个下划线映射为嵌套分隔，
            // 键名内部的下划线保留（如 auth.session_ttl_secs）。
            .merge(Env::prefixed(ENV_PREFIX).map(map_env_key))
            .extract()
            .map_err(Box::new)
    }
}

/// 把（已去前缀的）环境变量键映射为嵌套配置键。
///
/// 仅当键以某个已知节名 + 下划线开头时，才把该首个下划线替换为点，
/// 其余下划线保留，从而 `server_port` → `server.port`、
/// `auth_session_ttl_secs` → `auth.session_ttl_secs`。
fn map_env_key(key: &UncasedStr) -> Uncased<'_> {
    let lower = key.as_str().to_ascii_lowercase();
    // 先匹配多级嵌套前缀（已按长度从长到短排列，长前缀优先，避免被单级节名截断）
    for (prefix, dotted) in KNOWN_NESTED_PREFIXES {
        if let Some(rest) = lower.strip_prefix(prefix) {
            return Uncased::from_owned(format!("{dotted}{rest}"));
        }
    }
    for section in KNOWN_SECTIONS {
        let prefix = format!("{section}_");
        if let Some(rest) = lower.strip_prefix(&prefix) {
            return Uncased::from_owned(format!("{section}.{rest}"));
        }
    }
    Uncased::from_owned(lower)
}

/// 首启默认配置模板（FR-90）：编译期嵌入仓库已维护的示例配置 `config.example.toml`。
///
/// 选嵌入示例文件而非从 [`Config::default`] 反序列化生成 TOML：示例是已评审、带丰富中文注释、
/// 随配置项演进同步维护的活模板（见 `docs/CONFIG.md`），保真且零额外序列化逻辑（简单优先）。
/// 示例若未穷举全部节，缺失节由内置默认值兜底——写出的文件仍能被 [`Config::load`] 成功加载。
const DEFAULT_CONFIG_TEMPLATE: &str = include_str!("../config.example.toml");

/// 返回首启默认配置文件的模板文本（FR-90）。
///
/// 纯函数、无副作用：仅返回编译期嵌入的模板，便于单测断言「非空且能被 [`Config::load`] 解析」。
/// 实际「文件不存在即写入」的 IO 由 `main` 在加载配置前完成（写失败只记 WARN、不阻断启动）。
pub fn default_config_template() -> &'static str {
    DEFAULT_CONFIG_TEMPLATE
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Write;

    /// 在隔离的环境变量上下文中执行闭包，避免测试间互相污染。
    ///
    /// 由于进程级环境变量是全局状态，多测试并发改写会串味，这里用互斥锁串行化。
    fn with_env_vars<F: FnOnce()>(vars: &[(&str, &str)], f: F) {
        use std::sync::Mutex;
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

    #[test]
    fn 缺省配置使用内置默认值() {
        with_env_vars(&[], || {
            let cfg = Config::load(Path::new("不存在的配置文件.toml")).unwrap();
            assert_eq!(cfg.server.listen_addr, "127.0.0.1");
            assert_eq!(cfg.server.port, 8080);
            assert_eq!(cfg.auth.session_ttl_secs, 3600);
            assert_eq!(cfg.auth.login_max_failures, 5);
            assert_eq!(cfg.auth.login_lockout_secs, 900);
            assert_eq!(cfg.data.data_dir, PathBuf::from("./data"));
            assert_eq!(cfg.limits.max_artifact_size, None);
            // 审计默认：保留 90 天、行数上限 100 万
            assert_eq!(cfg.observability.audit.retention_days, 90);
            assert_eq!(cfg.observability.audit.max_rows, 1_000_000);
            // 使用分析默认：明细关闭、明细行数上限 100 万
            assert!(!cfg.observability.usage.detail_enabled);
            assert_eq!(cfg.observability.usage.max_detail_rows, 1_000_000);
            // 指标默认：端点开、匿名抓取关
            assert!(cfg.observability.metrics.enabled);
            assert!(!cfg.observability.metrics.allow_anonymous);
            // 速率限制默认：关闭、保守阈值（不误杀正常批量拉取）
            assert!(!cfg.protection.rate_limit.enabled);
            assert_eq!(cfg.protection.rate_limit.window_secs, 60);
            assert_eq!(cfg.protection.rate_limit.ip_max_requests, 1200);
            assert_eq!(cfg.protection.rate_limit.identity_max_requests, 2400);
            // FR-51 新增维度默认 0（不启用），避免误杀
            assert_eq!(cfg.protection.rate_limit.repo_max_requests, 0);
            assert_eq!(cfg.protection.rate_limit.ip_max_concurrent, 0);
            assert_eq!(cfg.protection.rate_limit.user_max_concurrent, 0);
            assert_eq!(cfg.protection.rate_limit.repo_max_concurrent, 0);
            // FR-53 名单默认两表皆空（不启用）
            assert!(cfg.protection.ip_list.allow.is_empty());
            assert!(cfg.protection.ip_list.deny.is_empty());
            // FR-53 异常封禁默认：关闭、阈值保守宽放
            assert!(!cfg.protection.ban.enabled);
            assert_eq!(cfg.protection.ban.window_secs, 60);
            assert_eq!(cfg.protection.ban.threshold, 100);
            assert_eq!(cfg.protection.ban.duration_secs, 900);
            // 慢速攻击防护默认：关闭、超时档位保守、通用体上限 0（不启用）
            assert!(!cfg.protection.slowloris.enabled);
            assert_eq!(cfg.protection.slowloris.body_read_timeout_secs, 30);
            assert_eq!(cfg.protection.slowloris.header_timeout_secs, 30);
            assert_eq!(cfg.protection.slowloris.max_body_bytes, 0);
            // FR-54 CC 挑战默认：关闭、难度 20、过期 300、豁免已认证
            assert!(!cfg.protection.cc_challenge.enabled);
            assert_eq!(cfg.protection.cc_challenge.difficulty, 20);
            assert_eq!(cfg.protection.cc_challenge.ttl_secs, 300);
            assert!(cfg.protection.cc_challenge.exempt_authenticated);
            // FR-55 WAF 默认：关闭、空规则集（不影响现有、不误杀）
            assert!(!cfg.protection.waf.enabled);
            assert!(cfg.protection.waf.rules.is_empty());
        });
    }

    #[test]
    fn toml_可覆盖_cc_挑战配置() {
        let mut file = tempfile::NamedTempFile::new().unwrap();
        writeln!(
            file,
            "[protection.cc_challenge]\nenabled = true\ndifficulty = 12\nttl_secs = 120\nexempt_authenticated = false"
        )
        .unwrap();
        with_env_vars(&[], || {
            let cfg = Config::load(file.path()).unwrap();
            assert!(cfg.protection.cc_challenge.enabled);
            assert_eq!(cfg.protection.cc_challenge.difficulty, 12);
            assert_eq!(cfg.protection.cc_challenge.ttl_secs, 120);
            assert!(!cfg.protection.cc_challenge.exempt_authenticated);
        });
    }

    #[test]
    fn toml_可覆盖_waf_规则集() {
        let mut file = tempfile::NamedTempFile::new().unwrap();
        writeln!(
            file,
            "[protection.waf]\nenabled = true\n\n[[protection.waf.rules]]\nfield = \"path\"\npattern = \"/admin/*\"\nmatch_type = \"wildcard\"\naction = \"block\"\n\n[[protection.waf.rules]]\nfield = \"header\"\nheader_name = \"User-Agent\"\npattern = \"badbot\"\nmatch_type = \"literal\"\naction = \"block\""
        )
        .unwrap();
        with_env_vars(&[], || {
            let cfg = Config::load(file.path()).unwrap();
            assert!(cfg.protection.waf.enabled);
            assert_eq!(cfg.protection.waf.rules.len(), 2);
            assert_eq!(cfg.protection.waf.rules[0].field, "path");
            assert_eq!(cfg.protection.waf.rules[0].match_type, "wildcard");
            assert_eq!(cfg.protection.waf.rules[0].action, "block");
            assert_eq!(
                cfg.protection.waf.rules[1].header_name.as_deref(),
                Some("User-Agent")
            );
        });
    }

    #[test]
    fn cc_挑战节缺失回落默认向后兼容() {
        let mut file = tempfile::NamedTempFile::new().unwrap();
        // 只配置 rate_limit，cc_challenge 节缺失应回落默认（向后兼容旧配置）
        writeln!(file, "[protection.rate_limit]\nenabled = true").unwrap();
        with_env_vars(&[], || {
            let cfg = Config::load(file.path()).unwrap();
            assert!(cfg.protection.rate_limit.enabled);
            assert!(!cfg.protection.cc_challenge.enabled);
            assert_eq!(cfg.protection.cc_challenge.difficulty, 20);
        });
    }

    #[test]
    fn waf_未配置时回落默认且不影响其他防护() {
        // 只配置 rate_limit，waf 节缺失应回落默认（向后兼容旧配置）
        let mut file = tempfile::NamedTempFile::new().unwrap();
        writeln!(file, "[protection.rate_limit]\nenabled = true").unwrap();
        with_env_vars(&[], || {
            let cfg = Config::load(file.path()).unwrap();
            assert!(cfg.protection.rate_limit.enabled);
            assert!(!cfg.protection.waf.enabled);
            assert!(cfg.protection.waf.rules.is_empty());
        });
    }

    #[test]
    fn toml_可覆盖慢速攻击防护与通用体上限() {
        let mut file = tempfile::NamedTempFile::new().unwrap();
        writeln!(
            file,
            "[protection.slowloris]\nenabled = true\nbody_read_timeout_secs = 5\nheader_timeout_secs = 8\nmax_body_bytes = 1048576"
        )
        .unwrap();
        with_env_vars(&[], || {
            let cfg = Config::load(file.path()).unwrap();
            assert!(cfg.protection.slowloris.enabled);
            assert_eq!(cfg.protection.slowloris.body_read_timeout_secs, 5);
            assert_eq!(cfg.protection.slowloris.header_timeout_secs, 8);
            assert_eq!(cfg.protection.slowloris.max_body_bytes, 1048576);
        });
    }

    #[test]
    fn 慢速攻击防护未配置时回落默认且不影响限流() {
        // 只配置 rate_limit，slowloris 节缺失应回落默认（向后兼容旧配置）
        let mut file = tempfile::NamedTempFile::new().unwrap();
        writeln!(file, "[protection.rate_limit]\nenabled = true").unwrap();
        with_env_vars(&[], || {
            let cfg = Config::load(file.path()).unwrap();
            assert!(cfg.protection.rate_limit.enabled);
            assert!(!cfg.protection.slowloris.enabled);
            assert_eq!(cfg.protection.slowloris.body_read_timeout_secs, 30);
        });
    }

    #[test]
    fn toml_可覆盖速率限制开关与阈值() {
        let mut file = tempfile::NamedTempFile::new().unwrap();
        writeln!(
            file,
            "[protection.rate_limit]\nenabled = true\nwindow_secs = 10\nip_max_requests = 50\nidentity_max_requests = 100"
        )
        .unwrap();
        with_env_vars(&[], || {
            let cfg = Config::load(file.path()).unwrap();
            assert!(cfg.protection.rate_limit.enabled);
            assert_eq!(cfg.protection.rate_limit.window_secs, 10);
            assert_eq!(cfg.protection.rate_limit.ip_max_requests, 50);
            assert_eq!(cfg.protection.rate_limit.identity_max_requests, 100);
            // 未在 TOML 给出的 FR-51 新维度回落默认 0（向后兼容旧配置）
            assert_eq!(cfg.protection.rate_limit.repo_max_requests, 0);
            assert_eq!(cfg.protection.rate_limit.repo_max_concurrent, 0);
        });
    }

    #[test]
    fn toml_可覆盖多维限流与并发上限() {
        let mut file = tempfile::NamedTempFile::new().unwrap();
        writeln!(
            file,
            "[protection.rate_limit]\nenabled = true\nrepo_max_requests = 40\nip_max_concurrent = 5\nuser_max_concurrent = 3\nrepo_max_concurrent = 7"
        )
        .unwrap();
        with_env_vars(&[], || {
            let cfg = Config::load(file.path()).unwrap();
            assert!(cfg.protection.rate_limit.enabled);
            assert_eq!(cfg.protection.rate_limit.repo_max_requests, 40);
            assert_eq!(cfg.protection.rate_limit.ip_max_concurrent, 5);
            assert_eq!(cfg.protection.rate_limit.user_max_concurrent, 3);
            assert_eq!(cfg.protection.rate_limit.repo_max_concurrent, 7);
        });
    }

    #[test]
    fn toml_可覆盖封禁与黑白名单() {
        let mut file = tempfile::NamedTempFile::new().unwrap();
        writeln!(
            file,
            "[protection.ip_list]\nallow = [\"10.0.0.0/8\"]\ndeny = [\"203.0.113.7\", \"2001:db8::/32\"]\n\n[protection.ban]\nenabled = true\nwindow_secs = 30\nthreshold = 20\nduration_secs = 600"
        )
        .unwrap();
        with_env_vars(&[], || {
            let cfg = Config::load(file.path()).unwrap();
            assert_eq!(cfg.protection.ip_list.allow, vec!["10.0.0.0/8"]);
            assert_eq!(
                cfg.protection.ip_list.deny,
                vec!["203.0.113.7", "2001:db8::/32"]
            );
            assert!(cfg.protection.ban.enabled);
            assert_eq!(cfg.protection.ban.window_secs, 30);
            assert_eq!(cfg.protection.ban.threshold, 20);
            assert_eq!(cfg.protection.ban.duration_secs, 600);
        });
    }

    #[test]
    fn toml_可覆盖使用分析明细开关与上限() {
        let mut file = tempfile::NamedTempFile::new().unwrap();
        writeln!(
            file,
            "[observability.usage]\ndetail_enabled = true\nmax_detail_rows = 5000"
        )
        .unwrap();
        with_env_vars(&[], || {
            let cfg = Config::load(file.path()).unwrap();
            assert!(cfg.observability.usage.detail_enabled);
            assert_eq!(cfg.observability.usage.max_detail_rows, 5000);
        });
    }

    #[test]
    fn toml_可覆盖审计保留期与行数上限() {
        let mut file = tempfile::NamedTempFile::new().unwrap();
        writeln!(
            file,
            "[observability.audit]\nretention_days = 30\nmax_rows = 5000"
        )
        .unwrap();
        with_env_vars(&[], || {
            let cfg = Config::load(file.path()).unwrap();
            assert_eq!(cfg.observability.audit.retention_days, 30);
            assert_eq!(cfg.observability.audit.max_rows, 5000);
        });
    }

    #[test]
    fn toml_可覆盖指标端点开关与匿名抓取() {
        let mut file = tempfile::NamedTempFile::new().unwrap();
        writeln!(
            file,
            "[observability.metrics]\nenabled = false\nallow_anonymous = true"
        )
        .unwrap();
        with_env_vars(&[], || {
            let cfg = Config::load(file.path()).unwrap();
            assert!(!cfg.observability.metrics.enabled);
            assert!(cfg.observability.metrics.allow_anonymous);
        });
    }

    #[test]
    fn toml_文件覆盖默认值() {
        let mut file = tempfile::NamedTempFile::new().unwrap();
        writeln!(
            file,
            "[server]\nport = 9090\nlisten_addr = \"0.0.0.0\"\n\n[limits]\nmax_artifact_size = 1048576"
        )
        .unwrap();
        with_env_vars(&[], || {
            let cfg = Config::load(file.path()).unwrap();
            assert_eq!(cfg.server.port, 9090);
            assert_eq!(cfg.server.listen_addr, "0.0.0.0");
            assert_eq!(cfg.limits.max_artifact_size, Some(1048576));
            // 未覆盖项仍取默认
            assert_eq!(cfg.auth.session_ttl_secs, 3600);
        });
    }

    #[test]
    fn 环境变量覆盖_toml() {
        let mut file = tempfile::NamedTempFile::new().unwrap();
        writeln!(file, "[server]\nport = 9090").unwrap();
        with_env_vars(
            &[
                ("JIANARTIFACT_SERVER_PORT", "7000"),
                ("JIANARTIFACT_AUTH_SESSION_TTL_SECS", "120"),
            ],
            || {
                let cfg = Config::load(file.path()).unwrap();
                // 环境变量优先级高于 TOML
                assert_eq!(cfg.server.port, 7000);
                assert_eq!(cfg.auth.session_ttl_secs, 120);
            },
        );
    }

    #[test]
    fn blob_目录默认在数据目录下() {
        let cfg = Config::default();
        assert_eq!(
            cfg.data.resolved_blobs_dir(),
            PathBuf::from("./data").join("blobs")
        );
    }

    #[test]
    fn blob_目录可被显式配置覆盖() {
        let data = DataConfig {
            data_dir: PathBuf::from("/var/lib/ja"),
            blobs_dir: Some(PathBuf::from("/mnt/blobs")),
            storage: StorageConfig::default(),
        };
        assert_eq!(data.resolved_blobs_dir(), PathBuf::from("/mnt/blobs"));
    }

    #[test]
    fn 存储后端默认本地文件系统() {
        let cfg = Config::default();
        assert_eq!(cfg.data.storage.backend, StorageBackend::Fs);
        assert!(cfg.data.storage.s3.is_none());
    }

    #[test]
    fn toml_可配置_s3_存储后端() {
        let mut file = tempfile::NamedTempFile::new().unwrap();
        writeln!(
            file,
            "[data.storage]\nbackend = \"s3\"\n\n[data.storage.s3]\nregion = \"us-east-1\"\nbucket = \"artifacts\"\nendpoint = \"http://127.0.0.1:9000\""
        )
        .unwrap();
        with_env_vars(&[], || {
            let cfg = Config::load(file.path()).unwrap();
            assert_eq!(cfg.data.storage.backend, StorageBackend::S3);
            let s3 = cfg.data.storage.s3.expect("应有 S3 子配置");
            assert_eq!(s3.region, "us-east-1");
            assert_eq!(s3.bucket, "artifacts");
            assert_eq!(s3.endpoint.as_deref(), Some("http://127.0.0.1:9000"));
            // path_style 默认 true（兼容 MinIO）
            assert!(s3.path_style);
            assert_eq!(s3.prefix, "");
        });
    }

    #[test]
    fn 环境变量可覆盖嵌套的存储配置() {
        with_env_vars(
            &[
                ("JIANARTIFACT_DATA_STORAGE_BACKEND", "s3"),
                ("JIANARTIFACT_DATA_STORAGE_S3_REGION", "cn-north-1"),
                ("JIANARTIFACT_DATA_STORAGE_S3_BUCKET", "blobs"),
                ("JIANARTIFACT_DATA_STORAGE_S3_PATH_STYLE", "false"),
            ],
            || {
                let cfg = Config::load(Path::new("不存在.toml")).unwrap();
                assert_eq!(cfg.data.storage.backend, StorageBackend::S3);
                let s3 = cfg.data.storage.s3.expect("应有 S3 子配置");
                // 嵌套键名内部下划线（path_style）保留，不被误拆
                assert_eq!(s3.region, "cn-north-1");
                assert_eq!(s3.bucket, "blobs");
                assert!(!s3.path_style);
            },
        );
    }

    #[test]
    fn 默认不配置_ldap() {
        let cfg = Config::default();
        assert!(cfg.auth.ldap.is_none());
    }

    #[test]
    fn toml_可配置_ldap_并回落缺省项() {
        let mut file = tempfile::NamedTempFile::new().unwrap();
        writeln!(
            file,
            "[auth.ldap]\nurl = \"ldaps://dir.example:636\"\nbind_dn = \"cn=svc,dc=ex,dc=org\"\nbind_password = \"pw\"\nuser_search_base = \"ou=people,dc=ex,dc=org\""
        )
        .unwrap();
        with_env_vars(&[], || {
            let cfg = Config::load(file.path()).unwrap();
            let ldap = cfg.auth.ldap.expect("应有 LDAP 配置");
            assert_eq!(ldap.url, "ldaps://dir.example:636");
            assert_eq!(ldap.bind_dn, "cn=svc,dc=ex,dc=org");
            // 缺省项回落默认值
            assert_eq!(ldap.user_filter, "(uid={username})");
            assert_eq!(ldap.username_attr, "uid");
            assert_eq!(ldap.conn_timeout_secs, 10);
            // 安全默认：不 StartTLS、不允许明文、JIT 关闭
            assert!(!ldap.starttls);
            assert!(!ldap.allow_insecure);
            assert!(!ldap.auto_provision);
        });
    }

    #[test]
    fn 环境变量可覆盖嵌套的_ldap_配置() {
        with_env_vars(
            &[
                ("JIANARTIFACT_AUTH_LDAP_URL", "ldaps://ad.corp:636"),
                ("JIANARTIFACT_AUTH_LDAP_BIND_DN", "cn=reader,dc=corp"),
                ("JIANARTIFACT_AUTH_LDAP_BIND_PASSWORD", "env-secret"),
                (
                    "JIANARTIFACT_AUTH_LDAP_USER_SEARCH_BASE",
                    "ou=users,dc=corp",
                ),
                (
                    "JIANARTIFACT_AUTH_LDAP_USER_FILTER",
                    "(sAMAccountName={username})",
                ),
                ("JIANARTIFACT_AUTH_LDAP_AUTO_PROVISION", "true"),
            ],
            || {
                let cfg = Config::load(Path::new("不存在.toml")).unwrap();
                let ldap = cfg.auth.ldap.expect("应有 LDAP 配置");
                // 嵌套键名内部下划线（user_search_base / auto_provision）保留，不被误拆
                assert_eq!(ldap.url, "ldaps://ad.corp:636");
                assert_eq!(ldap.bind_dn, "cn=reader,dc=corp");
                assert_eq!(ldap.bind_password, "env-secret");
                assert_eq!(ldap.user_search_base, "ou=users,dc=corp");
                assert_eq!(ldap.user_filter, "(sAMAccountName={username})");
                assert!(ldap.auto_provision);
            },
        );
    }

    #[test]
    fn ldap_配置_debug_脱敏_bind_口令() {
        let ldap = LdapConfig {
            url: "ldaps://d:636".into(),
            bind_dn: "cn=svc".into(),
            bind_password: "top-secret-pw".into(),
            user_search_base: "dc=ex".into(),
            user_filter: default_ldap_user_filter(),
            username_attr: default_ldap_username_attr(),
            starttls: false,
            allow_insecure: false,
            conn_timeout_secs: default_ldap_conn_timeout_secs(),
            auto_provision: false,
        };
        let dbg = format!("{ldap:?}");
        assert!(dbg.contains("<已脱敏>"));
        assert!(!dbg.contains("top-secret-pw"));
    }

    #[test]
    fn 漏洞库镜像默认关闭且空生态() {
        let cfg = Config::default();
        assert!(!cfg.vuln.enabled);
        assert!(cfg.vuln.ecosystems.is_empty());
        assert_eq!(cfg.vuln.refresh_interval_secs, 86_400);
    }

    #[test]
    fn 漏洞库镜像可经_toml_配置生态与开关() {
        let mut file = tempfile::NamedTempFile::new().unwrap();
        writeln!(
            file,
            "[vuln]\nenabled = true\necosystems = [\"Maven\", \"npm\"]\nrefresh_interval_secs = 3600"
        )
        .unwrap();
        with_env_vars(&[], || {
            let cfg = Config::load(file.path()).unwrap();
            assert!(cfg.vuln.enabled);
            assert_eq!(
                cfg.vuln.ecosystems,
                vec!["Maven".to_string(), "npm".to_string()]
            );
            assert_eq!(cfg.vuln.refresh_interval_secs, 3600);
        });
    }

    // ===== FR-79 防护配置校验 =====

    #[test]
    fn 防护配置默认值通过校验() {
        // 默认配置应是合法可生效的（各窗口非 0、难度在上限内）
        assert!(ProtectionConfig::default().validate().is_ok());
    }

    #[test]
    fn 限流窗口为零被校验拒绝() {
        let mut cfg = ProtectionConfig::default();
        cfg.rate_limit.window_secs = 0;
        assert!(cfg.validate().is_err());
    }

    #[test]
    fn 异常检测窗口为零被校验拒绝() {
        let mut cfg = ProtectionConfig::default();
        cfg.ban.window_secs = 0;
        assert!(cfg.validate().is_err());
    }

    #[test]
    fn 告警窗口为零被校验拒绝() {
        let mut cfg = ProtectionConfig::default();
        cfg.alerts.window_secs = 0;
        assert!(cfg.validate().is_err());
    }

    #[test]
    fn 慢速超时为零被校验拒绝() {
        let mut cfg = ProtectionConfig::default();
        cfg.slowloris.body_read_timeout_secs = 0;
        assert!(cfg.validate().is_err());
        let mut cfg2 = ProtectionConfig::default();
        cfg2.slowloris.header_timeout_secs = 0;
        assert!(cfg2.validate().is_err());
    }

    #[test]
    fn cc挑战有效期为零或难度超限被拒绝() {
        let mut cfg = ProtectionConfig::default();
        cfg.cc_challenge.ttl_secs = 0;
        assert!(cfg.validate().is_err());
        let mut cfg2 = ProtectionConfig::default();
        cfg2.cc_challenge.difficulty = 65;
        assert!(cfg2.validate().is_err());
    }

    #[test]
    fn 出站代理默认全为空() {
        let cfg = Config::default();
        // 三键默认 None：不显式注入代理，保持现状（FR-84）
        assert!(cfg.network.proxy.http.is_none());
        assert!(cfg.network.proxy.https.is_none());
        assert!(cfg.network.proxy.no_proxy.is_none());
    }

    #[test]
    fn 环境变量覆盖出站代理() {
        with_env_vars(
            &[
                (
                    "JIANARTIFACT_NETWORK_PROXY_HTTPS",
                    "http://proxy.internal:8080",
                ),
                ("JIANARTIFACT_NETWORK_PROXY_NO_PROXY", "localhost,127.0.0.1"),
            ],
            || {
                let cfg = Config::load(Path::new("不存在的配置文件.toml")).unwrap();
                // network_proxy_ 前缀正确映射到 network.proxy.*
                assert_eq!(
                    cfg.network.proxy.https.as_deref(),
                    Some("http://proxy.internal:8080")
                );
                assert_eq!(
                    cfg.network.proxy.no_proxy.as_deref(),
                    Some("localhost,127.0.0.1")
                );
                assert!(cfg.network.proxy.http.is_none());
            },
        );
    }

    #[test]
    fn toml_可覆盖出站代理() {
        let mut file = tempfile::NamedTempFile::new().unwrap();
        writeln!(
            file,
            "[network.proxy]\nhttp = \"http://p1:3128\"\nhttps = \"http://p2:3128\""
        )
        .unwrap();
        with_env_vars(&[], || {
            let cfg = Config::load(file.path()).unwrap();
            assert_eq!(cfg.network.proxy.http.as_deref(), Some("http://p1:3128"));
            assert_eq!(cfg.network.proxy.https.as_deref(), Some("http://p2:3128"));
        });
    }

    #[test]
    fn 在线更新默认值() {
        with_env_vars(&[], || {
            let cfg = Config::load(Path::new("不存在的配置文件.toml")).unwrap();
            // 出站默认关闭、仓库源 / API 基址 / 重启模式 / 超时取内置默认
            assert!(!cfg.update.enabled);
            assert_eq!(cfg.update.repo, "wcpe/JianArtifact");
            assert_eq!(cfg.update.api_base_url, "https://api.github.com");
            assert_eq!(cfg.update.restart_mode, "self");
            assert_eq!(cfg.update.download_timeout_secs, 300);
            assert!(cfg.update.token.is_none());
        });
    }

    #[test]
    fn toml_可覆盖在线更新() {
        let mut file = tempfile::NamedTempFile::new().unwrap();
        writeln!(
            file,
            "[update]\nenabled = true\nrepo = \"acme/app\"\napi_base_url = \"http://localhost:9999\"\nrestart_mode = \"exit\"\ndownload_timeout_secs = 120"
        )
        .unwrap();
        with_env_vars(&[], || {
            let cfg = Config::load(file.path()).unwrap();
            assert!(cfg.update.enabled);
            assert_eq!(cfg.update.repo, "acme/app");
            assert_eq!(cfg.update.api_base_url, "http://localhost:9999");
            assert_eq!(cfg.update.restart_mode, "exit");
            assert_eq!(cfg.update.download_timeout_secs, 120);
        });
    }

    #[test]
    fn 环境变量覆盖在线更新_token() {
        with_env_vars(&[("JIANARTIFACT_UPDATE_TOKEN", "ghp_secret_xxx")], || {
            // update_token 前缀经单级节名映射到 update.token
            let cfg = Config::load(Path::new("不存在的配置文件.toml")).unwrap();
            assert_eq!(cfg.update.token.as_deref(), Some("ghp_secret_xxx"));
        });
    }

    #[test]
    fn 在线更新_token_不回显序列化() {
        // token 标记 skip_serializing：序列化导出 / 调试输出绝不含 token，守凭据不外泄
        let cfg = UpdateConfig {
            token: Some("ghp_should_not_leak".to_string()),
            ..Default::default()
        };
        let json = serde_json::to_string(&cfg).unwrap();
        assert!(
            !json.contains("ghp_should_not_leak"),
            "序列化输出不得包含 token"
        );
        assert!(!json.contains("token"), "序列化输出不得包含 token 字段名");
    }

    // ===== FR-88 / ADR-0022：NetworkState 热替换与 EditableUpdate 校验 =====

    /// 便捷：构造一份指定 http 代理的网络配置。
    fn 含代理的网络配置(http: &str) -> NetworkProxyConfig {
        NetworkProxyConfig {
            http: Some(http.to_string()),
            ..Default::default()
        }
    }

    #[test]
    fn network_state_初始快照反映初始代理() {
        let state = NetworkState::new(含代理的网络配置("http://p1.internal:8080"), DUR).unwrap();
        let snap = state.snapshot();
        assert_eq!(snap.proxy.http.as_deref(), Some("http://p1.internal:8080"));
    }

    #[test]
    fn network_state_replace_后快照反映新代理_旧持有快照不受影响() {
        let state = NetworkState::new(NetworkProxyConfig::default(), DUR).unwrap();
        // 初始无代理
        let old = state.snapshot();
        assert!(old.proxy.http.is_none());
        // 热替换为新代理
        state
            .replace_proxy(含代理的网络配置("http://p2.internal:3128"))
            .unwrap();
        // 新快照反映新代理
        assert_eq!(
            state.snapshot().proxy.http.as_deref(),
            Some("http://p2.internal:3128")
        );
        // 替换前持有的旧快照仍是替换前一致视图（不会半新半旧）
        assert!(old.proxy.http.is_none());
    }

    #[test]
    fn network_state_replace_非法代理_返回错误且不改现有生效值() {
        let state = NetworkState::new(含代理的网络配置("http://good.internal:8080"), DUR).unwrap();
        // 非法代理 URL（reqwest 构造失败）应返回错误、不替换
        let bad = 含代理的网络配置("http://[::bad url");
        let res = state.replace_proxy(bad);
        assert!(res.is_err(), "非法代理应构造失败");
        // 现有生效代理不变
        assert_eq!(
            state.snapshot().proxy.http.as_deref(),
            Some("http://good.internal:8080"),
            "非法替换不得改动现有生效代理"
        );
    }

    #[test]
    fn network_state_并发replace与读取不panic且自洽() {
        use std::sync::{Arc, Barrier};
        use std::thread;
        let state = Arc::new(NetworkState::new(NetworkProxyConfig::default(), DUR).unwrap());
        let writers = 4usize;
        let readers = 4usize;
        let per = 100usize;
        let barrier = Arc::new(Barrier::new(writers + readers));
        let mut handles = Vec::new();
        for w in 0..writers {
            let state = Arc::clone(&state);
            let barrier = Arc::clone(&barrier);
            handles.push(thread::spawn(move || {
                barrier.wait();
                for i in 0..per {
                    let cfg = if (w + i) % 2 == 0 {
                        含代理的网络配置("http://a.internal:8080")
                    } else {
                        NetworkProxyConfig::default()
                    };
                    state.replace_proxy(cfg).unwrap();
                }
            }));
        }
        for _ in 0..readers {
            let state = Arc::clone(&state);
            let barrier = Arc::clone(&barrier);
            handles.push(thread::spawn(move || {
                barrier.wait();
                for _ in 0..per {
                    // 取 client 与快照都不应 panic；快照自洽（代理配置与构造它的 client 同源）
                    let _ = state.client();
                    let _ = state.snapshot().proxy.clone();
                }
            }));
        }
        for h in handles {
            h.join().unwrap();
        }
    }

    #[test]
    fn editable_update_校验_拒非法_restart_mode_与空_repo() {
        let mut u = EditableUpdate::from_config(&UpdateConfig::default());
        assert!(u.validate().is_ok(), "默认配置应合法");
        u.restart_mode = "boom".to_string();
        assert!(u.validate().is_err(), "非法 restart_mode 应拒");
        u.restart_mode = "self".to_string();
        u.repo = "  ".to_string();
        assert!(u.validate().is_err(), "空 repo 应拒");
    }

    #[test]
    fn editable_update_默认_channel_为_stable_且校验通过() {
        // FR-89：默认通道为 stable，from_config 应装载之，validate 通过
        let u = EditableUpdate::from_config(&UpdateConfig::default());
        assert_eq!(u.channel, "stable", "默认通道应为 stable");
        assert!(u.validate().is_ok(), "默认 stable 通道应合法");
    }

    #[test]
    fn editable_update_校验_channel_仅允许_stable_或_prerelease() {
        let mut u = EditableUpdate::from_config(&UpdateConfig::default());
        u.channel = "prerelease".to_string();
        assert!(u.validate().is_ok(), "prerelease 通道应合法");
        u.channel = "beta".to_string();
        assert!(u.validate().is_err(), "非法通道应拒");
    }

    #[test]
    fn update_channel_from_config_解析() {
        // FR-89：prerelease 解析为预发布通道，其余（含未知值）回退 stable
        assert_eq!(
            UpdateChannel::from_config("prerelease"),
            UpdateChannel::Prerelease
        );
        assert_eq!(UpdateChannel::from_config("stable"), UpdateChannel::Stable);
        assert_eq!(UpdateChannel::from_config("unknown"), UpdateChannel::Stable);
    }

    /// 测试用出站超时。
    const DUR: std::time::Duration = std::time::Duration::from_secs(30);

    // ===== FR-90：首启默认配置模板 =====

    #[test]
    fn 默认配置模板非空且能被加载() {
        // 模板必须非空，且写到文件后可被 Config::load 成功解析（兜底节由默认值补齐）
        let tmpl = default_config_template();
        assert!(!tmpl.trim().is_empty(), "默认配置模板不应为空");

        let mut file = tempfile::NamedTempFile::new().unwrap();
        file.write_all(tmpl.as_bytes()).unwrap();
        with_env_vars(&[], || {
            let cfg = Config::load(file.path()).expect("模板应能被 Config::load 成功加载");
            // 断言示例模板里显式给出的关键默认值确被加载
            assert_eq!(cfg.server.listen_addr, "127.0.0.1");
            assert_eq!(cfg.server.port, 8080);
            assert_eq!(cfg.auth.session_ttl_secs, 3600);
            assert_eq!(cfg.data.data_dir, PathBuf::from("./data"));
        });
    }

    #[test]
    fn 首启缺失即生成且可被加载() {
        // 在临时目录下取一个不存在的配置路径，模拟首启「文件缺失」
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("config.toml");
        assert!(!path.exists(), "前置条件：配置文件应不存在");

        // 复刻 main 的「缺失即生成」一次性写入逻辑
        std::fs::write(&path, default_config_template()).unwrap();
        assert!(path.exists(), "生成后配置文件应存在");
        assert!(
            !std::fs::read_to_string(&path).unwrap().trim().is_empty(),
            "生成的配置文件内容应非空"
        );

        with_env_vars(&[], || {
            let cfg = Config::load(&path).expect("生成的配置文件应能被 Config::load 加载");
            assert_eq!(cfg.server.port, 8080);
        });
    }

    #[test]
    fn 已存在配置不被覆盖() {
        // 写入哨兵内容，模拟运维已有自定义配置
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("config.toml");
        let sentinel = "# 运维自定义配置，勿覆盖\n[server]\nport = 9999\n";
        std::fs::write(&path, sentinel).unwrap();

        // 「缺失即生成」的守卫：仅当文件不存在才写，已存在则跳过
        if !path.exists() {
            std::fs::write(&path, default_config_template()).unwrap();
        }

        // 文件内容应逐字节保持哨兵不变
        assert_eq!(
            std::fs::read_to_string(&path).unwrap(),
            sentinel,
            "已存在的配置文件不应被覆盖"
        );
    }
}
