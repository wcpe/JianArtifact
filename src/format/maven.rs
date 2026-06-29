//! Maven 格式（FR-14，hosted + proxy）：以 Maven 仓库布局直存直取，按 Maven 原生语义判定覆盖。
//!
//! 作为统一 [`Format`] trait 的实现，复用通用制品机理（[`super::ArtifactService`]）的
//! 存储 / 代理 / 四校验和机理，本模块只负责 Maven 自身协议：路径映射、内容类型、
//! 覆盖 / 不可变策略（FR-61）与使用方式片段（FR-68）。不在此重造存储 / 代理 / 校验和。
//!
//! Maven 布局：`/{repo}/{groupId 以 / 分隔}/{artifactId}/{version}/{artifactId}-{version}.{扩展}`，
//! 以及目录级 `maven-metadata.xml`，再加各文件的校验和 sidecar（`.sha1` / `.md5` / `.sha256` / `.sha512`）。
//! 通用机理已为每个落盘文件算好四摘要，sidecar 内容即取对应摘要——客户端各文件独立 PUT / GET，
//! 通用机理逐文件存取，本格式不需特殊聚合。

use std::io::Read;

use crate::meta::ArtifactRecord;

use super::{
    normalize_repo_path, ArtifactCoordinates, Format, PathError, UsageSnippet, VulnCoordinate,
};

/// OSV 中 Maven 生态的标识（与公告 `package.ecosystem` 对齐）。
const MAVEN_ECOSYSTEM: &str = "Maven";

/// SNAPSHOT 版本后缀：版本目录以此结尾即为快照版（可覆盖）。
const SNAPSHOT_SUFFIX: &str = "-SNAPSHOT";

/// Maven 元数据文件名：目录级 `maven-metadata.xml`，随发布更新（可覆盖）。
const MAVEN_METADATA: &str = "maven-metadata.xml";

/// 校验和 / 签名 sidecar 扩展名：伴随主文件，随主文件可覆盖性走（这里一律允许覆盖，
/// 因主文件本身的覆盖策略已在主文件 PUT 时把关，sidecar 仅是其摘要的镜像）。
const SIDECAR_EXTENSIONS: [&str; 5] = ["sha1", "md5", "sha256", "sha512", "asc"];

/// Maven 格式处理器：仓库内路径即制品键，覆盖策略按 release/snapshot/metadata 区分。
pub struct MavenFormat;

impl MavenFormat {
    /// 判断给定仓库内路径是否为"可覆盖"文件。
    ///
    /// 规则（FR-61，对齐 docs/API.md 覆盖语义）：
    /// - `maven-metadata.xml`（及其 sidecar）随发布更新 → 可覆盖；
    /// - 校验和 / 签名 sidecar（`.sha1` / `.md5` / `.sha256` / `.sha512` / `.asc`）→ 可覆盖；
    /// - 路径含 `-SNAPSHOT` 版本段 → 快照版可覆盖；
    /// - 其余视为 release 正式构件 → 不可覆盖。
    fn is_overwritable(path: &str) -> bool {
        // 取末段文件名做基于文件名的判定
        let file_name = path.rsplit('/').next().unwrap_or(path);

        // ① maven-metadata.xml 自身允许更新
        if file_name == MAVEN_METADATA {
            return true;
        }

        // ② 任何 sidecar（含 maven-metadata.xml.sha1 等）允许更新
        if let Some(ext) = file_name.rsplit('.').next() {
            if SIDECAR_EXTENSIONS.contains(&ext.to_ascii_lowercase().as_str()) {
                return true;
            }
        }

        // ③ SNAPSHOT 版本：版本段以 -SNAPSHOT 结尾即为快照
        //    布局中倒数第二段为 version 目录；任一路径段命中 -SNAPSHOT 即判为快照构件
        if path.split('/').any(|seg| seg.ends_with(SNAPSHOT_SUFFIX)) {
            return true;
        }

        // ④ 其余为 release 正式构件，不可覆盖
        false
    }

    /// 据 GAV 与文件名拼出制品在仓库内的存储路径（Maven 布局）。
    ///
    /// 布局 `{groupId 点转斜杠}/{artifactId}/{version}/{文件名}`，供 Web 上传按表单 GAV 定位坐标。
    /// 各段做基础清洗（去首尾空白）；groupId 的 `.` 统一转 `/`，路径合法性最终由 `parse_path` 归一化把关。
    pub fn artifact_path(
        group_id: &str,
        artifact_id: &str,
        version: &str,
        file_name: &str,
    ) -> String {
        let group_path = group_id.trim().replace('.', "/");
        format!(
            "{}/{}/{}/{}",
            group_path.trim_matches('/'),
            artifact_id.trim(),
            version.trim(),
            file_name.trim()
        )
    }

    /// 从 jar（zip 容器）内嵌的 Maven 元数据提取 GAV（FR-120）。
    ///
    /// jar 即标准 zip：Maven 构建在 `META-INF/maven/<g>/<a>/pom.xml` 内嵌项目坐标，
    /// 同目录另有 `pom.properties`（`groupId` / `artifactId` / `version` 键值）。
    /// 优先解析 pom.xml（项目级 `groupId` / `version` 缺失时回落 `<parent>` 继承），
    /// pom.xml 不存在或字段不全时回落 pom.properties。
    /// 非 zip / 无内嵌 pom / 字段不全一律返回 None，调用方据此回落用户提供坐标（不崩）。
    pub fn parse_gav_from_jar(jar: &[u8]) -> Option<Gav> {
        let cursor = std::io::Cursor::new(jar);
        let mut archive = zip::ZipArchive::new(cursor).ok()?;

        // 优先内嵌 pom.xml（含 parent 继承），其次 pom.properties 兜底
        if let Some(name) = find_embedded_maven_entry(&mut archive, "pom.xml") {
            if let Some(bytes) = read_zip_entry(&mut archive, &name) {
                if let Some(gav) = parse_gav_from_pom_xml(&bytes) {
                    return Some(gav);
                }
            }
        }
        if let Some(name) = find_embedded_maven_entry(&mut archive, "pom.properties") {
            if let Some(bytes) = read_zip_entry(&mut archive, &name) {
                if let Some(gav) = parse_gav_from_pom_properties(&bytes) {
                    return Some(gav);
                }
            }
        }
        None
    }

    /// 判断路径是否为校验和 / 签名 sidecar（`.sha1` / `.md5` / `.sha256` / `.sha512` / `.asc`）。
    ///
    /// 服务端为主构件补齐校验和 sidecar 时据此跳过 sidecar 自身，避免生成「sidecar 的 sidecar」。
    pub fn is_sidecar(path: &str) -> bool {
        let file_name = path.rsplit('/').next().unwrap_or(path);
        file_name
            .rsplit('.')
            .next()
            .map(|ext| SIDECAR_EXTENSIONS.contains(&ext.to_ascii_lowercase().as_str()))
            .unwrap_or(false)
    }

    /// 拼某 GAV 的 pom 在仓库内的存储路径（`{group路径}/{a}/{v}/{a}-{v}.pom`）。
    pub fn pom_path(group_id: &str, artifact_id: &str, version: &str) -> String {
        let file = format!("{}-{}.pom", artifact_id.trim(), version.trim());
        Self::artifact_path(group_id, artifact_id, version, &file)
    }

    /// 拼 artifact 级 `maven-metadata.xml` 在仓库内的存储路径（`{group路径}/{a}/maven-metadata.xml`）。
    pub fn artifact_metadata_path(group_id: &str, artifact_id: &str) -> String {
        let group_path = group_id.trim().replace('.', "/");
        format!(
            "{}/{}/{}",
            group_path.trim_matches('/'),
            artifact_id.trim(),
            MAVEN_METADATA
        )
    }

    /// 反解仓库内路径为 GAV（布局 `{group路径}/{a}/{v}/{文件}`，≥4 段）；无法构成返回 None。
    ///
    /// 对外暴露给写入后编排（FR-121）判定本次写入归属的 artifact 坐标。
    pub fn gav_from_path(path: &str) -> Option<Gav> {
        Gav::from_path(path)
    }

    /// 拼某 `{groupId}/{artifactId}` 的版本目录前缀（`{group路径}/{artifactId}/`）。
    ///
    /// 既用于按前缀列举该 artifact 的全部制品，也用于版本聚合时筛选归属记录。
    pub fn artifact_prefix(group_id: &str, artifact_id: &str) -> String {
        let group_path = group_id.trim().replace('.', "/");
        format!("{}/{}/", group_path.trim_matches('/'), artifact_id.trim())
    }

    /// 据主构件文件名扩展名推断 Maven `packaging`（war / ear，默认 jar）。
    pub fn derive_packaging(file_name: &str) -> &'static str {
        match file_name
            .rsplit('.')
            .next()
            .map(str::to_ascii_lowercase)
            .as_deref()
        {
            Some("war") => "war",
            Some("ear") => "ear",
            _ => "jar",
        }
    }

    /// 从 jar（zip）内嵌的 `META-INF/maven/.../pom.xml` 原样取出 pom 字节（FR-121 pom 兜底第二级）。
    ///
    /// 与 [`Self::parse_gav_from_jar`] 共用条目定位；返回 pom.xml 原始字节供按布局落盘，非 zip / 无内嵌 pom 返回 None。
    pub fn extract_embedded_pom(jar: &[u8]) -> Option<Vec<u8>> {
        let cursor = std::io::Cursor::new(jar);
        let mut archive = zip::ZipArchive::new(cursor).ok()?;
        let name = find_embedded_maven_entry(&mut archive, "pom.xml")?;
        read_zip_entry(&mut archive, &name)
    }

    /// 按 GAV 生成最小合法 pom（FR-121 pom 兜底第三级）：`modelVersion` + GAV + `packaging`。
    pub fn build_minimal_pom(
        group_id: &str,
        artifact_id: &str,
        version: &str,
        packaging: &str,
    ) -> Vec<u8> {
        use std::fmt::Write as _;
        let mut xml = String::new();
        xml.push_str("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n");
        xml.push_str("<project xmlns=\"http://maven.apache.org/POM/4.0.0\">\n");
        xml.push_str("  <modelVersion>4.0.0</modelVersion>\n");
        let _ = writeln!(xml, "  <groupId>{}</groupId>", xml_escape(group_id.trim()));
        let _ = writeln!(
            xml,
            "  <artifactId>{}</artifactId>",
            xml_escape(artifact_id.trim())
        );
        let _ = writeln!(xml, "  <version>{}</version>", xml_escape(version.trim()));
        let _ = writeln!(
            xml,
            "  <packaging>{}</packaging>",
            xml_escape(packaging.trim())
        );
        xml.push_str("</project>\n");
        xml.into_bytes()
    }

    /// 从全仓制品记录聚合某 `{groupId}/{artifactId}` 下的版本（FR-121）。
    ///
    /// 取前缀 `{group路径}/{artifactId}/` 下记录的版本段，按各版本**首见 `created_at`** 升序去重；
    /// `last_updated` 取全部命中记录 `created_at` 的最大值、保留数字字符截 14 位为 `yyyyMMddHHmmss`。
    pub fn collect_versions(
        records: &[ArtifactRecord],
        group_id: &str,
        artifact_id: &str,
    ) -> MavenVersions {
        let prefix = Self::artifact_prefix(group_id, artifact_id);

        // 版本 → 最早 created_at（作排序键）
        let mut earliest: std::collections::HashMap<String, String> =
            std::collections::HashMap::new();
        let mut last_updated_raw: Option<String> = None;
        for r in records {
            let Some(rest) = r.path.strip_prefix(&prefix) else {
                continue;
            };
            // rest 形如 `{version}/{文件...}`；必须含 '/'，排除直接位于 {a} 下的 maven-metadata.xml 等
            let Some((version, _)) = rest.split_once('/') else {
                continue;
            };
            if version.is_empty() {
                continue;
            }
            earliest
                .entry(version.to_string())
                .and_modify(|e| {
                    if r.created_at < *e {
                        *e = r.created_at.clone();
                    }
                })
                .or_insert_with(|| r.created_at.clone());
            if last_updated_raw
                .as_deref()
                .is_none_or(|m| m < r.created_at.as_str())
            {
                last_updated_raw = Some(r.created_at.clone());
            }
        }

        // 按 (最早 created_at, 版本字符串) 升序排，得部署序版本列表
        let mut paired: Vec<(String, String)> = earliest.into_iter().map(|(v, c)| (c, v)).collect();
        paired.sort();
        let versions = paired.into_iter().map(|(_, v)| v).collect();

        let last_updated = last_updated_raw
            .map(|s| s.chars().filter(char::is_ascii_digit).take(14).collect())
            .unwrap_or_default();

        MavenVersions {
            versions,
            last_updated,
        }
    }

    /// 生成 artifact 级 `maven-metadata.xml` 字节（FR-121）。
    ///
    /// `latest` 取版本列表末位（部署序最新，含 SNAPSHOT）；`release` 取末位非 SNAPSHOT 版本（无则省略该元素）。
    pub fn build_artifact_metadata(
        group_id: &str,
        artifact_id: &str,
        versions: &MavenVersions,
    ) -> Vec<u8> {
        use std::fmt::Write as _;
        let latest = versions.versions.last().cloned().unwrap_or_default();
        let release = versions
            .versions
            .iter()
            .rev()
            .find(|v| !v.ends_with(SNAPSHOT_SUFFIX));

        let mut xml = String::new();
        xml.push_str("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n");
        xml.push_str("<metadata>\n");
        let _ = writeln!(xml, "  <groupId>{}</groupId>", xml_escape(group_id.trim()));
        let _ = writeln!(
            xml,
            "  <artifactId>{}</artifactId>",
            xml_escape(artifact_id.trim())
        );
        xml.push_str("  <versioning>\n");
        let _ = writeln!(xml, "    <latest>{}</latest>", xml_escape(&latest));
        if let Some(rel) = release {
            let _ = writeln!(xml, "    <release>{}</release>", xml_escape(rel));
        }
        xml.push_str("    <versions>\n");
        for v in &versions.versions {
            let _ = writeln!(xml, "      <version>{}</version>", xml_escape(v));
        }
        xml.push_str("    </versions>\n");
        let _ = writeln!(
            xml,
            "    <lastUpdated>{}</lastUpdated>",
            xml_escape(&versions.last_updated)
        );
        xml.push_str("  </versioning>\n");
        xml.push_str("</metadata>\n");
        xml.into_bytes()
    }
}

impl Format for MavenFormat {
    fn name(&self) -> &'static str {
        "maven"
    }

    fn parse_path(&self, raw_path: &str) -> Result<ArtifactCoordinates, PathError> {
        // Maven 以归一化后的仓库内路径作为制品坐标（拒绝目录穿越与空路径）
        let path = normalize_repo_path(raw_path)?;
        Ok(ArtifactCoordinates { path })
    }

    fn can_overwrite(&self, existing: &ArtifactRecord) -> bool {
        // 据既有制品的仓库内路径判定：release 主构件不可覆盖，其余按规则放行
        Self::is_overwritable(&existing.path)
    }

    fn content_type(&self, coords: &ArtifactCoordinates) -> Option<String> {
        // 按 Maven 常见制品扩展名粗判；无法判断时返回 None，交由通用默认值处理
        let file_name = coords.path.rsplit('/').next().unwrap_or(&coords.path);
        // maven-metadata.xml 与 .pom 都是 XML
        if file_name == MAVEN_METADATA {
            return Some("application/xml".to_string());
        }
        let ext = coords.path.rsplit('.').next().unwrap_or("");
        let ct = match ext.to_ascii_lowercase().as_str() {
            "jar" | "war" | "ear" => "application/java-archive",
            "pom" | "xml" => "application/xml",
            "module" | "json" => "application/json",
            "sha1" | "md5" | "sha256" | "sha512" => "text/plain; charset=utf-8",
            "asc" => "text/plain; charset=utf-8",
            "zip" => "application/zip",
            "tar" => "application/x-tar",
            "gz" | "tgz" => "application/gzip",
            // 其余一律未知，返回 None（不强行套 octet-stream，留默认层决定）
            _ => return None,
        };
        Some(ct.to_string())
    }

    fn usage_snippets(
        &self,
        public_base_url: &str,
        repo_name: &str,
        coords: &ArtifactCoordinates,
    ) -> Vec<UsageSnippet> {
        // 据制品路径反解 GAV，能解出则给 <dependency> + settings.xml 接入片段
        let base = public_base_url.trim_end_matches('/');
        let repo_url = format!("{base}/{repo_name}");

        let mut snippets = Vec::new();

        if let Some(gav) = Gav::from_path(&coords.path) {
            // 依赖坐标片段（FR-68）
            snippets.push(UsageSnippet {
                title: "依赖坐标".to_string(),
                language: "xml".to_string(),
                content: format!(
                    "<dependency>\n  <groupId>{}</groupId>\n  <artifactId>{}</artifactId>\n  <version>{}</version>\n</dependency>",
                    gav.group_id, gav.artifact_id, gav.version
                ),
            });
        }

        // 仓库接入片段：settings.xml 指向本仓库（供解析下载 / 部署）
        snippets.push(UsageSnippet {
            title: "仓库接入 (settings.xml)".to_string(),
            language: "xml".to_string(),
            content: format!(
                "<repository>\n  <id>{repo_name}</id>\n  <url>{repo_url}</url>\n</repository>"
            ),
        });

        snippets
    }

    fn vuln_coordinate(&self, coords: &ArtifactCoordinates) -> Option<VulnCoordinate> {
        // 仅对能反解出 GAV 的主构件路径产出坐标；sidecar 与 metadata 不参与匹配。
        // sidecar（.sha1/.md5/.sha256/.sha512/.asc）与 maven-metadata.xml 不是发布的库本体，
        // 跳过它们避免对同一版本重复匹配。
        let file_name = coords.path.rsplit('/').next().unwrap_or(&coords.path);
        if file_name == MAVEN_METADATA {
            return None;
        }
        if let Some(ext) = file_name.rsplit('.').next() {
            if SIDECAR_EXTENSIONS.contains(&ext.to_ascii_lowercase().as_str()) {
                return None;
            }
        }
        let gav = Gav::from_path(&coords.path)?;
        Some(VulnCoordinate {
            ecosystem: MAVEN_ECOSYSTEM.to_string(),
            // OSV Maven 坐标包名为 `groupId:artifactId`
            package: format!("{}:{}", gav.group_id, gav.artifact_id),
            version: gav.version,
        })
    }
}

/// Maven 版本聚合结果（FR-121）：某 artifact 下的版本列表与 lastUpdated。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct MavenVersions {
    /// 版本列表（按各版本首见 `created_at` 升序、去重，末位为最新部署）。
    pub versions: Vec<String>,
    /// lastUpdated（`yyyyMMddHHmmss`，取最大 `created_at` 的数字字符截 14 位；无版本为空串）。
    pub last_updated: String,
}

/// 对 XML 文本值做最小转义（`& < > " '`），用于把 GAV / 版本等坐标安全嵌入元素文本。
fn xml_escape(s: &str) -> String {
    s.replace('&', "&amp;")
        .replace('<', "&lt;")
        .replace('>', "&gt;")
        .replace('"', "&quot;")
        .replace('\'', "&apos;")
}

/// Maven 坐标（GAV）：由仓库内路径反解（[`Gav::from_path`]）或 jar 内嵌 pom
/// 解析（[`MavenFormat::parse_gav_from_jar`]）而来，用于生成依赖片段与免手填坐标。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Gav {
    /// groupId（以 `.` 分隔）。
    pub group_id: String,
    /// artifactId。
    pub artifact_id: String,
    /// version。
    pub version: String,
}

impl Gav {
    /// 从仓库内路径反解 GAV：布局为 `{group/路径}/{artifactId}/{version}/{文件}`。
    ///
    /// 取倒数第三段为 artifactId、倒数第二段为 version、其余前缀段以 `.` 连接为 groupId。
    /// 段数不足（无法构成合法 GAV，如目录级 metadata）时返回 None。
    fn from_path(path: &str) -> Option<Self> {
        let segments: Vec<&str> = path.split('/').filter(|s| !s.is_empty()).collect();
        // 至少需 4 段：group(>=1) + artifactId + version + 文件
        if segments.len() < 4 {
            return None;
        }
        let file_idx = segments.len() - 1;
        let version = segments[file_idx - 1];
        let artifact_id = segments[file_idx - 2];
        let group_segments = &segments[..file_idx - 2];
        if group_segments.is_empty() {
            return None;
        }
        Some(Gav {
            group_id: group_segments.join("."),
            artifact_id: artifact_id.to_string(),
            version: version.to_string(),
        })
    }
}

/// 在 jar 内查找 `META-INF/maven/.../{file_name}` 条目名（取首个命中）。
///
/// Maven 把内嵌坐标固定放在 `META-INF/maven/<groupId>/<artifactId>/` 下，
/// 这里以前缀 `META-INF/maven/` + 末段文件名定位，避免误取其它同名文件。
fn find_embedded_maven_entry<R: Read + std::io::Seek>(
    archive: &mut zip::ZipArchive<R>,
    file_name: &str,
) -> Option<String> {
    let suffix = format!("/{file_name}");
    (0..archive.len()).find_map(|i| {
        let entry = archive.by_index(i).ok()?;
        let name = entry.name();
        if name.starts_with("META-INF/maven/") && name.ends_with(&suffix) {
            Some(name.to_string())
        } else {
            None
        }
    })
}

/// 读取 zip 内指定条目的全部字节（读取失败返回 None）。
fn read_zip_entry<R: Read + std::io::Seek>(
    archive: &mut zip::ZipArchive<R>,
    name: &str,
) -> Option<Vec<u8>> {
    let mut entry = archive.by_name(name).ok()?;
    let mut buf = Vec::new();
    entry.read_to_end(&mut buf).ok()?;
    Some(buf)
}

/// 解析 pom.xml 提取 GAV：项目级 `groupId` / `artifactId` / `version`，
/// `groupId` / `version` 缺失时回落 `<parent>` 同名字段（Maven 继承语义）。
///
/// 以元素本地名栈定位「project 直接子元素」与「project/parent 直接子元素」，
/// 不误取 dependencies / build 等嵌套层的同名标签。三者齐备方返回 Some。
fn parse_gav_from_pom_xml(xml: &[u8]) -> Option<Gav> {
    use quick_xml::events::Event;
    use quick_xml::Reader;

    let mut reader = Reader::from_reader(xml);
    reader.config_mut().trim_text(true);

    // 元素本地名栈（如 ["project", "parent", "groupId"]）
    let mut stack: Vec<String> = Vec::new();
    let mut project_group: Option<String> = None;
    let mut project_artifact: Option<String> = None;
    let mut project_version: Option<String> = None;
    let mut parent_group: Option<String> = None;
    let mut parent_version: Option<String> = None;

    let mut buf = Vec::new();
    loop {
        match reader.read_event_into(&mut buf) {
            Ok(Event::Start(e)) => stack.push(local_name(e.name().as_ref())),
            Ok(Event::End(_)) => {
                stack.pop();
            }
            Ok(Event::Text(e)) => {
                let text = match e.xml_content(quick_xml::XmlVersion::default()) {
                    Ok(t) => t.trim().to_string(),
                    Err(_) => {
                        buf.clear();
                        continue;
                    }
                };
                if text.is_empty() {
                    buf.clear();
                    continue;
                }
                let path: Vec<&str> = stack.iter().map(String::as_str).collect();
                match path.as_slice() {
                    ["project", "groupId"] => project_group = Some(text),
                    ["project", "artifactId"] => project_artifact = Some(text),
                    ["project", "version"] => project_version = Some(text),
                    ["project", "parent", "groupId"] => parent_group = Some(text),
                    ["project", "parent", "version"] => parent_version = Some(text),
                    _ => {}
                }
            }
            Ok(Event::Eof) => break,
            Err(_) => return None,
            _ => {}
        }
        buf.clear();
    }

    Some(Gav {
        group_id: project_group.or(parent_group)?,
        artifact_id: project_artifact?,
        version: project_version.or(parent_version)?,
    })
}

/// 解析 pom.properties 提取 GAV：逐行 `key=value`，取 groupId / artifactId / version。
///
/// 跳过空行与 `#` / `!` 注释行；三者齐备方返回 Some。
fn parse_gav_from_pom_properties(bytes: &[u8]) -> Option<Gav> {
    let text = String::from_utf8_lossy(bytes);
    let mut group_id: Option<String> = None;
    let mut artifact_id: Option<String> = None;
    let mut version: Option<String> = None;
    for line in text.lines() {
        let line = line.trim();
        if line.is_empty() || line.starts_with('#') || line.starts_with('!') {
            continue;
        }
        let Some((key, value)) = line.split_once('=') else {
            continue;
        };
        let value = value.trim().to_string();
        if value.is_empty() {
            continue;
        }
        match key.trim() {
            "groupId" => group_id = Some(value),
            "artifactId" => artifact_id = Some(value),
            "version" => version = Some(value),
            _ => {}
        }
    }
    Some(Gav {
        group_id: group_id?,
        artifact_id: artifact_id?,
        version: version?,
    })
}

/// 取 XML 限定名的本地名（去命名空间前缀 `ns:`）。
fn local_name(qname: &[u8]) -> String {
    let s = String::from_utf8_lossy(qname);
    match s.rsplit_once(':') {
        Some((_, local)) => local.to_string(),
        None => s.to_string(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// 构造一条仅含路径的最小制品记录，供覆盖策略判定用。
    fn 制品(path: &str) -> ArtifactRecord {
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
    fn 名称为_maven() {
        assert_eq!(MavenFormat.name(), "maven");
    }

    #[test]
    fn 解析路径归一化且拒穿越() {
        assert_eq!(
            MavenFormat
                .parse_path("/com/foo/lib/1.0/lib-1.0.jar")
                .unwrap()
                .path,
            "com/foo/lib/1.0/lib-1.0.jar"
        );
        assert_eq!(
            MavenFormat.parse_path("com/../etc/passwd"),
            Err(PathError::Traversal)
        );
    }

    #[test]
    fn 据gav拼制品路径_点转斜杠() {
        // groupId 的点转斜杠，与 artifactId / version / 文件名拼为 Maven 布局
        assert_eq!(
            MavenFormat::artifact_path("com.example.app", "demo", "1.0.0", "demo-1.0.0.jar"),
            "com/example/app/demo/1.0.0/demo-1.0.0.jar"
        );
        // 各段前后空白被清洗，不产生多余斜杠
        assert_eq!(
            MavenFormat::artifact_path(" com.foo ", " lib ", " 2.0 ", " lib-2.0.pom "),
            "com/foo/lib/2.0/lib-2.0.pom"
        );
    }

    #[test]
    fn release_主构件不可覆盖() {
        // release 版的 jar / pom 不可覆盖
        assert!(!MavenFormat.can_overwrite(&制品("com/foo/lib/1.0/lib-1.0.jar")));
        assert!(!MavenFormat.can_overwrite(&制品("com/foo/lib/1.0/lib-1.0.pom")));
        assert!(!MavenFormat.can_overwrite(&制品("org/sample/app/2.3.1/app-2.3.1.war")));
    }

    #[test]
    fn snapshot_版可覆盖() {
        assert!(MavenFormat.can_overwrite(&制品("com/foo/lib/1.0-SNAPSHOT/lib-1.0-SNAPSHOT.jar")));
        // 带时间戳的 snapshot 制品文件，其版本目录仍以 -SNAPSHOT 结尾
        assert!(MavenFormat.can_overwrite(&制品(
            "com/foo/lib/1.0-SNAPSHOT/lib-1.0-20240101.120000-1.jar"
        )));
    }

    #[test]
    fn maven_metadata_可覆盖() {
        assert!(MavenFormat.can_overwrite(&制品("com/foo/lib/maven-metadata.xml")));
        assert!(MavenFormat.can_overwrite(&制品("com/foo/lib/1.0-SNAPSHOT/maven-metadata.xml")));
    }

    #[test]
    fn sidecar_校验和可覆盖() {
        // release 主构件不可覆盖，但其 sidecar 允许覆盖（随主文件摘要镜像）
        for ext in ["sha1", "md5", "sha256", "sha512", "asc"] {
            let p = format!("com/foo/lib/1.0/lib-1.0.jar.{ext}");
            assert!(
                MavenFormat.can_overwrite(&制品(&p)),
                "sidecar .{ext} 应可覆盖"
            );
        }
        // metadata 的 sidecar 亦可覆盖
        assert!(MavenFormat.can_overwrite(&制品("com/foo/lib/maven-metadata.xml.sha1")));
    }

    #[test]
    fn 内容类型按扩展名推断() {
        let c = |p: &str| {
            MavenFormat.content_type(&ArtifactCoordinates {
                path: p.to_string(),
            })
        };
        assert_eq!(
            c("a/b/lib-1.0.jar").as_deref(),
            Some("application/java-archive")
        );
        assert_eq!(c("a/b/lib-1.0.pom").as_deref(), Some("application/xml"));
        assert_eq!(
            c("a/b/maven-metadata.xml").as_deref(),
            Some("application/xml")
        );
        assert_eq!(
            c("a/b/lib-1.0.jar.sha1").as_deref(),
            Some("text/plain; charset=utf-8")
        );
        // 未知扩展名返回 None
        assert_eq!(c("a/b/file.unknownext"), None);
    }

    #[test]
    fn gav_反解四段以上路径() {
        let gav = Gav::from_path("com/example/foo/lib/1.2.3/lib-1.2.3.jar").unwrap();
        assert_eq!(gav.group_id, "com.example.foo");
        assert_eq!(gav.artifact_id, "lib");
        assert_eq!(gav.version, "1.2.3");
    }

    #[test]
    fn gav_段数不足返回_none() {
        // 目录级 metadata 路径无法构成 GAV
        assert!(Gav::from_path("com/foo/maven-metadata.xml").is_none());
        assert!(Gav::from_path("lib-1.0.jar").is_none());
    }

    #[test]
    fn 使用片段含依赖坐标与仓库接入() {
        let coords = ArtifactCoordinates {
            path: "com/example/lib/1.0/lib-1.0.jar".to_string(),
        };
        let snippets =
            MavenFormat.usage_snippets("http://localhost:8080/", "maven-hosted", &coords);
        // 应含依赖坐标 + 仓库接入两段
        assert_eq!(snippets.len(), 2);
        assert!(snippets[0]
            .content
            .contains("<groupId>com.example</groupId>"));
        assert!(snippets[0].content.contains("<artifactId>lib</artifactId>"));
        assert!(snippets[0].content.contains("<version>1.0</version>"));
        // 仓库接入 URL 无双斜杠
        assert!(snippets[1]
            .content
            .contains("http://localhost:8080/maven-hosted"));
        assert!(!snippets[1].content.contains("8080//maven-hosted"));
    }

    #[test]
    fn vuln坐标_从主构件反解生态包版本() {
        let coord = MavenFormat
            .vuln_coordinate(&ArtifactCoordinates {
                path: "org/apache/logging/log4j/log4j-core/2.14.1/log4j-core-2.14.1.jar"
                    .to_string(),
            })
            .unwrap();
        assert_eq!(coord.ecosystem, "Maven");
        // OSV Maven 包名为 groupId:artifactId
        assert_eq!(coord.package, "org.apache.logging.log4j:log4j-core");
        assert_eq!(coord.version, "2.14.1");
    }

    #[test]
    fn vuln坐标_sidecar与metadata不产出() {
        // sidecar 不参与匹配
        assert!(MavenFormat
            .vuln_coordinate(&ArtifactCoordinates {
                path: "com/foo/lib/1.0/lib-1.0.jar.sha1".to_string(),
            })
            .is_none());
        // maven-metadata.xml 不参与匹配
        assert!(MavenFormat
            .vuln_coordinate(&ArtifactCoordinates {
                path: "com/foo/lib/maven-metadata.xml".to_string(),
            })
            .is_none());
        // 段数不足无法解 GAV
        assert!(MavenFormat
            .vuln_coordinate(&ArtifactCoordinates {
                path: "lib-1.0.jar".to_string(),
            })
            .is_none());
    }

    #[test]
    fn 使用片段对无法解_gav_的路径仅给仓库接入() {
        let coords = ArtifactCoordinates {
            path: "com/foo/maven-metadata.xml".to_string(),
        };
        let snippets = MavenFormat.usage_snippets("http://localhost:8080", "m", &coords);
        // 无法解出 GAV 时只给仓库接入片段
        assert_eq!(snippets.len(), 1);
        assert_eq!(snippets[0].title, "仓库接入 (settings.xml)");
    }

    /// 构造一份最小 jar（zip）：可选放入内嵌 pom.xml / pom.properties 于标准 META-INF/maven 路径。
    fn 构造_jar(pom_xml: Option<&str>, pom_properties: Option<&str>) -> Vec<u8> {
        use std::io::Write;
        use zip::write::SimpleFileOptions;
        use zip::ZipWriter;
        let mut buf = Vec::new();
        {
            let cursor = std::io::Cursor::new(&mut buf);
            let mut zip = ZipWriter::new(cursor);
            let opts =
                SimpleFileOptions::default().compression_method(zip::CompressionMethod::Deflated);
            // 放一个普通 class 条目，验证不被误读为坐标来源
            zip.start_file("com/example/Demo.class", opts).unwrap();
            zip.write_all(b"CAFEBABE").unwrap();
            if let Some(xml) = pom_xml {
                zip.start_file("META-INF/maven/com.example/demo/pom.xml", opts)
                    .unwrap();
                zip.write_all(xml.as_bytes()).unwrap();
            }
            if let Some(props) = pom_properties {
                zip.start_file("META-INF/maven/com.example/demo/pom.properties", opts)
                    .unwrap();
                zip.write_all(props.as_bytes()).unwrap();
            }
            zip.finish().unwrap();
        }
        buf
    }

    #[test]
    fn jar内嵌pom_xml提取gav() {
        let xml = r#"<project>
            <groupId>com.example</groupId>
            <artifactId>demo</artifactId>
            <version>1.2.3</version>
        </project>"#;
        let jar = 构造_jar(Some(xml), None);
        let gav = MavenFormat::parse_gav_from_jar(&jar).unwrap();
        assert_eq!(gav.group_id, "com.example");
        assert_eq!(gav.artifact_id, "demo");
        assert_eq!(gav.version, "1.2.3");
    }

    #[test]
    fn jar内嵌pom_xml的groupid与version继承parent() {
        // 子项目仅声明 artifactId，groupId / version 继承 parent
        let xml = r#"<project>
            <parent>
                <groupId>com.parent</groupId>
                <artifactId>parent</artifactId>
                <version>9.9.9</version>
            </parent>
            <artifactId>child</artifactId>
        </project>"#;
        let jar = 构造_jar(Some(xml), None);
        let gav = MavenFormat::parse_gav_from_jar(&jar).unwrap();
        assert_eq!(gav.group_id, "com.parent");
        assert_eq!(gav.artifact_id, "child");
        assert_eq!(gav.version, "9.9.9");
    }

    #[test]
    fn jar内嵌pom_xml项目级优先于parent() {
        // 同时声明 parent 与项目级 groupId / version 时，项目级优先
        let xml = r#"<project>
            <parent>
                <groupId>com.parent</groupId>
                <artifactId>parent</artifactId>
                <version>9.9.9</version>
            </parent>
            <groupId>com.child</groupId>
            <artifactId>child</artifactId>
            <version>1.0.0</version>
        </project>"#;
        let jar = 构造_jar(Some(xml), None);
        let gav = MavenFormat::parse_gav_from_jar(&jar).unwrap();
        assert_eq!(gav.group_id, "com.child");
        assert_eq!(gav.version, "1.0.0");
    }

    #[test]
    fn jar内嵌pom_xml不取dependencies内的groupid() {
        let xml = r#"<project>
            <groupId>real.group</groupId>
            <artifactId>real-artifact</artifactId>
            <version>2.0.0</version>
            <dependencies>
                <dependency>
                    <groupId>other.group</groupId>
                    <artifactId>other</artifactId>
                    <version>3.0.0</version>
                </dependency>
            </dependencies>
        </project>"#;
        let jar = 构造_jar(Some(xml), None);
        let gav = MavenFormat::parse_gav_from_jar(&jar).unwrap();
        assert_eq!(gav.group_id, "real.group");
        assert_eq!(gav.artifact_id, "real-artifact");
        assert_eq!(gav.version, "2.0.0");
    }

    #[test]
    fn jar内嵌pom_properties兜底() {
        let props = "#Generated by Maven\ngroupId=com.props\nartifactId=props-lib\nversion=4.5.6\n";
        let jar = 构造_jar(None, Some(props));
        let gav = MavenFormat::parse_gav_from_jar(&jar).unwrap();
        assert_eq!(gav.group_id, "com.props");
        assert_eq!(gav.artifact_id, "props-lib");
        assert_eq!(gav.version, "4.5.6");
    }

    #[test]
    fn jar_pom_xml优先于properties() {
        let xml = r#"<project><groupId>from.xml</groupId><artifactId>x</artifactId><version>1.0</version></project>"#;
        let props = "groupId=from.props\nartifactId=p\nversion=2.0\n";
        let jar = 构造_jar(Some(xml), Some(props));
        let gav = MavenFormat::parse_gav_from_jar(&jar).unwrap();
        assert_eq!(gav.group_id, "from.xml");
        assert_eq!(gav.artifact_id, "x");
        assert_eq!(gav.version, "1.0");
    }

    #[test]
    fn jar_pom_xml不全时回落properties() {
        // pom.xml 缺 version，无法解出完整 GAV → 回落 properties
        let xml = r#"<project><groupId>g</groupId><artifactId>a</artifactId></project>"#;
        let props = "groupId=com.props\nartifactId=props-lib\nversion=4.5.6\n";
        let jar = 构造_jar(Some(xml), Some(props));
        let gav = MavenFormat::parse_gav_from_jar(&jar).unwrap();
        assert_eq!(gav.version, "4.5.6");
        assert_eq!(gav.group_id, "com.props");
    }

    #[test]
    fn jar无内嵌pom返回none() {
        let jar = 构造_jar(None, None);
        assert!(MavenFormat::parse_gav_from_jar(&jar).is_none());
    }

    #[test]
    fn 非zip字节返回none() {
        assert!(MavenFormat::parse_gav_from_jar(b"not-a-zip-file").is_none());
    }

    /// 构造一条带指定路径与创建时间的制品记录，供版本聚合判定用。
    fn 制品带时间(path: &str, created_at: &str) -> ArtifactRecord {
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
            created_at: created_at.to_string(),
        }
    }

    #[test]
    fn 派生文件路径拼装() {
        assert_eq!(
            MavenFormat::pom_path("com.example.app", "demo", "1.0.0"),
            "com/example/app/demo/1.0.0/demo-1.0.0.pom"
        );
        assert_eq!(
            MavenFormat::artifact_metadata_path("com.example.app", "demo"),
            "com/example/app/demo/maven-metadata.xml"
        );
    }

    #[test]
    fn packaging据扩展名推断() {
        assert_eq!(MavenFormat::derive_packaging("demo-1.0.jar"), "jar");
        assert_eq!(MavenFormat::derive_packaging("demo-1.0.war"), "war");
        assert_eq!(MavenFormat::derive_packaging("demo-1.0.EAR"), "ear");
        // 未知扩展名默认 jar
        assert_eq!(MavenFormat::derive_packaging("demo-1.0.bin"), "jar");
    }

    #[test]
    fn 从jar提取内嵌pom原样字节() {
        let xml = r#"<project><groupId>com.example</groupId><artifactId>demo</artifactId><version>1.2.3</version></project>"#;
        let jar = 构造_jar(Some(xml), None);
        let pom = MavenFormat::extract_embedded_pom(&jar).unwrap();
        assert_eq!(pom, xml.as_bytes());
        // 无内嵌 pom / 非 zip → None
        assert!(MavenFormat::extract_embedded_pom(&构造_jar(None, None)).is_none());
        assert!(MavenFormat::extract_embedded_pom(b"not-a-zip").is_none());
    }

    #[test]
    fn 生成最小pom含坐标与packaging() {
        let pom = MavenFormat::build_minimal_pom("com.example", "demo", "1.0.0", "jar");
        let s = String::from_utf8(pom).unwrap();
        assert!(s.contains("<modelVersion>4.0.0</modelVersion>"));
        assert!(s.contains("<groupId>com.example</groupId>"));
        assert!(s.contains("<artifactId>demo</artifactId>"));
        assert!(s.contains("<version>1.0.0</version>"));
        assert!(s.contains("<packaging>jar</packaging>"));
    }

    #[test]
    fn 版本聚合按部署序去重并算lastupdated() {
        let records = vec![
            // demo 的两个版本，注意 created_at 顺序（1.0 早于 2.0）
            制品带时间("com/example/demo/1.0/demo-1.0.jar", "2026-01-01 10:00:00"),
            制品带时间("com/example/demo/1.0/demo-1.0.pom", "2026-01-01 10:00:01"),
            制品带时间("com/example/demo/2.0/demo-2.0.jar", "2026-02-02 12:30:45"),
            // 直接位于 artifact 下的 maven-metadata.xml 不计入版本
            制品带时间("com/example/demo/maven-metadata.xml", "2026-02-02 12:30:46"),
            // 其他 artifact 不计入
            制品带时间("com/example/other/9.9/other-9.9.jar", "2026-03-03 09:00:00"),
        ];
        let v = MavenFormat::collect_versions(&records, "com.example", "demo");
        assert_eq!(v.versions, vec!["1.0".to_string(), "2.0".to_string()]);
        // lastUpdated 取命中记录最大 created_at（2.0 的 jar，12:30:45）去非数字截 14 位
        assert_eq!(v.last_updated, "20260202123045");
    }

    #[test]
    fn 生成metadata含latest_release与版本列表() {
        let versions = MavenVersions {
            versions: vec![
                "1.0".to_string(),
                "2.0-SNAPSHOT".to_string(),
                "2.0".to_string(),
            ],
            last_updated: "20260202123045".to_string(),
        };
        let xml = String::from_utf8(MavenFormat::build_artifact_metadata(
            "com.example",
            "demo",
            &versions,
        ))
        .unwrap();
        assert!(xml.contains("<groupId>com.example</groupId>"));
        assert!(xml.contains("<artifactId>demo</artifactId>"));
        // latest = 末位（2.0）；release = 末位非 SNAPSHOT（2.0）
        assert!(xml.contains("<latest>2.0</latest>"));
        assert!(xml.contains("<release>2.0</release>"));
        assert!(xml.contains("<version>1.0</version>"));
        assert!(xml.contains("<version>2.0-SNAPSHOT</version>"));
        assert!(xml.contains("<version>2.0</version>"));
        assert!(xml.contains("<lastUpdated>20260202123045</lastUpdated>"));
    }

    #[test]
    fn 仅snapshot版本时metadata省略release() {
        let versions = MavenVersions {
            versions: vec!["1.0-SNAPSHOT".to_string()],
            last_updated: "20260101100000".to_string(),
        };
        let xml =
            String::from_utf8(MavenFormat::build_artifact_metadata("g", "a", &versions)).unwrap();
        assert!(xml.contains("<latest>1.0-SNAPSHOT</latest>"));
        // 无 release 元素
        assert!(!xml.contains("<release>"));
    }

    #[test]
    fn 空版本集metadata不崩且无版本项() {
        let versions = MavenVersions {
            versions: vec![],
            last_updated: String::new(),
        };
        let xml =
            String::from_utf8(MavenFormat::build_artifact_metadata("g", "a", &versions)).unwrap();
        assert!(xml.contains("<latest></latest>"));
        assert!(!xml.contains("<version>"));
    }
}
