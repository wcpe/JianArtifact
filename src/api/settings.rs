//! 控制台设置只读聚合端点（FR-87）：仅 Admin 读取脱敏后的网络代理（FR-84）+ 在线更新（FR-85）
//! 配置与当前版本，供「设置」页展示。
//!
//! 设计要点：
//! - **薄 handler**：只做鉴权编排（仅 Admin）、读 `state.config` 组装脱敏 DTO、返回 JSON；无业务逻辑。
//! - **只读取向（守 ADR-0020）**：网络代理与在线更新配置真源是 TOML + env、运行时不热替换，本端点
//!   只读展示、不提供编辑；页面唯一写动作走 FR-85 既有 check/apply 端点。
//! - **脱敏红线**：响应**绝不含任何凭据**——代理 URL 经 [`sanitize_proxy_url`] 去 `user:pass@`；
//!   更新 token 只回 `has_token: bool`，绝不回显 token 本体。

use axum::{extract::State, Json};
use serde::Serialize;

use super::{ApiError, AppState, Identity};

/// 去除 URL 中的 userinfo（`scheme://user:pass@host` → `scheme://host`）。
///
/// 仅做凭据脱敏，不重排其余部分：
/// - userinfo 仅存在于 authority 段（`scheme://userinfo@host` 中、host 路径分隔 `/` 之前）。
///   取该段内最后一个 `@` 为 userinfo 与 host 的分界，去除其前段；保留 scheme、host、port、
///   path、query 原样。
/// - authority 段内无 `@`（含 `@` 仅出现在 path/query 时）：原样返回，不误删。
/// - 空串 / 异常形态：原样返回，不 panic（脱敏不应引入新错误）。
pub fn sanitize_proxy_url(url: &str) -> String {
    // authority 段起点：scheme 后 `//` 之后；无 `//`（非标准 URL）时整串视作 authority 起点。
    let authority_start = match url.find("://") {
        Some(scheme_end) => scheme_end + 3,
        None => 0,
    };
    // authority 段终点：authority 起点之后首个 `/`（path 起点）；无 path 时到串尾。
    let authority_end = url[authority_start..]
        .find('/')
        .map(|rel| authority_start + rel)
        .unwrap_or(url.len());
    // 仅在 authority 段内找 userinfo 分界 `@`（取最后一个，兼容口令含 `@`）；无则无 userinfo
    let Some(rel_at) = url[authority_start..authority_end].rfind('@') else {
        return url.to_string();
    };
    let at_pos = authority_start + rel_at;
    // 拼接：authority 起点之前（含 `scheme://`）+ `@` 之后（host 起点）
    let mut sanitized = String::with_capacity(url.len());
    sanitized.push_str(&url[..authority_start]);
    sanitized.push_str(&url[at_pos + 1..]);
    sanitized
}

/// 网络代理视图（脱敏后）。
#[derive(Debug, Serialize)]
pub struct NetworkProxyView {
    /// HTTP 出站代理 URL（已去除 `user:pass@` 凭据）。
    pub http: Option<String>,
    /// HTTPS 出站代理 URL（已去除 `user:pass@` 凭据）。
    pub https: Option<String>,
    /// 直连绕过列表（无凭据，原样）。
    pub no_proxy: Option<String>,
}

/// 在线更新视图（脱敏后）。
#[derive(Debug, Serialize)]
pub struct UpdateView {
    /// 是否启用在线更新（出站开关）。
    pub enabled: bool,
    /// 仓库源（`owner/repo`）。
    pub repo: String,
    /// GitHub API 基址。
    pub api_base_url: String,
    /// 重启模式（`self` / `exit`）。
    pub restart_mode: String,
    /// 是否已配置访问 token：**仅布尔，绝不回显 token 本体**。
    pub has_token: bool,
}

/// 设置页聚合视图（脱敏后）。
#[derive(Debug, Serialize)]
pub struct SettingsView {
    /// 当前运行版本（编译期注入）。
    pub current_version: String,
    /// 网络代理配置（脱敏）。
    pub network_proxy: NetworkProxyView,
    /// 在线更新配置（脱敏）。
    pub update: UpdateView,
}

/// 读取脱敏后的网络代理 + 在线更新配置与当前版本（仅 Admin）。
///
/// 未认证 401、非管理员 403（复用 [`Identity::require_admin`]）。读 `state.config`，
/// 代理 URL 去凭据、token 只回 `has_token`，响应绝不含任何凭据。
pub async fn get_settings(
    State(state): State<AppState>,
    identity: Identity,
) -> Result<Json<SettingsView>, ApiError> {
    identity.require_admin()?;

    let proxy = &state.config.network.proxy;
    let update = &state.config.update;

    let view = SettingsView {
        current_version: env!("CARGO_PKG_VERSION").to_string(),
        network_proxy: NetworkProxyView {
            http: proxy.http.as_deref().map(sanitize_proxy_url),
            https: proxy.https.as_deref().map(sanitize_proxy_url),
            no_proxy: proxy.no_proxy.clone(),
        },
        update: UpdateView {
            enabled: update.enabled,
            repo: update.repo.clone(),
            api_base_url: update.api_base_url.clone(),
            restart_mode: update.restart_mode.clone(),
            // 仅暴露是否已配置 token，绝不回显 token 本体
            has_token: update.token.is_some(),
        },
    };
    Ok(Json(view))
}

#[cfg(test)]
mod tests {
    use super::super::tests::{测试用状态, 读_json};
    use super::*;
    use crate::auth::hash_password;
    use crate::meta::Role;
    use axum::body::Body;
    use axum::http::{Request, StatusCode};
    use std::sync::Arc;
    use tower::ServiceExt;

    // ===== sanitize_proxy_url 纯函数穷举 =====

    #[test]
    fn 脱敏_去除带端口的_userinfo() {
        assert_eq!(
            sanitize_proxy_url("http://user:pass@proxy.internal:8080"),
            "http://proxy.internal:8080"
        );
    }

    #[test]
    fn 脱敏_去除仅用户名的_userinfo() {
        assert_eq!(
            sanitize_proxy_url("https://alice@proxy.internal"),
            "https://proxy.internal"
        );
    }

    #[test]
    fn 脱敏_无_userinfo_原样返回() {
        assert_eq!(
            sanitize_proxy_url("http://proxy.internal:8080"),
            "http://proxy.internal:8080"
        );
    }

    #[test]
    fn 脱敏_空串不_panic_原样返回() {
        assert_eq!(sanitize_proxy_url(""), "");
    }

    #[test]
    fn 脱敏_无_scheme_仍去除_userinfo() {
        // 无 scheme 的异常形态：把 `@` 前整段视作 userinfo 去除，结果不含凭据
        assert_eq!(sanitize_proxy_url("user:pass@host:8080"), "host:8080");
    }

    #[test]
    fn 脱敏_path_中的_at_不误删() {
        // `@` 出现在 path 段（authority 之后）：非 userinfo，原样返回
        assert_eq!(
            sanitize_proxy_url("http://proxy.internal/path@x"),
            "http://proxy.internal/path@x"
        );
    }

    #[test]
    fn 脱敏_多个_at_取最后一个_authority_分隔() {
        // 密码中含 `@`（少见但合法）：以最后一个 `@` 为 userinfo 与 host 分界
        assert_eq!(
            sanitize_proxy_url("http://user:p@ss@proxy.internal:8080"),
            "http://proxy.internal:8080"
        );
    }

    // ===== GET /api/v1/settings 端点鉴权 + 脱敏 =====

    /// 在状态库内建一个指定角色用户并签发其会话 JWT。
    async fn 签发令牌(state: &AppState, name: &str, role: Role) -> String {
        let uid = state
            .meta
            .create_user(name, &hash_password("pw").unwrap(), role)
            .await
            .unwrap();
        state.jwt.issue(&uid, name, role).unwrap()
    }

    /// 便捷：带可选 Bearer 令牌请求设置端点。
    async fn 请求(state: AppState, 令牌: Option<&str>) -> axum::response::Response {
        let app = super::super::build_router(state);
        let mut builder = Request::builder().method("GET").uri("/api/v1/settings");
        if let Some(t) = 令牌 {
            builder = builder.header("Authorization", format!("Bearer {t}"));
        }
        app.oneshot(builder.body(Body::empty()).unwrap())
            .await
            .unwrap()
    }

    #[tokio::test]
    async fn settings_匿名被拒_401() {
        let (state, _dir) = 测试用状态().await;
        let resp = 请求(state, None).await;
        assert_eq!(resp.status(), StatusCode::UNAUTHORIZED);
    }

    #[tokio::test]
    async fn settings_普通用户被拒_403() {
        let (state, _dir) = 测试用状态().await;
        let token = 签发令牌(&state, "u", Role::User).await;
        let resp = 请求(state, Some(&token)).await;
        assert_eq!(resp.status(), StatusCode::FORBIDDEN);
    }

    #[tokio::test]
    async fn settings_管理员成功_200_并脱敏代理凭据与隐藏_token() {
        let (mut state, _dir) = 测试用状态().await;
        // 注入含凭据的代理与更新 token，断言响应中均不回显凭据
        let mut cfg = (*state.config).clone();
        cfg.network.proxy.http = Some("http://user:pass@proxy.internal:8080".to_string());
        cfg.network.proxy.https = Some("https://secret:tok@proxy.internal:8443".to_string());
        cfg.network.proxy.no_proxy = Some("localhost,127.0.0.1".to_string());
        cfg.update.enabled = true;
        cfg.update.repo = "wcpe/JianArtifact".to_string();
        cfg.update.token = Some("ghp_supersecrettoken".to_string());
        state.config = Arc::new(cfg);

        let token = 签发令牌(&state, "admin", Role::Admin).await;
        let resp = 请求(state, Some(&token)).await;
        assert_eq!(resp.status(), StatusCode::OK);

        let body = 读_json(resp).await;
        // 当前版本回显
        assert_eq!(body["current_version"], env!("CARGO_PKG_VERSION"));
        // 代理 URL 已脱敏：不含凭据、保留 host:port
        assert_eq!(body["network_proxy"]["http"], "http://proxy.internal:8080");
        assert_eq!(
            body["network_proxy"]["https"],
            "https://proxy.internal:8443"
        );
        assert_eq!(body["network_proxy"]["no_proxy"], "localhost,127.0.0.1");
        // 更新区：仅 has_token 布尔，绝不回显 token 本体
        assert_eq!(body["update"]["enabled"], true);
        assert_eq!(body["update"]["repo"], "wcpe/JianArtifact");
        assert_eq!(body["update"]["has_token"], true);

        // 关键脱敏断言：整段响应文本中不得出现任何凭据明文
        let text = body.to_string();
        assert!(
            !text.contains("user:pass"),
            "代理用户名/口令不得回显：{text}"
        );
        assert!(!text.contains("secret:tok"), "代理凭据不得回显：{text}");
        assert!(
            !text.contains("ghp_supersecrettoken"),
            "更新 token 本体不得回显：{text}"
        );
    }

    #[tokio::test]
    async fn settings_未配置_token_时_has_token_为_false() {
        let (state, _dir) = 测试用状态().await;
        // 默认配置：update.token 为 None
        let token = 签发令牌(&state, "admin", Role::Admin).await;
        let resp = 请求(state, Some(&token)).await;
        assert_eq!(resp.status(), StatusCode::OK);
        let body = 读_json(resp).await;
        assert_eq!(body["update"]["has_token"], false);
    }
}
