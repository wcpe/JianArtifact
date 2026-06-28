//! 审计日志（FR-31，ADR-0015）：采集安全 / 管理类事件，异步批量落 SQLite。
//!
//! 设计（严格照 ADR-0015 的"审计日志"部分）：
//! - **异步不阻塞**：事件经进程内有界 channel 投递给单独写入任务批量入库；主请求路径只做
//!   一次非阻塞 `try_send`，**采集 / 写入失败只记 WARN、不影响业务**；channel 满时按
//!   "丢弃并计数 + WARN"降级，绝不反压主路径（testing-and-quality §2.8）。
//! - **全量非读**（FR-97，扩展 FR-31）：审计中间件在鉴权判定之后捕获 actor / result，
//!   **所有变更类请求（POST/PUT/PATCH/DELETE）一律产一条审计**——已知端点归精确语义 action
//!   （`repo.create` / `artifact.upload` 等），未显式归类的非读路径走兜底 `change.{method}`、不漏记；
//!   **读取类（GET/HEAD，含下载 / 浏览 / 搜索 / 详情）一律不入审计**（交使用分析计数，避免刷屏与性能损耗）。
//!   登录事件因需记录"被尝试的用户名"，由登录 handler 显式发事件（中间件跳过 `/auth/login`）。
//! - **脱敏**：actor 只记用户名；口令 / Token / JWT / 上游凭据一律不入审计。
//! - **保留期轮转**：后台任务按 `observability.audit.retention_days` 删旧 + `max_rows` 兜底。
//! - **管理查询**：仅 Admin 可查，分页复用统一 offset/limit 结构。

use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;
use std::time::Duration;

use axum::{
    extract::{Query, Request, State},
    http::{header::AUTHORIZATION, Method, StatusCode},
    middleware::Next,
    response::Response,
    Json,
};
use serde::{Deserialize, Serialize};
use tokio::sync::mpsc;

use crate::auth::{AuthIdentity, TOKEN_PREFIX};
use crate::meta::{AuditEntry, AuditQuery, MetaStore, NewAuditEntry};

use super::{ApiError, AppState, Identity};

/// 审计事件 channel 容量（有界）：满则丢弃 + 计数，绝不反压主路径。
const AUDIT_CHANNEL_CAPACITY: usize = 4096;
/// 写入任务单批最大条数：达到即落库，平衡时延与批处理收益。
const AUDIT_BATCH_MAX: usize = 64;
/// 写入任务批间最长等待：不足一批时也会在该间隔内落库，避免事件长时间滞留。
const AUDIT_FLUSH_INTERVAL: Duration = Duration::from_millis(500);
/// 保留期轮转的扫描周期。
const AUDIT_RETENTION_INTERVAL: Duration = Duration::from_secs(3600);
/// 请求 ID 头名称（与 `api::mod` 的设置保持一致）。
const REQUEST_ID_HEADER: &str = "x-request-id";

/// 主体身份种类。以小写字符串入库，避免魔法字符串散落。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ActorKind {
    /// Web 会话（JWT）。
    Session,
    /// API Token。
    Token,
    /// Basic Auth。
    Basic,
    /// 匿名。
    Anonymous,
}

impl ActorKind {
    /// 入库字符串。
    fn as_str(self) -> &'static str {
        match self {
            ActorKind::Session => "session",
            ActorKind::Token => "token",
            ActorKind::Basic => "basic",
            ActorKind::Anonymous => "anonymous",
        }
    }
}

/// 审计结果。以小写字符串入库。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum AuditResult {
    /// 成功。
    Success,
    /// 被拒（认证 / 鉴权失败、限流等）。
    Denied,
    /// 服务端错误。
    Error,
}

impl AuditResult {
    /// 入库字符串。
    fn as_str(self) -> &'static str {
        match self {
            AuditResult::Success => "success",
            AuditResult::Denied => "denied",
            AuditResult::Error => "error",
        }
    }

    /// 由 HTTP 状态码归类结果：2xx 成功；401/403/404/429 视为被拒；5xx 错误；其余按成功计。
    fn from_status(status: StatusCode) -> Self {
        if status.is_server_error() {
            AuditResult::Error
        } else if matches!(
            status,
            StatusCode::UNAUTHORIZED
                | StatusCode::FORBIDDEN
                | StatusCode::NOT_FOUND
                | StatusCode::TOO_MANY_REQUESTS
        ) {
            AuditResult::Denied
        } else {
            AuditResult::Success
        }
    }
}

/// 审计事件投递端：克隆廉价（内含 channel sender 与丢弃计数 Arc），随 AppState 共享。
///
/// 主路径只调用 `enqueue` 做一次非阻塞投递；写入与轮转在独立后台任务进行。
#[derive(Clone)]
pub struct AuditSink {
    sender: mpsc::Sender<NewAuditEntry>,
    /// channel 满而被丢弃的事件累计数（供观测 / 后续指标埋点）。
    dropped: Arc<AtomicU64>,
}

impl AuditSink {
    /// 非阻塞投递一条审计事件。channel 满时丢弃并计数 + WARN，绝不阻塞主路径。
    pub fn enqueue(&self, entry: NewAuditEntry) {
        match self.sender.try_send(entry) {
            Ok(()) => {}
            Err(mpsc::error::TrySendError::Full(dropped)) => {
                let total = self.dropped.fetch_add(1, Ordering::Relaxed) + 1;
                tracing::warn!(
                    动作 = %dropped.action,
                    累计丢弃 = total,
                    "审计事件队列已满，丢弃本条事件（采集降级，不影响业务）"
                );
            }
            Err(mpsc::error::TrySendError::Closed(_)) => {
                // 写入任务已退出（仅发生在停机阶段），按降级处理不报错
                tracing::warn!("审计写入任务已关闭，丢弃事件");
            }
        }
    }

    /// 已丢弃事件累计数（供测试与后续指标读取）。
    pub fn dropped_count(&self) -> u64 {
        self.dropped.load(Ordering::Relaxed)
    }

    /// 记录一次登录事件（由登录 handler 调用）。
    ///
    /// 登录在认证之前发生，actor 取被尝试的用户名（脱敏：绝不记口令）；身份种类固定 session
    /// （登录用于建立 Web 会话）。`result` 由 handler 按成功 / 被拒传入。
    pub fn record_login(
        &self,
        username: &str,
        result: AuditResult,
        source_ip: Option<&str>,
        request_id: Option<&str>,
    ) {
        self.enqueue(NewAuditEntry {
            actor: username.to_string(),
            actor_kind: ActorKind::Session.as_str().to_string(),
            request_id: request_id.map(str::to_owned),
            source_ip: source_ip.map(str::to_owned),
            action: "login".to_string(),
            target_repo: None,
            target: None,
            result: result.as_str().to_string(),
            detail: None,
        });
    }
}

/// 创建审计投递端与配套接收端。接收端交由 `spawn_audit_writer` 消费。
pub fn channel() -> (AuditSink, mpsc::Receiver<NewAuditEntry>) {
    let (sender, receiver) = mpsc::channel(AUDIT_CHANNEL_CAPACITY);
    let sink = AuditSink {
        sender,
        dropped: Arc::new(AtomicU64::new(0)),
    };
    (sink, receiver)
}

/// 启动审计写入后台任务：从 channel 聚批写入 SQLite。
///
/// 落库失败只记 WARN、丢弃该批，不让采集失败影响业务（ADR-0015）。
/// 所有 sender 释放后 channel 关闭，任务收尾退出。
pub fn spawn_audit_writer(meta: MetaStore, mut receiver: mpsc::Receiver<NewAuditEntry>) {
    tokio::spawn(async move {
        let mut batch: Vec<NewAuditEntry> = Vec::with_capacity(AUDIT_BATCH_MAX);
        loop {
            // 先阻塞等第一条；channel 关闭则把残余落库后退出
            let first = match receiver.recv().await {
                Some(e) => e,
                None => {
                    flush_batch(&meta, &mut batch).await;
                    break;
                }
            };
            batch.push(first);

            // 在 flush 间隔内尽量多收几条凑批，超时或满批即落库
            let _ = tokio::time::timeout(AUDIT_FLUSH_INTERVAL, async {
                while batch.len() < AUDIT_BATCH_MAX {
                    match receiver.recv().await {
                        Some(e) => batch.push(e),
                        None => break,
                    }
                }
            })
            .await;

            flush_batch(&meta, &mut batch).await;
        }
    });
}

/// 落库一批审计事件；失败只记 WARN 并清空该批（采集失败不影响业务）。
async fn flush_batch(meta: &MetaStore, batch: &mut Vec<NewAuditEntry>) {
    if batch.is_empty() {
        return;
    }
    if let Err(e) = meta.insert_audit_batch(batch).await {
        tracing::warn!(错误 = %e, 条数 = batch.len(), "审计批量写入失败，丢弃本批（不影响业务）");
    }
    batch.clear();
}

/// 启动审计保留期轮转后台任务：周期性按天数删旧 + 行数兜底。
pub fn spawn_audit_retention(meta: MetaStore, retention_days: u32, max_rows: u64) {
    tokio::spawn(async move {
        let mut ticker = tokio::time::interval(AUDIT_RETENTION_INTERVAL);
        loop {
            ticker.tick().await;
            match meta.prune_audit_by_age(retention_days).await {
                Ok(n) if n > 0 => {
                    tracing::info!(
                        删除行数 = n,
                        保留天数 = retention_days,
                        "审计日志按保留期轮转完成"
                    )
                }
                Ok(_) => {}
                Err(e) => tracing::warn!(错误 = %e, "审计保留期轮转失败"),
            }
            match meta.prune_audit_by_max_rows(max_rows).await {
                Ok(n) if n > 0 => {
                    tracing::warn!(
                        删除行数 = n,
                        行数上限 = max_rows,
                        "审计日志超行数上限，已删最旧行"
                    )
                }
                Ok(_) => {}
                Err(e) => tracing::warn!(错误 = %e, "审计行数兜底轮转失败"),
            }
        }
    });
}

/// 审计中间件：置于身份解析中间件之后，运行 handler 后按"方法 + 路径 + 状态"归类事件，
/// 命中任一变更类请求（非读方法，FR-97 全量非读）即非阻塞投递；读取类（GET/HEAD）不投递。
///
/// 登录事件由 `/auth/login` handler 自行发（需记被尝试用户名），本中间件跳过该路径避免重复。
pub async fn audit_layer(State(state): State<AppState>, request: Request, next: Next) -> Response {
    // 在 handler 消费请求前先取出归类所需的只读信息
    let method = request.method().clone();
    let path = request.uri().path().to_string();
    let request_id = header_value(&request, REQUEST_ID_HEADER);
    let source_ip = request
        .extensions()
        .get::<axum::extract::ConnectInfo<std::net::SocketAddr>>()
        .map(|ci| ci.0.ip().to_string());
    let actor_kind = classify_actor_kind(&request);
    let (actor, _) = actor_from_extensions(&request);

    let response = next.run(request).await;

    // 登录由 handler 发事件，避免重复
    if let Some(event) = classify_event(&method, &path) {
        let result = AuditResult::from_status(response.status());
        state.audit.enqueue(NewAuditEntry {
            actor,
            actor_kind: actor_kind.as_str().to_string(),
            request_id,
            source_ip,
            action: event.action.to_string(),
            target_repo: event.target_repo,
            target: event.target,
            result: result.as_str().to_string(),
            detail: None,
        });
    }

    response
}

/// 从请求扩展取出已解析身份的 actor（用户名或 anonymous）及其是否已认证。
fn actor_from_extensions(request: &Request) -> (String, bool) {
    match request.extensions().get::<AuthIdentity>() {
        Some(AuthIdentity::Authenticated(u)) => (u.username.clone(), true),
        _ => ("anonymous".to_string(), false),
    }
}

/// 取某请求头的字符串值（缺失 / 非法返回 None）。
fn header_value(request: &Request, name: &str) -> Option<String> {
    request
        .headers()
        .get(name)
        .and_then(|v| v.to_str().ok())
        .map(str::to_owned)
}

/// 由 `Authorization` 头的凭据形态归类 actor_kind（只看形态、不重做凭据校验）。
///
/// Basic → basic；Bearer 且值带 `jna_` 前缀 → token，否则按会话 JWT → session；
/// 无 scheme 的裸 `jna_` Token → token；无凭据 → anonymous。
fn classify_actor_kind(request: &Request) -> ActorKind {
    let header = match header_value(request, AUTHORIZATION.as_str()) {
        Some(h) => h,
        None => return ActorKind::Anonymous,
    };
    let header = header.trim();
    if let Some(rest) = strip_ci_prefix(header, "Basic ") {
        let _ = rest;
        return ActorKind::Basic;
    }
    if let Some(rest) = strip_ci_prefix(header, "Bearer ") {
        return if rest.trim().starts_with(TOKEN_PREFIX) {
            ActorKind::Token
        } else {
            ActorKind::Session
        };
    }
    if header.starts_with(TOKEN_PREFIX) {
        return ActorKind::Token;
    }
    ActorKind::Anonymous
}

/// 大小写不敏感地剥离 scheme 前缀（与 auth::basic 的语义一致，避免跨模块依赖其私有项）。
fn strip_ci_prefix<'a>(value: &'a str, prefix: &str) -> Option<&'a str> {
    if value.len() >= prefix.len() && value[..prefix.len()].eq_ignore_ascii_case(prefix) {
        Some(&value[prefix.len()..])
    } else {
        None
    }
}

/// 归类后的审计事件骨架（actor / result / 时间等由调用方补全）。
struct ClassifiedEvent {
    /// 事件动作枚举字符串。
    action: &'static str,
    /// 受影响仓库名（可空）。
    target_repo: Option<String>,
    /// 受影响对象坐标 / 路径（可空）。
    target: Option<String>,
}

/// 把"方法 + 路径"归类为审计事件；读取类（GET/HEAD）一律返回 None（FR-97 全量非读）。
///
/// 覆盖矩阵：**非读方法（POST/PUT/PATCH/DELETE）一律产事件**——已知管理 / 格式 / Docker
/// 路径归为精确语义 action（`repo.create` / `artifact.upload` 等），未显式归类的非读路径走
/// 兜底 `change.{method}`，保证无遗漏。读流量交使用分析计数，不入审计。
/// 管理 API 路径形如 `/api/v1/...`；格式 API 为 `/{repo}/{path..}`；Docker 为 `/v2/...`。
fn classify_event(method: &Method, path: &str) -> Option<ClassifiedEvent> {
    // 读取方法不入审计（全量非读的"非读"门）：下载 / 浏览 / 搜索 / 详情均为 GET / HEAD
    if !is_change_method(method) {
        return None;
    }
    // 登录由 handler 发事件（需记被尝试用户名），中间件跳过避免重复
    if path == "/api/v1/auth/login" {
        return None;
    }

    if let Some(rest) = path.strip_prefix("/api/v1/") {
        return Some(classify_management(method, rest));
    }
    if let Some(rest) = path.strip_prefix("/v2/") {
        return Some(classify_docker(method, rest));
    }
    // 其余视作格式 API（/{repo}/{path..}）
    Some(classify_format(method, path))
}

/// 是否为变更类方法（非读）：POST / PUT / PATCH / DELETE 产审计，GET / HEAD / OPTIONS 不产。
fn is_change_method(method: &Method) -> bool {
    matches!(
        *method,
        Method::POST | Method::PUT | Method::PATCH | Method::DELETE
    )
}

/// 变更类方法的兜底 action：`change.{小写方法名}`，给未显式归类的非读路径留痕、不漏记。
fn fallback_action(method: &Method) -> &'static str {
    match *method {
        Method::POST => "change.post",
        Method::PUT => "change.put",
        Method::PATCH => "change.patch",
        Method::DELETE => "change.delete",
        // is_change_method 已挡下其余方法，此分支不可达；保守兜底
        _ => "change.other",
    }
}

/// 归类管理 API 事件（路径已去掉 `/api/v1/` 前缀）。已知端点归精确 action，其余非读路径走兜底。
fn classify_management(method: &Method, rest: &str) -> ClassifiedEvent {
    let segs: Vec<&str> = rest.split('/').filter(|s| !s.is_empty()).collect();
    match segs.as_slice() {
        // 认证：登出 / 刷新（登录在入口已跳过）
        ["auth", "logout"] if method == Method::POST => simple("auth.logout"),
        ["auth", "refresh"] if method == Method::POST => simple("auth.refresh"),
        // 用户管理
        ["users"] if method == Method::POST => simple("user.create"),
        ["users", id] if method == Method::PATCH => target("user.update", id),
        ["users", id] if method == Method::DELETE => target("user.delete", id),
        // Token 管理
        ["tokens"] if method == Method::POST => simple("token.issue"),
        ["tokens", id] if method == Method::DELETE => target("token.revoke", id),
        // 仓库管理
        ["repositories"] if method == Method::POST => simple("repo.create"),
        ["repositories", id] if method == Method::PATCH => repo_target("repo.update", id),
        ["repositories", id] if method == Method::DELETE => repo_target("repo.delete", id),
        // 制品上传专用端点（POST /repositories/{id}/upload）
        ["repositories", id, "upload"] if method == Method::POST => {
            repo_target("artifact.upload", id)
        }
        // 制品详情删除（DELETE /repositories/{id}/artifacts/{path..}）
        ["repositories", id, "artifacts", ..] if method == Method::DELETE => {
            let target = rest
                .split_once("/artifacts/")
                .map(|(_, p)| p.to_string())
                .filter(|p| !p.is_empty());
            ClassifiedEvent {
                action: "artifact.delete",
                target_repo: Some((*id).to_string()),
                target,
            }
        }
        // ACL 管理（新增 / 删除均归为 acl.update）
        ["repositories", id, "acl"] if method == Method::POST => repo_target("acl.update", id),
        ["repositories", id, "acl", _aid] if method == Method::DELETE => {
            repo_target("acl.update", id)
        }
        // 组 ACL 管理（新增 / 删除均归为 group.acl.update）
        ["repositories", id, "group-acl"] if method == Method::POST => {
            repo_target("group.acl.update", id)
        }
        ["repositories", id, "group-acl", _aid] if method == Method::DELETE => {
            repo_target("group.acl.update", id)
        }
        // 用户组管理
        ["groups"] if method == Method::POST => simple("group.create"),
        ["groups", id] if method == Method::DELETE => target("group.delete", id),
        // 组成员增删（POST / DELETE 均归为 group.member.update）
        ["groups", id, "members"] if method == Method::POST => target("group.member.update", id),
        ["groups", id, "members", _uid] if method == Method::DELETE => {
            target("group.member.update", id)
        }
        // 设置 / 防护配置热替换
        ["settings"] if method == Method::PATCH => simple("settings.update"),
        ["protection", "config"] if method == Method::PATCH => simple("protection.config.update"),
        // 迁移任务控制（cancel / pause / resume）：target 记 "id/动作"
        ["migrate", "jobs", id, action] if method == Method::POST => ClassifiedEvent {
            action: "migrate.job.control",
            target_repo: None,
            target: Some(format!("{id}/{action}")),
        },
        // 迁移预览 / 搬运（preview / migrate）：target 记末段语义
        ["migrate", "nexus", ..] if method == Method::POST => {
            let tail = segs.last().copied().unwrap_or_default();
            ClassifiedEvent {
                action: "migrate.run",
                target_repo: None,
                target: Some(tail.to_string()),
            }
        }
        // 在线更新 apply / rollback
        ["update", "apply"] if method == Method::POST => simple("update.apply"),
        ["update", "rollback"] if method == Method::POST => simple("update.rollback"),
        // 系统重启 / 关闭（FR-109，ADR-0033）
        ["system", "restart"] if method == Method::POST => simple("system.restart"),
        ["system", "shutdown"] if method == Method::POST => simple("system.shutdown"),
        // 其余未显式归类的非读路径：按方法兜底留痕，不漏记
        _ => simple(fallback_action(method)),
    }
}

/// 归类 Docker Registry v2 事件（路径已去掉 `/v2/` 前缀；调用方已确保为非读方法）。
///
/// 写制品：PUT（manifest / blob 提交）；删制品：DELETE。blob 分块过程（POST/PATCH）走兜底留痕。
fn classify_docker(method: &Method, rest: &str) -> ClassifiedEvent {
    let (repo, target) = docker_repo_and_target(rest);
    match *method {
        Method::PUT => ClassifiedEvent {
            action: "artifact.upload",
            target_repo: repo,
            target,
        },
        Method::DELETE => ClassifiedEvent {
            action: "artifact.delete",
            target_repo: repo,
            target,
        },
        // blob 上传初始化 / 分块（POST / PATCH）：仍属变更类，按方法兜底留痕
        _ => ClassifiedEvent {
            action: fallback_action(method),
            target_repo: repo,
            target,
        },
    }
}

/// 从 Docker `/v2/` 之后的路径粗解析仓库名与对象（name 段直到 manifests/blobs 之前）。
fn docker_repo_and_target(rest: &str) -> (Option<String>, Option<String>) {
    if let Some(idx) = rest.find("/manifests/") {
        let name = &rest[..idx];
        let reference = &rest[idx + "/manifests/".len()..];
        return (
            Some(name.to_string()),
            Some(format!("manifests/{reference}")),
        );
    }
    if let Some(idx) = rest.find("/blobs/") {
        let name = &rest[..idx];
        let digest = &rest[idx + "/blobs/".len()..];
        return (Some(name.to_string()), Some(format!("blobs/{digest}")));
    }
    (None, Some(rest.to_string()))
}

/// 归类格式 API 事件：路径形如 `/{repo}/{path..}`（调用方已确保为非读方法）。
///
/// 写（PUT/POST）→ `artifact.upload`，删（DELETE）→ `artifact.delete`；仓库名缺失则按方法兜底。
fn classify_format(method: &Method, path: &str) -> ClassifiedEvent {
    let trimmed = path.trim_start_matches('/');
    let mut parts = trimmed.splitn(2, '/');
    let repo = match parts.next().filter(|s| !s.is_empty()) {
        Some(r) => r.to_string(),
        None => return simple(fallback_action(method)),
    };
    let target = parts.next().filter(|s| !s.is_empty()).map(str::to_owned);
    match *method {
        Method::PUT | Method::POST => ClassifiedEvent {
            action: "artifact.upload",
            target_repo: Some(repo),
            target,
        },
        Method::DELETE => ClassifiedEvent {
            action: "artifact.delete",
            target_repo: Some(repo),
            target,
        },
        // PATCH 等其余非读方法对格式 API 罕见，按方法兜底
        _ => ClassifiedEvent {
            action: fallback_action(method),
            target_repo: Some(repo),
            target,
        },
    }
}

/// 构造无目标的事件骨架。
fn simple(action: &'static str) -> ClassifiedEvent {
    ClassifiedEvent {
        action,
        target_repo: None,
        target: None,
    }
}

/// 构造带 target（如用户 / Token id）的事件骨架。
fn target(action: &'static str, target: &str) -> ClassifiedEvent {
    ClassifiedEvent {
        action,
        target_repo: None,
        target: Some(target.to_string()),
    }
}

/// 构造带 target_repo（仓库 id）的事件骨架。
fn repo_target(action: &'static str, repo: &str) -> ClassifiedEvent {
    ClassifiedEvent {
        action,
        target_repo: Some(repo.to_string()),
        target: None,
    }
}

// ============ 管理查询端点 ============

/// 默认分页容量。
const DEFAULT_LIMIT: i64 = 50;
/// 分页容量上限（对齐 API.md）。
const MAX_LIMIT: i64 = 1000;

/// 审计查询参数。
#[derive(Debug, Deserialize)]
pub struct AuditListQuery {
    /// 按动作过滤（可选）。
    #[serde(default)]
    pub action: Option<String>,
    /// 按仓库名过滤（可选）。
    #[serde(default)]
    pub target_repo: Option<String>,
    /// 按主体（用户名）过滤（可选）。
    #[serde(default)]
    pub actor: Option<String>,
    /// 分页起点（默认 0）。
    #[serde(default)]
    pub offset: Option<i64>,
    /// 分页容量（默认 50，上限 1000）。
    #[serde(default)]
    pub limit: Option<i64>,
}

/// 单条审计视图（对齐 audit_log 字段）。
#[derive(Debug, Serialize)]
pub struct AuditEntryDto {
    /// 自增主键。
    pub id: i64,
    /// 事件时间。
    pub ts: String,
    /// 行为主体（用户名或 anonymous）。
    pub actor: String,
    /// 主体身份种类。
    pub actor_kind: String,
    /// 关联请求 ID。
    pub request_id: Option<String>,
    /// 来源 IP。
    pub source_ip: Option<String>,
    /// 事件动作。
    pub action: String,
    /// 受影响仓库名。
    pub target_repo: Option<String>,
    /// 受影响对象坐标 / 路径。
    pub target: Option<String>,
    /// 结果。
    pub result: String,
    /// 结构化补充。
    pub detail: Option<String>,
}

impl From<AuditEntry> for AuditEntryDto {
    fn from(e: AuditEntry) -> Self {
        Self {
            id: e.id,
            ts: e.ts,
            actor: e.actor,
            actor_kind: e.actor_kind,
            request_id: e.request_id,
            source_ip: e.source_ip,
            action: e.action,
            target_repo: e.target_repo,
            target: e.target,
            result: e.result,
            detail: e.detail,
        }
    }
}

/// 统一分页响应结构（对齐 API.md §1）。
#[derive(Debug, Serialize)]
pub struct Paginated {
    /// 本页命中项。
    pub items: Vec<AuditEntryDto>,
    /// 满足过滤的总数。
    pub total: i64,
    /// 本页起点。
    pub offset: i64,
    /// 本页容量。
    pub limit: i64,
    /// 是否还有更多。
    pub has_more: bool,
}

/// 列出审计日志（仅 Admin）：按时间倒序，支持动作 / 仓库 / 主体过滤与分页。
pub async fn list_audit(
    State(state): State<AppState>,
    identity: Identity,
    Query(query): Query<AuditListQuery>,
) -> Result<Json<Paginated>, ApiError> {
    identity.require_admin()?;
    let offset = query.offset.unwrap_or(0).max(0);
    let limit = query.limit.unwrap_or(DEFAULT_LIMIT).clamp(1, MAX_LIMIT);

    let filter = AuditQuery {
        action: query.action.as_deref(),
        target_repo: query.target_repo.as_deref(),
        actor: query.actor.as_deref(),
        offset,
        limit,
    };
    let total = state.meta.count_audit(&filter).await?;
    let rows = state.meta.query_audit(&filter).await?;
    let items: Vec<AuditEntryDto> = rows.into_iter().map(AuditEntryDto::from).collect();
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
    use super::*;

    #[test]
    fn 状态码归类结果() {
        assert_eq!(
            AuditResult::from_status(StatusCode::OK),
            AuditResult::Success
        );
        assert_eq!(
            AuditResult::from_status(StatusCode::CREATED),
            AuditResult::Success
        );
        assert_eq!(
            AuditResult::from_status(StatusCode::UNAUTHORIZED),
            AuditResult::Denied
        );
        assert_eq!(
            AuditResult::from_status(StatusCode::FORBIDDEN),
            AuditResult::Denied
        );
        assert_eq!(
            AuditResult::from_status(StatusCode::NOT_FOUND),
            AuditResult::Denied
        );
        assert_eq!(
            AuditResult::from_status(StatusCode::TOO_MANY_REQUESTS),
            AuditResult::Denied
        );
        assert_eq!(
            AuditResult::from_status(StatusCode::INTERNAL_SERVER_ERROR),
            AuditResult::Error
        );
    }

    #[test]
    fn 管理事件归类覆盖各动作() {
        let c = |m: Method, p: &str| classify_event(&m, p);
        // 用户管理
        assert_eq!(
            c(Method::POST, "/api/v1/users").unwrap().action,
            "user.create"
        );
        assert_eq!(
            c(Method::PATCH, "/api/v1/users/u1").unwrap().action,
            "user.update"
        );
        assert_eq!(
            c(Method::DELETE, "/api/v1/users/u1").unwrap().action,
            "user.delete"
        );
        // Token 管理
        assert_eq!(
            c(Method::POST, "/api/v1/tokens").unwrap().action,
            "token.issue"
        );
        assert_eq!(
            c(Method::DELETE, "/api/v1/tokens/t1").unwrap().action,
            "token.revoke"
        );
        // 仓库管理（带 target_repo）
        let repo = c(Method::DELETE, "/api/v1/repositories/r1").unwrap();
        assert_eq!(repo.action, "repo.delete");
        assert_eq!(repo.target_repo.as_deref(), Some("r1"));
        // ACL 增删均归 acl.update
        assert_eq!(
            c(Method::POST, "/api/v1/repositories/r1/acl")
                .unwrap()
                .action,
            "acl.update"
        );
        assert_eq!(
            c(Method::DELETE, "/api/v1/repositories/r1/acl/a1")
                .unwrap()
                .action,
            "acl.update"
        );
    }

    #[test]
    fn 普通读与登录不入审计() {
        // 列表 / 详情读取不审计
        assert!(classify_event(&Method::GET, "/api/v1/users").is_none());
        assert!(classify_event(&Method::GET, "/api/v1/repositories/r1").is_none());
        assert!(classify_event(&Method::GET, "/api/v1/search").is_none());
        // 登录由 handler 发事件，中间件跳过
        assert!(classify_event(&Method::POST, "/api/v1/auth/login").is_none());
        // 健康检查不审计
        assert!(classify_event(&Method::GET, "/health").is_none());
    }

    #[test]
    fn 格式_api_只审计写与删() {
        // Raw / Maven 等格式上传
        let up = classify_event(&Method::PUT, "/raw-repo/a/b/c.txt").unwrap();
        assert_eq!(up.action, "artifact.upload");
        assert_eq!(up.target_repo.as_deref(), Some("raw-repo"));
        assert_eq!(up.target.as_deref(), Some("a/b/c.txt"));
        // 删除
        let del = classify_event(&Method::DELETE, "/raw-repo/a/b/c.txt").unwrap();
        assert_eq!(del.action, "artifact.delete");
        // 格式 API 的 GET 不审计（普通下载交指标计数）
        assert!(classify_event(&Method::GET, "/raw-repo/a/b/c.txt").is_none());
    }

    #[test]
    fn docker_写删归类带仓库与对象() {
        let manifest = classify_event(&Method::PUT, "/v2/library/app/manifests/latest").unwrap();
        assert_eq!(manifest.action, "artifact.upload");
        assert_eq!(manifest.target_repo.as_deref(), Some("library/app"));
        assert_eq!(manifest.target.as_deref(), Some("manifests/latest"));
        // 版本检查与 token 端点为 GET，自然被方法门挡下
        assert!(classify_event(&Method::GET, "/v2/").is_none());
        assert!(classify_event(&Method::GET, "/v2/token").is_none());
        // blob 上传的 POST/PATCH 过程按"全量非读"走兜底留痕（详见兜底用例）
        assert_eq!(
            classify_event(&Method::POST, "/v2/library/app/blobs/uploads/")
                .unwrap()
                .action,
            "change.post"
        );
    }

    #[test]
    fn 读取方法一律不入审计() {
        // GET / HEAD 任意路径都不产事件（全量非读的"非读"门）
        for p in [
            "/api/v1/users",
            "/api/v1/repositories/r1",
            "/api/v1/settings",
            "/api/v1/protection/config",
            "/api/v1/migrate/jobs/j1",
            "/raw-repo/a/b.txt",
            "/v2/library/app/manifests/latest",
        ] {
            assert!(
                classify_event(&Method::GET, p).is_none(),
                "GET {p} 不应入审计"
            );
            assert!(
                classify_event(&Method::HEAD, p).is_none(),
                "HEAD {p} 不应入审计"
            );
        }
    }

    #[test]
    fn 新覆盖变更端点归类() {
        let c = |m: Method, p: &str| classify_event(&m, p);
        // 设置 / 防护配置 PATCH
        assert_eq!(
            c(Method::PATCH, "/api/v1/settings").unwrap().action,
            "settings.update"
        );
        assert_eq!(
            c(Method::PATCH, "/api/v1/protection/config")
                .unwrap()
                .action,
            "protection.config.update"
        );
        // 迁移任务控制
        let ctl = c(Method::POST, "/api/v1/migrate/jobs/j1/cancel").unwrap();
        assert_eq!(ctl.action, "migrate.job.control");
        assert_eq!(ctl.target.as_deref(), Some("j1/cancel"));
        assert_eq!(
            c(Method::POST, "/api/v1/migrate/jobs/j1/pause")
                .unwrap()
                .action,
            "migrate.job.control"
        );
        // 迁移预览 / 搬运
        assert_eq!(
            c(Method::POST, "/api/v1/migrate/nexus/online/migrate")
                .unwrap()
                .action,
            "migrate.run"
        );
        assert_eq!(
            c(Method::POST, "/api/v1/migrate/nexus/preview")
                .unwrap()
                .action,
            "migrate.run"
        );
        // 在线更新 apply / rollback
        assert_eq!(
            c(Method::POST, "/api/v1/update/apply").unwrap().action,
            "update.apply"
        );
        assert_eq!(
            c(Method::POST, "/api/v1/update/rollback").unwrap().action,
            "update.rollback"
        );
        // 系统重启 / 关闭（FR-109，ADR-0033）
        assert_eq!(
            c(Method::POST, "/api/v1/system/restart").unwrap().action,
            "system.restart"
        );
        assert_eq!(
            c(Method::POST, "/api/v1/system/shutdown").unwrap().action,
            "system.shutdown"
        );
        // 登出 / 刷新
        assert_eq!(
            c(Method::POST, "/api/v1/auth/logout").unwrap().action,
            "auth.logout"
        );
        assert_eq!(
            c(Method::POST, "/api/v1/auth/refresh").unwrap().action,
            "auth.refresh"
        );
        // 制品上传专用端点
        let up = c(Method::POST, "/api/v1/repositories/r1/upload").unwrap();
        assert_eq!(up.action, "artifact.upload");
        assert_eq!(up.target_repo.as_deref(), Some("r1"));
        // 制品详情删除
        let del = c(Method::DELETE, "/api/v1/repositories/r1/artifacts/a/b.txt").unwrap();
        assert_eq!(del.action, "artifact.delete");
        assert_eq!(del.target_repo.as_deref(), Some("r1"));
        assert_eq!(del.target.as_deref(), Some("a/b.txt"));
    }

    #[test]
    fn 用户组与组acl归类() {
        let c = |m: Method, p: &str| classify_event(&m, p);
        assert_eq!(
            c(Method::POST, "/api/v1/groups").unwrap().action,
            "group.create"
        );
        assert_eq!(
            c(Method::DELETE, "/api/v1/groups/g1").unwrap().action,
            "group.delete"
        );
        assert_eq!(
            c(Method::POST, "/api/v1/groups/g1/members").unwrap().action,
            "group.member.update"
        );
        assert_eq!(
            c(Method::DELETE, "/api/v1/groups/g1/members/u1")
                .unwrap()
                .action,
            "group.member.update"
        );
        let ga = c(Method::POST, "/api/v1/repositories/r1/group-acl").unwrap();
        assert_eq!(ga.action, "group.acl.update");
        assert_eq!(ga.target_repo.as_deref(), Some("r1"));
        assert_eq!(
            c(Method::DELETE, "/api/v1/repositories/r1/group-acl/a1")
                .unwrap()
                .action,
            "group.acl.update"
        );
    }

    #[test]
    fn 未显式归类的非读端点走兜底() {
        // 任何未单列的 /api/v1 非读路径都不应漏记，按方法兜底
        assert_eq!(
            classify_event(&Method::POST, "/api/v1/some/new/endpoint")
                .unwrap()
                .action,
            "change.post"
        );
        assert_eq!(
            classify_event(&Method::PUT, "/api/v1/some/thing")
                .unwrap()
                .action,
            "change.put"
        );
        assert_eq!(
            classify_event(&Method::PATCH, "/api/v1/another")
                .unwrap()
                .action,
            "change.patch"
        );
        assert_eq!(
            classify_event(&Method::DELETE, "/api/v1/yet/another")
                .unwrap()
                .action,
            "change.delete"
        );
        // Docker blob 分块过程（POST/PATCH）也走兜底留痕（不再静默丢弃）
        assert_eq!(
            classify_event(&Method::POST, "/v2/library/app/blobs/uploads/")
                .unwrap()
                .action,
            "change.post"
        );
    }

    #[test]
    fn actor_kind_按凭据形态归类() {
        let build = |auth: Option<&str>| {
            let mut req = Request::builder().uri("/");
            if let Some(v) = auth {
                req = req.header(AUTHORIZATION, v);
            }
            classify_actor_kind(&req.body(axum::body::Body::empty()).unwrap())
        };
        assert_eq!(build(None), ActorKind::Anonymous);
        assert_eq!(build(Some("Basic dXNlcjpwYXNz")), ActorKind::Basic);
        assert_eq!(build(Some("Bearer jna_abcdef")), ActorKind::Token);
        assert_eq!(
            build(Some("Bearer eyJhbGci.payload.sig")),
            ActorKind::Session
        );
        assert_eq!(build(Some("jna_barebearer")), ActorKind::Token);
    }

    #[tokio::test]
    async fn 满队列丢弃并计数() {
        // 容量 1：写满后再投递应被丢弃并计数，绝不阻塞
        let (sender, _receiver) = mpsc::channel(1);
        let sink = AuditSink {
            sender,
            dropped: Arc::new(AtomicU64::new(0)),
        };
        let mk = || NewAuditEntry {
            actor: "a".into(),
            actor_kind: "session".into(),
            request_id: None,
            source_ip: None,
            action: "repo.create".into(),
            target_repo: None,
            target: None,
            result: "success".into(),
            detail: None,
        };
        sink.enqueue(mk()); // 占满容量
        sink.enqueue(mk()); // 丢弃 + 计数
        sink.enqueue(mk()); // 再丢弃 + 计数
        assert_eq!(sink.dropped_count(), 2);
    }
}
