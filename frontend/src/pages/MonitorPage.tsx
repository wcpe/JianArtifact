// 监控总览页（FR-99 重设计）：跨域 KPI 指标行 + 多指标时序网格，仅 Admin。
//
// 顶部：分类切换（全部 / 主机 / 使用分析 / 防护 / 缓存 / 存储仓库）+ 时间范围切换（1h / 24h / 7d）。
// KPI 行：各指标在所选范围内的最新值（末点 value）。
// 时序网格：每指标一张卡（标题 + 手搓 SVG 折线 + 当前值），消费 FR-105 GET /api/v1/monitor/metrics，
// 悬停看某时间点取值。无数据源指标（如缓存命中率）优雅显示「暂无数据」。
// 数据本机内部、不外发。审计 / 使用分析 / 防护监控为各自独立页（不再 tab 化整合于此）。

import { useCallback, useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Stack, Title, Text, Group, Card, SimpleGrid, SegmentedControl } from '@mantine/core';
import * as api from '../api/endpoints';
import type { MetricPoint } from '../api/types';
import { errorMessage } from '../lib/format';
import {
  CATEGORY_OPTIONS,
  TIME_RANGES,
  formatMetricValue,
  metricsForCategory,
  rangeToQuery,
  timeRange,
  type MetricCategoryFilter,
  type MetricMeta,
  type TimeRangeKey,
} from '../lib/metrics';
import { LineChart } from '../components/charts/LineChart';

/** 单指标的取数状态（时序点 + 错误，loading 由 points/error 均空表达）。 */
interface SeriesState {
  points: MetricPoint[];
  error: string | null;
}

/** 取某指标 series 的末点值（无点返回 null）。 */
function lastValue(points: MetricPoint[]): number | null {
  return points.length > 0 ? points[points.length - 1].value : null;
}

/** 前台自动刷新周期（毫秒）：监控页停留期间按此周期重取时序，无须手动切类目 / 区间。 */
export const MONITOR_REFRESH_MS = 15_000;

/** 监控总览页。 */
export function MonitorPage() {
  const { t } = useTranslation('monitor');
  const [category, setCategory] = useState<MetricCategoryFilter>('all');
  const [rangeKey, setRangeKey] = useState<TimeRangeKey>('24h');
  // 各指标键 → 取数状态
  const [series, setSeries] = useState<Record<string, SeriesState>>({});

  const visibleMetrics = metricsForCategory(category);

  // 取数：对当前可见且有数据源的指标按当前时间范围并发查询；无数据源指标不发请求（走空态）。
  const load = useCallback(() => {
    const range = timeRange(rangeKey);
    const { from, to, step } = rangeToQuery(range, Date.now());
    const targets = metricsForCategory(category).filter((m) => m.available);

    targets.forEach((m) => {
      api
        .getMetricSeries(m.key, { from, to, step })
        .then((res) => {
          setSeries((prev) => ({ ...prev, [m.key]: { points: res.points, error: null } }));
        })
        .catch((err) => {
          setSeries((prev) => ({ ...prev, [m.key]: { points: [], error: errorMessage(err) } }));
        });
    });
  }, [category, rangeKey]);

  // 首次加载 + 按周期前台轮询自动刷新；依赖（分类 / 区间）变化时重建定时器、卸载时清理（防泄漏）
  useEffect(() => {
    load();
    const timer = setInterval(load, MONITOR_REFRESH_MS);
    return () => clearInterval(timer);
  }, [load]);

  return (
    <Stack>
      <Title order={2}>{t('title')}</Title>
      <Text c="dimmed" size="sm">
        {t('description')}
      </Text>

      <Group justify="space-between" wrap="wrap">
        <SegmentedControl
          aria-label={t('categoryAriaLabel')}
          value={category}
          onChange={(v) => setCategory(v as MetricCategoryFilter)}
          data={CATEGORY_OPTIONS.map((o) => ({ value: o.value, label: o.label }))}
        />
        <SegmentedControl
          aria-label={t('timeRangeAriaLabel')}
          value={rangeKey}
          onChange={(v) => setRangeKey(v as TimeRangeKey)}
          data={TIME_RANGES.map((r) => ({ value: r.key, label: r.label }))}
        />
      </Group>

      {/* KPI 指标行：各指标当前值（末点） */}
      <SimpleGrid cols={{ base: 2, sm: 3, md: 4 }} spacing="sm">
        {visibleMetrics.map((m) => (
          <KpiCard key={m.key} meta={m} state={series[m.key]} />
        ))}
      </SimpleGrid>

      {/* 多指标时序网格：每指标一张卡（标题 + 折线 + 当前值） */}
      <SimpleGrid cols={{ base: 1, sm: 2, lg: 3 }} spacing="sm">
        {visibleMetrics.map((m) => (
          <MetricChartCard key={m.key} meta={m} state={series[m.key]} />
        ))}
      </SimpleGrid>
    </Stack>
  );
}

/** KPI 卡：指标名 + 当前值（无数据 / 无数据源显「—」）。 */
function KpiCard({ meta, state }: { meta: MetricMeta; state: SeriesState | undefined }) {
  const value = state ? lastValue(state.points) : null;
  const display = !meta.available || value === null ? '—' : formatMetricValue(value, meta.format);
  return (
    <Card withBorder padding="sm" radius="md">
      <Text size="xs" c="dimmed">
        {meta.label}
      </Text>
      <Text fw={700} size="xl">
        {display}
      </Text>
    </Card>
  );
}

/** 时序卡：标题 + 手搓折线（悬停看点值）+ 当前值；无数据源 / 无点走空态。 */
function MetricChartCard({ meta, state }: { meta: MetricMeta; state: SeriesState | undefined }) {
  const { t } = useTranslation('monitor');
  const points = meta.available && state ? state.points : [];
  const value = lastValue(points);
  const emptyText = !meta.available ? t('emptyPending') : t('common:empty');
  return (
    <Card withBorder padding="md" radius="md">
      <Group justify="space-between" mb="xs">
        <Text fw={600} size="sm">
          {meta.label}
        </Text>
        <Text size="sm" c="dimmed">
          {value === null ? '—' : formatMetricValue(value, meta.format)}
        </Text>
      </Group>
      {state?.error ? (
        <Text c="red" size="sm">
          {state.error}
        </Text>
      ) : (
        <LineChart
          points={points}
          emptyText={emptyText}
          valueFormat={(v) => formatMetricValue(v, meta.format)}
        />
      )}
    </Card>
  );
}
