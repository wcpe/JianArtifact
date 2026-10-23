// 仓库详情页的「上游健康」区块（FR-159）。
//
// 定位：它是 FR-114「当前连接状态」的**历史补充**，不替代那块六态徽章——
// 上面回答「此刻通不通」，这里回答「过去一段时间通不通、失败分别是什么原因」。
//
// 四条界面约束（规格 §2.4，逐条对应实现）：
//  1. request 与 probe 必须可区分：来源切换器 + 各自一张率值卡片，
//     绝不把两条序列平均成一条曲线（「探测 100% 成功但真实请求 60%」是必须被看见的信号）；
//  2. 空态必须区分「尚无采样」与「一直健康」：无采样时显示「尚无采样」+ 解释，
//     绝不显示 100%——把 0 样本算成满连通是最容易误导运维的一种错；
//  3. 窄屏降级单列（上下堆叠），趋势图与明细表横向可滚动；
//  4. 明细表必须说明「分钟桶聚合」口径，避免被误读成逐请求日志。
import {
  Alert,
  Badge,
  Box,
  Card,
  Grid,
  Group,
  ScrollArea,
  SegmentedControl,
  SimpleGrid,
  Stack,
  Table,
  Text,
  ThemeIcon,
} from "@mantine/core";
import { useMediaQuery } from "@mantine/hooks";
import { IconChartLine, IconHistory } from "@tabler/icons-react";
import { useMemo, useState } from "react";
import { useTranslation } from "react-i18next";

import { getUpstreamHealth, getUpstreamHealthRecent } from "../../api/endpoints";
import type {
  UpstreamHealthPoint,
  UpstreamHealthSourceValue,
  UpstreamHealthSummary,
} from "../../api/types";
import { useAsync } from "../../hooks/useAsync";
import { formatCount, formatStamp } from "../../lib/format";
import { density } from "../../theme/density";
import { AsyncBoundary } from "../AsyncBoundary";
import { DashboardRangePicker, type DashboardRange } from "../observability/DashboardRangePicker";
import { TrendChart } from "../observability/TrendChart";

/** 明细表条数上限：与契约 maximum 一致，够看清最近一段而不把页面拉长。 */
const RECENT_LIMIT = 50;

/** 明细表最小宽度：窄屏靠横向滚动，而不是把列挤成一团。 */
const RECENT_TABLE_MIN_WIDTH = 520;

/** 窄屏趋势图最小宽度：低于它时折线会挤成噪声，改为横向滚动查看。 */
const TREND_MIN_WIDTH = 520;

/** 失败原因在分布里的展示顺序（与 FR-158 的分类一一对应）。 */
const FAILURE_COLUMNS = [
  { key: "timeout", labelKey: "repoDetail.upstreamHealthFailureTimeout", color: "orange" },
  { key: "unreachable", labelKey: "repoDetail.upstreamHealthFailureUnreachable", color: "red" },
  { key: "slow", labelKey: "repoDetail.upstreamHealthFailureSlow", color: "yellow" },
  { key: "aborted", labelKey: "repoDetail.upstreamHealthFailureAborted", color: "grape" },
] as const;

/** 默认窗口：最近 24 小时（与端点的缺省一致）。 */
function defaultRange(): DashboardRange {
  const now = new Date();
  return {
    preset: "24h",
    from: new Date(now.getTime() - 86_400_000).toISOString(),
    to: now.toISOString(),
  };
}

/** 0~1 的连通率格式化为百分比；null / undefined 表示无采样，由调用方改显示「尚无采样」。 */
function formatRate(value: number | null | undefined): string | null {
  if (value === null || value === undefined) return null;
  return `${(value * 100).toFixed(1)}%`;
}

/** 单个分钟的连通率（0~1）；该桶 samples 为 0 时不出现，这里只做防御性除法。 */
function pointRate(point: UpstreamHealthPoint): number {
  if (!point.samples) return 0;
  return point.ok / point.samples;
}

/** 失败类别的键集合（与 FAILURE_COLUMNS 的 key 一一对应）。 */
type FailureKey = (typeof FAILURE_COLUMNS)[number]["key"];

/** 四类失败计数（顺序与 FAILURE_COLUMNS 对齐）。summary 为 null 时全 0。 */
function failureCounts(summary: UpstreamHealthSummary | null): Record<FailureKey, number> {
  return {
    timeout: summary?.timeout ?? 0,
    unreachable: summary?.unreachable ?? 0,
    slow: summary?.slow ?? 0,
    aborted: summary?.aborted ?? 0,
  };
}

export function UpstreamHealthPanel({ repoName }: { repoName: string }) {
  const { t } = useTranslation();
  const isNarrow = useMediaQuery("(max-width: 48em)") ?? false;
  const [range, setRange] = useState<DashboardRange>(defaultRange);
  // 来源切换：默认看「真实请求」——它才是用户实际体验；探测序列是第二个视图，不叠加平均。
  const [source, setSource] = useState<UpstreamHealthSourceValue>("request");

  const seriesState = useAsync(
    () => getUpstreamHealth(repoName, { from: range.from, to: range.to }),
    [repoName, range.from, range.to],
    { cacheKey: `repo:upstream-health:${repoName}:${range.from}:${range.to}` },
  );
  const recentState = useAsync(() => getUpstreamHealthRecent(repoName, RECENT_LIMIT), [repoName], {
    cacheKey: `repo:upstream-health-recent:${repoName}`,
  });

  const series = seriesState.data;
  // 汇总按来源分别取：缺失某个来源的键表示该来源在窗口内没有采样。
  const summary = series?.summaries?.[source] ?? null;
  const points = useMemo(
    () => (series?.points ?? []).filter((point) => point.source === source),
    [series, source],
  );
  // 趋势点：连通率 ×100 后交给 TrendChart 的 percent 单位（图例与悬停读数都读百分比）。
  const trendPoints = useMemo(
    () =>
      points.map((point) => ({
        label: formatStamp(point.bucketStart),
        value: pointRate(point) * 100,
      })),
    [points],
  );

  // 「尚无采样」的唯一判据是 samples 缺失或为 0，而不是 connectivity==0：
  // 后者可能是「有样本但全失败」，两者的排查方向完全不同。
  const hasSamples = Boolean(summary && summary.samples > 0);
  const hasAnySamplesInWindow = (series?.points ?? []).length > 0;
  const sourceLabel =
    source === "request"
      ? t("repoDetail.upstreamHealthSourceRequest")
      : t("repoDetail.upstreamHealthSourceProbe");
  const rate = formatRate(summary?.connectivity);

  return (
    <Card withBorder padding={density.cardPadding} radius="md" data-testid="repo-upstream-health">
      <Stack gap="md">
        <Group justify="space-between" align="center" wrap="wrap" gap="xs">
          <Group gap="xs" align="center">
            <ThemeIcon variant="light" size="sm" color="teal">
              <IconChartLine size={14} />
            </ThemeIcon>
            <Text size="sm" fw={600}>
              {t("repoDetail.upstreamHealthTitle")}
            </Text>
          </Group>
          <DashboardRangePicker value={range} onChange={setRange} />
        </Group>

        <Text size="xs" c="dimmed">
          {t("repoDetail.upstreamHealthHint")}
        </Text>

        <SegmentedControl
          size="xs"
          value={source}
          onChange={(value) => setSource(value as UpstreamHealthSourceValue)}
          data={[
            { value: "request", label: t("repoDetail.upstreamHealthSourceRequest") },
            { value: "probe", label: t("repoDetail.upstreamHealthSourceProbe") },
          ]}
        />

        <AsyncBoundary state={seriesState}>
          {() =>
            hasSamples ? (
              <Stack gap="md">
                {/* 率值卡片：连通率与延迟各自一格；连通率绝不在无采样时显示数字。 */}
                <SimpleGrid cols={isNarrow ? 1 : 2} spacing={density.gridSpacing}>
                  <RateCard
                    label={t("repoDetail.upstreamHealthConnectivity")}
                    value={rate ?? t("repoDetail.upstreamHealthNoSamples")}
                    detail={t("repoDetail.upstreamHealthSamplesCount", {
                      count: formatCount(summary?.samples ?? 0),
                    })}
                  />
                  <RateCard
                    label={t("repoDetail.upstreamHealthAvgLatency")}
                    value={
                      summary?.avgLatencyMs == null
                        ? t("repoDetail.upstreamHealthLatencyUnknown")
                        : `${formatCount(summary.avgLatencyMs)} ms`
                    }
                    detail={
                      summary?.p95LatencyMs == null
                        ? undefined
                        : t("repoDetail.upstreamHealthP95Detail", {
                            value: formatCount(summary.p95LatencyMs),
                          })
                    }
                  />
                </SimpleGrid>

                <Grid>
                  {/* 趋势图与失败分布：宽屏并排、窄屏单列。 */}
                  <Grid.Col span={{ base: 12, md: 8 }}>
                    <ScrollArea type="auto" scrollbars="x" offsetScrollbars>
                      <Box miw={isNarrow ? TREND_MIN_WIDTH : undefined}>
                        <TrendChart
                          title={t("repoDetail.upstreamHealthTrendTitle")}
                          summary={t("repoDetail.upstreamHealthTrendSummary", {
                            source: sourceLabel,
                            points: formatCount(trendPoints.length),
                          })}
                          primary={trendPoints}
                          primaryLabel={sourceLabel}
                          unit="percent"
                          yDomain={[0, 100]}
                        />
                      </Box>
                    </ScrollArea>
                  </Grid.Col>
                  <Grid.Col span={{ base: 12, md: 4 }}>
                    <FailureBreakdown summary={summary} />
                  </Grid.Col>
                </Grid>
              </Stack>
            ) : (
              // 空态：明确区分「尚无采样」与「一直健康」。
              // 窗口内有另一来源的采样时，说明「该来源没有采样」而不是「完全没有采样」——
              // 两句话指向的排查方向不同（前者是这条链路没跑，后者是整块刚启用）。
              <Alert
                color="gray"
                variant="light"
                icon={<IconHistory size={16} />}
                data-testid="repo-upstream-health-empty"
              >
                <Stack gap={2}>
                  <Text size="sm" fw={600}>
                    {t("repoDetail.upstreamHealthNoSamples")}
                  </Text>
                  <Text size="xs" c="dimmed">
                    {hasAnySamplesInWindow
                      ? t("repoDetail.upstreamHealthNoSamplesForSource")
                      : t("repoDetail.upstreamHealthNoSamplesHint")}
                  </Text>
                </Stack>
              </Alert>
            )
          }
        </AsyncBoundary>

        <RecentTable state={recentState} isNarrow={isNarrow} />
      </Stack>
    </Card>
  );
}

/** 一格率值卡片：大数字 + 说明。空值必须由调用方传入「尚无采样」文案，绝不显示 0%。 */
function RateCard({ label, value, detail }: { label: string; value: string; detail?: string }) {
  return (
    <Card withBorder padding="sm" radius="sm">
      <Stack gap={2}>
        <Text size="xs" c="dimmed">
          {label}
        </Text>
        <Text size="lg" fw={700}>
          {value}
        </Text>
        {detail ? (
          <Text size="xs" c="dimmed">
            {detail}
          </Text>
        ) : null}
      </Stack>
    </Card>
  );
}

/** 失败原因分布：四类计数 + 占比条。全为 0 时明确写「窗口内没有失败样本」。 */
function FailureBreakdown({ summary }: { summary: UpstreamHealthSummary | null }) {
  const { t } = useTranslation();
  const counts = failureCounts(summary);
  const total = FAILURE_COLUMNS.reduce((sum, column) => sum + counts[column.key], 0);
  return (
    <Card withBorder padding="sm" radius="sm" h="100%" data-testid="repo-upstream-health-failures">
      <Stack gap="xs">
        <Text size="sm" fw={600}>
          {t("repoDetail.upstreamHealthFailureTitle")}
        </Text>
        <Text size="xs" c="dimmed">
          {t("repoDetail.upstreamHealthFailureHint")}
        </Text>
        {total === 0 ? (
          <Text size="sm" c="dimmed">
            {t("repoDetail.upstreamHealthFailureNone")}
          </Text>
        ) : (
          FAILURE_COLUMNS.map((column) => {
            const count = counts[column.key];
            const ratio = total > 0 ? count / total : 0;
            return (
              <Stack key={column.key} gap={4}>
                <Group justify="space-between" gap="xs" wrap="nowrap">
                  <Text size="xs">{t(String(column.labelKey))}</Text>
                  <Text size="xs" fw={600}>
                    {formatCount(count)}
                  </Text>
                </Group>
                <Box
                  h={6}
                  style={{
                    borderRadius: 3,
                    background: "var(--mantine-color-gray-2)",
                    overflow: "hidden",
                  }}
                >
                  <Box
                    h="100%"
                    w={`${Math.round(ratio * 100)}%`}
                    bg={`var(--mantine-color-${column.color}-6)`}
                  />
                </Box>
              </Stack>
            );
          })
        )}
      </Stack>
    </Card>
  );
}

/**
 * 最近采样明细表：分钟桶摘要（同一分钟多次请求合并为一行）。
 *
 * 加载态只占位一次（首载），刷新时保留旧表——与 AsyncBoundary 的「刷新手感」一致，
 * 避免每 5 秒轮询把表格闪成骨架。
 */
function RecentTable({
  state,
  isNarrow,
}: {
  state: ReturnType<typeof useAsync<Awaited<ReturnType<typeof getUpstreamHealthRecent>>>>;
  isNarrow: boolean;
}) {
  const { t } = useTranslation();
  const items = state.data?.items ?? [];
  return (
    <Stack gap="xs" data-testid="repo-upstream-health-recent">
      <Group gap="xs" align="center">
        <ThemeIcon variant="light" size="sm" color="gray">
          <IconHistory size={14} />
        </ThemeIcon>
        <Text size="sm" fw={600}>
          {t("repoDetail.upstreamHealthRecentTitle")}
        </Text>
      </Group>
      <Text size="xs" c="dimmed">
        {t("repoDetail.upstreamHealthRecentHint")}
      </Text>
      <AsyncBoundary state={state}>
        {() =>
          items.length === 0 ? (
            <Text size="sm" c="dimmed">
              {t("repoDetail.upstreamHealthRecentEmpty")}
            </Text>
          ) : (
            <ScrollArea type="auto" scrollbars="x" offsetScrollbars>
              <Table striped highlightOnHover miw={isNarrow ? RECENT_TABLE_MIN_WIDTH : undefined}>
                <Table.Thead>
                  <Table.Tr>
                    <Table.Th>{t("repoDetail.upstreamHealthColTime")}</Table.Th>
                    <Table.Th>{t("repoDetail.upstreamHealthColSource")}</Table.Th>
                    <Table.Th>{t("repoDetail.upstreamHealthColResult")}</Table.Th>
                    <Table.Th>{t("repoDetail.upstreamHealthColSamples")}</Table.Th>
                    <Table.Th>{t("repoDetail.upstreamHealthColLatency")}</Table.Th>
                  </Table.Tr>
                </Table.Thead>
                <Table.Tbody>
                  {items.map((item) => {
                    const failed = item.timeout + item.unreachable + item.slow + item.aborted;
                    return (
                      <Table.Tr
                        key={`${item.bucketStart}-${item.source}`}
                        data-testid={`upstream-health-row-${item.bucketStart}-${item.source}`}
                      >
                        <Table.Td>
                          <Text size="xs">{formatStamp(item.bucketStart)}</Text>
                        </Table.Td>
                        <Table.Td>
                          <Badge
                            size="xs"
                            variant="light"
                            color={item.source === "request" ? "blue" : "grape"}
                          >
                            {item.source === "request"
                              ? t("repoDetail.upstreamHealthSourceRequest")
                              : t("repoDetail.upstreamHealthSourceProbe")}
                          </Badge>
                        </Table.Td>
                        <Table.Td>
                          {failed === 0 ? (
                            <Text size="xs" c="teal">
                              {t("repoDetail.upstreamHealthResultAllOk")}
                            </Text>
                          ) : (
                            <Text size="xs" c="red">
                              {t("repoDetail.upstreamHealthResultFailed", {
                                count: formatCount(failed),
                              })}
                            </Text>
                          )}
                        </Table.Td>
                        <Table.Td>
                          <Text size="xs">{formatCount(item.samples)}</Text>
                        </Table.Td>
                        <Table.Td>
                          <Text size="xs" c={item.avgLatencyMs == null ? "dimmed" : undefined}>
                            {item.avgLatencyMs == null
                              ? t("repoDetail.upstreamHealthLatencyUnknown")
                              : `${formatCount(item.avgLatencyMs)} ms`}
                          </Text>
                        </Table.Td>
                      </Table.Tr>
                    );
                  })}
                </Table.Tbody>
              </Table>
            </ScrollArea>
          )
        }
      </AsyncBoundary>
    </Stack>
  );
}
