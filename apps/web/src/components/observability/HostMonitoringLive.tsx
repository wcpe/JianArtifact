// 主机监控（v0.8.2 监控台布局）：
// - 状态行：单个状态胶囊 + 采样/更新时间 + 范围控件（徽章不再重复三处）；
// - 健康时采样状态收成一条细状态条，仅 stale 才弹出完整警示 Alert；
// - 主区（8/12）：CPU 实时大图 → 内存卡（总量/已用/可用/使用率三值 + 多线趋势，
//   含进程 RSS，已与原独立进程内存图合并）；右辅栏（4/12）：节点状态（就绪/组件/网络）；
// - 底部：磁盘（三值 + 多线趋势）/ 网络 / 进程指标表 3 格并排，进程类指标只在表内出现一次。
import {
  Alert,
  Badge,
  Box,
  Button,
  Card,
  Divider,
  Grid,
  Group,
  Loader,
  LoadingOverlay,
  Progress,
  Select,
  SimpleGrid,
  Skeleton,
  Stack,
  Table,
  Text,
  ThemeIcon,
} from "@mantine/core";
import {
  IconAlertTriangle,
  IconCircleCheck,
  IconCpu,
  IconRefresh,
  IconServer,
} from "@tabler/icons-react";
import { useCallback, useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";

import { getHostMonitoring } from "../../api/endpoints";
import type { HostMetricGroup, HostMetricPoint, HostMonitoring } from "../../api/types";
import { currentLocaleTag } from "../../i18n/current";
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

const rangeOptions: Array<{ value: Range; labelKey: string }> = [
  { value: "1h", labelKey: "hostMonitoring.range1h" },
  { value: "6h", labelKey: "hostMonitoring.range6h" },
  { value: "24h", labelKey: "hostMonitoring.range24h" },
  { value: "7d", labelKey: "hostMonitoring.range7d" },
  { value: "30d", labelKey: "hostMonitoring.range30d" },
];

// 主机指标可能缺采样，统一用「有值走共享格式化、无值回退占位」的容错包装，
// 避免各地各写一份 formatBytes / formatCount（进位分支不一致会给出不同字符串）。
// 默认占位统一为 "—"：新字段（memoryUsedBytes 等）在历史样本行为 null，判空渲染占位。
function bytesOr(value: number | null | undefined, fallback = "—"): string {
  return typeof value === "number" ? formatBytes(value) : fallback;
}

function percentOr(value: number | null | undefined, fallback = "—"): string {
  return typeof value === "number" ? `${value.toFixed(1)}%` : fallback;
}

function countOr(value: number | null | undefined, fallback = "—"): string {
  return typeof value === "number" ? formatCount(value) : fallback;
}

/**
 * 进程运行时长格式化：满 1 天显示「3d 04:12:33」（天数后缀由 i18n 提供，中文为「3天 04:12:33」），
 * 不足 1 天只显示 `04:12:33`。口径为挂钟秒数（processUptimeSeconds，仅本进程，
 * 不做系统进程枚举），非法值显示 "—"。
 */
function formatUptime(seconds: number | null | undefined, daySuffix: string): string {
  if (typeof seconds !== "number" || !Number.isFinite(seconds) || seconds < 0) return "—";
  const total = Math.floor(seconds);
  const days = Math.floor(total / 86_400);
  const rest = total % 86_400;
  const pad = (n: number) => String(n).padStart(2, "0");
  const clock = `${pad(Math.floor(rest / 3_600))}:${pad(Math.floor((rest % 3_600) / 60))}:${pad(rest % 60)}`;
  return days > 0 ? `${days}${daySuffix} ${clock}` : clock;
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

/** 容量三值格（标签 + 数值并排）：数值缺失时由 bytesOr / percentOr 落到 "—"。 */
function CapacityStat({ label, value }: { label: string; value: string }) {
  return (
    <Stack gap={2}>
      <Text size="xs" c="dimmed">
        {label}
      </Text>
      <Text size="sm" fw={600} style={{ fontVariantNumeric: "tabular-nums" }}>
        {value}
      </Text>
    </Stack>
  );
}

/**
 * 内存已用：优先读后端固定口径 `memoryUsedBytes`（= total − available，采样点同口径）；
 * 历史样本无该字段时才用 total − available 兜底推算，避免前端口径与后端漂移。
 * 返回已用字节数与使用率（percent），任一缺失为 null。
 */
function resolveMemoryUsed(sample: HostMetricPoint): {
  used: number | null;
  percent: number | null;
} {
  const total = typeof sample.memoryTotalBytes === "number" ? sample.memoryTotalBytes : null;
  const available =
    typeof sample.memoryAvailableBytes === "number" ? sample.memoryAvailableBytes : null;
  const used =
    typeof sample.memoryUsedBytes === "number"
      ? sample.memoryUsedBytes
      : total !== null && available !== null
        ? total - available
        : null;
  const percent = used !== null && total !== null && total > 0 ? (used / total) * 100 : null;
  return { used, percent };
}

/** 磁盘占用率：已用 ÷ 总量（数据目录所在卷）；新字段缺失（老数据历史行）为 null。 */
function diskUsagePercent(sample: HostMetricPoint): number | null {
  const total = typeof sample.diskTotalBytes === "number" ? sample.diskTotalBytes : null;
  const used = typeof sample.diskUsedBytes === "number" ? sample.diskUsedBytes : null;
  return used !== null && total !== null && total > 0 ? (used / total) * 100 : null;
}

/**
 * 进程指标表：把原散落在辅栏与底部趋势图里的进程类指标收敛成一张紧凑表。
 * 仅统计当前进程（不做系统进程枚举）；运行时长列为新增参数（processUptimeSeconds，挂钟秒）。
 */
function ProcessMetricsCard({
  sample,
  unavailable,
}: {
  sample: HostMetricPoint;
  unavailable: string;
}) {
  const { t } = useTranslation();
  const rows = [
    {
      label: t("hostMonitoring.processCpu"),
      value: percentOr(sample.processCpuPercent, unavailable),
      note: t("hostMonitoring.processCpuNote"),
    },
    {
      label: t("hostMonitoring.seriesProcessRss"),
      value: bytesOr(sample.processRssBytes, unavailable),
      note: t("hostMonitoring.processRssNote"),
    },
    {
      label: t("hostMonitoring.goroutines"),
      value: countOr(sample.goroutineCount, unavailable),
      note: t("hostMonitoring.goroutinesNote"),
    },
    {
      label: t("hostMonitoring.fileDescriptors"),
      value: countOr(sample.openFileDescriptors, unavailable),
      note: t("hostMonitoring.fileDescriptorsNote"),
    },
    {
      label: t("hostMonitoring.uptime"),
      value: formatUptime(sample.processUptimeSeconds, t("hostMonitoring.uptimeDaySuffix")),
      note: t("hostMonitoring.uptimeNote"),
    },
  ];
  return (
    <Card withBorder radius="md" padding={density.cardPadding}>
      <Group justify="space-between" mb="xs" wrap="nowrap">
        <Text fw={600}>{t("hostMonitoring.processMetrics")}</Text>
        <Text size="xs" c="dimmed">
          {t("hostMonitoring.processMetricsHint")}
        </Text>
      </Group>
      {/* 窄容器（lg 起该卡只占 4 栏）下隐藏「说明」列：备注是次要的释义，指标名 + 当前值本身已可读；
          宽容器（base/sm/md 整行）仍保留三列。用 hiddenFrom 走 CSS 媒体查询与 Grid 的 lg 断点对齐，
          不额外引入 JS 断点判断。 */}
      <Table layout="fixed" highlightOnHover verticalSpacing={6} horizontalSpacing="xs" fz="sm">
        <Table.Thead>
          <Table.Tr>
            <Table.Th w="38%">{t("hostMonitoring.colMetric")}</Table.Th>
            <Table.Th w="30%">{t("hostMonitoring.colCurrentValue")}</Table.Th>
            <Table.Th hiddenFrom="lg">{t("hostMonitoring.colNote")}</Table.Th>
          </Table.Tr>
        </Table.Thead>
        <Table.Tbody>
          {rows.map((row) => (
            <Table.Tr key={row.label}>
              <Table.Td c="dimmed">{row.label}</Table.Td>
              <Table.Td style={{ fontVariantNumeric: "tabular-nums" }}>{row.value}</Table.Td>
              <Table.Td
                hiddenFrom="lg"
                c="dimmed"
                style={{ fontSize: "var(--mantine-font-size-xs)" }}
              >
                {row.note}
              </Table.Td>
            </Table.Tr>
          ))}
        </Table.Tbody>
      </Table>
    </Card>
  );
}

/** 右辅栏：节点状态汇总（服务就绪 → 组件状态 → 主机资源 → 网络明细）。
 *  按内容取高（不再与左列强行等高，避免中间留出大片空白）；进程类指标
 *  （进程 CPU / RSS / goroutine / fd / 运行时长）与容量三值不再散落这里，
 *  统一收敛到内存图右侧的「进程指标」表与内存 / 磁盘卡，避免同一指标多处重复展示。 */
function NodeStatusRail({ data, sample }: { data: HostMonitoring; sample: HostMetricPoint }) {
  const { t } = useTranslation();
  const unavailable = t("hostMonitoring.unavailable");
  const readiness = groupStatus(sample.readinessState, t);
  const components = [
    { label: t("hostMonitoring.componentHost"), group: sample.hostState },
    { label: t("hostMonitoring.componentNetwork"), group: sample.networkState },
    { label: t("hostMonitoring.componentProcess"), group: sample.processState },
  ];
  const meters = [
    {
      label: t("hostMonitoring.seriesCpu"),
      percent: typeof sample.cpuPercent === "number" ? sample.cpuPercent : null,
      value: percentOr(sample.cpuPercent, unavailable),
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
  ];
  return (
    <Card withBorder radius="md" padding={density.cardPadding}>
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
      <Divider my="xs" label={t("hostMonitoring.componentStatus")} labelPosition="left" />
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
      <Divider my="xs" label={t("hostMonitoring.resourceUsage")} labelPosition="left" />
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
          </Stack>
        ))}
      </Stack>
      <Divider my="xs" label={t("hostMonitoring.metricDetails")} labelPosition="left" />
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
      <Text size="xs" c="dimmed" pt="sm">
        {t("hostMonitoring.latestSample", {
          time: data.latestSampleAt
            ? new Date(data.latestSampleAt).toLocaleString(currentLocaleTag())
            : unavailable,
        })}
      </Text>
    </Card>
  );
}

export function HostMonitoringLive() {
  const { t } = useTranslation();
  const [range, setRange] = useState<Range>("24h");
  // 网络卡选中的网卡：空串 = 全部网卡（不传 interface 查询参数，走全网卡聚合）。口径为「选定网卡后
  // 速率图与累计总量都按该网卡」，故该状态同时进入取数参数与 cacheKey。
  const [selectedInterface, setSelectedInterface] = useState("");
  const state = useAsync(
    () => {
      const to = new Date();
      const from = new Date(to.getTime() - rangeMillis[range]);
      return getHostMonitoring({
        from: from.toISOString(),
        to: to.toISOString(),
        interfaceName: selectedInterface || undefined,
      });
    },
    [range, selectedInterface],
    // keepPreviousData：切换时间档位或网卡时保留旧数据、仅后台刷新，避免整块内容被骨架替换造成
    // 布局重排（与搜索页换排序/翻页同一问题、同一处理）。
    { cacheKey: `host-monitoring:${range}:${selectedInterface}`, keepPreviousData: true },
  );
  // 网卡下线（新响应不再包含当前选择）时回退到「全部网卡」，而不是继续展示该网卡的空数据。
  // 仅在有响应（state.data 非空）时校验——取数途中 data 会被清空，若据此判定「网卡消失」会在
  // 用户刚选完网卡的瞬间把选择顶回「全部网卡」，造成口径来回抖动。
  useEffect(() => {
    if (!state.data || !selectedInterface) return;
    const names = (state.data.networkInterfaces ?? []).map((item) => item.name);
    if (!names.includes(selectedInterface)) setSelectedInterface("");
  }, [state.data, selectedInterface]);
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
        <Grid gap={density.gridSpacing}>
          <Grid.Col span={{ base: 12, lg: 8 }}>
            <Skeleton height={240} radius="md" />
          </Grid.Col>
          <Grid.Col span={{ base: 12, lg: 4 }}>
            <Skeleton height={240} radius="md" />
          </Grid.Col>
          <Grid.Col span={{ base: 12, lg: 8 }}>
            <Skeleton height={220} radius="md" />
          </Grid.Col>
          <Grid.Col span={{ base: 12, lg: 4 }}>
            <Skeleton height={220} radius="md" />
          </Grid.Col>
        </Grid>
        <SimpleGrid cols={{ base: 1, sm: 2 }} spacing={density.gridSpacing}>
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
  // 内存图左轴域：以系统内存总量为顶（取最新样本 memoryTotalBytes），已用线之上到轴顶的
  // 空白即空闲内存；历史样本该字段为 null 时退回 recharts 自适应，避免轴域塌缩。
  const memoryTotalBytes =
    typeof sample.memoryTotalBytes === "number" ? sample.memoryTotalBytes : null;
  const memoryYDomain: ["auto", "auto"] | [number, number] =
    memoryTotalBytes === null ? ["auto", "auto"] : [0, memoryTotalBytes];
  // 磁盘图左轴域：与内存图同构——以数据目录卷总量为顶（取最新样本 diskTotalBytes），已用面积
  // 之上到轴顶的空白即当前可用空间；历史样本该字段为 null 时退回 recharts 自适应，避免轴域塌缩。
  const diskTotalBytes = typeof sample.diskTotalBytes === "number" ? sample.diskTotalBytes : null;
  const diskYDomain: ["auto", "auto"] | [number, number] =
    diskTotalBytes === null ? ["auto", "auto"] : [0, diskTotalBytes];

  // 网卡选择器数据：首个选项固定为「全部网卡」（value 空串 = 不传 interface，走全网卡聚合）。
  // 响应缺 networkInterfaces（历史数据 / 非 Linux 平台）时名字列表为空，选择器整体不渲染。
  const interfaceNames = (data.networkInterfaces ?? []).map((item) => item.name);
  const interfaceOptions = [
    { value: "", label: t("hostMonitoring.networkInterfaceAll") },
    ...interfaceNames.map((name) => ({ value: name, label: name })),
  ];
  // 选中项已不在响应列表里时按「全部网卡」显示（取数口径的回落由上面的 effect 落到 state）。
  const interfaceSelectValue = interfaceNames.includes(selectedInterface) ? selectedInterface : "";

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
                ? new Date(data.latestSampleAt).toLocaleString(currentLocaleTag())
                : unavailable,
            })}
            {lastUpdated
              ? ` · ${t("hostMonitoring.pageUpdated", {
                  time: lastUpdated.toLocaleTimeString(currentLocaleTag()),
                })}`
              : ""}
          </Text>
        </Group>
        <PreviewRangeControls
          value={range}
          options={rangeOptions.map((option) => ({
            value: option.value,
            label: t(option.labelKey),
          }))}
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

      {/* 内容区加载反馈：切换时间档位 / 网卡时（keepPreviousData）旧数据保留在 DOM、布局不跳，
          仅在此叠加一层不遮挡内容的遮罩作为「正在换数据」的显式反馈。zIndex 取 5，远低于网卡
          下拉弹层（Mantine Popover 默认 300）——弹层挂在 body 的 portal 上，遮罩若取默认 400
          会盖住选项面板让用户点不到选项（已实测）。 */}
      <Box pos="relative">
        <LoadingOverlay
          visible={state.refreshing}
          zIndex={5}
          overlayProps={{ blur: 0, backgroundOpacity: 0.25 }}
          loaderProps={{ size: "sm" }}
          data-testid="host-monitoring-refreshing"
        />
        <Stack gap={density.gridSpacing}>
          {/* 主区两行（每行各满 12 栏）：第 1 行 CPU 实时大图 + 节点状态；第 2 行内存趋势 + 进程指标 */}
          <Grid gap={density.gridSpacing} align="start">
            <Grid.Col span={{ base: 12, lg: 8 }}>
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
            </Grid.Col>
            <Grid.Col span={{ base: 12, lg: 4 }}>
              <NodeStatusRail data={data} sample={sample} />
            </Grid.Col>
            <Grid.Col span={{ base: 12, lg: 8 }}>
              <Card withBorder radius="md" padding={density.cardPadding}>
                {/* 内存卡：总量 / 已用 / 可用 / 使用率三值并排；已用优先后端 memoryUsedBytes，
                null（历史样本）时按 total − available 兜底（见 resolveMemoryUsed 注释）。 */}
                <SimpleGrid cols={{ base: 2, sm: 4 }} spacing="xs" mb="xs">
                  <CapacityStat
                    label={t("hostMonitoring.memoryTotal")}
                    value={bytesOr(sample.memoryTotalBytes, unavailable)}
                  />
                  <CapacityStat
                    label={t("hostMonitoring.memoryUsed")}
                    // null 降级为 "—"：历史样本无 memoryUsedBytes 且 total/available 也缺时占位
                    value={bytesOr(resolveMemoryUsed(sample).used)}
                  />
                  <CapacityStat
                    label={t("hostMonitoring.seriesMemory")}
                    value={bytesOr(sample.memoryAvailableBytes, unavailable)}
                  />
                  <CapacityStat
                    label={t("hostMonitoring.memoryUsage")}
                    value={percentOr(resolveMemoryUsed(sample).percent)}
                  />
                </SimpleGrid>
                <TrendChart
                  title={t("hostMonitoring.trendMemory")}
                  summary={t("hostMonitoring.trendMemorySummary")}
                  // 纵轴=系统内存总量：只画「已用」面积，已用之上到轴顶的空白即空闲内存
                  //（不再单画「可用」折线）。进程 RSS 与内存同为字节量纲、共用同一左轴，
                  //故只有几十 MB 时会自然贴近图底——即「从最底部往上计算」；若走独立右轴，
                  //它会被拉伸成贴顶的平线，观感错误。
                  primary={points(data.samples, "memoryUsedBytes")}
                  secondary={points(data.samples, "processRssBytes")}
                  primaryLabel={t("hostMonitoring.seriesMemoryUsed")}
                  secondaryLabel={t("hostMonitoring.seriesProcessRss")}
                  unit="bytes"
                  yDomain={memoryYDomain}
                  // 右轴百分比：已用 ÷ 内存总量（与 yDomain 上界同源），左轴读字节量、右轴直接读占比。
                  rightPercentAxis
                  headerRight={
                    <Text size="sm" c="dimmed" style={{ fontVariantNumeric: "tabular-nums" }}>
                      {`${t("hostMonitoring.memoryUsage")} ${percentOr(resolveMemoryUsed(sample).percent)}`}
                    </Text>
                  }
                />
              </Card>
            </Grid.Col>
            <Grid.Col span={{ base: 12, lg: 4 }}>
              <ProcessMetricsCard sample={sample} unavailable={unavailable} />
            </Grid.Col>
          </Grid>

          {/* 底部：磁盘 / 网络两张趋势图平分（各占一半；进程指标表已上移到内存图右侧） */}
          <SimpleGrid cols={{ base: 1, sm: 2 }} spacing={density.gridSpacing}>
            <Card withBorder radius="md" padding={density.cardPadding}>
              {/* 磁盘卡：总量 / 已用 / 可用 / 占用率。diskTotalBytes / diskUsedBytes 是新字段，
              老数据历史行为 null——降级为 "—"，只保留可用值（不伪造 0 或推算值）。 */}
              <SimpleGrid cols={2} spacing="xs" mb="xs">
                <CapacityStat
                  label={t("hostMonitoring.diskTotal")}
                  // 新字段 null（老数据历史行）降级为 "—"，只保留可用值，不伪造 0
                  value={bytesOr(sample.diskTotalBytes)}
                />
                <CapacityStat
                  label={t("hostMonitoring.diskUsed")}
                  value={bytesOr(sample.diskUsedBytes)}
                />
                <CapacityStat
                  label={t("hostMonitoring.seriesDisk")}
                  value={bytesOr(sample.diskAvailableBytes, unavailable)}
                />
                <CapacityStat
                  label={t("hostMonitoring.diskUsage")}
                  value={percentOr(diskUsagePercent(sample))}
                />
              </SimpleGrid>
              <TrendChart
                title={t("hostMonitoring.trendDisk")}
                summary={t("hostMonitoring.trendDiskSummary")}
                // 纵轴=数据目录卷总量：与内存图同构——只画「已用」面积，已用之上到轴顶的空白即
                // 可用空间（不再单画「可用」折线；总量也不再走独立右轴当参照线，而是成为轴顶）。
                // 取舍：用户此前要求磁盘「拉大变动」，而 [0, total] 会让波动看起来更平；此处选
                // 正确性优先——旧的「可用+已用走左轴、总量走右轴」会让两条线各自贴顶，被误读成
                // 「已用≈总量」，与真实占用率矛盾。可用量仍由卡内三值文本给出精确数字。
                primary={points(data.samples, "diskUsedBytes")}
                primaryLabel={t("hostMonitoring.seriesDiskUsed")}
                unit="bytes"
                yDomain={diskYDomain}
                // 右轴百分比：已用 ÷ 数据目录卷总量（与 yDomain 上界同源）。
                rightPercentAxis
                headerRight={
                  <Text size="sm" c="dimmed" style={{ fontVariantNumeric: "tabular-nums" }}>
                    {`${t("hostMonitoring.diskUsage")} ${percentOr(diskUsagePercent(sample))}`}
                  </Text>
                }
                compact
              />
            </Card>
            <Card withBorder radius="md" padding={density.cardPadding}>
              {/* 网络卡：总接收 / 总发送两值（取自最新样本的自网卡启动累计 total，缺失降级为 "—" 不伪造 0）
              + 网卡选择器（首个选项「全部网卡」；选定后速率图与总量都按该网卡，由取数参数 interface 驱动）。 */}
              <Group justify="space-between" align="flex-end" wrap="nowrap" gap="sm" mb="xs">
                <Stack gap={2}>
                  <Group gap="xl" wrap="nowrap">
                    <CapacityStat
                      label={t("hostMonitoring.networkTotalReceive")}
                      value={bytesOr(sample.networkReceiveBytesTotal)}
                    />
                    <CapacityStat
                      label={t("hostMonitoring.networkTotalTransmit")}
                      value={bytesOr(sample.networkTransmitBytesTotal)}
                    />
                  </Group>
                  <Text size="xs" c="dimmed">
                    {t("hostMonitoring.networkInterfaceTotalHint")}
                  </Text>
                </Stack>
                {interfaceNames.length > 0 ? (
                  <Select
                    size="xs"
                    w={170}
                    label={t("hostMonitoring.networkInterface")}
                    data={interfaceOptions}
                    value={interfaceSelectValue}
                    onChange={(value) => setSelectedInterface(value ?? "")}
                    allowDeselect={false}
                    // 刷新中（换网卡 / 换档位）在控件自身也给出加载反馈，无需等整块遮罩。
                    rightSection={
                      state.refreshing && !state.loading ? <Loader size={12} aria-hidden /> : null
                    }
                    // 选项面板固定挂到 body 的 portal：遮罩 zIndex 只有 5，弹层 300 才不会被压住。
                    comboboxProps={{ withinPortal: true }}
                  />
                ) : null}
              </Group>
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
          </SimpleGrid>
        </Stack>
      </Box>
    </Stack>
  );
}
