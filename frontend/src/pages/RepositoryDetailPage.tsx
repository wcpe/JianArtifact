// 仓库详情页：配置（可见性 / 上游，管理员）、每仓库 ACL 管理（FR-20）、制品浏览（FR-22）。
// 经查询参数 ?id= 定位仓库，避免与后端格式 catch-all 路由冲突。

import { useCallback, useEffect, useState } from 'react';
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
  Table,
  ActionIcon,
  Anchor,
  Card,
  TextInput,
} from '@mantine/core';
import { IconTrash, IconArrowLeft } from '@tabler/icons-react';
import { useNavigate, useSearchParams } from 'react-router-dom';
import * as api from '../api/endpoints';
import type { ArtifactDto, RepositoryDto, Visibility } from '../api/types';
import { useAuth } from '../auth/useAuth';
import { errorMessage, formatBytes } from '../lib/format';
import { notifyError, notifySuccess } from '../lib/notify';
import { ErrorAlert } from '../components/ErrorAlert';
import { AclPanel } from '../components/AclPanel';

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

      <Tabs defaultValue="artifacts">
        <Tabs.List>
          <Tabs.Tab value="artifacts">制品浏览</Tabs.Tab>
          {isAdmin && <Tabs.Tab value="config">配置</Tabs.Tab>}
          {isAdmin && <Tabs.Tab value="acl">权限（ACL）</Tabs.Tab>}
        </Tabs.List>

        <Tabs.Panel value="artifacts" pt="md">
          <ArtifactsTab repo={repo} />
        </Tabs.Panel>

        {isAdmin && (
          <Tabs.Panel value="config" pt="md">
            <ConfigTab repo={repo} onUpdated={loadRepo} />
          </Tabs.Panel>
        )}

        {isAdmin && (
          <Tabs.Panel value="acl" pt="md">
            <AclPanel repoId={repo.id} />
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

/** 制品浏览页签（FR-22）。 */
function ArtifactsTab({ repo }: { repo: RepositoryDto }) {
  const navigate = useNavigate();
  const { isAdmin } = useAuth();
  const [artifacts, setArtifacts] = useState<ArtifactDto[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const reload = useCallback(() => {
    setLoading(true);
    api
      .listArtifacts(repo.id)
      .then(setArtifacts)
      .catch((err) => setError(errorMessage(err)))
      .finally(() => setLoading(false));
  }, [repo.id]);

  useEffect(reload, [reload]);

  const handleDelete = async (path: string) => {
    if (!window.confirm(`确认删除制品「${path}」？`)) return;
    try {
      await api.deleteArtifact(repo.id, path);
      notifySuccess('制品已删除');
      reload();
    } catch (err) {
      notifyError(errorMessage(err));
    }
  };

  const openDetail = (path: string) => {
    navigate(`/artifact?repo=${encodeURIComponent(repo.id)}&path=${encodeURIComponent(path)}`);
  };

  if (loading) {
    return (
      <Center h={120}>
        <Loader />
      </Center>
    );
  }
  if (error) return <ErrorAlert message={error} />;
  if (artifacts.length === 0) {
    return <Text c="dimmed">该仓库暂无制品。</Text>;
  }

  return (
    <Table.ScrollContainer minWidth={620}>
      <Table striped highlightOnHover>
        <Table.Thead>
          <Table.Tr>
            <Table.Th>路径</Table.Th>
            <Table.Th>大小</Table.Th>
            <Table.Th>缓存</Table.Th>
            <Table.Th>创建时间</Table.Th>
            <Table.Th>操作</Table.Th>
          </Table.Tr>
        </Table.Thead>
        <Table.Tbody>
          {artifacts.map((a) => (
            <Table.Tr key={a.path}>
              <Table.Td>
                <Anchor onClick={() => openDetail(a.path)}>{a.path}</Anchor>
              </Table.Td>
              <Table.Td>{formatBytes(a.size)}</Table.Td>
              <Table.Td>
                {a.cached ? (
                  <Badge size="sm" variant="light">
                    缓存
                  </Badge>
                ) : (
                  '-'
                )}
              </Table.Td>
              <Table.Td>
                <Text size="sm" c="dimmed">
                  {a.created_at}
                </Text>
              </Table.Td>
              <Table.Td>
                {isAdmin && (
                  <ActionIcon
                    variant="subtle"
                    color="red"
                    onClick={() => handleDelete(a.path)}
                    aria-label="删除制品"
                  >
                    <IconTrash size={18} />
                  </ActionIcon>
                )}
              </Table.Td>
            </Table.Tr>
          ))}
        </Table.Tbody>
      </Table>
    </Table.ScrollContainer>
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
