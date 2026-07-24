// 文件详情：元数据 + 多校验和 + 下载/HTML View + 依赖坐标 + usage 片段可复制。点文件夹时不渲染。
import { useState } from "react";
import {
  Badge,
  Button,
  Card,
  Code,
  CopyButton,
  Group,
  Select,
  Stack,
  Text,
  Title,
} from "@mantine/core";
import { IconDownload, IconExternalLink } from "@tabler/icons-react";
import { useTranslation } from "react-i18next";

import type { AssetSummary, RepoFormat, UsageInfo } from "../../api/types";
import { assetDownloadUrl, formatBytes } from "../../lib/assetTree";
import { buildCoordinateSnippets, htmlViewUrl } from "../../lib/coordinates";

interface Props {
  repoName: string;
  format: string;
  asset: AssetSummary;
  usage: UsageInfo | null;
  /** 是否展示下载按钮（协议可读） */
  showDownload?: boolean;
}

export function RepoFileDetail({
  repoName,
  format,
  asset,
  usage,
  showDownload = true,
}: Props) {
  const { t } = useTranslation();
  const downloadUrl = assetDownloadUrl(repoName, format, asset.path);
  const viewUrl = htmlViewUrl(repoName, asset.path);
  const coordinates = buildCoordinateSnippets(format as RepoFormat, asset.path);

  return (
    <Stack gap="md">
      <div>
        <Title order={5}>{t("repoDetail.fileDetailTitle")}</Title>
        <Text size="sm" c="dimmed" ff="monospace" style={{ wordBreak: "break-all" }}>
          {asset.path}
        </Text>
      </div>

      <Group gap="xs">
        <Badge variant="light">{formatBytes(asset.size)}</Badge>
        {asset.contentType && (
          <Badge variant="outline" color="gray">
            {asset.contentType}
          </Badge>
        )}
        <Badge variant="light" color="gray">
          {asset.updatedAt}
        </Badge>
      </Group>

      {/* 多校验和区域：SHA-256 / SHA-1 / MD5，各带复制按钮 */}
      <Stack gap={4}>
        <Text size="xs" c="dimmed" fw={600}>
          {t("repoDetail.assetHash")}
        </Text>
        <Group gap="xs" wrap="nowrap">
          <Code style={{ flex: 1, wordBreak: "break-all" }}>{asset.hash}</Code>
          <CopyButton value={asset.hash}>
            {({ copied, copy }) => (
              <Button size="xs" variant={copied ? "filled" : "light"} onClick={copy}>
                {copied ? t("common.copied") : t("common.copy")}
              </Button>
            )}
          </CopyButton>
        </Group>
        {asset.sha1 && (
          <ChecksumRow label={t("repoDetail.assetSha1")} value={asset.sha1} t={t} />
        )}
        {asset.md5 && (
          <ChecksumRow label={t("repoDetail.assetMd5")} value={asset.md5} t={t} />
        )}
      </Stack>

      <Group gap="xs">
        <CopyButton value={asset.path}>
          {({ copied, copy }) => (
            <Button size="xs" variant={copied ? "filled" : "light"} onClick={copy}>
              {copied ? t("common.copied") : t("repoDetail.copyPath")}
            </Button>
          )}
        </CopyButton>
        {showDownload && (
          <Button
            size="xs"
            component="a"
            href={downloadUrl}
            target="_blank"
            rel="noreferrer"
            leftSection={<IconDownload size={14} />}
            variant="light"
          >
            {t("repoDetail.download")}
          </Button>
        )}
        <Button
          size="xs"
          component="a"
          href={viewUrl}
          target="_blank"
          rel="noreferrer"
          leftSection={<IconExternalLink size={14} />}
          variant="light"
        >
          {t("repoDetail.htmlView")}
        </Button>
      </Group>

      {/* Maven 制品的多格式依赖坐标卡片（下拉切换 + 复制） */}
      {coordinates.length > 0 && <CoordinatesCard coordinates={coordinates} />}

      {usage && usage.snippets.length > 0 && (
        <Stack gap="sm">
          <Title order={6}>{t("repoDetail.usageTitle")}</Title>
          {usage.snippets.map((snippet, index) => (
            <Card key={index} withBorder padding="sm" radius="md">
              <Group justify="space-between" align="flex-start" mb={6} wrap="nowrap">
                <div>
                  <Text fw={600} size="sm">
                    {snippet.title}
                  </Text>
                  {snippet.description && (
                    <Text size="xs" c="dimmed">
                      {snippet.description}
                    </Text>
                  )}
                </div>
                <CopyButton value={snippet.code.replace("path/to/artifact", asset.path)}>
                  {({ copied, copy }) => (
                    <Button size="xs" variant={copied ? "filled" : "light"} onClick={copy}>
                      {copied ? t("common.copied") : t("common.copy")}
                    </Button>
                  )}
                </CopyButton>
              </Group>
              <Code block style={{ whiteSpace: "pre-wrap" }}>
                {snippet.code.replace("path/to/artifact", asset.path)}
              </Code>
            </Card>
          ))}
        </Stack>
      )}
    </Stack>
  );
}

/** 校验和行（标签 + 值 + 复制按钮）。 */
function ChecksumRow({
  label,
  value,
  t,
}: {
  label: string;
  value: string;
  t: (key: string) => string;
}) {
  return (
    <Group gap="xs" wrap="nowrap">
      <Text size="xs" c="dimmed" fw={600} style={{ width: 48, flexShrink: 0 }}>
        {label}
      </Text>
      <Code style={{ flex: 1, wordBreak: "break-all" }}>{value}</Code>
      <CopyButton value={value}>
        {({ copied, copy }) => (
          <Button size="xs" variant={copied ? "filled" : "light"} onClick={copy}>
            {copied ? t("common.copied") : t("common.copy")}
          </Button>
        )}
      </CopyButton>
    </Group>
  );
}

/** 依赖坐标卡片：下拉切换多格式坐标 + 复制（仅 Maven 主构件出现）。 */
function CoordinatesCard({
  coordinates,
}: {
  coordinates: { label: string; language: string; content: string }[];
}) {
  const { t } = useTranslation();
  const first = coordinates[0]!;
  const [active, setActive] = useState(first.label);
  const current = coordinates.find((c) => c.label === active) ?? first;
  return (
    <Card withBorder padding="sm" radius="md">
      <Group justify="space-between" align="flex-start" mb={6} wrap="nowrap">
        <Title order={6}>{t("repoDetail.coordinates")}</Title>
        <Select
          data={coordinates.map((c) => c.label)}
          value={active}
          onChange={(v) => setActive(v ?? first.label)}
          allowDeselect={false}
          size="xs"
          w={200}
          aria-label={t("repoDetail.coordinatesSelectAria")}
        />
      </Group>
      <Group justify="flex-end" mb={4}>
        <CopyButton value={current.content}>
          {({ copied, copy }) => (
            <Button size="xs" variant={copied ? "filled" : "light"} onClick={copy}>
              {copied ? t("common.copied") : t("common.copy")}
            </Button>
          )}
        </CopyButton>
      </Group>
      <Code block style={{ whiteSpace: "pre-wrap" }}>
        {current.content}
      </Code>
    </Card>
  );
}
