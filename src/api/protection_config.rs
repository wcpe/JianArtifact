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

    // 热替换：锁外重建派生态、短持写锁原子换指针，改完即时生效
    state.protection.replace(new_cfg.clone());
    // 记一条管理动作日志（仅记动作，不含敏感项；配置无凭据可泄露）
    tracing::info!(操作者 = %identity.actor_name(), "管理员更新了防护配置，已即时生效");

    Ok(Json(new_cfg))
}
