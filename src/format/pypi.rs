//! PyPI 格式（FR-27，hosted + proxy）：以 PyPI Simple Repository API（PEP 503）暴露项目 / 文件索引，
//! 以 legacy multipart 协议接收 `twine upload`，使官方 `pip` / `twine` 可直接互通。
//!
//! 作为统一 [`Format`] trait 的实现接入通用制品机理（存储 / 校验和 / 事务 / 单飞缓存），
//! 只负责 PyPI 自身协议：项目名 PEP503 规范化、Simple 页面（HTML / JSON）生成、
//! multipart 上传体解析、代理 Simple 页面链接重写（均为纯函数，便于穷举测试）。
//!
//! 制品在仓库内的存储约定：
//! - 发行文件（wheel / sdist）存于路径 `packages/{规范名}/{文件名}`
//!   （如 `packages/flask/Flask-3.0.0-py3-none-any.whl`）。
//! - Simple 页面不另存索引文档：hosted 由存储文件实时枚举生成，proxy 每次回源上游，
//!   避免与制品索引形成双真源。

use serde_json::{json, Value};

use crate::meta::ArtifactRecord;

use super::{normalize_repo_path, ArtifactCoordinates, Format, PathError, UsageSnippet};

/// 发行文件在仓库内的存储前缀段（PyPI 包文件统一落于此命名空间下）。
pub const PACKAGES_PREFIX: &str = "packages";

/// Simple 索引路径段（pip 以 `--index-url .../{repo}/simple/` 访问）。
pub const SIMPLE_SEGMENT: &str = "simple";

/// PEP691 JSON 内容协商的内容类型（Simple 页面 JSON 形态）。
pub const PEP691_CONTENT_TYPE: &str = "application/vnd.pypi.simple.v1+json";

/// PEP691 API 版本号（meta.api-version）。
const PEP691_API_VERSION: &str = "1.0";

/// PyPI 格式处理器：仓库内以 `packages/{规范名}/{文件}` 定位发行文件。
pub struct PypiFormat;

/// PyPI 协议错误：上传体不合法 / 摘要不符 / 文件已存在等。
#[derive(Debug, thiserror::Error, PartialEq, Eq)]
pub enum PypiError {
    /// 上传请求体结构不合法（缺 `content` 文件、缺文件名等）。
    #[error("PyPI 上传请求体不合法: {0}")]
    InvalidBody(String),
    /// 客户端声明的 sha256_digest 与服务端算得的不符。
    #[error("sha256 摘要不匹配: 客户端声明 {client}，服务端算得 {server}")]
    DigestMismatch {
        /// 客户端声明的摘要（hex）。
        client: String,
        /// 服务端算得的摘要（hex）。
        server: String,
    },
}

/// 从 twine multipart 上传体解析出的单次上传内容。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct UploadRequest {
    /// 项目名（未规范化的原始 `name` 字段）。
    pub name: String,
    /// 版本号（`version` 字段，可缺省为空串）。
    pub version: String,
    /// 客户端声明的 sha256 摘要（hex；缺省为 None，则不强制对账）。
    pub sha256_digest: Option<String>,
    /// 发行文件名（`content` 字段的 filename）。
    pub filename: String,
    /// 发行文件原始字节。
    pub content: Vec<u8>,
}

impl PypiFormat {
    /// PEP503 项目名规范化：小写、连续的 `.` / `-` / `_` 折叠为单个 `-`。
    ///
    /// 例：`Holy_Grail` → `holy-grail`；`A.B--C` → `a-b-c`。
    pub fn normalize_project(name: &str) -> String {
        let mut out = String::with_capacity(name.len());
        let mut prev_sep = false;
        for ch in name.chars() {
            if matches!(ch, '.' | '-' | '_') {
                // 连续分隔符只输出一个 `-`
                if !prev_sep {
                    out.push('-');
                    prev_sep = true;
                }
            } else {
                out.extend(ch.to_lowercase());
                prev_sep = false;
            }
        }
        out
    }

    /// 据项目名与文件名拼出发行文件在仓库内的存储路径（`packages/{规范名}/{文件}`）。
    pub fn package_path(project: &str, filename: &str) -> String {
        format!(
            "{PACKAGES_PREFIX}/{}/{filename}",
            Self::normalize_project(project)
        )
    }

    /// 从发行文件存储路径反解规范化项目名；非 `packages/{名}/{文件}` 形态返回 None。
    pub fn project_of_package_path(path: &str) -> Option<&str> {
        let rest = path.strip_prefix(&format!("{PACKAGES_PREFIX}/"))?;
        // rest 形如 `{规范名}/{文件}`，取首段为项目名（须存在文件段，避免误把目录当文件）
        let (project, file) = rest.split_once('/')?;
        if project.is_empty() || file.is_empty() {
            return None;
        }
        Some(project)
    }

    /// 从 Simple 项目页 URL 路径解析出请求的项目名（`simple/{project}/` 或 `simple/{project}`）。
    ///
    /// 返回 None 表示这是 Simple 根索引（`simple` / `simple/`）而非具体项目页。
    pub fn project_of_simple_path(path: &str) -> Option<String> {
        let rest = path.strip_prefix(SIMPLE_SEGMENT)?;
        // 去掉 simple 之后的前后斜杠，剩余非空即为项目名段
        let trimmed = rest.trim_matches('/');
        if trimmed.is_empty() {
            return None;
        }
        // 项目页只允许单段项目名（不含更深路径）
        if trimmed.contains('/') {
            return None;
        }
        Some(trimmed.to_string())
    }

    /// 解析 twine 的 multipart/form-data 上传体：逐字段提取 name / version / sha256_digest /
    /// content 文件（含 filename）。`:action` 须为 `file_upload`，否则视为不支持的动作。
    ///
    /// 经 axum `Multipart` 预解析为 (字段名, 文件名, 字节) 三元组列表后调用本纯函数，便于穷举测试。
    pub fn parse_upload(fields: &[MultipartField]) -> Result<UploadRequest, PypiError> {
        let mut name = None;
        let mut version = String::new();
        let mut sha256_digest = None;
        let mut filename = None;
        let mut content = None;
        let mut action = None;

        for f in fields {
            match f.name.as_str() {
                ":action" => action = Some(text(f)),
                "name" => name = Some(text(f)),
                "version" => version = text(f),
                "sha256_digest" => {
                    let v = text(f);
                    if !v.is_empty() {
                        sha256_digest = Some(v.to_ascii_lowercase());
                    }
                }
                "content" => {
                    filename = f.filename.clone();
                    content = Some(f.bytes.clone());
                }
                // 其余 metadata 字段对存储无影响，忽略（不臆造未来用途的解析）
                _ => {}
            }
        }

        // :action 须为 file_upload（twine 上传动作）
        match action.as_deref() {
            Some("file_upload") => {}
            Some(other) => {
                return Err(PypiError::InvalidBody(format!(
                    "不支持的动作 :action={other}"
                )))
            }
            None => return Err(PypiError::InvalidBody("缺少 :action 字段".to_string())),
        }

        let content =
            content.ok_or_else(|| PypiError::InvalidBody("缺少 content 文件".to_string()))?;
        let filename =
            filename.ok_or_else(|| PypiError::InvalidBody("content 缺少文件名".to_string()))?;
        if filename.is_empty() || filename.contains('/') || filename.contains('\\') {
            return Err(PypiError::InvalidBody("content 文件名非法".to_string()));
        }
        let name = name.ok_or_else(|| PypiError::InvalidBody("缺少 name 字段".to_string()))?;
        if name.is_empty() {
            return Err(PypiError::InvalidBody("name 不能为空".to_string()));
        }

        Ok(UploadRequest {
            name,
            version,
            sha256_digest,
            filename,
            content,
        })
    }

    /// 校验客户端声明的 sha256_digest 与服务端算得的摘要一致；客户端未声明时跳过。
    pub fn verify_digest(declared: Option<&str>, computed: &str) -> Result<(), PypiError> {
        if let Some(d) = declared {
            if !d.eq_ignore_ascii_case(computed) {
                return Err(PypiError::DigestMismatch {
                    client: d.to_string(),
                    server: computed.to_string(),
                });
            }
        }
        Ok(())
    }

    /// 生成 Simple 根索引 HTML（PEP503）：每个规范化项目名一个 `<a href="{name}/">{name}</a>`。
    pub fn simple_index_html(projects: &[String]) -> String {
        let mut body = String::new();
        for p in projects {
            let safe = html_escape(p);
            body.push_str(&format!("    <a href=\"{safe}/\">{safe}</a>\n"));
        }
        format!(
            "<!DOCTYPE html>\n<html>\n  <head>\n    <meta name=\"pypi:repository-version\" content=\"1.0\">\n    <title>Simple index</title>\n  </head>\n  <body>\n{body}  </body>\n</html>\n"
        )
    }

    /// 生成 Simple 项目页 HTML（PEP503）：每个文件一个 `<a>`，href 指向本仓库 packages 路径 + `#sha256=`。
    ///
    /// `files` 为 `(文件名, sha256-hex)` 列表；href 用相对路径 `../../packages/{规范名}/{文件}`，
    /// 使 pip 据当前项目页 URL（`.../{repo}/simple/{规范名}/`）正确解析到包文件。
    pub fn simple_project_html(project: &str, files: &[(String, String)]) -> String {
        let norm = Self::normalize_project(project);
        let mut body = String::new();
        for (filename, sha256) in files {
            let href = format!(
                "../../{PACKAGES_PREFIX}/{}/{}#sha256={}",
                norm, filename, sha256
            );
            body.push_str(&format!(
                "    <a href=\"{}\">{}</a>\n",
                html_escape(&href),
                html_escape(filename)
            ));
        }
        format!(
            "<!DOCTYPE html>\n<html>\n  <head>\n    <meta name=\"pypi:repository-version\" content=\"1.0\">\n    <title>Links for {0}</title>\n  </head>\n  <body>\n    <h1>Links for {0}</h1>\n{1}  </body>\n</html>\n",
            html_escape(&norm),
            body
        )
    }

    /// 生成 Simple 根索引 JSON（PEP691）。
    pub fn simple_index_json(projects: &[String]) -> Value {
        json!({
            "meta": { "api-version": PEP691_API_VERSION },
            "projects": projects.iter().map(|p| json!({ "name": p })).collect::<Vec<_>>(),
        })
    }

    /// 生成 Simple 项目页 JSON（PEP691）：files[].url 指向本仓库 packages 路径，hashes.sha256 填实。
    pub fn simple_project_json(project: &str, files: &[(String, String)]) -> Value {
        let norm = Self::normalize_project(project);
        let files_json: Vec<Value> = files
            .iter()
            .map(|(filename, sha256)| {
                json!({
                    "filename": filename,
                    "url": format!("../../{PACKAGES_PREFIX}/{norm}/{filename}#sha256={sha256}"),
                    "hashes": { "sha256": sha256 },
                })
            })
            .collect();
        json!({
            "meta": { "api-version": PEP691_API_VERSION },
            "name": norm,
            "files": files_json,
        })
    }

    /// 把上游 Simple 项目页 HTML 中各文件链接重写为指向本代理仓库的 `packages/{规范名}/{文件}`，
    /// 并保留 `#sha256=` 片段（校验照常）。
    ///
    /// 返回 (重写后的 HTML, 文件名→上游绝对/相对 URL 映射)；映射供包文件 cache-miss 时回源用。
    /// 仅做最小 HTML 链接改写（按 `<a href="...">文件名</a>` 提取），不引入完整 HTML 解析器。
    ///
    /// 同时剥除上游的 PEP658/714 `data-core-metadata` / `data-dist-info-metadata` 属性：
    /// 本代理不提供 `.metadata` sidecar（属范围外），保留该属性会诱导 pip 去拉取不存在的
    /// sidecar 而 404 致安装失败；剥除后 pip 回退为下载完整 wheel（经本代理缓存照常）。
    pub fn rewrite_proxy_project_html(
        upstream_html: &str,
        project: &str,
    ) -> (String, Vec<(String, String)>) {
        let stripped = strip_metadata_attrs(upstream_html);
        let norm = Self::normalize_project(project);
        let mut out = String::with_capacity(stripped.len());
        let mut mapping = Vec::new();
        let mut rest = stripped.as_str();

        // 逐个查找 href="..."，把指向包文件的链接重写为本仓库路径
        while let Some(pos) = rest.find("href=\"") {
            let (before, after) = rest.split_at(pos + "href=\"".len());
            out.push_str(before);
            // 取到下一个引号为止的原始 href
            let Some(end) = after.find('"') else {
                out.push_str(after);
                rest = "";
                break;
            };
            let raw_href = &after[..end];
            // 拆出 URL 主体与 #fragment（含 sha256）
            let (url_part, fragment) = match raw_href.split_once('#') {
                Some((u, f)) => (u, Some(f)),
                None => (raw_href, None),
            };
            // 文件名取 URL 路径最后一段（去掉查询串）
            let filename = url_part
                .split(['?', '#'])
                .next()
                .unwrap_or(url_part)
                .rsplit('/')
                .next()
                .unwrap_or("")
                .to_string();
            if filename.is_empty() {
                // 非文件链接，原样输出
                out.push_str(raw_href);
            } else {
                let mut local = format!("../../{PACKAGES_PREFIX}/{norm}/{filename}");
                if let Some(f) = fragment {
                    local.push('#');
                    local.push_str(f);
                }
                out.push_str(&local);
                mapping.push((filename, url_part.to_string()));
            }
            out.push('"');
            rest = &after[end + 1..];
        }
        out.push_str(rest);
        (out, mapping)
    }
}

/// 经 axum `Multipart` 预解析的单个 form 字段（纯数据，供 [`PypiFormat::parse_upload`] 消费）。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct MultipartField {
    /// 字段名（form name）。
    pub name: String,
    /// 文件名（仅文件型字段有；文本字段为 None）。
    pub filename: Option<String>,
    /// 字段字节内容。
    pub bytes: Vec<u8>,
}

/// 把字段字节按 UTF-8 解读为文本（非法 UTF-8 丢弃，文本字段不应含非法字节）。
fn text(f: &MultipartField) -> String {
    String::from_utf8_lossy(&f.bytes).to_string()
}

/// 最小 HTML 文本转义：仅转义会破坏属性 / 标签的字符（文件名 / 项目名出现 `&`、`<` 等时）。
fn html_escape(s: &str) -> String {
    s.replace('&', "&amp;")
        .replace('<', "&lt;")
        .replace('>', "&gt;")
        .replace('"', "&quot;")
}

/// 上游 Simple 页面会带 PEP658/714 的核心元数据属性名（不同版本规范用过两种命名）。
const METADATA_ATTR_NAMES: [&str; 2] = ["data-core-metadata", "data-dist-info-metadata"];

/// 从锚点 HTML 中剥除 `data-core-metadata` / `data-dist-info-metadata` 属性及其值。
///
/// 形如 ` data-core-metadata="sha256=..."`：从属性名前的空白起、到其值的闭合引号止整段删除。
/// 本代理不提供 `.metadata` sidecar，保留这些属性会让 pip 误以为可拉取 sidecar 而 404。
fn strip_metadata_attrs(html: &str) -> String {
    let mut out = html.to_string();
    for attr in METADATA_ATTR_NAMES {
        out = remove_attr(&out, attr);
    }
    out
}

/// 删除某个带引号值的属性的所有出现：` {attr}="..."`（连同属性名前的一个空白）。
///
/// 仅按 `{attr}="` 定位并删到下一个 `"`，不解析整棵 DOM；属性值不含裸引号（HTML 规范保证）。
fn remove_attr(html: &str, attr: &str) -> String {
    let needle = format!("{attr}=\"");
    let mut out = String::with_capacity(html.len());
    let mut rest = html;
    while let Some(pos) = rest.find(&needle) {
        // 连同属性名前的一个分隔空白一并删除，避免残留多余空格
        let mut keep_end = pos;
        if keep_end > 0 && rest.as_bytes()[keep_end - 1] == b' ' {
            keep_end -= 1;
        }
        out.push_str(&rest[..keep_end]);
        // 跳过属性值到其闭合引号之后
        let after_open = &rest[pos + needle.len()..];
        match after_open.find('"') {
            Some(close) => rest = &after_open[close + 1..],
            // 无闭合引号（异常 HTML）：丢弃其余部分，避免输出半截属性
            None => {
                rest = "";
                break;
            }
        }
    }
    out.push_str(rest);
    out
}

impl Format for PypiFormat {
    fn name(&self) -> &'static str {
        "pypi"
    }

    fn parse_path(&self, raw_path: &str) -> Result<ArtifactCoordinates, PathError> {
        // PyPI 以归一化后的仓库内路径作为制品键（发行文件用 `packages/{规范名}/{文件}`）
        let path = normalize_repo_path(raw_path)?;
        Ok(ArtifactCoordinates { path })
    }

    fn can_overwrite(&self, _existing: &ArtifactRecord) -> bool {
        // PyPI 已发布文件不可覆盖（FR-61）：同文件再次上传一律拒绝
        false
    }

    fn content_type(&self, coords: &ArtifactCoordinates) -> Option<String> {
        let file_name = coords.path.rsplit('/').next().unwrap_or(&coords.path);
        // wheel / egg 为 zip 家族；sdist 多为 tar.gz / zip
        if file_name.ends_with(".whl") || file_name.ends_with(".egg") {
            return Some("application/octet-stream".to_string());
        }
        if file_name.ends_with(".tar.gz") || file_name.ends_with(".tgz") {
            return Some("application/gzip".to_string());
        }
        if file_name.ends_with(".zip") {
            return Some("application/zip".to_string());
        }
        // 其余未知，返回 None 交默认层决定
        None
    }

    fn usage_snippets(
        &self,
        public_base_url: &str,
        repo_name: &str,
        coords: &ArtifactCoordinates,
    ) -> Vec<UsageSnippet> {
        let base = public_base_url.trim_end_matches('/');
        let index_url = format!("{base}/{repo_name}/{SIMPLE_SEGMENT}/");
        let upload_url = format!("{base}/{repo_name}/");
        // 从存储路径反推项目名（packages/{规范名}/{文件}），失败则用仓库名占位提示
        let project = PypiFormat::project_of_package_path(&coords.path).unwrap_or(repo_name);
        vec![
            UsageSnippet {
                title: "安装".to_string(),
                language: "bash".to_string(),
                content: format!("pip install --index-url {index_url} {project}"),
            },
            UsageSnippet {
                title: "上传 (twine)".to_string(),
                language: "bash".to_string(),
                // 凭据不入示例：仅给占位用户名 / Token 环境变量
                content: format!(
                    "twine upload --repository-url {upload_url} \\\n  -u __token__ -p ${{PYPI_TOKEN}} dist/*"
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

    fn 字段(name: &str, bytes: &[u8]) -> MultipartField {
        MultipartField {
            name: name.to_string(),
            filename: None,
            bytes: bytes.to_vec(),
        }
    }

    fn 文件字段(name: &str, filename: &str, bytes: &[u8]) -> MultipartField {
        MultipartField {
            name: name.to_string(),
            filename: Some(filename.to_string()),
            bytes: bytes.to_vec(),
        }
    }

    #[test]
    fn 名称为_pypi() {
        assert_eq!(PypiFormat.name(), "pypi");
    }

    #[test]
    fn pep503_项目名规范化() {
        assert_eq!(PypiFormat::normalize_project("Holy_Grail"), "holy-grail");
        assert_eq!(PypiFormat::normalize_project("A.B--C"), "a-b-c");
        assert_eq!(PypiFormat::normalize_project("flask"), "flask");
        assert_eq!(PypiFormat::normalize_project("Django"), "django");
        // 连续混合分隔符折叠为单个 -
        assert_eq!(PypiFormat::normalize_project("a_-._b"), "a-b");
    }

    #[test]
    fn 包路径用规范名() {
        assert_eq!(
            PypiFormat::package_path("Flask", "Flask-3.0.0-py3-none-any.whl"),
            "packages/flask/Flask-3.0.0-py3-none-any.whl"
        );
    }

    #[test]
    fn 包路径反解项目名() {
        assert_eq!(
            PypiFormat::project_of_package_path("packages/flask/Flask-3.0.0.tar.gz"),
            Some("flask")
        );
        // 非包路径返回 None
        assert_eq!(PypiFormat::project_of_package_path("simple/flask"), None);
        assert_eq!(PypiFormat::project_of_package_path("packages/flask"), None);
    }

    #[test]
    fn simple_路径解析项目名() {
        assert_eq!(PypiFormat::project_of_simple_path("simple"), None);
        assert_eq!(PypiFormat::project_of_simple_path("simple/"), None);
        assert_eq!(
            PypiFormat::project_of_simple_path("simple/flask/").as_deref(),
            Some("flask")
        );
        assert_eq!(
            PypiFormat::project_of_simple_path("simple/flask").as_deref(),
            Some("flask")
        );
        // 更深路径不视为项目页
        assert_eq!(PypiFormat::project_of_simple_path("simple/a/b"), None);
        // 非 simple 前缀返回 None
        assert_eq!(PypiFormat::project_of_simple_path("packages/x"), None);
    }

    #[test]
    fn 解析上传体提取文件与字段() {
        let fields = vec![
            字段(":action", b"file_upload"),
            字段("name", b"Flask"),
            字段("version", b"3.0.0"),
            字段("sha256_digest", b"ABCDEF"),
            文件字段("content", "Flask-3.0.0-py3-none-any.whl", b"WHEEL-BYTES"),
        ];
        let req = PypiFormat::parse_upload(&fields).unwrap();
        assert_eq!(req.name, "Flask");
        assert_eq!(req.version, "3.0.0");
        // 摘要被规范化为小写
        assert_eq!(req.sha256_digest.as_deref(), Some("abcdef"));
        assert_eq!(req.filename, "Flask-3.0.0-py3-none-any.whl");
        assert_eq!(req.content, b"WHEEL-BYTES");
    }

    #[test]
    fn 解析上传体缺_content_报错() {
        let fields = vec![字段(":action", b"file_upload"), 字段("name", b"x")];
        assert!(matches!(
            PypiFormat::parse_upload(&fields),
            Err(PypiError::InvalidBody(_))
        ));
    }

    #[test]
    fn 解析上传体缺_action_报错() {
        let fields = vec![
            字段("name", b"x"),
            文件字段("content", "x-1.0.tar.gz", b"B"),
        ];
        assert!(matches!(
            PypiFormat::parse_upload(&fields),
            Err(PypiError::InvalidBody(_))
        ));
    }

    #[test]
    fn 解析上传体非法文件名报错() {
        let fields = vec![
            字段(":action", b"file_upload"),
            字段("name", b"x"),
            文件字段("content", "../evil.whl", b"B"),
        ];
        assert!(matches!(
            PypiFormat::parse_upload(&fields),
            Err(PypiError::InvalidBody(_))
        ));
    }

    #[test]
    fn 摘要校验匹配与不匹配() {
        // 未声明：跳过
        assert!(PypiFormat::verify_digest(None, "deadbeef").is_ok());
        // 匹配（大小写不敏感）
        assert!(PypiFormat::verify_digest(Some("DEADBEEF"), "deadbeef").is_ok());
        // 不匹配
        assert!(matches!(
            PypiFormat::verify_digest(Some("aaaa"), "bbbb"),
            Err(PypiError::DigestMismatch { .. })
        ));
    }

    #[test]
    fn 不可覆盖() {
        assert!(!PypiFormat.can_overwrite(&记录("packages/flask/Flask-3.0.0.whl")));
    }

    #[test]
    fn 内容类型按扩展名推断() {
        let ct = |p: &str| {
            PypiFormat.content_type(&ArtifactCoordinates {
                path: p.to_string(),
            })
        };
        assert_eq!(
            ct("packages/flask/Flask-3.0.0-py3-none-any.whl").as_deref(),
            Some("application/octet-stream")
        );
        assert_eq!(
            ct("packages/flask/Flask-3.0.0.tar.gz").as_deref(),
            Some("application/gzip")
        );
        assert_eq!(
            ct("packages/x/x-1.0.zip").as_deref(),
            Some("application/zip")
        );
        assert_eq!(ct("packages/x/x.unknown"), None);
    }

    #[test]
    fn simple_根索引_html_含项目锚点() {
        let html = PypiFormat::simple_index_html(&["flask".to_string(), "django".to_string()]);
        assert!(html.contains("<!DOCTYPE html>"));
        assert!(html.contains("<a href=\"flask/\">flask</a>"));
        assert!(html.contains("<a href=\"django/\">django</a>"));
    }

    #[test]
    fn simple_项目页_html_含_sha256_片段() {
        let files = vec![
            (
                "Flask-3.0.0-py3-none-any.whl".to_string(),
                "abc123".to_string(),
            ),
            ("Flask-3.0.0.tar.gz".to_string(), "def456".to_string()),
        ];
        let html = PypiFormat::simple_project_html("Flask", &files);
        // href 指向本仓库 packages 路径（规范名），且带 #sha256=
        assert!(html.contains("../../packages/flask/Flask-3.0.0-py3-none-any.whl#sha256=abc123"));
        assert!(html.contains("../../packages/flask/Flask-3.0.0.tar.gz#sha256=def456"));
        // 锚点文本为文件名
        assert!(html.contains(">Flask-3.0.0-py3-none-any.whl</a>"));
    }

    #[test]
    fn simple_json_形态_pep691() {
        let idx = PypiFormat::simple_index_json(&["flask".to_string()]);
        assert_eq!(idx["meta"]["api-version"], "1.0");
        assert_eq!(idx["projects"][0]["name"], "flask");

        let files = vec![("flask-1.0.tar.gz".to_string(), "deadbeef".to_string())];
        let proj = PypiFormat::simple_project_json("Flask", &files);
        assert_eq!(proj["name"], "flask");
        assert_eq!(proj["files"][0]["filename"], "flask-1.0.tar.gz");
        assert_eq!(proj["files"][0]["hashes"]["sha256"], "deadbeef");
        assert!(proj["files"][0]["url"]
            .as_str()
            .unwrap()
            .contains("packages/flask/flask-1.0.tar.gz#sha256=deadbeef"));
    }

    #[test]
    fn 代理项目页重写文件链接指向本仓库() {
        let upstream = "<!DOCTYPE html><html><body>\
            <a href=\"https://files.pythonhosted.org/packages/ab/cd/flask-3.0.0.tar.gz#sha256=upstreamhash\">flask-3.0.0.tar.gz</a>\
            </body></html>";
        let (rewritten, mapping) = PypiFormat::rewrite_proxy_project_html(upstream, "Flask");
        // 链接重写为本仓库相对路径，保留 sha256 片段
        assert!(rewritten.contains("../../packages/flask/flask-3.0.0.tar.gz#sha256=upstreamhash"));
        // 不再含上游主机
        assert!(!rewritten.contains("files.pythonhosted.org"));
        // 映射记录文件名 → 上游绝对 URL（供包文件回源用，不含 fragment）
        assert_eq!(mapping.len(), 1);
        assert_eq!(mapping[0].0, "flask-3.0.0.tar.gz");
        assert_eq!(
            mapping[0].1,
            "https://files.pythonhosted.org/packages/ab/cd/flask-3.0.0.tar.gz"
        );
    }

    #[test]
    fn 代理重写剥除_pep658_元数据属性() {
        // 上游锚点带 data-core-metadata / data-dist-info-metadata（PEP658/714）
        let upstream =
            "<a href=\"https://files.pythonhosted.org/packages/ab/six-1.0.tar.gz#sha256=h\" \
            data-requires-python=\"&gt;=2.7\" \
            data-dist-info-metadata=\"sha256=m1\" \
            data-core-metadata=\"sha256=m2\">six-1.0.tar.gz</a>";
        let (rewritten, mapping) = PypiFormat::rewrite_proxy_project_html(upstream, "six");
        // 链接重写到本仓库且保留 sha256
        assert!(rewritten.contains("../../packages/six/six-1.0.tar.gz#sha256=h"));
        // 两类核心元数据属性均被剥除（本代理不提供 sidecar，保留会致 pip 拉取 .metadata 而 404）
        assert!(!rewritten.contains("data-core-metadata"));
        assert!(!rewritten.contains("data-dist-info-metadata"));
        // 与 sidecar 无关的属性（requires-python）保留
        assert!(rewritten.contains("data-requires-python"));
        assert_eq!(mapping.len(), 1);
        assert_eq!(mapping[0].0, "six-1.0.tar.gz");
    }

    #[test]
    fn 使用片段含_pip_与_twine() {
        let coords = ArtifactCoordinates {
            path: "packages/flask/Flask-3.0.0.whl".to_string(),
        };
        let snippets = PypiFormat.usage_snippets("http://localhost:8080/", "pypi-hosted", &coords);
        assert_eq!(snippets.len(), 2);
        assert!(snippets[0]
            .content
            .contains("pip install --index-url http://localhost:8080/pypi-hosted/simple/ flask"));
        assert!(snippets[1]
            .content
            .contains("twine upload --repository-url http://localhost:8080/pypi-hosted/"));
        // 凭据为占位，不含真实 Token
        assert!(snippets[1].content.contains("${PYPI_TOKEN}"));
        assert!(snippets[1].content.contains("__token__"));
    }
}
