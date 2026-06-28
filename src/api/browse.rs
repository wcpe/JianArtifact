//! 目录浏览编排（FR-75）：Accept 驱动双形态的仓库目录索引。
//!
//! handler 保持薄：读授权复用 `repo_access`（private 对无权一律 404、不泄露存在性），
//! 前缀列举下沉到 `meta`，目录项折叠是 `format` 的纯函数，本模块只做编排与渲染。
//! 仅通用格式（raw / maven 等经统一 trait 落库者）参与；npm / docker 等原生协议各有
//! 自身的尾斜杠语义，由其分派分支先行处理，不进本目录分支。

use axum::{
    http::{header, HeaderMap, StatusCode},
    response::{IntoResponse, Response},
    Json,
};
use serde::Serialize;

use crate::format::{collapse_directory_entries, DirEntry, DirEntryKind};

use super::repo_access::load_readable_repo_by_name;
use super::{ApiError, AppState, Identity};

/// 走原生协议、不参与通用目录浏览的格式名集合。
///
/// 这些格式的请求路径含协议语义（含尾斜杠的索引端点等），由各自分派分支处理，
/// 不应被目录浏览分支吞掉；其余格式（raw / maven 等）按仓库内路径直存，可通用浏览。
const NATIVE_FORMATS: &[&str] = &["npm", "docker", "cargo", "pypi", "nuget", "go"];

/// 判断该格式是否参与通用目录浏览（非原生协议格式即参与）。
pub(crate) fn is_browsable_format(format: &str) -> bool {
    !NATIVE_FORMATS.contains(&format)
}

/// 单条目录项的 JSON 视图。
#[derive(Debug, Serialize)]
struct DirEntryDto {
    /// 条目名（本层内，不含前缀）。
    name: String,
    /// 类型：`folder` / `file`。
    r#type: &'static str,
    /// 文件字节大小（子目录为 null）。
    #[serde(skip_serializing_if = "Option::is_none")]
    size: Option<i64>,
    /// 文件 sha256（子目录为 null）。
    #[serde(skip_serializing_if = "Option::is_none")]
    sha256: Option<String>,
    /// 文件创建时间（子目录为 null）。
    #[serde(skip_serializing_if = "Option::is_none")]
    created_at: Option<String>,
}

impl From<DirEntry> for DirEntryDto {
    fn from(e: DirEntry) -> Self {
        Self {
            name: e.name,
            r#type: e.kind.as_str(),
            size: e.size,
            sha256: e.sha256,
            created_at: e.created_at,
        }
    }
}

/// 目录列举的 JSON 响应。
#[derive(Debug, Serialize)]
struct DirListingDto {
    /// 所属仓库名。
    repo: String,
    /// 当前目录前缀（仓库根为空串）。
    path: String,
    /// 本层条目（目录在前、文件在后，各自升序）。
    entries: Vec<DirEntryDto>,
}

/// 处理目录浏览请求：读授权 → 按前缀列举 → 折叠一层 → 按 Accept 渲染 JSON / HTML。
///
/// `dir_path` 为去掉尾斜杠后的目录路径（仓库根为空串）。读授权失败（含匿名访问 private）
/// 一律 404，且无论 JSON / HTML 形态都不泄露资源存在性（错误体不含任何目录内容）。
pub(crate) async fn browse_directory(
    state: &AppState,
    identity: &Identity,
    repo_name: &str,
    dir_path: &str,
    headers: &HeaderMap,
) -> Result<Response, ApiError> {
    // 读授权：仓库不存在 / 无读权限（含匿名 private）一律 404 隐藏存在性
    let repo = load_readable_repo_by_name(state, identity, repo_name).await?;

    // 列举前缀：目录前缀须以 `/` 结尾（根为空串），与制品全路径对齐
    let prefix = if dir_path.is_empty() {
        String::new()
    } else {
        format!("{dir_path}/")
    };
    let records = state
        .meta
        .list_artifacts_under_prefix(&repo.id, &prefix)
        .await?;
    let entries = collapse_directory_entries(&prefix, &records);

    // Accept 协商：含 text/html（浏览器）→ HTML 索引页；否则默认 JSON
    if wants_html(headers) {
        Ok(render_html(&repo.name, dir_path, &entries).into_response())
    } else {
        let dto = DirListingDto {
            repo: repo.name.clone(),
            path: dir_path.to_string(),
            entries: entries.into_iter().map(DirEntryDto::from).collect(),
        };
        Ok(Json(dto).into_response())
    }
}

/// 是否优先返回 HTML：Accept 头中 `text/html` 排在 `application/json` 之前（或仅含 html）。
///
/// 简化的内容协商：不做完整 q 值排序，只判断是否更偏好 HTML——浏览器默认 Accept 以
/// `text/html` 起头，包管理器 / 脚本一般要 `application/json` 或不带偏好，落 JSON 默认。
fn wants_html(headers: &HeaderMap) -> bool {
    let Some(accept) = headers.get(header::ACCEPT).and_then(|v| v.to_str().ok()) else {
        return false;
    };
    let html_pos = accept.find("text/html");
    let json_pos = accept.find("application/json");
    match (html_pos, json_pos) {
        // 两者都在：谁更靠前更偏好谁
        (Some(h), Some(j)) => h < j,
        // 只有 html
        (Some(_), None) => true,
        // 只有 json 或都没有：默认 JSON
        _ => false,
    }
}

/// 渲染类 Apache 目录索引的 HTML 页（转义用户可见文本，防存储型 XSS）。
fn render_html(repo: &str, dir_path: &str, entries: &[DirEntry]) -> Response {
    let body = build_html_body(repo, dir_path, entries);
    (
        StatusCode::OK,
        [(header::CONTENT_TYPE, "text/html; charset=utf-8")],
        body,
    )
        .into_response()
}

/// 构造目录索引页 HTML 正文（纯函数，便于穷举测试）。
///
/// 非根目录在条目表首行补一条「返回上级」链接（href `../`，相对当前带尾斜杠的目录 URL，
/// 上跳一层）；根目录（`dir_path` 为空）不补，避免越出仓库根。
fn build_html_body(repo: &str, dir_path: &str, entries: &[DirEntry]) -> String {
    let display = if dir_path.is_empty() {
        format!("/{repo}/")
    } else {
        format!("/{repo}/{dir_path}/")
    };
    let title = format!("索引 {}", html_escape(&display));

    let mut rows = String::new();
    // 非根目录补「返回上级」：href 用相对 `../`，依当前目录尾斜杠 URL 自然上跳一层；
    // 根目录不补，防止链接越出仓库根。
    if !dir_path.is_empty() {
        rows.push_str(
            "<tr><td><a href=\"../\">../</a></td><td>目录</td><td class=\"size\">-</td></tr>",
        );
    }
    for entry in entries {
        let is_dir = entry.kind == DirEntryKind::Folder;
        // 子目录链接补尾斜杠（再次进入目录浏览），文件链接指向其本体
        let href = if is_dir {
            format!("{}/", html_escape(&entry.name))
        } else {
            html_escape(&entry.name)
        };
        let label = if is_dir {
            format!("{}/", html_escape(&entry.name))
        } else {
            html_escape(&entry.name)
        };
        let size = match entry.size {
            Some(s) => s.to_string(),
            None => "-".to_string(),
        };
        rows.push_str(&format!(
            "<tr><td><a href=\"{href}\">{label}</a></td><td>{}</td><td class=\"size\">{size}</td></tr>",
            if is_dir { "目录" } else { "文件" }
        ));
    }

    format!(
        "<!doctype html><html lang=\"zh-CN\"><head><meta charset=\"utf-8\">\
         <title>{title}</title></head><body>\
         <h1>{title}</h1><table>\
         <thead><tr><th>名称</th><th>类型</th><th>大小</th></tr></thead>\
         <tbody>{rows}</tbody></table></body></html>"
    )
}

/// 最小 HTML 转义：防止文件名 / 路径中的特殊字符破坏标签或注入脚本。
fn html_escape(input: &str) -> String {
    let mut out = String::with_capacity(input.len());
    for ch in input.chars() {
        match ch {
            '&' => out.push_str("&amp;"),
            '<' => out.push_str("&lt;"),
            '>' => out.push_str("&gt;"),
            '"' => out.push_str("&quot;"),
            '\'' => out.push_str("&#39;"),
            _ => out.push(ch),
        }
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;
    use axum::http::HeaderValue;

    #[test]
    fn 原生格式不参与通用目录浏览() {
        assert!(!is_browsable_format("npm"));
        assert!(!is_browsable_format("docker"));
        assert!(is_browsable_format("raw"));
        assert!(is_browsable_format("maven"));
    }

    #[test]
    fn accept_协商偏好判定() {
        let mut h = HeaderMap::new();
        // 浏览器默认 Accept 以 text/html 起头 → HTML
        h.insert(
            header::ACCEPT,
            HeaderValue::from_static("text/html,application/xhtml+xml,*/*"),
        );
        assert!(wants_html(&h));

        // 明确要 JSON → 不要 HTML
        h.insert(header::ACCEPT, HeaderValue::from_static("application/json"));
        assert!(!wants_html(&h));

        // 不带 Accept → 默认 JSON
        let empty = HeaderMap::new();
        assert!(!wants_html(&empty));
    }

    #[test]
    fn html_转义防注入() {
        assert_eq!(html_escape("a<b>&\"'"), "a&lt;b&gt;&amp;&quot;&#39;");
    }

    /// 便捷：构造一个文件目录项。
    fn 文件项(name: &str) -> DirEntry {
        DirEntry {
            name: name.to_string(),
            kind: DirEntryKind::File,
            size: Some(1),
            sha256: None,
            created_at: None,
        }
    }

    #[test]
    fn html目录_非根目录含返回上级链接() {
        // 非根目录（dir_path 非空）：表内须含 `../` 返回上级链接
        let body = build_html_body("repo", "a/b", &[文件项("c.txt")]);
        assert!(
            body.contains("<a href=\"../\">../</a>"),
            "非根目录应含返回上级链接，实际: {body}"
        );
    }

    #[test]
    fn html目录_根目录不含返回上级链接() {
        // 根目录（dir_path 为空）：不应出现 `../` 链接，避免越出仓库根
        let body = build_html_body("repo", "", &[文件项("c.txt")]);
        assert!(
            !body.contains("href=\"../\""),
            "根目录不应含返回上级链接，实际: {body}"
        );
    }
}
