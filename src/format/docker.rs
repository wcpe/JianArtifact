//! Docker / OCI 格式（FR-16）：以 Docker Registry v2 / OCI Distribution 协议暴露。
//!
//! 作为统一 [`Format`] trait 的实现，本文件只承载 docker 格式的**纯逻辑**：
//! 制品坐标（内部存储键）映射、覆盖策略、内容类型与使用方式片段，以及 digest 校验工具。
//! Registry v2 的 HTTP 状态机（blob 上传 POST/PATCH/PUT、manifest 存取、tag 解析）在
//! `api/docker_routes.rs`，存储仍复用通用 `BlobStore` / `MetaStore`，不绕过 `meta`。
//!
//! 内部存储键约定（仓库内 `artifacts.path`，对客户端不可见，仅作元数据索引键）：
//! - blob：`{image}/blobs/sha256:{hex}`，内容寻址，sha256 即 digest。
//! - manifest（按 digest）：`{image}/manifests/sha256:{hex}`，内容即 manifest 字节。
//! - tag 指针：`{image}/tags/{tag}`，内容为该 tag 当前指向的 manifest 字节（按 digest 去重，
//!   同 sha256 仅一份物理 blob）；content_type 记录 manifest 媒体类型，供 GET 回放。

use crate::meta::ArtifactRecord;

use super::{ArtifactCoordinates, Format, PathError, UsageSnippet};

/// Docker digest 算法前缀（当前仅支持 sha256，与 blob 内容寻址一致）。
pub const DIGEST_PREFIX: &str = "sha256:";

/// Docker registry v2 镜像清单（schema2）媒体类型。
pub const MEDIA_TYPE_MANIFEST_V2: &str = "application/vnd.docker.distribution.manifest.v2+json";

/// Docker / OCI 格式处理器：承载 docker 格式的纯逻辑，无状态。
pub struct DockerFormat;

impl Format for DockerFormat {
    fn name(&self) -> &'static str {
        "docker"
    }

    fn parse_path(&self, raw_path: &str) -> Result<ArtifactCoordinates, PathError> {
        // docker 的存储键由 docker_routes 内部按协议拼好（已含 image/blobs|manifests|tags 段），
        // 此处沿用通用归一化做基础校验：拒空、拒 `.` / `..` 穿越。
        let path = super::normalize_repo_path(raw_path)?;
        Ok(ArtifactCoordinates { path })
    }

    fn can_overwrite(&self, _existing: &ArtifactRecord) -> bool {
        // Docker 习惯：同一 tag 允许覆盖（FR-61）；blob / manifest 按 digest 内容寻址天然去重。
        true
    }

    fn content_type(&self, _coords: &ArtifactCoordinates) -> Option<String> {
        // docker 制品的内容类型由协议层按 manifest 媒体类型 / blob 二进制显式写入，
        // 不据存储键扩展名猜测，避免误判。返回 None 交由协议层显式设置。
        None
    }

    fn usage_snippets(
        &self,
        public_base_url: &str,
        repo_name: &str,
        coords: &ArtifactCoordinates,
    ) -> Vec<UsageSnippet> {
        // 据存储键反解出 image:tag，生成 docker login / pull 片段。
        // 对外地址去掉 scheme 作为 registry 主机（docker 客户端不接受带 scheme 的镜像引用）。
        let host = registry_host(public_base_url);
        let reference = image_reference(repo_name, &coords.path);
        let image = format!("{host}/{reference}");
        vec![
            UsageSnippet {
                title: "登录".to_string(),
                language: "bash".to_string(),
                content: format!("docker login {host}"),
            },
            UsageSnippet {
                title: "拉取".to_string(),
                language: "bash".to_string(),
                content: format!("docker pull {image}"),
            },
        ]
    }
}

/// 从对外基础 URL 提取 registry 主机部分（去掉 scheme 与尾部斜杠）。
fn registry_host(public_base_url: &str) -> String {
    public_base_url
        .trim_end_matches('/')
        .trim_start_matches("https://")
        .trim_start_matches("http://")
        .to_string()
}

/// 据内部存储键反解出 `{仓库}/{image}:{tag}` 形式的镜像引用，供使用片段展示。
///
/// 仅 `tags` 键能还原出可读的 `image:tag`；其余键（blobs / manifests）退化为 `仓库/image`。
fn image_reference(repo_name: &str, storage_path: &str) -> String {
    if let Some((image, tag)) = parse_tag_key(storage_path) {
        return format!("{repo_name}/{image}:{tag}");
    }
    // 退化：取第一段作为 image 名
    let image = storage_path.split('/').next().unwrap_or(storage_path);
    format!("{repo_name}/{image}")
}

/// 校验并解析 docker digest（`sha256:{64 位十六进制}`），返回其 hex 部分（即 blob 的 sha256）。
///
/// 仅接受 sha256 算法与 64 位小写十六进制，杜绝非法 digest 越权寻址其他 blob。
pub fn parse_digest(digest: &str) -> Option<String> {
    let hex = digest.strip_prefix(DIGEST_PREFIX)?;
    if hex.len() == 64 && hex.bytes().all(|b| b.is_ascii_hexdigit()) {
        // 归一化为小写，避免大小写不一致导致索引 / 寻址错配
        Some(hex.to_ascii_lowercase())
    } else {
        None
    }
}

/// 由 blob 的 sha256（hex）拼出完整 docker digest（`sha256:{hex}`）。
pub fn make_digest(sha256_hex: &str) -> String {
    format!("{DIGEST_PREFIX}{sha256_hex}")
}

/// 判断一个 manifest reference 是否为 digest 形式（否则视作 tag）。
pub fn is_digest_reference(reference: &str) -> bool {
    reference.starts_with(DIGEST_PREFIX)
}

/// blob 的内部存储键：`{image}/blobs/sha256:{hex}`。
pub fn blob_key(image: &str, digest: &str) -> String {
    format!("{image}/blobs/{digest}")
}

/// manifest（按 digest）的内部存储键：`{image}/manifests/sha256:{hex}`。
pub fn manifest_digest_key(image: &str, digest: &str) -> String {
    format!("{image}/manifests/{digest}")
}

/// tag 指针的内部存储键：`{image}/tags/{tag}`。
pub fn tag_key(image: &str, tag: &str) -> String {
    format!("{image}/tags/{tag}")
}

/// 从 tag 存储键反解 `(image, tag)`；非 tag 键返回 None。
fn parse_tag_key(storage_path: &str) -> Option<(String, String)> {
    // 形如 `{image}/tags/{tag}`，其中 image 可含多段（如 library/alpine）
    let (image, tag) = storage_path.rsplit_once("/tags/")?;
    if image.is_empty() || tag.is_empty() {
        return None;
    }
    Some((image.to_string(), tag.to_string()))
}

#[cfg(test)]
mod tests {
    use super::*;

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
    fn 名称为_docker() {
        assert_eq!(DockerFormat.name(), "docker");
    }

    #[test]
    fn tag_允许覆盖() {
        assert!(DockerFormat.can_overwrite(&占位制品()));
    }

    #[test]
    fn 解析合法_digest_得到小写_hex() {
        let d = "sha256:ABCDEF0123456789abcdef0123456789abcdef0123456789abcdef0123456789";
        let hex = parse_digest(d).unwrap();
        assert_eq!(hex.len(), 64);
        assert_eq!(
            hex,
            "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
        );
    }

    #[test]
    fn 拒绝非法_digest() {
        // 缺前缀
        assert!(parse_digest("abc").is_none());
        // 长度不足
        assert!(parse_digest("sha256:abcd").is_none());
        // 非十六进制
        assert!(parse_digest(&format!("sha256:{}", "z".repeat(64))).is_none());
        // 其他算法（当前仅支持 sha256）
        assert!(parse_digest(&format!("sha512:{}", "a".repeat(64))).is_none());
    }

    #[test]
    fn digest_往返一致() {
        let hex = "a".repeat(64);
        let d = make_digest(&hex);
        assert_eq!(d, format!("sha256:{hex}"));
        assert_eq!(parse_digest(&d).unwrap(), hex);
    }

    #[test]
    fn 区分_digest_与_tag_引用() {
        assert!(is_digest_reference(&format!("sha256:{}", "a".repeat(64))));
        assert!(!is_digest_reference("latest"));
        assert!(!is_digest_reference("v1.0"));
    }

    #[test]
    fn 存储键拼接与_tag_反解() {
        let digest = format!("sha256:{}", "a".repeat(64));
        assert_eq!(blob_key("library/alpine", &digest), format!("library/alpine/blobs/{digest}"));
        assert_eq!(
            manifest_digest_key("app", &digest),
            format!("app/manifests/{digest}")
        );
        assert_eq!(tag_key("library/alpine", "3.20"), "library/alpine/tags/3.20");

        // 反解多段 image 的 tag 键
        let (image, tag) = parse_tag_key("library/alpine/tags/3.20").unwrap();
        assert_eq!(image, "library/alpine");
        assert_eq!(tag, "3.20");
        // 非 tag 键返回 None
        assert!(parse_tag_key("app/blobs/sha256:x").is_none());
    }

    #[test]
    fn 使用片段含_login_与_pull_无_scheme() {
        let coords = ArtifactCoordinates {
            path: "library/alpine/tags/3.20".to_string(),
        };
        let snippets = DockerFormat.usage_snippets("http://127.0.0.1:18161", "hub", &coords);
        assert_eq!(snippets.len(), 2);
        // login 指向去 scheme 的主机
        assert!(snippets[0].content.contains("docker login 127.0.0.1:18161"));
        // pull 引用形如 host/repo/image:tag，且不含 http://
        let pull = &snippets[1].content;
        assert!(pull.contains("docker pull 127.0.0.1:18161/hub/library/alpine:3.20"));
        assert!(!pull.contains("http://"));
    }

    #[test]
    fn 解析路径拒穿越() {
        assert_eq!(
            DockerFormat.parse_path("a/../b"),
            Err(PathError::Traversal)
        );
        assert_eq!(DockerFormat.parse_path("").unwrap_err(), PathError::Empty);
    }
}
