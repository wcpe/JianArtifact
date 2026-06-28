// 通用制品上传页面（FR-74）：统一入口——选仓库 → 按格式渲染动态表单 → 选文件 → 带进度上传 → 结果提示。
//
// 仅支持向 hosted 仓库上传 Maven / npm / Raw 三格式（与后端 FR-73 一致）；
// proxy 仓库与其余格式不在选择列表内。坐标字段按所选仓库格式动态切换。

import { useEffect, useMemo, useState } from 'react';
import {
  Title,
  Stack,
  Select,
  TextInput,
  FileInput,
  Button,
  Progress,
  Group,
  Text,
  Loader,
  Center,
} from '@mantine/core';
import { IconUpload, IconFile } from '@tabler/icons-react';
import { useTranslation } from 'react-i18next';
import * as api from '../api/endpoints';
import type { RepoFormat, RepositoryDto } from '../api/types';
import { errorMessage } from '../lib/format';
import { notifyError, notifySuccess } from '../lib/notify';
import { ErrorAlert } from '../components/ErrorAlert';

/** 通用上传支持的格式（仅这三种走统一上传端点）。 */
const UPLOADABLE_FORMATS: RepoFormat[] = ['maven', 'npm', 'raw'];

/** 上传页面。 */
export function UploadPage() {
  const { t } = useTranslation('upload');
  const [repos, setRepos] = useState<RepositoryDto[]>([]);
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState<string | null>(null);

  // 表单状态
  const [repoId, setRepoId] = useState<string | null>(null);
  const [groupId, setGroupId] = useState('');
  const [artifactId, setArtifactId] = useState('');
  const [version, setVersion] = useState('');
  const [npmName, setNpmName] = useState('');
  const [npmVersion, setNpmVersion] = useState('');
  const [rawPath, setRawPath] = useState('');
  const [file, setFile] = useState<File | null>(null);

  // 上传状态
  const [uploading, setUploading] = useState(false);
  const [progress, setProgress] = useState(0);

  useEffect(() => {
    api
      .listRepositories()
      // 仅保留可上传的 hosted 仓库（Maven / npm / Raw）
      .then((all) =>
        setRepos(all.filter((r) => r.type === 'hosted' && UPLOADABLE_FORMATS.includes(r.format))),
      )
      .catch((err) => setLoadError(errorMessage(err)))
      .finally(() => setLoading(false));
  }, []);

  const selectedRepo = useMemo(() => repos.find((r) => r.id === repoId) ?? null, [repos, repoId]);

  /** 当前表单坐标字段是否填齐（据所选格式判定）。 */
  const coordsReady = useMemo(() => {
    if (!selectedRepo) return false;
    switch (selectedRepo.format) {
      case 'maven':
        return groupId.trim() !== '' && artifactId.trim() !== '' && version.trim() !== '';
      case 'npm':
        return npmName.trim() !== '' && npmVersion.trim() !== '';
      case 'raw':
        return rawPath.trim() !== '';
      default:
        return false;
    }
  }, [selectedRepo, groupId, artifactId, version, npmName, npmVersion, rawPath]);

  const canSubmit = !!selectedRepo && !!file && coordsReady && !uploading;

  /** 据所选格式构造上传表单字段。 */
  const buildFormData = (repo: RepositoryDto, picked: File): FormData => {
    const fd = new FormData();
    fd.append('file', picked);
    switch (repo.format) {
      case 'maven':
        fd.append('group_id', groupId.trim());
        fd.append('artifact_id', artifactId.trim());
        fd.append('version', version.trim());
        break;
      case 'npm':
        fd.append('name', npmName.trim());
        fd.append('version', npmVersion.trim());
        break;
      case 'raw':
        fd.append('path', rawPath.trim());
        break;
    }
    return fd;
  };

  const handleUpload = async () => {
    if (!selectedRepo || !file) return;
    setUploading(true);
    setProgress(0);
    try {
      await api.uploadArtifact(selectedRepo.id, buildFormData(selectedRepo, file), setProgress);
      notifySuccess(t('uploadSuccess'));
      // 成功后清空文件，保留坐标字段便于继续上传同一坐标族下的文件
      setFile(null);
      setProgress(0);
    } catch (err) {
      notifyError(errorMessage(err));
    } finally {
      setUploading(false);
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
    <Stack maw={560}>
      <Title order={2}>{t('title')}</Title>
      {loadError && <ErrorAlert message={loadError} />}

      <Select
        label={t('targetRepo')}
        placeholder={t('repoPlaceholder')}
        data={repos.map((r) => ({ value: r.id, label: `${r.name}（${r.format}）` }))}
        value={repoId}
        onChange={setRepoId}
        searchable
        nothingFoundMessage={t('noRepoFound')}
      />

      {selectedRepo?.format === 'maven' && (
        <>
          <TextInput
            label={t('mavenGroupId')}
            placeholder="com.example.app"
            value={groupId}
            onChange={(e) => setGroupId(e.currentTarget.value)}
            required
          />
          <TextInput
            label={t('mavenArtifactId')}
            placeholder="demo"
            value={artifactId}
            onChange={(e) => setArtifactId(e.currentTarget.value)}
            required
          />
          <TextInput
            label={t('mavenVersion')}
            placeholder="1.0.0"
            value={version}
            onChange={(e) => setVersion(e.currentTarget.value)}
            required
          />
        </>
      )}

      {selectedRepo?.format === 'npm' && (
        <>
          <TextInput
            label={t('npmName')}
            placeholder={t('npmNamePlaceholder')}
            value={npmName}
            onChange={(e) => setNpmName(e.currentTarget.value)}
            required
          />
          <TextInput
            label={t('npmVersion')}
            placeholder="4.17.21"
            value={npmVersion}
            onChange={(e) => setNpmVersion(e.currentTarget.value)}
            required
          />
        </>
      )}

      {selectedRepo?.format === 'raw' && (
        <TextInput
          label={t('rawPath')}
          placeholder="dir/sub/file.bin"
          value={rawPath}
          onChange={(e) => setRawPath(e.currentTarget.value)}
          required
        />
      )}

      {selectedRepo && (
        <FileInput
          label={t('fileLabel')}
          placeholder={t('filePlaceholder')}
          leftSection={<IconFile size={16} />}
          value={file}
          onChange={setFile}
          clearable
          required
        />
      )}

      {uploading && (
        <Stack gap={4}>
          <Progress value={progress} animated aria-label={t('progressAria')} />
          <Text size="xs" c="dimmed">
            {t('uploading', { progress })}
          </Text>
        </Stack>
      )}

      <Group justify="flex-end">
        <Button
          leftSection={<IconUpload size={16} />}
          onClick={handleUpload}
          loading={uploading}
          disabled={!canSubmit}
        >
          {t('upload')}
        </Button>
      </Group>
    </Stack>
  );
}
