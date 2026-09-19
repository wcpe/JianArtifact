// 业务仪表盘（真实读模型，v0.8.2 指挥舱布局）：
// - 顶栏：时间范围选择器 + 紧凑状态徽章 + 快捷操作下拉 + 刷新；
// - 8 张 KPI 卡压缩为一条「指标带」（数值内联 + 悬停提示），首屏信息密度前置；
// - 主区（8/12）：请求趋势主图 → 容量趋势 → 仓库状态；右栏（4/12）：需要处理 + 最近活跃贯穿全页。
import {
  Alert,
  Badge,
  Button,
  Card,
  Grid,
  Group,
  Menu,
  SimpleGrid,
  Skeleton,
  Stack,
  Text,
  ThemeIcon,
  Tooltip,
  UnstyledButton,
} from "@mantine/core";
import {
  IconActivity,
  IconAlertTriangle,
  IconBan,
  IconBolt,
  IconBox,
  IconChevronDown,
  IconClock,
  IconClipboardCheck,
  IconDatabase,
  IconDownload,
  IconEye,
  IconHistory,
  IconPackage,
  IconRefresh,
} from "@tabler/icons-react";
import { useCallback, useEffect, useRef, useState, type CSSProperties } from "react";
import { useTranslation } from "react-i18next";
import { useNavigate } from "react-router-dom";

import {
  getOperationsDashboard,
  getStatus,
  listAllRepositories,
  listAuditAttentions,
  listAuditEvents,
} from "../../api/endpoints";
import type {
  AuditAttention,
  AuditEvent,
  OperationsAlert,
  OperationsDashboard,
  Repository,
  StatusInfo,
} from "../../api/types";
import { currentLocaleTag } from "../../i18n/current";
import { useAsync, useVisibleRefresh } from "../../hooks/useAsync";
import { formatBytes, formatCount, formatStamp } from "../../lib/format";
import { UPSTREAM_BLOCKED_CODE } from "../../lib/connectionStatus";
import { density } from "../../theme/density";
import { OpsKpiBand } from "../ops/OpsKit";
import { TrendChart } from "./TrendChart";
import { RepositoryStatusPanel } from "./RepositoryStatusPanel";
import { DashboardRangePicker, type DashboardRange } from "./DashboardRangePicker";

type TrendSeries = Array<{ label: string; value: number }>;

function points<T extends { from: string }>(items: T[], field: keyof T): TrendSeries {
  return items.map((item) => ({
    label: formatStamp(item.from),
    value: (item[field] as unknown as number) ?? 0,
  }));
}

function severityColor(severity: string): string {
  if (severity === "critical") return "red";
  if (severity === "high") return "orange";
  if (severity === "warning") return "yellow";
  return "gray";
}

function resultColor(result: string): string {
  if (result === "failure") return "red";
  if (result === "success") return "teal";
  return "gray";
}

export function DashboardLive() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const [range, setRange] = useState<DashboardRange>(() => {
    const now = new Date();
    return {
      preset: "24h",
      from: new Date(now.getTime() - 86_400_000).toISOString(),
      to: now.toISOString(),
    };
  });
  const [lastUpdated, setLastUpdated] = useState<Date | null>(null);

  const dashboard = useAsync(
    () => getOperationsDashboard({ from: range.from, to: range.to }),
    [range.from, range.to],
    { cacheKey: `dashboard:${range.from}:${range.to}` },
  );
  const status = useAsync<StatusInfo>(getStatus, [], { cacheKey: "dashboard:status" });
  const attentions = useAsync(() => listAuditAttentions({ limit: 20 }), [], {
    cacheKey: "dashboard:attentions",
  });
  const recent = useAsync(
    () => listAuditEvents({ from: range.from, to: range.to, limit: 12 }),
    [range.from, range.to],
    { cacheKey: `dashboard:recent:${range.from}:${range.to}` },
  );
  // 仓库状态面板：全量仓库（含连接状态），供环形图与状态明细。
  // 必须拉全量（而不是写死 page_size）：面板上的「共 N 个仓库」与环形图分布都要与
  // 仓库列表一致，档位放大后不能出现"面板说 50、列表说 144"。
  const repos = useAsync(() => listAllRepositories(), [], {
    cacheKey: "dashboard:repos",
  });

  // 任一子请求在途即视为"忙"：慢接口下用它挡住轮询与手动刷新，避免请求在挂起期间累积。
  const busy = dashboard.refreshing || status.refreshing || recent.refreshing;
  const busyRef = useRef(busy);
  busyRef.current = busy;

  const reloadAll = useCallback(() => {
    if (busyRef.current) return;
    dashboard.reload();
    status.reload();
    attentions.reload();
    recent.reload();
    repos.reload();
  }, [dashboard.reload, status.reload, attentions.reload, recent.reload, repos.reload]);
  useVisibleRefresh(reloadAll);

  useEffect(() => {
    if (dashboard.data) setLastUpdated(new Date());
  }, [dashboard.data]);

  const requestTrend: {
    request: TrendSeries;
    download: TrendSeries;
    failure: TrendSeries;
  } | null = dashboard.data
    ? {
        request: points(dashboard.data.requestTrend, "requestCount"),
        download: points(dashboard.data.requestTrend, "downloadCount"),
        failure: points(dashboard.data.requestTrend, "failureCount"),
      }
    : null;
  const capacityTrend: TrendSeries | null = dashboard.data
    ? points(dashboard.data.capacityTrend, "logicalBytes")
    : null;

  const alertList = (dashboard.data?.alerts ?? []) as OperationsAlert[];
  const otherAlerts = alertList.filter((alert) => alert.code !== UPSTREAM_BLOCKED_CODE);

  return (
    <Stack gap={density.gridSpacing}>
      {/* 顶栏：范围选择 + 状态徽章 + 快捷下拉 + 刷新 */}
      <Group justify="space-between" align="center" wrap="wrap">
        <DashboardRangePicker value={range} onChange={setRange} />
        <Group gap="sm" align="center" wrap="nowrap">
          <StatusBadge status={status.data} loading={status.loading} />
          <QuickMenu onNavigate={(path) => navigate(path)} />
          {lastUpdated ? (
            <Text size="xs" c="dimmed" visibleFrom="sm">
              {t("dashboard.lastUpdated", {
                time: lastUpdated.toLocaleTimeString(currentLocaleTag()),
              })}
            </Text>
          ) : null}
        </Group>
      </Group>

      {dashboard.error || (!dashboard.data && dashboard.refreshError) ? (
        <Alert color="red" title={t("dashboard.title")} icon={<IconAlertTriangle size={18} />}>
          <Stack gap="sm">
            <Text size="sm">{dashboard.error?.message ?? t("common.retryLater")}</Text>
            <Button
              size="xs"
              variant="light"
              w="fit-content"
              onClick={dashboard.reload}
              leftSection={<IconRefresh size={14} />}
            >
              {t("common.retry")}
            </Button>
          </Stack>
        </Alert>
      ) : !dashboard.data ? (
        <DashboardSkeleton />
      ) : (
        <DashboardGrid
          data={dashboard.data}
          requestTrend={requestTrend}
          capacityTrend={capacityTrend}
          otherAlerts={otherAlerts}
          attentions={attentions.data?.items ?? []}
          attentionsLoading={attentions.loading}
          recent={recent.data?.items ?? []}
          recentLoading={recent.loading}
          repos={repos.data?.items ?? []}
          reposLoading={repos.loading}
          onOpenAttention={(attentionId) =>
            navigate(
              attentionId
                ? `/audit-logs?attentionId=${encodeURIComponent(attentionId)}`
                : "/audit-logs",
            )
          }
          onOpenRecent={() => navigate("/audit-logs")}
        />
      )}
    </Stack>
  );
}

/** KPI 指标带：8 项核心数值内联排布，悬停显示口径提示；失败/自动阻止非 0 时标红。 */
function KpiBand({ data, blockedRepos }: { data: OperationsDashboard; blockedRepos: number }) {
  const { t } = useTranslation();
  const { kpi } = data;
  const items = [
    {
      label: t("dashboard.metricsRepositories"),
      value: String(kpi.repositoryCount),
      hint: t("dashboard.metricsRepositoriesHint"),
      danger: false,
      icon: <IconBox size={14} color="var(--mantine-color-blue-6)" />,
    },
    {
      label: t("dashboard.metricsAssets"),
      value: kpi.assetCount.toLocaleString(currentLocaleTag()),
      hint: t("dashboard.metricsAssetsHint"),
      danger: false,
      icon: <IconPackage size={14} color="var(--mantine-color-cyan-6)" />,
    },
    {
      label: t("dashboard.metricsVolume"),
      value: formatBytes(kpi.logicalBytes),
      hint: t("dashboard.metricsVolumeHint"),
      danger: false,
      icon: <IconDatabase size={14} color="var(--mantine-color-teal-6)" />,
    },
    {
      label: t("dashboard.metricsRequests"),
      value: kpi.requestCount.toLocaleString(currentLocaleTag()),
      hint: t("dashboard.metricsRequestsHint"),
      danger: false,
      icon: <IconActivity size={14} color="var(--mantine-color-indigo-6)" />,
    },
    {
      label: t("dashboard.metricsDownloads"),
      value: kpi.downloadCount.toLocaleString(currentLocaleTag()),
      hint: t("dashboard.metricsDownloadsHint"),
      danger: false,
      icon: <IconDownload size={14} color="var(--mantine-color-green-6)" />,
    },
    {
      label: t("dashboard.metricsFailures"),
      value: kpi.failureCount.toLocaleString(currentLocaleTag()),
      hint: t("dashboard.metricsFailuresHint"),
      danger: kpi.failureCount > 0,
      icon: <IconAlertTriangle size={14} color="var(--mantine-color-orange-6)" />,
    },
    {
      label: t("dashboard.metricsCacheRate"),
      value:
        kpi.cacheHitRate === null
          ? t("dashboard.metricsCacheRateNone")
          : `${(kpi.cacheHitRate * 100).toFixed(1)}%`,
      hint: t("dashboard.metricsCacheRateHint"),
      danger: false,
      icon: <IconBolt size={14} color="var(--mantine-color-yellow-6)" />,
    },
    {
      label: t("dashboard.metricsBlockedRepos"),
      value: String(blockedRepos),
      hint: t("dashboard.metricsBlockedReposHint"),
      danger: blockedRepos > 0,
      icon: <IconBan size={14} color="var(--mantine-color-red-6)" />,
    },
  ];
  return (
    <OpsKpiBand
      label={t("dashboard.kpiStripLabel")}
      variant="strip"
      cols={{ base: 2, xs: 4, lg: 8 }}
      items={items}
    />
  );
}

/** 快捷操作：顶栏下拉菜单（原 6 按钮卡片降级，不再常驻占据首屏）。 */
function QuickMenu({ onNavigate }: { onNavigate: (path: string) => void }) {
  const { t } = useTranslation();
  const actions = [
    {
      icon: <IconBox size={16} />,
      label: t("dashboard.actionRepositories"),
      path: "/repositories",
    },
    {
      icon: <IconDownload size={16} />,
      label: t("dashboard.actionMigrations"),
      path: "/migrations",
    },
    { icon: <IconClock size={16} />, label: t("dashboard.actionAuditLogs"), path: "/audit-logs" },
    { icon: <IconBolt size={16} />, label: t("dashboard.actionTokens"), path: "/tokens" },
    {
      icon: <IconClipboardCheck size={16} />,
      label: t("dashboard.actionUsers"),
      path: "/users",
    },
  ];
  return (
    <Menu shadow="md" position="bottom-end" withinPortal>
      <Menu.Target>
        <Button
          variant="default"
          size="xs"
          leftSection={<IconBolt size={14} />}
          rightSection={<IconChevronDown size={12} />}
          aria-label={t("dashboard.tabQuickActions")}
        >
          {t("dashboard.tabQuickActions")}
        </Button>
      </Menu.Target>
      <Menu.Dropdown>
        {actions.map((action) => (
          <Menu.Item
            key={action.path}
            leftSection={action.icon}
            onClick={() => onNavigate(action.path)}
          >
            {action.label}
          </Menu.Item>
        ))}
      </Menu.Dropdown>
    </Menu>
  );
}

function DashboardGrid({
  data,
  requestTrend,
  capacityTrend,
  otherAlerts,
  attentions,
  attentionsLoading,
  recent,
  recentLoading,
  repos,
  reposLoading,
  onOpenAttention,
  onOpenRecent,
}: {
  data: OperationsDashboard;
  requestTrend: {
    request: TrendSeries;
    download: TrendSeries;
    failure: TrendSeries;
  } | null;
  capacityTrend: TrendSeries | null;
  otherAlerts: OperationsAlert[];
  attentions: AuditAttention[];
  attentionsLoading: boolean;
  recent: AuditEvent[];
  recentLoading: boolean;
  repos: Repository[];
  reposLoading: boolean;
  onOpenAttention: (attentionId: string) => void;
  onOpenRecent: () => void;
}) {
  const { t } = useTranslation();
  // KPI 指标带第 8 格：上游被自动阻止的 proxy 仓库数（与仓库状态面板同源）。
  const blockedRepos = repos.filter(
    (repo) => repo.connectionStatus?.status === "AUTO_BLOCKED",
  ).length;
  return (
    <Stack gap={density.gridSpacing}>
      {/* KPI 指标带：全宽置顶，数值内联不挤压 */}
      <KpiBand data={data} blockedRepos={blockedRepos} />
      <Grid gutter={density.gridSpacing} align="stretch">
        {/* 左主区：请求趋势主图 → 容量趋势 → 仓库状态 */}
        <Grid.Col span={{ base: 12, lg: 8 }}>
          <Stack gap={density.gridSpacing}>
            <Card withBorder radius="md" padding={density.cardPadding}>
              <TrendChart
                title={t("dashboard.trendRequests")}
                summary={t("dashboard.trendRequestsSummary")}
                primary={requestTrend?.request ?? []}
                secondary={requestTrend?.download ?? []}
                tertiary={requestTrend?.failure ?? []}
                primaryLabel={t("dashboard.trendRequestsPrimary")}
                secondaryLabel={t("dashboard.trendRequestsSecondary")}
                tertiaryLabel={t("dashboard.trendFailuresPrimary")}
                headerRight={
                  requestTrend?.request?.length ? (
                    <Group gap={6} wrap="nowrap">
                      <IconActivity size={20} color="var(--mantine-color-blue-6)" />
                      <Text size="xl" fw={700} lh={1.1}>
                        {formatCount(
                          requestTrend.request[requestTrend.request.length - 1]?.value ?? 0,
                        )}
                      </Text>
                    </Group>
                  ) : null
                }
              />
            </Card>
            <Card withBorder radius="md" padding={density.cardPadding}>
              <TrendChart
                title={t("dashboard.trendCapacity")}
                summary={t("dashboard.trendCapacitySummary")}
                primary={capacityTrend ?? []}
                primaryLabel={t("dashboard.trendCapacityPrimary")}
                unit="bytes"
                headerRight={
                  capacityTrend?.length ? (
                    <Group gap={6} wrap="nowrap">
                      <IconDatabase size={20} color="var(--mantine-color-teal-6)" />
                      <Text size="xl" fw={700} lh={1.1}>
                        {formatBytes(capacityTrend[capacityTrend.length - 1]?.value ?? 0)}
                      </Text>
                    </Group>
                  ) : null
                }
              />
            </Card>
            <RepositoryStatusPanel repos={repos} loading={reposLoading} />
          </Stack>
        </Grid.Col>

        {/* 右栏信息流：需要处理 + 最近活跃，贯穿左主区全部行 */}
        <Grid.Col span={{ base: 12, lg: 4 }}>
          <Stack gap={density.gridSpacing} h="100%">
            <AttentionPanel
              attentions={attentions}
              loading={attentionsLoading}
              opsAlerts={otherAlerts}
              onOpen={onOpenAttention}
            />
            <RecentActivity
              events={recent}
              loading={recentLoading}
              onOpen={onOpenRecent}
              style={{ flex: 1 }}
            />
          </Stack>
        </Grid.Col>
      </Grid>
    </Stack>
  );
}

function DashboardSkeleton() {
  const { t } = useTranslation();
  return (
    <Stack gap={density.gridSpacing} aria-busy="true" aria-label={t("dashboard.loading")}>
      {/* KPI 指标带：与真实 8 格单卡横带同构，避免数据到达瞬间整页跳动 */}
      <Card withBorder radius="md" padding="sm" component="section">
        <SimpleGrid cols={{ base: 2, xs: 4, lg: 8 }} verticalSpacing="sm" spacing={0}>
          {Array.from({ length: 8 }).map((_, index) => (
            <Stack key={index} gap={2} px="sm">
              <Skeleton height={12} width="70%" radius="sm" />
              <Skeleton height={20} width="52%" radius="sm" />
            </Stack>
          ))}
        </SimpleGrid>
      </Card>
      <Grid gutter={density.gridSpacing} align="stretch">
        <Grid.Col span={{ base: 12, lg: 8 }}>
          <Stack gap={density.gridSpacing}>
            {/* 趋势卡（图 200 + 剖析条）× 2 → 仓库状态面板 */}
            <Skeleton height={340} radius="md" />
            <Skeleton height={340} radius="md" />
            <Skeleton height={300} radius="md" />
          </Stack>
        </Grid.Col>
        <Grid.Col span={{ base: 12, lg: 4 }}>
          <Stack gap={density.gridSpacing} h="100%">
            {/* 需要处理（定高）+ 最近活跃（贯穿剩余高度） */}
            <Skeleton height={260} radius="md" />
            <Skeleton radius="md" style={{ flex: 1, minHeight: 320 }} />
          </Stack>
        </Grid.Col>
      </Grid>
    </Stack>
  );
}

function AttentionPanel({
  attentions,
  loading,
  opsAlerts,
  onOpen,
}: {
  attentions: AuditAttention[];
  loading: boolean;
  opsAlerts: OperationsAlert[];
  onOpen: (attentionId: string) => void;
}) {
  const { t } = useTranslation();
  const LIMIT = 5;
  const actionable = attentions.filter((attention) => attention.unacknowledgedRiskEventCount > 0);
  const visible = actionable.slice(0, LIMIT);
  const hasMore = actionable.length > LIMIT;
  return (
    <Card
      withBorder
      radius="md"
      padding={density.cardPadding}
      style={{ display: "flex", flexDirection: "column" }}
    >
      <Group justify="space-between" mb="xs">
        <Group gap="xs">
          <ThemeIcon variant="light" color="orange" size="sm" radius="md">
            <IconAlertTriangle size={14} />
          </ThemeIcon>
          <Text fw={700}>{t("dashboard.tabAttention")}</Text>
        </Group>
        {hasMore ? (
          <Button
            variant="subtle"
            size="xs"
            onClick={() => onOpen("")}
            aria-label={t("dashboard.attentionViewAll")}
            leftSection={<IconEye size={14} />}
          >
            {t("dashboard.attentionViewAll", { count: actionable.length })}
          </Button>
        ) : null}
      </Group>
      {loading && attentions.length === 0 ? (
        <SkeletonList rows={3} />
      ) : (
        <Stack gap="xs" style={{ flex: 1, minHeight: 0, overflowY: "auto" }}>
          {opsAlerts.length > 0 ? (
            <Alert
              color="yellow"
              icon={<IconAlertTriangle size={18} />}
              title={t("dashboard.attentionOpsAlerts", { count: opsAlerts.length })}
              mb="xs"
            >
              {t("dashboard.attentionOpsAlertsHint")}
            </Alert>
          ) : null}
          {visible.length === 0 && opsAlerts.length === 0 ? (
            <Alert
              color="teal"
              variant="light"
              icon={<IconClipboardCheck size={18} />}
              title={t("dashboard.attentionEmpty")}
            >
              {t("dashboard.attentionEmptyHint")}
            </Alert>
          ) : (
            visible.map((attention) => (
              <UnstyledButton
                key={attention.attentionId}
                onClick={() => onOpen(attention.attentionId)}
                aria-label={attention.action}
                style={{
                  display: "block",
                  padding: "8px 10px",
                  borderRadius: "var(--mantine-radius-md)",
                  border: "1px solid var(--mantine-color-default-border)",
                }}
              >
                <Stack gap={2}>
                  <Group justify="space-between" align="center" wrap="nowrap" gap="xs">
                    <Group gap={8} wrap="nowrap" style={{ minWidth: 0, flex: 1 }}>
                      <ThemeIcon
                        color={severityColor(attention.severity)}
                        variant="light"
                        size="sm"
                        radius="md"
                      >
                        <IconAlertTriangle size={13} />
                      </ThemeIcon>
                      <Text size="sm" fw={600} truncate style={{ minWidth: 0 }}>
                        {attention.action}
                      </Text>
                    </Group>
                    <Badge color={severityColor(attention.severity)} variant="light" size="sm">
                      {t(`auditSeverity.${attention.severity}`, {
                        defaultValue: attention.severity,
                      })}
                    </Badge>
                  </Group>
                  <Text size="xs" c="dimmed" truncate pl={34}>
                    {attention.target?.label}
                    {" · "}
                    {t("dashboard.attentionRiskCount", {
                      count: attention.unacknowledgedRiskEventCount,
                    })}
                  </Text>
                </Stack>
              </UnstyledButton>
            ))
          )}
        </Stack>
      )}
    </Card>
  );
}

/** 紧凑状态徽章：就绪状态 + 版本，用户数收进悬停提示。 */
function StatusBadge({ status, loading }: { status?: StatusInfo | null; loading: boolean }) {
  const { t } = useTranslation();
  if (loading && !status) {
    return <Skeleton height={22} width={104} radius="xl" />;
  }
  if (!status) return null;
  return (
    <Tooltip label={`${t("dashboard.statusUsers")}：${status.userCount}`} openDelay={300}>
      <Badge
        color={status.ready ? "green" : "yellow"}
        variant="light"
        leftSection="●"
        aria-label={t("dashboard.statusReadiness")}
      >
        {`${status.ready ? t("dashboard.statusOnline") : t("dashboard.statusOffline")} · v${status.version || "—"}`}
      </Badge>
    </Tooltip>
  );
}

function RecentActivity({
  events,
  loading,
  onOpen,
  style,
}: {
  events: AuditEvent[];
  loading: boolean;
  onOpen: () => void;
  style?: CSSProperties;
}) {
  const { t } = useTranslation();
  // 右栏贯穿布局：活动列表吃掉剩余高度，超出滚动；条数收敛到 8 避免右栏撑长页面。
  const LIMIT = 8;
  const visible = events.slice(0, LIMIT);
  const hasMore = events.length > LIMIT;
  return (
    <Card
      withBorder
      radius="md"
      padding={density.cardPadding}
      style={{ display: "flex", flexDirection: "column", ...style }}
    >
      <Group justify="space-between" mb="xs">
        <Group gap="xs">
          <ThemeIcon variant="light" color="cyan" size="sm" radius="md">
            <IconHistory size={14} />
          </ThemeIcon>
          <Text fw={700}>{t("dashboard.tabRecent")}</Text>
        </Group>
        {hasMore ? (
          <Button
            variant="subtle"
            size="xs"
            onClick={onOpen}
            aria-label={t("dashboard.recentViewAll")}
            leftSection={<IconEye size={14} />}
          >
            {t("dashboard.recentViewAll")}
          </Button>
        ) : null}
      </Group>
      {loading && events.length === 0 ? (
        <SkeletonList rows={5} />
      ) : events.length === 0 ? (
        <Alert
          color="gray"
          variant="light"
          icon={<IconHistory size={18} />}
          title={t("dashboard.recentEmpty")}
        >
          {t("dashboard.recentEmptyHint")}
        </Alert>
      ) : (
        <Stack gap="xs" style={{ flex: 1, minHeight: 0, overflowY: "auto" }}>
          {visible.map((event) => (
            <UnstyledButton
              key={event.eventId}
              onClick={onOpen}
              aria-label={event.action}
              style={{
                display: "block",
                padding: "8px 10px",
                borderRadius: "var(--mantine-radius-md)",
                border: "1px solid var(--mantine-color-default-border)",
              }}
            >
              <Stack gap={2}>
                <Group justify="space-between" align="center" wrap="nowrap" gap="xs">
                  <Group gap={8} wrap="nowrap" style={{ minWidth: 0, flex: 1 }}>
                    <ThemeIcon
                      color={resultColor(event.result)}
                      variant="light"
                      size="sm"
                      radius="md"
                    >
                      <IconClock size={13} />
                    </ThemeIcon>
                    <Text size="sm" fw={500} truncate style={{ minWidth: 0 }}>
                      {event.action}
                    </Text>
                  </Group>
                  <Badge color={resultColor(event.result)} variant="light" size="sm">
                    {t(`auditResult.${event.result}`, { defaultValue: event.result })}
                  </Badge>
                </Group>
                <Text size="xs" c="dimmed" truncate pl={34}>
                  {event.actor?.displayName ?? "—"}
                  {event.target?.label ? ` · ${event.target.label}` : ""}
                  {" · "}
                  {formatStamp(event.occurredAt)}
                </Text>
              </Stack>
            </UnstyledButton>
          ))}
        </Stack>
      )}
    </Card>
  );
}

function SkeletonList({ rows = 3 }: { rows?: number }) {
  return (
    <Stack gap="sm">
      {Array.from({ length: rows }).map((_, index) => (
        <Skeleton key={index} height={56} radius="md" />
      ))}
    </Stack>
  );
}
