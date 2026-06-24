//! 指标键与标签常量（FR-32，ADR-0015）：集中定义指标名与低基数标签值。
//!
//! 单独成叶子模块（不依赖任何业务层），供 `api`（exporter + 中间件）与
//! `format`（proxy 回源边界埋点）共享，避免指标名以魔法字符串散落各处、防止跨层依赖。
//!
//! 基数纪律（ADR-0015 红线）：所有标签取**有界枚举值**——HTTP 方法、状态类（2xx/4xx/5xx）、
//! 格式名等——**严禁**以仓库名 / 路径 / 用户名 / 制品坐标等无界值作标签，否则注册表与抓取代价爆炸。

/// HTTP 请求计数（标签：method / status_class / format）。
pub const HTTP_REQUESTS_TOTAL: &str = "jianartifact_http_requests_total";
/// HTTP 请求延迟直方图（秒；标签：method / format）。
pub const HTTP_REQUEST_DURATION_SECONDS: &str = "jianartifact_http_request_duration_seconds";
/// 上传字节累计（标签：format）。
pub const HTTP_UPLOAD_BYTES_TOTAL: &str = "jianartifact_http_upload_bytes_total";
/// 下载字节累计（标签：format）。
pub const HTTP_DOWNLOAD_BYTES_TOTAL: &str = "jianartifact_http_download_bytes_total";
/// 当前并发上传数（gauge，进出请求增减）。
pub const HTTP_UPLOADS_IN_FLIGHT: &str = "jianartifact_http_uploads_in_flight";

/// 代理缓存命中 / 未命中计数（标签：result=hit|miss，format）。
pub const PROXY_CACHE_TOTAL: &str = "jianartifact_proxy_cache_total";
/// 上游回源耗时直方图（秒；标签：format）。
pub const PROXY_UPSTREAM_DURATION_SECONDS: &str = "jianartifact_proxy_upstream_duration_seconds";
/// 上游回源失败计数（标签：format）。
pub const PROXY_UPSTREAM_FAILURES_TOTAL: &str = "jianartifact_proxy_upstream_failures_total";

/// 审计事件因 channel 满被丢弃的累计数（gauge，渲染时从 AuditSink 读取）。
pub const AUDIT_DROPPED_TOTAL: &str = "jianartifact_audit_dropped_total";

/// 标签键：HTTP 方法。
pub const LABEL_METHOD: &str = "method";
/// 标签键：HTTP 状态类（2xx / 4xx / 5xx 等）。
pub const LABEL_STATUS_CLASS: &str = "status_class";
/// 标签键：制品格式名（maven / npm / docker / raw 等；未知为 unknown）。
pub const LABEL_FORMAT: &str = "format";
/// 标签键：缓存结果（hit / miss）。
pub const LABEL_RESULT: &str = "result";

/// 缓存命中标签值。
pub const RESULT_HIT: &str = "hit";
/// 缓存未命中标签值。
pub const RESULT_MISS: &str = "miss";
/// 格式未知时的占位标签值（保持低基数，避免空串）。
pub const FORMAT_UNKNOWN: &str = "unknown";

/// 把仓库格式名（DB 中的动态字符串）映射为**有界静态**的 format 标签值。
///
/// 仅识别已实现的格式集合，未知格式归 [`FORMAT_UNKNOWN`]——把标签基数锁死在已知格式数，
/// 防止异常 / 越界的格式名撑大标签基数（守 ADR-0015 基数纪律）。
pub fn format_label_for(format: &str) -> &'static str {
    match format {
        "maven" => "maven",
        "npm" => "npm",
        "docker" => "docker",
        "raw" => "raw",
        "pypi" => "pypi",
        "cargo" => "cargo",
        "go" => "go",
        "nuget" => "nuget",
        _ => FORMAT_UNKNOWN,
    }
}

/// 把 HTTP 状态码归类为低基数的状态类标签（`2xx` / `4xx` / `5xx` 等）。
///
/// 用整百段而非逐码，避免状态码成为高基数标签。
pub fn status_class(status: u16) -> &'static str {
    match status / 100 {
        1 => "1xx",
        2 => "2xx",
        3 => "3xx",
        4 => "4xx",
        5 => "5xx",
        _ => "other",
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn 状态类按整百段归类() {
        assert_eq!(status_class(100), "1xx");
        assert_eq!(status_class(200), "2xx");
        assert_eq!(status_class(204), "2xx");
        assert_eq!(status_class(301), "3xx");
        assert_eq!(status_class(404), "4xx");
        assert_eq!(status_class(500), "5xx");
        assert_eq!(status_class(999), "other");
    }
}
