// 仓库详情：制品浏览（分页 + 前缀过滤）+ 客户端使用说明片段（可复制）。
import {
  Badge,
  Button,
  Card,
  Code,
  CopyButton,
  Group,
  Stack,
  Table,
  Text,
  TextInput,
  Title,
} from "@mantine/core";
import { EmptyState, PageHeader } from "@jianartifact/ui";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { useNavigate, useParams } from "react-router-dom";

import { AsyncBoundary } from "../components/AsyncBoundary";
import { getRepositoryUsage, listRepositoryAssets } from "../api/endpoints";
import { useAsync } from "../hooks/useAsync";

/** 字节数转人类可读大小。 */
function formatSize(bytes: number): string {
  if (bytes < 1024) {
    return `${bytes} B`;
  }
  const units = ["KB", "MB", "GB", "TB"];
  let value = bytes / 1024;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit += 1;
  }
  return `${value.toFixed(1)} ${units[unit]}`;
}

export function RepositoryDetailPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const { name = "" } = useParams();
  const [prefixInput, setPrefixInput] = useState("");
  const [prefix, setPrefix] = useState("");

  const assets = useAsync(
    () => listRepositoryAssets(name, { page_size: 100, prefix: prefix || undefined }),
    [name, prefix],
  );
  const usage = useAsync(() => getRepositoryUsage(name), [name]);

  const applyPrefix = () => {
    setPrefix(prefixInput.trim());
  };

  return (
    <>
      <PageHeader
        title={`${t("repoDetail.title")} · ${name}`}
        actions={
          <Button variant="default" onClick={() => navigate("/repositories")}>
            {t("common.close")}
          </Button>
        }
      />

      <Title order={4} mb="sm">
        {t("repoDetail.browseTitle")}
      </Title>
      <Group gap="xs" mb="sm" align="flex-end">
        <TextInput
          label={t("repoDetail.prefixLabel")}
          placeholder={t("repoDetail.prefixPlaceholder")}
          value={prefixInput}
          onChange={(e) => setPrefixInput(e.currentTarget.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter") {
              applyPrefix();
            }
          }}
          w={320}
        />
        <Button variant="light" onClick={applyPrefix}>
          {t("common.search")}
        </Button>
      </Group>

      <AsyncBoundary state={assets}>
        {(list) =>
          list.items.length === 0 ? (
            <EmptyState message={t("repoDetail.assetsEmpty")} />
          ) : (
            <Table striped highlightOnHover>
              <Table.Thead>
                <Table.Tr>
                  <Table.Th>{t("repoDetail.assetPath")}</Table.Th>
                  <Table.Th>{t("repoDetail.assetSize")}</Table.Th>
                  <Table.Th>{t("repoDetail.assetHash")}</Table.Th>
                  <Table.Th>{t("repoDetail.assetUpdatedAt")}</Table.Th>
                </Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {list.items.map((asset) => (
                  <Table.Tr key={asset.path}>
                    <Table.Td>{asset.path}</Table.Td>
                    <Table.Td>{formatSize(asset.size)}</Table.Td>
                    <Table.Td>
                      <Code>{asset.hash.slice(0, 12)}</Code>
                    </Table.Td>
                    <Table.Td>{asset.updatedAt}</Table.Td>
                  </Table.Tr>
                ))}
              </Table.Tbody>
            </Table>
          )
        }
      </AsyncBoundary>

      <Title order={4} mt="xl" mb="sm">
        {t("repoDetail.usageTitle")}
      </Title>
      <AsyncBoundary state={usage}>
        {(info) => (
          <Stack gap="md">
            <Group gap="xs">
              <Badge variant="light">{info.format}</Badge>
              <Badge variant="light" color="gray">
                {info.type}
              </Badge>
            </Group>
            {info.snippets.map((snippet, index) => (
              <Card key={index} withBorder padding="md">
                <Group justify="space-between" align="flex-start" mb="xs" wrap="nowrap">
                  <Stack gap={2}>
                    <Text fw={600}>{snippet.title}</Text>
                    {snippet.description ? (
                      <Text c="dimmed" size="sm">
                        {snippet.description}
                      </Text>
                    ) : null}
                  </Stack>
                  <CopyButton value={snippet.code}>
                    {({ copied, copy }) => (
                      <Button size="xs" variant={copied ? "filled" : "light"} onClick={copy}>
                        {copied ? t("common.copied") : t("common.copy")}
                      </Button>
                    )}
                  </CopyButton>
                </Group>
                <Code block>{snippet.code}</Code>
              </Card>
            ))}
          </Stack>
        )}
      </AsyncBoundary>
    </>
  );
}
