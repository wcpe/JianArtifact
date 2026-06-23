// 制品详情页（FR-22 / FR-66 / FR-68 / FR-69）：展示元数据、四校验和与按格式生成的使用方式片段。
// 经查询参数 ?repo=&path= 定位，避免与后端格式 catch-all 路由冲突。

import { useEffect, useState } from 'react';
import {
  Title,
  Stack,
  Group,
  Badge,
  Loader,
  Center,
  Text,
  Table,
  Card,
  Code,
  CopyButton,
  ActionIcon,
  Button,
  Tabs,
} from '@mantine/core';
import { IconCopy, IconCheck, IconArrowLeft } from '@tabler/icons-react';
import { useNavigate, useSearchParams } from 'react-router-dom';
import * as api from '../api/endpoints';
import type { ArtifactDetailDto } from '../api/types';
import { errorMessage, formatBytes } from '../lib/format';
import { ErrorAlert } from '../components/ErrorAlert';

/** 制品详情页面。 */
export function ArtifactDetailPage() {
  const [params] = useSearchParams();
  const navigate = useNavigate();
  const repoId = params.get('repo') ?? '';
  const path = params.get('path') ?? '';
  const [detail, setDetail] = useState<ArtifactDetailDto | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (!repoId || !path) {
      setError('缺少制品标识');
      setLoading(false);
      return;
    }
    api
      .getArtifactDetail(repoId, path)
      .then(setDetail)
      .catch((err) => setError(errorMessage(err)))
      .finally(() => setLoading(false));
  }, [repoId, path]);

  if (loading) {
    return (
      <Center h={200}>
        <Loader />
      </Center>
    );
  }

  return (
    <Stack>
      <Group>
        <Button
          variant="subtle"
          size="xs"
          leftSection={<IconArrowLeft size={16} />}
          onClick={() => navigate(-1)}
        >
          返回
        </Button>
      </Group>

      {error || !detail ? (
        <ErrorAlert message={error ?? '制品不存在'} />
      ) : (
        <>
          <Group>
            <Title order={3}>{detail.path}</Title>
            <Badge variant="light">{detail.format}</Badge>
            {detail.cached && (
              <Badge variant="light" color="cyan">
                缓存
              </Badge>
            )}
          </Group>

          <Card withBorder padding="lg" radius="md">
            <Stack gap="xs">
              <InfoRow label="所属仓库" value={detail.repo_name} />
              <InfoRow label="格式" value={detail.format} />
              <InfoRow label="大小" value={formatBytes(detail.size)} />
              <InfoRow label="内容类型" value={detail.content_type ?? '-'} />
              <InfoRow label="创建时间" value={detail.created_at} />
            </Stack>
          </Card>

          <Card withBorder padding="lg" radius="md">
            <Title order={4} mb="sm">
              校验和
            </Title>
            <Table>
              <Table.Tbody>
                <ChecksumRow label="SHA-256" value={detail.checksums.sha256} />
                <ChecksumRow label="SHA-1" value={detail.checksums.sha1} />
                <ChecksumRow label="MD5" value={detail.checksums.md5} />
                <ChecksumRow label="SHA-512" value={detail.checksums.sha512} />
              </Table.Tbody>
            </Table>
          </Card>

          {detail.usage.length > 0 && (
            <Card withBorder padding="lg" radius="md">
              <Title order={4} mb="sm">
                使用方式
              </Title>
              <Tabs defaultValue={detail.usage[0]?.title}>
                <Tabs.List>
                  {detail.usage.map((snippet) => (
                    <Tabs.Tab key={snippet.title} value={snippet.title}>
                      {snippet.title}
                    </Tabs.Tab>
                  ))}
                </Tabs.List>
                {detail.usage.map((snippet) => (
                  <Tabs.Panel key={snippet.title} value={snippet.title} pt="sm">
                    <Group justify="flex-end" mb="xs">
                      <CopyButton value={snippet.content}>
                        {({ copied, copy }) => (
                          <Button
                            size="xs"
                            variant="subtle"
                            leftSection={copied ? <IconCheck size={14} /> : <IconCopy size={14} />}
                            onClick={copy}
                          >
                            {copied ? '已复制' : '复制'}
                          </Button>
                        )}
                      </CopyButton>
                    </Group>
                    <Code block>{snippet.content}</Code>
                  </Tabs.Panel>
                ))}
              </Tabs>
            </Card>
          )}
        </>
      )}
    </Stack>
  );
}

/** 信息行。 */
function InfoRow({ label, value }: { label: string; value: string }) {
  return (
    <Group>
      <Text size="sm" c="dimmed" w={100}>
        {label}
      </Text>
      <Text size="sm">{value}</Text>
    </Group>
  );
}

/** 校验和行（带复制）。 */
function ChecksumRow({ label, value }: { label: string; value: string }) {
  return (
    <Table.Tr>
      <Table.Td w={100}>
        <Text size="sm" fw={600}>
          {label}
        </Text>
      </Table.Td>
      <Table.Td>
        <Group gap="xs" wrap="nowrap">
          <Code style={{ wordBreak: 'break-all' }}>{value}</Code>
          <CopyButton value={value}>
            {({ copied, copy }) => (
              <ActionIcon variant="subtle" onClick={copy} aria-label="复制校验和">
                {copied ? <IconCheck size={16} /> : <IconCopy size={16} />}
              </ActionIcon>
            )}
          </CopyButton>
        </Group>
      </Table.Td>
    </Table.Tr>
  );
}
