// 监控指标元数据（FR-99）：集中定义 FR-105 时序查询的 10 个指标键、显示名、所属分类与值格式，
// 以及监控页的时间范围档位。作为前端侧单一真源，避免指标键 / 分类散落成魔法串。
//
// 指标键严格对齐后端 FR-105（docs/API.md GET /api/v1/monitor/metrics 的「指标键」清单）。
// 缓存命中率本期后端未采（待埋点），作为「已知但无数据源」的占位条目收录，渲染走空态。

import { formatBytes } from './format';

/** 指标所属分类（用于监控页分类切换过滤）。 */
export type MetricCategory = 'host' | 'usage' | 'protection' | 'cache' | 'storage';

/** 指标值格式：百分比 / 字节 / 计数，决定 KPI 与折线当前值如何展示。 */
export type MetricValueFormat = 'percent' | 'bytes' | 'count';

/** 单个指标的元数据。 */
export interface MetricMeta {
  /** 后端指标键（对齐 FR-105）。 */
  key: string;
  /** 中文显示名。 */
  label: string;
  /** 所属分类。 */
  category: MetricCategory;
  /** 值格式。 */
  format: MetricValueFormat;
  /**
   * 本期是否有后端数据源。false 表示后端尚未采集（如缓存命中率），
   * 页面据此对其展示「暂无数据」占位、不发查询。
   */
  available: boolean;
}

/**
 * FR-105 指标清单（单一真源）。
 * 顺序即监控页默认展示顺序。
 */
export const METRICS: readonly MetricMeta[] = [
  // 主机
  {
    key: 'host.cpu_percent',
    label: 'CPU 使用率',
    category: 'host',
    format: 'percent',
    available: true,
  },
  {
    key: 'host.memory_percent',
    label: '内存使用率',
    category: 'host',
    format: 'percent',
    available: true,
  },
  {
    key: 'host.disk_percent',
    label: '磁盘使用率',
    category: 'host',
    format: 'percent',
    available: true,
  },
  // 存储 / 仓库
  {
    key: 'storage.repo_count',
    label: '仓库数',
    category: 'storage',
    format: 'count',
    available: true,
  },
  {
    key: 'storage.blob_count',
    label: '去重 blob 数',
    category: 'storage',
    format: 'count',
    available: true,
  },
  {
    key: 'storage.total_bytes',
    label: '存储用量',
    category: 'storage',
    format: 'bytes',
    available: true,
  },
  // 防护
  {
    key: 'protection.active_bans',
    label: '活跃封禁 IP',
    category: 'protection',
    format: 'count',
    available: true,
  },
  {
    key: 'protection.rate_limited_total',
    label: '限流累计被拒',
    category: 'protection',
    format: 'count',
    available: true,
  },
  // 使用分析
  {
    key: 'usage.access_total',
    label: '累计访问量',
    category: 'usage',
    format: 'count',
    available: true,
  },
  {
    key: 'usage.download_total',
    label: '累计下载量',
    category: 'usage',
    format: 'count',
    available: true,
  },
  // 缓存（本期后端未采，占位走空态）
  {
    key: 'cache.hit_ratio',
    label: '缓存命中率',
    category: 'cache',
    format: 'percent',
    available: false,
  },
] as const;

/** 分类切换选项（含「全部」聚合视图）。 */
export type MetricCategoryFilter = 'all' | MetricCategory;

/** 分类切换的展示选项（值 + 中文标签）。 */
export const CATEGORY_OPTIONS: readonly { value: MetricCategoryFilter; label: string }[] = [
  { value: 'all', label: '全部' },
  { value: 'host', label: '主机' },
  { value: 'usage', label: '使用分析' },
  { value: 'protection', label: '防护' },
  { value: 'cache', label: '缓存' },
  { value: 'storage', label: '存储仓库' },
] as const;

/** 按分类过滤指标（「全部」返回全集）。 */
export function metricsForCategory(category: MetricCategoryFilter): MetricMeta[] {
  if (category === 'all') {
    return [...METRICS];
  }
  return METRICS.filter((m) => m.category === category);
}

/** 时间范围档位键。 */
export type TimeRangeKey = '1h' | '24h' | '7d';

/** 单个时间范围档位：窗口跨度与建议降采样步长（均毫秒；step=0 表示不降采样）。 */
export interface TimeRange {
  key: TimeRangeKey;
  label: string;
  /** 窗口跨度（毫秒），用于据「现在」推算 from。 */
  rangeMs: number;
  /** 建议降采样步长（毫秒），0 = 不降采样、返回原始点。 */
  stepMs: number;
}

const MINUTE_MS = 60 * 1000;
const HOUR_MS = 60 * MINUTE_MS;
const DAY_MS = 24 * HOUR_MS;

/** 时间范围档位（顺序即切换控件顺序）。 */
export const TIME_RANGES: readonly TimeRange[] = [
  { key: '1h', label: '1 小时', rangeMs: HOUR_MS, stepMs: 0 },
  { key: '24h', label: '24 小时', rangeMs: DAY_MS, stepMs: 5 * MINUTE_MS },
  { key: '7d', label: '7 天', rangeMs: 7 * DAY_MS, stepMs: HOUR_MS },
] as const;

/** 取某档位定义（未知键回落首档）。 */
export function timeRange(key: TimeRangeKey): TimeRange {
  return TIME_RANGES.find((r) => r.key === key) ?? TIME_RANGES[0];
}

/** 把某档位换算为 GET /monitor/metrics 的查询参数（基于传入的「现在」时刻，便于测试可控）。 */
export function rangeToQuery(
  range: TimeRange,
  now: number,
): { from: number; to: number; step: number } {
  return { from: now - range.rangeMs, to: now, step: range.stepMs };
}

/** 按指标格式把数值渲染为展示串。 */
export function formatMetricValue(value: number, format: MetricValueFormat): string {
  switch (format) {
    case 'percent':
      return `${Math.round(value)}%`;
    case 'bytes':
      return formatBytes(value);
    case 'count':
      // 计数取整展示（采样落库为标量 REAL，计数语义下应为整数）
      return `${Math.round(value)}`;
    default:
      return `${value}`;
  }
}
