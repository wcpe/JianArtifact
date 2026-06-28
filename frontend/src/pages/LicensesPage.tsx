// 开源许可页（FR-102，ADR-0025）：公开（匿名可访问）展示本产品全部依赖的开源许可归因。
//
// 数据来自后端 GET /api/v1/licenses（公开），由构建期脚本扫描 Rust crates + 前端 npm
// （运行时 + 开发依赖）生成、嵌入二进制；纯本地构建期采集、运行时不外发（守 ADR-0009）。
// 版式：顶部四张统计卡（依赖总数 / 运行时 / 开发 / 许可证种类）+ 按包名过滤搜索框 +
// 按 运行时 / 开发 分组的表格（包名 / 版本 / 许可证 / 作者）。

import { useEffect, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import {
  SimpleGrid,
  Card,
  Text,
  Title,
  Stack,
  Table,
  TextInput,
  Loader,
  Center,
  Alert,
} from '@mantine/core';
import * as api from '../api/endpoints';
import type { LicenseEntry, LicenseManifest } from '../api/types';
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

/** 一组依赖的表格（按运行时 / 开发分组）。 */
function LicenseTable({ title, entries }: { title: string; entries: LicenseEntry[] }) {
  const { t } = useTranslation('licenses');
  return (
    <Card withBorder padding="lg" radius="md">
      <Title order={4} mb="sm">
        {title}（{entries.length}）
      </Title>
      {entries.length === 0 ? (
        <Text c="dimmed" size="sm">
          {t('noMatch')}
        </Text>
      ) : (
        <Table.ScrollContainer minWidth={520}>
          <Table striped highlightOnHover>
            <Table.Thead>
              <Table.Tr>
                <Table.Th>{t('colName')}</Table.Th>
                <Table.Th>{t('colVersion')}</Table.Th>
                <Table.Th>{t('colLicense')}</Table.Th>
                <Table.Th>{t('colAuthor')}</Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {entries.map((e) => (
                <Table.Tr key={`${e.source}:${e.name}@${e.version}`}>
                  <Table.Td>{e.name}</Table.Td>
                  <Table.Td>{e.version}</Table.Td>
                  <Table.Td>{e.license || '—'}</Table.Td>
                  <Table.Td>{e.author || '—'}</Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        </Table.ScrollContainer>
      )}
    </Card>
  );
}

/** 开源许可页。 */
export function LicensesPage() {
  const { t } = useTranslation('licenses');
  const [data, setData] = useState<LicenseManifest | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [query, setQuery] = useState('');

  useEffect(() => {
    api
      .getLicenses()
      .then(setData)
      .catch((err) => setError(errorMessage(err)))
      .finally(() => setLoading(false));
  }, []);

  // 按包名过滤（大小写不敏感）；空查询返回全部
  const filtered = useMemo(() => {
    const entries = data?.entries ?? [];
    const q = query.trim().toLowerCase();
    if (!q) return entries;
    return entries.filter((e) => e.name.toLowerCase().includes(q));
  }, [data, query]);

  const runtime = filtered.filter((e) => e.kind === 'runtime');
  const dev = filtered.filter((e) => e.kind === 'dev');

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

      {data && !data.generated && (
        <Alert color="yellow" title={t('notGeneratedTitle')}>
          {t('notGeneratedBody')}
        </Alert>
      )}

      {data && (
        <>
          <SimpleGrid cols={{ base: 2, sm: 4 }}>
            <StatCard label={t('statTotal')} value={data.summary.total} />
            <StatCard label={t('runtimeDeps')} value={data.summary.runtime} />
            <StatCard label={t('devDeps')} value={data.summary.dev} />
            <StatCard label={t('statLicenses')} value={data.summary.licenses} />
          </SimpleGrid>

          <TextInput
            placeholder={t('filterPlaceholder')}
            value={query}
            onChange={(e) => setQuery(e.currentTarget.value)}
            aria-label={t('filterAriaLabel')}
          />

          <LicenseTable title={t('runtimeDeps')} entries={runtime} />
          <LicenseTable title={t('devDeps')} entries={dev} />
        </>
      )}
    </Stack>
  );
}
