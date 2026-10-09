// FR-114：仓库连接状态展示映射（颜色 + i18n 标签键）。
// 供 RepositoriesPage 徽章与 RepositoryDetailPage 状态显示共用，避免两处重复。
import type { ConnectionStatusValue } from "../api/types";

/** 上游自动阻止的告警 code（observability alerts 与连接状态共用）。 */
export const UPSTREAM_BLOCKED_CODE = "upstream_auto_blocked";

/**
 * 该状态是否属于「阻止态」。
 *
 * FR-43 起阻止态有两个取值：`AUTO_BLOCKED`（窗口内封锁）与 `HALF_OPEN`（窗口已到、正在试探）。
 * 半开期业务流量同样等效封锁（每轮只放行一个探测），故一切「阻止中仓库数」的统计都必须把两者
 * 一并计入——只数 `AUTO_BLOCKED` 会让半开仓库凭空消失，与后端 `blockedCount` 的口径对不上。
 */
export function isBlockedStatus(status: ConnectionStatusValue): boolean {
  return status === "AUTO_BLOCKED" || status === "HALF_OPEN";
}

/**
 * 连接状态 → Mantine Badge 颜色
 * （可用绿 / 自动阻止橙 / 半开青 / 不可用红 / 离线灰 / 未连接灰）。
 */
export const CONN_COLOR: Record<ConnectionStatusValue, string> = {
  AVAILABLE: "green",
  AUTO_BLOCKED: "orange",
  // FR-43：半开（窗口已到、正在试探上游）仍是阻止态，但需与「窗口内封锁」区分，
  // 故取橙与蓝之间的青色：既非「恢复绿」也非「不可用红」，一眼可辨是过渡态。
  HALF_OPEN: "cyan",
  UNAVAILABLE: "red",
  OFFLINE: "gray",
  READY: "gray",
};

/** 连接状态 → i18n 标签键。 */
export const CONN_LABEL_KEY: Record<ConnectionStatusValue, string> = {
  AVAILABLE: "repositories.statusAvailable",
  AUTO_BLOCKED: "repositories.statusAutoBlocked",
  HALF_OPEN: "repositories.statusHalfOpen",
  UNAVAILABLE: "repositories.statusUnavailable",
  OFFLINE: "repositories.statusOffline",
  READY: "repositories.statusReady",
};
