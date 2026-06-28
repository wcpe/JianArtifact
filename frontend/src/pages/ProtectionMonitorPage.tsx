// 防护状态监控页（FR-78，ADR-0017）：展示七层防护各维度窗内计数快照、当前封禁 IP 数与告警列表。
//
// 数据来自后端 GET /api/v1/protection/status 与 /api/v1/protection/alerts（均仅管理员）。
// “实时”用定时轮询刷新快照实现（无需 websocket）；告警列表分页查询，按时间倒序。
// 隐私红线：纯本机内部聚合，不接任何外部遥测 / 导出。

import { useCallback, useEffect, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import {
  SimpleGrid,
  Card,
  Text,
  Title,
  Stack,
  Table,
  Loader,
  Center,
  Badge,
  Group,
} from '@mantine/core';
import * as api from '../api/endpoints';
import type { Paginated, ProtectionAlertDto, ProtectionStatusDto } from '../api/types';
import { errorMessage } from '../lib/format';
import { dimensionLabel, severityColor, severityLabel } from '../lib/protection';
import { ErrorAlert } from '../components/ErrorAlert';

/** 状态快照轮询周期（毫秒）：5 秒刷新一次，足够“实时”而不过度压后端。 */
const POLL_INTERVAL_MS = 5000;
/** 告警列表单页容量。 */
const ALERTS_PAGE_LIMIT = 50;

/** 统计卡片。 */
function StatCard({ label, value }: { label: string; value: string | number }) {
  return (
    <Card withBorder padding="lg" radius="md">
      <Text size="sm" c="dimmed">
        {label}
      </Text>
      <Text fw={700} size="xl">
        {value}
      </Text>
    </Card>
  );
}

/** 防护状态监控页面。 */
export function ProtectionMonitorPage() {
  const { t } = useTranslation('protectionMonitor');
  const [status, setStatus] = useState<ProtectionStatusDto | null>(null);
  const [alerts, setAlerts] = useState<Paginated<ProtectionAlertDto> | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  // 用 ref 保存最新错误判定，避免轮询闭包捕获过期的 setState 依赖
  const loadStatus = useCallback(async () => {
    try {
      const data = await api.protectionStatus();
      setStatus(data);
      setError(null);
    } catch (err) {
      setError(errorMessage(err));
    }
  }, []);

  const loadAlerts = useCallback(async () => {
    try {
      const data = await api.listProtectionAlerts({ limit: ALERTS_PAGE_LIMIT });
      setAlerts(data);
      setError(null);
    } catch (err) {
      setError(errorMessage(err));
    }
  }, []);

  // 首次加载：拉一次状态与告警，结束后解除整页加载态
  useEffect(() => {
    let active = true;
    Promise.all([loadStatus(), loadAlerts()]).finally(() => {
      if (active) setLoading(false);
    });
    return () => {
      active = false;
    };
  }, [loadStatus, loadAlerts]);

  // 轮询：仅刷新状态快照（“实时”维度），告警列表随刷新顺带更新
  const pollRef = useRef(loadStatus);
  pollRef.current = loadStatus;
  useEffect(() => {
    const timer = setInterval(() => {
      void pollRef.current();
    }, POLL_INTERVAL_MS);
    return () => clearInterval(timer);
  }, []);

  if (loading) {
    return (
      <Center h={200}>
        <Loader />
      </Center>
    );
  }

  return (
    <Stack>
      <Title order={2}>{t('title')}</Title>
      <Text c="dimmed">{t('description')}</Text>
      {error && <ErrorAlert message={error} />}

      {status && (
        <>
          <SimpleGrid cols={{ base: 1, sm: 3 }}>
            <StatCard label={t('activeBannedIps')} value={status.active_banned_ips} />
            <StatCard
              label={t('alertsEval')}
              value={status.alerts_enabled ? t('common:enabled') : t('alertsDisabled')}
            />
            <StatCard label={t('windowSecs')} value={status.window_secs} />
          </SimpleGrid>

          <Card withBorder padding="lg" radius="md">
            <Title order={4} mb="sm">
              {t('windowCounts')}
            </Title>
            <SimpleGrid cols={{ base: 2, sm: 3, lg: 5 }}>
              {status.window_counts.map((d) => (
                <Card key={d.dimension} withBorder padding="md" radius="sm">
                  <Text size="sm" c="dimmed">
                    {dimensionLabel(d.dimension)}
                  </Text>
                  <Text fw={700} size="xl">
                    {d.count}
                  </Text>
                </Card>
              ))}
            </SimpleGrid>
          </Card>
        </>
      )}

      <Card withBorder padding="lg" radius="md">
        <Group justify="space-between" mb="sm">
          <Title order={4}>{t('alertList')}</Title>
          {alerts && (
            <Text size="sm" c="dimmed">
              {t('total', { count: alerts.total })}
            </Text>
          )}
        </Group>
        {alerts && alerts.items.length === 0 ? (
          <Text c="dimmed" size="sm">
            {t('noAlerts')}
          </Text>
        ) : (
          <Table.ScrollContainer minWidth={640}>
            <Table striped highlightOnHover>
              <Table.Thead>
                <Table.Tr>
                  <Table.Th>{t('thTime')}</Table.Th>
                  <Table.Th>{t('thDimension')}</Table.Th>
                  <Table.Th>{t('thSeverity')}</Table.Th>
                  <Table.Th ta="right">{t('thObserved')}</Table.Th>
                  <Table.Th ta="right">{t('thThreshold')}</Table.Th>
                  <Table.Th>{t('thDetail')}</Table.Th>
                </Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {alerts?.items.map((a) => (
                  <Table.Tr key={a.id}>
                    <Table.Td>{a.ts}</Table.Td>
                    <Table.Td>{dimensionLabel(a.dimension)}</Table.Td>
                    <Table.Td>
                      <Badge color={severityColor(a.severity)} variant="light">
                        {severityLabel(a.severity)}
                      </Badge>
                    </Table.Td>
                    <Table.Td ta="right">{a.observed_value}</Table.Td>
                    <Table.Td ta="right">{a.threshold}</Table.Td>
                    <Table.Td>{a.detail ?? '—'}</Table.Td>
                  </Table.Tr>
                ))}
              </Table.Tbody>
            </Table>
          </Table.ScrollContainer>
        )}
      </Card>
    </Stack>
  );
}
