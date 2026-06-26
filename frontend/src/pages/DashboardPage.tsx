// 仪表盘（FR-18）：仅展示基础信息——当前用户、可见仓库数、格式 / 类型分布。
// 不含访问量 / 下载量等使用分析富面板（属 P2）。

import { useEffect, useState } from 'react';
import { SimpleGrid, Card, Text, Title, Group, Stack, Badge, Loader, Center } from '@mantine/core';
import * as api from '../api/endpoints';
import type { RepositoryDto } from '../api/types';
import { useAuth } from '../auth/useAuth';
import { errorMessage } from '../lib/format';
import { ErrorAlert } from '../components/ErrorAlert';
import { density } from '../theme/density';

/** 统计卡片：按密度基线瘦身（padding 由 lg 收紧为 md）。 */
function StatCard({ label, value }: { label: string; value: string | number }) {
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

/** 按键统计计数。 */
function countBy<T>(items: T[], key: (item: T) => string): Record<string, number> {
  const result: Record<string, number> = {};
  for (const item of items) {
    const k = key(item);
    result[k] = (result[k] ?? 0) + 1;
  }
  return result;
}

/** 仪表盘页面。 */
export function DashboardPage() {
  const { user } = useAuth();
  const [repos, setRepos] = useState<RepositoryDto[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    api
      .listRepositories()
      .then(setRepos)
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

  const byFormat = countBy(repos, (r) => r.format);
  const byType = countBy(repos, (r) => r.type);

  return (
    <Stack gap={density.gridSpacing}>
      <Title order={2}>仪表盘</Title>
      <Text c="dimmed">欢迎，{user?.username}。以下为当前可见范围内的基础信息。</Text>
      {error && <ErrorAlert message={error} />}

      <SimpleGrid cols={{ base: 1, sm: 2, lg: 4 }} spacing={density.gridSpacing}>
        <StatCard label="当前用户" value={user?.username ?? '-'} />
        <StatCard label="角色" value={user?.role === 'admin' ? '管理员' : '用户'} />
        <StatCard label="可见仓库数" value={repos.length} />
        <StatCard label="格式种类" value={Object.keys(byFormat).length} />
      </SimpleGrid>

      <SimpleGrid cols={{ base: 1, md: 2 }} spacing={density.gridSpacing}>
        <Card withBorder padding={density.cardPadding} radius="md">
          <Title order={4} mb="sm">
            格式分布
          </Title>
          {Object.keys(byFormat).length === 0 ? (
            <Text c="dimmed" size="sm">
              暂无可见仓库
            </Text>
          ) : (
            <Group gap="xs">
              {Object.entries(byFormat).map(([fmt, count]) => (
                <Badge key={fmt} size="lg" variant="light">
                  {fmt}：{count}
                </Badge>
              ))}
            </Group>
          )}
        </Card>
        <Card withBorder padding={density.cardPadding} radius="md">
          <Title order={4} mb="sm">
            类型分布
          </Title>
          {Object.keys(byType).length === 0 ? (
            <Text c="dimmed" size="sm">
              暂无可见仓库
            </Text>
          ) : (
            <Group gap="xs">
              {Object.entries(byType).map(([type, count]) => (
                <Badge key={type} size="lg" variant="light" color="grape">
                  {type === 'hosted' ? '托管' : '代理'}：{count}
                </Badge>
              ))}
            </Group>
          )}
        </Card>
      </SimpleGrid>
    </Stack>
  );
}
