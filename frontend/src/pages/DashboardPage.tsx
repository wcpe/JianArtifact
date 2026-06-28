// 仪表盘全局状态概览（FR-108，增强 FR-18）：一眼看清这台实例的全局状态。
//
// 管理员视图：顶部 4 张 KPI 卡（仓库 / 制品 / 存储用量 / 用户数）+ 主机健康（CPU/内存/磁盘 进度条）
// + 近期活动（审计最近事件）+ 系统状态（在线更新 / 七层防护 / 漏洞库 / 运行时长）。
// 数字一律格式化：字节走 formatBytes（人类可读）、计数千分位、uptime 人类可读。
// 非管理员 / 匿名视图：降级为基础信息（当前用户 + 可见仓库数），不调任何仅管理员端点。
//
// 数据源全部复用既有端点，仅 KPI 四元组经新增薄端点 GET /api/v1/dashboard/summary 聚合
// （制品数 / 去重存储字节无法低成本从既有前端端点拿到）。各仅管理员端点各自独立取数与兜底，
// 单项失败只让该卡显空 / 错，不拖垮整页。本机内部数据、不外发。

import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import {
  SimpleGrid,
  Card,
  Text,
  Title,
  Group,
  Stack,
  Badge,
  Progress,
  Loader,
  Center,
  Skeleton,
  Transition,
} from '@mantine/core';
import * as api from '../api/endpoints';
import type { DashboardSummary, HostMetrics, AuditEntryDto } from '../api/types';
import { useAuth } from '../auth/useAuth';
import { formatBytes, formatCount, formatUptime, formatRelativeTime } from '../lib/format';
import { density } from '../theme/density';
import { TopProgressBar } from '../components/TopProgressBar';
import { tAuditAction } from '../i18n';

/** 主机监控前台轮询间隔（FR-112）：每 5 秒刷新一次主机健康。 */
const HOST_POLL_INTERVAL_MS = 5000;

/** 系统状态四项的语义取值（与渲染解耦，便于各端点独立兜底）。 */
interface SystemStatus {
  /** 在线更新：有新版 latest 串 / 已是最新 / 未启用 / 未知。 */
  update: { kind: 'available'; latest: string } | { kind: 'latest' | 'disabled' | 'unknown' };
  /** 七层防护：正常 / 异常（据活跃封禁数或窗内计数推导）/ 未知。 */
  protection: 'ok' | 'alert' | 'unknown';
  /** 漏洞库是否启用（未知为 null）。 */
  vulnEnabled: boolean | null;
}

/** KPI 统计卡（值已格式化好的展示串）。 */
function KpiCard({ label, value }: { label: string; value: string }) {
  return (
    <Card withBorder padding={density.cardPadding} radius="md">
      <Text size="sm" c="dimmed">
        {label}
      </Text>
      <Text fw={700} size="xl">
        {value}
      </Text>
    </Card>
  );
}

/** 单条带标签 + 百分比的进度条（主机健康用）。 */
function HealthBar({ label, percent, detail }: { label: string; percent: number; detail: string }) {
  const value = Math.min(100, Math.max(0, Math.round(percent)));
  return (
    <div>
      <Group justify="space-between" mb={2}>
        <Text size="sm">{label}</Text>
        <Text size="sm" c="dimmed">
          {detail}
        </Text>
      </Group>
      <Progress value={value} size="md" radius="sm" color={value >= 90 ? 'red' : undefined} />
    </div>
  );
}

/** 主机使用率百分比（避免除零）。 */
function ratio(used: number, total: number): number {
  return total > 0 ? (used / total) * 100 : 0;
}

/** 管理员 KPI 区：4 张卡，存储用量人类可读、计数千分位。 */
function KpiSection({ summary }: { summary: DashboardSummary }) {
  const { t } = useTranslation('dashboard');
  return (
    <SimpleGrid cols={{ base: 1, sm: 2, lg: 4 }} spacing={density.gridSpacing}>
      <KpiCard label={t('repoCount')} value={formatCount(summary.repo_count)} />
      <KpiCard label={t('artifactCount')} value={formatCount(summary.artifact_count)} />
      <KpiCard label={t('storageUsage')} value={formatBytes(summary.total_bytes)} />
      <KpiCard label={t('userCount')} value={formatCount(summary.user_count)} />
    </SimpleGrid>
  );
}

/** 主机健康卡：CPU / 内存 / 磁盘三条进度条。 */
function HostHealthCard({ host }: { host: HostMetrics }) {
  const { t } = useTranslation('dashboard');
  const memPercent = ratio(host.memory.used_bytes, host.memory.total_bytes);
  const diskUsed = host.disk.total_bytes - host.disk.available_bytes;
  const diskPercent = ratio(diskUsed, host.disk.total_bytes);
  return (
    <Card withBorder padding={density.cardPadding} radius="md">
      <Title order={4} mb="sm">
        {t('hostHealth')}
      </Title>
      <Stack gap="sm">
        <HealthBar
          label={t('cpu')}
          percent={host.cpu.usage_percent}
          detail={`${Math.round(host.cpu.usage_percent)}%`}
        />
        <HealthBar
          label={t('memory')}
          percent={memPercent}
          detail={`${formatBytes(host.memory.used_bytes)} / ${formatBytes(host.memory.total_bytes)}`}
        />
        <HealthBar
          label={t('disk')}
          percent={diskPercent}
          detail={`${formatBytes(diskUsed)} / ${formatBytes(host.disk.total_bytes)}`}
        />
      </Stack>
    </Card>
  );
}

/** 近期活动卡：审计最近事件 + 相对时间。 */
function RecentActivityCard({ events }: { events: AuditEntryDto[] }) {
  const { t } = useTranslation('dashboard');
  return (
    <Card withBorder padding={density.cardPadding} radius="md">
      <Title order={4} mb="sm">
        {t('recentActivity')}
      </Title>
      {events.length === 0 ? (
        <Text c="dimmed" size="sm">
          {t('noActivity')}
        </Text>
      ) : (
        <Stack gap="xs">
          {events.map((e) => (
            <Group key={e.id} justify="space-between" wrap="nowrap">
              <Text size="sm" truncate>
                <Text span fw={600}>
                  {tAuditAction(e.action)}
                </Text>{' '}
                {e.actor}
                {e.target_repo ? (
                  <Text span c="dimmed">
                    {' '}
                    @{e.target_repo}
                  </Text>
                ) : null}
              </Text>
              <Text size="xs" c="dimmed" style={{ whiteSpace: 'nowrap' }}>
                {formatRelativeTime(e.ts)}
              </Text>
            </Group>
          ))}
        </Stack>
      )}
    </Card>
  );
}

/** 系统状态单项行（标签 + 状态徽章 / 文本）。 */
function StatusRow({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <Group justify="space-between">
      <Text size="sm">{label}</Text>
      {children}
    </Group>
  );
}

/** 系统状态卡：在线更新 / 七层防护 / 漏洞库 / 运行时长。 */
function SystemStatusCard({
  status,
  uptimeSecs,
}: {
  status: SystemStatus;
  uptimeSecs: number | null;
}) {
  const { t } = useTranslation('dashboard');
  return (
    <Card withBorder padding={density.cardPadding} radius="md">
      <Title order={4} mb="sm">
        {t('systemStatus')}
      </Title>
      <Stack gap="xs">
        <StatusRow label={t('onlineUpdate')}>
          {status.update.kind === 'available' ? (
            <Badge color="blue" variant="light">
              {t('updateAvailable', { version: status.update.latest })}
            </Badge>
          ) : status.update.kind === 'latest' ? (
            <Badge color="green" variant="light">
              {t('updateLatest')}
            </Badge>
          ) : status.update.kind === 'disabled' ? (
            <Badge color="gray" variant="light">
              {t('updateDisabled')}
            </Badge>
          ) : (
            <Text size="sm" c="dimmed">
              —
            </Text>
          )}
        </StatusRow>
        <StatusRow label={t('protection')}>
          {status.protection === 'ok' ? (
            <Badge color="green" variant="light">
              {t('protectionOk')}
            </Badge>
          ) : status.protection === 'alert' ? (
            <Badge color="orange" variant="light">
              {t('protectionAlert')}
            </Badge>
          ) : (
            <Text size="sm" c="dimmed">
              —
            </Text>
          )}
        </StatusRow>
        <StatusRow label={t('vulnDb')}>
          {status.vulnEnabled === null ? (
            <Text size="sm" c="dimmed">
              —
            </Text>
          ) : (
            <Badge color={status.vulnEnabled ? 'green' : 'gray'} variant="light">
              {status.vulnEnabled ? t('vulnEnabled') : t('vulnDisabled')}
            </Badge>
          )}
        </StatusRow>
        <StatusRow label={t('uptime')}>
          <Text size="sm" c="dimmed">
            {uptimeSecs === null ? '—' : formatUptime(uptimeSecs)}
          </Text>
        </StatusRow>
      </Stack>
    </Card>
  );
}

/** KPI 区加载占位（FR-112）：保留 4 张卡布局高度，消除「卡片从上挤出」的抖动。 */
function KpiSkeleton() {
  return (
    <SimpleGrid cols={{ base: 1, sm: 2, lg: 4 }} spacing={density.gridSpacing}>
      {Array.from({ length: 4 }).map((_, i) => (
        <Skeleton key={i} height={78} radius="md" />
      ))}
    </SimpleGrid>
  );
}

/** 管理员仪表盘：聚合 KPI + 主机健康 + 近期活动 + 系统状态。 */
function AdminDashboard() {
  const [summary, setSummary] = useState<DashboardSummary | null>(null);
  const [host, setHost] = useState<HostMetrics | null>(null);
  const [events, setEvents] = useState<AuditEntryDto[]>([]);
  const [status, setStatus] = useState<SystemStatus>({
    update: { kind: 'unknown' },
    protection: 'unknown',
    vulnEnabled: null,
  });
  // 首屏加载态（FR-112）：驱动顶部进度条、骨架占位与内容淡入；首批端点全部 settle 后转 false。
  const [loading, setLoading] = useState(true);

  // 各端点独立取数：单项失败仅影响该卡（兜底为空 / —），不拖垮整页。
  useEffect(() => {
    const tasks = [
      api
        .getDashboardSummary()
        .then(setSummary)
        .catch(() => setSummary(null)),
      api
        .getHostMonitor()
        .then(setHost)
        .catch(() => setHost(null)),
      api
        .listAudit({ limit: 8 })
        .then((page) => setEvents(page.items))
        .catch(() => setEvents([])),
      // 在线更新：未启用返回 409，按「未启用」处理而非报错；其余失败按未知。
      api
        .checkUpdate()
        .then((c) =>
          setStatus((prev) => ({
            ...prev,
            update: c.update_available
              ? { kind: 'available', latest: c.latest_version }
              : { kind: 'latest' },
          })),
        )
        .catch((err) => {
          const disabled = err && typeof err === 'object' && 'status' in err && err.status === 409;
          setStatus((prev) => ({ ...prev, update: { kind: disabled ? 'disabled' : 'unknown' } }));
        }),
      // 漏洞库启用：取动态配置 vuln.enabled。
      api
        .getDynamicConfig()
        .then((cfg) => setStatus((prev) => ({ ...prev, vulnEnabled: cfg.vuln.enabled })))
        .catch(() => setStatus((prev) => ({ ...prev, vulnEnabled: null }))),
      // 七层防护：据状态快照推导正常 / 异常（有活跃封禁或窗内任一维度计数即视为异常）。
      api
        .protectionStatus()
        .then((s) => {
          const active = s.active_banned_ips > 0 || s.window_counts.some((d) => d.count > 0);
          setStatus((prev) => ({ ...prev, protection: active ? 'alert' : 'ok' }));
        })
        .catch(() => setStatus((prev) => ({ ...prev, protection: 'unknown' }))),
    ];
    void Promise.allSettled(tasks).then(() => setLoading(false));
  }, []);

  // 主机健康前台 5s 实时轮询（FR-112）：页面不可见时暂停、卸载时清理，不再卡为一次性。
  useEffect(() => {
    let timer: ReturnType<typeof setInterval> | null = null;
    const refresh = () => {
      api
        .getHostMonitor()
        .then(setHost)
        .catch(() => {
          // 轮询失败保留上一帧数据，不闪空、不报错；下个周期再试。
        });
    };
    const start = () => {
      if (timer === null) timer = setInterval(refresh, HOST_POLL_INTERVAL_MS);
    };
    const stop = () => {
      if (timer !== null) {
        clearInterval(timer);
        timer = null;
      }
    };
    const onVisibility = () => {
      if (document.visibilityState === 'visible') {
        refresh(); // 回到前台立即补一帧，避免等满一个周期才更新。
        start();
      } else {
        stop();
      }
    };
    if (document.visibilityState === 'visible') start();
    document.addEventListener('visibilitychange', onVisibility);
    return () => {
      document.removeEventListener('visibilitychange', onVisibility);
      stop();
    };
  }, []);

  return (
    <>
      <TopProgressBar loading={loading} />

      {loading ? (
        <KpiSkeleton />
      ) : (
        <Transition mounted transition="fade" duration={300} timingFunction="ease">
          {(transitionStyle) => (
            <Stack gap={density.gridSpacing} style={transitionStyle}>
              {summary && <KpiSection summary={summary} />}

              <SimpleGrid cols={{ base: 1, md: 2 }} spacing={density.gridSpacing}>
                {host && <HostHealthCard host={host} />}
                <SystemStatusCard status={status} uptimeSecs={host?.uptime_secs ?? null} />
              </SimpleGrid>

              <RecentActivityCard events={events} />
            </Stack>
          )}
        </Transition>
      )}
    </>
  );
}

/** 非管理员 / 匿名降级视图：仅当前用户 + 可见仓库数（不调仅管理员端点）。 */
function BasicDashboard() {
  const { t } = useTranslation('dashboard');
  const [repoCount, setRepoCount] = useState<number | null>(null);

  useEffect(() => {
    api
      .listRepositories()
      .then((repos) => setRepoCount(repos.length))
      .catch(() => setRepoCount(null));
  }, []);

  if (repoCount === null) {
    return (
      <Center h={120}>
        <Loader />
      </Center>
    );
  }

  return (
    <SimpleGrid cols={{ base: 1, sm: 2 }} spacing={density.gridSpacing}>
      <KpiCard label={t('visibleRepoCount')} value={formatCount(repoCount)} />
    </SimpleGrid>
  );
}

/** 仪表盘页面：按角色分流到管理员概览或基础降级视图。 */
export function DashboardPage() {
  const { t } = useTranslation('dashboard');
  const { user, isAdmin } = useAuth();

  return (
    // position: relative 让顶部进度条（绝对定位）锚定在仪表盘内容区顶部。
    <Stack gap={density.gridSpacing} style={{ position: 'relative' }}>
      <Title order={2}>{t('title')}</Title>
      <Text c="dimmed">
        {t('welcome', { username: user?.username })}
        {isAdmin ? t('welcomeAdmin') : t('welcomeBasic')}
      </Text>
      {isAdmin ? <AdminDashboard /> : <BasicDashboard />}
    </Stack>
  );
}
