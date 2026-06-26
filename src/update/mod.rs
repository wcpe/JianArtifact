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
use serde::Serialize;
use sha2::Sha256;
use tokio::io::{AsyncRead, AsyncReadExt};

mod restart;
mod source;

#[cfg(test)]
mod tests;

pub use restart::{ApplyGuard, RestartHandle, RestartRequest};
pub use source::{GithubReleaseSource, Release, ReleaseAsset, ReleaseSource};

/// 临时下载子目录名（位于数据目录下），存放下载中的资产，校验失败即清理。
const UPDATE_TMP_SUBDIR: &str = "update-tmp";
/// FR-86 三发布目标之一：Linux x86_64。
const TARGET_LINUX_X64: &str = "x86_64-unknown-linux-gnu";
/// FR-86 三发布目标之一：Windows x86_64。
const TARGET_WINDOWS_X64: &str = "x86_64-pc-windows-msvc";
/// FR-86 三发布目标之一：macOS aarch64。
const TARGET_MACOS_ARM64: &str = "aarch64-apple-darwin";
/// 资产名前缀（与 FR-86 命名契约一致：`jianartifact-{version}-{target}{ext}`）。
const ASSET_PREFIX: &str = "jianartifact";

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
}

/// 更新检查结果（对外响应载体）。
#[derive(Debug, Clone, Serialize, PartialEq, Eq)]
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
pub(crate) fn is_update_available(current: &str, latest: &str) -> Result<bool, UpdateError> {
    let cur = parse_version(current)?;
    let lat = parse_version(latest)?;
    Ok((lat.major, lat.minor, lat.patch) > (cur.major, cur.minor, cur.patch))
}

/// 据当前版本与 Release 组装更新检查结果（纯函数，可测）。
///
/// 推导本机 target 与资产名、比对版本；不要求 Release 中已含资产（检查阶段只看版本）。
pub fn build_check(current_version: &str, release: &Release) -> Result<UpdateCheck, UpdateError> {
    let target = current_target()?;
    let latest = release.version();
    let update_available = is_update_available(current_version, &latest)?;
    Ok(UpdateCheck {
        current_version: current_version.to_string(),
        latest_version: latest.clone(),
        update_available,
        asset_name: asset_name(&latest, target),
        notes: release.body.clone(),
    })
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

/// 在 exe 同目录生成「原文件名 + 后缀」的兄弟路径（同卷以保 rename 原子）。
fn sibling_with_suffix(exe: &Path, suffix: &str) -> PathBuf {
    let mut file_name = exe
        .file_name()
        .map(|s| s.to_os_string())
        .unwrap_or_default();
    file_name.push(suffix);
    match exe.parent() {
        Some(dir) => dir.join(file_name),
        None => PathBuf::from(file_name),
    }
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
    // .old 仅 Windows 产生；.new 任意平台都可能残留
    if cfg!(windows) {
        remove_stale_file(&sibling_with_suffix(&exe, ".old"), "残留旧二进制");
    }
    remove_stale_file(&sibling_with_suffix(&exe, ".new"), "残留暂存新二进制");
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
        // 若已存在残留 .old，先尽力删除，避免改名失败
        if old.exists() {
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
        // Unix：替换前把旧 exe 复制为 .bak（单步回退兜底）
        if let Some(backup) = &plan.backup {
            std::fs::copy(&plan.current_exe, backup).map_err(|e| UpdateError::Io(e.to_string()))?;
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
pub async fn apply_update<S: ReleaseSource>(
    source: &S,
    current_version: &str,
    current_exe: &Path,
    data_dir: &Path,
) -> Result<ApplyOutcome, UpdateError> {
    let target = current_target()?;
    let release = source.fetch_latest_release().await?;
    let latest = release.version();

    // 防御性校验：仅当最新版本确实高于当前版本才替换，避免把同版 / 旧版当新版落地
    if !is_update_available(current_version, &latest)? {
        return Err(UpdateError::NoUpdate(format!(
            "最新版本 {latest} 不高于当前版本 {current_version}"
        )));
    }

    // 资产名与其 .sha256 名
    let bin_name = asset_name(&latest, target);
    let sha_name = format!("{bin_name}.sha256");
    let bin_asset = release
        .find_asset(&bin_name)
        .ok_or_else(|| UpdateError::MissingAsset(bin_name.clone()))?;
    let sha_asset = release
        .find_asset(&sha_name)
        .ok_or_else(|| UpdateError::MissingAsset(sha_name.clone()))?;

    // 临时下载目录（数据目录下），按需创建
    let tmp_dir = data_dir.join(UPDATE_TMP_SUBDIR);
    tokio::fs::create_dir_all(&tmp_dir)
        .await
        .map_err(|e| UpdateError::Io(e.to_string()))?;
    let tmp_bin = tmp_dir.join(&bin_name);

    // 流式下载二进制，边写边算 sha256
    let reader = source.download_asset(&bin_asset.download_url).await?;
    let actual = download_to_file(reader, &tmp_bin).await?;

    // 下载 .sha256 资产（小文件）取期望摘要
    let expected = {
        let mut sha_reader = source.download_asset(&sha_asset.download_url).await?;
        let mut content = String::new();
        sha_reader
            .read_to_string(&mut content)
            .await
            .map_err(|e| UpdateError::Io(e.to_string()))?;
        parse_sha256_content(&content)?
    };

    // 定长校验：不一致即删临时文件、拒绝替换、保留旧二进制
    if let Err(e) = verify_checksum(&actual, &expected) {
        let _ = tokio::fs::remove_file(&tmp_bin).await;
        tracing::warn!("下载内容 sha256 与发布不符，已删临时文件、拒绝替换");
        return Err(e);
    }

    // 校验通过：把临时文件移到 exe 同目录（同卷保 rename 原子，跨卷先 copy）后原子替换
    let plan = plan_replace(current_exe);
    stage_file(&tmp_bin, &plan.staged).await?;
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
