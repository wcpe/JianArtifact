// 仓库管理界面（FR-19 + FR-135）：列表 / 创建 / 删除，并跳转到详情做配置与浏览。
// FR-135 增强：制品数 / 总大小 / 状态 / upstream URL 展示 + proxy 仓库连通性测试按钮（Admin）。
// 创建 / 删除 / 连通性测试仅管理员可见；列表对所有登录用户按可见性过滤展示。

import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
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
  Alert,
} from '@mantine/core';
import { useDisclosure } from '@mantine/hooks';
import { IconPlus, IconTrash, IconSettings, IconPlugConnected } from '@tabler/icons-react';
import { useNavigate } from 'react-router-dom';
import * as api from '../api/endpoints';
import type {
  ConnectivityResult,
  CreateRepositoryRequest,
  RepoFormat,
  RepoType,
  RepositoryDto,
  Visibility,
} from '../api/types';
import { useAuth } from '../auth/useAuth';
import { errorMessage } from '../lib/format';
import { formatBytes } from '../lib/format';
import { notifyError, notifySuccess } from '../lib/notify';
import { ErrorAlert } from '../components/ErrorAlert';

// 仓库格式下拉的取值顺序（标签经 i18n 的 repositories.formats.* 解析）。
const FORMAT_VALUES: RepoFormat[] = [
  'maven',
  'npm',
  'docker',
  'raw',
  'cargo',
  'go',
  'pypi',
  'nuget',
];

/** 连通性测试结果展示（成功绿色 / 失败红色）。 */
function ConnectivityAlert({ result }: { result: ConnectivityResult }) {
  const { t } = useTranslation('repositories');
  if (result.ok) {
    return (
      <Alert color="green" title={t('connectivitySuccess')}>
        {t('connectivityStatus', { status: result.status })}
        {t('connectivityElapsed', { ms: result.elapsed_ms })}
      </Alert>
    );
  }
  return (
    <Alert color="red" title={t('connectivityFail')}>
      {result.error ?? t('connectivityUnknownError')}
      {t('connectivityElapsed', { ms: result.elapsed_ms })}
    </Alert>
  );
}

/** 仓库管理页面。 */
export function RepositoriesPage() {
  const { t } = useTranslation('repositories');
  const { isAdmin } = useAuth();
  const navigate = useNavigate();
  const [repos, setRepos] = useState<RepositoryDto[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [modalOpened, modal] = useDisclosure(false);
  // 连通性测试状态
  const [testingId, setTestingId] = useState<string | null>(null);
  const [testResult, setTestResult] = useState<ConnectivityResult | null>(null);
  const [testRepoName, setTestRepoName] = useState<string>('');
  const [resultOpened, resultModal] = useDisclosure(false);

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
    if (!window.confirm(t('deleteConfirm', { name: repo.name }))) {
      return;
    }
    try {
      await api.deleteRepository(repo.id);
      notifySuccess(t('deleteSuccess'));
      reload();
    } catch (err) {
      notifyError(errorMessage(err));
    }
  };

  const handleTestConnectivity = async (repo: RepositoryDto) => {
    setTestingId(repo.id);
    setTestRepoName(repo.name);
    try {
      const result = await api.testRepoConnectivity(repo.id);
      setTestResult(result);
      resultModal.open();
    } catch (err) {
      notifyError(errorMessage(err));
    } finally {
      setTestingId(null);
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
        <Title order={2}>{t('title')}</Title>
        {isAdmin && (
          <Button leftSection={<IconPlus size={16} />} onClick={modal.open}>
            {t('createRepo')}
          </Button>
        )}
      </Group>
      {error && <ErrorAlert message={error} />}

      {repos.length === 0 ? (
        <Text c="dimmed">{t('emptyHint')}</Text>
      ) : (
        <Table.ScrollContainer minWidth={860}>
          <Table striped highlightOnHover>
            <Table.Thead>
              <Table.Tr>
                <Table.Th>{t('colName')}</Table.Th>
                <Table.Th>{t('colFormat')}</Table.Th>
                <Table.Th>{t('colType')}</Table.Th>
                <Table.Th>{t('colVisibility')}</Table.Th>
                <Table.Th>{t('colArtifactCount')}</Table.Th>
                <Table.Th>{t('colTotalSize')}</Table.Th>
                <Table.Th>{t('colStatus')}</Table.Th>
                <Table.Th>{t('colUpstream')}</Table.Th>
                <Table.Th>{t('colActions')}</Table.Th>
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
                  <Table.Td>
                    {repo.type === 'hosted' ? t('common:repoHosted') : t('common:repoProxy')}
                  </Table.Td>
                  <Table.Td>
                    <Badge color={repo.visibility === 'public' ? 'green' : 'gray'} variant="light">
                      {repo.visibility === 'public'
                        ? t('common:visibilityPublic')
                        : t('common:visibilityPrivate')}
                    </Badge>
                  </Table.Td>
                  <Table.Td>
                    <Text size="sm">{repo.artifact_count}</Text>
                  </Table.Td>
                  <Table.Td>
                    <Text size="sm">{formatBytes(repo.total_size)}</Text>
                  </Table.Td>
                  <Table.Td>
                    <Badge color={repo.status === 'active' ? 'teal' : 'gray'} variant="light">
                      {repo.status}
                    </Badge>
                  </Table.Td>
                  <Table.Td>
                    <Text size="sm" c="dimmed" truncate maw={200} title={repo.upstream_url ?? ''}>
                      {repo.upstream_url ?? '-'}
                    </Text>
                  </Table.Td>
                  <Table.Td>
                    <Group gap="xs">
                      <ActionIcon
                        variant="subtle"
                        onClick={() => navigate(`/repository?id=${encodeURIComponent(repo.id)}`)}
                        aria-label={t('configBrowse')}
                      >
                        <IconSettings size={18} />
                      </ActionIcon>
                      {isAdmin && repo.type === 'proxy' && repo.upstream_url && (
                        <ActionIcon
                          variant="subtle"
                          color="blue"
                          loading={testingId === repo.id}
                          onClick={() => handleTestConnectivity(repo)}
                          aria-label={t('testConnectivity')}
                        >
                          <IconPlugConnected size={18} />
                        </ActionIcon>
                      )}
                      {isAdmin && (
                        <ActionIcon
                          variant="subtle"
                          color="red"
                          onClick={() => handleDelete(repo)}
                          aria-label={t('deleteRepo')}
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

      {/* 连通性测试结果弹窗 */}
      <Modal
        opened={resultOpened}
        onClose={resultModal.close}
        title={t('connectivityModalTitle', { name: testRepoName })}
        centered
      >
        {testResult && <ConnectivityAlert result={testResult} />}
      </Modal>

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
  const { t } = useTranslation('repositories');
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
      notifySuccess(t('createSuccess'));
      reset();
      onCreated();
    } catch (err) {
      notifyError(errorMessage(err));
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <Modal opened={opened} onClose={onClose} title={t('modalTitle')} centered>
      <Stack>
        <TextInput
          label={t('nameLabel')}
          placeholder={t('namePlaceholder')}
          value={name}
          onChange={(e) => setName(e.currentTarget.value)}
          required
        />
        <Select
          label={t('formatLabel')}
          data={FORMAT_VALUES.map((value) => ({ value, label: t(`formats.${value}`) }))}
          value={format}
          onChange={(v) => setFormat((v as RepoFormat) ?? 'raw')}
          allowDeselect={false}
        />
        <Select
          label={t('typeLabel')}
          data={[
            { value: 'hosted', label: t('typeHosted') },
            { value: 'proxy', label: t('typeProxy') },
          ]}
          value={type}
          onChange={(v) => setType((v as RepoType) ?? 'hosted')}
          allowDeselect={false}
        />
        <Select
          label={t('visibilityLabel')}
          data={[
            { value: 'private', label: t('visibilityPrivate') },
            { value: 'public', label: t('visibilityPublic') },
          ]}
          value={visibility}
          onChange={(v) => setVisibility((v as Visibility) ?? 'private')}
          allowDeselect={false}
        />
        {type === 'proxy' && (
          <TextInput
            label={t('upstreamLabel')}
            placeholder="https://repo1.maven.org/maven2"
            value={upstreamUrl}
            onChange={(e) => setUpstreamUrl(e.currentTarget.value)}
            required
          />
        )}
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>
            {t('common:cancel')}
          </Button>
          <Button onClick={handleSubmit} loading={submitting} disabled={!name}>
            {t('create')}
          </Button>
        </Group>
      </Stack>
    </Modal>
  );
}
