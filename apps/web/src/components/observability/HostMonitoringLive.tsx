// 主机监控（v0.8.2 监控台布局）：
// - 状态行：单个状态胶囊 + 采样/更新时间 + 范围控件（徽章不再重复三处）；
// - 健康时采样状态收成一条细状态条，仅 stale 才弹出完整警示 Alert；
// - 主区（8/12）：CPU 实时大图（当前值前置）→ 内存趋势；右辅栏（4/12）：节点状态（就绪 / goroutine / fd / RSS / 磁盘 / 采样时间）；
// - 底部：磁盘 / 网络 / 进程 3 图并排，不再出现孤行。
import {
  Alert,
  Badge,
  Button,
  Card,
  Divider,
  Grid,
  Group,
  Progress,
  SimpleGrid,
  Skeleton,
  Stack,
  Text,
  ThemeIcon,
} from "@mantine/core";
import {
  IconAlertTriangle,
  IconCircleCheck,
  IconCpu,
  IconRefresh,
  IconServer,
  IconStack2,
} from "@tabler/icons-react";
import { useCallback, useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";

import { getHostMonitoring } from "../../api/endpoints";
import type { HostMetricGroup, HostMetricPoint, HostMonitoring } from "../../api/types";
import { useAsync, useVisibleRefresh } from "../../hooks/useAsync";
import { formatBytes, formatCount, formatStamp } from "../../lib/format";
import { density } from "../../theme/density";
import { StatusPill } from "../ops/OpsKit";
import { PreviewRangeControls } from "./PreviewRangeControls";
import { TrendChart } from "./TrendChart";

type Range = "1h" | "6h" | "24h" | "7d" | "30d";

const rangeMillis: Record<Range, number> = {
  "1h": 3_600_000,
  "6h": 21_600_000,
  "24h": 86_400_000,
  "7d": 604_800_000,
  "30d": 2_592_000_000,
};

const rangeOptions: Array<{ value: Range; label: string }> = [
  { value: "1h", label: "近 1 小时" },
  { value: "6h", label: "近 6 小时" },
  { value: "24h", label: "近 24 小时" },
  { value: "7d", label: "近 7 天" },
  { value: "30d", label: "近 30 天" },
];

// 主机指标可能缺采样，统一用「有值走共享格式化、无值回退占位」的容错包装，
// 避免各地各写一份 formatBytes / formatCount（进位分支不一致会给出不同字符串）。
function bytesOr(value: number | null | undefined, fallback = ""): string {
  return typeof value === "number" ? formatBytes(value) : fallback;
}

function percentOr(value: number | null | undefined, fallback = ""): string {
  return typeof value === "number" ? `${value.toFixed(1)}%` : fallback;
}

function countOr(value: number | null | undefined, fallback = ""): string {
  return typeof value === "number" ? formatCount(value) : fallback;
}

type TFunction = (key: string, options?: Record<string, unknown>) => string;

function groupStatus(group: HostMetricGroup, t: TFunction): { label: string; color: string } {
  switch (group.state) {
    case "ok":
      return { label: t("hostMonitoring.groupOk"), color: "teal" };
    case "unsupported":
      return { label: t("hostMonitoring.groupUnsupported"), color: "gray" };
    case "unavailable":
      return { label: t("hostMonitoring.groupUnavailable"), color: "yellow" };
    default:
      return { label: t("hostMonitoring.groupError"), color: "red" };
  }
}

function hostStatus(
  value: HostMonitoring["hostState"],
  t: TFunction,
): { label: string; color: string } {
  switch (value) {
    case "healthy":
      return { label: t("hostMonitoring.statusHealthy"), color: "teal" };
    case "stale":
      return { label: t("hostMonitoring.statusStale"), color: "yellow" };
    default:
      return { label: t("hostMonitoring.statusWaiting"), color: "gray" };
  }
}

function points(samples: HostMetricPoint[], field: keyof HostMetricPoint) {
  return samples.flatMap((sample) => {
    const value = sample[field];
    return typeof value === "number"
      ? [
          {
            label: formatStamp(sample.from),
            value,
          },
        ]
      : [];
  });
}

/** 仪表条配色：越接近满载越告警。 */
function meterColor(percent: number): string {
  if (percent >= 90) return "red";
  if (percent >= 75) return "yellow";
  return "blue";
}

/** 右辅栏：节点状态汇总（服务就绪 → 组件状态 → 资源仪表 → 运行明细），分区填满右栏与左列等高。 */
function NodeStatusRail({ data, sample }: { data: HostMonitoring; sample: HostMetricPoint }) {
  const { t } = useTranslation();
  const unavailable = t("hostMonitoring.unavailable");
  const readiness = groupStatus(sample.readinessState, t);
  const components = [
    { label: t("hostMonitoring.componentHost"), group: sample.hostState },
    { label: t("hostMonitoring.componentNetwork"), group: sample.networkState },
    { label: t("hostMonitoring.componentProcess"), group: sample.processState },
  ];
  const memoryTotal = typeof sample.memoryTotalBytes === "number" ? sample.memoryTotalBytes : null;
  const memoryAvailable =
    typeof sample.memoryAvailableBytes === "number" ? sample.memoryAvailableBytes : null;
  const memoryUsedPercent =
    memoryTotal !== null && memoryAvailable !== null && memoryTotal > 0
      ? ((memoryTotal - memoryAvailable) / memoryTotal) * 100
      : null;
  const meters = [
    {
      label: t("hostMonitoring.seriesCpu"),
      percent: typeof sample.cpuPercent === "number" ? sample.cpuPercent : null,
      value: percentOr(sample.cpuPercent, unavailable),
      detail: undefined as string | undefined,
    },
    {
      label: t("hostMonitoring.memoryUsage"),
      percent: memoryUsedPercent,
      value: percentOr(memoryUsedPercent, unavailable),
      detail:
        memoryTotal !== null && memoryAvailable !== null
          ? t("hostMonitoring.memoryUsageDetail", {
              available: bytesOr(memoryAvailable),
              total: bytesOr(memoryTotal),
            })
          : undefined,
    },
    {
      label: t("hostMonitoring.processCpu"),
      percent: typeof sample.processCpuPercent === "number" ? sample.processCpuPercent : null,
      value: percentOr(sample.processCpuPercent, unavailable),
      detail: undefined as string | undefined,
    },
  ];
  const details = [
    {
      label: t("hostMonitoring.seriesNetworkRx"),
      value: `${bytesOr(sample.networkReceiveBytesPerSecond, unavailable)}${t("hostMonitoring.perSecond")}`,
    },
    {
      label: t("hostMonitoring.seriesNetworkTx"),
      value: `${bytesOr(sample.networkTransmitBytesPerSecond, unavailable)}${t("hostMonitoring.perSecond")}`,
    },
    {
      label: t("hostMonitoring.seriesProcessRss"),
      value: bytesOr(sample.processRssBytes, unavailable),
    },
    {
      label: t("hostMonitoring.goroutines"),
      value: countOr(sample.goroutineCount, unavailable),
    },
    {
      label: t("hostMonitoring.fileDescriptors"),
      value: countOr(sample.openFileDescriptors, unavailable),
    },
    {
      label: t("hostMonitoring.dataDirAvailable"),
      value: bytesOr(sample.diskAvailableBytes, unavailable),
    },
  ];
  return (
    <Card
      withBorder
      radius="md"
      padding={density.cardPadding}
      h="100%"
      style={{ display: "flex", flexDirection: "column" }}
    >
      <Group gap="xs" mb="sm">
        <ThemeIcon variant="light" color="indigo" size="sm" radius="md">
          <IconServer size={14} />
        </ThemeIcon>
        <Text fw={700}>{t("hostMonitoring.nodeStatus")}</Text>
      </Group>
      <Group justify="space-between" wrap="nowrap">
        <Text size="sm" c="dimmed">
          {t("hostMonitoring.cardReadiness")}
        </Text>
        <Badge color={readiness.color} variant="light">
          {readiness.label}
        </Badge>
      </Group>
      <Divider my="sm" label={t("hostMonitoring.componentStatus")} labelPosition="left" />
      <Stack gap="xs">
        {components.map((component) => {
          const status = groupStatus(component.group, t);
          return (
            <Group key={component.label} justify="space-between" wrap="nowrap">
              <Text size="sm" c="dimmed">
                {component.label}
              </Text>
              <Badge color={status.color} variant="light">
                {status.label}
              </Badge>
            </Group>
          );
        })}
      </Stack>
      <Divider my="sm" label={t("hostMonitoring.resourceUsage")} labelPosition="left" />
      <Stack gap="sm">
        {meters.map((meter) => (
          <Stack key={meter.label} gap={4}>
            <Group justify="space-between" wrap="nowrap">
              <Text size="sm" c="dimmed">
                {meter.label}
              </Text>
              <Text size="sm" fw={600}>
                {meter.value}
              </Text>
            </Group>
            {meter.percent !== null ? (
              <Progress
                value={Math.min(100, Math.max(0, meter.percent))}
                color={meterColor(meter.percent)}
                size={6}
                radius="xl"
              />
            ) : null}
            {meter.detail ? (
              <Text size="xs" c="dimmed">
                {meter.detail}
              </Text>
            ) : null}
          </Stack>
        ))}
      </Stack>
      <Divider my="sm" label={t("hostMonitoring.metricDetails")} labelPosition="left" />
      <Stack gap="xs">
        {details.map((row) => (
          <Group key={row.label} justify="space-between" wrap="nowrap">
            <Text size="sm" c="dimmed">
              {row.label}
            </Text>
            <Text size="sm" fw={600}>
              {row.value}
            </Text>
          </Group>
        ))}
      </Stack>
      <Text size="xs" c="dimmed" mt="auto" pt="sm">
        {t("hostMonitoring.latestSample", {
          time: data.latestSampleAt
            ? new Date(data.latestSampleAt).toLocaleString("zh-CN")
            : unavailable,
        })}
      </Text>
    </Card>
  );
}

export function HostMonitoringLive() {
  const { t } = useTranslation();
  const [range, setRange] = useState<Range>("24h");
  const state = useAsync(
    () => {
      const to = new Date();
      const from = new Date(to.getTime() - rangeMillis[range]);
      return getHostMonitoring({ from: from.toISOString(), to: to.toISOString() });
    },
    [range],
    { cacheKey: `host-monitoring:${range}` },
  );
  // 慢接口保护：上一轮采样还没落地时跳过本轮轮询，避免请求在挂起期间累积成雪崩。
  const refreshingRef = useRef(state.refreshing);
  refreshingRef.current = state.refreshing;
  const pollRefresh = useCallback(() => {
    if (refreshingRef.current) return;
    state.reload();
  }, [state.reload]);
  useVisibleRefresh(pollRefresh);

  const [lastUpdated, setLastUpdated] = useState<Date | null>(null);
  useEffect(() => {
    if (state.data) setLastUpdated(new Date());
  }, [state.data]);

  const unavailable = t("hostMonitoring.unavailable");

  if (state.error) {
    return (
      <Alert
        color="red"
        title={t("hostMonitoring.errorTitle")}
        icon={<IconAlertTriangle size={18} />}
      >
        <Stack gap="sm">
          <Text size="sm">{state.error.message}</Text>
          <Button
            size="xs"
            variant="light"
            w="fit-content"
            onClick={state.reload}
            leftSection={<IconRefresh size={14} />}
          >
            {t("common.retry", { defaultValue: "重试" })}
          </Button>
        </Stack>
      </Alert>
    );
  }
  if (!state.data) {
    return (
      <Stack gap={density.gridSpacing} aria-busy="true" aria-label={t("hostMonitoring.loading")}>
        <Group justify="space-between">
          <Skeleton height={22} width={200} radius="sm" />
          <Skeleton height={28} width={120} radius="sm" />
        </Group>
        <Skeleton height={32} radius="md" />
        <Grid gutter={density.gridSpacing}>
          <Grid.Col span={{ base: 12, lg: 8 }}>
            <Stack gap={density.gridSpacing}>
              <Skeleton height={240} radius="md" />
              <Skeleton height={220} radius="md" />
            </Stack>
          </Grid.Col>
          <Grid.Col span={{ base: 12, lg: 4 }}>
            <Skeleton height={470} radius="md" />
          </Grid.Col>
        </Grid>
        <SimpleGrid cols={{ base: 1, sm: 2, lg: 3 }} spacing={density.gridSpacing}>
          <Skeleton height={220} radius="md" />
          <Skeleton height={220} radius="md" />
          <Skeleton height={220} radius="md" />
        </SimpleGrid>
      </Stack>
    );
  }
  if (!state.data.latest) {
    return (
      <Alert color="gray" title={t("hostMonitoring.noSampleTitle")}>
        {t("hostMonitoring.noSampleDescription")}
      </Alert>
    );
  }

  const data = state.data;
  const sample = data.latest!;
  const status = hostStatus(data.hostState, t);

  return (
    <Stack gap={density.gridSpacing}>
      {/* 状态行：唯一一处状态徽章 + 采样/更新时间 + 范围控件 */}
      <Group justify="space-between" align="center" wrap="wrap">
        <Group gap="sm" wrap="nowrap">
          <StatusPill
            tone={status.color === "teal" ? "green" : status.color === "yellow" ? "orange" : "gray"}
            text={status.label}
          />
          <Text size="sm" c="dimmed">
            {t("hostMonitoring.latestSample", {
              time: data.latestSampleAt
                ? new Date(data.latestSampleAt).toLocaleString("zh-CN")
                : unavailable,
            })}
            {lastUpdated
              ? ` · ${t("hostMonitoring.pageUpdated", {
                  time: lastUpdated.toLocaleTimeString("zh-CN"),
                })}`
              : ""}
          </Text>
        </Group>
        <PreviewRangeControls
          value={range}
          options={rangeOptions}
          onChange={(value) => setRange(value as Range)}
          ariaLabel={t("hostMonitoring.rangeLabel", { defaultValue: "采样时间范围" })}
        />
      </Group>

      {/* 采样状态：健康 → 细状态条；stale → 完整警示（需要立即处理）。 */}
      {data.hostState === "stale" ? (
        <Alert
          color="yellow"
          icon={<IconAlertTriangle size={18} />}
          title={t("hostMonitoring.staleAlertTitle")}
        >
          {t("hostMonitoring.staleAlertDescription")}
        </Alert>
      ) : (
        <Group
          gap="xs"
          wrap="nowrap"
          px="sm"
          py={6}
          style={{
            border: "1px solid var(--mantine-color-teal-2)",
            borderRadius: "var(--mantine-radius-md)",
            background: "var(--mantine-color-teal-0)",
          }}
        >
          <IconCircleCheck size={16} color="var(--mantine-color-teal-7)" aria-hidden />
          <Text size="sm" c="dimmed">
            {t("hostMonitoring.sampleOkShort")}
          </Text>
        </Group>
      )}

      {state.refreshError ? (
        <Alert
          color="yellow"
          title={t("hostMonitoring.refreshFailedTitle")}
          icon={<IconAlertTriangle size={18} />}
        >
          <Stack gap="xs">
            <Text size="sm">
              {t("hostMonitoring.refreshFailedDescription")} {state.refreshError.message}
            </Text>
            <Button
              size="xs"
              variant="light"
              color="yellow"
              w="fit-content"
              onClick={state.reload}
              leftSection={<IconRefresh size={14} />}
            >
              {t("common.retry", { defaultValue: "重试" })}
            </Button>
          </Stack>
        </Alert>
      ) : null}

      {/* 主区：CPU 实时大图 + 内存趋势；右辅栏节点状态 */}
      <Grid gutter={density.gridSpacing} align="stretch">
        <Grid.Col span={{ base: 12, lg: 8 }}>
          <Stack gap={density.gridSpacing}>
            <Card withBorder radius="md" padding={density.cardPadding}>
              <TrendChart
                title={t("hostMonitoring.trendCpu")}
                summary={t("hostMonitoring.trendCpuSummary")}
                primary={points(data.samples, "cpuPercent")}
                primaryLabel={t("hostMonitoring.seriesCpu")}
                unit="percent"
                headerRight={
                  <Group gap="xs" wrap="nowrap">
                    <Badge size="sm" variant="light" color="blue">
                      {t("hostMonitoring.liveBadge")}
                    </Badge>
                    <IconCpu size={20} color="var(--mantine-color-blue-6)" />
                    <Text size="xl" fw={700} lh={1.1}>
                      {percentOr(sample.cpuPercent, unavailable)}
                    </Text>
                  </Group>
                }
              />
            </Card>
            <Card withBorder radius="md" padding={density.cardPadding}>
              <TrendChart
                title={t("hostMonitoring.trendMemory")}
                summary={t("hostMonitoring.trendMemorySummary")}
                primary={points(data.samples, "memoryAvailableBytes")}
                primaryLabel={t("hostMonitoring.seriesMemory")}
                unit="bytes"
                headerRight={
                  <Group gap="xs" wrap="nowrap">
                    <IconStack2 size={20} color="var(--mantine-color-teal-6)" />
                    <Text size="xl" fw={700} lh={1.1}>
                      {bytesOr(sample.memoryAvailableBytes, unavailable)}
                    </Text>
                  </Group>
                }
              />
            </Card>
          </Stack>
        </Grid.Col>
        <Grid.Col span={{ base: 12, lg: 4 }}>
          <NodeStatusRail data={data} sample={sample} />
        </Grid.Col>
      </Grid>

      {/* 底部：磁盘 / 网络 / 进程 3 图并排 */}
      <SimpleGrid cols={{ base: 1, sm: 2, lg: 3 }} spacing={density.gridSpacing}>
        <Card withBorder radius="md" padding={density.cardPadding}>
          <TrendChart
            title={t("hostMonitoring.trendDisk")}
            summary={t("hostMonitoring.trendDiskSummary")}
            primary={points(data.samples, "diskAvailableBytes")}
            primaryLabel={t("hostMonitoring.seriesDisk")}
            unit="bytes"
            compact
          />
        </Card>
        <Card withBorder radius="md" padding={density.cardPadding}>
          <TrendChart
            title={t("hostMonitoring.trendNetwork")}
            summary={t("hostMonitoring.trendNetworkSummary")}
            primary={points(data.samples, "networkReceiveBytesPerSecond")}
            secondary={points(data.samples, "networkTransmitBytesPerSecond")}
            unit="bytes"
            primaryLabel={t("hostMonitoring.seriesNetworkRx")}
            secondaryLabel={t("hostMonitoring.seriesNetworkTx")}
            compact
          />
        </Card>
        <Card withBorder radius="md" padding={density.cardPadding}>
          <TrendChart
            title={t("hostMonitoring.trendProcess")}
            summary={t("hostMonitoring.trendProcessSummary")}
            primary={points(data.samples, "processRssBytes")}
            primaryLabel={t("hostMonitoring.seriesProcessRss")}
            unit="bytes"
            compact
          />
        </Card>
      </SimpleGrid>
    </Stack>
  );
}
