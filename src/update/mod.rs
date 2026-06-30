//! 在线更新编排模块（FR-85，ADR-0021）。
//!
//! 管理员手动触发的完整自更新：查 GitHub 最新稳定 Release、与当前版本比对，按本机 target
//! 下载对应资产、流式校验 sha256、原子替换运行中的二进制并触发自动重启。
//!
//! 分层：`api → update → config`，单向无环。本模块**不依赖 meta**（自更新不碰 DB）。
//! 出站经统一出站客户端 helper（FR-84 / ADR-0020，honor `[network.proxy]`）。
//!
//! 安全：仅 sha256 完整性校验，校验通过才替换、替换是最后一步；token / 凭据绝不进日志 / 错误 / 序列化。
#![forbid(unsafe_code)]

use std::path::{Path, PathBuf};

use digest::Digest;
use serde::{Deserialize, Serialize};
use sha2::Sha256;
use tokio::io::{AsyncRead, AsyncReadExt};

mod restart;
mod source;
mod state;

#[cfg(test)]
mod tests;

pub use crate::config::UpdateChannel;
pub use restart::{ApplyGuard, RestartHandle, RestartRequest};
pub use source::{GithubReleaseSource, Release, ReleaseAsset, ReleaseSource};
pub use state::{
    load_state, now_unix_secs, persist_state, state_path, update_state, CachedCheck, UpdateKind,
    UpdatePhase, UpdateProgress, UpdateState,
};

/// 进度共享态（FR-126）：异步更新 job 持续更新，`GET /update/jobs/{id}` 读取。
pub type ProgressSlot = std::sync::Mutex<UpdateProgress>;

/// 取进度锁（容忍中毒：恢复内部数据继续，不让一次 panic 永久毒死进度查询）。
fn lock_progress(p: &ProgressSlot) -> std::sync::MutexGuard<'_, UpdateProgress> {
    p.lock().unwrap_or_else(|e| e.into_inner())
}

/// 临时下载子目录名（位于数据目录下），存放下载中的资产，校验失败即清理。
const UPDATE_TMP_SUBDIR: &str = "update-tmp";
/// FR-86 三发布目标之一：Linux x86_64（musl 静态链接，不依赖 glibc、跨发行版可跑）。
const TARGET_LINUX_X64: &str = "x86_64-unknown-linux-musl";
/// FR-86 三发布目标之一：Windows x86_64。
const TARGET_WINDOWS_X64: &str = "x86_64-pc-windows-msvc";
/// FR-86 三发布目标之一：macOS aarch64。
const TARGET_MACOS_ARM64: &str = "aarch64-apple-darwin";
/// 资产名前缀（与 FR-86 命名契约一致：`jianartifact-{version}-{target}{ext}`）。
const ASSET_PREFIX: &str = "jianartifact";
/// 压缩包扩展名（FR-138：三平台统一打 zip，避免引入 tar/flate2 新依赖）。
const ARCHIVE_EXT: &str = ".zip";
/// 持久回滚备份后缀（FR-104，ADR-0026）：升级前把当前二进制复制为 `{exe}.rollback.bak`，
/// 作为跨平台一致的单一回滚源；**不被启动清理**（区别于 Windows 临时 `.old`）。
const ROLLBACK_BACKUP_SUFFIX: &str = ".rollback.bak";

/// 在线更新错误类型。
#[derive(Debug, thiserror::Error)]
pub enum UpdateError {
    /// 在线更新未启用（`[update] enabled=false`）：不联网、不应用。
    #[error("在线更新未启用")]
    Disabled,
    /// 当前平台无自更新资产（不在 FR-86 三发布目标内）。
    #[error("当前平台不支持自更新: {0}")]
    UnsupportedPlatform(String),
    /// 版本串非法（无法解析 `major.minor.patch`）。
    #[error("版本串非法: {0}")]
    InvalidVersion(String),
    /// 无可应用的更新（最新版本不高于当前版本）。
    #[error("无可应用的更新: {0}")]
    NoUpdate(String),
    /// Release 中缺少所需资产（二进制或其 `.sha256`）。
    #[error("缺少所需资产: {0}")]
    MissingAsset(String),
    /// 下载内容 sha256 与发布的 `.sha256` 不一致：拒绝替换。
    #[error("下载内容校验和不一致")]
    ChecksumMismatch,
    /// 上游不可达 / 超时 / 返回错误状态（不向调用方泄露内部细节）。
    #[error("上游访问失败: {0}")]
    Upstream(String),
    /// 解析上游响应失败。
    #[error("解析上游响应失败: {0}")]
    Parse(String),
    /// 本地文件系统操作失败（下载落盘 / 替换）。
    #[error("文件操作失败: {0}")]
    Io(String),
    /// 无可回滚的备份版本（FR-104）：从未成功升级过、或备份已缺失。
    #[error("无可回滚的备份版本")]
    NoBackup,
}

/// 更新检查结果（对外响应载体）。
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct UpdateCheck {
    /// 当前运行版本（`CARGO_PKG_VERSION`）。
    pub current_version: String,
    /// 最新稳定版本（`tag_name` 去前导 `v`）。
    pub latest_version: String,
    /// 是否有更新（`latest > current`）。
    pub update_available: bool,
    /// 本机 target 对应资产名。
    pub asset_name: String,
    /// 发布说明（Release `body`）。
    pub notes: String,
}

/// 重启模式（FR-85）。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum RestartMode {
    /// 自拉起：重启后由旧进程拉起新进程再退出。
    SelfRespawn,
    /// 仅退出：交外部进程管理器（systemd / docker）重启。
    Exit,
}

impl RestartMode {
    /// 由配置字符串解析重启模式：未知值回退 `self`（最安全的裸进程默认）。
    pub fn from_config(s: &str) -> Self {
        match s {
            "exit" => RestartMode::Exit,
            _ => RestartMode::SelfRespawn,
        }
    }
}

/// 推导本机发布 target（纯函数，可测）。
///
/// 据 `std::env::consts::{OS, ARCH}` 映射到 FR-86 三发布目标之一；其余组合明确报
/// [`UpdateError::UnsupportedPlatform`]，不静默乱下资产。
pub fn current_target() -> Result<&'static str, UpdateError> {
    resolve_target(std::env::consts::OS, std::env::consts::ARCH)
}

/// target 推导核心（参数化 OS/ARCH，便于跨平台穷举测试）。
pub(crate) fn resolve_target(os: &str, arch: &str) -> Result<&'static str, UpdateError> {
    match (os, arch) {
        ("linux", "x86_64") => Ok(TARGET_LINUX_X64),
        ("windows", "x86_64") => Ok(TARGET_WINDOWS_X64),
        ("macos", "aarch64") => Ok(TARGET_MACOS_ARM64),
        _ => Err(UpdateError::UnsupportedPlatform(format!("{os}/{arch}"))),
    }
}

/// 据 target 推导可执行文件扩展名（Windows 为 `.exe`，其余为空）。
pub(crate) fn target_ext(target: &str) -> &'static str {
    if target == TARGET_WINDOWS_X64 {
        ".exe"
    } else {
        ""
    }
}

/// 推导资产名（纯函数，可测）：`jianartifact-{version}-{target}{ext}`（FR-86 §3.1）。
pub(crate) fn asset_name(version: &str, target: &str) -> String {
    format!(
        "{ASSET_PREFIX}-{version}-{target}{ext}",
        ext = target_ext(target)
    )
}

/// 推导压缩包资产名（纯函数，可测；FR-138）：`jianartifact-{version}-{target}.zip`。
///
/// 三平台统一打 zip，不按平台区分 tar.gz / zip；zip 内含单个可执行文件（`jianartifact{ext}`）。
pub(crate) fn archive_asset_name(version: &str, target: &str) -> String {
    format!("{ASSET_PREFIX}-{version}-{target}{ARCHIVE_EXT}")
}

/// 解析后的三段语义版本（用于比较，纯函数）。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
struct SemVer {
    major: u64,
    minor: u64,
    patch: u64,
}

/// 解析 `major.minor.patch`（去前导 `v`，忽略预发布 / 构建元数据后缀）。
///
/// `/releases/latest` 只返稳定版，故主版本号后的 `-pre` / `+build` 后缀按忽略处理；
/// 三段须为合法整数，否则报 [`UpdateError::InvalidVersion`]。
fn parse_version(raw: &str) -> Result<SemVer, UpdateError> {
    let trimmed = raw.trim();
    let no_v = trimmed.strip_prefix('v').unwrap_or(trimmed);
    // 截断预发布 / 构建元数据后缀（取首个 '-' 或 '+' 之前）
    let core = no_v.split(['-', '+']).next().unwrap_or("").trim();
    let mut parts = core.split('.');
    let mut next = |s: &str| -> Result<u64, UpdateError> {
        parts
            .next()
            .ok_or_else(|| UpdateError::InvalidVersion(raw.to_string()))
            .and_then(|p| {
                p.parse::<u64>()
                    .map_err(|_| UpdateError::InvalidVersion(s.to_string()))
            })
    };
    let major = next(raw)?;
    let minor = next(raw)?;
    let patch = next(raw)?;
    // 多余段（如四段版本）视为非法，避免静默截断
    if parts.next().is_some() {
        return Err(UpdateError::InvalidVersion(raw.to_string()));
    }
    Ok(SemVer {
        major,
        minor,
        patch,
    })
}

/// 比较版本，判定是否有更新（`latest > current`，纯函数，可测）。
///
/// 仅比较 `major.minor.patch` 三段、忽略预发布 / 构建后缀，是 stable 通道的判定口径。
pub(crate) fn is_update_available(current: &str, latest: &str) -> Result<bool, UpdateError> {
    let cur = parse_version(current)?;
    let lat = parse_version(latest)?;
    Ok((lat.major, lat.minor, lat.patch) > (cur.major, cur.minor, cur.patch))
}

/// 归一化版本串用于完整比较：去首尾空白与前导 `v`（prerelease 通道按完整串判定）。
fn normalize_version(raw: &str) -> &str {
    let trimmed = raw.trim();
    trimmed.strip_prefix('v').unwrap_or(trimmed)
}

/// 按更新通道判定是否有更新（纯函数，可测；FR-89 通道分流）。
///
/// - `Stable`：维持 SemVer 三段严格更高语义（忽略预发布 / 构建后缀），仅当核心版本确实更高才更新。
/// - `Prerelease`：dev 预发布常与当前正式版共享核心版本（如 `0.4.0` vs `0.4.0-dev.5.<sha>`），
///   故改按**完整版本串**判定——目标与当前不同即视为可更新 / 可切换；完全相同则无更新。
pub(crate) fn is_update_available_for_channel(
    channel: UpdateChannel,
    current: &str,
    latest: &str,
) -> Result<bool, UpdateError> {
    match channel {
        UpdateChannel::Stable => is_update_available(current, latest),
        UpdateChannel::Prerelease => {
            // 校验两侧均为合法版本串（含预发布后缀），非法即报错、不静默放行
            parse_version(current)?;
            parse_version(latest)?;
            Ok(normalize_version(current) != normalize_version(latest))
        }
    }
}

/// 据当前版本与 Release 组装更新检查结果（纯函数，可测）。
///
/// 推导本机 target 与资产名、按通道比对版本；不要求 Release 中已含资产（检查阶段只看版本）。
/// `channel` 决定版本判定口径（FR-89）：stable 要求 SemVer 严格更高，prerelease 按完整串不同即可更新。
pub fn build_check(
    channel: UpdateChannel,
    current_version: &str,
    release: &Release,
) -> Result<UpdateCheck, UpdateError> {
    let target = current_target()?;
    let latest = release.version();
    let update_available = is_update_available_for_channel(channel, current_version, &latest)?;
    Ok(UpdateCheck {
        current_version: current_version.to_string(),
        latest_version: latest.clone(),
        update_available,
        asset_name: asset_name(&latest, target),
        notes: release.body.clone(),
    })
}

/// 异步检查 job 核心（FR-126）：联网查 Release → 组装 [`UpdateCheck`]，逐阶段写日志、返回结果。
///
/// 仅做检查、不下载 / 不替换；出站经 `source`（已注入代理，honor `[network.proxy]`）。供 `api::update`
/// 的检查 job 后台调用：成功后把结果写进度并留存到状态文件（留存在 handler 层做，守分层）。
pub async fn check_with_progress<S: ReleaseSource>(
    source: &S,
    channel: UpdateChannel,
    current_version: &str,
) -> Result<UpdateCheck, UpdateError> {
    tracing::info!("在线更新：开始联网检查最新发布");
    let release = source.fetch_latest_release(channel).await?;
    let check = build_check(channel, current_version, &release)?;
    tracing::info!(
        最新版本 = %check.latest_version,
        有更新 = check.update_available,
        "在线更新：检查完成"
    );
    Ok(check)
}

/// 二进制替换规划（跨平台，路径推导可单测）。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ReplacePlan {
    /// 当前运行的二进制路径（替换目标）。
    pub current_exe: PathBuf,
    /// 临时新二进制最终落到 exe 同目录的路径（同卷以保 rename 原子）。
    pub staged: PathBuf,
    /// Unix：替换前把旧 exe 复制为该 `.bak`（单步回退兜底）；Windows 为 `None`。
    pub backup: Option<PathBuf>,
    /// Windows：先把运行中 exe 改名为该 `.old`，下次启动清理；Unix 为 `None`。
    pub old: Option<PathBuf>,
}

/// 据当前 exe 路径生成替换规划（纯函数，跨平台可测）。
///
/// 临时新文件落到 exe 同目录（同卷保 rename 原子）；Unix 留 `{exe}.bak`、Windows 留 `{exe}.old`。
pub fn plan_replace(current_exe: &Path) -> ReplacePlan {
    let staged = sibling_with_suffix(current_exe, ".new");
    if cfg!(windows) {
        ReplacePlan {
            current_exe: current_exe.to_path_buf(),
            staged,
            backup: None,
            old: Some(sibling_with_suffix(current_exe, ".old")),
        }
    } else {
        ReplacePlan {
            current_exe: current_exe.to_path_buf(),
            staged,
            backup: Some(sibling_with_suffix(current_exe, ".bak")),
            old: None,
        }
    }
}

/// 自更新管理的临时 / 备份后缀集合（ADR-0032）。
///
/// 用于：① 派生临时 / 备份名前先剥离已有后缀，防 compound（`.bak.bak`）；② 启动清理识别 compound 残留。
/// `.rollback.bak` 须排在 `.bak` 前（更长者优先匹配），避免把它误当作单层 `.bak`。
const MANAGED_SUFFIXES: &[&str] = &[".rollback.bak", ".bak", ".old", ".new"];

/// 把文件名末尾叠加的更新管理后缀（可能多层）全部剥离，得到「规范 exe 名」（ADR-0032）。
///
/// 正常运行时 exe 名不带这些后缀，剥离为恒等；仅当用户手动跑了备份文件（名以 `.bak`/`.rollback.bak`
/// 结尾）再触发更新时生效，使派生的临时 / 备份名收敛到规范名、不再 compound。
fn strip_managed_suffixes(file_name: &std::ffi::OsStr) -> std::ffi::OsString {
    let mut name = file_name.to_string_lossy().into_owned();
    loop {
        let before = name.len();
        for suffix in MANAGED_SUFFIXES {
            if let Some(stripped) = name.strip_suffix(suffix) {
                name.truncate(stripped.len());
                break;
            }
        }
        if name.len() == before {
            break;
        }
    }
    std::ffi::OsString::from(name)
}

/// 在 exe 同目录生成「规范 exe 名 + 后缀」的兄弟路径（同卷以保 rename 原子）。
///
/// 追加后缀前先剥离 exe 名末尾已有的更新管理后缀（ADR-0032），防止在 `.bak` 文件上再叠 `.bak`。
fn sibling_with_suffix(exe: &Path, suffix: &str) -> PathBuf {
    let mut file_name = exe
        .file_name()
        .map(strip_managed_suffixes)
        .unwrap_or_default();
    file_name.push(suffix);
    match exe.parent() {
        Some(dir) => dir.join(file_name),
        None => PathBuf::from(file_name),
    }
}

/// 推导持久回滚备份路径（纯函数，跨平台可测；FR-104，ADR-0026）。
///
/// 在 exe 同目录（同卷以保 rename 原子）落 `{exe}.rollback.bak`，作为单一回滚源。
pub fn rollback_backup_path(current_exe: &Path) -> PathBuf {
    sibling_with_suffix(current_exe, ROLLBACK_BACKUP_SUFFIX)
}

/// 回滚是否可用（纯查询）：持久回滚备份是否存在（FR-104）。
///
/// 设置聚合视图（FR-87）据此暴露 `rollback_available`，供控制台启用 / 禁用回滚按钮。
pub fn rollback_available(current_exe: &Path) -> bool {
    rollback_backup_path(current_exe).exists()
}

/// 回滚规划（FR-104，跨平台，路径推导可单测）。
///
/// 回滚本质是「再做一次原子替换，只是新内容是持久备份里的旧二进制」，故内嵌 [`ReplacePlan`]
/// 复用 ADR-0021 的 [`execute_replace`]；仅多一个 `backup_source`（回滚源）。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RollbackPlan {
    /// 当前运行的二进制路径（替换目标）。
    pub current_exe: PathBuf,
    /// 回滚源：持久回滚备份 `{exe}.rollback.bak`。
    pub backup_source: PathBuf,
    /// 暂存路径：把备份 copy 到 exe 同目录 `.new`，再经 [`execute_replace`] 原子换回。
    pub staged: PathBuf,
    /// 复用的原子替换规划（承载 Windows `.old` 等平台分支）。
    pub replace: ReplacePlan,
}

/// 据当前 exe 路径生成回滚规划（纯函数，跨平台可测；FR-104，ADR-0026）。
pub fn plan_rollback(current_exe: &Path) -> RollbackPlan {
    let replace = plan_replace(current_exe);
    RollbackPlan {
        current_exe: current_exe.to_path_buf(),
        backup_source: rollback_backup_path(current_exe),
        staged: replace.staged.clone(),
        replace,
    }
}

/// 回滚结果（FR-104）：还原后落地的二进制路径，供 handler 置位重启请求。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RollbackOutcome {
    /// 回滚后落地的二进制路径（重启时拉起之）。
    pub exe: PathBuf,
}

/// 回滚到上一版本（FR-104，ADR-0026）：校验备份存在 → 原子换回 → 返回落地路径（不含重启）。
///
/// 流程：校验持久回滚备份存在（不存在报 [`UpdateError::NoBackup`]）→ 把备份 copy 到 exe 同目录
/// `.new` 暂存 → 复用 [`execute_replace`] 原子换回当前二进制 → 返回落地路径。替换执行同步阻塞，
/// 放阻塞线程池。失败尽力清理暂存 `.new`、旧二进制在 `execute_replace` 内尽力还原，不留半截。
///
/// `current_exe` 由调用方注入（便于测试用 tempdir）。重启由 handler 置请求 + main 拉起。
pub async fn rollback(current_exe: &Path) -> Result<RollbackOutcome, UpdateError> {
    let plan = plan_rollback(current_exe);
    // 校验回滚源存在（异步元数据查询，锁外 IO）
    if !tokio::fs::try_exists(&plan.backup_source)
        .await
        .map_err(|e| UpdateError::Io(e.to_string()))?
    {
        return Err(UpdateError::NoBackup);
    }
    // 把备份复制到 exe 同目录 `.new` 暂存（同卷以保后续 rename 原子）
    tokio::fs::copy(&plan.backup_source, &plan.staged)
        .await
        .map_err(|e| UpdateError::Io(e.to_string()))?;
    // 复用原子替换：把暂存的旧二进制换回当前 exe（同步阻塞，放阻塞线程池）
    let replace = plan.replace.clone();
    let exec_result = tokio::task::spawn_blocking(move || execute_replace(&replace))
        .await
        .map_err(|e| UpdateError::Io(e.to_string()))?;
    if let Err(e) = exec_result {
        // 替换执行失败：尽力清理暂存 `.new`，旧二进制已在 execute_replace 内尽力还原
        let _ = tokio::fs::remove_file(&plan.staged).await;
        return Err(e);
    }
    tracing::info!("已用持久回滚备份还原上一版二进制，准备触发自动重启");
    Ok(RollbackOutcome {
        exe: plan.current_exe,
    })
}

/// 启动早期清理上次自更新留下的残留临时文件（best-effort，失败仅 WARN，不阻断启动）。
///
/// 清理两类残留（在 main 早期调用）：
/// - `{exe}.old`：仅 Windows——运行中 exe 改名后留下的旧二进制（下次启动清理）。
/// - `{exe}.new`：任意平台——跨卷 copy fallback 或替换执行失败时残留的暂存新文件（m3）。
pub fn cleanup_stale_old() {
    let Ok(exe) = std::env::current_exe() else {
        return;
    };
    cleanup_stale_artifacts(&exe);
}

/// 据给定 exe 路径清理残留临时文件（注入 exe 便于测试）。
fn cleanup_stale_artifacts(exe: &Path) {
    // .old 仅 Windows 产生；.new 任意平台都可能残留
    if cfg!(windows) {
        remove_stale_file(&sibling_with_suffix(exe, ".old"), "残留旧二进制");
    }
    remove_stale_file(&sibling_with_suffix(exe, ".new"), "残留暂存新二进制");
    remove_compound_managed_files(exe);
}

/// 清理 compound 残留（ADR-0032）：删 exe 同目录里名为「规范 exe 名 + 两层及以上管理后缀」的文件
/// （如 `.bak.bak`、`.bak.rollback.bak`，多由用户手动跑备份文件再更新留下）；**保留**单层
/// `.bak`（ADR-0021 事务兜底）与 `.rollback.bak`（ADR-0026 持久回滚源）。best-effort，失败仅 WARN。
fn remove_compound_managed_files(exe: &Path) {
    let (Some(dir), Some(canonical)) = (exe.parent(), exe.file_name().map(strip_managed_suffixes))
    else {
        return;
    };
    let Ok(entries) = std::fs::read_dir(dir) else {
        return;
    };
    let canonical = canonical.to_string_lossy().into_owned();
    for entry in entries.flatten() {
        let name = entry.file_name().to_string_lossy().into_owned();
        // 仅处理「规范名 + 管理后缀」者；剥去规范名前缀后剩余须是 ≥2 层管理后缀才算 compound
        let Some(remainder) = name.strip_prefix(&canonical) else {
            continue;
        };
        if managed_suffix_layers(remainder) >= 2 {
            remove_stale_file(&entry.path(), "compound 残留备份");
        }
    }
}

/// 数文件名剩余串由几层管理后缀叠成（ADR-0032）：从左反复剥离最长匹配的管理后缀；
/// 若中途遇到非管理内容则返回 0（不是纯管理后缀串，不清理）。
fn managed_suffix_layers(remainder: &str) -> usize {
    let mut rest = remainder;
    let mut layers = 0;
    while !rest.is_empty() {
        let Some(stripped) = MANAGED_SUFFIXES
            .iter()
            .find_map(|suffix| rest.strip_prefix(suffix))
        else {
            return 0;
        };
        rest = stripped;
        layers += 1;
    }
    layers
}

/// 尽力删除一个残留临时文件（不存在则跳过；失败仅 WARN，不阻断启动）。
fn remove_stale_file(path: &Path, desc: &str) {
    if path.exists() {
        if let Err(e) = std::fs::remove_file(path) {
            tracing::warn!(路径 = %path.display(), 错误 = %e, "清理{desc}失败（将下次重试）");
        } else {
            tracing::info!(路径 = %path.display(), "已清理自更新{desc}");
        }
    }
}

/// 从 zip 压缩包中解压第一个文件到指定路径（FR-138，在 `spawn_blocking` 中执行）。
///
/// zip 内预期只含一个文件（即可执行二进制）；取第一个文件，按名忽略，直接流式写到 `dest`。
/// zip 不合法、文件为空或 IO 失败均报 [`UpdateError::Io`]，由调用方清理临时文件。
fn extract_zip_entry_sync(
    zip_path: &std::path::Path,
    dest: &std::path::Path,
) -> Result<(), UpdateError> {
    use std::io::{Read, Write};
    let file = std::fs::File::open(zip_path).map_err(|e| UpdateError::Io(e.to_string()))?;
    let mut archive = zip::ZipArchive::new(file).map_err(|e| UpdateError::Io(e.to_string()))?;
    if archive.is_empty() {
        return Err(UpdateError::Io("压缩包为空".to_string()));
    }
    // 取第一个文件（zip 内含单个二进制，不关注文件名）
    let mut entry = archive
        .by_index(0)
        .map_err(|e| UpdateError::Io(e.to_string()))?;
    let mut out = std::fs::File::create(dest).map_err(|e| UpdateError::Io(e.to_string()))?;
    let mut buf = vec![0u8; 64 * 1024];
    loop {
        let n = entry
            .read(&mut buf)
            .map_err(|e| UpdateError::Io(e.to_string()))?;
        if n == 0 {
            break;
        }
        out.write_all(&buf[..n])
            .map_err(|e| UpdateError::Io(e.to_string()))?;
    }
    out.flush().map_err(|e| UpdateError::Io(e.to_string()))?;
    Ok(())
}

/// 异步封装：在阻塞线程池中解压 zip 到 `dest`（FR-138）。
///
/// 失败不自行清理 `dest`，由调用方决定清理策略（与 `download_to_file` 保持一致）。
pub(crate) async fn extract_zip_binary(zip_path: &Path, dest: &Path) -> Result<(), UpdateError> {
    let zip_path = zip_path.to_path_buf();
    let dest = dest.to_path_buf();
    tokio::task::spawn_blocking(move || extract_zip_entry_sync(&zip_path, &dest))
        .await
        .map_err(|e| UpdateError::Io(e.to_string()))?
}

/// 流式下载资产到临时文件，边写边算 sha256（不二次读盘、不整体载入内存），返回实算 hex。
///
/// 失败时尽力删除半截临时文件，不留残留。
async fn download_to_file(
    mut reader: Box<dyn AsyncRead + Send + Unpin>,
    dest: &Path,
) -> Result<String, UpdateError> {
    let mut file = tokio::fs::File::create(dest)
        .await
        .map_err(|e| UpdateError::Io(e.to_string()))?;
    let mut hasher = Sha256::new();
    let mut buf = vec![0u8; 64 * 1024];
    loop {
        let n = match reader.read(&mut buf).await {
            Ok(0) => break,
            Ok(n) => n,
            Err(e) => {
                drop(file);
                let _ = tokio::fs::remove_file(dest).await;
                return Err(UpdateError::Io(e.to_string()));
            }
        };
        hasher.update(&buf[..n]);
        if let Err(e) = tokio::io::AsyncWriteExt::write_all(&mut file, &buf[..n]).await {
            drop(file);
            let _ = tokio::fs::remove_file(dest).await;
            return Err(UpdateError::Io(e.to_string()));
        }
    }
    if let Err(e) = tokio::io::AsyncWriteExt::flush(&mut file).await {
        let _ = tokio::fs::remove_file(dest).await;
        return Err(UpdateError::Io(e.to_string()));
    }
    Ok(format!("{:x}", hasher.finalize()))
}

/// 从 `.sha256` 资产内容解析裸 64 位十六进制摘要（纯函数，可测）。
///
/// 兼容「`<hex>  <filename>`」（sha256sum 格式）与纯 hex 两种形态：取首个空白前的 token。
pub(crate) fn parse_sha256_content(content: &str) -> Result<String, UpdateError> {
    let token = content
        .split_whitespace()
        .next()
        .unwrap_or("")
        .to_ascii_lowercase();
    if token.len() == 64 && token.bytes().all(|b| b.is_ascii_hexdigit()) {
        Ok(token)
    } else {
        Err(UpdateError::Parse("sha256 内容非法".to_string()))
    }
}

/// 定长校验下载摘要与发布摘要是否一致（纯函数，可测）。
pub(crate) fn verify_checksum(actual: &str, expected: &str) -> Result<(), UpdateError> {
    if actual.eq_ignore_ascii_case(expected) {
        Ok(())
    } else {
        Err(UpdateError::ChecksumMismatch)
    }
}

/// 执行原子替换（按 `cfg!(windows)` 分支，跨平台）。
///
/// 调用前 `staged` 必须已是校验通过的新二进制（落在与 `current_exe` 同目录 / 同卷）。
/// 替换是最后一步：本函数仅在校验通过后被调用。失败尽力回滚，不留破坏态。
fn execute_replace(plan: &ReplacePlan) -> Result<(), UpdateError> {
    #[cfg(windows)]
    {
        // Windows：运行中 exe 不能被覆盖，但可改名。先把运行中 exe 改名为 .old，再落新文件。
        let old = plan.old.as_ref().expect("Windows 替换规划必含 .old 路径");
        // 若已存在残留 .old，先尽力删除，避免改名失败；守 old == 当前 exe（剥离后缀后可能相等）不误删运行二进制
        if old != &plan.current_exe && old.exists() {
            let _ = std::fs::remove_file(old);
        }
        std::fs::rename(&plan.current_exe, old).map_err(|e| UpdateError::Io(e.to_string()))?;
        if let Err(e) = std::fs::rename(&plan.staged, &plan.current_exe) {
            // 落新文件失败：尽力把 .old 改回原位，使进程续以旧版可用
            let _ = std::fs::rename(old, &plan.current_exe);
            return Err(UpdateError::Io(e.to_string()));
        }
        Ok(())
    }
    #[cfg(not(windows))]
    {
        use std::os::unix::fs::PermissionsExt;
        // Unix：替换前把旧 exe 复制为 .bak（单步回退兜底）。
        // 守自拷贝：剥离后缀后规范 .bak 名可能恰等于当前 exe（用户跑的就是 .bak 文件），跳过避免毁源。
        if let Some(backup) = &plan.backup {
            if backup != &plan.current_exe {
                std::fs::copy(&plan.current_exe, backup)
                    .map_err(|e| UpdateError::Io(e.to_string()))?;
            }
        }
        // 给新文件置可执行权限 0755
        let perms = std::fs::Permissions::from_mode(0o755);
        std::fs::set_permissions(&plan.staged, perms)
            .map_err(|e| UpdateError::Io(e.to_string()))?;
        // rename 原子覆盖：运行中进程持旧 inode 不受影响
        std::fs::rename(&plan.staged, &plan.current_exe)
            .map_err(|e| UpdateError::Io(e.to_string()))?;
        Ok(())
    }
}

/// 应用更新的结果（成功返回新版本号，供 handler 回包并置位重启请求）。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ApplyOutcome {
    /// 替换后的新版本号。
    pub new_version: String,
    /// 替换后落地的二进制路径（重启时拉起之）。
    pub exe: PathBuf,
}

/// 应用更新：下载 → 校验 → 原子替换（不含重启；重启由 handler 置请求 + main 拉起）。
///
/// 流程：取最新 Release → 推导 target / 资产名 → 在 assets 里精确匹配二进制与 `.sha256`
/// → 流式下载到数据目录临时文件并边算 sha256 → 下载 `.sha256` 资产解析期望值 → 定长比对
/// → 一致则把临时文件落到 exe 同目录并原子替换。任何校验失败：删临时文件、不触碰二进制。
///
/// `current_exe` 与 `data_dir` 由调用方注入（便于测试用 tempdir）。出站经 `source`（已注入代理）。
/// `channel` 决定取哪一条 Release（FR-89：stable 仅稳定版 / prerelease 含预发布最新一条）。
pub async fn apply_update<S: ReleaseSource>(
    source: &S,
    channel: UpdateChannel,
    current_version: &str,
    current_exe: &Path,
    data_dir: &Path,
) -> Result<ApplyOutcome, UpdateError> {
    // 无进度上报的便捷入口（同步调用 / 既有测试用）：委托进度版，传 None。
    apply_update_with_progress(
        source,
        channel,
        current_version,
        current_exe,
        data_dir,
        None,
    )
    .await
}

/// 应用更新并逐阶段上报进度（FR-126，异步 job 用）。
///
/// 与 [`apply_update`] 同逻辑（复用 download / verify / replace 核心与失败回滚，**不改安全门**），
/// 仅在「下载 / 校验 / 替换」边界更新 `progress`（若有）并写中文分级 `tracing` 日志，便于后台 `tail`
/// 看进度。进度锁临界区只更新内存态、不持锁做 IO（锁外做 IO）。
pub async fn apply_update_with_progress<S: ReleaseSource>(
    source: &S,
    channel: UpdateChannel,
    current_version: &str,
    current_exe: &Path,
    data_dir: &Path,
    progress: Option<&ProgressSlot>,
) -> Result<ApplyOutcome, UpdateError> {
    let set_phase = |phase: UpdatePhase| {
        if let Some(p) = progress {
            lock_progress(p).phase = phase;
        }
    };

    let target = current_target()?;
    tracing::info!("在线更新：开始检查最新发布与版本比对");
    let release = source.fetch_latest_release(channel).await?;
    let latest = release.version();

    // 防御性校验：按通道判定是否可更新，避免把同版 / 旧版当新版落地（FR-89 通道分流）。
    // stable 要求 SemVer 严格更高；prerelease 仅当目标与当前版本串不同才替换。
    if !is_update_available_for_channel(channel, current_version, &latest)? {
        return Err(UpdateError::NoUpdate(format!(
            "最新版本 {latest} 相对当前版本 {current_version} 无可应用更新"
        )));
    }
    if let Some(p) = progress {
        lock_progress(p).latest_version = Some(latest.clone());
    }

    // 临时下载目录（数据目录下），按需创建
    let tmp_dir = data_dir.join(UPDATE_TMP_SUBDIR);
    tokio::fs::create_dir_all(&tmp_dir)
        .await
        .map_err(|e| UpdateError::Io(e.to_string()))?;

    // FR-138：优先选压缩包资产（zip + .sha256），回落到裸可执行资产（兼容旧版发布）。
    // zip 路径：下载 zip → 校验 sha256 → 解压取出二进制（staged 直接是解压后的二进制）。
    // 裸 exe 路径：下载二进制 → 校验 sha256（与现有逻辑一致）。
    let zip_name = archive_asset_name(&latest, target);
    let zip_sha_name = format!("{zip_name}.sha256");
    let use_zip =
        release.find_asset(&zip_name).is_some() && release.find_asset(&zip_sha_name).is_some();

    // 最终落盘到 staged 的二进制路径（供后续 stage_file 移到 exe 同目录）
    let staged_bin: PathBuf;

    set_phase(UpdatePhase::Downloading);
    if use_zip {
        // --- zip 路径 ---
        let zip_asset = release.find_asset(&zip_name).unwrap();
        let zip_sha_asset = release.find_asset(&zip_sha_name).unwrap();
        let tmp_zip = tmp_dir.join(&zip_name);

        tracing::info!(资产 = %zip_name, 新版本 = %latest, "在线更新：开始流式下载压缩包资产（FR-138）");
        let reader = source.download_asset(&zip_asset.download_url).await?;
        let actual = download_to_file(reader, &tmp_zip).await?;

        // 下载 zip.sha256 取期望摘要
        let expected = {
            let mut sha_reader = source.download_asset(&zip_sha_asset.download_url).await?;
            let mut content = String::new();
            sha_reader
                .read_to_string(&mut content)
                .await
                .map_err(|e| UpdateError::Io(e.to_string()))?;
            parse_sha256_content(&content)?
        };

        // 校验 zip 整体 sha256
        set_phase(UpdatePhase::Verifying);
        tracing::info!("在线更新：压缩包下载完成，开始校验 sha256（FR-138）");
        if let Err(e) = verify_checksum(&actual, &expected) {
            let _ = tokio::fs::remove_file(&tmp_zip).await;
            tracing::warn!("压缩包 sha256 与发布不符，已删临时文件、拒绝解压替换（FR-138）");
            return Err(e);
        }

        // 解压取出二进制到 tmp_bin（与 zip 同目录）
        let bin_name = asset_name(&latest, target);
        let tmp_bin = tmp_dir.join(&bin_name);
        if let Err(e) = extract_zip_binary(&tmp_zip, &tmp_bin).await {
            let _ = tokio::fs::remove_file(&tmp_zip).await;
            let _ = tokio::fs::remove_file(&tmp_bin).await;
            tracing::warn!(错误 = %e, "解压压缩包失败，已删临时文件（FR-138）");
            return Err(e);
        }
        // zip 已用完，清理
        let _ = tokio::fs::remove_file(&tmp_zip).await;
        staged_bin = tmp_bin;
    } else {
        // --- 裸 exe 回落路径（兼容无 zip 的旧版发布）---
        let bin_name = asset_name(&latest, target);
        let sha_name = format!("{bin_name}.sha256");
        let bin_asset = release
            .find_asset(&bin_name)
            .ok_or_else(|| UpdateError::MissingAsset(bin_name.clone()))?;
        let sha_asset = release
            .find_asset(&sha_name)
            .ok_or_else(|| UpdateError::MissingAsset(sha_name.clone()))?;
        let tmp_bin = tmp_dir.join(&bin_name);

        tracing::info!(资产 = %bin_name, 新版本 = %latest, "在线更新：开始流式下载更新资产（回落裸 exe）");
        let reader = source.download_asset(&bin_asset.download_url).await?;
        let actual = download_to_file(reader, &tmp_bin).await?;

        let expected = {
            let mut sha_reader = source.download_asset(&sha_asset.download_url).await?;
            let mut content = String::new();
            sha_reader
                .read_to_string(&mut content)
                .await
                .map_err(|e| UpdateError::Io(e.to_string()))?;
            parse_sha256_content(&content)?
        };

        set_phase(UpdatePhase::Verifying);
        tracing::info!("在线更新：下载完成，开始校验 sha256");
        if let Err(e) = verify_checksum(&actual, &expected) {
            let _ = tokio::fs::remove_file(&tmp_bin).await;
            tracing::warn!("下载内容 sha256 与发布不符，已删临时文件、拒绝替换");
            return Err(e);
        }
        staged_bin = tmp_bin;
    }

    set_phase(UpdatePhase::Replacing);
    tracing::info!("在线更新：sha256 校验通过，开始原子替换二进制");

    // 校验通过：替换前先把当前运行的二进制持久备份为回滚源（FR-104，ADR-0026）。
    // 覆盖单一备份（只留上一版）；落盘失败即报错、不触碰二进制。该备份独立于下方的
    // Unix `.bak` / Windows `.old`（临时兜底 / 启动清理），且不被启动清理。
    let rollback_bak = rollback_backup_path(current_exe);
    // 守自拷贝：剥离后缀后规范回滚备份名可能恰等于当前 exe（用户跑的就是 .rollback.bak），跳过避免毁源。
    if rollback_bak != current_exe {
        tokio::fs::copy(current_exe, &rollback_bak)
            .await
            .map_err(|e| UpdateError::Io(e.to_string()))?;
    }

    // 把临时文件移到 exe 同目录（同卷保 rename 原子，跨卷先 copy）后原子替换
    let plan = plan_replace(current_exe);
    stage_file(&staged_bin, &plan.staged).await?;
    // 替换执行是同步阻塞文件操作，放到阻塞线程池
    let plan_for_exec = plan.clone();
    let exec_result = tokio::task::spawn_blocking(move || execute_replace(&plan_for_exec))
        .await
        .map_err(|e| UpdateError::Io(e.to_string()))?;
    // 替换执行失败：尽力清理已暂存的 .new（m3，避免残留无人清理），旧二进制已在 execute_replace 内尽力还原
    if let Err(e) = exec_result {
        let _ = tokio::fs::remove_file(&plan.staged).await;
        return Err(e);
    }

    tracing::info!(新版本 = %latest, "二进制原子替换完成，准备触发自动重启");
    Ok(ApplyOutcome {
        new_version: latest,
        exe: plan.current_exe,
    })
}

/// 把临时文件落到目标路径（同卷 rename，跨卷 fallback 到 copy + 删源）。
async fn stage_file(tmp: &Path, staged: &Path) -> Result<(), UpdateError> {
    match tokio::fs::rename(tmp, staged).await {
        Ok(()) => Ok(()),
        Err(_) => {
            // 跨卷 rename 失败：copy 到同目录临时名后删源
            tokio::fs::copy(tmp, staged)
                .await
                .map_err(|e| UpdateError::Io(e.to_string()))?;
            let _ = tokio::fs::remove_file(tmp).await;
            Ok(())
        }
    }
}
