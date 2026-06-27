// 通用展示辅助：体积格式化、错误文案提取。

import { ApiError } from '../api/client';

/** 把字节数格式化为人类可读体积。 */
export function formatBytes(bytes: number): string {
  // 字节为整数语义；降采样平均可能产生小数浮点，取整避免渲染原始浮点（如 121.33333333333333）
  if (bytes < 1024) return `${Math.round(bytes)} B`;
  const units = ['KB', 'MB', 'GB', 'TB'];
  let value = bytes / 1024;
  let unitIndex = 0;
  while (value >= 1024 && unitIndex < units.length - 1) {
    value /= 1024;
    unitIndex += 1;
  }
  return `${value.toFixed(2)} ${units[unitIndex]}`;
}

/** 把整数计数格式化为带千分位分隔的串（如 12345 → "12,345"）。 */
export function formatCount(count: number): string {
  return Math.round(count).toLocaleString('en-US');
}

/** 把秒数格式化为人类可读的运行时长（如 "3 天 4 小时"、"5 分钟"、"刚刚启动")。 */
export function formatUptime(seconds: number): string {
  const total = Math.max(0, Math.floor(seconds));
  const days = Math.floor(total / 86400);
  const hours = Math.floor((total % 86400) / 3600);
  const minutes = Math.floor((total % 3600) / 60);
  if (days > 0) return `${days} 天 ${hours} 小时`;
  if (hours > 0) return `${hours} 小时 ${minutes} 分钟`;
  if (minutes > 0) return `${minutes} 分钟`;
  return '刚刚启动';
}

/**
 * 把 ISO 时间串格式化为相对「现在」的中文相对时间（如 "3 分钟前"、"刚刚"）。
 * `now` 可注入便于测试；解析失败回退原串。
 */
export function formatRelativeTime(iso: string, now: number = Date.now()): string {
  const then = Date.parse(iso);
  if (Number.isNaN(then)) return iso;
  const diffSecs = Math.floor((now - then) / 1000);
  if (diffSecs < 60) return '刚刚';
  const minutes = Math.floor(diffSecs / 60);
  if (minutes < 60) return `${minutes} 分钟前`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours} 小时前`;
  const days = Math.floor(hours / 24);
  return `${days} 天前`;
}

/** 从任意错误中提取面向用户的中文文案。 */
export function errorMessage(err: unknown): string {
  if (err instanceof ApiError) {
    return err.message;
  }
  if (err instanceof Error) {
    return err.message;
  }
  return '发生未知错误';
}
