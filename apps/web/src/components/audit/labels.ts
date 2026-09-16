// 审计工作台（方案 A）共享字典与格式化：类别/结果/严重度/认证来源的
// i18n 键映射与语义色，供左右栏与抽屉共用，避免魔法字符串散落。
import type { AuditCategory, AuditResult } from "../../api/types";
import { parseUtc } from "../../lib/timeFormat";

/** 类别 → i18n 键（标签统一走 auditWorkbench 命名空间）。 */
export const CATEGORY_LABEL_KEYS: Record<string, string> = {
  management_change: "auditWorkbench.categoryManagementChange",
  asset_change: "auditWorkbench.categoryAssetChange",
  security_event: "auditWorkbench.categorySecurityEvent",
  replication: "auditWorkbench.categoryReplication",
};

/** 类别筛选可选项（服务端契约枚举）。 */
export const CATEGORY_VALUES: AuditCategory[] = [
  "management_change",
  "asset_change",
  "security_event",
  "replication",
];

/** 结果筛选可选项（服务端契约枚举）。 */
export const RESULT_VALUES: AuditResult[] = ["success", "failure", "pending", "unknown"];

/** 结果 → 既有 auditResult 命名空间键。 */
export function resultLabelKey(result: unknown): string {
  const value = typeof result === "string" ? result : "unknown";
  return `auditResult.${value in RESULT_KEY_WHITELIST ? value : "unknown"}`;
}

const RESULT_KEY_WHITELIST: Record<string, true> = {
  success: true,
  failure: true,
  pending: true,
  unknown: true,
  mixed: true,
  running: true,
};

/** 结果 → Mantine 语义色。 */
export function resultColor(result: unknown): string {
  switch (result) {
    case "failure":
      return "red";
    case "success":
      return "green";
    case "pending":
      return "orange";
    case "running":
      return "blue";
    default:
      return "gray";
  }
}

/** 严重度 → i18n 键。 */
export function severityLabelKey(severity: unknown): string | null {
  switch (severity) {
    case "high":
      return "auditWorkbench.severityHigh";
    case "medium":
      return "auditWorkbench.severityMedium";
    case "low":
      return "auditWorkbench.severityLow";
    case "info":
      return "auditWorkbench.severityInfo";
    default:
      return null;
  }
}

/** 严重度 → Mantine 语义色。 */
export function severityColor(severity: unknown): string {
  return severity === "high" ? "red" : severity === "medium" ? "orange" : "gray";
}

/** 认证来源 → i18n 键。 */
const AUTH_SOURCE_KEYS: Record<string, string> = {
  jwt: "authSource.jwt",
  api_key: "authSource.apiKey",
  web: "authSource.session",
  anonymous: "authSource.anonymous",
  system: "authSource.system",
  // 旧契约枚举（兼容历史数据）
  web_jwt: "auditWorkbench.authWebJwt",
  protocol_token: "auditWorkbench.authProtocolToken",
  basic: "auditWorkbench.authBasic",
};

export function authSourceLabelKey(source: unknown): string | null {
  return typeof source === "string" ? (AUTH_SOURCE_KEYS[source] ?? null) : null;
}

/** 防御式读取目标/实体的展示文本（label → entityKey → entityType）。 */
export function asText(value: unknown): string {
  if (typeof value === "string") return value;
  if (value && typeof value === "object") {
    const t = value as { label?: unknown; entityKey?: unknown; entityType?: unknown };
    if (typeof t.label === "string") return t.label;
    if (typeof t.entityKey === "string") return t.entityKey;
    if (typeof t.entityType === "string") return t.entityType;
  }
  return "—";
}

/** 操作者展示文本：显示名（认证来源）。 */
export function actorText(actor: unknown, t: (key: string) => string): string {
  if (!actor || typeof actor !== "object") return t("auditWorkbench.actorSystem");
  const a = actor as {
    displayName?: unknown;
    authSource?: unknown;
    actorUsername?: unknown;
    actorAuthSource?: unknown;
  };
  const rawName =
    typeof a.displayName === "string" && a.displayName
      ? a.displayName
      : typeof a.actorUsername === "string" && a.actorUsername
        ? a.actorUsername
        : "";
  const name = rawName || t("auditWorkbench.actorSystem");
  const sourceKey = authSourceLabelKey(a.authSource ?? a.actorAuthSource);
  return sourceKey ? `${name}（${t(sourceKey)}）` : name;
}

/** 审计动作 → i18n 键；未知动作回退显示原始契约值。 */
export const ACTION_LABEL_KEYS: Record<string, string> = {
  "auth.login": "auditAction.authLogin",
  "auth.login_rejected": "auditAction.authLoginRejected",
  "auth.logout": "auditAction.authLogout",
  "asset.put": "auditAction.assetPut",
  "asset.upload": "auditAction.assetPut",
  "asset.operation": "auditAction.assetOperation",
  "asset.delete": "auditAction.assetDelete",
  "asset.download": "auditAction.assetDownload",
  "repo.create": "auditAction.repoCreate",
  "repo.update": "auditAction.repoUpdate",
  "repo.delete": "auditAction.repoDelete",
  "replication.apply": "auditAction.replicationApply",
  "replication.trigger": "auditAction.replicationTrigger",
  "setting.update": "auditAction.settingUpdate",
  "security.policy_rejected": "auditAction.securityPolicyRejected",
  "token.create": "auditAction.tokenCreate",
  "token.delete": "auditAction.tokenDelete",
  "user.create": "auditAction.userCreate",
  "user.update": "auditAction.userUpdate",
  "user.delete": "auditAction.userDelete",
};

export function actionLabel(action: unknown, t: (key: string) => string): string {
  const value = typeof action === "string" ? action.trim() : "";
  if (!value) return "—";
  const key = ACTION_LABEL_KEYS[value];
  return key ? t(key) : value;
}

/** 完整时间：2026/9/10 16:37:19（审计列表主时间列）。 */
export function formatFullTime(value: unknown): string {
  const date = new Date(typeof value === "string" ? value : "");
  if (Number.isNaN(date.getTime())) return "—";
  const pad = (input: number) => String(input).padStart(2, "0");
  return (
    `${date.getFullYear()}/${date.getMonth() + 1}/${date.getDate()} ` +
    `${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}`
  );
}

/** 耗时：907 ms / 1.24 s。 */
export function formatDuration(ms: unknown): string {
  if (typeof ms !== "number" || !Number.isFinite(ms) || ms < 0) return "—";
  return ms < 1000 ? `${Math.round(ms)} ms` : `${(ms / 1000).toFixed(2)} s`;
}

/** 操作者邮箱（缺失回退显示名）。 */
export function actorEmail(actor: unknown): string {
  const record = (actor ?? {}) as { email?: unknown; displayName?: unknown };
  const email = typeof record.email === "string" ? record.email : "";
  if (email) return email;
  return typeof record.displayName === "string" ? record.displayName : "—";
}

/** 认证方式展示（缺失/未知回退原值）。 */
export function authSourceText(source: unknown, t: (key: string) => string): string {
  const value = typeof source === "string" ? source : "";
  if (!value) return "—";
  const key = authSourceLabelKey(value);
  return key ? t(key) : value;
}

/** HTTP 请求方法（大写展示）。 */
export function requestMethod(method: unknown): string {
  return typeof method === "string" && method ? method.toUpperCase() : "—";
}

export function parseTime(value: unknown): number {
  // 统一走 UTC 解析（兼容 SQLite naive 与 RFC3339 输入），避免 naive 串被当本地时间。
  const date = typeof value === "string" ? parseUtc(value) : null;
  return date ? date.getTime() : 0;
}

const timeFormatter = new Intl.DateTimeFormat("zh-CN", {
  dateStyle: "medium",
  timeStyle: "short",
});

export function formatTime(value: unknown): string {
  const t = parseTime(value);
  return t ? timeFormatter.format(new Date(t)) : "—";
}

const clockFormatter = new Intl.DateTimeFormat("zh-CN", {
  hour: "2-digit",
  minute: "2-digit",
  second: "2-digit",
  hour12: false,
});

/**
 * 只取时刻（hh:mm:ss）。窄屏审计表的时间列只有 ~78px，带日期会折成三行；
 * 日期在分区标题的时间范围里已经表达过，这里只留时刻。
 */
export function formatClock(value: unknown): string {
  const t = parseTime(value);
  return t ? clockFormatter.format(new Date(t)) : "—";
}

const sameDay = (a: Date, b: Date) =>
  a.getFullYear() === b.getFullYear() &&
  a.getMonth() === b.getMonth() &&
  a.getDate() === b.getDate();

const shortTime = new Intl.DateTimeFormat("zh-CN", { hour: "2-digit", minute: "2-digit" });
const shortDate = new Intl.DateTimeFormat("zh-CN", { month: "numeric", day: "numeric" });
const fullDate = new Intl.DateTimeFormat("zh-CN", {
  year: "numeric",
  month: "numeric",
  day: "numeric",
});

/** 紧凑时间：今天只显示时分，今年省略年份，跨年补年份。 */
export function formatShortTime(value: unknown): string {
  const t = parseTime(value);
  if (!t) return "—";
  const date = new Date(t);
  const now = new Date();
  if (sameDay(date, now)) return shortTime.format(date);
  if (date.getFullYear() === now.getFullYear()) return shortDate.format(date);
  return fullDate.format(date);
}
