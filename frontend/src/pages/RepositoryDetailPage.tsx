// 仓库详情页（FR-20 / FR-22 / FR-76 / FR-93）：浏览（左文件树 + 右制品详情）、配置与 ACL（管理员）。
// 经查询参数 ?id= 定位仓库，避免与后端格式 catch-all 路由冲突。

import { useCallback, useEffect, useMemo, useState } from 'react';
import {
  Tabs,
  Title,
  Stack,
  Group,
  Badge,
  Loader,
  Center,
  Text,
  Button,
  Select,
  ActionIcon,
  Card,
  TextInput,
  ScrollArea,
  UnstyledButton,
} from '@mantine/core';
import {
  IconArrowLeft,
  IconFolder,
  IconFolderOpen,
  IconChevronRight,
  IconChevronDown,
  IconTrash,
} from '@tabler/icons-react';
import { useNavigate, useSearchParams } from 'react-router-dom';
import * as api from '../api/endpoints';
import type { ArtifactDetailDto, ArtifactDto, RepositoryDto, Visibility } from '../api/types';
import { useAuth } from '../auth/useAuth';
import { buildDirectoryListing } from '../lib/browse';
import { FormatIcon } from '../lib/formatIcon';
import { errorMessage } from '../lib/format';
import { notifyError, notifySuccess } from '../lib/notify';
import { ErrorAlert } from '../components/ErrorAlert';
import { ArtifactDetailPanel } from '../components/ArtifactDetailPanel';
import { AclPanel } from '../components/AclPanel';
import { GroupAclPanel } from '../components/GroupAclPanel';

/** 仓库详情页。 */
export function RepositoryDetailPage() {
  const [params] = useSearchParams();
  const repoId = params.get('id') ?? '';
  const { isAdmin } = useAuth();
  const [repo, setRepo] = useState<RepositoryDto | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const loadRepo = useCallback(() => {
    if (!repoId) {
      setError('缺少仓库标识');
      setLoading(false);
      return;
    }
    setLoading(true);
    api
      .getRepository(repoId)
      .then(setRepo)
      .catch((err) => setError(errorMessage(err)))
      .finally(() => setLoading(false));
  }, [repoId]);

  useEffect(loadRepo, [loadRepo]);

  if (loading) {
    return (
      <Center h={200}>
        <Loader />
      </Center>
    );
  }
  if (error || !repo) {
    return (
      <Stack>
        <BackButton />
        <ErrorAlert message={error ?? '仓库不存在'} />
      </Stack>
    );
  }

  return (
    <Stack>
      <BackButton />
      <Group justify="space-between">
        <Group>
          <Title order={2}>{repo.name}</Title>
          <Badge variant="light">{repo.format}</Badge>
          <Badge variant="light" color="grape">
            {repo.type === 'hosted' ? '托管' : '代理'}
          </Badge>
          <Badge color={repo.visibility === 'public' ? 'green' : 'gray'} variant="light">
            {repo.visibility === 'public' ? '公开' : '私有'}
          </Badge>
        </Group>
      </Group>

      <Tabs defaultValue="browse">
        <Tabs.List>
          <Tabs.Tab value="browse" leftSection={<IconFolderOpen size={16} />}>
            浏览
          </Tabs.Tab>
          {isAdmin && <Tabs.Tab value="config">配置</Tabs.Tab>}
          {isAdmin && <Tabs.Tab value="acl">权限（ACL）</Tabs.Tab>}
        </Tabs.List>

        <Tabs.Panel value="browse" pt="md">
          <BrowseTab repo={repo} />
        </Tabs.Panel>

        {isAdmin && (
          <Tabs.Panel value="config" pt="md">
            <ConfigTab repo={repo} onUpdated={loadRepo} />
          </Tabs.Panel>
        )}

        {isAdmin && (
          <Tabs.Panel value="acl" pt="md">
            <Stack gap="xl">
              <Stack gap="sm">
                <Title order={4}>用户授权</Title>
                <AclPanel repoId={repo.id} />
              </Stack>
              <Stack gap="sm">
                <Title order={4}>用户组授权</Title>
                <GroupAclPanel repoId={repo.id} />
              </Stack>
            </Stack>
          </Tabs.Panel>
        )}
      </Tabs>
    </Stack>
  );
}

/** 返回仓库列表按钮。 */
function BackButton() {
  const navigate = useNavigate();
  return (
    <Group>
      <Button
        variant="subtle"
        size="xs"
        leftSection={<IconArrowLeft size={16} />}
        onClick={() => navigate('/repositories')}
      >
        返回仓库列表
      </Button>
    </Group>
  );
}

/**
 * 浏览页签（FR-93）：左侧文件树（逐级展开）+ 右侧制品详情面板。
 * 一次性拉取仓库制品索引（FR-75），客户端按目录折叠成树；点文件加载详情。
 */
function BrowseTab({ repo }: { repo: RepositoryDto }) {
  const { isAdmin } = useAuth();
  const [artifacts, setArtifacts] = useState<ArtifactDto[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  // 右侧详情：选中文件路径 + 加载态
  const [selectedPath, setSelectedPath] = useState<string | null>(null);
  const [detail, setDetail] = useState<ArtifactDetailDto | null>(null);
  const [detailLoading, setDetailLoading] = useState(false);
  const [detailError, setDetailError] = useState<string | null>(null);

  const reload = useCallback(() => {
    setLoading(true);
    api
      .listArtifacts(repo.id)
      .then(setArtifacts)
      .catch((err) => setError(errorMessage(err)))
      .finally(() => setLoading(false));
  }, [repo.id]);

  useEffect(reload, [reload]);

  const selectFile = useCallback(
    (path: string) => {
      setSelectedPath(path);
      setDetail(null);
      setDetailError(null);
      setDetailLoading(true);
      api
        .getArtifactDetail(repo.id, path)
        .then(setDetail)
        .catch((err) => setDetailError(errorMessage(err)))
        .finally(() => setDetailLoading(false));
    },
    [repo.id],
  );

  const handleDelete = async (path: string) => {
    if (!window.confirm(`确认删除制品「${path}」？`)) return;
    try {
      await api.deleteArtifact(repo.id, path);
      notifySuccess('制品已删除');
      if (selectedPath === path) {
        setSelectedPath(null);
        setDetail(null);
      }
      reload();
    } catch (err) {
      notifyError(errorMessage(err));
    }
  };

  if (loading) {
    return (
      <Center h={160}>
        <Loader />
      </Center>
    );
  }
  if (error) return <ErrorAlert message={error} />;
  if (artifacts.length === 0) {
    return <Text c="dimmed">该仓库暂无制品。</Text>;
  }

  return (
    <Group align="flex-start" gap="lg" wrap="nowrap">
      {/* 左：文件树 */}
      <Card withBorder padding="sm" radius="md" w={320} style={{ flexShrink: 0 }}>
        <ScrollArea.Autosize mah={520}>
          <FileTree
            repo={repo}
            artifacts={artifacts}
            selectedPath={selectedPath}
            onSelectFile={selectFile}
            onDelete={isAdmin ? handleDelete : undefined}
          />
        </ScrollArea.Autosize>
      </Card>

      {/* 右：详情面板 */}
      <div style={{ flex: 1, minWidth: 0 }}>
        {!selectedPath ? (
          <Center h={160}>
            <Text c="dimmed">从左侧选择一个文件查看详情。</Text>
          </Center>
        ) : detailLoading ? (
          <Center h={160}>
            <Loader />
          </Center>
        ) : detailError || !detail ? (
          <ErrorAlert message={detailError ?? '制品不存在'} />
        ) : (
          <ArtifactDetailPanel detail={detail} />
        )}
      </div>
    </Group>
  );
}

/**
 * 文件树：从仓库根递归渲染目录 / 文件，目录可逐级展开。
 *
 * 展开态在此集中持有（按目录**完整前缀路径**键，如 `com/example/`），向下传给各层。
 * 这样折叠父目录再重新展开时，深层子目录的展开态不丢失（子层不再各自持有局部 state）。
 */
function FileTree({
  repo,
  artifacts,
  selectedPath,
  onSelectFile,
  onDelete,
}: {
  repo: RepositoryDto;
  artifacts: ArtifactDto[];
  selectedPath: string | null;
  onSelectFile: (path: string) => void;
  onDelete?: (path: string) => void;
}) {
  const [expanded, setExpanded] = useState<Set<string>>(new Set());

  const toggle = (path: string) => {
    setExpanded((prev) => {
      const next = new Set(prev);
      if (next.has(path)) next.delete(path);
      else next.add(path);
      return next;
    });
  };

  return (
    <TreeLevel
      repo={repo}
      artifacts={artifacts}
      prefix=""
      depth={0}
      selectedPath={selectedPath}
      expanded={expanded}
      onToggle={toggle}
      onSelectFile={onSelectFile}
      onDelete={onDelete}
    />
  );
}

/** 树的一层：渲染给定前缀下的目录 / 文件条目，目录展开时递归渲染下一层。 */
function TreeLevel({
  repo,
  artifacts,
  prefix,
  depth,
  selectedPath,
  expanded,
  onToggle,
  onSelectFile,
  onDelete,
}: {
  repo: RepositoryDto;
  artifacts: ArtifactDto[];
  prefix: string;
  depth: number;
  selectedPath: string | null;
  /** 全树共享的展开态（按目录完整前缀路径键）。 */
  expanded: Set<string>;
  /** 切换某目录展开态（传目录完整前缀路径）。 */
  onToggle: (path: string) => void;
  onSelectFile: (path: string) => void;
  onDelete?: (path: string) => void;
}) {
  const entries = useMemo(() => buildDirectoryListing(artifacts, prefix), [artifacts, prefix]);

  return (
    <Stack gap={2}>
      {entries.map((e) => {
        const indent = depth * 16;
        if (e.type === 'folder') {
          const childPrefix = `${prefix}${e.name}/`;
          const open = expanded.has(childPrefix);
          return (
            <div key={`d:${e.name}`}>
              <UnstyledButton
                onClick={() => onToggle(childPrefix)}
                px={6}
                py={4}
                style={{ width: '100%', borderRadius: 4, paddingLeft: indent + 6 }}
              >
                <Group gap={4} wrap="nowrap">
                  {open ? <IconChevronDown size={14} /> : <IconChevronRight size={14} />}
                  {open ? <IconFolderOpen size={16} /> : <IconFolder size={16} />}
                  <Text size="sm" truncate>
                    {e.name}
                  </Text>
                </Group>
              </UnstyledButton>
              {open && (
                <TreeLevel
                  repo={repo}
                  artifacts={artifacts}
                  prefix={childPrefix}
                  depth={depth + 1}
                  selectedPath={selectedPath}
                  expanded={expanded}
                  onToggle={onToggle}
                  onSelectFile={onSelectFile}
                  onDelete={onDelete}
                />
              )}
            </div>
          );
        }
        // 文件叶子
        const active = selectedPath === e.path;
        return (
          <Group key={`f:${e.path}`} gap={4} wrap="nowrap" style={{ paddingLeft: indent + 6 }}>
            <UnstyledButton
              onClick={() => onSelectFile(e.path!)}
              px={6}
              py={4}
              style={{
                flex: 1,
                minWidth: 0,
                borderRadius: 4,
                fontWeight: active ? 600 : 400,
              }}
            >
              <Group gap={6} wrap="nowrap">
                <FormatIcon format={repo.format} />
                <Text size="sm" truncate>
                  {e.name}
                </Text>
              </Group>
            </UnstyledButton>
            {onDelete && (
              <ActionIcon
                variant="subtle"
                color="red"
                size="sm"
                onClick={() => onDelete(e.path!)}
                aria-label="删除制品"
              >
                <IconTrash size={15} />
              </ActionIcon>
            )}
          </Group>
        );
      })}
    </Stack>
  );
}

/** 仓库配置页签（仅管理员）。 */
function ConfigTab({ repo, onUpdated }: { repo: RepositoryDto; onUpdated: () => void }) {
  const [visibility, setVisibility] = useState<Visibility>(repo.visibility);
  const [upstreamUrl, setUpstreamUrl] = useState(repo.upstream_url ?? '');
  const [submitting, setSubmitting] = useState(false);

  const handleSave = async () => {
    setSubmitting(true);
    try {
      await api.updateRepository(repo.id, {
        visibility,
        upstream_url: repo.type === 'proxy' ? upstreamUrl : undefined,
      });
      notifySuccess('仓库配置已更新');
      onUpdated();
    } catch (err) {
      notifyError(errorMessage(err));
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <Card withBorder padding="lg" radius="md" maw={480}>
      <Stack>
        <Select
          label="可见性"
          data={[
            { value: 'private', label: '私有（private）' },
            { value: 'public', label: '公开（public）' },
          ]}
          value={visibility}
          onChange={(v) => setVisibility((v as Visibility) ?? repo.visibility)}
          allowDeselect={false}
        />
        {repo.type === 'proxy' && (
          <TextInput
            label="上游地址"
            placeholder="https://repo1.maven.org/maven2"
            value={upstreamUrl}
            onChange={(e) => setUpstreamUrl(e.currentTarget.value)}
          />
        )}
        <Group justify="flex-end">
          <Button onClick={handleSave} loading={submitting}>
            保存
          </Button>
        </Group>
      </Stack>
    </Card>
  );
}
