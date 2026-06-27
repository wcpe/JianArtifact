// 监控指标元数据测试（FR-99）：分类过滤、时间范围换算、值格式化。

import { describe, it, expect } from 'vitest';
import {
  METRICS,
  metricsForCategory,
  rangeToQuery,
  timeRange,
  formatMetricValue,
  TIME_RANGES,
} from './metrics';

describe('metrics 元数据', () => {
  it('收录 FR-105 的 10 个有数据源指标 + 缓存命中率占位', () => {
    const available = METRICS.filter((m) => m.available);
    expect(available).toHaveLength(10);
    // 缓存命中率为已知但本期无数据源的占位
    const cacheHit = METRICS.find((m) => m.key === 'cache.hit_ratio');
    expect(cacheHit?.available).toBe(false);
    expect(cacheHit?.category).toBe('cache');
  });

  it('指标键对齐后端 FR-105 契约', () => {
    const keys = METRICS.map((m) => m.key);
    expect(keys).toContain('host.cpu_percent');
    expect(keys).toContain('storage.total_bytes');
    expect(keys).toContain('protection.rate_limited_total');
    expect(keys).toContain('usage.download_total');
  });
});

describe('metricsForCategory', () => {
  it('「全部」返回全集', () => {
    expect(metricsForCategory('all')).toHaveLength(METRICS.length);
  });

  it('「主机」只返回 host.* 指标', () => {
    const host = metricsForCategory('host');
    expect(host.every((m) => m.category === 'host')).toBe(true);
    expect(host.map((m) => m.key)).toEqual([
      'host.cpu_percent',
      'host.memory_percent',
      'host.disk_percent',
    ]);
  });

  it('「防护」只返回 protection.* 指标', () => {
    const prot = metricsForCategory('protection');
    expect(prot.every((m) => m.category === 'protection')).toBe(true);
    expect(prot).toHaveLength(2);
  });

  it('「缓存」返回占位指标（无数据源）', () => {
    const cache = metricsForCategory('cache');
    expect(cache).toHaveLength(1);
    expect(cache[0].available).toBe(false);
  });
});

describe('rangeToQuery', () => {
  it('据「现在」推算 from/to/step（1h 不降采样）', () => {
    const now = 1_000_000_000_000;
    const q = rangeToQuery(timeRange('1h'), now);
    expect(q.to).toBe(now);
    expect(q.from).toBe(now - 60 * 60 * 1000);
    expect(q.step).toBe(0);
  });

  it('7d 档给非零降采样步长', () => {
    const now = 2_000_000_000_000;
    const q = rangeToQuery(timeRange('7d'), now);
    expect(q.from).toBe(now - 7 * 24 * 60 * 60 * 1000);
    expect(q.step).toBeGreaterThan(0);
  });

  it('不同档位的 from/step 各不相同', () => {
    const now = 1_700_000_000_000;
    const froms = TIME_RANGES.map((r) => rangeToQuery(r, now).from);
    // 三档窗口跨度不同 → from 互不相等
    expect(new Set(froms).size).toBe(TIME_RANGES.length);
  });
});

describe('formatMetricValue', () => {
  it('百分比取整加 %', () => {
    expect(formatMetricValue(42.7, 'percent')).toBe('43%');
  });

  it('字节走 formatBytes', () => {
    expect(formatMetricValue(1024, 'bytes')).toBe('1.00 KB');
  });

  it('字节型降采样平均产物取整、不渲染原始浮点', () => {
    // 监控页字节 gauge 经降采样平均后为小数，展示须取整且带单位
    const out = formatMetricValue(121.33333333333333, 'bytes');
    expect(out).toBe('121 B');
    expect(out).not.toMatch(/\d\.\d{3,}/);
  });

  it('计数取整', () => {
    expect(formatMetricValue(7.0, 'count')).toBe('7');
  });
});
