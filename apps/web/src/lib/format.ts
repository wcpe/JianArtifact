// 通用数值格式化（图表坐标轴 / KPI / 悬停读数共用）。
import { parseUtc } from "./timeFormat";

/**
 * 人类可读文件大小（B/KB/MB/GB/TB，一位小数）。
 * 负值（如区间变动为下降）先取绝对值再格式化，最后补回负号。
 */
export function formatBytes(bytes: number): string {
  const sign = bytes < 0 ? "-" : "";
  const size = Math.abs(bytes);
  if (size < 1024) {
    return `${sign}${size} B`;
  }
  const units = ["KB", "MB", "GB", "TB"];
  let value = size / 1024;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit += 1;
  }
  return `${sign}${value.toFixed(1)} ${units[unit]}`;
}

/** 千分位整数（计数型指标）。 */
export function formatCount(value: number): string {
  return value.toLocaleString("zh-CN");
}

/**
 * 短时间戳（列表行 / 图表刻度共用）：
 * 当天只显示 HH:mm，跨天显示「M/d HH:mm」，避免一排重复的同款标签。
 * 注意必须同时给 hour 与 minute——只给 hour 时 zh-CN 会输出「16时」而不是「16:00」。
 */
export function formatStamp(iso: string): string {
  // 后端时间可能是 SQLite naive UTC（YYYY-MM-DD HH:MM:SS）或 RFC3339，统一解析。
  const date = parseUtc(iso);
  if (!date) return "—";
  const sameDay = date.toDateString() === new Date().toDateString();
  return sameDay
    ? date.toLocaleTimeString("zh-CN", { hour: "2-digit", minute: "2-digit" })
    : date.toLocaleString("zh-CN", {
        month: "numeric",
        day: "numeric",
        hour: "2-digit",
        minute: "2-digit",
      });
}
