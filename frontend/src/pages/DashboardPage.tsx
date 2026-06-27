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
} from '@mantine/core';
import * as api from '../api/endpoints';
import type { DashboardSummary, HostMetrics, AuditEntryDto } from '../api/types';
import { useAuth } from '../auth/useAuth';
import { formatBytes, formatCount, formatUptime, formatRelativeTime } from '../lib/format';
import { density } from '../theme/density';

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
  return (
    <SimpleGrid cols={{ base: 1, sm: 2, lg: 4 }} spacing={density.gridSpacing}>
      <KpiCard label="仓库数" value={formatCount(summary.repo_count)} />
      <KpiCard label="制品数" value={formatCount(summary.artifact_count)} />
      <KpiCard label="存储用量" value={formatBytes(summary.total_bytes)} />
      <KpiCard label="用户数" value={formatCount(summary.user_count)} />
    </SimpleGrid>
  );
}

/** 主机健康卡：CPU / 内存 / 磁盘三条进度条。 */
function HostHealthCard({ host }: { host: HostMetrics }) {
  const memPercent = ratio(host.memory.used_bytes, host.memory.total_bytes);
  const diskUsed = host.disk.total_bytes - host.disk.available_bytes;
  const diskPercent = ratio(diskUsed, host.disk.total_bytes);
  return (
    <Card withBorder padding={density.cardPadding} radius="md">
      <Title order={4} mb="sm">
        主机健康
      </Title>
      <Stack gap="sm">
        <HealthBar
          label="CPU"
          percent={host.cpu.usage_percent}
          detail={`${Math.round(host.cpu.usage_percent)}%`}
        />
        <HealthBar
          label="内存"
          percent={memPercent}
          detail={`${formatBytes(host.memory.used_bytes)} / ${formatBytes(host.memory.total_bytes)}`}
        />
        <HealthBar
          label="磁盘"
          percent={diskPercent}
          detail={`${formatBytes(diskUsed)} / ${formatBytes(host.disk.total_bytes)}`}
        />
      </Stack>
    </Card>
  );
}

/** 近期活动卡：审计最近事件 + 相对时间。 */
function RecentActivityCard({ events }: { events: AuditEntryDto[] }) {
  return (
    <Card withBorder padding={density.cardPadding} radius="md">
      <Title order={4} mb="sm">
        近期活动
      </Title>
      {events.length === 0 ? (
        <Text c="dimmed" size="sm">
          暂无活动记录
        </Text>
      ) : (
        <Stack gap="xs">
          {events.map((e) => (
            <Group key={e.id} justify="space-between" wrap="nowrap">
              <Text size="sm" truncate>
                <Text span fw={600}>
                  {e.action}
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
  return (
    <Card withBorder padding={density.cardPadding} radius="md">
      <Title order={4} mb="sm">
        系统状态
      </Title>
      <Stack gap="xs">
        <StatusRow label="在线更新">
          {status.update.kind === 'available' ? (
            <Badge color="blue" variant="light">
              有新版 {status.update.latest}
            </Badge>
          ) : status.update.kind === 'latest' ? (
            <Badge color="green" variant="light">
              已是最新
            </Badge>
          ) : status.update.kind === 'disabled' ? (
            <Badge color="gray" variant="light">
              未启用
            </Badge>
          ) : (
            <Text size="sm" c="dimmed">
              —
            </Text>
          )}
        </StatusRow>
        <StatusRow label="七层防护">
          {status.protection === 'ok' ? (
            <Badge color="green" variant="light">
              正常
            </Badge>
          ) : status.protection === 'alert' ? (
            <Badge color="orange" variant="light">
              异常
            </Badge>
          ) : (
            <Text size="sm" c="dimmed">
              —
            </Text>
          )}
        </StatusRow>
        <StatusRow label="漏洞库">
          {status.vulnEnabled === null ? (
            <Text size="sm" c="dimmed">
              —
            </Text>
          ) : (
            <Badge color={status.vulnEnabled ? 'green' : 'gray'} variant="light">
              {status.vulnEnabled ? '已启用' : '未启用'}
            </Badge>
          )}
        </StatusRow>
        <StatusRow label="运行时长">
          <Text size="sm" c="dimmed">
            {uptimeSecs === null ? '—' : formatUptime(uptimeSecs)}
          </Text>
        </StatusRow>
      </Stack>
    </Card>
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

  // 各端点独立取数：单项失败仅影响该卡（兜底为空 / —），不拖垮整页。
  useEffect(() => {
    api
      .getDashboardSummary()
      .then(setSummary)
      .catch(() => setSummary(null));
    api
      .getHostMonitor()
      .then(setHost)
      .catch(() => setHost(null));
    api
      .listAudit({ limit: 8 })
      .then((page) => setEvents(page.items))
      .catch(() => setEvents([]));
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
      });
    // 漏洞库启用：取动态配置 vuln.enabled。
    api
      .getDynamicConfig()
      .then((cfg) => setStatus((prev) => ({ ...prev, vulnEnabled: cfg.vuln.enabled })))
      .catch(() => setStatus((prev) => ({ ...prev, vulnEnabled: null })));
    // 七层防护：据状态快照推导正常 / 异常（有活跃封禁或窗内任一维度计数即视为异常）。
    api
      .protectionStatus()
      .then((s) => {
        const active = s.active_banned_ips > 0 || s.window_counts.some((d) => d.count > 0);
        setStatus((prev) => ({ ...prev, protection: active ? 'alert' : 'ok' }));
      })
      .catch(() => setStatus((prev) => ({ ...prev, protection: 'unknown' })));
  }, []);

  return (
    <>
      {summary && <KpiSection summary={summary} />}

      <SimpleGrid cols={{ base: 1, md: 2 }} spacing={density.gridSpacing}>
        {host && <HostHealthCard host={host} />}
        <SystemStatusCard status={status} uptimeSecs={host?.uptime_secs ?? null} />
      </SimpleGrid>

      <RecentActivityCard events={events} />
    </>
  );
}

/** 非管理员 / 匿名降级视图：仅当前用户 + 可见仓库数（不调仅管理员端点）。 */
function BasicDashboard() {
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
      <KpiCard label="可见仓库数" value={formatCount(repoCount)} />
    </SimpleGrid>
  );
}

/** 仪表盘页面：按角色分流到管理员概览或基础降级视图。 */
export function DashboardPage() {
  const { user, isAdmin } = useAuth();

  return (
    <Stack gap={density.gridSpacing}>
      <Title order={2}>仪表盘</Title>
      <Text c="dimmed">
        欢迎，{user?.username}。
        {isAdmin ? '以下为本实例的全局状态概览。' : '以下为当前可见范围内的基础信息。'}
      </Text>
      {isAdmin ? <AdminDashboard /> : <BasicDashboard />}
    </Stack>
  );
}
