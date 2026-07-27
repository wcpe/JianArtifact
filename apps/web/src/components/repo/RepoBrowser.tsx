// 仓库浏览器：左树右详情两栏固定布局；可选 Raw 上传；管理端与公开页共用。
// FR-54: 文件树懒加载（基于 tree API 按目录按需加载）。
// FR-57: 浏览页内搜索（调用 searchAssets 带 repository 过滤）。
import {
  Alert,
  Badge,
  Box,
  Button,
  Card,
  FileButton,
  Group,
  Loader,
  LoadingOverlay,
  ScrollArea,
  Stack,
  Text,
  TextInput,
  Title,
} from "@mantine/core";
import { EmptyState } from "@jianartifact/ui";
import { IconSearch, IconUpload, IconX } from "@tabler/icons-react";
import { useCallback, useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";

import {
  getRepositoryTree,
  getRepositoryUsage,
  listRepositories,
  searchAssets,
  uploadRawAsset,
} from "../../api/endpoints";
import type { AssetSummary, Repository, UsageInfo } from "../../api/types";
import { useAsync, REFRESH_EVENT } from "../../hooks/useAsync";
import type { AssetTreeNode } from "../../lib/assetTree";
import { notifyError, notifySuccess } from "../../lib/feedback";
import { density } from "../../theme/density";
import { AsyncBoundary } from "../AsyncBoundary";
import { MavenUploadCard } from "./MavenUploadCard";
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

/** 将 tree API 响应转为 AssetTreeNode[] （目录 children=undefined 表示未加载）。 */
function treeEntryToNodes(
  dirs: string[],
  files: { path: string; size: number; hash: string; contentType?: string; updatedAt: string }[],
): AssetTreeNode[] {
  const nodes: AssetTreeNode[] = [];
  for (const d of dirs) {
    const name = d.endsWith("/") ? d.slice(0, -1).split("/").pop()! : d.split("/").pop()!;
    nodes.push({ name, path: d.replace(/\/$/, ""), kind: "dir", children: undefined });
  }
  for (const f of files) {
    const name = f.path.split("/").pop()!;
    nodes.push({
      name,
      path: f.path,
      kind: "file",
      asset: {
        path: f.path,
        size: f.size,
        hash: f.hash,
        contentType: f.contentType ?? "application/octet-stream",
        updatedAt: f.updatedAt,
      },
    });
  }
  // 目录优先，字母排序
  nodes.sort((a, b) => {
    if (a.kind !== b.kind) return a.kind === "dir" ? -1 : 1;
    return a.name.localeCompare(b.name);
  });
  return nodes;
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

  // FR-54: 懒加载树状态
  const [treeNodes, setTreeNodes] = useState<AssetTreeNode[]>([]);
  const [treeLoading, setTreeLoading] = useState(true);
  const [treeError, setTreeError] = useState<string | null>(null);

  // FR-57: 仓库内搜索
  const [searchQuery, setSearchQuery] = useState("");
  const [searchResults, setSearchResults] = useState<AssetTreeNode[] | null>(null);
  const [searching, setSearching] = useState(false);

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
  // FR-73: Maven hosted 走 GAV 表单网页上传
  const canMavenUpload = allowUpload && format === "maven" && repoType === "hosted" && !publicMode;

  // FR-54: 加载根目录；FR-69: 仅仓库切换时清空重置，刷新（上传/全局刷新）保留旧树避免整页重刷
  const prevRepoRef = useRef<string | null>(null);
  useEffect(() => {
    const repoChanged = prevRepoRef.current !== repoName;
    prevRepoRef.current = repoName;
    if (repoChanged) {
      setTreeNodes([]);
      setSelected(null);
      setSearchResults(null);
      setSearchQuery("");
    }
    setTreeLoading(true);
    setTreeError(null);
    getRepositoryTree(repoName, "")
      .then((entry) => {
        setTreeNodes(treeEntryToNodes(entry.directories, entry.files));
      })
      .catch((e: Error) => setTreeError(e.message))
      .finally(() => setTreeLoading(false));
  }, [repoName, reloadNonce]);

  // FR-54: 目录懒加载回调——递归更新树状态
  const updateNodeChildren = useCallback(
    (nodes: AssetTreeNode[], targetPath: string, children: AssetTreeNode[]): AssetTreeNode[] =>
      nodes.map((n) => {
        if (n.path === targetPath) {
          return { ...n, children };
        }
        if (n.kind === "dir" && n.children && targetPath.startsWith(n.path + "/")) {
          return { ...n, children: updateNodeChildren(n.children, targetPath, children) };
        }
        return n;
      }),
    [],
  );

  const handleExpandDir = useCallback(
    (node: AssetTreeNode) => {
      const prefix = node.path.endsWith("/") ? node.path : node.path + "/";
      getRepositoryTree(repoName, prefix)
        .then((entry) => {
          const children = treeEntryToNodes(entry.directories, entry.files);
          setTreeNodes((prev) => updateNodeChildren(prev, node.path, children));
        })
        .catch(() => {
          // 加载失败时设为空数组（显示无内容）
          setTreeNodes((prev) => updateNodeChildren(prev, node.path, []));
        });
    },
    [repoName, updateNodeChildren],
  );

  // FR-57: 仓库内搜索
  const handleInRepoSearch = () => {
    const q = searchQuery.trim();
    if (!q) {
      setSearchResults(null);
      return;
    }
    setSearching(true);
    searchAssets({ q, repository: repoName, page_size: 200 })
      .then((res) => {
        const nodes: AssetTreeNode[] = res.items.map((item) => ({
          name: item.path.split("/").pop()!,
          path: item.path,
          kind: "file" as const,
          asset: {
            path: item.path,
            size: item.size,
            hash: item.hash,
            contentType: "application/octet-stream",
            updatedAt: item.updatedAt,
          },
        }));
        setSearchResults(nodes);
      })
      .catch(() => setSearchResults([]))
      .finally(() => setSearching(false));
  };

  const clearSearch = () => {
    setSearchQuery("");
    setSearchResults(null);
  };

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

  // 文件树展示内容（搜索结果 or 懒加载树）
  const displayNodes = searchResults ?? treeNodes;
  const isEmpty = !treeLoading && !treeError && treeNodes.length === 0;

  // 全局刷新事件监听
  const reloadNonceRef = useRef(reloadNonce);
  reloadNonceRef.current = reloadNonce;
  useEffect(() => {
    const handler = () => setReloadNonce((n) => n + 1);
    window.addEventListener(REFRESH_EVENT, handler);
    return () => window.removeEventListener(REFRESH_EVENT, handler);
  }, []);

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

      {/* FR-73: Maven hosted 网页上传（GAV 表单，服务端生成 pom/校验和/metadata） */}
      {canMavenUpload && (
        <MavenUploadCard repoName={repoName} onUploaded={() => setReloadNonce((n) => n + 1)} />
      )}

      {allowUpload && !canUpload && !canMavenUpload && (
        <Alert color="gray" title={t("repoDetail.uploadClientOnlyTitle")}>
          {t("repoDetail.uploadClientOnly")}
        </Alert>
      )}

      {treeError && <Alert color="red">{treeError}</Alert>}

      {/* FR-69: 中央 Loader 仅首载（无旧树可展示）时出现 */}
      {treeLoading && treeNodes.length === 0 && (
        <Group justify="center" py="xl">
          <Loader size="sm" />
          <Text size="sm" c="dimmed">
            {t("common.loading", { defaultValue: "加载中..." })}
          </Text>
        </Group>
      )}

      {isEmpty && (
        <EmptyState
          message={t("repoDetail.assetsEmpty")}
          description={canUpload ? t("repoDetail.assetsEmptyUploadHint") : undefined}
        />
      )}

      {!treeError && treeNodes.length > 0 && (
        <Box
          style={{
            display: "flex",
            gap: "var(--mantine-spacing-md)",
            height: "calc(100vh - 280px)",
            minHeight: 400,
          }}
        >
          {/* 左侧：文件树 + 搜索 */}
          <Card
            withBorder
            padding={density.cardPadding}
            radius="md"
            style={{
              width: 360,
              minWidth: 280,
              flexShrink: 0,
              display: "flex",
              flexDirection: "column",
              position: "relative",
            }}
          >
            {/* FR-69: 刷新期间保留旧树，仅叠加覆盖层 */}
            <LoadingOverlay
              visible={treeLoading}
              zIndex={10}
              overlayProps={{ radius: "sm", blur: 1 }}
              loaderProps={{ size: "sm" }}
              transitionProps={{ duration: 150 }}
            />
            {/* FR-57: 仓库内搜索栏 */}
            <TextInput
              size="xs"
              placeholder={t("repoDetail.searchPlaceholder", { defaultValue: "搜索制品..." })}
              leftSection={<IconSearch size={14} />}
              rightSection={
                searchQuery ? (
                  <IconX size={14} style={{ cursor: "pointer" }} onClick={clearSearch} />
                ) : undefined
              }
              value={searchQuery}
              onChange={(e) => setSearchQuery(e.currentTarget.value)}
              onKeyDown={(e) => e.key === "Enter" && handleInRepoSearch()}
              mb="xs"
            />
            {searching && (
              <Group justify="center" py="xs">
                <Loader size={14} />
              </Group>
            )}
            {searchResults !== null && (
              <Text size="xs" c="dimmed" mb="xs">
                {t("repoDetail.searchResultCount", {
                  count: searchResults.length,
                  defaultValue: `找到 ${searchResults.length} 条结果`,
                })}
              </Text>
            )}
            <ScrollArea style={{ flex: 1 }} type="auto" offsetScrollbars>
              <RepoAssetTree
                nodes={displayNodes}
                selectedPath={selected?.path ?? null}
                onSelectFile={onSelectFile}
                onSelectDir={onSelectDir}
                onExpandDir={searchResults === null ? handleExpandDir : undefined}
                maxHeight="none"
              />
            </ScrollArea>
          </Card>

          {/* 右侧：文件详情 / 使用说明 */}
          <Card
            withBorder
            padding={density.cardPadding}
            radius="md"
            style={{ flex: 1, display: "flex", flexDirection: "column", overflow: "hidden" }}
          >
            <ScrollArea style={{ flex: 1 }} type="auto" offsetScrollbars>
              {selected ? (
                <RepoFileDetail
                  repoName={repoName}
                  format={format}
                  asset={selected}
                  usage={usageState.data}
                  showDownload
                />
              ) : (
                <UsagePanel usageState={usageState} />
              )}
            </ScrollArea>
          </Card>
        </Box>
      )}
    </Stack>
  );
}

/** 使用说明面板：右侧无选中文件时展示。 */
function UsagePanel({ usageState }: { usageState: ReturnType<typeof useAsync<UsageInfo>> }) {
  const { t } = useTranslation();
  return (
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
  );
}
