// FR-114：仓库连接状态展示映射（颜色 + i18n 标签键）。
// 供 RepositoriesPage 徽章与 RepositoryDetailPage 状态显示共用，避免两处重复。
import type { ConnectionStatusValue } from "../api/types";

/** 连接状态 → Mantine Badge 颜色（可用绿/自动阻止橙/不可用红/离线灰/未连接灰）。 */
export const CONN_COLOR: Record<ConnectionStatusValue, string> = {
  AVAILABLE: "green",
  AUTO_BLOCKED: "orange",
  UNAVAILABLE: "red",
  OFFLINE: "gray",
  READY: "gray",
};

/** 连接状态 → i18n 标签键。 */
export const CONN_LABEL_KEY: Record<ConnectionStatusValue, string> = {
  AVAILABLE: "repositories.statusAvailable",
  AUTO_BLOCKED: "repositories.statusAutoBlocked",
  UNAVAILABLE: "repositories.statusUnavailable",
  OFFLINE: "repositories.statusOffline",
  READY: "repositories.statusReady",
};
