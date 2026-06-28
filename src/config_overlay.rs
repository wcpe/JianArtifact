//! 动态配置覆盖层（FR-106，ADR-0028）：装配层把「文件默认 ⊕ DB 覆盖 ⊕ env 显式」合并成生效配置。
//!
//! 本模块位于装配层（`main` 与 `config` / `meta` 之上），**`config` 模块本身零 DB 依赖**：
//! 覆盖发生在这里，依赖方向 `api → meta → config` 不变、无环（守 ADR-0028 红线 1）。
//!
//! 三段式优先级（钉死，见 spec §3）：`生效 = env 显式 > DB 覆盖 > 文件默认`。
//! - 文件默认：`Config::load`（默认 → TOML → env）得到的基线。
//! - DB 覆盖（`app_settings`）：覆盖文件默认。
//! - env 显式：最高——某节若有 env 显式给值，则该节**不被 DB 覆盖**（部署环境强约束 > 面板改动）。
//!
//! 白名单（守红线 2「凭据绝不入库」）：只有 [`DYNAMIC_KEYS`] 列出的「非密钥」节可入库 / 被覆盖；
//! 未列出的键一律不写 DB、不参与覆盖（默认拒绝）。凭据（代理账密 / update token / OIDC·LDAP 密钥 /
//! JWT 密钥）与 bootstrap 项（`server.*` / `data.*`）永不入清单。
//!
//! 合并 / 解析为纯函数（输入：文件默认 + env 显式键集合 + DB 覆盖 map；输出：生效 `Config`），
//! 便于穷举测试。解析失败只 WARN + 回落文件默认，不阻断启动（守韧性，spec §5）。

use std::collections::BTreeSet;

use serde::Serialize;
use tracing::warn;

use crate::config::{
    AuditConfig, Config, LimitsConfig, MetricsConfig, MetricsTimeseriesConfig, ProtectionConfig,
    UsageConfig, VulnConfig,
};

/// 环境变量前缀（与 `config::ENV_PREFIX` 同口径；此处装配层独立持有一份常量，避免暴露 config 私有项）。
const ENV_PREFIX: &str = "JIANARTIFACT_";

/// 动态配置白名单：可入库 / 可被 DB 覆盖的「非密钥」节键（点分路径）。
///
/// **默认拒绝**：不在本清单的键一律不写 DB、不参与覆盖。新增节须显式加入本清单方可入库，
/// 防凭据 / bootstrap 误入。各节均为阈值 / 开关 / 名单 / 规则等非密钥项：
/// - `auth` 仅承载三个可调标量（`session_ttl_secs` / `login_max_failures` / `login_lockout_secs`），
///   经专用非密钥视图序列化，OIDC / LDAP 密钥子节**绝不**入库（见 [`AuthTunables`]）。
/// - `update` 仅承载非密钥字段（`enabled` / `repo` / `api_base_url` / `restart_mode` / `channel`），
///   经专用非密钥视图序列化，`token` **绝不**入库（真源 env，见 [`UpdateTunables`]）。
// 网络代理在 app_settings 的落库键（ADR-0030，与 api::settings::PROXY_SETTING_KEY 同值）：
// 代理走专用加密落库与装配层恢复路径、不归通用动态配置白名单，本模块合并时静默跳过它。
const PROXY_PERSIST_KEY: &str = "network.proxy";

pub const DYNAMIC_KEYS: &[&str] = &[
    "limits",
    "protection",
    "observability.audit",
    "observability.usage",
    "observability.metrics",
    "observability.metrics_timeseries",
    "vuln",
    "auth",
    "update",
];

/// `auth` 节可入库的「非密钥」可调标量视图（FR-106）。
///
/// 仅含三个运行时可调标量，**不含** OIDC / LDAP 密钥子节——后者真源是文件 + env、绝不入库
/// （守红线 2）。专用视图保证 `auth` 节序列化入库时不可能带出任何凭据。
#[derive(Debug, Clone, Serialize, serde::Deserialize, PartialEq, Eq)]
pub struct AuthTunables {
    /// Web 会话 / JWT 有效期（秒）。
    pub session_ttl_secs: u64,
    /// 触发锁定的连续登录失败次数。
    pub login_max_failures: u32,
    /// 锁定时长（秒）。
    pub login_lockout_secs: u64,
}

/// `update` 节可入库的「非密钥」字段视图（FR-106，对应 ADR-0022 的 [`EditableUpdate`] 非密钥子集）。
///
/// 仅含五个非密钥字段，**不含** `token`——token 真源是 env、绝不入库（守红线 2）。专用视图保证
/// `update` 节序列化入库时不可能带出 token；落库 / 装载只覆盖这五个字段，token 保持内存槽（文件 / env）。
#[derive(Debug, Clone, Serialize, serde::Deserialize, PartialEq, Eq)]
pub struct UpdateTunables {
    /// 是否启用在线更新（出站开关）。
    pub enabled: bool,
    /// 仓库源（`owner/repo`）。
    pub repo: String,
    /// GitHub API 基址。
    pub api_base_url: String,
    /// 重启模式（`self` / `exit`）。
    pub restart_mode: String,
    /// 更新通道（`stable` / `prerelease`）。
    pub channel: String,
}

impl UpdateTunables {
    /// 从 [`crate::config::EditableUpdate`] 摘取非密钥字段（PATCH 落库用，token 自动剔除）。
    pub fn from_editable(upd: &crate::config::EditableUpdate) -> Self {
        Self {
            enabled: upd.enabled,
            repo: upd.repo.clone(),
            api_base_url: upd.api_base_url.clone(),
            restart_mode: upd.restart_mode.clone(),
            channel: upd.channel.clone(),
        }
    }
}

/// 把（去前缀、小写化的）环境变量键映射为点分配置路径。
///
/// 复刻 `config::map_env_key` 的口径（已知嵌套前缀优先、再单级节名），用于反推「哪些点分路径来自
/// env 显式给值」。与 config 内逻辑保持一致：节名后首个下划线映射为点，键名内部下划线保留。
fn env_key_to_dotted(lower_key: &str) -> String {
    // 已知多级嵌套前缀（长前缀优先），与 config::KNOWN_NESTED_PREFIXES 对齐
    const NESTED: &[(&str, &str)] = &[
        ("data_storage_s3_", "data.storage.s3."),
        ("data_storage_", "data.storage."),
        ("auth_oidc_", "auth.oidc."),
        ("auth_ldap_", "auth.ldap."),
        ("network_proxy_", "network.proxy."),
        (
            "observability_metrics_timeseries_",
            "observability.metrics_timeseries.",
        ),
    ];
    // 已知单级节名，与 config::KNOWN_SECTIONS 对齐
    const SECTIONS: &[&str] = &[
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
    for (prefix, dotted) in NESTED {
        if let Some(rest) = lower_key.strip_prefix(prefix) {
            return format!("{dotted}{rest}");
        }
    }
    for section in SECTIONS {
        let prefix = format!("{section}_");
        if let Some(rest) = lower_key.strip_prefix(&prefix) {
            return format!("{section}.{rest}");
        }
    }
    lower_key.to_string()
}

/// 扫描进程环境变量，收集所有以 `JIANARTIFACT_` 开头的「env 显式给值」点分配置路径集合。
///
/// 装配层在 `Config::load` 后调用一次；与合并逻辑解耦（合并是纯函数、只吃这个集合）。
pub fn collect_env_explicit_keys() -> BTreeSet<String> {
    std::env::vars()
        .filter_map(|(k, _)| {
            let upper = k.to_ascii_uppercase();
            upper
                .strip_prefix(ENV_PREFIX)
                .map(|rest| env_key_to_dotted(&rest.to_ascii_lowercase()))
        })
        .collect()
}

/// 判断某白名单节是否被 env 显式钉住（该节下有任一 env 显式键）。
///
/// 节键 `section`（如 `observability.audit`）被钉住 ⟺ 存在 env 显式点分键等于它、或以 `section.` 为前缀。
fn section_pinned_by_env(section: &str, env_keys: &BTreeSet<String>) -> bool {
    let prefix = format!("{section}.");
    env_keys
        .iter()
        .any(|k| k == section || k.starts_with(&prefix))
}

/// 把单个白名单节的 DB JSON 覆盖应用到生效 `Config`（解析失败只 WARN、保留文件默认）。
///
/// 每节反序列化为对应强类型再整体替换该子树；类型不符 / JSON 损坏即记 WARN 跳过该节、不动现值。
fn apply_section(cfg: &mut Config, key: &str, json: &str) {
    /// 解析一节 JSON 为强类型，失败记 WARN 并返回 None（保留文件默认）。
    fn parse<T: serde::de::DeserializeOwned>(key: &str, json: &str) -> Option<T> {
        match serde_json::from_str::<T>(json) {
            Ok(v) => Some(v),
            Err(e) => {
                warn!(配置节 = key, 原因 = %e, "DB 动态配置解析失败，回落文件默认");
                None
            }
        }
    }
    match key {
        "limits" => {
            if let Some(v) = parse::<LimitsConfig>(key, json) {
                cfg.limits = v;
            }
        }
        "protection" => {
            if let Some(v) = parse::<ProtectionConfig>(key, json) {
                cfg.protection = v;
            }
        }
        "observability.audit" => {
            if let Some(v) = parse::<AuditConfig>(key, json) {
                cfg.observability.audit = v;
            }
        }
        "observability.usage" => {
            if let Some(v) = parse::<UsageConfig>(key, json) {
                cfg.observability.usage = v;
            }
        }
        "observability.metrics" => {
            if let Some(v) = parse::<MetricsConfig>(key, json) {
                cfg.observability.metrics = v;
            }
        }
        "observability.metrics_timeseries" => {
            if let Some(v) = parse::<MetricsTimeseriesConfig>(key, json) {
                cfg.observability.metrics_timeseries = v;
            }
        }
        "vuln" => {
            if let Some(v) = parse::<VulnConfig>(key, json) {
                cfg.vuln = v;
            }
        }
        "auth" => {
            if let Some(v) = parse::<AuthTunables>(key, json) {
                // 只覆盖三个可调标量，OIDC / LDAP 子节（含密钥）保持文件 / env 不动
                cfg.auth.session_ttl_secs = v.session_ttl_secs;
                cfg.auth.login_max_failures = v.login_max_failures;
                cfg.auth.login_lockout_secs = v.login_lockout_secs;
            }
        }
        "update" => {
            if let Some(v) = parse::<UpdateTunables>(key, json) {
                // 只覆盖五个非密钥字段，token 保持文件 / env（真源不动，绝不被 DB 覆盖）
                cfg.update.enabled = v.enabled;
                cfg.update.repo = v.repo;
                cfg.update.api_base_url = v.api_base_url;
                cfg.update.restart_mode = v.restart_mode;
                cfg.update.channel = v.channel;
            }
        }
        // 默认拒绝：非白名单键（含意外混入的凭据 / bootstrap 键）一律忽略、不应用
        other => {
            warn!(
                配置键 = other,
                "DB 中存在非白名单动态配置键，已忽略（默认拒绝）"
            );
        }
    }
}

/// 合并三段式生效配置（纯函数，FR-106 / ADR-0028）。
///
/// 输入：
/// - `file_default`：`Config::load` 得到的文件默认（含 TOML + env 覆盖后的基线）。
/// - `env_keys`：env 显式给值的点分路径集合（由 [`collect_env_explicit_keys`] 采集）。
/// - `db_overlay`：DB `app_settings` 读出的 `(key, value_json)` 列表。
///
/// 输出：生效 `Config`。规则：对每个**白名单**节，若该节未被 env 显式钉住，则用 DB 覆盖之；
/// env 钉住的节保留文件默认（即 env 值），不被 DB 覆盖。非白名单键忽略（默认拒绝）。
pub fn merge_effective_config(
    file_default: Config,
    env_keys: &BTreeSet<String>,
    db_overlay: &[(String, String)],
) -> Config {
    let mut cfg = file_default;
    for (key, json) in db_overlay {
        // 网络代理走专用加密落库 / 装配层恢复路径（ADR-0030），不归本通用白名单管——
        // 在此静默跳过，避免误报「非白名单已忽略（默认拒绝）」吓到用户（代理实际被正确恢复）。
        if key == PROXY_PERSIST_KEY {
            continue;
        }
        // 白名单外的键一律不应用（默认拒绝，防凭据 / bootstrap 误入）
        if !DYNAMIC_KEYS.contains(&key.as_str()) {
            warn!(配置键 = %key, "DB 中存在非白名单动态配置键，已忽略（默认拒绝）");
            continue;
        }
        // env 显式钉住该节时，跳过 DB 覆盖（env > DB）
        if section_pinned_by_env(key, env_keys) {
            continue;
        }
        apply_section(&mut cfg, key, json);
    }
    cfg
}

#[cfg(test)]
mod tests {
    use super::*;

    /// 便捷：把一组点分键收成 env 集合。
    fn env_set(keys: &[&str]) -> BTreeSet<String> {
        keys.iter().map(|s| s.to_string()).collect()
    }

    // ===== env 键反推映射 =====

    #[test]
    fn env_键反推_单级节名() {
        assert_eq!(
            env_key_to_dotted("limits_max_artifact_size"),
            "limits.max_artifact_size"
        );
        assert_eq!(
            env_key_to_dotted("auth_session_ttl_secs"),
            "auth.session_ttl_secs"
        );
    }

    #[test]
    fn env_键反推_嵌套前缀优先() {
        assert_eq!(
            env_key_to_dotted("observability_metrics_timeseries_sample_interval_secs"),
            "observability.metrics_timeseries.sample_interval_secs"
        );
        assert_eq!(
            env_key_to_dotted("network_proxy_https"),
            "network.proxy.https"
        );
    }

    // ===== 节是否被 env 钉住 =====

    #[test]
    fn 节被_env_钉住_前缀匹配() {
        let env = env_set(&["observability.audit.retention_days"]);
        assert!(section_pinned_by_env("observability.audit", &env));
        // 不同节不受影响
        assert!(!section_pinned_by_env("observability.usage", &env));
        assert!(!section_pinned_by_env("limits", &env));
    }

    #[test]
    fn 节被_env_钉住_整节精确匹配() {
        let env = env_set(&["limits"]);
        assert!(section_pinned_by_env("limits", &env));
    }

    // ===== 三层优先级穷举 =====

    #[test]
    fn 无_db_无_env_取文件默认() {
        let file = Config::default();
        let eff = merge_effective_config(file.clone(), &env_set(&[]), &[]);
        assert_eq!(eff.limits.max_artifact_size, None);
        assert_eq!(eff.auth.session_ttl_secs, file.auth.session_ttl_secs);
    }

    #[test]
    fn db_覆盖文件默认_无_env() {
        let file = Config::default();
        // DB 把 limits.max_artifact_size 改为 4096
        let db = vec![(
            "limits".to_string(),
            "{\"max_artifact_size\":4096}".to_string(),
        )];
        let eff = merge_effective_config(file, &env_set(&[]), &db);
        assert_eq!(eff.limits.max_artifact_size, Some(4096));
    }

    #[test]
    fn env_显式钉住时_db_不覆盖() {
        // 文件默认 limits=None；DB 想改 4096；但 env 显式给了 limits.max_artifact_size（=8192 已并入 file）
        let mut file = Config::default();
        file.limits.max_artifact_size = Some(8192); // 模拟 env 已并入文件基线
        let db = vec![(
            "limits".to_string(),
            "{\"max_artifact_size\":4096}".to_string(),
        )];
        let env = env_set(&["limits.max_artifact_size"]);
        let eff = merge_effective_config(file, &env, &db);
        // env 钉住 → 保留文件基线（env 值 8192），DB 的 4096 不生效
        assert_eq!(eff.limits.max_artifact_size, Some(8192));
    }

    #[test]
    fn 非白名单键被默认拒绝_不应用() {
        let file = Config::default();
        // server / data 是 bootstrap 项，绝不应被 DB 覆盖
        let db = vec![
            ("server".to_string(), "{\"port\":1234}".to_string()),
            ("data".to_string(), "{\"data_dir\":\"/evil\"}".to_string()),
        ];
        let eff = merge_effective_config(file.clone(), &env_set(&[]), &db);
        assert_eq!(eff.server.port, file.server.port);
        assert_eq!(eff.data.data_dir, file.data.data_dir);
    }

    #[test]
    fn protection_整节_db_覆盖() {
        let file = Config::default();
        let pc = ProtectionConfig {
            rate_limit: crate::config::RateLimitConfig {
                enabled: true,
                window_secs: 30,
                ..Default::default()
            },
            ..Default::default()
        };
        let json = serde_json::to_string(&pc).unwrap();
        let db = vec![("protection".to_string(), json)];
        let eff = merge_effective_config(file, &env_set(&[]), &db);
        assert!(eff.protection.rate_limit.enabled);
        assert_eq!(eff.protection.rate_limit.window_secs, 30);
    }

    #[test]
    fn auth_节只覆盖三个可调标量_不触碰_oidc_ldap() {
        let mut file = Config::default();
        // 文件已配置 OIDC（含密钥）；DB auth 覆盖不应抹掉它
        file.auth.oidc = Some(crate::config::OidcConfig {
            issuer: "https://idp".into(),
            client_id: "cid".into(),
            client_secret: "secret".into(),
            redirect_uri: "https://cb".into(),
            auto_provision: false,
        });
        let tun = AuthTunables {
            session_ttl_secs: 120,
            login_max_failures: 9,
            login_lockout_secs: 60,
        };
        let db = vec![("auth".to_string(), serde_json::to_string(&tun).unwrap())];
        let eff = merge_effective_config(file, &env_set(&[]), &db);
        assert_eq!(eff.auth.session_ttl_secs, 120);
        assert_eq!(eff.auth.login_max_failures, 9);
        assert_eq!(eff.auth.login_lockout_secs, 60);
        // OIDC 子节（含密钥）原样保留，DB 覆盖只动三个标量
        assert!(eff.auth.oidc.is_some());
        assert_eq!(eff.auth.oidc.unwrap().client_secret, "secret");
    }

    #[test]
    fn db_损坏_json_回落文件默认_不_panic() {
        let file = Config::default();
        let db = vec![("limits".to_string(), "{ 不是合法 json".to_string())];
        let eff = merge_effective_config(file.clone(), &env_set(&[]), &db);
        // 解析失败保留文件默认（None），应用照常起得来
        assert_eq!(eff.limits.max_artifact_size, file.limits.max_artifact_size);
    }

    #[test]
    fn db_类型不符_回落文件默认() {
        let file = Config::default();
        // limits 期望 { max_artifact_size: u64? }，给个数组 → 反序列化失败、回落默认
        let db = vec![("limits".to_string(), "[1,2,3]".to_string())];
        let eff = merge_effective_config(file.clone(), &env_set(&[]), &db);
        assert_eq!(eff.limits.max_artifact_size, file.limits.max_artifact_size);
    }

    #[test]
    fn auth_视图序列化不含凭据字段名() {
        // AuthTunables 仅三个标量，序列化绝不含 oidc / ldap / secret / password 等字段名
        let tun = AuthTunables {
            session_ttl_secs: 3600,
            login_max_failures: 5,
            login_lockout_secs: 900,
        };
        let json = serde_json::to_string(&tun).unwrap();
        for forbidden in [
            "oidc",
            "ldap",
            "secret",
            "password",
            "client_secret",
            "bind_password",
        ] {
            assert!(
                !json.contains(forbidden),
                "auth 视图不得含凭据字段名 {forbidden}：{json}"
            );
        }
    }
}
