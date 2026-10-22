// 搜索结果右栏：选中文件详情 + 同版本文件列表。
//
// 与左栏树的分工：树负责"在哪个版本里"，详情负责"这个文件是什么、怎么取走"。
// 同版本文件列表可就地切换选中项（不返回列表、不重发请求）；窄屏下由父级传入
// `onBack` 渲染顶部返回行，双栏下不传即不渲染。
import {
  Badge,
  Button,
  Code,
  Group,
  Paper,
  Stack,
  Text,
  Title,
  UnstyledButton,
} from "@mantine/core";
import { IconArrowLeft, IconDownload, IconExternalLink } from "@tabler/icons-react";
import type { ReactNode } from "react";
import { useTranslation } from "react-i18next";

import type { SearchPackage, SearchPackageFile, SearchPackageVersion } from "../../api/endpoints";
import { assetDownloadUrl, formatBytes } from "../../lib/assetTree";
import { fileKey, precisionMeta } from "../../lib/searchPackage";
import { formatUtcToLocal } from "../../lib/timeFormat";
import { CopyTextButton } from "../CopyTextButton";

/** 元数据行：左标签右值，值允许换行（路径 / 哈希会很长）。 */
function MetaRow({ label, children }: { label: string; children: ReactNode }) {
  return (
    <Group gap={8} wrap="nowrap" align="flex-start">
      <Text size="xs" c="dimmed" style={{ flexShrink: 0, width: 72 }}>
        {label}
      </Text>
      <div style={{ flex: 1, minWidth: 0 }}>{children}</div>
    </Group>
  );
}

interface SearchDetailPanelProps {
  pkg: SearchPackage;
  /** 无坐标伪包没有版本层。 */
  version: SearchPackageVersion | null;
  file: SearchPackageFile;
  /** 同版本（伪包为同包）的全部文件。 */
  siblings: readonly SearchPackageFile[];
  /** 窄屏返回列表；不传则不渲染返回行（双栏不需要）。 */
  onBack?: () => void;
  onSelectFile: (file: SearchPackageFile) => void;
  onOpenRepository: (repository: string, path: string) => void;
}

/** 选中文件详情面板。 */
export function SearchDetailPanel({
  pkg,
  version,
  file,
  siblings,
  onBack,
  onSelectFile,
  onOpenRepository,
}: SearchDetailPanelProps) {
  const { t } = useTranslation();
  const meta = precisionMeta(version?.precision);
  const downloadUrl = assetDownloadUrl(file.repository, "", file.path);
  // SHA1 截断显示：完整值在 title 与复制按钮里，列表里不必占满一行。
  const sha1 = file.sha1 ?? "";
  const sha1Short = sha1.length > 16 ? `${sha1.slice(0, 16)}…` : sha1 || "—";
  const currentKey = fileKey(file);

  return (
    <Stack gap="md" data-testid="search-detail-panel">
      {onBack && (
        <Button
          variant="subtle"
          size="compact-sm"
          leftSection={<IconArrowLeft size={14} />}
          onClick={onBack}
          aria-label={t("search.backToResults")}
          style={{ alignSelf: "flex-start" }}
        >
          {t("search.backToResults")}
        </Button>
      )}

      <div>
        <Title order={5} style={{ wordBreak: "break-all" }}>
          {file.name}
        </Title>
        <Group gap={6} wrap="wrap" mt={4}>
          <Text size="xs" c="dimmed">
            {pkg.kind === "file"
              ? pkg.coordinate
              : `${pkg.coordinate}${version ? `:${version.version}` : ""}`}
          </Text>
          {meta.labelKey && (
            <Badge variant="light" color={meta.color} size="xs">
              {t(meta.labelKey)}
            </Badge>
          )}
        </Group>
      </div>

      <Paper withBorder radius="md" p="sm">
        <Stack gap={8}>
          <MetaRow label={t("search.detailCoordinate")}>
            <Text size="xs" style={{ wordBreak: "break-all" }}>
              {pkg.coordinate}
            </Text>
          </MetaRow>
          {version && (
            <MetaRow label={t("search.detailVersion")}>
              <Group gap={6} wrap="wrap">
                <Text size="xs">{version.version}</Text>
                {meta.labelKey && (
                  <Badge variant="light" color={meta.color} size="xs">
                    {t(meta.labelKey)}
                  </Badge>
                )}
              </Group>
            </MetaRow>
          )}
          <MetaRow label={t("search.detailRepository")}>
            <Badge variant="light" size="xs">
              {file.repository}
            </Badge>
          </MetaRow>
          <MetaRow label={t("search.detailPath")}>
            <Group gap={6} wrap="nowrap" align="flex-start">
              <Code
                style={{
                  flex: 1,
                  minWidth: 0,
                  wordBreak: "break-all",
                  whiteSpace: "pre-wrap",
                  fontSize: "var(--mantine-font-size-xs)",
                }}
              >
                {file.path}
              </Code>
              <CopyTextButton
                value={file.path}
                size="compact-xs"
                aria-label={t("repoDetail.copyPath")}
              />
            </Group>
          </MetaRow>
          <MetaRow label={t("search.detailSize")}>
            <Text size="xs">{formatBytes(file.size)}</Text>
          </MetaRow>
          <MetaRow label={t("search.detailUpdated")}>
            <Text size="xs">{formatUtcToLocal(file.updatedAt)}</Text>
          </MetaRow>
          <MetaRow label={t("search.detailSha1")}>
            <Group gap={6} wrap="nowrap">
              <Text size="xs" ff="monospace" title={sha1 || undefined}>
                {sha1Short}
              </Text>
              {sha1 && (
                <CopyTextButton
                  value={sha1}
                  size="compact-xs"
                  aria-label={t("search.detailSha1")}
                />
              )}
            </Group>
          </MetaRow>
        </Stack>
      </Paper>

      <Group gap="xs" wrap="wrap">
        <Button
          component="a"
          href={downloadUrl}
          target="_blank"
          rel="noreferrer"
          size="xs"
          variant="light"
          leftSection={<IconDownload size={14} />}
        >
          {t("repoDetail.download")}
        </Button>
        {/* 带命中路径进仓库详情，进仓库后直接定位到该文件。 */}
        <Button
          size="xs"
          variant="light"
          leftSection={<IconExternalLink size={14} />}
          onClick={() => onOpenRepository(file.repository, file.path)}
        >
          {t("search.openRepository")}
        </Button>
      </Group>

      <div>
        <Text size="sm" fw={600} mb={6}>
          {t("search.sameVersionFiles", { count: siblings.length })}
        </Text>
        <Stack gap={2}>
          {siblings.map((item) => {
            const selected = fileKey(item) === currentKey;
            return (
              // 行容器**不能**是按钮：行内的「下载」是 `<a href>`，而 `<button>` 里既不允许
              // 出现可交互后代（HTML 内容模型禁止），浏览器也会把可聚焦后代从 Tab 序列里剔除，
              // 键盘用户根本到不了下载链接。故拆成并列的「选中按钮 + 下载链接」两个元素，
              // 行样式（内边距 / 选中底色）挂在容器上。
              <Group
                key={fileKey(item)}
                gap={8}
                wrap="nowrap"
                data-selected={selected ? "true" : undefined}
                style={{
                  padding: "4px 8px",
                  borderRadius: 4,
                  // 选中行高亮：规格指定的蓝 0 号底色。
                  background: selected ? "var(--mantine-color-blue-0)" : undefined,
                }}
              >
                <UnstyledButton
                  onClick={() => onSelectFile(item)}
                  aria-current={selected ? "true" : undefined}
                  data-testid="search-sibling-file"
                  style={{ display: "block", flex: 1, minWidth: 0, textAlign: "left" }}
                >
                  <Group gap={8} wrap="nowrap">
                    <Text size="xs" lineClamp={1} style={{ flex: 1, minWidth: 0 }}>
                      {item.name}
                    </Text>
                    <Text size="xs" c="dimmed" style={{ flexShrink: 0 }}>
                      {formatBytes(item.size)}
                    </Text>
                  </Group>
                </UnstyledButton>
                <Button
                  component="a"
                  href={assetDownloadUrl(item.repository, "", item.path)}
                  target="_blank"
                  rel="noreferrer"
                  size="compact-xs"
                  variant="subtle"
                  leftSection={<IconDownload size={12} />}
                  aria-label={`${t("repoDetail.download")} ${item.name}`}
                  style={{ flexShrink: 0 }}
                >
                  {t("repoDetail.download")}
                </Button>
              </Group>
            );
          })}
        </Stack>
      </div>
    </Stack>
  );
}
