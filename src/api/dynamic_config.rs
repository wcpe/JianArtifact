//! 动态配置面板读写端点（FR-106，ADR-0028）：Admin 在线读取 / 编辑「新 Dynamic 节」的**非密钥**项，
//! 落库 `app_settings`、**重启生效**。
//!
//! 覆盖范围（均为阈值 / 开关 / 周期 / 容量等非密钥项）：
//! - `limits`（上传上限）
//! - `observability.audit` / `observability.usage` / `observability.metrics` /
//!   `observability.metrics_timeseries`
//! - `vuln`（漏洞库开关 / 源 / 周期）
//! - `auth` 三个可调标量（会话 TTL / 登录失败阈值 / 锁定时长），经 [`AuthTunables`] 非密钥视图——
//!   **OIDC / LDAP 密钥绝不在此读写**（守凭据红线）。
//!
//! 设计要点：
//! - **薄 handler**：只做鉴权编排（仅 Admin）、校验、按节 `upsert_setting` 落库、组装响应；无业务逻辑。
//! - **生效语义诚实**：这些节多在**启动期**装载（审计保留任务、使用裁剪、指标采样间隔、vuln 刷新、
//!   JWT TTL、登录锁定等），无现成热替换槽——本期落库后**重启生效**（黄金组合「变更=改 DB 记录、
//!   下次装载生效」），不为每个后台任务强造热替换槽（YAGNI）。
//! - **GET 回显「当前 + 待生效」值**：以启动期生效配置（`state.config`，已是 env⊕DB⊕文件 合并值）为基线，
//!   叠加**当前 DB 覆盖**（含本次 PATCH 后写入的待生效值），让面板回显与「重启后会生效的值」一致。
//! - **白名单 + 默认拒绝**：只写 [`DYNAMIC_KEYS`] 中本端点负责的非密钥节键；凭据 / bootstrap 键不经此路径。
//! - **校验失败不落库**：任一节校验未过返回 400 且**不写任何节**（GET 仍返回旧值）。
//! - **凭据红线**：`auth` 经 [`AuthTunables`] 序列化，结构上不可能带出 OIDC / LDAP 密钥；其余节本就无凭据。

use axum::{extract::State, Json};
use serde::{Deserialize, Serialize};

use crate::config::{
    AuditConfig, LimitsConfig, MetricsConfig, MetricsTimeseriesConfig, UsageConfig, VulnConfig,
};
use crate::config_overlay::{merge_effective_config, AuthTunables};

use super::{ApiError, AppState, Identity};

/// 动态配置面板视图（GET 回显 / PATCH 请求体共用形态）。
///
/// 各节均为非密钥项；`auth` 仅三个可调标量（经 [`AuthTunables`]），不含任何 OIDC / LDAP 密钥。
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct DynamicConfigDto {
    /// 上传等限制（`limits` 节）。
    pub limits: LimitsConfig,
    /// 审计日志保留（`observability.audit` 节）。
    pub audit: AuditConfig,
    /// 使用分析采集（`observability.usage` 节）。
    pub usage: UsageConfig,
    /// Prometheus 指标端点（`observability.metrics` 节）。
    pub metrics: MetricsConfig,
    /// 指标时序采集（`observability.metrics_timeseries` 节）。
    pub metrics_timeseries: MetricsTimeseriesConfig,
    /// 漏洞库离线镜像（`vuln` 节）。
    pub vuln: VulnConfig,
    /// 认证可调标量（`auth` 节非密钥视图）：会话 TTL / 登录失败阈值 / 锁定时长。
    pub auth: AuthTunables,
}

impl DynamicConfigDto {
    /// 从一份生效配置投影出面板 DTO（只取本端点负责的非密钥节）。
    fn from_config(cfg: &crate::config::Config) -> Self {
        Self {
            limits: cfg.limits.clone(),
            audit: cfg.observability.audit.clone(),
            usage: cfg.observability.usage.clone(),
            metrics: cfg.observability.metrics.clone(),
            metrics_timeseries: cfg.observability.metrics_timeseries.clone(),
            vuln: cfg.vuln.clone(),
            auth: AuthTunables {
                session_ttl_secs: cfg.auth.session_ttl_secs,
                login_max_failures: cfg.auth.login_max_failures,
                login_lockout_secs: cfg.auth.login_lockout_secs,
            },
        }
    }

    /// 校验各节非密钥项的数值边界（纯函数、无副作用，便于穷举测试）。
    ///
    /// 仅校验会导致运行异常或无意义的边界：周期 / 间隔类必须 > 0（否则后台任务无法成立 / 死循环），
    /// 会话 TTL 必须 > 0（否则签发即过期）。其余阈值 / 容量 / 开关无硬下界（0 多表示「不启用」或
    /// 由各自语义兜底）。校验通过返回 `Ok(())`，否则返回中文原因。
    fn validate(&self) -> Result<(), String> {
        if self.metrics_timeseries.sample_interval_secs == 0 {
            return Err(
                "指标时序采样间隔（metrics_timeseries.sample_interval_secs）必须大于 0".to_string(),
            );
        }
        if self.vuln.refresh_interval_secs == 0 {
            return Err("漏洞库刷新周期（vuln.refresh_interval_secs）必须大于 0".to_string());
        }
        if self.vuln.download_timeout_secs == 0 {
            return Err("漏洞库下载超时（vuln.download_timeout_secs）必须大于 0".to_string());
        }
        if self.auth.session_ttl_secs == 0 {
            return Err("会话有效期（auth.session_ttl_secs）必须大于 0".to_string());
        }
        if self.auth.login_lockout_secs == 0 {
            return Err("登录锁定时长（auth.login_lockout_secs）必须大于 0".to_string());
        }
        Ok(())
    }
}

/// 据启动期生效配置叠加当前 DB 覆盖，组装面板回显 DTO（GET 与 PATCH 成功后复用）。
///
/// 以 `state.config`（启动期 env⊕DB⊕文件 合并值）为基线，再叠加**当前** `app_settings` 覆盖——
/// 因 `state.config` 已含启动时的 DB 覆盖，再叠加同一批 DB 行是幂等的；本次 PATCH 新写入的行则
/// 在此被叠加进来，使 GET 回显与「重启后会生效的值」一致（诚实标注：这些节重启生效）。
/// 读 DB 失败只 WARN、回落 `state.config`（不阻断回显）。
async fn current_dto(state: &AppState) -> DynamicConfigDto {
    let db_overlay = match state.meta.load_settings().await {
        Ok(rows) => rows,
        Err(e) => {
            tracing::warn!(原因 = %e, "读取动态配置（app_settings）失败，回显回落启动期生效值");
            Vec::new()
        }
    };
    // env 显式集合此处传空：state.config 已并入 env，且回显意在展示「DB 待生效值」，
    // 不二次按 env 钉住（合并为幂等叠加）。
    let effective = merge_effective_config(
        (*state.config).clone(),
        &std::collections::BTreeSet::new(),
        &db_overlay,
    );
    DynamicConfigDto::from_config(&effective)
}

/// 读取动态配置面板各非密钥节的当前 / 待生效值（仅 Admin）。
///
/// 未认证 401、非管理员 403。回显含本次 PATCH 后写入 DB 的待生效值；这些节**重启生效**（前端标注）。
/// 各节均无凭据，整体回显安全（`auth` 仅三个标量，结构上不含 OIDC / LDAP 密钥）。
pub async fn get_dynamic_config(
    State(state): State<AppState>,
    identity: Identity,
) -> Result<Json<DynamicConfigDto>, ApiError> {
    identity.require_admin()?;
    Ok(Json(current_dto(&state).await))
}

/// 编辑动态配置面板各非密钥节（仅 Admin），校验通过即落库 `app_settings`、**重启生效**。
///
/// 校验失败返回 400 且**不写任何节**（再次 GET 仍返回旧值）；成功后按节 `upsert_setting` 落库。
/// 这些节无现成热替换槽，本期不即时换槽——下次启动经覆盖层装载生效（黄金组合）。
/// `auth` 经 [`AuthTunables`] 序列化，绝不带出 OIDC / LDAP 密钥；其余节本就无凭据。
/// 落库失败返回 500（DB 不可用）；序列化失败属内部错误。
pub async fn patch_dynamic_config(
    State(state): State<AppState>,
    identity: Identity,
    Json(dto): Json<DynamicConfigDto>,
) -> Result<Json<DynamicConfigDto>, ApiError> {
    identity.require_admin()?;

    // 先整体校验（任一节非法即拒，不写任何节）
    dto.validate()
        .map_err(|reason| ApiError::BadRequest(format!("动态配置非法：{reason}")))?;

    // 按节序列化为 JSON 片段，落库其对应白名单键。auth 经非密钥视图（不可能带出密钥）。
    // 任一序列化 / 落库失败即报错（已校验，序列化失败属内部异常；落库失败为 DB 不可用）。
    upsert_section(&state, "limits", &dto.limits).await?;
    upsert_section(&state, "observability.audit", &dto.audit).await?;
    upsert_section(&state, "observability.usage", &dto.usage).await?;
    upsert_section(&state, "observability.metrics", &dto.metrics).await?;
    upsert_section(
        &state,
        "observability.metrics_timeseries",
        &dto.metrics_timeseries,
    )
    .await?;
    upsert_section(&state, "vuln", &dto.vuln).await?;
    upsert_section(&state, "auth", &dto.auth).await?;

    // 记一条管理动作日志（仅记动作，各节均无凭据可泄露）
    tracing::info!(操作者 = %identity.actor_name(), "管理员更新了动态配置（limits / observability / vuln / auth 非密钥项），重启生效");

    // 回显「当前 + 待生效」值（叠加刚写入的 DB 覆盖）
    Ok(Json(current_dto(&state).await))
}

/// 把一节非密钥配置序列化为 JSON 落库 `app_settings`（经白名单键）。
///
/// 序列化失败属内部异常（已是强类型），落库失败为 DB 不可用，均冒泡给调用方转 500。
async fn upsert_section<T: Serialize>(
    state: &AppState,
    key: &str,
    value: &T,
) -> Result<(), ApiError> {
    let json = serde_json::to_string(value).map_err(|e| {
        tracing::error!(配置节 = key, 原因 = %e, "动态配置序列化失败");
        ApiError::Internal
    })?;
    state.meta.upsert_setting(key, &json).await.map_err(|e| {
        tracing::error!(配置节 = key, 原因 = %e, "动态配置落库失败");
        ApiError::Internal
    })?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::super::tests::{测试用状态, 读_json};
    use super::*;
    use crate::auth::hash_password;
    use crate::meta::Role;
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

    /// 便捷：带可选 Bearer 令牌 GET 动态配置端点。
    async fn 请求_get(state: AppState, 令牌: Option<&str>) -> axum::response::Response {
        let app = super::super::build_router(state);
        let mut builder = Request::builder()
            .method("GET")
            .uri("/api/v1/settings/dynamic");
        if let Some(t) = 令牌 {
            builder = builder.header("Authorization", format!("Bearer {t}"));
        }
        app.oneshot(builder.body(Body::empty()).unwrap())
            .await
            .unwrap()
    }

    /// 便捷：带可选 Bearer 令牌 PATCH 动态配置端点（JSON 请求体）。
    async fn 请求_patch(
        state: AppState,
        令牌: Option<&str>,
        body: serde_json::Value,
    ) -> axum::response::Response {
        let app = super::super::build_router(state);
        let mut builder = Request::builder()
            .method("PATCH")
            .uri("/api/v1/settings/dynamic")
            .header("Content-Type", "application/json");
        if let Some(t) = 令牌 {
            builder = builder.header("Authorization", format!("Bearer {t}"));
        }
        app.oneshot(builder.body(Body::from(body.to_string())).unwrap())
            .await
            .unwrap()
    }

    /// 构造一份合法的动态配置编辑体（各节非密钥项）。
    fn 合法编辑体() -> serde_json::Value {
        serde_json::json!({
            "limits": { "max_artifact_size": 4096 },
            "audit": { "retention_days": 30, "max_rows": 500000 },
            "usage": { "detail_enabled": true, "max_detail_rows": 200000 },
            "metrics": { "enabled": false, "allow_anonymous": false },
            "metrics_timeseries": { "enabled": true, "sample_interval_secs": 120, "retention_days": 14, "max_rows": 500000 },
            "vuln": { "enabled": true, "source_base_url": "https://osv.example", "ecosystems": ["Maven"], "refresh_interval_secs": 43200, "download_timeout_secs": 300 },
            "auth": { "session_ttl_secs": 7200, "login_max_failures": 8, "login_lockout_secs": 600 }
        })
    }

    // ===== 鉴权 =====

    #[tokio::test]
    async fn get_匿名被拒_401() {
        let (state, _dir) = 测试用状态().await;
        let resp = 请求_get(state, None).await;
        assert_eq!(resp.status(), StatusCode::UNAUTHORIZED);
    }

    #[tokio::test]
    async fn get_普通用户被拒_403() {
        let (state, _dir) = 测试用状态().await;
        let token = 签发令牌(&state, "u", Role::User).await;
        let resp = 请求_get(state, Some(&token)).await;
        assert_eq!(resp.status(), StatusCode::FORBIDDEN);
    }

    #[tokio::test]
    async fn patch_匿名被拒_401() {
        let (state, _dir) = 测试用状态().await;
        let resp = 请求_patch(state, None, 合法编辑体()).await;
        assert_eq!(resp.status(), StatusCode::UNAUTHORIZED);
    }

    #[tokio::test]
    async fn patch_普通用户被拒_403() {
        let (state, _dir) = 测试用状态().await;
        let token = 签发令牌(&state, "u", Role::User).await;
        let resp = 请求_patch(state, Some(&token), 合法编辑体()).await;
        assert_eq!(resp.status(), StatusCode::FORBIDDEN);
    }

    // ===== GET 回显当前生效值 =====

    #[tokio::test]
    async fn get_管理员回显默认生效值() {
        // 空库（无 DB 覆盖）：GET 回显文件默认（= 测试用状态的默认 Config）
        let (state, _dir) = 测试用状态().await;
        let token = 签发令牌(&state, "admin", Role::Admin).await;
        let resp = 请求_get(state, Some(&token)).await;
        assert_eq!(resp.status(), StatusCode::OK);
        let body = 读_json(resp).await;
        // 默认值：limits.max_artifact_size = null；auth.session_ttl_secs = 3600；metrics.enabled = true
        assert!(body["limits"]["max_artifact_size"].is_null());
        assert_eq!(body["auth"]["session_ttl_secs"], 3600);
        assert_eq!(body["metrics"]["enabled"], true);
        assert_eq!(body["metrics_timeseries"]["sample_interval_secs"], 60);
        assert_eq!(body["vuln"]["enabled"], false);
    }

    // ===== PATCH 校验 + 落库 + 重装载仍生效 =====

    #[tokio::test]
    async fn patch_管理员成功_落库各节_重装载仍生效() {
        let (state, _dir) = 测试用状态().await;
        let token = 签发令牌(&state, "admin", Role::Admin).await;
        let meta = state.meta.clone();
        let resp = 请求_patch(state, Some(&token), 合法编辑体()).await;
        assert_eq!(resp.status(), StatusCode::OK);

        // 各节均已落库 app_settings
        let rows = meta.load_settings().await.unwrap();
        for key in [
            "limits",
            "observability.audit",
            "observability.usage",
            "observability.metrics",
            "observability.metrics_timeseries",
            "vuln",
            "auth",
        ] {
            assert!(rows.iter().any(|(k, _)| k == key), "{key} 应已落库");
        }
        // 重新装载（重启等价）：经覆盖纯函数合并后各改动仍生效
        let eff = merge_effective_config(
            crate::config::Config::default(),
            &std::collections::BTreeSet::new(),
            &rows,
        );
        assert_eq!(eff.limits.max_artifact_size, Some(4096));
        assert_eq!(eff.observability.audit.retention_days, 30);
        assert!(eff.observability.usage.detail_enabled);
        assert!(!eff.observability.metrics.enabled);
        assert_eq!(
            eff.observability.metrics_timeseries.sample_interval_secs,
            120
        );
        assert!(eff.vuln.enabled);
        assert_eq!(eff.vuln.refresh_interval_secs, 43200);
        assert_eq!(eff.auth.session_ttl_secs, 7200);
        assert_eq!(eff.auth.login_max_failures, 8);
        assert_eq!(eff.auth.login_lockout_secs, 600);
    }

    #[tokio::test]
    async fn patch_成功后_get_回显待生效值() {
        // PATCH 写入待生效值后，GET 回显应反映新值（叠加 DB 覆盖）
        let (state, _dir) = 测试用状态().await;
        let token = 签发令牌(&state, "admin", Role::Admin).await;
        let app = super::super::build_router(state);

        let patch = app
            .clone()
            .oneshot(
                Request::builder()
                    .method("PATCH")
                    .uri("/api/v1/settings/dynamic")
                    .header("Authorization", format!("Bearer {token}"))
                    .header("Content-Type", "application/json")
                    .body(Body::from(合法编辑体().to_string()))
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(patch.status(), StatusCode::OK);

        let get = app
            .oneshot(
                Request::builder()
                    .method("GET")
                    .uri("/api/v1/settings/dynamic")
                    .header("Authorization", format!("Bearer {token}"))
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(get.status(), StatusCode::OK);
        let body = 读_json(get).await;
        assert_eq!(body["limits"]["max_artifact_size"], 4096);
        assert_eq!(body["auth"]["session_ttl_secs"], 7200);
        assert_eq!(body["vuln"]["enabled"], true);
    }

    // ===== 校验失败不落库 =====

    #[tokio::test]
    async fn patch_非法采样间隔_400_不落库() {
        let (state, _dir) = 测试用状态().await;
        let token = 签发令牌(&state, "admin", Role::Admin).await;
        let meta = state.meta.clone();
        let mut body = 合法编辑体();
        body["metrics_timeseries"]["sample_interval_secs"] = serde_json::json!(0);
        let resp = 请求_patch(state, Some(&token), body).await;
        assert_eq!(resp.status(), StatusCode::BAD_REQUEST);
        assert!(
            meta.load_settings().await.unwrap().is_empty(),
            "校验失败不得落库任何节"
        );
    }

    #[tokio::test]
    async fn patch_非法会话_ttl_400_不落库() {
        let (state, _dir) = 测试用状态().await;
        let token = 签发令牌(&state, "admin", Role::Admin).await;
        let meta = state.meta.clone();
        let mut body = 合法编辑体();
        body["auth"]["session_ttl_secs"] = serde_json::json!(0);
        let resp = 请求_patch(state, Some(&token), body).await;
        assert_eq!(resp.status(), StatusCode::BAD_REQUEST);
        assert!(
            meta.load_settings().await.unwrap().is_empty(),
            "校验失败不得落库任何节"
        );
    }

    // ===== 凭据红线：auth 节落库不含 OIDC / LDAP 密钥字段 =====

    #[tokio::test]
    async fn patch_auth_落库_仅三标量_无凭据字段() {
        let (state, _dir) = 测试用状态().await;
        let token = 签发令牌(&state, "admin", Role::Admin).await;
        let meta = state.meta.clone();
        let resp = 请求_patch(state, Some(&token), 合法编辑体()).await;
        assert_eq!(resp.status(), StatusCode::OK);

        let rows = meta.load_settings().await.unwrap();
        let auth_json = rows
            .iter()
            .find(|(k, _)| k == "auth")
            .map(|(_, v)| v.clone())
            .expect("auth 节应已落库");
        // auth 节经 AuthTunables 视图序列化：仅三标量，绝不含 oidc / ldap / secret / password 字段名
        for forbidden in [
            "oidc",
            "ldap",
            "secret",
            "password",
            "client_secret",
            "bind_password",
        ] {
            assert!(
                !auth_json.contains(forbidden),
                "auth 节落库不得含凭据字段名 {forbidden}：{auth_json}"
            );
        }
        // 全量落库 JSON 同样不含任何凭据相关串
        let all: String = rows.iter().map(|(_, v)| v.clone()).collect();
        for forbidden in ["client_secret", "bind_password", "token"] {
            assert!(
                !all.contains(forbidden),
                "动态配置落库不得含凭据相关串 {forbidden}"
            );
        }
    }

    // ===== 非白名单 / bootstrap 键不经此端点（端点只写固定白名单键）=====

    #[tokio::test]
    async fn patch_落库键集_限于白名单_无_server_data() {
        // 端点只 upsert 固定白名单键，绝不写 server / data 等 bootstrap 键
        let (state, _dir) = 测试用状态().await;
        let token = 签发令牌(&state, "admin", Role::Admin).await;
        let meta = state.meta.clone();
        let resp = 请求_patch(state, Some(&token), 合法编辑体()).await;
        assert_eq!(resp.status(), StatusCode::OK);
        let rows = meta.load_settings().await.unwrap();
        for (k, _) in &rows {
            assert!(
                crate::config_overlay::DYNAMIC_KEYS.contains(&k.as_str()),
                "落库键 {k} 必须在动态白名单内"
            );
            assert!(
                k != "server" && k != "data",
                "bootstrap 键 {k} 绝不应被本端点写入"
            );
        }
    }
}
