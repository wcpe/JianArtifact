//! 系统日志查询端点（FR-107，ADR-0029）：读运行日志文件 → 纯函数解析 + 级别过滤 + tail/分页，仅 Admin。
//!
//! 设计要点：
//! - **薄 handler**：只做鉴权（仅 Admin）、读文件、调 `logs` 纯函数、封装分页响应；解析 / 过滤 / 切片逻辑
//!   全在 `crate::logs`（无 IO、可穷举单测），本 handler 不写业务。
//! - **仅 Admin**：运行日志属管理视图，未认证 401、非管理员 403。
//! - **载体文件、不落库**：读 `{data_dir}/logs/app.log`，与审计（业务留痕落 SQLite）严格区分。
//! - **健壮**：文件不存在 / 为空 → 返回空列表、HTTP 200，不报错。GET 读取类不入审计（与 FR-97 一致）。

use axum::{
    extract::{Query, State},
    Json,
};
use serde::{Deserialize, Serialize};

use crate::logs::{self, LogEntry};

use super::{ApiError, AppState, Identity};

/// 默认分页容量（最近 N 行）。
const DEFAULT_LIMIT: i64 = 200;
/// 分页容量上限（对齐 API.md）。
const MAX_LIMIT: i64 = 1000;

/// 系统日志查询参数。
#[derive(Debug, Deserialize)]
pub struct SystemLogListQuery {
    /// 按级别过滤（可选，大小写不敏感：ERROR/WARN/INFO/DEBUG/TRACE）；无法识别即视为不过滤。
    #[serde(default)]
    pub level: Option<String>,
    /// 分页起点（从最新行起向更旧偏移，默认 0）。
    #[serde(default)]
    pub offset: Option<i64>,
    /// 分页容量（默认 200，上限 1000）。
    #[serde(default)]
    pub limit: Option<i64>,
}

/// 统一分页响应结构（对齐 API.md §1 与审计端点）。
#[derive(Debug, Serialize)]
pub struct Paginated {
    /// 本页命中条目（最新在前）。
    pub items: Vec<LogEntry>,
    /// 满足过滤的总数。
    pub total: i64,
    /// 本页起点。
    pub offset: i64,
    /// 本页容量。
    pub limit: i64,
    /// 是否还有更多。
    pub has_more: bool,
}

/// 列出系统运行日志（仅 Admin）：tail 最近若干行，支持级别过滤与分页。
pub async fn list_system_logs(
    State(state): State<AppState>,
    identity: Identity,
    Query(query): Query<SystemLogListQuery>,
) -> Result<Json<Paginated>, ApiError> {
    identity.require_admin()?;
    let offset = query.offset.unwrap_or(0).max(0);
    let limit = query.limit.unwrap_or(DEFAULT_LIMIT).clamp(1, MAX_LIMIT);
    // 无法识别的级别串视为不过滤（宽松对待客户端输入）
    let level = query.level.as_deref().and_then(logs::parse_level);

    // 读运行日志文件全部行（文件缺失 / 空 → 空集合，不报错）
    let path = logs::log_file_path(&state.config.data.data_dir);
    let lines = logs::read_log_lines(&path);

    let (items, total) = logs::tail_filter(&lines, level, offset as usize, limit as usize);
    let total = total as i64;
    let has_more = offset + (items.len() as i64) < total;

    Ok(Json(Paginated {
        items,
        total,
        offset,
        limit,
        has_more,
    }))
}

#[cfg(test)]
mod tests {
    use super::super::tests::{测试用状态, 读_json};
    use super::super::AppState;
    use crate::auth::hash_password;
    use crate::meta::Role;
    use axum::body::Body;
    use axum::http::{Request, StatusCode};
    use std::sync::Arc;
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

    /// 把状态的配置数据目录指向给定路径（便于在已知目录放置 app.log）。
    fn 指定数据目录(state: &mut AppState, dir: &std::path::Path) {
        let mut cfg = (*state.config).clone();
        cfg.data.data_dir = dir.to_path_buf();
        state.config = Arc::new(cfg);
    }

    /// 在 `{data_dir}/logs/app.log` 写入给定内容。
    fn 写日志文件(data_dir: &std::path::Path, content: &str) {
        let logs_dir = crate::logs::logs_dir(data_dir);
        std::fs::create_dir_all(&logs_dir).unwrap();
        std::fs::write(crate::logs::log_file_path(data_dir), content).unwrap();
    }

    /// 便捷：带可选 Bearer 令牌、可选查询串请求系统日志端点。
    async fn 请求日志(
        state: AppState,
        令牌: Option<&str>,
        查询: &str,
    ) -> axum::response::Response {
        let app = super::super::build_router(state);
        let uri = if 查询.is_empty() {
            "/api/v1/system-logs".to_string()
        } else {
            format!("/api/v1/system-logs?{查询}")
        };
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
        let resp = 请求日志(state, None, "").await;
        assert_eq!(resp.status(), StatusCode::UNAUTHORIZED);
    }

    #[tokio::test]
    async fn 普通用户访问被拒_403() {
        let (state, _dir) = 测试用状态().await;
        let token = 签发令牌(&state, "u", Role::User).await;
        let resp = 请求日志(state, Some(&token), "").await;
        assert_eq!(resp.status(), StatusCode::FORBIDDEN);
    }

    #[tokio::test]
    async fn 管理员_文件不存在返回空列表_200() {
        let (mut state, dir) = 测试用状态().await;
        指定数据目录(&mut state, dir.path()); // 该目录下尚无 logs/app.log
        let token = 签发令牌(&state, "admin", Role::Admin).await;
        let resp = 请求日志(state, Some(&token), "").await;
        assert_eq!(resp.status(), StatusCode::OK);
        let body = 读_json(resp).await;
        assert_eq!(body["total"].as_i64().unwrap(), 0);
        assert!(body["items"].as_array().unwrap().is_empty());
        assert!(!body["has_more"].as_bool().unwrap());
    }

    #[tokio::test]
    async fn 管理员_读取并最新在前() {
        let (mut state, dir) = 测试用状态().await;
        指定数据目录(&mut state, dir.path());
        写日志文件(
            dir.path(),
            "2026-06-27T08:00:01.000000Z  INFO m: 一\n\
             2026-06-27T08:00:02.000000Z ERROR m: 二\n\
             2026-06-27T08:00:03.000000Z  WARN m: 三\n",
        );
        let token = 签发令牌(&state, "admin", Role::Admin).await;
        let resp = 请求日志(state, Some(&token), "").await;
        assert_eq!(resp.status(), StatusCode::OK);
        let body = 读_json(resp).await;
        assert_eq!(body["total"].as_i64().unwrap(), 3);
        let items = body["items"].as_array().unwrap();
        assert_eq!(items.len(), 3);
        // 最新（三）在前
        assert_eq!(items[0]["message"].as_str().unwrap(), "m: 三");
        assert_eq!(items[0]["level"].as_str().unwrap(), "WARN");
        assert_eq!(items[2]["message"].as_str().unwrap(), "m: 一");
    }

    #[tokio::test]
    async fn 管理员_按级别过滤() {
        let (mut state, dir) = 测试用状态().await;
        指定数据目录(&mut state, dir.path());
        写日志文件(
            dir.path(),
            "2026-06-27T08:00:01.000000Z  INFO m: 一\n\
             2026-06-27T08:00:02.000000Z ERROR m: 二\n\
             2026-06-27T08:00:03.000000Z  INFO m: 三\n\
             2026-06-27T08:00:04.000000Z ERROR m: 四\n",
        );
        let token = 签发令牌(&state, "admin", Role::Admin).await;
        // 小写 level 也应被识别
        let resp = 请求日志(state, Some(&token), "level=error").await;
        assert_eq!(resp.status(), StatusCode::OK);
        let body = 读_json(resp).await;
        assert_eq!(body["total"].as_i64().unwrap(), 2, "仅两条 ERROR");
        let items = body["items"].as_array().unwrap();
        assert_eq!(items.len(), 2);
        assert!(items.iter().all(|e| e["level"].as_str() == Some("ERROR")));
    }

    #[tokio::test]
    async fn 管理员_分页与has_more() {
        let (mut state, dir) = 测试用状态().await;
        指定数据目录(&mut state, dir.path());
        // 写 5 行
        let content: String = (1..=5)
            .map(|i| format!("2026-06-27T08:00:0{i}.000000Z  INFO m: {i}\n"))
            .collect();
        写日志文件(dir.path(), &content);
        let token = 签发令牌(&state, "admin", Role::Admin).await;
        // 取最新 2 条：应 has_more=true，最新（5）在前
        let resp = 请求日志(state, Some(&token), "offset=0&limit=2").await;
        let body = 读_json(resp).await;
        assert_eq!(body["total"].as_i64().unwrap(), 5);
        let items = body["items"].as_array().unwrap();
        assert_eq!(items.len(), 2);
        assert_eq!(items[0]["message"].as_str().unwrap(), "m: 5");
        assert!(body["has_more"].as_bool().unwrap());
    }
}
