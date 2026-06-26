// 制品详情面板（FR-93）：右侧 / 独立页共用的制品详情展示组件。
//
// 承载元数据、四校验和、后端按格式生成的使用方式片段（FR-68）、前端生成的多格式依赖坐标
// （FR-93，下拉切换 + 复制）、HTML View 外链（FR-75 索引视图）与下载按钮。
// 纯展示：接收已加载的 ArtifactDetailDto，自身不发请求。

import { useState } from 'react';
import {
  Title,
  Stack,
  Group,
  Badge,
  Text,
  Table,
  Card,
  Code,
  CopyButton,
  ActionIcon,
  Button,
  Tabs,
  Select,
} from '@mantine/core';
import { IconCopy, IconCheck, IconDownload, IconExternalLink } from '@tabler/icons-react';
import type { ArtifactDetailDto } from '../api/types';
import { formatBytes } from '../lib/format';
import { buildCoordinateSnippets, htmlViewUrl, downloadUrl } from '../lib/coordinates';

/** 制品详情面板。 */
export function ArtifactDetailPanel({ detail }: { detail: ArtifactDetailDto }) {
  const coordinates = buildCoordinateSnippets(detail.format, detail.path);

  return (
    <Stack>
      <Group justify="space-between" wrap="nowrap">
        <Group gap="xs" wrap="nowrap">
          <Title order={4} style={{ wordBreak: 'break-all' }}>
            {detail.path}
          </Title>
          <Badge variant="light">{detail.format}</Badge>
          {detail.cached && (
            <Badge variant="light" color="cyan">
              缓存
            </Badge>
          )}
        </Group>
        <Group gap="xs" wrap="nowrap">
          <Button
            component="a"
            href={htmlViewUrl(detail.repo_name, detail.path)}
            target="_blank"
            rel="noreferrer"
            size="xs"
            variant="default"
            leftSection={<IconExternalLink size={14} />}
          >
            HTML View
          </Button>
          <Button
            component="a"
            href={downloadUrl(detail.repo_name, detail.path)}
            size="xs"
            leftSection={<IconDownload size={14} />}
          >
            下载
          </Button>
        </Group>
      </Group>

      <Card withBorder padding="md" radius="md">
        <Stack gap={4}>
          <InfoRow label="所属仓库" value={detail.repo_name} />
          <InfoRow label="格式" value={detail.format} />
          <InfoRow label="大小" value={formatBytes(detail.size)} />
          <InfoRow label="内容类型" value={detail.content_type ?? '-'} />
          <InfoRow label="创建时间" value={detail.created_at} />
        </Stack>
      </Card>

      <Card withBorder padding="md" radius="md">
        <Title order={5} mb="sm">
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

      {coordinates.length > 0 && <CoordinatesCard />}

      {detail.usage.length > 0 && (
        <Card withBorder padding="md" radius="md">
          <Title order={5} mb="sm">
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
                <CopyableCode content={snippet.content} />
              </Tabs.Panel>
            ))}
          </Tabs>
        </Card>
      )}
    </Stack>
  );

  /** 依赖坐标卡片：下拉切换多格式坐标 + 复制（仅 Maven 主构件出现）。 */
  function CoordinatesCard() {
    const [active, setActive] = useState(coordinates[0].label);
    const current = coordinates.find((c) => c.label === active) ?? coordinates[0];
    return (
      <Card withBorder padding="md" radius="md">
        <Group justify="space-between" mb="sm">
          <Title order={5}>依赖坐标</Title>
          <Select
            data={coordinates.map((c) => c.label)}
            value={active}
            onChange={(v) => setActive(v ?? coordinates[0].label)}
            allowDeselect={false}
            size="xs"
            w={200}
            aria-label="选择依赖坐标格式"
          />
        </Group>
        <CopyableCode content={current.content} />
      </Card>
    );
  }
}

/** 信息行。 */
function InfoRow({ label, value }: { label: string; value: string }) {
  return (
    <Group gap="sm" wrap="nowrap">
      <Text size="sm" c="dimmed" w={80}>
        {label}
      </Text>
      <Text size="sm" style={{ wordBreak: 'break-all' }}>
        {value}
      </Text>
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

/** 带复制按钮的代码块。 */
function CopyableCode({ content }: { content: string }) {
  return (
    <>
      <Group justify="flex-end" mb="xs">
        <CopyButton value={content}>
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
      <Code block>{content}</Code>
    </>
  );
}
