//! 在线更新模块单元测试（FR-85，ADR-0021）。
//!
//! 穷举纯函数（target 推导 / 版本比较 / 资产名 / sha256 校验 / 替换规划），并以 fake
//! `ReleaseSource` 验证 apply 全链（校验通过走替换、不一致拒绝、缺资产报错）。
//! 端到端「替换→重启→新版本运行」无真机不可验，本处只覆盖可测部分。

use std::collections::HashMap;
use std::path::Path;
use std::sync::Mutex;

use tokio::io::AsyncRead;

use super::source::{Release, ReleaseAsset, ReleaseSource};
use super::*;

// ---------- target 推导（三平台 + 不支持平台报错）----------

#[test]
fn target_推导_三平台() {
    assert_eq!(
        resolve_target("linux", "x86_64").unwrap(),
        "x86_64-unknown-linux-musl"
    );
    assert_eq!(
        resolve_target("windows", "x86_64").unwrap(),
        "x86_64-pc-windows-msvc"
    );
    assert_eq!(
        resolve_target("macos", "aarch64").unwrap(),
        "aarch64-apple-darwin"
    );
}

#[test]
fn target_推导_不支持平台报错() {
    assert!(matches!(
        resolve_target("linux", "aarch64"),
        Err(UpdateError::UnsupportedPlatform(_))
    ));
    assert!(matches!(
        resolve_target("freebsd", "x86_64"),
        Err(UpdateError::UnsupportedPlatform(_))
    ));
}

#[test]
fn 扩展名_仅_windows_为_exe() {
    assert_eq!(target_ext("x86_64-pc-windows-msvc"), ".exe");
    assert_eq!(target_ext("x86_64-unknown-linux-musl"), "");
    assert_eq!(target_ext("aarch64-apple-darwin"), "");
}

// ---------- 资产名推导 ----------

#[test]
fn 资产名_含版本_target_扩展名() {
    assert_eq!(
        asset_name("0.4.0", "x86_64-pc-windows-msvc"),
        "jianartifact-0.4.0-x86_64-pc-windows-msvc.exe"
    );
    assert_eq!(
        asset_name("1.2.3", "x86_64-unknown-linux-musl"),
        "jianartifact-1.2.3-x86_64-unknown-linux-musl"
    );
}

// ---------- 版本比较（更新/不更新/相等/非法串）----------

#[test]
fn 版本比较_有更新() {
    assert!(is_update_available("0.3.0", "0.4.0").unwrap());
    assert!(is_update_available("0.3.0", "0.3.1").unwrap());
    assert!(is_update_available("0.3.0", "1.0.0").unwrap());
    // tag 带前导 v 与预发布后缀均被规整 / 忽略
    assert!(is_update_available("0.3.0", "v0.4.0").unwrap());
    assert!(is_update_available("0.3.0", "0.4.0-rc.1").unwrap());
}

#[test]
fn 版本比较_不更新或相等() {
    assert!(!is_update_available("0.4.0", "0.4.0").unwrap());
    assert!(!is_update_available("0.4.0", "0.3.9").unwrap());
    assert!(!is_update_available("1.0.0", "0.9.9").unwrap());
}

#[test]
fn 版本比较_非法串报错() {
    assert!(matches!(
        is_update_available("0.3.0", "abc"),
        Err(UpdateError::InvalidVersion(_))
    ));
    assert!(matches!(
        is_update_available("0.3", "0.4.0"),
        Err(UpdateError::InvalidVersion(_))
    ));
    assert!(matches!(
        is_update_available("0.3.0", "0.4.0.1"),
        Err(UpdateError::InvalidVersion(_))
    ));
}

// ---------- sha256 内容解析 + 校验（一致/不一致）----------

#[test]
fn sha256_解析_纯_hex_与_sha256sum_格式() {
    let hex = "a".repeat(64);
    assert_eq!(parse_sha256_content(&hex).unwrap(), hex);
    // sha256sum 形态：<hex>  <filename>
    let line = format!("{hex}  jianartifact-0.4.0-x86_64-unknown-linux-musl");
    assert_eq!(parse_sha256_content(&line).unwrap(), hex);
    // 大写规整为小写
    let upper = "A".repeat(64);
    assert_eq!(parse_sha256_content(&upper).unwrap(), "a".repeat(64));
}

#[test]
fn sha256_解析_非法报错() {
    assert!(parse_sha256_content("xyz").is_err());
    assert!(parse_sha256_content(&"g".repeat(64)).is_err());
    assert!(parse_sha256_content("").is_err());
}

#[test]
fn 校验和_一致与不一致() {
    let a = "a".repeat(64);
    assert!(verify_checksum(&a, &a).is_ok());
    // 大小写无关
    assert!(verify_checksum(&a, &"A".repeat(64)).is_ok());
    assert!(matches!(
        verify_checksum(&a, &"b".repeat(64)),
        Err(UpdateError::ChecksumMismatch)
    ));
}

// ---------- 替换规划（unix / windows 路径推导跨平台可测）----------

#[test]
fn 替换规划_路径推导() {
    let exe = Path::new("/opt/app/jianartifact");
    let plan = plan_replace(exe);
    assert_eq!(plan.current_exe, exe);
    assert_eq!(plan.staged, Path::new("/opt/app/jianartifact.new"));
    if cfg!(windows) {
        assert_eq!(
            plan.old.as_deref(),
            Some(Path::new("/opt/app/jianartifact.old"))
        );
        assert!(plan.backup.is_none());
    } else {
        assert_eq!(
            plan.backup.as_deref(),
            Some(Path::new("/opt/app/jianartifact.bak"))
        );
        assert!(plan.old.is_none());
    }
}

// ---------- 回滚备份路径推导（FR-104，跨平台可测）----------

#[test]
fn 回滚备份路径_同目录加固定后缀() {
    let exe = Path::new("/opt/app/jianartifact");
    assert_eq!(
        rollback_backup_path(exe),
        Path::new("/opt/app/jianartifact.rollback.bak")
    );
    // Windows 形态：保留原扩展名、整体再加后缀
    let win = Path::new(r"C:\app\jianartifact.exe");
    assert_eq!(
        rollback_backup_path(win),
        Path::new(r"C:\app\jianartifact.exe.rollback.bak")
    );
}

// ---------- 回滚规划（FR-104，unix / windows 路径推导跨平台可测）----------

#[test]
fn 回滚规划_路径推导() {
    let exe = Path::new("/opt/app/jianartifact");
    let plan = plan_rollback(exe);
    assert_eq!(plan.current_exe, exe);
    assert_eq!(
        plan.backup_source,
        Path::new("/opt/app/jianartifact.rollback.bak")
    );
    assert_eq!(plan.staged, Path::new("/opt/app/jianartifact.new"));
    if cfg!(windows) {
        assert_eq!(
            plan.replace.old.as_deref(),
            Some(Path::new("/opt/app/jianartifact.old"))
        );
    } else {
        assert!(plan.replace.old.is_none());
    }
}

// ---------- 回滚可用性：备份存在与否（FR-104）----------

#[tokio::test]
async fn 回滚可用性_随备份存在与否() {
    let dir = tempfile::tempdir().unwrap();
    let exe = dir.path().join("jianartifact");
    tokio::fs::write(&exe, b"CUR").await.unwrap();
    // 无备份 → 不可回滚
    assert!(!rollback_available(&exe));
    // 造一个备份 → 可回滚
    tokio::fs::write(dir.path().join("jianartifact.rollback.bak"), b"OLD")
        .await
        .unwrap();
    assert!(rollback_available(&exe));
}

// ---------- 回滚执行：有备份还原成功 ----------

#[tokio::test]
async fn 回滚_有备份_还原上一版() {
    let dir = tempfile::tempdir().unwrap();
    let exe = dir.path().join("jianartifact");
    // 当前是新版（坏版本），备份是旧版（要回滚到的目标）
    tokio::fs::write(&exe, b"NEW-BAD-BINARY").await.unwrap();
    tokio::fs::write(
        dir.path().join("jianartifact.rollback.bak"),
        b"OLD-GOOD-BINARY",
    )
    .await
    .unwrap();

    let outcome = rollback(&exe).await.expect("有备份应回滚成功");
    assert_eq!(outcome.exe, exe);
    // 回滚后 exe 内容应为旧版备份
    assert_eq!(tokio::fs::read(&exe).await.unwrap(), b"OLD-GOOD-BINARY");
}

// ---------- 回滚执行：无备份报 NoBackup ----------

#[tokio::test]
async fn 回滚_无备份_报错() {
    let dir = tempfile::tempdir().unwrap();
    let exe = dir.path().join("jianartifact");
    tokio::fs::write(&exe, b"CUR-BINARY").await.unwrap();

    let err = rollback(&exe).await.unwrap_err();
    assert!(matches!(err, UpdateError::NoBackup));
    // 当前二进制不受影响
    assert_eq!(tokio::fs::read(&exe).await.unwrap(), b"CUR-BINARY");
}

// ---------- apply：升级后应留持久回滚备份（FR-104）----------

#[tokio::test]
async fn apply_成功后留持久回滚备份() {
    let dir = tempfile::tempdir().unwrap();
    let exe = dir.path().join("jianartifact");
    tokio::fs::write(&exe, b"OLD-BINARY").await.unwrap();

    let new_bin = b"NEW-BINARY-BYTES";
    let (release, assets) = release_with_assets("0.4.0", new_bin);
    let source = FakeSource::new(release, assets);

    apply_update(&source, UpdateChannel::Stable, "0.3.0", &exe, dir.path())
        .await
        .expect("校验通过应成功替换");
    // 持久回滚备份应留有升级前的旧二进制（跨平台一致，独立于 .bak/.old）
    let rollback_bak = dir.path().join("jianartifact.rollback.bak");
    assert_eq!(
        tokio::fs::read(&rollback_bak).await.unwrap(),
        b"OLD-BINARY",
        "升级后应留持久回滚备份（升级前的旧二进制）"
    );
}

// ---------- fake ReleaseSource ----------

/// 测试用 fake 源：注入构造好的 Release 与「url → 字节」映射，不触网。
struct FakeSource {
    release: Result<Release, UpdateError>,
    /// url → 内容字节（download_asset 据此返回）。
    assets: HashMap<String, Vec<u8>>,
    /// 记录被请求的下载 url（断言只下了该下的）。
    downloaded: Mutex<Vec<String>>,
    /// 记录被请求的更新通道（FR-89：断言 stable / prerelease 各取对应源）。
    channels: Mutex<Vec<UpdateChannel>>,
}

impl FakeSource {
    fn new(release: Release, assets: HashMap<String, Vec<u8>>) -> Self {
        Self {
            release: Ok(release),
            assets,
            downloaded: Mutex::new(Vec::new()),
            channels: Mutex::new(Vec::new()),
        }
    }
}

impl ReleaseSource for FakeSource {
    async fn fetch_latest_release(&self, channel: UpdateChannel) -> Result<Release, UpdateError> {
        // 记录被请求的通道，便于断言「stable / prerelease 各取对应源」
        self.channels
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .push(channel);
        match &self.release {
            Ok(r) => Ok(r.clone()),
            Err(_) => Err(UpdateError::Upstream("fake 上游失败".to_string())),
        }
    }

    async fn download_asset(
        &self,
        url: &str,
    ) -> Result<Box<dyn AsyncRead + Send + Unpin>, UpdateError> {
        self.downloaded
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .push(url.to_string());
        match self.assets.get(url) {
            Some(bytes) => Ok(Box::new(std::io::Cursor::new(bytes.clone()))),
            None => Err(UpdateError::Upstream(format!("无此资产: {url}"))),
        }
    }
}

/// 计算字节的 sha256 hex（测试辅助）。
fn sha256_hex(data: &[u8]) -> String {
    use digest::Digest;
    let mut h = sha2::Sha256::new();
    h.update(data);
    format!("{:x}", h.finalize())
}

/// 构造一份含本机 target 资产 + 其 .sha256 的 Release（正确 sha256）。
fn release_with_assets(version: &str, bin: &[u8]) -> (Release, HashMap<String, Vec<u8>>) {
    let target = current_target().expect("测试运行平台应为受支持的三目标之一");
    let bin_name = asset_name(version, target);
    let sha_name = format!("{bin_name}.sha256");
    let bin_url = format!("https://example/{bin_name}");
    let sha_url = format!("https://example/{sha_name}");
    let sha_content = format!("{}  {bin_name}", sha256_hex(bin));

    let release = Release {
        tag_name: format!("v{version}"),
        name: format!("Release {version}"),
        body: "发布说明".to_string(),
        assets: vec![
            ReleaseAsset {
                name: bin_name,
                download_url: bin_url.clone(),
            },
            ReleaseAsset {
                name: sha_name,
                download_url: sha_url.clone(),
            },
        ],
    };
    let mut assets = HashMap::new();
    assets.insert(bin_url, bin.to_vec());
    assets.insert(sha_url, sha_content.into_bytes());
    (release, assets)
}

// ---------- apply：正确 sha256 → 走到替换 ----------

#[tokio::test]
async fn apply_正确_sha256_落地替换() {
    let dir = tempfile::tempdir().unwrap();
    // 造一个假的「当前 exe」
    let exe = dir.path().join("jianartifact");
    tokio::fs::write(&exe, b"OLD-BINARY").await.unwrap();

    let new_bin = b"NEW-BINARY-BYTES";
    let (release, assets) = release_with_assets("0.4.0", new_bin);
    let source = FakeSource::new(release, assets);

    let outcome = apply_update(&source, UpdateChannel::Stable, "0.3.0", &exe, dir.path())
        .await
        .expect("校验通过应成功替换");
    assert_eq!(outcome.new_version, "0.4.0");
    assert_eq!(outcome.exe, exe);
    // 替换后 exe 内容应为新二进制
    let after = tokio::fs::read(&exe).await.unwrap();
    assert_eq!(after, new_bin);
    // Unix 应留 .bak 旧副本
    if !cfg!(windows) {
        let bak = dir.path().join("jianartifact.bak");
        assert_eq!(tokio::fs::read(&bak).await.unwrap(), b"OLD-BINARY");
    }
}

// ---------- apply：sha256 不一致 → 拒绝替换、删临时文件、不触碰二进制 ----------

#[tokio::test]
async fn apply_sha256_不一致_拒绝替换() {
    let dir = tempfile::tempdir().unwrap();
    let exe = dir.path().join("jianartifact");
    tokio::fs::write(&exe, b"OLD-BINARY").await.unwrap();

    let target = current_target().unwrap();
    let version = "0.4.0";
    let bin_name = asset_name(version, target);
    let sha_name = format!("{bin_name}.sha256");
    let bin_url = format!("https://example/{bin_name}");
    let sha_url = format!("https://example/{sha_name}");
    // 故意写一个不匹配的 sha256
    let wrong_sha = "0".repeat(64);
    let release = Release {
        tag_name: format!("v{version}"),
        name: "x".to_string(),
        body: String::new(),
        assets: vec![
            ReleaseAsset {
                name: bin_name.clone(),
                download_url: bin_url.clone(),
            },
            ReleaseAsset {
                name: sha_name,
                download_url: sha_url.clone(),
            },
        ],
    };
    let mut assets = HashMap::new();
    assets.insert(bin_url, b"NEW-BINARY-BYTES".to_vec());
    assets.insert(sha_url, wrong_sha.into_bytes());
    let source = FakeSource::new(release, assets);

    let err = apply_update(&source, UpdateChannel::Stable, "0.3.0", &exe, dir.path())
        .await
        .unwrap_err();
    assert!(matches!(err, UpdateError::ChecksumMismatch));
    // 旧二进制保留、内容不变
    assert_eq!(tokio::fs::read(&exe).await.unwrap(), b"OLD-BINARY");
    // 临时文件已删
    let tmp_bin = dir.path().join("update-tmp").join(&bin_name);
    assert!(!tmp_bin.exists(), "校验失败应删临时文件");
}

// ---------- apply：缺二进制资产 / 缺 .sha256 → 报错 ----------

#[tokio::test]
async fn apply_缺二进制资产_报错() {
    let dir = tempfile::tempdir().unwrap();
    let exe = dir.path().join("jianartifact");
    tokio::fs::write(&exe, b"OLD").await.unwrap();
    // Release 不含任何资产
    let release = Release {
        tag_name: "v0.4.0".to_string(),
        name: "x".to_string(),
        body: String::new(),
        assets: vec![],
    };
    let source = FakeSource::new(release, HashMap::new());
    let err = apply_update(&source, UpdateChannel::Stable, "0.3.0", &exe, dir.path())
        .await
        .unwrap_err();
    assert!(matches!(err, UpdateError::MissingAsset(_)));
    assert_eq!(tokio::fs::read(&exe).await.unwrap(), b"OLD");
}

#[tokio::test]
async fn apply_缺_sha256_资产_报错() {
    let dir = tempfile::tempdir().unwrap();
    let exe = dir.path().join("jianartifact");
    tokio::fs::write(&exe, b"OLD").await.unwrap();
    let target = current_target().unwrap();
    let bin_name = asset_name("0.4.0", target);
    let bin_url = format!("https://example/{bin_name}");
    // 只放二进制资产，不放 .sha256
    let release = Release {
        tag_name: "v0.4.0".to_string(),
        name: "x".to_string(),
        body: String::new(),
        assets: vec![ReleaseAsset {
            name: bin_name,
            download_url: bin_url.clone(),
        }],
    };
    let mut assets = HashMap::new();
    assets.insert(bin_url, b"NEW".to_vec());
    let source = FakeSource::new(release, assets);
    let err = apply_update(&source, UpdateChannel::Stable, "0.3.0", &exe, dir.path())
        .await
        .unwrap_err();
    assert!(matches!(err, UpdateError::MissingAsset(_)));
}

// ---------- build_check：版本比对 + 资产名 ----------

#[test]
fn 检查结果_组装() {
    let target = current_target().unwrap();
    let release = Release {
        tag_name: "v0.4.0".to_string(),
        name: "Release 0.4.0".to_string(),
        body: "说明".to_string(),
        assets: vec![],
    };
    let check = build_check(UpdateChannel::Stable, "0.3.0", &release).unwrap();
    assert_eq!(check.current_version, "0.3.0");
    assert_eq!(check.latest_version, "0.4.0");
    assert!(check.update_available);
    assert_eq!(check.asset_name, asset_name("0.4.0", target));
    assert_eq!(check.notes, "说明");
}

#[test]
fn prerelease_当前版本反映_dev_构建时收敛为无更新() {
    // 回归守护（修复版本号不收敛）：prerelease 滚动 Release（tag=dev、name 内嵌完整 dev 版本串）。
    let release = Release {
        tag_name: "dev".to_string(),
        name: "0.4.0-dev.8.4488ab2".to_string(),
        body: String::new(),
        assets: vec![],
    };
    // 当 current 反映真实 dev 构建版本（CI 注入 build_version 后的形态）→ 与 latest 相等 → 无更新（收敛）
    let converged =
        build_check(UpdateChannel::Prerelease, "0.4.0-dev.8.4488ab2", &release).unwrap();
    assert_eq!(converged.latest_version, "0.4.0-dev.8.4488ab2");
    assert!(
        !converged.update_available,
        "当前版本已等于最新 dev 版本，应收敛为无更新"
    );

    // 反衬 bug：若 current 仍是裸 CARGO_PKG_VERSION（0.4.0，未注入），则永远 != dev 串 → 一直有更新
    let buggy = build_check(UpdateChannel::Prerelease, "0.4.0", &release).unwrap();
    assert!(
        buggy.update_available,
        "裸 0.4.0 与 dev 串不等 → 一直显示有更新（正是注入 build_version 前的不收敛症状）"
    );
}

// ---------- release JSON 解析 ----------

#[test]
fn 解析_release_json() {
    let body = r#"{
        "tag_name": "v0.4.0",
        "name": "Release 0.4.0",
        "body": "发布说明",
        "assets": [
            {"name": "a.bin", "browser_download_url": "https://x/a.bin", "size": 10},
            {"name": "a.bin.sha256", "browser_download_url": "https://x/a.bin.sha256"}
        ]
    }"#;
    let r = super::source::parse_release(body).unwrap();
    assert_eq!(r.tag_name, "v0.4.0");
    assert_eq!(r.version(), "0.4.0");
    assert_eq!(r.assets.len(), 2);
    assert_eq!(
        r.find_asset("a.bin").unwrap().download_url,
        "https://x/a.bin"
    );
}

#[test]
fn version_滚动dev标签_回退到name版本串() {
    // prerelease 滚动发布：tag_name 是固定标签 `dev`（非版本串），版本应回退到 name。
    let r = Release {
        tag_name: "dev".to_string(),
        name: "0.4.0-dev.5.a68e6d0".to_string(),
        body: String::new(),
        assets: vec![],
    };
    assert_eq!(r.version(), "0.4.0-dev.5.a68e6d0");
    // 正式版：tag_name=vX.Y.Z 仍走 tag，不受回退影响
    let r2 = Release {
        tag_name: "v0.4.0".to_string(),
        name: "Release 0.4.0".to_string(),
        body: String::new(),
        assets: vec![],
    };
    assert_eq!(r2.version(), "0.4.0");
}

#[test]
fn 解析_release_缺_tag_报错() {
    assert!(matches!(
        super::source::parse_release(r#"{"name":"x"}"#),
        Err(UpdateError::Parse(_))
    ));
}

// ---------- prerelease 列表解析（FR-89：跳 draft、取最新一条）----------

#[test]
fn 解析_release_列表_跳_draft_取最新() {
    // 列表按发布时间倒序：首条为 draft 应跳过，取下一条（含预发布）
    let body = r#"[
        {"tag_name": "v0.5.0-rc.2", "draft": true, "prerelease": true, "assets": []},
        {"tag_name": "v0.5.0-rc.1", "draft": false, "prerelease": true,
         "assets": [{"name": "a.bin", "browser_download_url": "https://x/a.bin"}]},
        {"tag_name": "v0.4.0", "draft": false, "prerelease": false, "assets": []}
    ]"#;
    let r = super::source::parse_release_list(body).unwrap();
    // 跳过 draft 的 rc.2，取最新非 draft 的 rc.1（预发布）
    assert_eq!(r.tag_name, "v0.5.0-rc.1");
    assert_eq!(r.version(), "0.5.0-rc.1");
    assert_eq!(r.assets.len(), 1);
}

#[test]
fn 解析_release_列表_空或全_draft_报上游错误() {
    // 空列表
    assert!(matches!(
        super::source::parse_release_list("[]"),
        Err(UpdateError::Upstream(_))
    ));
    // 全为 draft
    let body = r#"[{"tag_name": "v0.5.0-rc.1", "draft": true, "assets": []}]"#;
    assert!(matches!(
        super::source::parse_release_list(body),
        Err(UpdateError::Upstream(_))
    ));
}

#[test]
fn 解析_release_列表_非数组报解析错() {
    // prerelease 通道期望数组；返回单对象应报 Parse 错
    assert!(matches!(
        super::source::parse_release_list(r#"{"tag_name":"v0.4.0"}"#),
        Err(UpdateError::Parse(_))
    ));
}

// ---------- 通道选源：stable 取稳定、prerelease 取预发布（FR-89）----------

#[tokio::test]
async fn apply_prerelease_通道_升级到预发布版() {
    // prerelease 通道下，fake 源返回预发布版 → 走到替换、断言记录的通道为 Prerelease
    let dir = tempfile::tempdir().unwrap();
    let exe = dir.path().join("jianartifact");
    tokio::fs::write(&exe, b"OLD-BINARY").await.unwrap();

    let new_bin = b"PRERELEASE-BINARY";
    // 预发布版 0.5.0-rc.1：版本比较忽略后缀，0.5.0 > 0.3.0，应可升级
    let (release, assets) = release_with_assets("0.5.0-rc.1", new_bin);
    let source = FakeSource::new(release, assets);

    let outcome = apply_update(
        &source,
        UpdateChannel::Prerelease,
        "0.3.0",
        &exe,
        dir.path(),
    )
    .await
    .expect("prerelease 通道校验通过应替换");
    assert_eq!(outcome.new_version, "0.5.0-rc.1");
    assert_eq!(tokio::fs::read(&exe).await.unwrap(), new_bin);
    // 断言确实以 Prerelease 通道取的源
    let chans = source.channels.lock().unwrap_or_else(|e| e.into_inner());
    assert_eq!(chans.as_slice(), [UpdateChannel::Prerelease]);
}

// ---------- 复现：dev 预发布同核心版本应可升级（FR-89 通道分流，bug 复现）----------

#[test]
fn 通道分流_prerelease_同核心dev构建判为可更新() {
    // 复现根因 2：当前 0.4.0，最新 dev 预发布 0.4.0-dev.5.<sha>（核心版本相等）。
    // prerelease 通道应「目标 != 当前即可更新」，而非按 major.minor.patch 判 false。
    assert!(
        is_update_available_for_channel(UpdateChannel::Prerelease, "0.4.0", "0.4.0-dev.5.a68e6d0")
            .unwrap(),
        "prerelease 通道下不同的 dev 构建应判为可更新"
    );
    // 完全相同的版本串：无可更新（避免无意义自替换）。
    assert!(
        !is_update_available_for_channel(
            UpdateChannel::Prerelease,
            "0.4.0-dev.5.a68e6d0",
            "0.4.0-dev.5.a68e6d0"
        )
        .unwrap(),
        "prerelease 通道下完全相同的版本串应判为无更新"
    );
}

#[test]
fn 通道分流_stable_语义不变仍要求严格更高() {
    // stable 通道语义保持不变：核心版本必须严格更高才更新。
    assert!(
        !is_update_available_for_channel(UpdateChannel::Stable, "0.4.0", "0.4.0").unwrap(),
        "stable 通道下同版本应判为无更新"
    );
    assert!(
        is_update_available_for_channel(UpdateChannel::Stable, "0.3.0", "0.4.0").unwrap(),
        "stable 通道下更高版本应判为可更新"
    );
    // stable 通道下，dev 预发布同核心版本仍判无更新（不被本修破坏）。
    assert!(
        !is_update_available_for_channel(UpdateChannel::Stable, "0.4.0", "0.4.0-dev.5.a68e6d0")
            .unwrap(),
        "stable 通道下同核心版本的 dev 串仍判无更新"
    );
}

#[test]
fn 检查结果_prerelease通道_dev串判可更新且资产名匹配() {
    // 复现根因 1+2 合流：prerelease 通道、当前 0.4.0、最新 dev 预发布资产（GitHub 已把 '+' 写成 '.'）。
    // build_check 应判 update_available=true，且 asset_name 与 release 资产名（dot 串）一致。
    let target = current_target().unwrap();
    let dev_version = "0.4.0-dev.5.a68e6d0"; // 资产名无 '+'，与 GitHub 存储一致
    let bin_name = asset_name(dev_version, target);
    let release = Release {
        tag_name: "dev".to_string(),
        name: dev_version.to_string(),
        body: "开发版快照".to_string(),
        assets: vec![ReleaseAsset {
            name: bin_name.clone(),
            download_url: format!("https://example/{bin_name}"),
        }],
    };
    let check = build_check(UpdateChannel::Prerelease, "0.4.0", &release).unwrap();
    assert!(
        check.update_available,
        "prerelease 通道下不同 dev 构建应判可更新"
    );
    assert_eq!(check.latest_version, dev_version);
    // 资产名按 dot 串重构，应与 release 中实际资产名精确匹配（find_asset 命中）
    assert_eq!(check.asset_name, bin_name);
    assert!(
        release.find_asset(&check.asset_name).is_some(),
        "build_check 推导的资产名应在 release 资产中命中"
    );
}

#[tokio::test]
async fn apply_prerelease_通道_dev同核心版本落地替换() {
    // 复现根因 2 在 apply 链路：当前 0.4.0，最新 dev 预发布同核心版本 → 应走到替换而非 NoUpdate。
    let dir = tempfile::tempdir().unwrap();
    let exe = dir.path().join("jianartifact");
    tokio::fs::write(&exe, b"OLD-BINARY").await.unwrap();

    let new_bin = b"DEV-SNAPSHOT-BINARY";
    let (release, assets) = release_with_assets("0.4.0-dev.5.a68e6d0", new_bin);
    let source = FakeSource::new(release, assets);

    let outcome = apply_update(
        &source,
        UpdateChannel::Prerelease,
        "0.4.0",
        &exe,
        dir.path(),
    )
    .await
    .expect("prerelease 通道下不同 dev 构建应走到替换");
    assert_eq!(outcome.new_version, "0.4.0-dev.5.a68e6d0");
    assert_eq!(tokio::fs::read(&exe).await.unwrap(), new_bin);
}

#[tokio::test]
async fn fetch_stable_与_prerelease_各记录对应通道() {
    // 验证 fake 源记录的通道与传入一致（stable / prerelease 各取对应源）
    let (release, assets) = release_with_assets("0.4.0", b"BIN");
    let source = FakeSource::new(release, assets);

    let _ = source
        .fetch_latest_release(UpdateChannel::Stable)
        .await
        .unwrap();
    let _ = source
        .fetch_latest_release(UpdateChannel::Prerelease)
        .await
        .unwrap();
    let chans = source.channels.lock().unwrap_or_else(|e| e.into_inner());
    assert_eq!(
        chans.as_slice(),
        [UpdateChannel::Stable, UpdateChannel::Prerelease]
    );
}

// ---------- apply 单飞互斥（M2）----------

#[test]
fn apply_单飞_第二次抢占失败() {
    let handle = std::sync::Arc::new(RestartHandle::default());
    // 第一个抢到 guard
    let guard = handle.try_begin_apply().expect("首个应抢到 apply 标志");
    // 持有期间第二个抢不到
    assert!(
        handle.try_begin_apply().is_none(),
        "在途时第二个 apply 应抢占失败"
    );
    // guard 释放后可再次抢占（标志复位）
    drop(guard);
    assert!(
        handle.try_begin_apply().is_some(),
        "释放后标志应复位、可再次抢占"
    );
}

#[test]
fn apply_单飞_guard_出错路径也复位() {
    let handle = std::sync::Arc::new(RestartHandle::default());
    // 模拟一次 apply 中途 ? 早返回：guard 在作用域结束即析构复位
    {
        let _guard = handle.try_begin_apply().expect("应抢到");
        // 此作用域代表 apply 执行中（含任意早返回点）
    }
    // 析构后标志复位，下一次仍可抢占
    assert!(
        handle.try_begin_apply().is_some(),
        "出错 / 早返回后 guard 析构应复位标志"
    );
}
