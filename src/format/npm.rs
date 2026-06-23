//! npm registry 格式（FR-15）：以 npm registry 协议暴露 packument 与 tarball。
//!
//! 作为统一 [`Format`] trait 的实现接入通用制品机理（存储 / 校验和 / 事务），
//! 只负责 npm 自身协议：路径映射、版本不可变策略、内容类型与使用片段，
//! 以及 packument JSON 的生成 / 合并与代理 URL 重写（纯函数，便于穷举测试）。
//!
//! 制品在仓库内的存储约定：
//! - packument（包级 JSON 文档）存于路径 `{包名}`（如 `lodash`、`@scope/pkg`）。
//! - tarball 存于路径 `{包名}/-/{tarball 文件名}`（如 `lodash/-/lodash-4.17.21.tgz`）。

use base64::Engine;
use serde_json::{Map, Value};

use crate::meta::ArtifactRecord;

use super::{normalize_repo_path, ArtifactCoordinates, Format, PathError, UsageSnippet};

/// npm packument 在仓库内的内容类型。
const PACKUMENT_CONTENT_TYPE: &str = "application/json";
/// npm tarball 的内容类型（npm 客户端按 octet-stream 处理 .tgz）。
const TARBALL_CONTENT_TYPE: &str = "application/octet-stream";
/// tarball 在包内的目录分隔段（npm 协议固定为 `-`）。
const TARBALL_SEGMENT: &str = "-";

/// npm 格式处理器：仓库内以包名定位 packument，以 `{包名}/-/{文件}` 定位 tarball。
pub struct NpmFormat;

/// npm 协议错误：发布请求体不合法 / 版本已存在等。
#[derive(Debug, thiserror::Error, PartialEq, Eq)]
pub enum NpmError {
    /// 发布请求体结构不合法（缺字段 / 类型不符 / base64 解码失败等）。
    #[error("npm 发布请求体不合法: {0}")]
    InvalidBody(String),
    /// 该版本已发布，按 npm 语义不可覆盖（FR-61）。
    #[error("版本 {0} 已发布，不可覆盖")]
    VersionExists(String),
}

/// 从 npm 发布请求体解析出的单次发布内容。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct PublishRequest {
    /// 包名（含 scope，如 `@scope/name`）。
    pub package: String,
    /// 本次发布的版本号。
    pub version: String,
    /// tarball 文件名（如 `name-1.0.0.tgz`）。
    pub tarball_name: String,
    /// tarball 原始字节（已 base64 解码）。
    pub tarball: Vec<u8>,
    /// 本次发布版本对应的 version manifest（packument 中 `versions[ver]` 的内容）。
    pub version_manifest: Value,
    /// 本次发布携带的 dist-tags（如 `{"latest":"1.0.0"}`）。
    pub dist_tags: Map<String, Value>,
}

impl NpmFormat {
    /// 解析 `npm publish` 的请求体 JSON，提取本次发布的版本、tarball 与 manifest。
    ///
    /// npm 单次 publish 只携带一个新版本与一个 `_attachments` 附件，这里据此提取；
    /// 不修改字节内容（tarball 原样落盘、摘要由存储层算）。
    pub fn parse_publish(body: &[u8]) -> Result<PublishRequest, NpmError> {
        let root: Value = serde_json::from_slice(body)
            .map_err(|e| NpmError::InvalidBody(format!("JSON 解析失败: {e}")))?;

        let package = root
            .get("name")
            .and_then(Value::as_str)
            .ok_or_else(|| NpmError::InvalidBody("缺少包名 name".to_string()))?
            .to_string();

        // 从 versions 取唯一的新版本 manifest
        let versions = root
            .get("versions")
            .and_then(Value::as_object)
            .ok_or_else(|| NpmError::InvalidBody("缺少 versions".to_string()))?;
        let (version, version_manifest) = versions
            .iter()
            .next()
            .ok_or_else(|| NpmError::InvalidBody("versions 为空".to_string()))?;
        let version = version.clone();
        let version_manifest = version_manifest.clone();

        // 从 _attachments 取唯一的 tarball 附件并 base64 解码
        let attachments = root
            .get("_attachments")
            .and_then(Value::as_object)
            .ok_or_else(|| NpmError::InvalidBody("缺少 _attachments".to_string()))?;
        let (tarball_name, attachment) = attachments
            .iter()
            .next()
            .ok_or_else(|| NpmError::InvalidBody("_attachments 为空".to_string()))?;
        let data_b64 = attachment
            .get("data")
            .and_then(Value::as_str)
            .ok_or_else(|| NpmError::InvalidBody("附件缺少 data".to_string()))?;
        let tarball = base64::engine::general_purpose::STANDARD
            .decode(data_b64)
            .map_err(|e| NpmError::InvalidBody(format!("附件 base64 解码失败: {e}")))?;

        // dist-tags 可缺省（缺省时按 latest=version 兜底）
        let dist_tags = root
            .get("dist-tags")
            .and_then(Value::as_object)
            .cloned()
            .unwrap_or_default();

        Ok(PublishRequest {
            package,
            version,
            tarball_name: tarball_name.clone(),
            tarball,
            version_manifest,
            dist_tags,
        })
    }

    /// 据包名与 tarball 文件名拼出 tarball 在仓库内的存储路径（`{包名}/-/{文件}`）。
    pub fn tarball_path(package: &str, tarball_name: &str) -> String {
        format!("{package}/{TARBALL_SEGMENT}/{tarball_name}")
    }

    /// 合并发布到 packument：把新版本写入 `versions`、更新 `dist-tags`，
    /// 并把该版本的 `dist.tarball` 重写为指向本仓库、`dist.integrity`/`shasum` 填实算摘要。
    ///
    /// `existing` 为已存在的 packument（首次发布时传 None）；返回合并后的 packument JSON。
    /// 若该版本已存在则返回 [`NpmError::VersionExists`]（npm 已发布不可覆盖，FR-61）。
    pub fn merge_packument(
        existing: Option<&Value>,
        req: &PublishRequest,
        public_base_url: &str,
        repo_name: &str,
        sha1_hex: &str,
        sha512_b64: &str,
    ) -> Result<Value, NpmError> {
        // 以已有 packument 为基底，无则新建最小骨架
        let mut packument = match existing {
            Some(v) => v.clone(),
            None => Value::Object(Map::new()),
        };
        let obj = packument
            .as_object_mut()
            .ok_or_else(|| NpmError::InvalidBody("packument 不是对象".to_string()))?;

        // 顶层 name 与 versions / dist-tags 容器
        obj.entry("name")
            .or_insert_with(|| Value::String(req.package.clone()));
        let versions = obj
            .entry("versions")
            .or_insert_with(|| Value::Object(Map::new()))
            .as_object_mut()
            .ok_or_else(|| NpmError::InvalidBody("versions 不是对象".to_string()))?;

        // 已发布不可覆盖
        if versions.contains_key(&req.version) {
            return Err(NpmError::VersionExists(req.version.clone()));
        }

        // 复制本版本 manifest，并重写 dist 指向本仓库、填实摘要
        let mut manifest = req.version_manifest.clone();
        let manifest_obj = manifest
            .as_object_mut()
            .ok_or_else(|| NpmError::InvalidBody("version manifest 不是对象".to_string()))?;
        let tarball_url =
            Self::tarball_url(public_base_url, repo_name, &req.package, &req.tarball_name);
        let mut dist = Map::new();
        dist.insert("tarball".to_string(), Value::String(tarball_url));
        dist.insert("shasum".to_string(), Value::String(sha1_hex.to_string()));
        dist.insert(
            "integrity".to_string(),
            Value::String(format!("sha512-{sha512_b64}")),
        );
        manifest_obj.insert("dist".to_string(), Value::Object(dist));
        versions.insert(req.version.clone(), manifest);

        // 合并 dist-tags（本次发布的覆盖既有同名 tag；缺省补 latest）
        let tags = obj
            .entry("dist-tags")
            .or_insert_with(|| Value::Object(Map::new()))
            .as_object_mut()
            .ok_or_else(|| NpmError::InvalidBody("dist-tags 不是对象".to_string()))?;
        if req.dist_tags.is_empty() {
            tags.insert("latest".to_string(), Value::String(req.version.clone()));
        } else {
            for (k, v) in &req.dist_tags {
                tags.insert(k.clone(), v.clone());
            }
        }

        Ok(packument)
    }

    /// 把上游 packument 中所有版本的 `dist.tarball` URL 重写为指向本代理仓库，
    /// 使 `npm install` 经代理拉取 tarball（cache-miss 时再回源）。
    ///
    /// 仅改写 tarball 指向，不动 integrity/shasum（仍为上游算得的原值，校验照常）。
    pub fn rewrite_proxy_packument(
        upstream_packument: &[u8],
        public_base_url: &str,
        repo_name: &str,
    ) -> Result<Vec<u8>, NpmError> {
        let mut doc: Value = serde_json::from_slice(upstream_packument)
            .map_err(|e| NpmError::InvalidBody(format!("上游 packument 解析失败: {e}")))?;
        let package = doc
            .get("name")
            .and_then(Value::as_str)
            .ok_or_else(|| NpmError::InvalidBody("上游 packument 缺少 name".to_string()))?
            .to_string();

        if let Some(versions) = doc.get_mut("versions").and_then(Value::as_object_mut) {
            for manifest in versions.values_mut() {
                let Some(dist) = manifest.get_mut("dist").and_then(Value::as_object_mut) else {
                    continue;
                };
                // 从原 tarball URL 提取文件名，重写为本仓库地址
                let Some(name) = dist
                    .get("tarball")
                    .and_then(Value::as_str)
                    .and_then(|u| u.rsplit('/').next())
                    .map(str::to_string)
                else {
                    continue;
                };
                let url = Self::tarball_url(public_base_url, repo_name, &package, &name);
                dist.insert("tarball".to_string(), Value::String(url));
            }
        }

        serde_json::to_vec(&doc)
            .map_err(|e| NpmError::InvalidBody(format!("packument 序列化失败: {e}")))
    }

    /// 拼出 tarball 在本仓库的对外 URL：`{base}/{repo}/{包名}/-/{文件}`。
    fn tarball_url(
        public_base_url: &str,
        repo_name: &str,
        package: &str,
        tarball_name: &str,
    ) -> String {
        let base = public_base_url.trim_end_matches('/');
        format!("{base}/{repo_name}/{package}/{TARBALL_SEGMENT}/{tarball_name}")
    }
}

impl Format for NpmFormat {
    fn name(&self) -> &'static str {
        "npm"
    }

    fn parse_path(&self, raw_path: &str) -> Result<ArtifactCoordinates, PathError> {
        // npm 以归一化后的仓库内路径作为制品键（packument 用包名、tarball 用 `{包名}/-/{文件}`）
        let path = normalize_repo_path(raw_path)?;
        Ok(ArtifactCoordinates { path })
    }

    fn can_overwrite(&self, existing: &ArtifactRecord) -> bool {
        // packument 是包级聚合文档，发布新版本时需更新；tarball（含 `/-/` 段）已发布不可覆盖。
        // 据存储路径区分：含 `/-/` 段者为 tarball → 不可覆盖；否则为 packument → 可更新。
        !existing.path.contains(&format!("/{TARBALL_SEGMENT}/"))
    }

    fn content_type(&self, coords: &ArtifactCoordinates) -> Option<String> {
        // tarball（含 `/-/` 段）按 octet-stream；其余视为 packument JSON
        if coords.path.contains(&format!("/{TARBALL_SEGMENT}/")) {
            Some(TARBALL_CONTENT_TYPE.to_string())
        } else {
            Some(PACKUMENT_CONTENT_TYPE.to_string())
        }
    }

    fn usage_snippets(
        &self,
        public_base_url: &str,
        repo_name: &str,
        coords: &ArtifactCoordinates,
    ) -> Vec<UsageSnippet> {
        let base = public_base_url.trim_end_matches('/');
        // 从存储路径反推包名：tarball 取 `/-/` 之前的段，packument 即路径本身
        let package = match coords.path.split_once(&format!("/{TARBALL_SEGMENT}/")) {
            Some((pkg, _)) => pkg.to_string(),
            None => coords.path.clone(),
        };
        let registry = format!("{base}/{repo_name}/");
        vec![
            UsageSnippet {
                title: "安装".to_string(),
                language: "bash".to_string(),
                content: format!("npm install {package} --registry {registry}"),
            },
            UsageSnippet {
                title: "仓库接入（.npmrc）".to_string(),
                language: "ini".to_string(),
                // 仅示意接入位，不写入真实 Token（凭据不入示例）
                content: format!(
                    "registry={registry}\n//{}/{repo_name}/:_authToken=${{NPM_TOKEN}}",
                    base.trim_start_matches("http://")
                        .trim_start_matches("https://")
                ),
            },
        ]
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// 构造一条最小制品记录，仅用于覆盖策略 / 内容类型判定。
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

    /// 构造一份最小 npm publish 请求体（含 base64 tarball）。
    fn 发布体(package: &str, version: &str, tarball_name: &str, tarball: &[u8]) -> Vec<u8> {
        let data = base64::engine::general_purpose::STANDARD.encode(tarball);
        serde_json::json!({
            "name": package,
            "versions": {
                version: { "name": package, "version": version }
            },
            "dist-tags": { "latest": version },
            "_attachments": {
                tarball_name: { "content_type": "application/octet-stream", "data": data, "length": tarball.len() }
            }
        })
        .to_string()
        .into_bytes()
    }

    #[test]
    fn 名称为_npm() {
        assert_eq!(NpmFormat.name(), "npm");
    }

    #[test]
    fn 解析发布体提取版本与_tarball() {
        let body = 发布体("lodash", "4.17.21", "lodash-4.17.21.tgz", b"TARBALL");
        let req = NpmFormat::parse_publish(&body).unwrap();
        assert_eq!(req.package, "lodash");
        assert_eq!(req.version, "4.17.21");
        assert_eq!(req.tarball_name, "lodash-4.17.21.tgz");
        assert_eq!(req.tarball, b"TARBALL");
        assert_eq!(req.dist_tags.get("latest").unwrap(), "4.17.21");
    }

    #[test]
    fn 解析_scoped_包发布体() {
        let body = 发布体("@scope/pkg", "1.0.0", "pkg-1.0.0.tgz", b"X");
        let req = NpmFormat::parse_publish(&body).unwrap();
        assert_eq!(req.package, "@scope/pkg");
        assert_eq!(req.version, "1.0.0");
    }

    #[test]
    fn 解析发布体缺字段报错() {
        // 缺 _attachments
        let body = serde_json::json!({
            "name": "x",
            "versions": { "1.0.0": {} }
        })
        .to_string()
        .into_bytes();
        assert!(matches!(
            NpmFormat::parse_publish(&body),
            Err(NpmError::InvalidBody(_))
        ));
    }

    #[test]
    fn tarball_存储路径含_分隔段() {
        assert_eq!(
            NpmFormat::tarball_path("lodash", "lodash-4.17.21.tgz"),
            "lodash/-/lodash-4.17.21.tgz"
        );
        assert_eq!(
            NpmFormat::tarball_path("@scope/pkg", "pkg-1.0.0.tgz"),
            "@scope/pkg/-/pkg-1.0.0.tgz"
        );
    }

    #[test]
    fn 覆盖策略_packument可更新_tarball不可覆盖() {
        // packument（无 /-/ 段）可更新
        assert!(NpmFormat.can_overwrite(&记录("lodash")));
        assert!(NpmFormat.can_overwrite(&记录("@scope/pkg")));
        // tarball（含 /-/ 段）不可覆盖
        assert!(!NpmFormat.can_overwrite(&记录("lodash/-/lodash-4.17.21.tgz")));
        assert!(!NpmFormat.can_overwrite(&记录("@scope/pkg/-/pkg-1.0.0.tgz")));
    }

    #[test]
    fn 内容类型按_tarball_与_packument_区分() {
        let ct = |p: &str| {
            NpmFormat.content_type(&ArtifactCoordinates {
                path: p.to_string(),
            })
        };
        assert_eq!(ct("lodash").as_deref(), Some("application/json"));
        assert_eq!(
            ct("lodash/-/lodash-4.17.21.tgz").as_deref(),
            Some("application/octet-stream")
        );
    }

    #[test]
    fn 首次发布生成_packument_并重写_dist() {
        let req =
            NpmFormat::parse_publish(&发布体("lodash", "4.17.21", "lodash-4.17.21.tgz", b"T"))
                .unwrap();
        let pack = NpmFormat::merge_packument(
            None,
            &req,
            "http://localhost:8080",
            "npm-hosted",
            "sha1值",
            "c2hhNTEy",
        )
        .unwrap();

        assert_eq!(pack["name"], "lodash");
        assert_eq!(pack["dist-tags"]["latest"], "4.17.21");
        let dist = &pack["versions"]["4.17.21"]["dist"];
        // dist.tarball 指向本仓库
        assert_eq!(
            dist["tarball"],
            "http://localhost:8080/npm-hosted/lodash/-/lodash-4.17.21.tgz"
        );
        assert_eq!(dist["shasum"], "sha1值");
        assert_eq!(dist["integrity"], "sha512-c2hhNTEy");
    }

    #[test]
    fn 合并新版本到既有_packument() {
        let req1 =
            NpmFormat::parse_publish(&发布体("pkg", "1.0.0", "pkg-1.0.0.tgz", b"A")).unwrap();
        let pack = NpmFormat::merge_packument(None, &req1, "http://h", "r", "s1", "i1").unwrap();

        let req2 =
            NpmFormat::parse_publish(&发布体("pkg", "2.0.0", "pkg-2.0.0.tgz", b"B")).unwrap();
        let merged =
            NpmFormat::merge_packument(Some(&pack), &req2, "http://h", "r", "s2", "i2").unwrap();

        // 两个版本都在
        assert!(merged["versions"]["1.0.0"].is_object());
        assert!(merged["versions"]["2.0.0"].is_object());
        // latest 更新到新版本
        assert_eq!(merged["dist-tags"]["latest"], "2.0.0");
    }

    #[test]
    fn 重复发布同版本返回_versionexists() {
        let req = NpmFormat::parse_publish(&发布体("pkg", "1.0.0", "pkg-1.0.0.tgz", b"A")).unwrap();
        let pack = NpmFormat::merge_packument(None, &req, "http://h", "r", "s", "i").unwrap();
        // 再次发布同版本应被拒
        let err =
            NpmFormat::merge_packument(Some(&pack), &req, "http://h", "r", "s", "i").unwrap_err();
        assert_eq!(err, NpmError::VersionExists("1.0.0".to_string()));
    }

    #[test]
    fn 代理_packument_重写_tarball_指向本仓库() {
        let upstream = serde_json::json!({
            "name": "lodash",
            "dist-tags": { "latest": "4.17.21" },
            "versions": {
                "4.17.21": {
                    "name": "lodash",
                    "version": "4.17.21",
                    "dist": {
                        "tarball": "https://registry.npmjs.org/lodash/-/lodash-4.17.21.tgz",
                        "integrity": "sha512-上游原值",
                        "shasum": "上游sha1"
                    }
                }
            }
        })
        .to_string()
        .into_bytes();

        let rewritten =
            NpmFormat::rewrite_proxy_packument(&upstream, "http://localhost:8080", "npm-proxy")
                .unwrap();
        let doc: Value = serde_json::from_slice(&rewritten).unwrap();
        let dist = &doc["versions"]["4.17.21"]["dist"];
        // tarball 重写为本仓库
        assert_eq!(
            dist["tarball"],
            "http://localhost:8080/npm-proxy/lodash/-/lodash-4.17.21.tgz"
        );
        // integrity / shasum 保持上游原值（校验照常）
        assert_eq!(dist["integrity"], "sha512-上游原值");
        assert_eq!(dist["shasum"], "上游sha1");
    }

    #[test]
    fn 使用片段含_install_与_npmrc_接入() {
        let coords = ArtifactCoordinates {
            path: "lodash".to_string(),
        };
        let snippets = NpmFormat.usage_snippets("http://localhost:8080/", "npm-hosted", &coords);
        assert_eq!(snippets.len(), 2);
        assert!(snippets[0].content.contains("npm install lodash"));
        assert!(snippets[0]
            .content
            .contains("http://localhost:8080/npm-hosted/"));
        // 接入片段含 registry 与 _authToken 占位，但不含真实 Token 明文
        assert!(snippets[1]
            .content
            .contains("registry=http://localhost:8080/npm-hosted/"));
        assert!(snippets[1].content.contains("_authToken="));
    }

    #[test]
    fn 使用片段从_tarball_路径反推包名() {
        let coords = ArtifactCoordinates {
            path: "lodash/-/lodash-4.17.21.tgz".to_string(),
        };
        let snippets = NpmFormat.usage_snippets("http://h", "r", &coords);
        // 应反推出包名 lodash，而非整条 tarball 路径
        assert!(snippets[0].content.contains("npm install lodash "));
    }
}
