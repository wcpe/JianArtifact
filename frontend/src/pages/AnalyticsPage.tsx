// 使用分析数据面板（FR-58，ADR-0009）：展示访问量 / 下载量总览、热门制品、仓库用量。
//
// 数据来自后端 GET /api/v1/analytics/usage（仅管理员），消费 FR-57 采集的本机内部聚合数据。
// 隐私红线：面板只展示本机内部统计，不接任何外部遥测 / 导出。
// 与基础仪表盘（FR-18）相互独立：基础仪表盘只展示基础信息，本页是 P2 的富数据面板。

import { useEffect, useState } from 'react';
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
  const { t } = useTranslation('analytics');
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
      <Title order={2}>{t('title')}</Title>
      <Text c="dimmed">{t('subtitle')}</Text>
      {error && <ErrorAlert message={error} />}

      {data && (
        <>
          <SimpleGrid cols={{ base: 1, sm: 2 }}>
            <StatCard label={t('totalAccess')} value={data.total_access} />
            <StatCard label={t('totalDownload')} value={data.total_download} />
          </SimpleGrid>

          <SimpleGrid cols={{ base: 1, lg: 2 }}>
            <Card withBorder padding="lg" radius="md">
              <Title order={4} mb="sm">
                {t('topDownloads')}
              </Title>
              {data.top_downloads.length === 0 ? (
                <Text c="dimmed" size="sm">
                  {t('noDownloadRecords')}
                </Text>
              ) : (
                <Table.ScrollContainer minWidth={420}>
                  <Table striped highlightOnHover>
                    <Table.Thead>
                      <Table.Tr>
                        <Table.Th>{t('colRepo')}</Table.Th>
                        <Table.Th>{t('colArtifactPath')}</Table.Th>
                        <Table.Th ta="right">{t('colDownloadCount')}</Table.Th>
                      </Table.Tr>
                    </Table.Thead>
                    <Table.Tbody>
                      {data.top_downloads.map((item) => (
                        <Table.Tr key={`${item.repo_name}/${item.repo_path}`}>
                          <Table.Td>{item.repo_name}</Table.Td>
                          <Table.Td>{item.repo_path || t('repoLevel')}</Table.Td>
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
                {t('repoUsage')}
              </Title>
              {data.repo_usage.length === 0 ? (
                <Text c="dimmed" size="sm">
                  {t('noDownloadRecords')}
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
