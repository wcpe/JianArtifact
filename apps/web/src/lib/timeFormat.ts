// 后端时间统一渲染入口：
// 后端 SQLite `datetime('now')` 存的是 UTC 字符串，格式 `YYYY-MM-DD HH:MM:SS`
// （无时区后缀），直传前端；另一部分字段（审计/可观测）是 Go time.Time 序列化的
// RFC3339（带 Z/偏移）。此处把两类输入统一解析为 Date，再按浏览器本地时区格式化。

/** 匹配无时区后缀的 naive 时间：`YYYY-MM-DD HH:MM:SS`（空格或 T，可带毫秒）。 */
const NAIVE_DATE_TIME =
  /^(\d{4}-\d{2}-\d{2})[ T](\d{2}:\d{2}:\d{2})(?:\.(\d+))?$/;

/**
 * 解析后端时间字符串为 Date。
 * - naive `YYYY-MM-DD HH:MM:SS`（无时区）：按 **UTC** 解析（后端 datetime('now') 语义）。
 * - 已带时区（RFC3339 `...Z` / `...+08:00`）：按原样解析。
 * - 空 / 非法输入返回 null。
 */
export function parseUtc(value: string | null | undefined): Date | null {
  if (typeof value !== "string") return null;
  const trimmed = value.trim();
  if (!trimmed) return null;
  const naive = NAIVE_DATE_TIME.exec(trimmed);
  const date = naive
    ? new Date(`${naive[1]}T${naive[2]}${naive[3] ? `.${naive[3]}` : ""}Z`)
    : new Date(trimmed);
  return Number.isNaN(date.getTime()) ? null : date;
}

const pad = (input: number) => String(input).padStart(2, "0");

/**
 * 完整时间（浏览器本地时区）：`YYYY-MM-DD HH:mm:ss`。
 * 空值返回空串（与调用方既有 `value || "—"` 的空值处理兼容）；非法输入原样返回。
 */
export function formatUtcToLocal(value: string | null | undefined): string {
  const date = parseUtc(value);
  if (!date) return typeof value === "string" ? value : "";
  return (
    `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())} ` +
    `${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}`
  );
}

/** 日期粒度（浏览器本地时区）：`YYYY-MM-DD`。空值返回空串；非法输入原样返回。 */
export function formatUtcToLocalDate(value: string | null | undefined): string {
  const date = parseUtc(value);
  if (!date) return typeof value === "string" ? value : "";
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}`;
}
