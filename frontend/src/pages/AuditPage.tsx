// 审计日志查询页面（FR-77，ADR-0015）：分页表格 + 过滤（操作者 / 动作 / 仓库）+ 行详情，仅 Admin。
//
// 数据来自后端 GET /api/v1/audit（仅管理员），按时间倒序返回写 / 管理 / 授权拒绝类精选事件。
// 路由已由 RequireAdmin 守卫；本页只读展示，不做任何写操作。

import { useEffect, useState, type FormEvent } from 'react';
import {
  Title,
  Stack,
  Table,
  TextInput,
  Button,
  Group,
  Text,
  Loader,
  Center,
  Badge,
  Pagination,
  Modal,
  Code,
} from '@mantine/core';
import { IconSearch } from '@tabler/icons-react';
import * as api from '../api/endpoints';
import type { AuditEntryDto } from '../api/types';
import { errorMessage } from '../lib/format';
import { ErrorAlert } from '../components/ErrorAlert';

/** 单页容量（对齐后端默认 50）。 */
const PAGE_SIZE = 50;

/** 结果 → 徽章配色：成功绿、被拒橙、错误红，其余灰。 */
function resultColor(result: string): string {
  switch (result) {
    case 'success':
      return 'green';
    case 'denied':
      return 'orange';
    case 'error':
      return 'red';
    default:
      return 'gray';
  }
}

/** 审计详情弹窗：展示一条记录的全部字段（含请求 ID / 来源 IP / target / detail）。 */
function AuditDetailModal({
  entry,
  onClose,
}: {
  entry: AuditEntryDto | null;
  onClose: () => void;
}) {
  return (
    <Modal opened={entry !== null} onClose={onClose} title="审计详情" centered size="lg">
      {entry && (
        <Stack gap="xs">
          <DetailRow label="时间" value={entry.ts} />
          <DetailRow label="操作者" value={entry.actor} />
          <DetailRow label="身份种类" value={entry.actor_kind} />
          <DetailRow label="动作" value={entry.action} />
          <DetailRow label="结果" value={entry.result} />
          <DetailRow label="仓库" value={entry.target_repo} />
          <DetailRow label="对象" value={entry.target} />
          <DetailRow label="来源 IP" value={entry.source_ip} />
          <DetailRow label="请求 ID" value={entry.request_id} />
          {entry.detail && (
            <div>
              <Text size="sm" c="dimmed">
                补充
              </Text>
              <Code block>{entry.detail}</Code>
            </div>
          )}
        </Stack>
      )}
    </Modal>
  );
}

/** 详情字段行：空值以占位符展示。 */
function DetailRow({ label, value }: { label: string; value: string | null }) {
  return (
    <Group gap="md" wrap="nowrap" align="flex-start">
      <Text size="sm" c="dimmed" w={80} style={{ flexShrink: 0 }}>
        {label}
      </Text>
      <Text size="sm" style={{ wordBreak: 'break-all' }}>
        {value ?? '—'}
      </Text>
    </Group>
  );
}

/** 审计日志查询页面。 */
export function AuditPage() {
  // 已提交生效的过滤条件（点击查询时从输入框快照）
  const [filter, setFilter] = useState<{ actor: string; action: string; repo: string }>({
    actor: '',
    action: '',
    repo: '',
  });
  // 输入框当前值（未提交）
  const [actorInput, setActorInput] = useState('');
  const [actionInput, setActionInput] = useState('');
  const [repoInput, setRepoInput] = useState('');

  const [entries, setEntries] = useState<AuditEntryDto[]>([]);
  const [total, setTotal] = useState(0);
  const [page, setPage] = useState(1);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [selected, setSelected] = useState<AuditEntryDto | null>(null);

  // 过滤条件或页码变化时重新拉取
  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    setError(null);
    api
      .listAudit({
        actor: filter.actor || undefined,
        action: filter.action || undefined,
        target_repo: filter.repo || undefined,
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
  }, [filter, page]);

  // 提交过滤：回到第一页并应用输入框快照
  const handleSubmit = (e: FormEvent) => {
    e.preventDefault();
    setPage(1);
    setFilter({ actor: actorInput.trim(), action: actionInput.trim(), repo: repoInput.trim() });
  };

  const totalPages = Math.max(1, Math.ceil(total / PAGE_SIZE));

  return (
    <Stack>
      <Title order={2}>审计日志</Title>
      <Text c="dimmed">记录写 / 管理 / 授权拒绝类安全事件，按时间倒序。点击任意行查看详情。</Text>

      <form onSubmit={handleSubmit}>
        <Group align="flex-end">
          <TextInput
            label="操作者"
            placeholder="按用户名过滤"
            value={actorInput}
            onChange={(e) => setActorInput(e.currentTarget.value)}
          />
          <TextInput
            label="动作"
            placeholder="如 repo.create"
            value={actionInput}
            onChange={(e) => setActionInput(e.currentTarget.value)}
          />
          <TextInput
            label="仓库"
            placeholder="按仓库名过滤"
            value={repoInput}
            onChange={(e) => setRepoInput(e.currentTarget.value)}
          />
          <Button type="submit" leftSection={<IconSearch size={16} />}>
            查询
          </Button>
        </Group>
      </form>

      {error && <ErrorAlert message={error} />}

      {loading ? (
        <Center h={160}>
          <Loader />
        </Center>
      ) : entries.length === 0 ? (
        <Text c="dimmed">暂无审计记录。</Text>
      ) : (
        <Stack>
          <Text size="sm" c="dimmed">
            共 {total} 条记录
          </Text>
          <Table.ScrollContainer minWidth={760}>
            <Table striped highlightOnHover>
              <Table.Thead>
                <Table.Tr>
                  <Table.Th>时间</Table.Th>
                  <Table.Th>操作者</Table.Th>
                  <Table.Th>动作</Table.Th>
                  <Table.Th>仓库</Table.Th>
                  <Table.Th>结果</Table.Th>
                </Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {entries.map((entry) => (
                  <Table.Tr
                    key={entry.id}
                    onClick={() => setSelected(entry)}
                    style={{ cursor: 'pointer' }}
                  >
                    <Table.Td>{entry.ts}</Table.Td>
                    <Table.Td>{entry.actor}</Table.Td>
                    <Table.Td>{entry.action}</Table.Td>
                    <Table.Td>{entry.target_repo ?? '—'}</Table.Td>
                    <Table.Td>
                      <Badge variant="light" size="sm" color={resultColor(entry.result)}>
                        {entry.result}
                      </Badge>
                    </Table.Td>
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

      <AuditDetailModal entry={selected} onClose={() => setSelected(null)} />
    </Stack>
  );
}
