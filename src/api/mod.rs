//! API 层：axum 路由与中间件。本层保持轻薄，只做协议适配与错误转换，不写业务逻辑。
//!
//! 本批仅提供健康检查端点与统一错误类型；认证/鉴权/格式路由为后续批次。

use std::sync::Arc;

use axum::{
    extract::State,
    http::StatusCode,
    response::{IntoResponse, Response},
    routing::get,
    Json, Router,
};
use serde_json::json;
use tower_http::request_id::{MakeRequestUuid, PropagateRequestIdLayer, SetRequestIdLayer};
use tower_http::trace::TraceLayer;

use crate::config::Config;
use crate::meta::MetaStore;
use crate::storage::LocalFsStore;

/// 请求 ID 头名称。
const REQUEST_ID_HEADER: &str = "x-request-id";

/// 应用共享状态：配置、元数据存储、blob 存储。
///
/// 用 Arc 包裹以便在各 handler 间廉价克隆共享。
#[derive(Clone)]
pub struct AppState {
    /// 运行期配置。
    pub config: Arc<Config>,
    /// 元数据存储（内部已是连接池，克隆廉价）。
    pub meta: MetaStore,
    /// blob 存储。
    pub store: LocalFsStore,
}

/// 统一 API 错误类型，转换为 JSON 响应 `{"error":{"code","message"}}`。
#[derive(Debug, thiserror::Error)]
pub enum ApiError {
    /// 内部服务器错误。
    #[error("内部服务器错误")]
    Internal,
}

impl ApiError {
    /// 返回该错误对应的 HTTP 状态码。
    fn status(&self) -> StatusCode {
        match self {
            ApiError::Internal => StatusCode::INTERNAL_SERVER_ERROR,
        }
    }

    /// 返回该错误的稳定错误码（供客户端机器识别）。
    fn code(&self) -> &'static str {
        match self {
            ApiError::Internal => "internal_error",
        }
    }
}

impl IntoResponse for ApiError {
    fn into_response(self) -> Response {
        let status = self.status();
        let body = Json(json!({
            "error": {
                "code": self.code(),
                "message": self.to_string(),
            }
        }));
        (status, body).into_response()
    }
}

/// 构建 axum 路由：挂健康检查端点与请求 ID、追踪中间件。
pub fn build_router(state: AppState) -> Router {
    Router::new()
        .route("/health", get(health))
        .with_state(state)
        // 中间件顺序：设置请求 ID → 追踪 → 透传请求 ID 到响应
        .layer(PropagateRequestIdLayer::x_request_id())
        .layer(TraceLayer::new_for_http())
        .layer(SetRequestIdLayer::new(
            REQUEST_ID_HEADER.parse().expect("请求 ID 头名称合法"),
            MakeRequestUuid,
        ))
}

/// 健康检查处理器：无需认证，返回 200 与简单状态 JSON。
async fn health(State(state): State<AppState>) -> impl IntoResponse {
    Json(json!({
        "status": "ok",
        "version": env!("CARGO_PKG_VERSION"),
        // 不泄露敏感信息，仅回显服务监听端口供探活区分
        "port": state.config.server.port,
    }))
}

#[cfg(test)]
mod tests {
    use super::*;
    use axum::body::Body;
    use axum::http::Request;
    use http_body_util::BodyExt;
    use tower::ServiceExt;

    async fn 测试用状态() -> (AppState, tempfile::TempDir) {
        let dir = tempfile::tempdir().unwrap();
        let meta = MetaStore::open_in_memory().await.unwrap();
        let store = LocalFsStore::new(dir.path().join("blobs")).await.unwrap();
        let state = AppState {
            config: Arc::new(Config::default()),
            meta,
            store,
        };
        (state, dir)
    }

    #[tokio::test]
    async fn health_返回_200_与_ok状态() {
        let (state, _dir) = 测试用状态().await;
        let app = build_router(state);

        let response = app
            .oneshot(
                Request::builder()
                    .uri("/health")
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();

        assert_eq!(response.status(), StatusCode::OK);
        // 响应应带回请求 ID 头
        assert!(response.headers().contains_key(REQUEST_ID_HEADER));

        let bytes = response.into_body().collect().await.unwrap().to_bytes();
        let body: serde_json::Value = serde_json::from_slice(&bytes).unwrap();
        assert_eq!(body["status"], "ok");
        assert_eq!(body["port"], 8080);
    }

    #[tokio::test]
    async fn 未知路径返回_404() {
        let (state, _dir) = 测试用状态().await;
        let app = build_router(state);
        let response = app
            .oneshot(
                Request::builder()
                    .uri("/不存在")
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(response.status(), StatusCode::NOT_FOUND);
    }
}
