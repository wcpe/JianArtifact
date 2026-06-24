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
use jianartifact::auth::{self, BootstrapOutcome, JwtSigner, LoginGuard};
use jianartifact::config::Config;
use jianartifact::format::{ArtifactService, DockerRegistry, FormatRegistry};
use jianartifact::meta::MetaStore;
use jianartifact::proxy::HttpUpstream;
use jianartifact::storage::BlobBackend;

/// 命令行参数。
#[derive(Debug, Parser)]
#[command(name = "jianartifact", about = "轻量级多格式制品库管理器", version)]
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

    // 通用制品机理服务：本地 blob 存储 + reqwest 上游（纯 rustls）+ 单飞缓存
    let upstream = HttpUpstream::new(std::time::Duration::from_secs(
        cfg.proxy.upstream_timeout_secs,
    ))
    .context("初始化上游 HTTP 客户端失败")?;
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
    .with_graceful_shutdown(shutdown_signal())
    .await
    .context("HTTP 服务异常退出")?;

    info!("服务已优雅关闭");
    Ok(())
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

/// 等待 Ctrl+C 信号以触发优雅关闭。
async fn shutdown_signal() {
    if let Err(e) = tokio::signal::ctrl_c().await {
        warn!(错误 = %e, "监听关闭信号失败，将依赖进程退出");
    }
    info!("收到关闭信号，开始优雅停机");
}
