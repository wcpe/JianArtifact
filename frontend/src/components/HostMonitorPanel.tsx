// 主机监控面板（FR-99，消费 FR-98 GET /api/v1/monitor/host）：
// CPU / 内存 / 磁盘占用环形 + 逐盘明细 + uptime；按请求采样（对齐 FR-98 不后台轮询），提供手动刷新。
// 仅 Admin 可达（路由已由 RequireAdmin 守卫）。数据本机内部、不外发。

import { useCallback, useEffect, useState } from 'react';
import {
  Card,
  Text,
  Title,
  Stack,
  Group,
  Button,
  SimpleGrid,
  Table,
  Loader,
  Center,
} from '@mantine/core';
import { IconRefresh } from '@tabler/icons-react';
import * as api from '../api/endpoints';
import type { HostMetrics } from '../api/types';
import { errorMessage, formatBytes } from '../lib/format';
import { ErrorAlert } from './ErrorAlert';
import { RingChart } from './charts/RingChart';

/** 安全占比：分母为 0 时返回 0，避免 NaN。 */
function ratioPercent(used: number, total: number): number {
  return total > 0 ? (used / total) * 100 : 0;
}

/** 把秒数格式化为「Nd Nh Nm Ns」人类可读时长。 */
function formatUptime(secs: number): string {
  const d = Math.floor(secs / 86400);
  const h = Math.floor((secs % 86400) / 3600);
  const m = Math.floor((secs % 3600) / 60);
  const s = secs % 60;
  const parts: string[] = [];
  if (d > 0) parts.push(`${d} 天`);
  if (h > 0) parts.push(`${h} 时`);
  if (m > 0) parts.push(`${m} 分`);
  parts.push(`${s} 秒`);
  return parts.join(' ');
}

/** 主机监控面板。 */
export function HostMonitorPanel() {
  const [metrics, setMetrics] = useState<HostMetrics | null>(null);
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(async () => {
    setRefreshing(true);
    try {
      const data = await api.getHostMonitor();
      setMetrics(data);
      setError(null);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setRefreshing(false);
    }
  }, []);

  // 首次加载：拉一次后解除整页加载态（后续刷新只置 refreshing）
  useEffect(() => {
    let active = true;
    load().finally(() => {
      if (active) setLoading(false);
    });
    return () => {
      active = false;
    };
  }, [load]);

  if (loading) {
    return (
      <Center h={200}>
        <Loader />
      </Center>
    );
  }

  const memUsedPct = metrics
    ? ratioPercent(metrics.memory.used_bytes, metrics.memory.total_bytes)
    : 0;
  const diskUsedBytes = metrics ? metrics.disk.total_bytes - metrics.disk.available_bytes : 0;
  const diskUsedPct = metrics ? ratioPercent(diskUsedBytes, metrics.disk.total_bytes) : 0;

  return (
    <Stack>
      <Group justify="space-between">
        <Text c="dimmed">
          本机基础资源画像（CPU / 内存 / 磁盘 / 运行时长）；按请求采样，数据本机内部、不外发。
        </Text>
        <Button
          variant="light"
          size="xs"
          leftSection={<IconRefresh size={16} />}
          loading={refreshing}
          onClick={() => void load()}
        >
          刷新
        </Button>
      </Group>

      {error && <ErrorAlert message={error} />}

      {metrics && (
        <>
          <Card withBorder padding="lg" radius="md">
            <SimpleGrid cols={{ base: 1, sm: 3 }} spacing="lg">
              <Center>
                <RingChart
                  value={metrics.cpu.usage_percent}
                  label="CPU"
                  caption={`${metrics.cpu.logical_cores} 核`}
                />
              </Center>
              <Center>
                <RingChart
                  value={memUsedPct}
                  label="内存"
                  caption={`${formatBytes(metrics.memory.used_bytes)} / ${formatBytes(metrics.memory.total_bytes)}`}
                />
              </Center>
              <Center>
                <RingChart
                  value={diskUsedPct}
                  label="磁盘"
                  caption={`${formatBytes(diskUsedBytes)} / ${formatBytes(metrics.disk.total_bytes)}`}
                />
              </Center>
            </SimpleGrid>
          </Card>

          <SimpleGrid cols={{ base: 1, sm: 2 }}>
            <Card withBorder padding="lg" radius="md">
              <Text size="sm" c="dimmed">
                系统运行时长
              </Text>
              <Text fw={700} size="xl">
                {formatUptime(metrics.uptime_secs)}
              </Text>
            </Card>
            <Card withBorder padding="lg" radius="md">
              <Text size="sm" c="dimmed">
                交换分区
              </Text>
              <Text fw={700} size="xl">
                {formatBytes(metrics.memory.swap_used_bytes)} /{' '}
                {formatBytes(metrics.memory.swap_total_bytes)}
              </Text>
            </Card>
          </SimpleGrid>

          <Card withBorder padding="lg" radius="md">
            <Title order={4} mb="sm">
              磁盘逐盘明细
            </Title>
            {metrics.disk.disks.length === 0 ? (
              <Text c="dimmed" size="sm">
                未检测到磁盘
              </Text>
            ) : (
              <Table.ScrollContainer minWidth={420}>
                <Table striped highlightOnHover>
                  <Table.Thead>
                    <Table.Tr>
                      <Table.Th>挂载点</Table.Th>
                      <Table.Th ta="right">总容量</Table.Th>
                      <Table.Th ta="right">可用</Table.Th>
                      <Table.Th ta="right">已用</Table.Th>
                    </Table.Tr>
                  </Table.Thead>
                  <Table.Tbody>
                    {metrics.disk.disks.map((d) => (
                      <Table.Tr key={d.mount_point}>
                        <Table.Td>{d.mount_point}</Table.Td>
                        <Table.Td ta="right">{formatBytes(d.total_bytes)}</Table.Td>
                        <Table.Td ta="right">{formatBytes(d.available_bytes)}</Table.Td>
                        <Table.Td ta="right">
                          {formatBytes(d.total_bytes - d.available_bytes)}
                        </Table.Td>
                      </Table.Tr>
                    ))}
                  </Table.Tbody>
                </Table>
              </Table.ScrollContainer>
            )}
          </Card>
        </>
      )}
    </Stack>
  );
}
