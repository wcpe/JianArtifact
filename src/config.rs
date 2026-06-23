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
/// 环境变量前缀。
const ENV_PREFIX: &str = "JIANARTIFACT_";
/// 已知配置节名。环境变量映射时，仅把节名与键名之间的首个下划线视作嵌套分隔，
/// 键名内部的下划线（如 `session_ttl_secs`）保持原样。
const KNOWN_SECTIONS: &[&str] = &["server", "data", "auth", "limits"];

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
}

impl Default for DataConfig {
    fn default() -> Self {
        Self {
            data_dir: PathBuf::from(DEFAULT_DATA_DIR),
            blobs_dir: None,
        }
    }
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
}

impl Default for AuthConfig {
    fn default() -> Self {
        Self {
            session_ttl_secs: DEFAULT_SESSION_TTL_SECS,
            login_max_failures: DEFAULT_LOGIN_MAX_FAILURES,
            login_lockout_secs: DEFAULT_LOGIN_LOCKOUT_SECS,
        }
    }
}

/// 上传等限制配置。
#[derive(Debug, Clone, Serialize, Deserialize, Default)]
pub struct LimitsConfig {
    /// 单个制品上传大小上限（字节）；为 None 表示不额外限制。超限返回 413。
    #[serde(default)]
    pub max_artifact_size: Option<u64>,
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
    for section in KNOWN_SECTIONS {
        let prefix = format!("{section}_");
        if let Some(rest) = lower.strip_prefix(&prefix) {
            return Uncased::from_owned(format!("{section}.{rest}"));
        }
    }
    Uncased::from_owned(lower)
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
        };
        assert_eq!(data.resolved_blobs_dir(), PathBuf::from("/mnt/blobs"));
    }
}
