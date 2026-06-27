//! 开源许可查询端点（FR-102，ADR-0025）：返回构建期扫描嵌入的依赖许可清单。
//!
//! 设计要点：
//! - **薄 handler**：只取嵌入清单组装响应；扫描 / 生成在构建期完成，运行时不写业务、不联网。
//! - **公开（匿名可读）**：开源归因本应公开可查，端点不经鉴权门（不调 `require_*`），
//!   匿名亦可访问；不涉及任何私有 / 敏感数据。
//! - **静态嵌入、不碰 DB / 网络**：清单经 `include_str!` 编译期嵌入（见 `crate::licenses`），
//!   本端点纯读内存，不外发、不 phone-home（守 ADR-0009 基调）；GET 读取类不入审计。

use axum::Json;

use crate::licenses::{self, LicenseManifest};

/// 查询开源许可清单（公开，匿名可读）：返回 Rust + 前端、运行时 + 开发依赖的归因与汇总。
///
/// 本地未生成时返回占位清单（`generated=false`、空条目），前端据此显空态。
pub async fn list_licenses() -> Json<LicenseManifest> {
    Json(licenses::embedded().clone())
}

#[cfg(test)]
mod tests {
    use super::super::tests::{测试用状态, 读_json};
    use axum::body::Body;
    use axum::http::{Request, StatusCode};
    use tower::ServiceExt;

    /// 便捷：带可选 Bearer 令牌请求许可端点。
    async fn 请求许可(令牌: Option<&str>) -> axum::response::Response {
        let (state, _dir) = 测试用状态().await;
        let app = super::super::build_router(state);
        let mut builder = Request::builder().uri("/api/v1/licenses");
        if let Some(t) = 令牌 {
            builder = builder.header("Authorization", format!("Bearer {t}"));
        }
        app.oneshot(builder.body(Body::empty()).unwrap())
            .await
            .unwrap()
    }

    #[tokio::test]
    async fn 匿名可读返回_200() {
        // 关键：公开端点匿名可读，不被鉴权门拦成 401/403
        let resp = 请求许可(None).await;
        assert_eq!(resp.status(), StatusCode::OK);
    }

    #[tokio::test]
    async fn 返回许可清单结构() {
        let resp = 请求许可(None).await;
        assert_eq!(resp.status(), StatusCode::OK);
        let body = 读_json(resp).await;
        // 结构含 generated / entries / summary
        assert!(body["generated"].is_boolean(), "应含 generated 字段");
        assert!(body["entries"].is_array(), "entries 应为数组");
        assert!(body["summary"].is_object(), "应含 summary 字段");
        // summary 含四项统计
        assert!(body["summary"]["total"].is_u64(), "summary.total 应为整数");
        assert!(
            body["summary"]["runtime"].is_u64(),
            "summary.runtime 应为整数"
        );
        assert!(body["summary"]["dev"].is_u64(), "summary.dev 应为整数");
        assert!(
            body["summary"]["licenses"].is_u64(),
            "summary.licenses 应为整数"
        );
    }
}
