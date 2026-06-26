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
        "x86_64-unknown-linux-gnu"
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
    assert_eq!(target_ext("x86_64-unknown-linux-gnu"), "");
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
        asset_name("1.2.3", "x86_64-unknown-linux-gnu"),
        "jianartifact-1.2.3-x86_64-unknown-linux-gnu"
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
    let line = format!("{hex}  jianartifact-0.4.0-x86_64-unknown-linux-gnu");
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

// ---------- fake ReleaseSource ----------

/// 测试用 fake 源：注入构造好的 Release 与「url → 字节」映射，不触网。
struct FakeSource {
    release: Result<Release, UpdateError>,
    /// url → 内容字节（download_asset 据此返回）。
    assets: HashMap<String, Vec<u8>>,
    /// 记录被请求的下载 url（断言只下了该下的）。
    downloaded: Mutex<Vec<String>>,
}

impl FakeSource {
    fn new(release: Release, assets: HashMap<String, Vec<u8>>) -> Self {
        Self {
            release: Ok(release),
            assets,
            downloaded: Mutex::new(Vec::new()),
        }
    }
}

impl ReleaseSource for FakeSource {
    async fn fetch_latest_release(&self) -> Result<Release, UpdateError> {
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

    let outcome = apply_update(&source, "0.3.0", &exe, dir.path())
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

    let err = apply_update(&source, "0.3.0", &exe, dir.path())
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
    let err = apply_update(&source, "0.3.0", &exe, dir.path())
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
    let err = apply_update(&source, "0.3.0", &exe, dir.path())
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
    let check = build_check("0.3.0", &release).unwrap();
    assert_eq!(check.current_version, "0.3.0");
    assert_eq!(check.latest_version, "0.4.0");
    assert!(check.update_available);
    assert_eq!(check.asset_name, asset_name("0.4.0", target));
    assert_eq!(check.notes, "说明");
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
fn 解析_release_缺_tag_报错() {
    assert!(matches!(
        super::source::parse_release(r#"{"name":"x"}"#),
        Err(UpdateError::Parse(_))
    ));
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
