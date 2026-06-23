// 跨仓库制品搜索界面（FR-22）：按关键字 / 坐标搜索，结果按读权限过滤（后端保证）。

import { useState, type FormEvent } from 'react';
import {
  TextInput,
  Button,
  Group,
  Title,
  Stack,
  Table,
  Select,
  Text,
  Loader,
  Center,
  Anchor,
  Badge,
  Pagination,
} from '@mantine/core';
import { IconSearch } from '@tabler/icons-react';
import { useNavigate } from 'react-router-dom';
import * as api from '../api/endpoints';
import type { RepoFormat, SearchHit } from '../api/types';
import { errorMessage, formatBytes } from '../lib/format';
import { ErrorAlert } from '../components/ErrorAlert';

const PAGE_SIZE = 20;

const FORMAT_FILTER: { value: string; label: string }[] = [
  { value: '', label: '全部格式' },
  { value: 'maven', label: 'Maven' },
  { value: 'npm', label: 'npm' },
  { value: 'docker', label: 'Docker / OCI' },
  { value: 'raw', label: 'Raw' },
];

/** 制品搜索页面。 */
export function SearchPage() {
  const navigate = useNavigate();
  const [query, setQuery] = useState('');
  const [format, setFormat] = useState('');
  const [hits, setHits] = useState<SearchHit[]>([]);
  const [total, setTotal] = useState(0);
  const [page, setPage] = useState(1);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [searched, setSearched] = useState(false);

  const runSearch = async (targetPage: number) => {
    if (!query.trim()) return;
    setLoading(true);
    setError(null);
    try {
      const resp = await api.search(query.trim(), {
        format: format ? (format as RepoFormat) : undefined,
        offset: (targetPage - 1) * PAGE_SIZE,
        limit: PAGE_SIZE,
      });
      setHits(resp.items);
      setTotal(resp.total);
      setPage(targetPage);
      setSearched(true);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setLoading(false);
    }
  };

  const handleSubmit = (e: FormEvent) => {
    e.preventDefault();
    runSearch(1);
  };

  const totalPages = Math.max(1, Math.ceil(total / PAGE_SIZE));

  return (
    <Stack>
      <Title order={2}>制品搜索</Title>
      <form onSubmit={handleSubmit}>
        <Group align="flex-end">
          <TextInput
            label="关键字 / 坐标"
            placeholder="按制品路径关键字搜索"
            value={query}
            onChange={(e) => setQuery(e.currentTarget.value)}
            flex={1}
          />
          <Select
            label="格式"
            data={FORMAT_FILTER}
            value={format}
            onChange={(v) => setFormat(v ?? '')}
            allowDeselect={false}
            maw={160}
          />
          <Button type="submit" leftSection={<IconSearch size={16} />} disabled={!query.trim()}>
            搜索
          </Button>
        </Group>
      </form>

      {error && <ErrorAlert message={error} />}

      {loading ? (
        <Center h={120}>
          <Loader />
        </Center>
      ) : searched ? (
        hits.length === 0 ? (
          <Text c="dimmed">未找到匹配的制品。</Text>
        ) : (
          <Stack>
            <Text size="sm" c="dimmed">
              共 {total} 条结果
            </Text>
            <Table.ScrollContainer minWidth={680}>
              <Table striped highlightOnHover>
                <Table.Thead>
                  <Table.Tr>
                    <Table.Th>仓库</Table.Th>
                    <Table.Th>格式</Table.Th>
                    <Table.Th>路径</Table.Th>
                    <Table.Th>大小</Table.Th>
                  </Table.Tr>
                </Table.Thead>
                <Table.Tbody>
                  {hits.map((hit) => (
                    <Table.Tr key={`${hit.repo_id}/${hit.path}`}>
                      <Table.Td>{hit.repo_name}</Table.Td>
                      <Table.Td>
                        <Badge variant="light" size="sm">
                          {hit.format}
                        </Badge>
                      </Table.Td>
                      <Table.Td>
                        <Anchor
                          onClick={() =>
                            navigate(
                              `/artifact?repo=${encodeURIComponent(hit.repo_id)}&path=${encodeURIComponent(hit.path)}`,
                            )
                          }
                        >
                          {hit.path}
                        </Anchor>
                      </Table.Td>
                      <Table.Td>{formatBytes(hit.size)}</Table.Td>
                    </Table.Tr>
                  ))}
                </Table.Tbody>
              </Table>
            </Table.ScrollContainer>
            {totalPages > 1 && (
              <Group justify="center">
                <Pagination value={page} onChange={(p) => runSearch(p)} total={totalPages} />
              </Group>
            )}
          </Stack>
        )
      ) : (
        <Text c="dimmed">输入关键字开始搜索。</Text>
      )}
    </Stack>
  );
}
