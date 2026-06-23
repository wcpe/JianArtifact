//! Web 模块：经 `rust-embed` 在编译期嵌入前端构建产物（`frontend/dist`），并在 axum 中提供
//! 单页应用（SPA）静态资源与客户端路由回退。
//!
//! 路由优先级（在 `api::build_router` 中先挂 API / 格式 / 健康检查，再挂本模块）：
//! - `/assets/{*path}`：命中嵌入的静态资源（JS / CSS / 图片等），按扩展名推断 Content-Type；
//!   未命中返回 404。
//! - 其余未被前面路由匹配的 GET 请求（含 `/`、`/login`、`/repositories` 等客户端路由）经
//!   `fallback` 回退到 `index.html`，交由前端路由处理。
//!
//! 健壮性：干净检出下 `frontend/dist` 可能只有占位（无 `index.html`）。此时返回 503 友好提示页，
//! 使后端单测在未构建前端时仍可编译与运行通过（见 ADR-0001 构建顺序约定）。

use axum::{
    body::Body,
    extract::Path,
    http::{header, StatusCode, Uri},
    response::{IntoResponse, Response},
};
use rust_embed::RustEmbed;

/// 嵌入前端构建产物目录。编译期从 `frontend/dist` 读取（路径相对 crate 根）。
///
/// 干净检出下该目录仅含 `.gitkeep` 占位，嵌入为空集；构建前端后再 `cargo build` 即纳入真实产物。
#[derive(RustEmbed)]
#[folder = "frontend/dist"]
struct Assets;

/// SPA 入口文件名。
const INDEX_HTML: &str = "index.html";

/// 前端尚未构建时的占位提示页（503）。
const NOT_BUILT_HINT: &str = "<!doctype html><html lang=\"zh-CN\"><head><meta charset=\"utf-8\">\
<title>JianArtifact</title></head><body style=\"font-family:sans-serif;padding:2rem\">\
<h1>前端尚未构建</h1><p>请先执行 <code>pnpm -C frontend build</code> 再重新构建后端二进制，\
以便嵌入 Web 控制台资源。后端 API 与格式端点不受影响，可正常使用。</p></body></html>";

/// 提供 `/assets/{*path}` 静态资源：命中返回内容并按扩展名设置 Content-Type，未命中 404。
pub async fn serve_asset(Path(path): Path<String>) -> Response {
    // 资源在嵌入集合中的键包含 assets/ 前缀（与 dist 布局一致）
    let asset_path = format!("assets/{path}");
    match Assets::get(&asset_path) {
        Some(content) => asset_response(&asset_path, content.data.into_owned()),
        None => StatusCode::NOT_FOUND.into_response(),
    }
}

/// SPA 回退：未被其它路由匹配的请求一律返回 `index.html`，交前端路由处理。
///
/// `index.html` 不存在（前端未构建）时返回 503 占位页，保证后端可独立运行 / 测试。
pub async fn spa_fallback(_uri: Uri) -> Response {
    match Assets::get(INDEX_HTML) {
        Some(content) => {
            let mut resp = Body::from(content.data.into_owned()).into_response();
            resp.headers_mut().insert(
                header::CONTENT_TYPE,
                header::HeaderValue::from_static("text/html; charset=utf-8"),
            );
            resp
        }
        None => (
            StatusCode::SERVICE_UNAVAILABLE,
            [(header::CONTENT_TYPE, "text/html; charset=utf-8")],
            NOT_BUILT_HINT,
        )
            .into_response(),
    }
}

/// 据资源路径扩展名推断 Content-Type，构造带正确头的静态资源响应。
fn asset_response(path: &str, data: Vec<u8>) -> Response {
    let mime = mime_guess::from_path(path).first_or_octet_stream();
    let mut resp = Body::from(data).into_response();
    if let Ok(value) = header::HeaderValue::from_str(mime.as_ref()) {
        resp.headers_mut().insert(header::CONTENT_TYPE, value);
    }
    resp
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn 占位页提示包含构建指引() {
        // 干净检出下没有 index.html，spa_fallback 应给出构建提示文案
        assert!(NOT_BUILT_HINT.contains("pnpm -C frontend build"));
    }
}
