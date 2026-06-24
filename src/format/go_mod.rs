//! Go 模块格式（FR-28，hosted + proxy）：按 GOPROXY 协议暴露模块版本的 list / info / mod / zip / latest。
//!
//! 作为统一 [`Format`] trait 的实现接入通用制品机理（存储 / 校验和 / 事务 / 单飞缓存），
//! 只负责 Go 自身协议：GOPROXY 端点路径解析、模块路径大小写 bang 编码（`!x` ↔ `X`）、
//! 版本不可变策略、内容类型与使用片段，以及 `.info` JSON 生成 / `@v/list` 与 `@latest`
//! 聚合所需的纯函数（便于穷举单测）。
//!
//! 制品在仓库内的存储约定（与 GOPROXY 磁盘缓存布局一致，模块段为 bang 编码原形）：
//! - go.mod 存于 `{module_bang}/@v/{version}.mod`
//! - 模块 zip 存于 `{module_bang}/@v/{version}.zip`
//! - 版本元信息存于 `{module_bang}/@v/{version}.info`
//!
//! `@v/list`（版本列表）与 `@latest`（最新版本）为易变聚合文档，不单独存储：
//! hosted 据已存版本制品动态聚合，proxy 回源透传——避免与索引互为权威。

use crate::meta::ArtifactRecord;

use super::{normalize_repo_path, ArtifactCoordinates, Format, PathError, UsageSnippet};

/// GOPROXY 版本目录分隔段（协议固定为 `@v`）。
const VERSION_SEGMENT: &str = "@v";
/// `@latest` 端点末段。
const LATEST_SEGMENT: &str = "@latest";
/// `.info` 元信息内容类型（GOPROXY 返回 JSON）。
const INFO_CONTENT_TYPE: &str = "application/json";
/// `.mod` 文本内容类型。
const MOD_CONTENT_TYPE: &str = "text/plain; charset=utf-8";
/// 模块 zip 内容类型。
const ZIP_CONTENT_TYPE: &str = "application/zip";

/// Go 格式处理器：仓库内以 `{module_bang}/@v/{version}.{ext}` 定位每个版本文件。
pub struct GoFormat;

/// Go 协议错误：路径不是合法 GOPROXY 端点 / bang 编码非法等。
#[derive(Debug, thiserror::Error, PartialEq, Eq)]
pub enum GoError {
    /// 请求路径不是可识别的 GOPROXY 端点。
    #[error("不是合法的 Go 模块端点路径")]
    InvalidEndpoint,
    /// 模块路径的 bang 大小写编码非法（`!` 后非小写字母 / 末尾孤立 `!`）。
    #[error("模块路径 bang 编码非法")]
    InvalidBang,
}

/// 模块版本文件的类型（`.info` / `.mod` / `.zip`）。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum VersionFile {
    /// 版本元信息 JSON（`{"Version","Time"}`）。
    Info,
    /// go.mod 文本。
    Mod,
    /// 模块 zip。
    Zip,
}

impl VersionFile {
    /// 文件扩展名（不含点）。
    pub fn ext(self) -> &'static str {
        match self {
            VersionFile::Info => "info",
            VersionFile::Mod => "mod",
            VersionFile::Zip => "zip",
        }
    }

    /// 据扩展名识别版本文件类型；非三者之一返回 None。
    pub fn from_ext(ext: &str) -> Option<Self> {
        match ext {
            "info" => Some(VersionFile::Info),
            "mod" => Some(VersionFile::Mod),
            "zip" => Some(VersionFile::Zip),
            _ => None,
        }
    }
}

/// 解析后的 GOPROXY 请求：据仓库内路径分派到具体端点。
///
/// `module` 为 **bang 解码后** 的规范模块路径（用于聚合 / 展示 / 回源对账）；
/// 存储键仍用 bang 原形（见 [`GoFormat::version_storage_path`]）。
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum GoRequest {
    /// `GET {module}/@v/list`：列出该模块所有版本。
    List {
        /// bang 解码后的规范模块路径。
        module: String,
        /// 模块路径的 bang 原形（存储键前缀）。
        module_bang: String,
    },
    /// `GET {module}/@latest`：最新版本 info。
    Latest {
        /// bang 解码后的规范模块路径。
        module: String,
        /// 模块路径的 bang 原形（存储键前缀）。
        module_bang: String,
    },
    /// `GET/PUT {module}/@v/{version}.{info|mod|zip}`：单个版本文件。
    Version {
        /// bang 解码后的规范模块路径。
        module: String,
        /// 模块路径的 bang 原形（存储键前缀）。
        module_bang: String,
        /// 版本号（如 `v1.2.3`）。
        version: String,
        /// 文件类型。
        file: VersionFile,
    },
}

impl GoFormat {
    /// 把模块路径中的大写字母还原为 bang 编码（`X` → `!x`）。
    ///
    /// 用于据规范模块路径反推存储键前缀；仅大写 ASCII 字母前加 `!` 并转小写，其余原样。
    pub fn encode_bang(module: &str) -> String {
        let mut out = String::with_capacity(module.len());
        for ch in module.chars() {
            if ch.is_ascii_uppercase() {
                out.push('!');
                out.push(ch.to_ascii_lowercase());
            } else {
                out.push(ch);
            }
        }
        out
    }

    /// 把 bang 编码的模块路径解码为规范模块路径（`!x` → `X`）。
    ///
    /// `!` 后必须紧跟小写 ASCII 字母，否则视为非法（GOPROXY 规范）。
    pub fn decode_bang(encoded: &str) -> Result<String, GoError> {
        let mut out = String::with_capacity(encoded.len());
        let mut chars = encoded.chars();
        while let Some(ch) = chars.next() {
            if ch == '!' {
                // `!` 后须紧跟小写字母，转为对应大写
                match chars.next() {
                    Some(next) if next.is_ascii_lowercase() => out.push(next.to_ascii_uppercase()),
                    _ => return Err(GoError::InvalidBang),
                }
            } else {
                out.push(ch);
            }
        }
        Ok(out)
    }

    /// 据 bang 原形模块、版本与文件类型拼出仓库内存储键：`{module_bang}/@v/{version}.{ext}`。
    pub fn version_storage_path(module_bang: &str, version: &str, file: VersionFile) -> String {
        format!("{module_bang}/{VERSION_SEGMENT}/{version}.{}", file.ext())
    }

    /// 把归一化后的仓库内路径解析为 GOPROXY 请求。
    ///
    /// 识别三类末尾形态：`.../@v/list`、`.../@latest`、`.../@v/{version}.{ext}`；
    /// 其余一律 [`GoError::InvalidEndpoint`]。模块段经 bang 解码得规范模块路径。
    pub fn parse_request(path: &str) -> Result<GoRequest, GoError> {
        // `@latest`：末段为 @latest，其前缀即模块 bang 原形
        if let Some(module_bang) = path.strip_suffix(&format!("/{LATEST_SEGMENT}")) {
            let module_bang = module_bang.to_string();
            let module = Self::decode_bang(&module_bang)?;
            return Ok(GoRequest::Latest {
                module,
                module_bang,
            });
        }

        // 其余端点都在 `/@v/` 之后：切出模块前缀与 @v 之后的尾段
        let marker = format!("/{VERSION_SEGMENT}/");
        let (module_bang, rest) = path.rsplit_once(&marker).ok_or(GoError::InvalidEndpoint)?;
        if module_bang.is_empty() || rest.is_empty() {
            return Err(GoError::InvalidEndpoint);
        }
        let module_bang = module_bang.to_string();
        let module = Self::decode_bang(&module_bang)?;

        // `@v/list`：尾段恰为 list
        if rest == "list" {
            return Ok(GoRequest::List {
                module,
                module_bang,
            });
        }

        // `@v/{version}.{ext}`：尾段须形如 `{version}.{info|mod|zip}`
        let (version, ext) = rest.rsplit_once('.').ok_or(GoError::InvalidEndpoint)?;
        let file = VersionFile::from_ext(ext).ok_or(GoError::InvalidEndpoint)?;
        if version.is_empty() {
            return Err(GoError::InvalidEndpoint);
        }
        Ok(GoRequest::Version {
            module,
            module_bang,
            version: version.to_string(),
            file,
        })
    }

    /// 生成 `.info` 元信息 JSON 字节：`{"Version":"<version>","Time":"<time_rfc3339>"}`。
    pub fn build_info_json(version: &str, time_rfc3339: &str) -> Vec<u8> {
        serde_json::json!({ "Version": version, "Time": time_rfc3339 })
            .to_string()
            .into_bytes()
    }

    /// 把 SQLite `CURRENT_TIMESTAMP`（`YYYY-MM-DD HH:MM:SS`）转为 RFC3339（`...THH:MM:SSZ`）。
    ///
    /// 仅满足 `go` 客户端对 `.info` Time 字段为合法 RFC3339 的要求；已是 RFC3339 形态则原样返回。
    pub fn timestamp_to_rfc3339(ts: &str) -> String {
        // 已含 `T`（疑似已是 RFC3339）则不再加工
        if ts.contains('T') {
            return ts.to_string();
        }
        match ts.split_once(' ') {
            Some((date, time)) => format!("{date}T{time}Z"),
            // 非预期格式：兜底给一个固定 epoch，避免返回非法 Time
            None => "1970-01-01T00:00:00Z".to_string(),
        }
    }

    /// 从给定版本号集合中选出"最新版本"：按 Go 语义版本排序取最大。
    ///
    /// 排序规则（[`compare_semver`]）：主.次.补丁数值比较，正式版高于同核心的预发布版；
    /// 集合为空返回 None。
    pub fn latest_version(versions: &[String]) -> Option<String> {
        versions.iter().max_by(|a, b| compare_semver(a, b)).cloned()
    }
}

impl Format for GoFormat {
    fn name(&self) -> &'static str {
        "go"
    }

    fn parse_path(&self, raw_path: &str) -> Result<ArtifactCoordinates, PathError> {
        // Go 以归一化后的仓库内路径作为制品键（模块段为 bang 原形，与磁盘缓存布局一致）
        let path = normalize_repo_path(raw_path)?;
        Ok(ArtifactCoordinates { path })
    }

    fn can_overwrite(&self, _existing: &ArtifactRecord) -> bool {
        // Go 模块版本一经发布即不可变（FR-61）：同版本文件不可覆盖
        false
    }

    fn content_type(&self, coords: &ArtifactCoordinates) -> Option<String> {
        // 据末段扩展名判定：.zip→zip、.mod→text、.info→json；其余 None 交默认层
        let ext = coords.path.rsplit('.').next().unwrap_or("");
        let ct = match VersionFile::from_ext(ext) {
            Some(VersionFile::Zip) => ZIP_CONTENT_TYPE,
            Some(VersionFile::Mod) => MOD_CONTENT_TYPE,
            Some(VersionFile::Info) => INFO_CONTENT_TYPE,
            None => return None,
        };
        Some(ct.to_string())
    }

    fn usage_snippets(
        &self,
        public_base_url: &str,
        repo_name: &str,
        coords: &ArtifactCoordinates,
    ) -> Vec<UsageSnippet> {
        let base = public_base_url.trim_end_matches('/');
        let proxy_url = format!("{base}/{repo_name}");
        let mut snippets = vec![UsageSnippet {
            title: "配置 GOPROXY".to_string(),
            language: "bash".to_string(),
            content: format!("export GOPROXY={proxy_url}"),
        }];

        // 能从存储路径反解出 module@version 时给 go get 片段
        if let Some((module, version)) = module_version_from_path(&coords.path) {
            snippets.push(UsageSnippet {
                title: "获取模块".to_string(),
                language: "bash".to_string(),
                content: format!("go get {module}@{version}"),
            });
        }

        snippets
    }
}

/// 从仓库内存储路径反解 `(模块路径, 版本)`：布局 `{module_bang}/@v/{version}.{ext}`。
///
/// 模块段经 bang 解码还原大小写；无法构成合法版本文件路径时返回 None（如 list / latest）。
fn module_version_from_path(path: &str) -> Option<(String, String)> {
    let marker = format!("/{VERSION_SEGMENT}/");
    let (module_bang, rest) = path.rsplit_once(&marker)?;
    let (version, ext) = rest.rsplit_once('.')?;
    VersionFile::from_ext(ext)?;
    if module_bang.is_empty() || version.is_empty() {
        return None;
    }
    let module = GoFormat::decode_bang(module_bang).ok()?;
    Some((module, version.to_string()))
}

/// 比较两个 Go 语义版本号大小（用于 `@latest` 取最大）。
///
/// 解析 `vMAJOR.MINOR.PATCH`：核心三段按数值比较；核心相同时，无预发布后缀（正式版）
/// 大于有预发布后缀（如 `v1.0.0` > `v1.0.0-rc1`），两者都有预发布则按字典序比较后缀。
/// 无法解析的版本按字符串兜底比较，避免 panic。
fn compare_semver(a: &str, b: &str) -> std::cmp::Ordering {
    use std::cmp::Ordering;
    match (parse_semver(a), parse_semver(b)) {
        (Some(va), Some(vb)) => {
            va.core
                .cmp(&vb.core)
                .then_with(|| match (va.pre.is_empty(), vb.pre.is_empty()) {
                    // 正式版（无预发布）大于预发布版
                    (true, false) => Ordering::Greater,
                    (false, true) => Ordering::Less,
                    // 同类则按预发布后缀字典序
                    _ => va.pre.cmp(&vb.pre),
                })
        }
        // 任一无法解析：按字符串兜底
        _ => a.cmp(b),
    }
}

/// 解析后的语义版本：核心三段数值 + 预发布后缀。
struct SemVer {
    /// (major, minor, patch) 三段数值。
    core: (u64, u64, u64),
    /// 预发布后缀（`-` 之后部分；正式版为空串）。
    pre: String,
}

/// 解析 `vMAJOR.MINOR.PATCH[-pre]`：成功返回核心三段与预发布后缀，否则 None。
fn parse_semver(v: &str) -> Option<SemVer> {
    let s = v.strip_prefix('v')?;
    // 切出预发布后缀（首个 `-` 之后）
    let (core_str, pre) = match s.split_once('-') {
        Some((c, p)) => (c, p.to_string()),
        None => (s, String::new()),
    };
    let mut it = core_str.split('.');
    let major = it.next()?.parse::<u64>().ok()?;
    let minor = it.next()?.parse::<u64>().ok()?;
    let patch = it.next()?.parse::<u64>().ok()?;
    // 核心段恰为三段
    if it.next().is_some() {
        return None;
    }
    Some(SemVer {
        core: (major, minor, patch),
        pre,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    /// 构造一条仅含路径的最小制品记录，供覆盖策略判定用。
    fn 记录(path: &str) -> ArtifactRecord {
        ArtifactRecord {
            id: "id".to_string(),
            repo_id: "r".to_string(),
            path: path.to_string(),
            size: 1,
            sha256: "s".to_string(),
            sha1: "s".to_string(),
            md5: "s".to_string(),
            sha512: "s".to_string(),
            content_type: None,
            cached: 0,
            created_at: "now".to_string(),
        }
    }

    #[test]
    fn 名称为_go() {
        assert_eq!(GoFormat.name(), "go");
    }

    #[test]
    fn bang_编码大写字母() {
        // 大写转 !小写，其余原样
        assert_eq!(GoFormat::encode_bang("github.com/Foo"), "github.com/!foo");
        assert_eq!(
            GoFormat::encode_bang("GitHub.com/BarBaz"),
            "!git!hub.com/!bar!baz"
        );
        assert_eq!(
            GoFormat::encode_bang("golang.org/x/text"),
            "golang.org/x/text"
        );
    }

    #[test]
    fn bang_解码还原大写() {
        assert_eq!(
            GoFormat::decode_bang("github.com/!foo").unwrap(),
            "github.com/Foo"
        );
        assert_eq!(
            GoFormat::decode_bang("!git!hub.com/!bar!baz").unwrap(),
            "GitHub.com/BarBaz"
        );
        assert_eq!(
            GoFormat::decode_bang("golang.org/x/text").unwrap(),
            "golang.org/x/text"
        );
    }

    #[test]
    fn bang_编解码往返一致() {
        for m in [
            "github.com/Sirupsen/logrus",
            "GitHub.com/A/B",
            "no-uppercase/here",
        ] {
            assert_eq!(
                GoFormat::decode_bang(&GoFormat::encode_bang(m)).unwrap(),
                m,
                "模块 {m} 编解码应往返一致"
            );
        }
    }

    #[test]
    fn bang_非法序列报错() {
        // `!` 后非小写字母
        assert_eq!(GoFormat::decode_bang("foo/!Bar"), Err(GoError::InvalidBang));
        assert_eq!(GoFormat::decode_bang("foo/!1"), Err(GoError::InvalidBang));
        // 末尾孤立 `!`
        assert_eq!(GoFormat::decode_bang("foo/bar!"), Err(GoError::InvalidBang));
    }

    #[test]
    fn 解析_list_端点() {
        let req = GoFormat::parse_request("github.com/!foo/bar/@v/list").unwrap();
        assert_eq!(
            req,
            GoRequest::List {
                module: "github.com/Foo/bar".to_string(),
                module_bang: "github.com/!foo/bar".to_string(),
            }
        );
    }

    #[test]
    fn 解析_latest_端点() {
        let req = GoFormat::parse_request("golang.org/x/text/@latest").unwrap();
        assert_eq!(
            req,
            GoRequest::Latest {
                module: "golang.org/x/text".to_string(),
                module_bang: "golang.org/x/text".to_string(),
            }
        );
    }

    #[test]
    fn 解析版本文件端点三种扩展名() {
        let cases = [
            ("info", VersionFile::Info),
            ("mod", VersionFile::Mod),
            ("zip", VersionFile::Zip),
        ];
        for (ext, file) in cases {
            let path = format!("golang.org/x/text/@v/v0.3.7.{ext}");
            let req = GoFormat::parse_request(&path).unwrap();
            assert_eq!(
                req,
                GoRequest::Version {
                    module: "golang.org/x/text".to_string(),
                    module_bang: "golang.org/x/text".to_string(),
                    version: "v0.3.7".to_string(),
                    file,
                }
            );
        }
    }

    #[test]
    fn 解析含_bang_的版本端点解码模块() {
        let req = GoFormat::parse_request("github.com/!sirupsen/logrus/@v/v1.9.0.zip").unwrap();
        assert_eq!(
            req,
            GoRequest::Version {
                module: "github.com/Sirupsen/logrus".to_string(),
                module_bang: "github.com/!sirupsen/logrus".to_string(),
                version: "v1.9.0".to_string(),
                file: VersionFile::Zip,
            }
        );
    }

    #[test]
    fn 解析非法端点报错() {
        // 无 @v / @latest 段
        assert_eq!(
            GoFormat::parse_request("github.com/foo/bar"),
            Err(GoError::InvalidEndpoint)
        );
        // @v 段但扩展名非法
        assert_eq!(
            GoFormat::parse_request("foo/@v/v1.0.0.txt"),
            Err(GoError::InvalidEndpoint)
        );
        // @v 后缺版本
        assert_eq!(
            GoFormat::parse_request("foo/@v/.mod"),
            Err(GoError::InvalidEndpoint)
        );
        // 空模块前缀
        assert_eq!(
            GoFormat::parse_request("@v/list"),
            Err(GoError::InvalidEndpoint)
        );
    }

    #[test]
    fn 存储路径拼接() {
        assert_eq!(
            GoFormat::version_storage_path("golang.org/x/text", "v0.3.7", VersionFile::Mod),
            "golang.org/x/text/@v/v0.3.7.mod"
        );
        assert_eq!(
            GoFormat::version_storage_path("github.com/!foo/bar", "v1.0.0", VersionFile::Zip),
            "github.com/!foo/bar/@v/v1.0.0.zip"
        );
    }

    #[test]
    fn info_json_含_version_与_time() {
        let bytes = GoFormat::build_info_json("v1.2.3", "2024-01-02T03:04:05Z");
        let v: serde_json::Value = serde_json::from_slice(&bytes).unwrap();
        assert_eq!(v["Version"], "v1.2.3");
        assert_eq!(v["Time"], "2024-01-02T03:04:05Z");
    }

    #[test]
    fn 时间戳转_rfc3339() {
        // SQLite CURRENT_TIMESTAMP 形态
        assert_eq!(
            GoFormat::timestamp_to_rfc3339("2024-01-02 03:04:05"),
            "2024-01-02T03:04:05Z"
        );
        // 已是 RFC3339 形态原样返回
        assert_eq!(
            GoFormat::timestamp_to_rfc3339("2024-01-02T03:04:05Z"),
            "2024-01-02T03:04:05Z"
        );
    }

    #[test]
    fn latest_取最大版本() {
        let versions = vec![
            "v1.0.0".to_string(),
            "v1.2.0".to_string(),
            "v1.1.5".to_string(),
            "v0.9.9".to_string(),
        ];
        assert_eq!(GoFormat::latest_version(&versions).unwrap(), "v1.2.0");
    }

    #[test]
    fn latest_正式版高于预发布() {
        let versions = vec!["v1.0.0-rc1".to_string(), "v1.0.0".to_string()];
        assert_eq!(GoFormat::latest_version(&versions).unwrap(), "v1.0.0");
        // 同核心多个预发布取字典序较大者
        let pre = vec!["v2.0.0-alpha".to_string(), "v2.0.0-beta".to_string()];
        assert_eq!(GoFormat::latest_version(&pre).unwrap(), "v2.0.0-beta");
    }

    #[test]
    fn latest_空集合为_none() {
        assert!(GoFormat::latest_version(&[]).is_none());
    }

    #[test]
    fn go_版本不可覆盖() {
        assert!(!GoFormat.can_overwrite(&记录("golang.org/x/text/@v/v0.3.7.mod")));
        assert!(!GoFormat.can_overwrite(&记录("foo/@v/v1.0.0.zip")));
    }

    #[test]
    fn 内容类型按扩展名推断() {
        let c = |p: &str| {
            GoFormat.content_type(&ArtifactCoordinates {
                path: p.to_string(),
            })
        };
        assert_eq!(c("foo/@v/v1.0.0.zip").as_deref(), Some("application/zip"));
        assert_eq!(
            c("foo/@v/v1.0.0.mod").as_deref(),
            Some("text/plain; charset=utf-8")
        );
        assert_eq!(c("foo/@v/v1.0.0.info").as_deref(), Some("application/json"));
        // 未知扩展名返回 None
        assert_eq!(c("foo/@v/v1.0.0.txt"), None);
    }

    #[test]
    fn 解析路径归一化且拒穿越() {
        assert_eq!(
            GoFormat
                .parse_path("/golang.org/x/text/@v/v0.3.7.mod")
                .unwrap()
                .path,
            "golang.org/x/text/@v/v0.3.7.mod"
        );
        assert_eq!(
            GoFormat.parse_path("foo/../etc/passwd"),
            Err(PathError::Traversal)
        );
    }

    #[test]
    fn 使用片段含_goproxy_与_go_get() {
        let coords = ArtifactCoordinates {
            path: "github.com/!foo/bar/@v/v1.2.3.zip".to_string(),
        };
        let snippets = GoFormat.usage_snippets("http://localhost:8080/", "go-hosted", &coords);
        assert_eq!(snippets.len(), 2);
        // GOPROXY 指向本仓库，无双斜杠
        assert!(snippets[0]
            .content
            .contains("export GOPROXY=http://localhost:8080/go-hosted"));
        assert!(!snippets[0].content.contains("8080//go-hosted"));
        // go get 用解码后的模块路径
        assert!(snippets[1]
            .content
            .contains("go get github.com/Foo/bar@v1.2.3"));
    }

    #[test]
    fn 使用片段对非版本路径仅给_goproxy() {
        // list / latest 等聚合端点路径无法反解 module@version，只给 GOPROXY 片段
        let coords = ArtifactCoordinates {
            path: "github.com/foo/bar/@v/list".to_string(),
        };
        let snippets = GoFormat.usage_snippets("http://h", "r", &coords);
        assert_eq!(snippets.len(), 1);
        assert_eq!(snippets[0].title, "配置 GOPROXY");
    }
}
