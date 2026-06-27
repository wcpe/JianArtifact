//! JianArtifact 二进制入口：加载配置 → 打开 SQLite 并跑迁移 → 首启管理员引导
//! → 构建 axum 路由 → 监听并提供服务。
#![forbid(unsafe_code)]

use std::net::SocketAddr;
use std::path::PathBuf;
use std::sync::Arc;

use anyhow::Context;
use clap::Parser;
use tracing::{info, warn};
use tracing_subscriber::EnvFilter;

use jianartifact::api::{self, AppState};
use jianartifact::auth::{
    self, BootstrapOutcome, JwtSigner, LdapProvider, LdapSettings, LoginGuard, OidcProvider,
    OidcSettings,
};
use jianartifact::config::Config;
use jianartifact::format::{ArtifactService, DockerRegistry, FormatRegistry};
use jianartifact::meta::MetaStore;
use jianartifact::proxy::HttpUpstream;
use jianartifact::storage::BlobBackend;
use jianartifact::vuln::{self, HttpMirrorSource, VulnMirror};

/// 命令行参数。
#[derive(Debug, Parser)]
// version 取 build_version()：优先 CI 注入的完整版本串（含 prerelease dev.N.sha），回退 CARGO_PKG_VERSION
#[command(name = "jianartifact", about = "轻量级多格式制品库管理器", version = jianartifact::version::build_version())]
struct Cli {
    /// 配置文件路径。
    #[arg(long, default_value = "./config.toml")]
    config: PathBuf,
    /// 数据目录；提供时覆盖配置文件中的 data.data_dir。
    #[arg(long)]
    data_dir: Option<PathBuf>,
}

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    init_tracing();

    let cli = Cli::parse();

    // 启动早期清理上次自更新在 Windows 留下的残留旧二进制（FR-85，best-effort）
    jianartifact::update::cleanup_stale_old();

    // 首启缺失即生成默认配置（FR-90）：配置文件不存在时写一份带中文注释的默认配置到该路径，
    // 便于运维拿到单二进制后有现成可编辑的配置；已存在则绝不覆盖。须在加载配置之前完成。
    ensure_default_config(&cli.config);

    // 加载配置（默认值 → TOML → 环境变量覆盖）
    let mut cfg = Config::load(&cli.config)
        .with_context(|| format!("加载配置失败: {}", cli.config.display()))?;
    // 命令行 --data-dir 优先级最高，覆盖配置
    if let Some(dir) = cli.data_dir {
        cfg.data.data_dir = dir;
    }
    info!(配置文件 = %cli.config.display(), "配置加载完成");

    // 确保数据目录与 blob 目录存在
    let data_dir = cfg.data.data_dir.clone();
    let blobs_dir = cfg.data.resolved_blobs_dir();
    tokio::fs::create_dir_all(&data_dir)
        .await
        .with_context(|| format!("创建数据目录失败: {}", data_dir.display()))?;

    // 打开元数据库并跑迁移
    let db_path = cfg.data.database_path();
    let meta = MetaStore::open(&db_path)
        .await
        .with_context(|| format!("打开元数据库失败: {}", db_path.display()))?;
    info!(数据库 = %db_path.display(), "元数据库就绪");

    // 初始化 blob 存储：按配置选 fs（默认）/ s3 后端；S3 临时文件中转目录在数据目录下
    let s3_tmp_dir = data_dir.join("s3-tmp");
    let store = BlobBackend::from_config(&cfg.data.storage, &blobs_dir, &s3_tmp_dir)
        .await
        .context("初始化 blob 存储失败")?;
    match &store {
        BlobBackend::Fs(_) => {
            info!(blob目录 = %blobs_dir.display(), "blob 存储就绪（本地文件系统）")
        }
        #[cfg(feature = "s3")]
        BlobBackend::S3(_) => info!("blob 存储就绪（S3 兼容对象存储）"),
    }

    // 首启管理员引导（仅空库触发）
    bootstrap_and_log(&meta).await?;

    // 初始化 JWT 签名器（密钥真源在数据目录的 .jwt_secret，无则生成、绝不入库不进日志）
    let jwt = JwtSigner::from_data_dir(&data_dir, cfg.auth.session_ttl_secs)
        .context("初始化 JWT 签名密钥失败")?;
    info!("JWT 会话签名器就绪");

    // 登录暴力破解防护守卫（进程内存计数）
    let login_guard = Arc::new(LoginGuard::new(
        cfg.auth.login_max_failures,
        cfg.auth.login_lockout_secs,
    ));

    // 运行时可编辑设置热替换槽（FR-88，ADR-0022）：以 [network.proxy] + [update] 文件 / env 配置装载初值，
    // 收拢出站网络代理（含据其构造的 reqwest::Client）与在线更新可调字段。全部出站点与在线更新端点经本槽
    // 取当前值；管理端 PATCH /api/v1/settings 锁外重建、原子换槽即时生效、无须重启。出站 client 超时沿用
    // 上游回源口径。
    let settings = Arc::new(
        jianartifact::config::EditableSettings::new(
            cfg.network.proxy.clone(),
            std::time::Duration::from_secs(cfg.proxy.upstream_timeout_secs),
            &cfg.update,
        )
        .map_err(|e| anyhow::anyhow!(e))
        .context("初始化运行时可编辑设置槽失败")?,
    );

    // 通用制品机理服务：本地 blob 存储 + reqwest 上游（纯 rustls，经热替换槽取当前 client）+ 单飞缓存
    let upstream = HttpUpstream::with_network_state(settings.network.clone());
    let artifacts = Arc::new(ArtifactService::new(store.clone(), meta.clone(), upstream));
    // 格式注册表：注册已实现格式（Raw、Maven、npm、Docker），其余格式由后续批次接入
    let formats = Arc::new(FormatRegistry::with_builtin());
    // Docker Registry v2 存储服务：上传会话临时文件落数据目录下的 uploads 子目录
    let docker = Arc::new(
        DockerRegistry::new(
            store.clone(),
            meta.clone(),
            data_dir.join("uploads"),
            cfg.limits.max_artifact_size,
        )
        .await
        .context("初始化 Docker Registry 服务失败")?,
    );
    info!("制品机理与格式注册表就绪");

    // 审计日志（FR-31，ADR-0015）：建有界 channel，启动批量写入与保留期轮转后台任务。
    // 主路径只非阻塞投递；写入 / 轮转失败只记 WARN，不影响业务。
    let (audit, audit_rx) = api::audit_channel();
    api::spawn_audit_writer(meta.clone(), audit_rx);
    api::spawn_audit_retention(
        meta.clone(),
        cfg.observability.audit.retention_days,
        cfg.observability.audit.max_rows,
    );
    info!(
        保留天数 = cfg.observability.audit.retention_days,
        行数上限 = cfg.observability.audit.max_rows,
        "审计日志采集与保留期轮转已就绪"
    );

    // 使用分析采集（FR-57，ADR-0009）：建有界 channel，启动聚合写入与明细兜底裁剪后台任务。
    // 聚合计数始终采集；明细按配置开关落库。主路径只非阻塞采集；写入失败只记 WARN，不影响业务。
    // 数据落本地、默认不外发、不向外部遥测 phone-home（本批不做外部导出）。
    let detail_enabled = cfg.observability.usage.detail_enabled;
    let (usage, usage_rx) = api::usage_channel();
    api::spawn_usage_writer(meta.clone(), usage_rx, detail_enabled);
    if detail_enabled {
        api::spawn_usage_pruner(meta.clone(), cfg.observability.usage.max_detail_rows);
    }
    info!(
        明细开启 = detail_enabled,
        明细行数上限 = cfg.observability.usage.max_detail_rows,
        "使用分析采集已就绪（数据本机内部、默认不外发）"
    );

    // 防护监控与阈值告警（FR-56，ADR-0017）：建有界 channel，启动告警写入与行数兜底裁剪后台任务，
    // 构造进程内告警评估器随状态共享。主路径只在防护命中点非阻塞累加 / 投递；写入失败只记 WARN，不影响业务。
    // 告警是本机内部数据：只落本地 SQLite、不外发、不内置外发型通知。
    let (alerts, alert_rx) = api::alert_channel();
    api::spawn_alert_writer(meta.clone(), alert_rx);
    api::spawn_alert_pruner(meta.clone(), cfg.protection.alerts.max_rows);
    let alert_engine = Arc::new(api::AlertEngine::new(alerts.clone()));
    if cfg.protection.alerts.enabled {
        info!(
            窗口秒 = cfg.protection.alerts.window_secs,
            行数上限 = cfg.protection.alerts.max_rows,
            "防护阈值告警已启用（窗内各维度达阈值即告警，数据本机内部、不外发）"
        );
    } else {
        info!("防护阈值告警未启用，跳过（仍可经 /metrics 与状态端点观测防护计数）");
    }

    // 漏洞库离线镜像（FR-70，ADR-0012）：默认关闭，启用时后台周期下载公开漏洞数据落本地库。
    // 仅镜像/落库，不做制品坐标匹配（FR-71）；下载公开数据集整包，不外发本机制品坐标。
    let _vuln_refresh = if cfg.vuln.enabled {
        // 持共享出站网络热替换槽，后台周期刷新每次下载取当前 client（运行时 PATCH 改代理后下次刷新即生效）
        let source = HttpMirrorSource::with_network_state(
            cfg.vuln.source_base_url.clone(),
            settings.network.clone(),
        );
        let mirror = Arc::new(VulnMirror::new(meta.clone(), source, &data_dir));
        vuln::spawn_refresh_loop(mirror, cfg.vuln.clone())
    } else {
        info!("漏洞库离线镜像未启用，跳过");
        None
    };

    // Prometheus 指标（FR-32，ADR-0015）：启用时安装进程内 recorder（pull 模型，仅 /metrics 抓取时渲染）。
    // 安装失败（如同进程重复安装）记 WARN 后降级为不挂端点，不阻断启动。
    let metrics = if cfg.observability.metrics.enabled {
        match api::install_recorder() {
            Ok(handle) => {
                info!(
                    允许匿名抓取 = cfg.observability.metrics.allow_anonymous,
                    "Prometheus 指标端点已就绪：GET /metrics"
                );
                Some(handle)
            }
            Err(e) => {
                warn!(原因 = %e, "安装 Prometheus recorder 失败，指标端点降级关闭");
                None
            }
        }
    } else {
        info!("Prometheus 指标端点未启用，跳过");
        None
    };

    // 基础速率限制（FR-33，ADR-0008）：进程内限流器随状态共享，按配置开关在中间件生效。
    // 默认关闭、阈值保守，避免误杀正常批量拉取；仅应用层（L7），L3/L4 交前置设施。
    let rate_limiter = Arc::new(api::RateLimiter::new());
    if cfg.protection.rate_limit.enabled {
        info!(
            窗口秒 = cfg.protection.rate_limit.window_secs,
            单IP上限 = cfg.protection.rate_limit.ip_max_requests,
            单身份上限 = cfg.protection.rate_limit.identity_max_requests,
            "基础速率限制已启用（IP / 身份维度）"
        );
    } else {
        info!("基础速率限制未启用，跳过");
    }

    // CC 挑战（FR-54，ADR-0008）：挑战签名器复用 JWT 派生的域分隔子密钥（不直接泄露 JWT 密钥本体），
    // 随状态共享、按配置开关在中间件生效。默认关闭、默认豁免已认证客户端，仅对匿名可疑流量要求 PoW。
    let cc_challenger = Arc::new(api::CcChallenger::new(&jwt.derive_key(b"cc-challenge")));
    if cfg.protection.cc_challenge.enabled {
        warn!(
            难度位 = cfg.protection.cc_challenge.difficulty,
            过期秒 = cfg.protection.cc_challenge.ttl_secs,
            豁免已认证 = cfg.protection.cc_challenge.exempt_authenticated,
            "CC 挑战已启用（PoW 工作量证明）——注意：正常包管理器 CLI 不会解 PoW，匿名拉取将被挑战拦截"
        );
    } else {
        info!("CC 挑战未启用，跳过");
    }

    // OIDC 认证 provider（FR-34，ADR-0016）：仅当配置了 `[auth.oidc]` 才实例化（未配置即不存在）。
    // client_secret 真源 env / 配置，绝不入库 / 进日志；复用纯 rustls 的 reqwest 客户端。
    let oidc = if let Some(oidc_cfg) = cfg.auth.oidc.clone() {
        // 持共享出站网络热替换槽，登录出站取当前 client（运行时 PATCH 改代理即时生效；FR-88，ADR-0022）
        let provider = OidcProvider::new(
            OidcSettings {
                issuer: oidc_cfg.issuer,
                client_id: oidc_cfg.client_id,
                client_secret: oidc_cfg.client_secret,
                redirect_uri: oidc_cfg.redirect_uri,
                auto_provision: oidc_cfg.auto_provision,
            },
            settings.network.clone(),
        );
        info!(
            JIT开通 = oidc_cfg.auto_provision,
            "OIDC 认证集成已启用（授权码流 + PKCE）"
        );
        Some(Arc::new(provider))
    } else {
        info!("OIDC 认证集成未配置，跳过");
        None
    };

    // LDAP 认证 provider（FR-35，ADR-0016）：仅当配置了 `[auth.ldap]` 才实例化（未配置即不存在）。
    // bind 口令真源 env / 配置，绝不入库 / 进日志；连接走 LDAPS / StartTLS（rustls），默认不接受明文。
    let ldap = if let Some(ldap_cfg) = cfg.auth.ldap.clone() {
        let provider = LdapProvider::new(LdapSettings {
            url: ldap_cfg.url,
            bind_dn: ldap_cfg.bind_dn,
            bind_password: ldap_cfg.bind_password,
            user_search_base: ldap_cfg.user_search_base,
            user_filter: ldap_cfg.user_filter,
            username_attr: ldap_cfg.username_attr,
            starttls: ldap_cfg.starttls,
            allow_insecure: ldap_cfg.allow_insecure,
            conn_timeout_secs: ldap_cfg.conn_timeout_secs,
        });
        info!(
            JIT开通 = ldap_cfg.auto_provision,
            StartTLS = ldap_cfg.starttls,
            允许明文 = ldap_cfg.allow_insecure,
            "LDAP 认证集成已启用（bind 校验）"
        );
        Some(Arc::new(provider))
    } else {
        info!("LDAP 认证集成未配置，跳过");
        None
    };

    // 可配置 WAF 规则引擎（FR-55，ADR-0008）：规则在构建防护热替换槽时按 [protection.waf] 编译一次
    // （正则预编译、非法规则记 WARN 跳过、不阻断启动）；中间件按请求模式匹配阻断 / 放行。默认空规则集 + 关闭。
    if cfg.protection.waf.enabled && !cfg.protection.waf.rules.is_empty() {
        info!(
            规则数 = cfg.protection.waf.rules.len(),
            "WAF 规则引擎已启用（请求模式匹配与阻断）"
        );
    } else {
        info!("WAF 规则引擎未启用或规则集为空，跳过");
    }

    // 运行时防护配置热替换槽（FR-79，扩展 ADR-0008）：以 [protection.*] 文件配置装载当前生效快照
    // （含 IP 名单匹配器、WAF 规则集等派生态）；管理端 PATCH 经 protection.replace 即时生效、无须重启。
    // 防护配置真源自此为本槽，中间件不再读 config.protection。
    let protection = Arc::new(api::ProtectionState::new(cfg.protection.clone()));

    // 在线更新重启句柄（FR-85，ADR-0021）：随 AppState 共享；自更新替换成功后置位重启请求并
    // 触发优雅停机，serve 返回后据此拉起新进程或退出。
    let restart = Arc::new(jianartifact::update::RestartHandle::default());

    // 主机 / 系统监控采样器（FR-98，ADR-0023）：单进程共享一份 sysinfo::System（refresh 需 &mut），
    // 经 Mutex 串行化按请求采样；纯本机内部采样、不外发、不落库、不后台轮询。
    let host_system = Arc::new(tokio::sync::Mutex::new(sysinfo::System::new()));

    // 构建路由与共享状态
    let state = AppState {
        config: Arc::new(cfg.clone()),
        meta,
        store,
        jwt,
        login_guard,
        artifacts,
        formats,
        docker: Some(docker),
        audit,
        usage,
        metrics,
        rate_limiter,
        oidc,
        oidc_flows: Arc::new(api::OidcFlowStore::new()),
        ldap,
        // FR-79：运行时防护配置热替换槽（含 IP 名单匹配器、WAF 规则集等派生态），PATCH 即时生效
        protection,
        // FR-53：封禁登记表为空进程内内存（重启即清，配置热替换不清空已积累的封禁 / 信号计数）
        ban_registry: Arc::new(api::BanRegistry::new()),
        // FR-54：CC 挑战签名器（密钥复用 JWT 派生子密钥，无状态签发 / 校验 PoW 挑战）
        cc_challenger,
        // FR-56：防护告警投递端 + 进程内告警评估器（窗内各维度达阈值即告警并异步落库）
        alerts,
        alert_engine,
        // FR-85：在线更新重启句柄（自更新替换成功后触发优雅停机 + 重启）
        restart: restart.clone(),
        // FR-88：运行时可编辑设置热替换槽（出站网络代理 + 在线更新可调字段），PATCH 即时生效
        settings,
        // FR-98：主机 / 系统监控采样器（按请求采样 CPU / 内存 / 磁盘 / uptime）
        host_system,
    };
    let app = api::build_router(state);

    // 监听并提供服务
    let bind_addr = format!("{}:{}", cfg.server.listen_addr, cfg.server.port);
    let listener = tokio::net::TcpListener::bind(&bind_addr)
        .await
        .with_context(|| format!("监听地址失败: {bind_addr}"))?;
    info!(监听 = %bind_addr, "服务启动，开始接受请求");

    // 携带连接信息以便登录防护按来源 IP 计数
    axum::serve(
        listener,
        app.into_make_service_with_connect_info::<SocketAddr>(),
    )
    .with_graceful_shutdown(shutdown_signal(restart.clone()))
    .await
    .context("HTTP 服务异常退出")?;

    info!("服务已优雅关闭");

    // 在线更新（FR-85，ADR-0021，§3.8）：serve 已排空在途请求并释放监听端口，此时若有重启请求则处理：
    // - self：拉起新进程（端口已释放，避免新旧争用）后旧进程退出；
    // - exit：直接退出码 0，交外部进程管理器（systemd / docker）重启。
    if let Some(req) = restart.take() {
        handle_restart(req)?;
    }
    Ok(())
}

/// 据重启请求拉起新进程或退出（FR-85，§3.8）。**真正的拉起进程 + 端口序列需真机验证**。
fn handle_restart(req: jianartifact::update::RestartRequest) -> anyhow::Result<()> {
    use jianartifact::update::RestartMode;
    match req.mode {
        RestartMode::SelfRespawn => {
            info!(二进制 = %req.exe.display(), "自更新：拉起新进程后退出");
            std::process::Command::new(&req.exe)
                .args(&req.argv)
                .spawn()
                .with_context(|| format!("拉起新进程失败: {}", req.exe.display()))?;
            // 拉起成功后正常退出，交还端口给新进程
            std::process::exit(0);
        }
        RestartMode::Exit => {
            info!("自更新：仅退出，交外部进程管理器重启");
            std::process::exit(0);
        }
    }
}

/// 首启缺失即生成默认配置文件（FR-90）。
///
/// 配置文件不存在时，写入 `config` 层嵌入的带中文注释默认模板并记 INFO；已存在则跳过、绝不覆盖。
/// 写入失败（如目录无权限）只记 WARN、不阻断启动——回落到「文件不存在」语义，后续照常用默认值 + env 加载。
/// 启动期一次性 IO，用同步 `std::fs` 即可（早于大量并发，简单直接）。
fn ensure_default_config(config_path: &std::path::Path) {
    if config_path.exists() {
        return;
    }
    match std::fs::write(config_path, jianartifact::config::default_config_template()) {
        Ok(()) => {
            info!(配置文件 = %config_path.display(), "配置文件缺失，已生成带注释的默认配置，可按需编辑")
        }
        Err(e) => warn!(
            配置文件 = %config_path.display(),
            错误 = %e,
            "生成默认配置文件失败，将回落默认值加载（请检查目录权限）"
        ),
    }
}

/// 初始化分级日志：默认 info，可经 RUST_LOG 调整。
fn init_tracing() {
    let filter = EnvFilter::try_from_default_env().unwrap_or_else(|_| EnvFilter::new("info"));
    tracing_subscriber::fmt().with_env_filter(filter).init();
}

/// 执行首启引导并按结果打印日志（随机口令仅首启打印一次）。
async fn bootstrap_and_log(meta: &MetaStore) -> anyhow::Result<()> {
    match auth::bootstrap_admin(meta)
        .await
        .context("首启管理员引导失败")?
    {
        BootstrapOutcome::AlreadyInitialized => {
            info!("已存在用户，跳过首启管理员引导");
        }
        BootstrapOutcome::CreatedFromEnv { username } => {
            info!(用户名 = %username, "已根据环境变量创建首个管理员");
        }
        BootstrapOutcome::CreatedWithRandomPassword { username, password } => {
            // 随机口令仅首启打印一次，提示运维妥善保管并尽快改密
            warn!(
                用户名 = %username,
                初始口令 = %password,
                "已创建首个管理员并生成随机初始口令，请妥善保管并尽快登录后修改"
            );
        }
    }
    Ok(())
}

/// 等待优雅关闭触发：Ctrl+C 信号，或在线更新置位的重启通知（FR-85，§3.8），任一即停机。
async fn shutdown_signal(restart: Arc<jianartifact::update::RestartHandle>) {
    let ctrl_c = async {
        if let Err(e) = tokio::signal::ctrl_c().await {
            warn!(错误 = %e, "监听关闭信号失败，将依赖进程退出");
        }
    };
    tokio::select! {
        _ = ctrl_c => info!("收到关闭信号，开始优雅停机"),
        _ = restart.notified() => info!("收到在线更新重启请求，开始优雅停机"),
    }
}
