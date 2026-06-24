//! NuGet v3 格式（FR-29，hosted + proxy）：以 NuGet v3 协议暴露服务索引、扁平容器
//! 版本列表与 .nupkg / .nuspec，并支持 `nuget push` 发布。
//!
//! 作为统一 [`Format`] trait 的实现接入通用制品机理（存储 / 校验和 / 事务 / 单飞缓存），
//! 只负责 NuGet 自身协议：id / version 小写规范化、存储路径映射、覆盖（不可变）策略、
//! 内容类型与使用片段，以及服务索引 / 版本列表 / .nuspec 解析等**纯函数**（便于穷举测试）。
//!
//! 存储约定（flat container，id 与 version 均小写）：
//! - .nupkg 存于 `{id}/{version}/{id}.{version}.nupkg`
//! - .nuspec 存于 `{id}/{version}/{id}.nuspec`
//! - 版本列表 `v3-flatcontainer/{id}/index.json` 由元数据索引动态生成，不另存聚合文档。

use std::io::Read;

use serde_json::{json, Value};

use crate::meta::ArtifactRecord;

use super::{normalize_repo_path, ArtifactCoordinates, Format, PathError, UsageSnippet};

/// .nupkg 内容类型（NuGet 包为二进制 zip）。
const NUPKG_CONTENT_TYPE: &str = "application/octet-stream";
/// .nuspec 内容类型（XML 清单）。
const NUSPEC_CONTENT_TYPE: &str = "application/xml";
/// 服务索引 / 版本列表内容类型。
const JSON_CONTENT_TYPE: &str = "application/json";

/// 扁平容器路径前缀（NuGet v3 PackageBaseAddress 资源根）。
pub const FLATCONTAINER_PREFIX: &str = "v3-flatcontainer";
/// 服务索引相对路径（`GET /{repo}/v3/index.json`）。
pub const SERVICE_INDEX_PATH: &str = "v3/index.json";
/// 发布端点相对路径（`PUT /{repo}/v3/package`）。
pub const PUBLISH_PATH: &str = "v3/package";

/// NuGet 格式处理器：以小写规范化的 `{id}/{version}/...` 定位 .nupkg / .nuspec。
pub struct NuGetFormat;

/// NuGet 协议错误：上传包不合法 / 版本已存在等。
#[derive(Debug, thiserror::Error, PartialEq, Eq)]
pub enum NuGetError {
    /// 上传的 .nupkg 不合法（非 zip / 缺 .nuspec / .nuspec 解析失败 / 缺 id/version）。
    #[error("NuGet 包不合法: {0}")]
    InvalidPackage(String),
    /// 该 id+version 已发布，按 NuGet 默认策略不可覆盖（FR-61）。
    #[error("包 {0} 版本 {1} 已发布，不可覆盖")]
    VersionExists(String, String),
}

/// 从 .nuspec 解析出的包标识。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct PackageIdentity {
    /// 包 id（保留 .nuspec 原始大小写，用于展示）。
    pub id: String,
    /// 版本号（保留 .nuspec 原始大小写）。
    pub version: String,
}

impl NuGetFormat {
    /// 服务索引相对路径（既作 proxy 回源 rel_path，也作端点逻辑标识）。
    pub const SERVICE_INDEX_PATH: &'static str = SERVICE_INDEX_PATH;

    /// 规范化包 id：小写（NuGet flat container 约定 id 全小写）。
    pub fn normalize_id(id: &str) -> String {
        id.to_ascii_lowercase()
    }

    /// 规范化版本号：小写（语义化版本预发布段大小写不敏感，flat container 用小写键）。
    pub fn normalize_version(version: &str) -> String {
        version.to_ascii_lowercase()
    }

    /// 拼 .nupkg 在仓库内的存储键：`v3-flatcontainer/{id}/{version}/{id}.{version}.nupkg`（均小写）。
    ///
    /// 存储键带扁平容器前缀，与对外下载 URL 的仓库内段一致，使 hosted 直传落键与 proxy 回源 rel_path
    /// 统一：proxy 仓库的 `upstream_url` 配为上游服务根（如 `https://api.nuget.org`），通用机理以
    /// `{upstream_url}/{存储键}` 拼出上游扁平容器地址，无需为代理另设第二个基址。
    pub fn nupkg_path(id: &str, version: &str) -> String {
        let id = Self::normalize_id(id);
        let version = Self::normalize_version(version);
        format!("{FLATCONTAINER_PREFIX}/{id}/{version}/{id}.{version}.nupkg")
    }

    /// 拼 .nuspec 在仓库内的存储键：`v3-flatcontainer/{id}/{version}/{id}.nuspec`（均小写）。
    pub fn nuspec_path(id: &str, version: &str) -> String {
        let id = Self::normalize_id(id);
        let version = Self::normalize_version(version);
        format!("{FLATCONTAINER_PREFIX}/{id}/{version}/{id}.nuspec")
    }

    /// 拼扁平容器版本列表的相对路径：`v3-flatcontainer/{id}/index.json`（id 小写）。
    ///
    /// 既作 proxy 回源版本列表的 rel_path，也作版本列表端点的逻辑标识；版本列表本身动态生成、不落盘。
    pub fn versions_index_path(id: &str) -> String {
        let id = Self::normalize_id(id);
        format!("{FLATCONTAINER_PREFIX}/{id}/index.json")
    }

    /// 据存储键前缀筛出该包的 .nupkg 存储路径前缀（`v3-flatcontainer/{id}/`），用于动态汇总版本。
    fn package_storage_prefix(id: &str) -> String {
        format!("{FLATCONTAINER_PREFIX}/{}/", Self::normalize_id(id))
    }

    /// 把 .nupkg（zip）容器内根级的 `*.nuspec` 条目字节读出。
    ///
    /// .nupkg 即标准 zip，清单为根目录下唯一的 `*.nuspec`；遍历条目取第一个根级 .nuspec。
    /// 非 zip / 无 .nuspec / 读取失败均返回 [`NuGetError::InvalidPackage`]。
    pub fn read_nuspec_from_nupkg(nupkg: &[u8]) -> Result<Vec<u8>, NuGetError> {
        let cursor = std::io::Cursor::new(nupkg);
        let mut archive = zip::ZipArchive::new(cursor)
            .map_err(|e| NuGetError::InvalidPackage(format!("不是有效的 zip 包: {e}")))?;

        // 找到根级（不含路径分隔）的 .nuspec 条目名
        let nuspec_name = (0..archive.len()).find_map(|i| {
            let entry = archive.by_index(i).ok()?;
            let name = entry.name();
            let is_root = !name.contains('/');
            let is_nuspec = name.to_ascii_lowercase().ends_with(".nuspec");
            if is_root && is_nuspec {
                Some(name.to_string())
            } else {
                None
            }
        });
        let nuspec_name = nuspec_name
            .ok_or_else(|| NuGetError::InvalidPackage("包内未找到根级 .nuspec".to_string()))?;

        let mut entry = archive
            .by_name(&nuspec_name)
            .map_err(|e| NuGetError::InvalidPackage(format!("读取 .nuspec 条目失败: {e}")))?;
        let mut buf = Vec::new();
        entry
            .read_to_end(&mut buf)
            .map_err(|e| NuGetError::InvalidPackage(format!("解压 .nuspec 失败: {e}")))?;
        Ok(buf)
    }

    /// 解析 .nuspec（XML），取 `<metadata>` 下的 `<id>` 与 `<version>`。
    ///
    /// 用 quick-xml 流式读取，定位 `metadata` 内的直接子元素 `id` / `version` 文本；
    /// 命名空间前缀（如 `<id>`）以本地名匹配。缺任一字段返回 [`NuGetError::InvalidPackage`]。
    pub fn parse_nuspec(xml: &[u8]) -> Result<PackageIdentity, NuGetError> {
        use quick_xml::events::Event;
        use quick_xml::Reader;

        let mut reader = Reader::from_reader(xml);
        reader.config_mut().trim_text(true);

        let mut in_metadata = false;
        // 当前正处于的目标字段（仅在 metadata 直接子元素层级采集）
        let mut current: Option<Field> = None;
        let mut depth_in_metadata = 0usize;
        let mut id: Option<String> = None;
        let mut version: Option<String> = None;

        let mut buf = Vec::new();
        loop {
            match reader.read_event_into(&mut buf) {
                Ok(Event::Start(e)) => {
                    let local = local_name(e.name().as_ref());
                    if !in_metadata {
                        if local == "metadata" {
                            in_metadata = true;
                            depth_in_metadata = 0;
                        }
                        continue;
                    }
                    depth_in_metadata += 1;
                    // 仅采集 metadata 的直接子元素（depth == 1）
                    if depth_in_metadata == 1 {
                        current = match local.as_str() {
                            "id" => Some(Field::Id),
                            "version" => Some(Field::Version),
                            _ => None,
                        };
                    }
                }
                Ok(Event::Text(e)) => {
                    if let Some(field) = current {
                        // xml_content 按 XML 实体（如 &amp;）解码文本内容
                        let text = e
                            .xml_content(quick_xml::XmlVersion::default())
                            .map_err(|err| {
                                NuGetError::InvalidPackage(format!(".nuspec 文本解析失败: {err}"))
                            })?
                            .to_string();
                        match field {
                            Field::Id => id = Some(text),
                            Field::Version => version = Some(text),
                        }
                    }
                }
                Ok(Event::End(e)) => {
                    let local = local_name(e.name().as_ref());
                    if in_metadata {
                        if local == "metadata" && depth_in_metadata == 0 {
                            // metadata 闭合，后续不再采集
                            in_metadata = false;
                            continue;
                        }
                        if depth_in_metadata > 0 {
                            depth_in_metadata -= 1;
                            if depth_in_metadata == 0 {
                                current = None;
                            }
                        }
                    }
                }
                Ok(Event::Eof) => break,
                Err(e) => {
                    return Err(NuGetError::InvalidPackage(format!(
                        ".nuspec XML 解析失败: {e}"
                    )))
                }
                _ => {}
            }
            buf.clear();
        }

        let id = id
            .filter(|s| !s.is_empty())
            .ok_or_else(|| NuGetError::InvalidPackage(".nuspec 缺少 id".to_string()))?;
        let version = version
            .filter(|s| !s.is_empty())
            .ok_or_else(|| NuGetError::InvalidPackage(".nuspec 缺少 version".to_string()))?;
        Ok(PackageIdentity { id, version })
    }

    /// 生成 v3 服务索引 JSON：声明扁平容器（PackageBaseAddress）与发布端点（PackagePublish）。
    ///
    /// `@id` 指向本仓库对外地址，使客户端经本服务拉取版本列表 / .nupkg 与执行 push。
    pub fn service_index(public_base_url: &str, repo_name: &str) -> Value {
        let base = public_base_url.trim_end_matches('/');
        let flat = format!("{base}/{repo_name}/{FLATCONTAINER_PREFIX}/");
        let publish = format!("{base}/{repo_name}/{PUBLISH_PATH}");
        json!({
            "version": "3.0.0",
            "resources": [
                {
                    "@id": flat,
                    "@type": "PackageBaseAddress/3.0.0",
                    "comment": "扁平容器：版本列表与 .nupkg / .nuspec 下载"
                },
                {
                    "@id": publish,
                    "@type": "PackagePublish/2.0.0",
                    "comment": "nuget push 发布端点"
                }
            ]
        })
    }

    /// 把版本字符串列表组装为扁平容器版本列表 JSON：`{"versions":[...]}`。
    ///
    /// 版本统一小写、去重、升序（纯字符串排序，足够 flat container 用途）。
    pub fn versions_index(versions: &[String]) -> Value {
        let mut normalized: Vec<String> = versions
            .iter()
            .map(|v| Self::normalize_version(v))
            .collect();
        normalized.sort();
        normalized.dedup();
        json!({ "versions": normalized })
    }

    /// 从仓库制品列表中汇总某包的所有版本（据 .nupkg 存储路径反解版本段）。
    ///
    /// 只认 `{id}/{version}/{id}.{version}.nupkg` 形态的 .nupkg 路径，提取其 version 段。
    pub fn collect_versions(records: &[ArtifactRecord], id: &str) -> Vec<String> {
        let prefix = Self::package_storage_prefix(id);
        records
            .iter()
            .filter_map(|r| {
                let rest = r.path.strip_prefix(&prefix)?;
                // rest 形如 `{version}/{id}.{version}.nupkg`
                if !rest.ends_with(".nupkg") {
                    return None;
                }
                let version = rest.split('/').next()?;
                if version.is_empty() {
                    None
                } else {
                    Some(version.to_string())
                }
            })
            .collect()
    }

    /// 把上游 v3 服务索引中各 resource 的 `@id` 重写为指向本代理仓库。
    ///
    /// 仅重写本服务已实现的资源类型（PackageBaseAddress 扁平容器），使经代理拉取版本列表与
    /// .nupkg；其余资源原样保留（其端点未在本服务实现，由客户端按需处理 / 忽略）。
    pub fn rewrite_proxy_service_index(
        upstream: &[u8],
        public_base_url: &str,
        repo_name: &str,
    ) -> Result<Vec<u8>, NuGetError> {
        let base = public_base_url.trim_end_matches('/');
        let mut doc: Value = serde_json::from_slice(upstream)
            .map_err(|e| NuGetError::InvalidPackage(format!("上游服务索引解析失败: {e}")))?;

        let flat = format!("{base}/{repo_name}/{FLATCONTAINER_PREFIX}/");
        if let Some(resources) = doc.get_mut("resources").and_then(Value::as_array_mut) {
            for res in resources.iter_mut() {
                let Some(ty) = res.get("@type").and_then(Value::as_str) else {
                    continue;
                };
                // PackageBaseAddress 各版本（3.0.0 等）统一重写为本代理扁平容器
                if ty.starts_with("PackageBaseAddress/") {
                    if let Some(obj) = res.as_object_mut() {
                        obj.insert("@id".to_string(), Value::String(flat.clone()));
                    }
                }
            }
        }
        serde_json::to_vec(&doc)
            .map_err(|e| NuGetError::InvalidPackage(format!("服务索引序列化失败: {e}")))
    }
}

/// .nuspec 解析时当前正在采集的目标字段。
#[derive(Debug, Clone, Copy)]
enum Field {
    /// 包 id。
    Id,
    /// 版本号。
    Version,
}

/// 取 XML 限定名的本地名（去掉命名空间前缀 `ns:` 部分）。
fn local_name(qname: &[u8]) -> String {
    let s = String::from_utf8_lossy(qname);
    match s.rsplit_once(':') {
        Some((_, local)) => local.to_string(),
        None => s.to_string(),
    }
}

impl Format for NuGetFormat {
    fn name(&self) -> &'static str {
        "nuget"
    }

    fn parse_path(&self, raw_path: &str) -> Result<ArtifactCoordinates, PathError> {
        // NuGet 以归一化后的仓库内路径作为制品键（.nupkg / .nuspec 均按存储布局拼成）
        let path = normalize_repo_path(raw_path)?;
        Ok(ArtifactCoordinates { path })
    }

    fn can_overwrite(&self, _existing: &ArtifactRecord) -> bool {
        // NuGet 默认策略：已发布的 .nupkg / .nuspec 不可覆盖（FR-61）
        false
    }

    fn content_type(&self, coords: &ArtifactCoordinates) -> Option<String> {
        let lower = coords.path.to_ascii_lowercase();
        let ct = if lower.ends_with(".nupkg") {
            NUPKG_CONTENT_TYPE
        } else if lower.ends_with(".nuspec") {
            NUSPEC_CONTENT_TYPE
        } else if lower.ends_with(".json") {
            JSON_CONTENT_TYPE
        } else {
            return None;
        };
        Some(ct.to_string())
    }

    fn usage_snippets(
        &self,
        public_base_url: &str,
        repo_name: &str,
        _coords: &ArtifactCoordinates,
    ) -> Vec<UsageSnippet> {
        let base = public_base_url.trim_end_matches('/');
        let source = format!("{base}/{repo_name}/{SERVICE_INDEX_PATH}");
        vec![
            UsageSnippet {
                title: "添加源".to_string(),
                language: "bash".to_string(),
                // 仅占位 api-key，不写真实凭据
                content: format!(
                    "dotnet nuget add source {source} --name jianartifact --username <user> --password ${{NUGET_API_KEY}}"
                ),
            },
            UsageSnippet {
                title: "安装包".to_string(),
                language: "bash".to_string(),
                content: format!("dotnet add package <包名> --source {source}"),
            },
        ]
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// 构造一条最小制品记录，仅用于覆盖策略 / 内容类型 / 版本汇总判定。
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

    /// 构造一份最小 .nupkg（zip，内含根级 .nuspec）。
    fn 构造_nupkg(nuspec_name: &str, nuspec_xml: &str) -> Vec<u8> {
        use zip::write::SimpleFileOptions;
        use zip::ZipWriter;
        let mut buf = Vec::new();
        {
            let cursor = std::io::Cursor::new(&mut buf);
            let mut zip = ZipWriter::new(cursor);
            // 用 store（不压缩）也可被读取；这里用 deflate 验证解压链路
            let opts =
                SimpleFileOptions::default().compression_method(zip::CompressionMethod::Deflated);
            use std::io::Write;
            zip.start_file(nuspec_name, opts).unwrap();
            zip.write_all(nuspec_xml.as_bytes()).unwrap();
            // 再放一个非 .nuspec 文件，验证只挑 .nuspec
            zip.start_file("lib/net8.0/x.dll", opts).unwrap();
            zip.write_all(b"FAKE-DLL").unwrap();
            zip.finish().unwrap();
        }
        buf
    }

    #[test]
    fn 名称为_nuget() {
        assert_eq!(NuGetFormat.name(), "nuget");
    }

    #[test]
    fn id_与_version_小写规范化() {
        assert_eq!(
            NuGetFormat::normalize_id("Newtonsoft.Json"),
            "newtonsoft.json"
        );
        assert_eq!(NuGetFormat::normalize_version("13.0.3-Beta"), "13.0.3-beta");
    }

    #[test]
    fn 存储路径按小写拼接() {
        assert_eq!(
            NuGetFormat::nupkg_path("Newtonsoft.Json", "13.0.3"),
            "v3-flatcontainer/newtonsoft.json/13.0.3/newtonsoft.json.13.0.3.nupkg"
        );
        assert_eq!(
            NuGetFormat::nuspec_path("Newtonsoft.Json", "13.0.3"),
            "v3-flatcontainer/newtonsoft.json/13.0.3/newtonsoft.json.nuspec"
        );
        assert_eq!(
            NuGetFormat::versions_index_path("Newtonsoft.Json"),
            "v3-flatcontainer/newtonsoft.json/index.json"
        );
    }

    #[test]
    fn 解析_nuspec_取_id_与_version() {
        let xml = r#"<?xml version="1.0"?>
            <package xmlns="http://schemas.microsoft.com/packaging/2013/05/nuspec.xsd">
              <metadata>
                <id>My.Package</id>
                <version>1.2.3</version>
                <authors>someone</authors>
              </metadata>
            </package>"#;
        let pid = NuGetFormat::parse_nuspec(xml.as_bytes()).unwrap();
        assert_eq!(pid.id, "My.Package");
        assert_eq!(pid.version, "1.2.3");
    }

    #[test]
    fn 解析_nuspec_带命名空间前缀() {
        // 带前缀的限定名应按本地名匹配
        let xml = r#"<ns:package xmlns:ns="x">
              <ns:metadata>
                <ns:id>Pre.Fixed</ns:id>
                <ns:version>0.1.0</ns:version>
              </ns:metadata>
            </ns:package>"#;
        let pid = NuGetFormat::parse_nuspec(xml.as_bytes()).unwrap();
        assert_eq!(pid.id, "Pre.Fixed");
        assert_eq!(pid.version, "0.1.0");
    }

    #[test]
    fn 解析_nuspec_缺字段报错() {
        let xml = r#"<package><metadata><id>X</id></metadata></package>"#;
        assert!(matches!(
            NuGetFormat::parse_nuspec(xml.as_bytes()),
            Err(NuGetError::InvalidPackage(_))
        ));
    }

    #[test]
    fn 解析_nuspec_不取嵌套层非直接子元素() {
        // dependencies 内的 id 不应被误当成包 id
        let xml = r#"<package><metadata>
                <id>Real.Id</id>
                <version>2.0.0</version>
                <dependencies>
                  <dependency id="Other" version="9.9.9" />
                </dependencies>
              </metadata></package>"#;
        let pid = NuGetFormat::parse_nuspec(xml.as_bytes()).unwrap();
        assert_eq!(pid.id, "Real.Id");
        assert_eq!(pid.version, "2.0.0");
    }

    #[test]
    fn 从_nupkg_读出_nuspec_并解析() {
        let xml =
            r#"<package><metadata><id>Zip.Pkg</id><version>3.1.4</version></metadata></package>"#;
        let nupkg = 构造_nupkg("Zip.Pkg.nuspec", xml);
        let nuspec = NuGetFormat::read_nuspec_from_nupkg(&nupkg).unwrap();
        let pid = NuGetFormat::parse_nuspec(&nuspec).unwrap();
        assert_eq!(pid.id, "Zip.Pkg");
        assert_eq!(pid.version, "3.1.4");
    }

    #[test]
    fn 非_zip_包读_nuspec_报错() {
        assert!(matches!(
            NuGetFormat::read_nuspec_from_nupkg(b"not-a-zip"),
            Err(NuGetError::InvalidPackage(_))
        ));
    }

    #[test]
    fn zip_内无_nuspec_报错() {
        use zip::write::SimpleFileOptions;
        use zip::ZipWriter;
        let mut buf = Vec::new();
        {
            let cursor = std::io::Cursor::new(&mut buf);
            let mut zip = ZipWriter::new(cursor);
            use std::io::Write;
            zip.start_file("readme.txt", SimpleFileOptions::default())
                .unwrap();
            zip.write_all(b"hi").unwrap();
            zip.finish().unwrap();
        }
        assert!(matches!(
            NuGetFormat::read_nuspec_from_nupkg(&buf),
            Err(NuGetError::InvalidPackage(_))
        ));
    }

    #[test]
    fn 服务索引含扁平容器与发布端点() {
        let idx = NuGetFormat::service_index("http://localhost:8080/", "nuget-hosted");
        assert_eq!(idx["version"], "3.0.0");
        let resources = idx["resources"].as_array().unwrap();
        // 扁平容器 @id 指向本仓库、不出现双斜杠
        let flat = resources
            .iter()
            .find(|r| r["@type"] == "PackageBaseAddress/3.0.0")
            .unwrap();
        assert_eq!(
            flat["@id"],
            "http://localhost:8080/nuget-hosted/v3-flatcontainer/"
        );
        let publish = resources
            .iter()
            .find(|r| r["@type"] == "PackagePublish/2.0.0")
            .unwrap();
        assert_eq!(
            publish["@id"],
            "http://localhost:8080/nuget-hosted/v3/package"
        );
    }

    #[test]
    fn 版本列表去重排序且小写() {
        let versions = vec![
            "2.0.0".to_string(),
            "1.0.0".to_string(),
            "1.0.0".to_string(),
            "1.5.0-RC".to_string(),
        ];
        let idx = NuGetFormat::versions_index(&versions);
        let arr = idx["versions"].as_array().unwrap();
        let got: Vec<&str> = arr.iter().map(|v| v.as_str().unwrap()).collect();
        assert_eq!(got, vec!["1.0.0", "1.5.0-rc", "2.0.0"]);
    }

    #[test]
    fn 从制品列表汇总某包版本() {
        let records = vec![
            记录("v3-flatcontainer/pkg/1.0.0/pkg.1.0.0.nupkg"),
            记录("v3-flatcontainer/pkg/1.0.0/pkg.nuspec"),
            记录("v3-flatcontainer/pkg/2.0.0/pkg.2.0.0.nupkg"),
            // 其他包不计入
            记录("v3-flatcontainer/other/9.9.9/other.9.9.9.nupkg"),
        ];
        let mut versions = NuGetFormat::collect_versions(&records, "pkg");
        versions.sort();
        assert_eq!(versions, vec!["1.0.0", "2.0.0"]);
    }

    #[test]
    fn 代理服务索引重写扁平容器指向本仓库() {
        let upstream = json!({
            "version": "3.0.0",
            "resources": [
                {
                    "@id": "https://api.nuget.org/v3-flatcontainer/",
                    "@type": "PackageBaseAddress/3.0.0"
                },
                {
                    "@id": "https://api.nuget.org/v3/registration5-gz-semver2/",
                    "@type": "RegistrationsBaseUrl/Versioned"
                }
            ]
        })
        .to_string()
        .into_bytes();
        let rewritten =
            NuGetFormat::rewrite_proxy_service_index(&upstream, "http://localhost:8080", "mirror")
                .unwrap();
        let doc: Value = serde_json::from_slice(&rewritten).unwrap();
        let resources = doc["resources"].as_array().unwrap();
        // 扁平容器重写为本仓库
        assert_eq!(
            resources[0]["@id"],
            "http://localhost:8080/mirror/v3-flatcontainer/"
        );
        // 未实现的资源保持上游原值
        assert_eq!(
            resources[1]["@id"],
            "https://api.nuget.org/v3/registration5-gz-semver2/"
        );
    }

    #[test]
    fn 内容类型按扩展名推断() {
        let ct = |p: &str| {
            NuGetFormat.content_type(&ArtifactCoordinates {
                path: p.to_string(),
            })
        };
        assert_eq!(
            ct("pkg/1.0.0/pkg.1.0.0.nupkg").as_deref(),
            Some("application/octet-stream")
        );
        assert_eq!(
            ct("pkg/1.0.0/pkg.nuspec").as_deref(),
            Some("application/xml")
        );
        assert_eq!(
            ct("v3-flatcontainer/pkg/index.json").as_deref(),
            Some("application/json")
        );
        assert_eq!(ct("no-ext"), None);
    }

    #[test]
    fn nuget_一律不可覆盖() {
        assert!(!NuGetFormat.can_overwrite(&记录("pkg/1.0.0/pkg.1.0.0.nupkg")));
        assert!(!NuGetFormat.can_overwrite(&记录("pkg/1.0.0/pkg.nuspec")));
    }

    #[test]
    fn 使用片段含添加源与安装且不含真实凭据() {
        let coords = ArtifactCoordinates {
            path: "pkg/1.0.0/pkg.1.0.0.nupkg".to_string(),
        };
        let snippets =
            NuGetFormat.usage_snippets("http://localhost:8080/", "nuget-hosted", &coords);
        assert_eq!(snippets.len(), 2);
        assert!(snippets[0]
            .content
            .contains("http://localhost:8080/nuget-hosted/v3/index.json"));
        // 占位凭据而非真实 Token
        assert!(snippets[0].content.contains("${NUGET_API_KEY}"));
        assert!(snippets[1].content.contains("dotnet add package"));
    }
}
