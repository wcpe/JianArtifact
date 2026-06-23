//! Raw 通用文件格式（FR-17）：以路径直存直取，覆盖允许，无 sidecar。
//!
//! 作为统一 [`Format`] trait 的首个实现，端到端验证通用制品机理；其余格式按同一 trait 接入。

use crate::meta::ArtifactRecord;

use super::{normalize_repo_path, ArtifactCoordinates, Format, PathError, UsageSnippet};

/// Raw 格式处理器：仓库内路径即制品键，覆盖允许，内容类型按扩展名粗判。
pub struct RawFormat;

impl Format for RawFormat {
    fn name(&self) -> &'static str {
        "raw"
    }

    fn parse_path(&self, raw_path: &str) -> Result<ArtifactCoordinates, PathError> {
        // Raw 直接以归一化后的仓库内路径作为制品坐标
        let path = normalize_repo_path(raw_path)?;
        Ok(ArtifactCoordinates { path })
    }

    fn can_overwrite(&self, _existing: &ArtifactRecord) -> bool {
        // Raw 同路径文件允许覆盖（API.md 覆盖语义）
        true
    }

    fn content_type(&self, coords: &ArtifactCoordinates) -> Option<String> {
        // 按扩展名做最小粗判；无法判断时返回 None，交由通用默认值处理
        let ext = coords.path.rsplit('.').next().unwrap_or("");
        let ct = match ext.to_ascii_lowercase().as_str() {
            "txt" => "text/plain; charset=utf-8",
            "json" => "application/json",
            "xml" => "application/xml",
            "html" | "htm" => "text/html; charset=utf-8",
            "tar" => "application/x-tar",
            "gz" | "tgz" => "application/gzip",
            "zip" => "application/zip",
            "jar" => "application/java-archive",
            "pdf" => "application/pdf",
            "png" => "image/png",
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
        // 拼接对外可访问的完整 URL（去除 base_url 尾部多余斜杠，避免出现双斜杠）
        let base = public_base_url.trim_end_matches('/');
        let url = format!("{base}/{repo_name}/{}", coords.path);
        vec![
            UsageSnippet {
                title: "下载".to_string(),
                language: "bash".to_string(),
                content: format!("curl -fL -O {url}"),
            },
            UsageSnippet {
                title: "上传".to_string(),
                language: "bash".to_string(),
                content: format!("curl -f --upload-file <本地文件> {url}"),
            },
        ]
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// 构造一条最小制品记录，仅供覆盖策略判定用。
    fn 占位制品() -> ArtifactRecord {
        ArtifactRecord {
            id: "id".to_string(),
            repo_id: "r".to_string(),
            path: "p".to_string(),
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
    fn 名称为_raw() {
        assert_eq!(RawFormat.name(), "raw");
    }

    #[test]
    fn 解析路径归一化且拒穿越() {
        assert_eq!(RawFormat.parse_path("/a//b.txt").unwrap().path, "a/b.txt");
        assert_eq!(RawFormat.parse_path("a/../b"), Err(PathError::Traversal));
    }

    #[test]
    fn raw_允许覆盖() {
        assert!(RawFormat.can_overwrite(&占位制品()));
    }

    #[test]
    fn 内容类型按扩展名推断() {
        let c = |p: &str| {
            RawFormat.content_type(&ArtifactCoordinates {
                path: p.to_string(),
            })
        };
        assert_eq!(c("a.json").as_deref(), Some("application/json"));
        assert_eq!(c("a/b.txt").as_deref(), Some("text/plain; charset=utf-8"));
        // 未知扩展名返回 None
        assert_eq!(c("a.unknownext"), None);
        assert_eq!(c("no-ext"), None);
    }

    #[test]
    fn 使用片段含完整_url_且无双斜杠() {
        let coords = ArtifactCoordinates {
            path: "dir/file.bin".to_string(),
        };
        // base_url 尾部带斜杠也不应产生双斜杠
        let snippets = RawFormat.usage_snippets("http://localhost:8080/", "files", &coords);
        assert_eq!(snippets.len(), 2);
        assert!(snippets[0]
            .content
            .contains("http://localhost:8080/files/dir/file.bin"));
        assert!(!snippets[0].content.contains("8080//files"));
    }
}
