//! 指标时序查询端点（FR-105，ADR-0027）：`GET /api/v1/monitor/metrics`，仅 Admin。
//!
//! 设计要点：
//! - **薄 handler**：只做鉴权编排（仅 Admin）、参数缺省补全、调用 `meta` 取原始样本、调用
//!   `metrics_sampler::downsample` 纯函数聚合、组装响应；聚合逻辑下沉纯函数，handler 不写业务。
//! - **仅 Admin**：时序属管理视图，未认证 401、非管理员 403。
//! - **本机内部、不外发**（ADR-0009 / 0015 基调）：纯本地时序查询，不接任何外部上报 / 导出。

use axum::{
    extract::{Query, State},
    Json,
};
use serde::{Deserialize, Serialize};

use super::metrics_sampler::{downsample, TsPoint};
use super::{ApiError, AppState, Identity};

/// 缺省时间窗：to 不传时取「现在」，from 不传时取 to 往前 1 小时。
const DEFAULT_WINDOW_MS: i64 = 3_600_000;

/// 时序查询参数。
#[derive(Debug, Deserialize)]
pub struct MetricsQuery {
    /// 指标键（必填，缺失 / 空返回 400）。
    pub metric: String,
    /// 范围起点（Unix 毫秒）；缺省为 `to - 1 小时`。
    #[serde(default)]
    pub from: Option<i64>,
    /// 范围终点（Unix 毫秒）；缺省为「现在」。
    #[serde(default)]
    pub to: Option<i64>,
    /// 降采样步长（毫秒）；缺省 0 表示不降采样（返回原始样本点）。
    #[serde(default)]
    pub step: Option<i64>,
}

/// 时序查询响应：指标键 + 时序点列表。
#[derive(Debug, Serialize)]
pub struct MetricsSeriesDto {
    /// 指标键。
    pub metric: String,
    /// 时序点（按 ts 升序；降采样后为桶起点 + 桶内平均）。
    pub points: Vec<TsPoint>,
}

/// 查询指标时序（仅 Admin）：按 metric / from / to / step 取样并降采样返回。
///
/// 纯查本机内部时序、不外发；聚合逻辑由 `downsample` 纯函数承担，handler 不写业务。
pub async fn query_metrics(
    State(state): State<AppState>,
    identity: Identity,
    Query(query): Query<MetricsQuery>,
) -> Result<Json<MetricsSeriesDto>, ApiError> {
    identity.require_admin()?;

    let metric = query.metric.trim();
    if metric.is_empty() {
        return Err(ApiError::BadRequest("metric 参数不能为空".to_string()));
    }

    // to 缺省取现在；from 缺省取 to 往前 1 小时；step 缺省 0（不降采样）
    let now_ms = now_millis();
    let to = query.to.unwrap_or(now_ms);
    let from = query.from.unwrap_or(to - DEFAULT_WINDOW_MS);
    let step = query.step.unwrap_or(0);

    let samples = state.meta.query_metric_samples(metric, from, to).await?;
    let points = downsample(&samples, step);

    Ok(Json(MetricsSeriesDto {
        metric: metric.to_string(),
        points,
    }))
}

/// 当前 Unix 毫秒（UTC）。
fn now_millis() -> i64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_millis() as i64)
        .unwrap_or(0)
}

#[cfg(test)]
mod tests {
    use super::super::tests::{测试用状态, 读_json};
    use super::super::AppState;
    use crate::auth::hash_password;
    use crate::meta::{NewMetricSample, Role};
    use axum::body::Body;
    use axum::http::{Request, StatusCode};
    use tower::ServiceExt;

    /// 在状态库内建一个指定角色用户并签发其会话 JWT。
    async fn 签发令牌(state: &AppState, name: &str, role: Role) -> String {
        let uid = state
            .meta
            .create_user(name, &hash_password("pw").unwrap(), role)
            .await
            .unwrap();
        state.jwt.issue(&uid, name, role).unwrap()
    }

    /// 便捷：带可选 Bearer 令牌请求时序查询端点。
    async fn 请求时序(
        state: AppState,
        uri: &str,
        令牌: Option<&str>,
    ) -> axum::response::Response {
        let app = super::super::build_router(state);
        let mut builder = Request::builder().uri(uri);
        if let Some(t) = 令牌 {
            builder = builder.header("Authorization", format!("Bearer {t}"));
        }
        app.oneshot(builder.body(Body::empty()).unwrap())
            .await
            .unwrap()
    }

    #[tokio::test]
    async fn 匿名访问被拒_401() {
        let (state, _dir) = 测试用状态().await;
        let resp = 请求时序(
            state,
            "/api/v1/monitor/metrics?metric=host.cpu_percent",
            None,
        )
        .await;
        assert_eq!(resp.status(), StatusCode::UNAUTHORIZED);
    }

    #[tokio::test]
    async fn 普通用户访问被拒_403() {
        let (state, _dir) = 测试用状态().await;
        let token = 签发令牌(&state, "u", Role::User).await;
        let resp = 请求时序(
            state,
            "/api/v1/monitor/metrics?metric=host.cpu_percent",
            Some(&token),
        )
        .await;
        assert_eq!(resp.status(), StatusCode::FORBIDDEN);
    }

    #[tokio::test]
    async fn 缺少_metric_参数返回_400() {
        let (state, _dir) = 测试用状态().await;
        let token = 签发令牌(&state, "admin", Role::Admin).await;
        let resp = 请求时序(state, "/api/v1/monitor/metrics", Some(&token)).await;
        assert_eq!(resp.status(), StatusCode::BAD_REQUEST);
    }

    #[tokio::test]
    async fn 管理员查询返回时序点且按_ts_升序() {
        let (state, _dir) = 测试用状态().await;
        let token = 签发令牌(&state, "admin", Role::Admin).await;
        // 经 meta 直接插几条样本（乱序），断言查回非空且升序
        state
            .meta
            .insert_metric_samples(&[
                NewMetricSample {
                    metric_key: "host.cpu_percent".into(),
                    ts: 3000,
                    value: 3.0,
                },
                NewMetricSample {
                    metric_key: "host.cpu_percent".into(),
                    ts: 1000,
                    value: 1.0,
                },
                NewMetricSample {
                    metric_key: "host.cpu_percent".into(),
                    ts: 2000,
                    value: 2.0,
                },
            ])
            .await
            .unwrap();

        let resp = 请求时序(
            state,
            "/api/v1/monitor/metrics?metric=host.cpu_percent&from=0&to=10000",
            Some(&token),
        )
        .await;
        assert_eq!(resp.status(), StatusCode::OK);
        let body = 读_json(resp).await;
        assert_eq!(body["metric"], "host.cpu_percent");
        let points = body["points"].as_array().unwrap();
        assert_eq!(points.len(), 3);
        // 按 ts 升序
        assert_eq!(points[0]["ts"], 1000);
        assert_eq!(points[1]["ts"], 2000);
        assert_eq!(points[2]["ts"], 3000);
    }
}
