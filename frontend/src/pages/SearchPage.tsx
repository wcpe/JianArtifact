// 跨仓库制品搜索界面（FR-22/67/94）：搜索入口前移到页眉，本页经 URL ?q= 自动搜索，
// 结果按仓库分组 → 路径层级文件夹树展示、每个仓库 / 文件按格式渲染专属 icon；
// 结果按读权限过滤（后端保证）。

import { useEffect, useMemo, useState, type FormEvent } from 'react';
import {
  TextInput,
  Button,
  Group,
  Title,
  Stack,
  Select,
  Text,
  Loader,
  Center,
  Anchor,
  Collapse,
  UnstyledButton,
  Pagination,
} from '@mantine/core';
import {
  IconSearch,
  IconChevronRight,
  IconChevronDown,
  IconFolder,
  IconFolderOpen,
} from '@tabler/icons-react';
import { useNavigate, useSearchParams } from 'react-router-dom';
import * as api from '../api/endpoints';
import type { RepoFormat } from '../api/types';
import { buildSearchTree, type SearchRepoGroup, type SearchTreeNode } from '../lib/searchTree';
import { FormatIcon } from '../lib/formatIcon';
import { errorMessage, formatBytes } from '../lib/format';
import { density } from '../theme/density';
import { ErrorAlert } from '../components/ErrorAlert';

/** 每层缩进像素（与 FR-93 仓库详情树一致的层级观感）。 */
const INDENT_STEP = 16;

/** 收集一棵树里全部目录前缀（用于默认全展开，结果一眼可见）。 */
function collectFolderPaths(nodes: SearchTreeNode[], acc: Set<string>): Set<string> {
  for (const node of nodes) {
    if (node.type === 'folder') {
      acc.add(node.path);
      collectFolderPaths(node.children, acc);
    }
  }
  return acc;
}

const PAGE_SIZE = 20;

const FORMAT_FILTER: { value: string; label: string }[] = [
  { value: '', label: '全部格式' },
  { value: 'maven', label: 'Maven' },
  { value: 'npm', label: 'npm' },
  { value: 'docker', label: 'Docker / OCI' },
  { value: 'raw', label: 'Raw' },
];

/** 统计一棵树的命中（文件叶子）总数，用于分组节点显示「N 项」。 */
function countFiles(nodes: SearchTreeNode[]): number {
  let n = 0;
  for (const node of nodes) {
    if (node.type === 'file') n += 1;
    else n += countFiles(node.children);
  }
  return n;
}

/** 递归渲染搜索树一层节点：目录可逐级展开、文件点击进详情。 */
function TreeNodes({
  nodes,
  format,
  depth,
  expanded,
  onToggle,
  onOpenFile,
}: {
  nodes: SearchTreeNode[];
  format: RepoFormat;
  depth: number;
  expanded: Set<string>;
  onToggle: (path: string) => void;
  onOpenFile: (path: string) => void;
}) {
  return (
    <Stack gap={2}>
      {nodes.map((node) => {
        const indent = depth * INDENT_STEP;
        if (node.type === 'folder') {
          const open = expanded.has(node.path);
          return (
            <div key={`d:${node.path}`}>
              <UnstyledButton
                onClick={() => onToggle(node.path)}
                px={6}
                py={4}
                style={{ width: '100%', borderRadius: 4, paddingLeft: indent + 6 }}
              >
                <Group gap={4} wrap="nowrap">
                  {open ? <IconChevronDown size={14} /> : <IconChevronRight size={14} />}
                  {open ? <IconFolderOpen size={16} /> : <IconFolder size={16} />}
                  <Text size="sm" truncate>
                    {node.name}
                  </Text>
                </Group>
              </UnstyledButton>
              {open && (
                <TreeNodes
                  nodes={node.children}
                  format={format}
                  depth={depth + 1}
                  expanded={expanded}
                  onToggle={onToggle}
                  onOpenFile={onOpenFile}
                />
              )}
            </div>
          );
        }
        // 文件叶子：格式 icon + 文件名 + 大小，点击进详情
        return (
          <Group
            key={`f:${node.path}`}
            gap={density.inlineGap}
            wrap="nowrap"
            style={{ paddingLeft: indent + 6 }}
          >
            <FormatIcon format={format} />
            <Anchor size="sm" onClick={() => onOpenFile(node.path)}>
              {node.name}
            </Anchor>
            <Text size="xs" c="dimmed">
              {formatBytes(node.hit.size)}
            </Text>
          </Group>
        );
      })}
    </Stack>
  );
}

/** 单个仓库分组：可折叠的分组节点，组内为该仓库命中折叠出的路径层级树。 */
function RepoGroupNode({ group }: { group: SearchRepoGroup }) {
  const navigate = useNavigate();
  // 仓库分组默认展开，便于一眼看到命中；点击折叠 / 展开。
  const [open, setOpen] = useState(true);
  // 组内目录展开态：按完整目录前缀路径键，默认全展开；折叠父目录再重展开不丢子层态。
  const [expanded, setExpanded] = useState<Set<string>>(() =>
    collectFolderPaths(group.tree, new Set()),
  );
  const fileCount = useMemo(() => countFiles(group.tree), [group.tree]);

  const toggle = (path: string) =>
    setExpanded((prev) => {
      const next = new Set(prev);
      if (next.has(path)) next.delete(path);
      else next.add(path);
      return next;
    });

  const openFile = (path: string) =>
    navigate(`/artifact?repo=${encodeURIComponent(group.repoId)}&path=${encodeURIComponent(path)}`);

  return (
    <Stack gap={4}>
      <UnstyledButton
        onClick={() => setOpen((v) => !v)}
        aria-label={`${group.format} 仓库 ${group.repoName}`}
      >
        <Group gap={density.inlineGap} wrap="nowrap">
          {open ? <IconChevronDown size={14} /> : <IconChevronRight size={14} />}
          <FormatIcon format={group.format} />
          <Text fw={600} size="sm">
            {group.repoName}
          </Text>
          <Text size="xs" c="dimmed">
            {fileCount} 项
          </Text>
        </Group>
      </UnstyledButton>
      <Collapse in={open}>
        <Stack gap={2} pl="lg">
          <TreeNodes
            nodes={group.tree}
            format={group.format}
            depth={0}
            expanded={expanded}
            onToggle={toggle}
            onOpenFile={openFile}
          />
        </Stack>
      </Collapse>
    </Stack>
  );
}

/** 制品搜索页面：搜索框联动页眉（经 URL ?q= 承载），结果树形展示。 */
export function SearchPage() {
  const [searchParams, setSearchParams] = useSearchParams();
  const urlQuery = searchParams.get('q') ?? '';

  const [query, setQuery] = useState(urlQuery);
  const [format, setFormat] = useState('');
  const [groups, setGroups] = useState<SearchRepoGroup[]>([]);
  const [total, setTotal] = useState(0);
  const [page, setPage] = useState(1);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [searched, setSearched] = useState(false);

  const runSearch = async (keyword: string, fmt: string, targetPage: number) => {
    if (!keyword.trim()) return;
    setLoading(true);
    setError(null);
    try {
      const resp = await api.search(keyword.trim(), {
        format: fmt ? (fmt as RepoFormat) : undefined,
        offset: (targetPage - 1) * PAGE_SIZE,
        limit: PAGE_SIZE,
      });
      setGroups(buildSearchTree(resp.items));
      setTotal(resp.total);
      setPage(targetPage);
      setSearched(true);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setLoading(false);
    }
  };

  // 页眉跳转或深链带来的 ?q= 变化：同步输入框并自动发起搜索（回到第一页）。
  useEffect(() => {
    setQuery(urlQuery);
    if (urlQuery.trim()) {
      void runSearch(urlQuery, format, 1);
    }
    // 仅当 URL 中的 q 变化时触发；format / 翻页走页内交互，不进此 effect。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [urlQuery]);

  // 页内提交：把关键字写回 URL（与页眉同一真源），由上面的 effect 统一发起搜索。
  const handleSubmit = (e: FormEvent) => {
    e.preventDefault();
    const keyword = query.trim();
    if (!keyword) return;
    if (keyword === urlQuery) {
      // URL 未变（effect 不会重跑），直接按当前格式重搜
      void runSearch(keyword, format, 1);
    } else {
      setSearchParams({ q: keyword });
    }
  };

  const totalPages = Math.max(1, Math.ceil(total / PAGE_SIZE));

  return (
    <Stack gap={density.gridSpacing}>
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
        groups.length === 0 ? (
          <Text c="dimmed">未找到匹配的制品。</Text>
        ) : (
          <Stack gap={density.gridSpacing}>
            <Text size="sm" c="dimmed">
              共 {total} 条结果
            </Text>
            <Stack gap="sm">
              {groups.map((group) => (
                <RepoGroupNode key={group.repoId} group={group} />
              ))}
            </Stack>
            {totalPages > 1 && (
              <Group justify="center">
                <Pagination
                  value={page}
                  onChange={(p) => runSearch(query, format, p)}
                  total={totalPages}
                />
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
