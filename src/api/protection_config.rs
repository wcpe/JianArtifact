//! 防护配置管理端点（FR-79，扩展 ADR-0008）：Admin 在线读取 / 修改各防护维度配置，改完即时生效。
//!
//! 设计要点：
//! - **薄 handler**：只做鉴权编排（仅 Admin）、调用 [`ProtectionConfig::validate`] 校验、调用
//!   [`crate::api::ProtectionState::replace`] 热替换、组装响应；不写业务逻辑。
//! - **整体替换语义**：`PATCH` 以一份完整 `ProtectionConfig` 整体替换当前生效的防护子树（前端从 `GET`
//!   取当前配置、改后回传），校验通过即时生效、下一个请求按新值判定，**无须重启**。
//! - **校验失败不改状态**：校验未过返回 400 且**不替换**现有配置（GET 仍返回旧值）。
//! - **无明文密钥**：`ProtectionConfig` 各维度均为阈值 / 开关 / 难度 / IP 名单 / WAF 规则，**不含**
//!   任何密码 / Token / 上游凭据；整体序列化回显不泄露敏感项（守安全脱敏红线）。
//! - **仅 Admin**：防护配置属管理操作，未认证 401、非管理员 403（复用 [`Identity::require_admin`]）。

use axum::{extract::State, Json};

use crate::config::ProtectionConfig;

use super::{ApiError, AppState, Identity};

/// 读取当前生效的防护配置（仅 Admin）。
///
/// 取自运行时防护热替换槽的当前快照（防护配置真源），反映含运行时 PATCH 在内的最新生效值。
pub async fn get_protection_config(
    State(state): State<AppState>,
    identity: Identity,
) -> Result<Json<ProtectionConfig>, ApiError> {
    identity.require_admin()?;
    // 克隆当前快照中的配置返回；ProtectionConfig 无敏感项，整体回显安全
    let snapshot = state.protection.snapshot();
    Ok(Json(snapshot.config.clone()))
}

/// 整体替换防护配置（仅 Admin），校验通过即时生效、无须重启。
///
/// 请求体为一份完整 `ProtectionConfig`（前端从 GET 取当前值、改后整体回传）。校验失败返回 400 且
/// 不改变现有配置；成功后重建派生态（IP 名单匹配器、WAF 规则集）并原子替换，下一个请求即按新值判定。
pub async fn patch_protection_config(
    State(state): State<AppState>,
    identity: Identity,
    Json(new_cfg): Json<ProtectionConfig>,
) -> Result<Json<ProtectionConfig>, ApiError> {
    identity.require_admin()?;

    // 校验各维度数值边界；非法即拒，不触碰现有生效配置
    new_cfg
        .validate()
        .map_err(|reason| ApiError::BadRequest(format!("防护配置非法：{reason}")))?;

    // 持久化到 app_settings（FR-106，ADR-0028）：protection 整节为非密钥项（阈值 / 开关 / 名单 / 规则），
    // 序列化为 JSON 落库，重启后经装配层覆盖仍生效。落库在换槽前做（IO 在锁外）；落库失败只 WARN、
    // 不阻断热替换（即时生效优先，重启回落上次入库值或文件默认）。
    match serde_json::to_string(&new_cfg) {
        Ok(json) => {
            if let Err(e) = state.meta.upsert_setting("protection", &json).await {
                tracing::warn!(原因 = %e, "防护配置落库失败，热替换仍生效（重启回落上次入库 / 文件默认）");
            }
        }
        Err(e) => {
            tracing::warn!(原因 = %e, "防护配置序列化失败，跳过落库，热替换仍生效");
        }
    }

    // 热替换：锁外重建派生态、短持写锁原子换指针，改完即时生效
    state.protection.replace(new_cfg.clone());
    // 记一条管理动作日志（仅记动作，不含敏感项；配置无凭据可泄露）
    tracing::info!(操作者 = %identity.actor_name(), "管理员更新了防护配置，已即时生效");

    Ok(Json(new_cfg))
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

    /// 便捷：带 Bearer 令牌 PATCH 防护配置端点。
    async fn 请求_patch(
        state: AppState,
        令牌: &str,
        body: serde_json::Value,
    ) -> axum::response::Response {
        let app = super::super::build_router(state);
        app.oneshot(
            Request::builder()
                .method("PATCH")
                .uri("/api/v1/protection/config")
                .header("Content-Type", "application/json")
                .header("Authorization", format!("Bearer {令牌}"))
                .body(Body::from(body.to_string()))
                .unwrap(),
        )
        .await
        .unwrap()
    }

    #[tokio::test]
    async fn patch_防护配置_成功后落库_app_settings() {
        // FR-106：PATCH 非密钥防护配置 → 写 app_settings（key=protection）→ 重新装载后仍生效
        let (state, _dir) = 测试用状态().await;
        let token = 签发令牌(&state, "admin", Role::Admin).await;
        let meta = state.meta.clone();

        let body = serde_json::json!({
            "rate_limit": { "enabled": true, "window_secs": 30, "ip_max_requests": 50, "identity_max_requests": 100, "repo_max_requests": 0, "ip_max_concurrent": 0, "user_max_concurrent": 0, "repo_max_concurrent": 0 },
            "ip_list": { "allow": [], "deny": [] },
            "ban": { "enabled": false, "window_secs": 60, "threshold": 100, "duration_secs": 900 },
            "slowloris": { "enabled": false, "body_read_timeout_secs": 30, "header_timeout_secs": 30, "max_body_bytes": 0 },
            "cc_challenge": { "enabled": false, "difficulty": 20, "ttl_secs": 300, "exempt_authenticated": true },
            "waf": { "enabled": false, "rules": [] },
            "alerts": { "enabled": false, "window_secs": 300, "rate_limit_warn_threshold": 1000, "ban_warn_threshold": 50, "cc_challenge_fail_warn_threshold": 1000, "waf_block_warn_threshold": 500, "slowloris_warn_threshold": 200, "max_rows": 100000 }
        });
        let resp = 请求_patch(state, &token, body).await;
        assert_eq!(resp.status(), StatusCode::OK);

        // 已落库 app_settings（key=protection）
        let rows = meta.load_settings().await.unwrap();
        let stored = rows
            .iter()
            .find(|(k, _)| k == "protection")
            .expect("protection 应已落库");
        // 重新装载：经覆盖纯函数合并后，rate_limit 改动仍生效（重启等价）
        let eff = crate::config_overlay::merge_effective_config(
            crate::config::Config::default(),
            &std::collections::BTreeSet::new(),
            &rows,
        );
        let _ = stored;
        assert!(
            eff.protection.rate_limit.enabled,
            "重新装载后限流开关应仍为 true"
        );
        assert_eq!(eff.protection.rate_limit.window_secs, 30);
    }

    #[tokio::test]
    async fn patch_防护配置_落库无凭据明文() {
        // FR-106 红线：protection 为非密钥节，落库 JSON 不含任何凭据字段名 / 明文
        let (state, _dir) = 测试用状态().await;
        let token = 签发令牌(&state, "admin", Role::Admin).await;
        let meta = state.meta.clone();
        let body = serde_json::json!({
            "rate_limit": { "enabled": true, "window_secs": 60, "ip_max_requests": 1200, "identity_max_requests": 2400, "repo_max_requests": 0, "ip_max_concurrent": 0, "user_max_concurrent": 0, "repo_max_concurrent": 0 },
            "ip_list": { "allow": [], "deny": [] },
            "ban": { "enabled": false, "window_secs": 60, "threshold": 100, "duration_secs": 900 },
            "slowloris": { "enabled": false, "body_read_timeout_secs": 30, "header_timeout_secs": 30, "max_body_bytes": 0 },
            "cc_challenge": { "enabled": false, "difficulty": 20, "ttl_secs": 300, "exempt_authenticated": true },
            "waf": { "enabled": false, "rules": [] },
            "alerts": { "enabled": false, "window_secs": 300, "rate_limit_warn_threshold": 1000, "ban_warn_threshold": 50, "cc_challenge_fail_warn_threshold": 1000, "waf_block_warn_threshold": 500, "slowloris_warn_threshold": 200, "max_rows": 100000 }
        });
        let resp = 请求_patch(state, &token, body).await;
        assert_eq!(resp.status(), StatusCode::OK);
        let rows = meta.load_settings().await.unwrap();
        let all_json: String = rows.iter().map(|(_, v)| v.clone()).collect();
        for forbidden in [
            "token",
            "password",
            "secret",
            "client_secret",
            "bind_password",
            "user:pass",
        ] {
            assert!(
                !all_json.contains(forbidden),
                "防护配置落库不得含凭据相关串 {forbidden}"
            );
        }
    }

    #[tokio::test]
    async fn patch_防护配置_校验失败_400_不落库() {
        // FR-106：非法配置（window_secs=0）返回 400，app_settings 不被写入
        let (state, _dir) = 测试用状态().await;
        let token = 签发令牌(&state, "admin", Role::Admin).await;
        let meta = state.meta.clone();
        let body = serde_json::json!({
            "rate_limit": { "enabled": true, "window_secs": 0, "ip_max_requests": 50, "identity_max_requests": 100, "repo_max_requests": 0, "ip_max_concurrent": 0, "user_max_concurrent": 0, "repo_max_concurrent": 0 },
            "ip_list": { "allow": [], "deny": [] },
            "ban": { "enabled": false, "window_secs": 60, "threshold": 100, "duration_secs": 900 },
            "slowloris": { "enabled": false, "body_read_timeout_secs": 30, "header_timeout_secs": 30, "max_body_bytes": 0 },
            "cc_challenge": { "enabled": false, "difficulty": 20, "ttl_secs": 300, "exempt_authenticated": true },
            "waf": { "enabled": false, "rules": [] },
            "alerts": { "enabled": false, "window_secs": 300, "rate_limit_warn_threshold": 1000, "ban_warn_threshold": 50, "cc_challenge_fail_warn_threshold": 1000, "waf_block_warn_threshold": 500, "slowloris_warn_threshold": 200, "max_rows": 100000 }
        });
        let resp = 请求_patch(state, &token, body).await;
        assert_eq!(resp.status(), StatusCode::BAD_REQUEST);
        // 校验失败先于落库：app_settings 仍为空（未写入 protection）
        assert!(
            meta.load_settings().await.unwrap().is_empty(),
            "校验失败不得落库"
        );
    }

    #[tokio::test]
    async fn get_防护配置_不含敏感项() {
        // 防护配置整体无凭据，GET 回显安全（守红线）
        let (state, _dir) = 测试用状态().await;
        let token = 签发令牌(&state, "admin", Role::Admin).await;
        let app = super::super::build_router(state);
        let resp = app
            .oneshot(
                Request::builder()
                    .method("GET")
                    .uri("/api/v1/protection/config")
                    .header("Authorization", format!("Bearer {token}"))
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(resp.status(), StatusCode::OK);
        let body = 读_json(resp).await;
        let text = body.to_string();
        for forbidden in ["password", "secret", "token"] {
            assert!(!text.contains(forbidden), "GET 回显不得含 {forbidden}");
        }
    }
}
