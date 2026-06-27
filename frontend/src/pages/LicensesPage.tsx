// 开源许可页（FR-102，ADR-0025）：公开（匿名可访问）展示本产品全部依赖的开源许可归因。
//
// 数据来自后端 GET /api/v1/licenses（公开），由构建期脚本扫描 Rust crates + 前端 npm
// （运行时 + 开发依赖）生成、嵌入二进制；纯本地构建期采集、运行时不外发（守 ADR-0009）。
// 版式：顶部四张统计卡（依赖总数 / 运行时 / 开发 / 许可证种类）+ 按包名过滤搜索框 +
// 按 运行时 / 开发 分组的表格（包名 / 版本 / 许可证 / 作者）。

import { useEffect, useMemo, useState } from 'react';
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
  return (
    <Card withBorder padding="lg" radius="md">
      <Title order={4} mb="sm">
        {title}（{entries.length}）
      </Title>
      {entries.length === 0 ? (
        <Text c="dimmed" size="sm">
          无匹配依赖
        </Text>
      ) : (
        <Table.ScrollContainer minWidth={520}>
          <Table striped highlightOnHover>
            <Table.Thead>
              <Table.Tr>
                <Table.Th>包名</Table.Th>
                <Table.Th>版本</Table.Th>
                <Table.Th>许可证</Table.Th>
                <Table.Th>作者</Table.Th>
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
      <Title order={2}>开源许可</Title>
      <Text c="dimmed">
        本产品依赖的开源组件及其许可证与作者；清单由构建期扫描生成，数据为本机内部、不外发。
      </Text>
      {error && <ErrorAlert message={error} />}

      {data && !data.generated && (
        <Alert color="yellow" title="许可清单未生成">
          当前二进制未嵌入开源许可清单（本地开发未运行生成脚本）。正式发布版会在构建期自动生成并嵌入。
        </Alert>
      )}

      {data && (
        <>
          <SimpleGrid cols={{ base: 2, sm: 4 }}>
            <StatCard label="依赖总数" value={data.summary.total} />
            <StatCard label="运行时依赖" value={data.summary.runtime} />
            <StatCard label="开发依赖" value={data.summary.dev} />
            <StatCard label="许可证种类" value={data.summary.licenses} />
          </SimpleGrid>

          <TextInput
            placeholder="按包名过滤…"
            value={query}
            onChange={(e) => setQuery(e.currentTarget.value)}
            aria-label="按包名过滤"
          />

          <LicenseTable title="运行时依赖" entries={runtime} />
          <LicenseTable title="开发依赖" entries={dev} />
        </>
      )}
    </Stack>
  );
}
