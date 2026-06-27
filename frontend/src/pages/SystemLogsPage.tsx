// 系统日志查询页面（FR-107，ADR-0029）：级别过滤 + 刷新 + 分页表格（时间 / 级别 / 消息），仅 Admin。
//
// 数据来自后端 GET /api/v1/system-logs（仅管理员），tail 运行日志文件、最新在前。
// 与审计日志（业务留痕）区分：本页是运行时技术日志（tracing 的 ERROR/WARN/INFO/DEBUG）。
// 路由已由 RequireAdmin 守卫；本页只读展示，不做任何写操作。

import { useEffect, useState } from 'react';
import {
  Title,
  Stack,
  Table,
  Select,
  Button,
  Group,
  Text,
  Loader,
  Center,
  Badge,
  Pagination,
} from '@mantine/core';
import { IconRefresh } from '@tabler/icons-react';
import * as api from '../api/endpoints';
import type { SystemLogEntryDto } from '../api/types';
import { errorMessage } from '../lib/format';
import { ErrorAlert } from '../components/ErrorAlert';

/** 单页容量（对齐后端默认 200）。 */
const PAGE_SIZE = 200;

/** 级别过滤可选项（空串表示全部）。 */
const LEVEL_OPTIONS = [
  { value: '', label: '全部级别' },
  { value: 'ERROR', label: 'ERROR' },
  { value: 'WARN', label: 'WARN' },
  { value: 'INFO', label: 'INFO' },
  { value: 'DEBUG', label: 'DEBUG' },
  { value: 'TRACE', label: 'TRACE' },
];

/** 级别 → 徽章配色：错误红、警告橙、信息蓝、调试灰、追踪灰，无级别灰。 */
function levelColor(level: string | null): string {
  switch (level) {
    case 'ERROR':
      return 'red';
    case 'WARN':
      return 'orange';
    case 'INFO':
      return 'blue';
    case 'DEBUG':
      return 'gray';
    case 'TRACE':
      return 'gray';
    default:
      return 'gray';
  }
}

/** 系统日志查询页面。 */
export function SystemLogsPage() {
  // 已生效的级别过滤（空串=全部）
  const [level, setLevel] = useState('');
  const [entries, setEntries] = useState<SystemLogEntryDto[]>([]);
  const [total, setTotal] = useState(0);
  const [page, setPage] = useState(1);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  // 刷新计数器：变化即触发重新拉取（手动刷新按钮自增）
  const [refreshTick, setRefreshTick] = useState(0);

  // 级别 / 页码 / 刷新变化时重新拉取
  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    setError(null);
    api
      .listSystemLogs({
        level: level || undefined,
        offset: (page - 1) * PAGE_SIZE,
        limit: PAGE_SIZE,
      })
      .then((resp) => {
        if (cancelled) return;
        setEntries(resp.items);
        setTotal(resp.total);
      })
      .catch((err) => {
        if (cancelled) return;
        setError(errorMessage(err));
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [level, page, refreshTick]);

  // 切换级别：回到第一页
  const handleLevelChange = (value: string | null) => {
    setPage(1);
    setLevel(value ?? '');
  };

  const totalPages = Math.max(1, Math.ceil(total / PAGE_SIZE));

  return (
    <Stack>
      <Title order={2}>系统日志</Title>
      <Text c="dimmed">
        应用运行时技术日志（按级别 ERROR / WARN / INFO /
        DEBUG），最新在前。与审计日志（业务留痕）不同。
      </Text>

      <Group align="flex-end">
        <Select
          label="级别"
          data={LEVEL_OPTIONS}
          value={level}
          onChange={handleLevelChange}
          allowDeselect={false}
          w={160}
        />
        <Button
          variant="default"
          leftSection={<IconRefresh size={16} />}
          onClick={() => setRefreshTick((t) => t + 1)}
        >
          刷新
        </Button>
      </Group>

      {error && <ErrorAlert message={error} />}

      {loading ? (
        <Center h={160}>
          <Loader />
        </Center>
      ) : entries.length === 0 ? (
        <Text c="dimmed">暂无日志记录。</Text>
      ) : (
        <Stack>
          <Text size="sm" c="dimmed">
            共 {total} 条记录
          </Text>
          <Table.ScrollContainer minWidth={760}>
            <Table striped highlightOnHover>
              <Table.Thead>
                <Table.Tr>
                  <Table.Th w={220}>时间</Table.Th>
                  <Table.Th w={90}>级别</Table.Th>
                  <Table.Th>消息</Table.Th>
                </Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {entries.map((entry, idx) => (
                  <Table.Tr key={idx}>
                    <Table.Td>{entry.timestamp ?? '—'}</Table.Td>
                    <Table.Td>
                      <Badge variant="light" size="sm" color={levelColor(entry.level)}>
                        {entry.level ?? '—'}
                      </Badge>
                    </Table.Td>
                    <Table.Td style={{ wordBreak: 'break-all' }}>{entry.message}</Table.Td>
                  </Table.Tr>
                ))}
              </Table.Tbody>
            </Table>
          </Table.ScrollContainer>
          {totalPages > 1 && (
            <Group justify="center">
              <Pagination value={page} onChange={setPage} total={totalPages} />
            </Group>
          )}
        </Stack>
      )}
    </Stack>
  );
}
