// 仓库浏览器：左树右详情；可选 Raw 上传；管理端与公开页共用。
import {
  Alert,
  Badge,
  Button,
  Card,
  FileButton,
  Group,
  SimpleGrid,
  Stack,
  Text,
  TextInput,
  Title,
} from "@mantine/core";
import { EmptyState } from "@jianartifact/ui";
import { IconUpload } from "@tabler/icons-react";
import { useMemo, useState } from "react";
import { useTranslation } from "react-i18next";

import {
  getRepositoryUsage,
  listAllRepositoryAssets,
  listRepositories,
  uploadRawAsset,
} from "../../api/endpoints";
import type { AssetSummary, Repository, UsageInfo } from "../../api/types";
import { useAsync } from "../../hooks/useAsync";
import { buildAssetTree, type AssetTreeNode } from "../../lib/assetTree";
import { notifyError, notifySuccess } from "../../lib/feedback";
import { density } from "../../theme/density";
import { AsyncBoundary } from "../AsyncBoundary";
import { RepoAssetTree } from "./RepoAssetTree";
import { RepoFileDetail } from "./RepoFileDetail";

interface Props {
  repoName: string;
  /** 管理端可上传；公开页 false */
  allowUpload?: boolean;
  /** 公开页不拉仓库列表（匿名 list 可能空/需登录） */
  publicMode?: boolean;
  /** 公开页已知的 format（来自 usage） */
  forcedFormat?: string;
  forcedType?: string;
}

export function RepoBrowser({
  repoName,
  allowUpload = false,
  publicMode = false,
  forcedFormat,
  forcedType,
}: Props) {
  const { t } = useTranslation();
  const [selected, setSelected] = useState<AssetSummary | null>(null);
  const [uploadPath, setUploadPath] = useState("");
  const [uploading, setUploading] = useState(false);
  const [reloadNonce, setReloadNonce] = useState(0);

  const assetsState = useAsync(() => listAllRepositoryAssets(repoName), [repoName, reloadNonce]);
  const usageState = useAsync(() => getRepositoryUsage(repoName), [repoName]);
  const repoState = useAsync(
    () =>
      publicMode
        ? Promise.resolve(null as Repository | null)
        : listRepositories({ page_size: 100 }).then(
            (list) => list.items.find((r) => r.name === repoName) ?? null,
          ),
    [repoName, publicMode],
  );

  const format = forcedFormat || repoState.data?.format || usageState.data?.format || "raw";
  const repoType = forcedType || repoState.data?.type || usageState.data?.type || "hosted";
  const canUpload = allowUpload && format === "raw" && repoType === "hosted" && !publicMode;

  const tree = useMemo(() => {
    if (!assetsState.data) {
      return [] as AssetTreeNode[];
    }
    return buildAssetTree(assetsState.data.items);
  }, [assetsState.data]);

  const onSelectFile = (node: AssetTreeNode) => {
    if (node.asset) {
      setSelected(node.asset);
    }
  };
  const onSelectDir = () => {
    setSelected(null);
  };

  const handleUpload = (file: File | null) => {
    if (!file || !canUpload) {
      return;
    }
    const path = (uploadPath.trim() || file.name).replace(/^\/+/, "");
    if (!path) {
      notifyError(t("repoDetail.uploadNeedPath"));
      return;
    }
    setUploading(true);
    uploadRawAsset(repoName, path, file)
      .then(() => {
        notifySuccess(t("repoDetail.uploadOk"));
        setUploadPath("");
        setReloadNonce((n) => n + 1);
      })
      .catch((e: Error) => notifyError(e.message || t("common.error")))
      .finally(() => setUploading(false));
  };

  return (
    <Stack gap="md">
      <Group gap="xs">
        {format && <Badge variant="light">{format}</Badge>}
        {repoType && (
          <Badge variant="outline" color="gray">
            {repoType}
          </Badge>
        )}
        {repoState.data?.visibility && (
          <Badge variant="light" color={repoState.data.visibility === "public" ? "blue" : "gray"}>
            {repoState.data.visibility === "public"
              ? t("repositories.visibilityPublic")
              : t("repositories.visibilityPrivate")}
          </Badge>
        )}
        {publicMode && (
          <Badge variant="light" color="teal">
            {t("repoDetail.publicBadge")}
          </Badge>
        )}
      </Group>

      {canUpload && (
        <Card withBorder padding={density.cardPadding} radius="md">
          <Stack gap="sm">
            <Title order={5}>{t("repoDetail.uploadTitle")}</Title>
            <Text size="xs" c="dimmed">
              {t("repoDetail.uploadHint")}
            </Text>
            <TextInput
              label={t("repoDetail.uploadPath")}
              description={t("repoDetail.uploadPathHint")}
              placeholder="path/to/file.bin"
              value={uploadPath}
              onChange={(e) => setUploadPath(e.currentTarget.value)}
              disabled={uploading}
            />
            <Group>
              <FileButton onChange={handleUpload} disabled={uploading}>
                {(props) => (
                  <Button {...props} leftSection={<IconUpload size={16} />} loading={uploading}>
                    {t("repoDetail.uploadPick")}
                  </Button>
                )}
              </FileButton>
            </Group>
          </Stack>
        </Card>
      )}

      {allowUpload && !canUpload && (
        <Alert color="gray" title={t("repoDetail.uploadClientOnlyTitle")}>
          {t("repoDetail.uploadClientOnly")}
        </Alert>
      )}

      <AsyncBoundary state={assetsState}>
        {(data) =>
          data.items.length === 0 ? (
            <EmptyState
              message={t("repoDetail.assetsEmpty")}
              description={canUpload ? t("repoDetail.assetsEmptyUploadHint") : undefined}
            />
          ) : (
            <Stack gap="sm">
              {data.truncated && (
                <Alert color="yellow">
                  {t("repoDetail.treeTruncated", { n: data.items.length, total: data.total })}
                </Alert>
              )}
              <Text size="sm" c="dimmed">
                {t("repoDetail.assetCount", { n: data.total })}
              </Text>
              <SimpleGrid cols={{ base: 1, md: 2 }} spacing="md">
                <Card withBorder padding={density.cardPadding} radius="md">
                  <Title order={5} mb="sm">
                    {t("repoDetail.treeTitle")}
                  </Title>
                  <RepoAssetTree
                    nodes={tree}
                    selectedPath={selected?.path ?? null}
                    onSelectFile={onSelectFile}
                    onSelectDir={onSelectDir}
                  />
                </Card>
                <Card withBorder padding={density.cardPadding} radius="md">
                  {selected ? (
                    <RepoFileDetail
                      repoName={repoName}
                      format={format}
                      asset={selected}
                      usage={usageState.data}
                      showDownload
                    />
                  ) : (
                    <Stack gap="xs" py="xl" align="center">
                      <Text c="dimmed" size="sm">
                        {t("repoDetail.selectFileHint")}
                      </Text>
                    </Stack>
                  )}
                </Card>
              </SimpleGrid>
            </Stack>
          )
        }
      </AsyncBoundary>

      {/* 仓级 usage 总览（未选文件时也可看） */}
      {!selected && (
        <AsyncBoundary state={usageState}>
          {(usage: UsageInfo) => (
            <Stack gap="sm">
              <Title order={5}>{t("repoDetail.usageTitle")}</Title>
              {usage.snippets.map((snippet, index) => (
                <Card key={index} withBorder padding="sm" radius="md">
                  <Text fw={600} size="sm" mb={4}>
                    {snippet.title}
                  </Text>
                  {snippet.description && (
                    <Text size="xs" c="dimmed" mb={6}>
                      {snippet.description}
                    </Text>
                  )}
                  <Text
                    component="pre"
                    size="xs"
                    ff="monospace"
                    style={{
                      whiteSpace: "pre-wrap",
                      margin: 0,
                      background: "var(--mantine-color-default-hover)",
                      padding: 8,
                      borderRadius: 4,
                    }}
                  >
                    {snippet.code}
                  </Text>
                </Card>
              ))}
            </Stack>
          )}
        </AsyncBoundary>
      )}
    </Stack>
  );
}
