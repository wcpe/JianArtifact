// 使用分析数据面板（FR-58，ADR-0009）：展示访问量 / 下载量总览、热门制品、仓库用量。
//
// 数据来自后端 GET /api/v1/analytics/usage（仅管理员），消费 FR-57 采集的本机内部聚合数据。
// 隐私红线：面板只展示本机内部统计，不接任何外部遥测 / 导出。
// 与基础仪表盘（FR-18）相互独立：基础仪表盘只展示基础信息，本页是 P2 的富数据面板。

import { useEffect, useState } from 'react';
import {
  SimpleGrid,
  Card,
  Text,
  Title,
  Stack,
  Table,
  Loader,
  Center,
  Progress,
  Group,
} from '@mantine/core';
import * as api from '../api/endpoints';
import type { UsageAnalyticsDto } from '../api/types';
import { errorMessage } from '../lib/format';
import { ErrorAlert } from '../components/ErrorAlert';

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

/** 使用分析数据面板页面。 */
export function AnalyticsPage() {
  const [data, setData] = useState<UsageAnalyticsDto | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    api
      .usageAnalytics()
      .then(setData)
      .catch((err) => setError(errorMessage(err)))
      .finally(() => setLoading(false));
  }, []);

  if (loading) {
    return (
      <Center h={200}>
        <Loader />
      </Center>
    );
  }

  // 仓库用量进度条以最大计数为基准归一，便于横向对比各仓库占比
  const maxRepoCount = data?.repo_usage.reduce((max, r) => Math.max(max, r.count), 0) ?? 0;

  return (
    <Stack>
      <Title order={2}>使用分析</Title>
      <Text c="dimmed">访问量 / 下载量、热门制品与仓库用量；数据为本机内部统计，不外发。</Text>
      {error && <ErrorAlert message={error} />}

      {data && (
        <>
          <SimpleGrid cols={{ base: 1, sm: 2 }}>
            <StatCard label="累计访问量" value={data.total_access} />
            <StatCard label="累计下载量" value={data.total_download} />
          </SimpleGrid>

          <SimpleGrid cols={{ base: 1, lg: 2 }}>
            <Card withBorder padding="lg" radius="md">
              <Title order={4} mb="sm">
                热门制品（按下载量）
              </Title>
              {data.top_downloads.length === 0 ? (
                <Text c="dimmed" size="sm">
                  暂无下载记录
                </Text>
              ) : (
                <Table.ScrollContainer minWidth={420}>
                  <Table striped highlightOnHover>
                    <Table.Thead>
                      <Table.Tr>
                        <Table.Th>仓库</Table.Th>
                        <Table.Th>制品路径</Table.Th>
                        <Table.Th ta="right">下载量</Table.Th>
                      </Table.Tr>
                    </Table.Thead>
                    <Table.Tbody>
                      {data.top_downloads.map((item) => (
                        <Table.Tr key={`${item.repo_name}/${item.repo_path}`}>
                          <Table.Td>{item.repo_name}</Table.Td>
                          <Table.Td>{item.repo_path || '（仓库级）'}</Table.Td>
                          <Table.Td ta="right">{item.count}</Table.Td>
                        </Table.Tr>
                      ))}
                    </Table.Tbody>
                  </Table>
                </Table.ScrollContainer>
              )}
            </Card>

            <Card withBorder padding="lg" radius="md">
              <Title order={4} mb="sm">
                仓库用量（按下载量）
              </Title>
              {data.repo_usage.length === 0 ? (
                <Text c="dimmed" size="sm">
                  暂无下载记录
                </Text>
              ) : (
                <Stack gap="xs">
                  {data.repo_usage.map((repo) => (
                    <div key={repo.repo_name}>
                      <Group justify="space-between" mb={2}>
                        <Text size="sm">{repo.repo_name}</Text>
                        <Text size="sm" c="dimmed">
                          {repo.count}
                        </Text>
                      </Group>
                      <Progress
                        value={maxRepoCount > 0 ? (repo.count / maxRepoCount) * 100 : 0}
                        size="md"
                        radius="sm"
                      />
                    </div>
                  ))}
                </Stack>
              )}
            </Card>
          </SimpleGrid>
        </>
      )}
    </Stack>
  );
}
