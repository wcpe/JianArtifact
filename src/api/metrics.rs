//! Prometheus 指标端点（FR-32，ADR-0015）：进程内注册表 + 本机 `GET /metrics`（pull 模型）。
//!
//! 设计（严格照 ADR-0015 的"指标"部分）：
//! - **进程内 recorder**：用 `metrics` facade + `metrics-exporter-prometheus` 进程内 recorder，
//!   不引外部时序库、不主动向任何外部端点 push / remote-write；仅在 `/metrics` 被抓取时渲染。
//! - **低基数标签**：HTTP 维度只用 method / status_class / format 等**有界枚举值**，
//!   **严禁**以仓库名 / 路径 / 用户名作无界标签（基数纪律见 [`crate::metrics_keys`]）。
//! - **热路径低开销**：中间件只做原子计数 / 直方图观测（`metrics` 宏，无锁），
//!   不在热路径做字符串格式化或锁外重 IO；渲染只在抓取时发生。
//! - **端点鉴权**：默认要求认证且仅 Admin；`observability.metrics.allow_anonymous=true` 时放开匿名
//!   （供把 `/metrics` 限定在内网 / 反代后的部署）。`enabled=false` 时端点返回 404。

use std::sync::Arc;
use std::time::Instant;

use axum::{
    extract::{Request, State},
    http::{header, Method, StatusCode},
    middleware::Next,
    response::{IntoResponse, Response},
};
use metrics::{counter, gauge, histogram};
use metrics_exporter_prometheus::{Matcher, PrometheusBuilder, PrometheusHandle};

use crate::metrics_keys as keys;

use super::{ApiError, AppState, Identity};

/// 延迟直方图的桶边界（秒）：覆盖亚毫秒到十秒，匹配制品库读写 / 回源的典型分布。
const LATENCY_BUCKETS: &[f64] = &[
    0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0, 10.0,
];

/// 进程内 Prometheus 注册表句柄：随 AppState 共享，`/metrics` 抓取时调用 `render`。
///
/// 克隆廉价（内部 Arc）。仅当 `observability.metrics.enabled=true` 时由 [`install_recorder`]
/// 安装全局 recorder 并产出本句柄；未启用时 AppState 持 `None`，端点返回 404。
#[derive(Clone)]
pub struct MetricsHandle {
    handle: Arc<PrometheusHandle>,
}

impl MetricsHandle {
    /// 渲染当前注册表为 Prometheus 文本格式（仅在抓取时调用）。
    fn render(&self) -> String {
        self.handle.render()
    }
}

/// 安装进程内 Prometheus 全局 recorder 并返回句柄。
///
/// 进程内只应安装一次（在启动编排中调用一次）；为延迟直方图设置固定桶边界。
/// 失败（如重复安装）返回错误，由调用方决定降级（记 WARN 后不挂端点）。
pub fn install_recorder() -> Result<MetricsHandle, String> {
    let handle = PrometheusBuilder::new()
        .set_buckets_for_metric(
            Matcher::Suffix("_duration_seconds".to_string()),
            LATENCY_BUCKETS,
        )
        .map_err(|e| format!("设置延迟直方图桶失败: {e}"))?
        .install_recorder()
        .map_err(|e| format!("安装 Prometheus recorder 失败: {e}"))?;
    Ok(MetricsHandle {
        handle: Arc::new(handle),
    })
}

/// `format` 标签来源：格式 handler 在解析出 `repo.format` 后写入响应扩展，供中间件读取，
/// 避免中间件为取 format 再查一次 DB（热路径不加 DB 查询）。值为有界静态格式名。
#[derive(Clone, Copy)]
pub struct FormatLabel(pub &'static str);

/// 指标中间件：采集 HTTP 维度（method / status_class / format）+ 延迟直方图
/// + 上传 / 下载字节 + 当前并发上传数。置于请求热路径，只做原子观测。
///
/// `format` 标签优先取响应扩展中的 [`FormatLabel`]（由格式 handler 写入，零额外 DB 查询）；
/// 缺失时按路径命名空间静态归类（`/v2/*` → docker，其余 → unknown），守低基数。
/// 上传字节取请求体 `Content-Length`（流式上传不预先读体，故用声明长度近似）；
/// 下载字节取响应体 `Content-Length`。
pub async fn metrics_layer(
    State(_state): State<AppState>,
    request: Request,
    next: Next,
) -> Response {
    let method = method_label(request.method());
    let path_format = format_label_from_path(request.uri().path());
    let is_upload = is_upload_method(request.method());
    let upload_bytes = if is_upload {
        content_length(request.headers())
    } else {
        0
    };

    // 并发上传数：进入时 +1，离开时 -1（gauge）。仅对写方法计数，避免读流量噪声。
    let in_flight = if is_upload {
        let g = gauge!(keys::HTTP_UPLOADS_IN_FLIGHT);
        g.increment(1.0);
        Some(g)
    } else {
        None
    };

    let started = Instant::now();
    let response = next.run(request).await;
    let elapsed = started.elapsed().as_secs_f64();

    if let Some(g) = in_flight {
        g.decrement(1.0);
    }

    // format 标签：优先取 handler 写入的响应扩展，缺失回退路径命名空间归类
    let format = response
        .extensions()
        .get::<FormatLabel>()
        .map(|f| f.0)
        .unwrap_or(path_format);

    let status = response.status();
    let status_class = keys::status_class(status.as_u16());

    // 请求计数（method / status_class / format）
    counter!(
        keys::HTTP_REQUESTS_TOTAL,
        keys::LABEL_METHOD => method,
        keys::LABEL_STATUS_CLASS => status_class,
        keys::LABEL_FORMAT => format,
    )
    .increment(1);

    // 延迟直方图（method / format）
    histogram!(
        keys::HTTP_REQUEST_DURATION_SECONDS,
        keys::LABEL_METHOD => method,
        keys::LABEL_FORMAT => format,
    )
    .record(elapsed);

    // 上传 / 下载字节累计（按 format）。仅在成功类响应上计上传字节，避免把被拒上传计入。
    if is_upload && status.is_success() && upload_bytes > 0 {
        counter!(keys::HTTP_UPLOAD_BYTES_TOTAL, keys::LABEL_FORMAT => format)
            .increment(upload_bytes);
    }
    if !is_upload {
        let down = content_length(response.headers());
        if down > 0 {
            counter!(keys::HTTP_DOWNLOAD_BYTES_TOTAL, keys::LABEL_FORMAT => format).increment(down);
        }
    }

    response
}

/// `GET /metrics` 处理器：按配置鉴权后渲染注册表为 Prometheus 文本。
///
/// - `enabled=false`：返回 404（端点形同不存在，不泄露运行画像）。
/// - `allow_anonymous=false`（默认）：要求认证且仅 Admin，否则 401 / 403。
/// - `allow_anonymous=true`：免认证抓取（运维须把端点限内网 / 反代后）。
pub async fn metrics_endpoint(
    State(state): State<AppState>,
    identity: Identity,
) -> Result<Response, ApiError> {
    let cfg = &state.config.observability.metrics;
    // 未启用：端点形同不存在
    let handle = match (cfg.enabled, &state.metrics) {
        (true, Some(h)) => h,
        _ => return Err(ApiError::NotFound),
    };

    // 鉴权：默认仅 Admin；显式开启匿名时跳过
    if !cfg.allow_anonymous {
        identity.require_admin()?;
    }

    // 渲染前把审计 channel 丢弃累计数同步进注册表（gauge），随指标一并暴露
    gauge!(keys::AUDIT_DROPPED_TOTAL).set(state.audit.dropped_count() as f64);
    // 渲染前把当前封禁 IP 数同步进注册表（gauge，FR-56）：反映实时封禁规模
    gauge!(keys::BAN_ACTIVE_IPS).set(
        state
            .ban_registry
            .active_ban_count(std::time::Instant::now()) as f64,
    );

    let body = handle.render();
    // Prometheus 文本格式的标准 Content-Type
    Ok((
        StatusCode::OK,
        [(
            header::CONTENT_TYPE,
            "text/plain; version=0.0.4; charset=utf-8",
        )],
        body,
    )
        .into_response())
}

/// 把 HTTP 方法映射为低基数的静态标签（避免分配；非常见方法归 other）。
fn method_label(method: &Method) -> &'static str {
    match *method {
        Method::GET => "GET",
        Method::HEAD => "HEAD",
        Method::POST => "POST",
        Method::PUT => "PUT",
        Method::PATCH => "PATCH",
        Method::DELETE => "DELETE",
        Method::OPTIONS => "OPTIONS",
        _ => "other",
    }
}

/// 是否为写（上传）方法：用于并发上传 gauge 与上传字节归类。
fn is_upload_method(method: &Method) -> bool {
    matches!(*method, Method::PUT | Method::POST | Method::PATCH)
}

/// 仅从请求路径命名空间静态归类 `format` 标签（不查 DB，守热路径）。
///
/// `/v2/*` 固定为 `docker`（Docker Registry v2）；通用格式 API（`/{repo}/{path}`）因 URL 本身
/// 不含格式信息，回退 `unknown`——真实格式名由格式 handler 经 [`FormatLabel`] 响应扩展补全。
/// 仅返回**有界枚举**值，绝不把仓库名 / 路径作标签（守基数纪律）。
fn format_label_from_path(path: &str) -> &'static str {
    let trimmed = path.trim_start_matches('/');
    if trimmed == "v2" || trimmed.starts_with("v2/") {
        return "docker";
    }
    keys::FORMAT_UNKNOWN
}

/// 读取 `Content-Length` 头为字节数（缺失 / 非法返回 0）。
fn content_length(headers: &header::HeaderMap) -> u64 {
    headers
        .get(header::CONTENT_LENGTH)
        .and_then(|v| v.to_str().ok())
        .and_then(|s| s.parse::<u64>().ok())
        .unwrap_or(0)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn 方法映射为有界标签() {
        assert_eq!(method_label(&Method::GET), "GET");
        assert_eq!(method_label(&Method::PUT), "PUT");
        assert_eq!(
            method_label(&Method::from_bytes(b"PROPFIND").unwrap()),
            "other"
        );
    }

    #[test]
    fn 写方法判定() {
        assert!(is_upload_method(&Method::PUT));
        assert!(is_upload_method(&Method::POST));
        assert!(is_upload_method(&Method::PATCH));
        assert!(!is_upload_method(&Method::GET));
        assert!(!is_upload_method(&Method::DELETE));
    }

    #[test]
    fn content_length_解析() {
        let mut h = header::HeaderMap::new();
        assert_eq!(content_length(&h), 0);
        h.insert(header::CONTENT_LENGTH, "1024".parse().unwrap());
        assert_eq!(content_length(&h), 1024);
        h.insert(header::CONTENT_LENGTH, "abc".parse().unwrap());
        assert_eq!(content_length(&h), 0);
    }
}
