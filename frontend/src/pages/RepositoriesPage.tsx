// 仓库管理界面（FR-19）：列表 / 创建 / 删除，并跳转到详情做配置与浏览。
// 创建 / 删除仅管理员可见；列表对所有登录用户按可见性过滤展示。

import { useEffect, useState } from 'react';
import {
  Table,
  Button,
  Group,
  Title,
  Stack,
  Badge,
  Modal,
  TextInput,
  Select,
  ActionIcon,
  Text,
  Loader,
  Center,
  Anchor,
} from '@mantine/core';
import { useDisclosure } from '@mantine/hooks';
import { IconPlus, IconTrash, IconSettings } from '@tabler/icons-react';
import { useNavigate } from 'react-router-dom';
import * as api from '../api/endpoints';
import type {
  CreateRepositoryRequest,
  RepoFormat,
  RepoType,
  RepositoryDto,
  Visibility,
} from '../api/types';
import { useAuth } from '../auth/useAuth';
import { errorMessage } from '../lib/format';
import { notifyError, notifySuccess } from '../lib/notify';
import { ErrorAlert } from '../components/ErrorAlert';

const FORMAT_OPTIONS: { value: RepoFormat; label: string }[] = [
  { value: 'maven', label: 'Maven' },
  { value: 'npm', label: 'npm' },
  { value: 'docker', label: 'Docker / OCI' },
  { value: 'raw', label: 'Raw 通用文件' },
  { value: 'cargo', label: 'Cargo' },
  { value: 'go', label: 'Go 模块' },
  { value: 'pypi', label: 'PyPI' },
  { value: 'nuget', label: 'NuGet' },
];

/** 仓库管理页面。 */
export function RepositoriesPage() {
  const { isAdmin } = useAuth();
  const navigate = useNavigate();
  const [repos, setRepos] = useState<RepositoryDto[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [modalOpened, modal] = useDisclosure(false);

  const reload = () => {
    setLoading(true);
    api
      .listRepositories()
      .then(setRepos)
      .catch((err) => setError(errorMessage(err)))
      .finally(() => setLoading(false));
  };

  useEffect(reload, []);

  const handleDelete = async (repo: RepositoryDto) => {
    if (!window.confirm(`确认删除仓库「${repo.name}」？该操作不可撤销。`)) {
      return;
    }
    try {
      await api.deleteRepository(repo.id);
      notifySuccess('仓库已删除');
      reload();
    } catch (err) {
      notifyError(errorMessage(err));
    }
  };

  if (loading) {
    return (
      <Center h={200}>
        <Loader />
      </Center>
    );
  }

  return (
    <Stack>
      <Group justify="space-between">
        <Title order={2}>仓库管理</Title>
        {isAdmin && (
          <Button leftSection={<IconPlus size={16} />} onClick={modal.open}>
            创建仓库
          </Button>
        )}
      </Group>
      {error && <ErrorAlert message={error} />}

      {repos.length === 0 ? (
        <Text c="dimmed">暂无可见仓库。</Text>
      ) : (
        <Table.ScrollContainer minWidth={680}>
          <Table striped highlightOnHover>
            <Table.Thead>
              <Table.Tr>
                <Table.Th>名称</Table.Th>
                <Table.Th>格式</Table.Th>
                <Table.Th>类型</Table.Th>
                <Table.Th>可见性</Table.Th>
                <Table.Th>上游</Table.Th>
                <Table.Th>操作</Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {repos.map((repo) => (
                <Table.Tr key={repo.id}>
                  <Table.Td>
                    <Anchor
                      onClick={() => navigate(`/repository?id=${encodeURIComponent(repo.id)}`)}
                    >
                      {repo.name}
                    </Anchor>
                  </Table.Td>
                  <Table.Td>{repo.format}</Table.Td>
                  <Table.Td>{repo.type === 'hosted' ? '托管' : '代理'}</Table.Td>
                  <Table.Td>
                    <Badge color={repo.visibility === 'public' ? 'green' : 'gray'} variant="light">
                      {repo.visibility === 'public' ? '公开' : '私有'}
                    </Badge>
                  </Table.Td>
                  <Table.Td>
                    <Text size="sm" c="dimmed" truncate maw={220}>
                      {repo.upstream_url ?? '-'}
                    </Text>
                  </Table.Td>
                  <Table.Td>
                    <Group gap="xs">
                      <ActionIcon
                        variant="subtle"
                        onClick={() => navigate(`/repository?id=${encodeURIComponent(repo.id)}`)}
                        aria-label="配置 / 浏览"
                      >
                        <IconSettings size={18} />
                      </ActionIcon>
                      {isAdmin && (
                        <ActionIcon
                          variant="subtle"
                          color="red"
                          onClick={() => handleDelete(repo)}
                          aria-label="删除仓库"
                        >
                          <IconTrash size={18} />
                        </ActionIcon>
                      )}
                    </Group>
                  </Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        </Table.ScrollContainer>
      )}

      <CreateRepoModal
        opened={modalOpened}
        onClose={modal.close}
        onCreated={() => {
          modal.close();
          reload();
        }}
      />
    </Stack>
  );
}

/** 创建仓库弹窗。 */
function CreateRepoModal({
  opened,
  onClose,
  onCreated,
}: {
  opened: boolean;
  onClose: () => void;
  onCreated: () => void;
}) {
  const [name, setName] = useState('');
  const [format, setFormat] = useState<RepoFormat>('raw');
  const [type, setType] = useState<RepoType>('hosted');
  const [visibility, setVisibility] = useState<Visibility>('private');
  const [upstreamUrl, setUpstreamUrl] = useState('');
  const [submitting, setSubmitting] = useState(false);

  const reset = () => {
    setName('');
    setFormat('raw');
    setType('hosted');
    setVisibility('private');
    setUpstreamUrl('');
  };

  const handleSubmit = async () => {
    setSubmitting(true);
    try {
      const req: CreateRepositoryRequest = {
        name,
        format,
        type,
        visibility,
        upstream_url: type === 'proxy' ? upstreamUrl : null,
      };
      await api.createRepository(req);
      notifySuccess('仓库已创建');
      reset();
      onCreated();
    } catch (err) {
      notifyError(errorMessage(err));
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <Modal opened={opened} onClose={onClose} title="创建仓库" centered>
      <Stack>
        <TextInput
          label="仓库名"
          placeholder="如 maven-releases"
          value={name}
          onChange={(e) => setName(e.currentTarget.value)}
          required
        />
        <Select
          label="格式"
          data={FORMAT_OPTIONS}
          value={format}
          onChange={(v) => setFormat((v as RepoFormat) ?? 'raw')}
          allowDeselect={false}
        />
        <Select
          label="类型"
          data={[
            { value: 'hosted', label: '托管（hosted）' },
            { value: 'proxy', label: '代理（proxy）' },
          ]}
          value={type}
          onChange={(v) => setType((v as RepoType) ?? 'hosted')}
          allowDeselect={false}
        />
        <Select
          label="可见性"
          data={[
            { value: 'private', label: '私有（private）' },
            { value: 'public', label: '公开（public）' },
          ]}
          value={visibility}
          onChange={(v) => setVisibility((v as Visibility) ?? 'private')}
          allowDeselect={false}
        />
        {type === 'proxy' && (
          <TextInput
            label="上游地址"
            placeholder="https://repo1.maven.org/maven2"
            value={upstreamUrl}
            onChange={(e) => setUpstreamUrl(e.currentTarget.value)}
            required
          />
        )}
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>
            取消
          </Button>
          <Button onClick={handleSubmit} loading={submitting} disabled={!name}>
            创建
          </Button>
        </Group>
      </Stack>
    </Modal>
  );
}
