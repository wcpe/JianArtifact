// 仓库详情页（FR-20 / FR-22 / FR-76 / FR-93）：浏览（左文件树 + 右制品详情）、配置与 ACL（管理员）。
// 经查询参数 ?id= 定位仓库，避免与后端格式 catch-all 路由冲突。

import { useCallback, useEffect, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
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
import { TREE_INDENT_STEP } from '../lib/browseTree';

/** 仓库详情页。 */
export function RepositoryDetailPage() {
  const { t } = useTranslation('repositoryDetail');
  const [params] = useSearchParams();
  const repoId = params.get('id') ?? '';
  const { isAdmin } = useAuth();
  const [repo, setRepo] = useState<RepositoryDto | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const loadRepo = useCallback(() => {
    if (!repoId) {
      setError(t('missingId'));
      setLoading(false);
      return;
    }
    setLoading(true);
    api
      .getRepository(repoId)
      .then(setRepo)
      .catch((err) => setError(errorMessage(err)))
      .finally(() => setLoading(false));
  }, [repoId, t]);

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
        <ErrorAlert message={error ?? t('notFound')} />
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
            {repo.type === 'hosted' ? t('common:repoHosted') : t('common:repoProxy')}
          </Badge>
          <Badge color={repo.visibility === 'public' ? 'green' : 'gray'} variant="light">
            {repo.visibility === 'public'
              ? t('common:visibilityPublic')
              : t('common:visibilityPrivate')}
          </Badge>
        </Group>
      </Group>

      <Tabs defaultValue="browse">
        <Tabs.List>
          <Tabs.Tab value="browse" leftSection={<IconFolderOpen size={16} />}>
            {t('tabBrowse')}
          </Tabs.Tab>
          {isAdmin && <Tabs.Tab value="config">{t('tabConfig')}</Tabs.Tab>}
          {isAdmin && <Tabs.Tab value="acl">{t('tabAcl')}</Tabs.Tab>}
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
                <Title order={4}>{t('aclUsers')}</Title>
                <AclPanel repoId={repo.id} />
              </Stack>
              <Stack gap="sm">
                <Title order={4}>{t('aclGroups')}</Title>
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
  const { t } = useTranslation('repositoryDetail');
  const navigate = useNavigate();
  return (
    <Group>
      <Button
        variant="subtle"
        size="xs"
        leftSection={<IconArrowLeft size={16} />}
        onClick={() => navigate('/repositories')}
      >
        {t('backToList')}
      </Button>
    </Group>
  );
}

/**
 * 浏览页签（FR-93）：左侧文件树（逐级展开）+ 右侧制品详情面板。
 * 一次性拉取仓库制品索引（FR-75），客户端按目录折叠成树；点文件加载详情。
 */
function BrowseTab({ repo }: { repo: RepositoryDto }) {
  const { t } = useTranslation('repositoryDetail');
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
    if (!window.confirm(t('deleteConfirm', { path }))) return;
    try {
      await api.deleteArtifact(repo.id, path);
      notifySuccess(t('deleteSuccess'));
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
    return <Text c="dimmed">{t('emptyArtifacts')}</Text>;
  }

  return (
    // 固定高度的浏览区（FR-115）：整体不随内容整页滚动，左树为主、右详情为辅，两栏各自独立滚动。
    // 高度取视口减去页眉 / 标题 / 页签等顶部占用，让文件树铺满内容区可用高度。
    <Group
      data-testid="browse-layout"
      align="stretch"
      gap="lg"
      wrap="nowrap"
      h="calc(100vh - 220px)"
      mih={360}
    >
      {/* 左：文件树（固定窄边栏，长名/深层经左右+上下滚动查看；详情占剩余大头，FR-115 真机调整） */}
      <Card
        withBorder
        padding="sm"
        radius="md"
        w={340}
        style={{ flexShrink: 0, display: 'flex', flexDirection: 'column' }}
      >
        <ScrollArea h="100%" type="auto" data-testid="browse-tree-scroll">
          <FileTree
            repo={repo}
            artifacts={artifacts}
            selectedPath={selectedPath}
            onSelectFile={selectFile}
            onDelete={isAdmin ? handleDelete : undefined}
          />
        </ScrollArea>
      </Card>

      {/* 右：详情面板（辅栏，独立滚动） */}
      <Card
        withBorder
        padding="sm"
        radius="md"
        flex={1}
        style={{ minWidth: 0, display: 'flex', flexDirection: 'column' }}
      >
        <ScrollArea h="100%" data-testid="browse-detail-scroll">
          {!selectedPath ? (
            <Center h={160}>
              <Text c="dimmed">{t('selectFileHint')}</Text>
            </Center>
          ) : detailLoading ? (
            <Center h={160}>
              <Loader />
            </Center>
          ) : detailError || !detail ? (
            <ErrorAlert message={detailError ?? t('artifactNotFound')} />
          ) : (
            <ArtifactDetailPanel detail={detail} />
          )}
        </ScrollArea>
      </Card>
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
  const { t } = useTranslation('repositoryDetail');
  const entries = useMemo(() => buildDirectoryListing(artifacts, prefix), [artifacts, prefix]);

  return (
    <Stack gap={2}>
      {entries.map((e) => {
        const indent = depth * TREE_INDENT_STEP;
        if (e.type === 'folder') {
          const childPrefix = `${prefix}${e.name}/`;
          const open = expanded.has(childPrefix);
          return (
            <div key={`d:${e.name}`}>
              <UnstyledButton
                data-testid="tree-folder"
                onClick={() => onToggle(childPrefix)}
                py={4}
                // 缩进经 style.paddingLeft 表达「层级递进」；不可再用 Mantine `px` prop——
                // 它会覆盖此处 paddingLeft，使所有目录恒为 6px、丢失层级缩进（FR-115 修复）。
                style={{ width: '100%', borderRadius: 4, paddingLeft: indent + 6, paddingRight: 6 }}
              >
                <Group gap={4} wrap="nowrap" align="flex-start">
                  {open ? <IconChevronDown size={14} /> : <IconChevronRight size={14} />}
                  {open ? <IconFolderOpen size={16} /> : <IconFolder size={16} />}
                  {/* 文件名不截断（FR-115）：不换行，超出窄边栏经横向滚动看全名 */}
                  <Text size="sm" style={{ whiteSpace: 'nowrap' }}>
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
          <Group
            key={`f:${e.path}`}
            data-testid="tree-file"
            gap={4}
            wrap="nowrap"
            align="flex-start"
            style={{ paddingLeft: indent + 6 }}
          >
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
              <Group gap={6} wrap="nowrap" align="flex-start">
                <FormatIcon format={repo.format} />
                {/* 文件名不截断（FR-115）：不换行，超出窄边栏经横向滚动看全名（.jar/.jar.md5 等） */}
                <Text size="sm" style={{ whiteSpace: 'nowrap' }}>
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
                aria-label={t('deleteArtifact')}
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
  const { t } = useTranslation('repositoryDetail');
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
      notifySuccess(t('configSaved'));
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
          label={t('visibilityLabel')}
          data={[
            { value: 'private', label: t('visibilityPrivateOption') },
            { value: 'public', label: t('visibilityPublicOption') },
          ]}
          value={visibility}
          onChange={(v) => setVisibility((v as Visibility) ?? repo.visibility)}
          allowDeselect={false}
        />
        {repo.type === 'proxy' && (
          <TextInput
            label={t('upstreamUrlLabel')}
            placeholder="https://repo1.maven.org/maven2"
            value={upstreamUrl}
            onChange={(e) => setUpstreamUrl(e.currentTarget.value)}
          />
        )}
        <Group justify="flex-end">
          <Button onClick={handleSave} loading={submitting}>
            {t('common:save')}
          </Button>
        </Group>
      </Stack>
    </Card>
  );
}
