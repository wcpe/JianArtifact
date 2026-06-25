//! 管理员防护状态端点（FR-56，ADR-0017）：返回本机七层防护的健康快照与告警历史。
//!
//! 设计要点：
//! - **薄 handler**：只做鉴权编排、调用进程内评估器 / `meta` 查询、组装响应；不写业务逻辑。
//! - **仅 Admin**：防护运行画像属管理视图，未认证 401、非管理员 403（复用 `Identity::require_admin`）。
//! - **纯本机聚合、零外发**：窗内计数取自进程内 [`crate::api::AlertEngine`]，封禁 IP 数取自进程内
//!   封禁登记表，告警历史查本地 SQLite；**绝不外发、不向外部 phone-home**（架构不变量 / ADR-0009）。
//! - **分页复用统一结构**：告警历史查询沿用项目统一 offset/limit 分页响应（对齐 API.md §1）。

use std::time::Instant;

use axum::{
    extract::{Query, State},
    Json,
};
use serde::{Deserialize, Serialize};

use crate::meta::{AlertQuery, AlertRecord};

use super::{ApiError, AppState, Identity};

/// 默认分页容量。
const DEFAULT_LIMIT: i64 = 50;
/// 分页容量上限（对齐 API.md）。
const MAX_LIMIT: i64 = 1000;
/// 状态快照中内联返回的最近告警条数。
const RECENT_ALERTS: i64 = 10;

/// 单维度窗内计数（状态快照项）。
#[derive(Debug, Serialize)]
pub struct DimensionCountDto {
    /// 防护维度（rate_limit / ban / cc_challenge / waf / slowloris）。
    pub dimension: String,
    /// 当前评估窗内累计计数。
    pub count: u64,
}

/// 单条告警视图（对齐 protection_alerts 字段）。
#[derive(Debug, Serialize)]
pub struct AlertDto {
    /// 自增主键。
    pub id: i64,
    /// 告警时间（UTC）。
    pub ts: String,
    /// 防护维度。
    pub dimension: String,
    /// 严重度（warn | error）。
    pub severity: String,
    /// 触发告警时的窗内观测计数。
    pub observed_value: i64,
    /// 触发告警的阈值。
    pub threshold: i64,
    /// 评估时间窗时长（秒）。
    pub window_secs: i64,
    /// 结构化补充。
    pub detail: Option<String>,
}

impl From<AlertRecord> for AlertDto {
    fn from(r: AlertRecord) -> Self {
        Self {
            id: r.id,
            ts: r.ts,
            dimension: r.dimension,
            severity: r.severity,
            observed_value: r.observed_value,
            threshold: r.threshold,
            window_secs: r.window_secs,
            detail: r.detail,
        }
    }
}

/// 防护状态快照（数据面板总览）。
#[derive(Debug, Serialize)]
pub struct ProtectionStatusDto {
    /// 告警评估是否启用。
    pub alerts_enabled: bool,
    /// 当前评估窗时长（秒）。
    pub window_secs: u64,
    /// 各防护维度当前窗内计数。
    pub window_counts: Vec<DimensionCountDto>,
    /// 当前处于封禁中的 IP 数。
    pub active_banned_ips: usize,
    /// 因队列满被丢弃的告警累计数（采集降级观测）。
    pub dropped_alerts: u64,
    /// 最近若干条告警（按时间倒序）。
    pub recent_alerts: Vec<AlertDto>,
}

/// 防护状态端点（仅 Admin）：返回各维度窗内计数、当前封禁 IP 数、最近告警。
///
/// 纯本机聚合、零外发（架构不变量 / ADR-0009）；非管理员被拒。
pub async fn protection_status(
    State(state): State<AppState>,
    identity: Identity,
) -> Result<Json<ProtectionStatusDto>, ApiError> {
    identity.require_admin()?;

    // 从热替换槽取当前快照（FR-79）：状态展示的告警窗口 / 启用状态须反映当前生效配置
    let snapshot = state.protection.snapshot();
    let alerts_cfg = &snapshot.config.alerts;
    let window_secs = alerts_cfg.window_secs.max(1);
    let now = Instant::now();

    // 各维度窗内计数（进程内评估器，纯内存读取）
    let window_counts = state
        .alert_engine
        .snapshot(now, window_secs)
        .into_iter()
        .map(|d| DimensionCountDto {
            dimension: d.dimension.as_str().to_string(),
            count: d.count,
        })
        .collect();

    // 当前封禁中的 IP 数（进程内封禁登记表）
    let active_banned_ips = state.ban_registry.active_ban_count(now);

    // 最近若干条告警（查本地 SQLite，零外发）
    let recent = state
        .meta
        .query_alerts(&AlertQuery {
            offset: 0,
            limit: RECENT_ALERTS,
            ..Default::default()
        })
        .await?
        .into_iter()
        .map(AlertDto::from)
        .collect();

    Ok(Json(ProtectionStatusDto {
        alerts_enabled: alerts_cfg.enabled,
        window_secs,
        window_counts,
        active_banned_ips,
        dropped_alerts: state.alerts.dropped_count(),
        recent_alerts: recent,
    }))
}

/// 告警历史查询参数。
#[derive(Debug, Deserialize)]
pub struct AlertListQuery {
    /// 按维度过滤（可选）。
    #[serde(default)]
    pub dimension: Option<String>,
    /// 分页起点（默认 0）。
    #[serde(default)]
    pub offset: Option<i64>,
    /// 分页容量（默认 50，上限 1000）。
    #[serde(default)]
    pub limit: Option<i64>,
}

/// 统一分页响应结构（对齐 API.md §1）。
#[derive(Debug, Serialize)]
pub struct Paginated {
    /// 本页命中项。
    pub items: Vec<AlertDto>,
    /// 满足过滤的总数。
    pub total: i64,
    /// 本页起点。
    pub offset: i64,
    /// 本页容量。
    pub limit: i64,
    /// 是否还有更多。
    pub has_more: bool,
}

/// 列出告警历史（仅 Admin）：按时间倒序，支持维度过滤与分页。
///
/// 纯查本地 SQLite、零外发；非管理员被拒。
pub async fn list_alerts(
    State(state): State<AppState>,
    identity: Identity,
    Query(query): Query<AlertListQuery>,
) -> Result<Json<Paginated>, ApiError> {
    identity.require_admin()?;
    let offset = query.offset.unwrap_or(0).max(0);
    let limit = query.limit.unwrap_or(DEFAULT_LIMIT).clamp(1, MAX_LIMIT);

    let filter = AlertQuery {
        dimension: query.dimension.as_deref(),
        offset,
        limit,
    };
    let total = state.meta.count_alerts(&filter).await?;
    let rows = state.meta.query_alerts(&filter).await?;
    let items: Vec<AlertDto> = rows.into_iter().map(AlertDto::from).collect();
    let has_more = offset + (items.len() as i64) < total;

    Ok(Json(Paginated {
        items,
        total,
        offset,
        limit,
        has_more,
    }))
}
