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

/// Maven 坐标（GAV）：由仓库内路径反解而来，用于生成依赖片段。
#[derive(Debug, Clone, PartialEq, Eq)]
struct Gav {
    /// groupId（以 `.` 分隔）。
    group_id: String,
    /// artifactId。
    artifact_id: String,
    /// version。
    version: String,
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
}
