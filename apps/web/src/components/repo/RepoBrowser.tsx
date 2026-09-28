// 仓库浏览器：左树右详情两栏固定布局；可选 Raw 上传；管理端与公开页共用。
// FR-54: 文件树懒加载（基于 tree API 按目录按需加载）。
// FR-57: 浏览页内搜索（调用 searchAssets 带 repository 过滤）。
import {
  Alert,
  Anchor,
  Box,
  Button,
  Card,
  Collapse,
  Divider,
  Drawer,
  FileButton,
  Group,
  Loader,
  LoadingOverlay,
  Modal,
  ScrollArea,
  Select,
  Skeleton,
  Stack,
  Text,
  TextInput,
  Title,
} from "@mantine/core";
import { EmptyState } from "@jianartifact/ui";
import { ContentSkeleton } from "../AsyncBoundary";
import {
  IconChevronDown,
  IconChevronUp,
  IconRefresh,
  IconSearch,
  IconTrash,
  IconUpload,
  IconX,
} from "@tabler/icons-react";
import { useLocalStorage, useMediaQuery } from "@mantine/hooks";
import { useCallback, useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";

import {
  applyAssetOperation,
  getRepositoryTree,
  getRepositoryUsage,
  listAllRepositories,
  searchAssets,
  uploadRawAsset,
} from "../../api/endpoints";
import type { AssetSummary, Repository, UsageInfo, UsageSnippet } from "../../api/types";
import { useAsync, REFRESH_EVENT } from "../../hooks/useAsync";
import type { AssetTreeNode } from "../../lib/assetTree";
import { buildAssetTree } from "../../lib/assetTree";
import { assetOperationTarget, operationSupportsPathMutation } from "../../lib/assetOperations";
import { confirmDanger, notifyError, notifySuccess } from "../../lib/feedback";
import { useAuth } from "../../auth/AuthContext";
import { density } from "../../theme/density";
import { AsyncBoundary } from "../AsyncBoundary";
import { CopyTextButton } from "../CopyTextButton";
import { MavenUploadCard } from "./MavenUploadCard";
import { RepoAssetTree } from "./RepoAssetTree";
import { RepoFileDetail } from "./RepoFileDetail";
import { TrendChart } from "../observability/TrendChart";

/**
 * 仓库级下载趋势视图（阶段 D-1）：由 RepositoryDetailPage 取数后下传，
 * 避免 RepoBrowser 自己再发一次同口径请求。三态互斥，供右栏顶部固定区渲染。
 */
export interface RepoDownloadTrendView {
  /** 近 24h 趋势点（label=桶起点本地时间，value=下载次数）；尚无数据时为 null。 */
  points: { label: string; value: number }[] | null;
  /** 首载进行中且无数据可展示 → 渲染骨架。 */
  loading: boolean;
  /** 取数失败且无数据可展示 → 渲染降级文案。 */
  error: boolean;
  /**
   * 全时段累计下载次数（接口未就绪/失败时为 null）。图表区已改为默认直接展示、
   * 不再有折叠摘要行，故本字段当前未被本组件消费；保留以兼容调用方传参。
   */
  total: number | null;
}

interface Props {
  repoName: string;
  /** 管理端可上传；公开页 false */
  allowUpload?: boolean;
  /** 公开页不拉仓库列表（匿名 list 可能空/需登录） */
  publicMode?: boolean;
  /** 公开页已知的 format（来自 usage） */
  forcedFormat?: string;
  forcedType?: string;
  /** FR-145：搜索结果直达的命中路径（进入时逐级展开并选中；失效静默降级）。 */
  highlightPath?: string;
  /**
   * 仓库级下载趋势：常驻渲染在右侧详情卡片顶部，与「当前选中文件」无关。
   * 由详情页取数后下传（不传则不渲染该区域，既有直接使用 RepoBrowser 的场景不受影响）。
   */
  downloadTrend?: RepoDownloadTrendView;
}

/** 将 tree API 响应转为 AssetTreeNode[] （目录 children=undefined 表示未加载）。 */
function treeEntryToNodes(
  dirs: string[],
  files: {
    path: string;
    size: number;
    hash: string;
    sha1?: string;
    md5?: string;
    contentType?: string;
    createdAt?: string;
    updatedAt: string;
    downloadCount?: number;
  }[],
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
      downloadCount: f.downloadCount,
      asset: {
        path: f.path,
        size: f.size,
        hash: f.hash,
        sha1: f.sha1,
        md5: f.md5,
        contentType: f.contentType ?? "application/octet-stream",
        createdAt: f.createdAt,
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

function visibleTreePaths(event: React.MouseEvent<HTMLButtonElement>, fallback: string): string[] {
  const tree = event.currentTarget.closest('[role="tree"]');
  if (!tree) return [fallback];
  return [...tree.querySelectorAll<HTMLElement>('[role="treeitem"][data-path]')]
    .map((element) => element.dataset.path)
    .filter((path): path is string => Boolean(path));
}

function selectTreePaths(
  nodePath: string,
  visiblePaths: string[],
  selectedPaths: Set<string>,
  anchor: string | null,
  event: React.MouseEvent<HTMLButtonElement>,
): { paths: Set<string>; anchor: string } {
  const index = visiblePaths.indexOf(nodePath);
  const anchorIndex = anchor ? visiblePaths.indexOf(anchor) : -1;
  if (event.shiftKey && index >= 0 && anchorIndex >= 0) {
    const start = Math.min(index, anchorIndex);
    const end = Math.max(index, anchorIndex);
    return { paths: new Set(visiblePaths.slice(start, end + 1)), anchor: anchor ?? nodePath };
  }
  const next = event.ctrlKey || event.metaKey ? new Set(selectedPaths) : new Set<string>();
  if (event.ctrlKey || event.metaKey) {
    if (next.has(nodePath)) next.delete(nodePath);
    else next.add(nodePath);
  } else {
    next.add(nodePath);
  }
  return { paths: next, anchor: nodePath };
}

export function RepoBrowser({
  repoName,
  allowUpload = false,
  publicMode = false,
  forcedFormat,
  forcedType,
  highlightPath,
  downloadTrend,
}: Props) {
  const { t } = useTranslation();
  const { user } = useAuth();
  // FR-105：资产操作仅全局管理员可见可用。
  const isAdmin = user?.role === "admin";
  const [selected, setSelected] = useState<AssetSummary | null>(null);
  // FR-105：文件和目录的选择路径集合。
  const [selectedPaths, setSelectedPaths] = useState<Set<string>>(new Set());
  const [selectionAnchor, setSelectionAnchor] = useState<string | null>(null);
  const [operationBusy, setOperationBusy] = useState(false);
  const [contextMenu, setContextMenu] = useState<{ x: number; y: number } | null>(null);
  const [pathOperation, setPathOperation] = useState<"move" | "rename" | null>(null);
  const [pathInput, setPathInput] = useState("");
  const [uploadPath, setUploadPath] = useState("");
  const [uploading, setUploading] = useState(false);
  const [reloadNonce, setReloadNonce] = useState(0);
  // 上传区默认收起，点击按钮展开（避免常驻占位挤压文件树）。
  const [uploadOpen, setUploadOpen] = useState(false);
  // 窄屏底部抽屉：选中文件时自动打开；未选中文件时由树卡下方的「使用说明」入口打开。
  const [usageDrawerOpen, setUsageDrawerOpen] = useState(false);

  // FR-99: 左树宽度可拖拽调整（分割条），偏好本地持久化；min 280 / max 720。
  const [treeWidth, setTreeWidth] = useLocalStorage<number>({
    key: "jianartifact.treeWidth",
    defaultValue: 360,
    getInitialValueInEffect: false,
  });
  // 窄屏（< 48em）：左右并排会把两栏都压到不可读——树宽 280px 起步，390px 手机上
  // 详情栏只剩几十像素（文字竖排溢出）。此宽度起改为上下堆叠，各自内滚。
  const isNarrow = useMediaQuery("(max-width: 48em)") ?? false;
  const dragRef = useRef<{ startX: number; startWidth: number } | null>(null);
  const onSplitterMouseDown = (e: React.MouseEvent) => {
    e.preventDefault();
    dragRef.current = { startX: e.clientX, startWidth: treeWidth };
    document.body.style.cursor = "col-resize";
    document.body.style.userSelect = "none";
    const onMove = (ev: MouseEvent) => {
      if (!dragRef.current) return;
      const delta = ev.clientX - dragRef.current.startX;
      setTreeWidth(Math.min(720, Math.max(280, dragRef.current.startWidth + delta)));
    };
    const onUp = () => {
      dragRef.current = null;
      document.body.style.cursor = "";
      document.body.style.userSelect = "";
      window.removeEventListener("mousemove", onMove);
      window.removeEventListener("mouseup", onUp);
    };
    window.addEventListener("mousemove", onMove);
    window.addEventListener("mouseup", onUp);
  };

  // FR-54: 懒加载树状态
  const [treeNodes, setTreeNodes] = useState<AssetTreeNode[]>([]);
  const [treeLoading, setTreeLoading] = useState(true);
  const [treeError, setTreeError] = useState<string | null>(null);
  const [treeActionError, setTreeActionError] = useState<string | null>(null);

  // FR-57: 仓库内搜索
  const [searchQuery, setSearchQuery] = useState("");
  const [searchResults, setSearchResults] = useState<AssetTreeNode[] | null>(null);
  // 分页累积的原始结果（供「加载更多」追加后重建树）与后端命中的总数
  const [searchLoaded, setSearchLoaded] = useState<AssetSummary[]>([]);
  const [searchTotal, setSearchTotal] = useState(0);
  const [searchPage, setSearchPage] = useState(1);
  const [searching, setSearching] = useState(false);

  const usageState = useAsync(() => getRepositoryUsage(repoName), [repoName], {
    cacheKey: `repo:usage:${repoName}`,
  });
  const repoState = useAsync(
    () =>
      publicMode
        ? Promise.resolve(null as Repository | null)
        : listAllRepositories().then(
            (list) =>
              list.items.find((r) => r.name === repoName || r.aliases?.includes(repoName)) ?? null,
          ),
    [repoName, publicMode],
    // 公开态不发请求，**不能**与管理态共用缓存键：useAsync 的同键在途去重会把
    // 「立刻 resolve(null)」的 Promise 复用给真正要仓库数据的调用方（详情页页头），
    // 表现为仓库信息永远停在骨架、徽章不出现。
    publicMode ? undefined : { cacheKey: `repo:detail:${repoName}:managed` },
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
      setTreeActionError(null);
      setSelected(null);
      setSelectedPaths(new Set());
      setSelectionAnchor(null);
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
      setTreeActionError(null);
      // 返回本次加载出的子节点（FR-145 的逐级定位需要按序等待；既有调用忽略返回值，兼容）。
      return getRepositoryTree(repoName, prefix)
        .then((entry) => {
          const children = treeEntryToNodes(entry.directories, entry.files);
          setTreeNodes((prev) => updateNodeChildren(prev, node.path, children));
          return children;
        })
        .catch((error: unknown) => {
          setTreeActionError(error instanceof Error ? error.message : t("common.error"));
          return undefined;
        });
    },
    [repoName, t, updateNodeChildren],
  );

  // FR-57: 仓库内搜索（结果拼成目录树展示，而非拍平列表）
  const handleInRepoSearch = () => {
    const q = searchQuery.trim();
    setSelectedPaths(new Set());
    setSelectionAnchor(null);
    if (!q) {
      setSearchResults(null);
      setSearchLoaded([]);
      setSearchTotal(0);
      return;
    }
    setTreeActionError(null);
    setSearching(true);
    // page_size 取后端上限（100）：此前传 200 会被后端静默丢弃、回落默认 20 条
    // （表现为「仓库内搜索只能搜到前几个结果」）；超出部分由「加载更多」翻页补齐。
    searchAssets({ q, repository: repoName, page: 1, page_size: 100 })
      .then((res) => {
        const assets: AssetSummary[] = res.items.map((item) => ({
          path: item.path,
          size: item.size,
          hash: item.hash,
          contentType: "application/octet-stream",
          updatedAt: item.updatedAt,
        }));
        setSearchLoaded(assets);
        setSearchTotal(res.total);
        setSearchPage(1);
        setSearchResults(buildAssetTree(assets));
      })
      .catch((error: unknown) => {
        setTreeActionError(error instanceof Error ? error.message : t("common.error"));
      })
      .finally(() => setSearching(false));
  };

  // 「加载更多」：命中数超过单页上限（100）时逐页追加，直至翻完 searchTotal。
  const handleLoadMoreSearch = () => {
    const q = searchQuery.trim();
    if (!q) return;
    setTreeActionError(null);
    setSearching(true);
    searchAssets({ q, repository: repoName, page: searchPage + 1, page_size: 100 })
      .then((res) => {
        const added: AssetSummary[] = res.items.map((item) => ({
          path: item.path,
          size: item.size,
          hash: item.hash,
          contentType: "application/octet-stream",
          updatedAt: item.updatedAt,
        }));
        const merged = [...searchLoaded, ...added];
        setSearchLoaded(merged);
        setSearchPage((page) => page + 1);
        setSearchResults(buildAssetTree(merged));
      })
      .catch((error: unknown) => {
        setTreeActionError(error instanceof Error ? error.message : t("common.error"));
      })
      .finally(() => setSearching(false));
  };

  const clearSearch = () => {
    setSearchQuery("");
    setSearchResults(null);
    setSearchLoaded([]);
    setSearchTotal(0);
    setTreeActionError(null);
    setSelectedPaths(new Set());
    setSelectionAnchor(null);
  };

  const onSelectFile = (node: AssetTreeNode) => {
    if (node.asset) {
      // 窄屏：选中制品即弹出底部抽屉（抽屉内容按 selected 判定），
      // 先收起「使用说明」抽屉态，避免关闭后又回到使用说明。
      setUsageDrawerOpen(false);
      setSelected(node.asset);
    }
  };
  const onSelectDir = () => {
    setSelected(null);
    setSelectedPaths(new Set());
  };

  const findNode = (path: string, nodes: AssetTreeNode[]): AssetTreeNode | null => {
    for (const node of nodes) {
      if (node.path === path) return node;
      if (node.children) {
        const found = findNode(path, node.children);
        if (found) return found;
      }
    }
    return null;
  };

  const selectedTargets = (paths: string[]) =>
    paths.map((path) => {
      const node = findNode(path, displayNodes) ?? { path, kind: "file" as const };
      return assetOperationTarget(format, node.path, node.kind);
    });

  // FR-105：在当前可见树顺序中处理普通、Ctrl/Meta、Shift 选择。
  const onNodeInteraction = (node: AssetTreeNode, event: React.MouseEvent<HTMLButtonElement>) => {
    if (!isAdmin) {
      if (node.kind === "dir") onSelectDir();
      else onSelectFile(node);
      return;
    }
    const selection = selectTreePaths(
      node.path,
      visibleTreePaths(event, node.path),
      selectedPaths,
      selectionAnchor,
      event,
    );
    setSelectedPaths(selection.paths);
    setSelectionAnchor(selection.anchor);
    setSelected(node.kind === "file" ? (node.asset ?? null) : null);
  };

  const handleContextMenu = (node: AssetTreeNode, event: React.MouseEvent<HTMLButtonElement>) => {
    event.preventDefault();
    if (!isAdmin) return;
    if (!selectedPaths.has(node.path)) {
      setSelectedPaths(new Set([node.path]));
      setSelectionAnchor(node.path);
      setSelected(node.kind === "file" ? (node.asset ?? null) : null);
    }
    setContextMenu({ x: event.clientX, y: event.clientY });
  };

  const submitDelete = (paths: string[]) => {
    setOperationBusy(true);
    applyAssetOperation(repoName, { action: "delete", targets: selectedTargets(paths) })
      .then((res) => {
        notifySuccess(t("repoDetail.batchDeleteOk", { count: res.affected }));
        setSelectedPaths(new Set());
        setSelectionAnchor(null);
        setSelected(null);
        setReloadNonce((n) => n + 1);
      })
      .catch(notifyError)
      .finally(() => setOperationBusy(false));
  };

  // FR-105：统一删除事务成功后才清空选择；失败时保留选择以便重试。
  const handleBatchDelete = () => {
    const paths = [...selectedPaths];
    if (paths.length === 0) {
      notifyError(t("repoDetail.batchDeleteEmptySelection"));
      return;
    }
    confirmDanger({
      title: t("common.delete"),
      message: t("repoDetail.batchDeleteConfirm", { count: paths.length }),
      confirmLabel: t("common.delete"),
      cancelLabel: t("common.cancel"),
      onConfirm: () => submitDelete(paths),
    });
  };

  const openPathOperation = (action: "move" | "rename") => {
    setContextMenu(null);
    const first = [...selectedPaths][0] ?? "";
    setPathOperation(action);
    setPathInput(action === "rename" ? first : "");
  };

  const submitPathOperation = () => {
    if (!pathOperation || !pathInput.trim()) return;
    const paths = [...selectedPaths];
    setOperationBusy(true);
    applyAssetOperation(repoName, {
      action: pathOperation,
      targets: selectedTargets(paths),
      ...(pathOperation === "move"
        ? { destinationPath: pathInput.trim() }
        : { newPath: pathInput.trim() }),
    })
      .then((res) => {
        notifySuccess(t("repoDetail.assetOperationOk", { count: res.affected }));
        setPathOperation(null);
        setSelectedPaths(new Set());
        setSelectionAnchor(null);
        setSelected(null);
        setReloadNonce((n) => n + 1);
      })
      .catch(notifyError)
      .finally(() => setOperationBusy(false));
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

  // FR-145：搜索结果直达——启动一次定位，内部等待根层就绪后逐级懒加载展开并选中命中文件；
  // 路径失效静默降级。注意两点：
  // 1) 定位链**不能**随 effect 依赖变化被取消——每次展开都会 setTreeNodes，若依赖它做 cleanup，
  //    链会在第一步展开后被打断且 consumed 标记已写、不再重启（线上真实网络延迟下必现）；
  // 2) 因此根层就绪改为在链内轮询等待（treeNodesRef 读最新值），effect 只在 highlight 变化时启动。
  const treeNodesRef = useRef<AssetTreeNode[]>([]);
  treeNodesRef.current = treeNodes;
  const highlightConsumedRef = useRef<string | null>(null);
  useEffect(() => {
    if (!highlightPath || highlightConsumedRef.current === highlightPath) return;
    highlightConsumedRef.current = highlightPath;
    void (async () => {
      for (let waited = 0; treeNodesRef.current.length === 0 && waited < 100; waited += 1) {
        await new Promise((resolve) => setTimeout(resolve, 100));
      }
      const parts = highlightPath.split("/").filter(Boolean);
      let level: AssetTreeNode[] = treeNodesRef.current;
      for (let index = 0; index < parts.length - 1; index += 1) {
        const prefix = parts.slice(0, index + 1).join("/");
        const dirNode = level.find((node) => node.path === prefix && node.kind === "dir");
        if (!dirNode) return; // 路径失效：静默降级
        if (dirNode.children) {
          level = dirNode.children;
          continue;
        }
        const children = await handleExpandDir(dirNode);
        if (!children) return; // 展开失败：静默降级
        level = children;
      }
      const fileNode = level.find((node) => node.path === highlightPath && node.kind === "file");
      if (!fileNode?.asset) return; // 失效降级
      onSelectFile(fileNode);
      requestAnimationFrame(() => {
        document
          .querySelector(`[data-path="${CSS.escape(highlightPath)}"]`)
          ?.scrollIntoView({ block: "nearest" });
      });
    })();
  }, [highlightPath, handleExpandDir, onSelectFile]);

  // 窄屏底部抽屉：选中制品或点开「使用说明」时打开；关闭即清空选中（树保持原有展开与滚动状态）。
  const narrowDrawerOpen = isNarrow && (selected !== null || usageDrawerOpen);
  const closeNarrowDrawer = () => {
    setSelected(null);
    setUsageDrawerOpen(false);
  };

  /**
   * 右侧详情内容：下载趋势 + 选中制品的完整详情（未选中时是使用说明）。
   * 桌面固定右栏卡片与窄屏底部抽屉**复用同一份内容**，避免两处实现各自漂移；
   * 滚动容器一并内置，两处外壳只需给出受约束的高度（卡片 / 抽屉内容区）即可。
   */
  const detailPanelContent = (
    <ScrollArea style={{ flex: 1, minHeight: 0 }} type="auto" offsetScrollbars>
      {/* 下载趋势：与下方详情 / 使用说明**同处一个滚动区**，作为滚动内容的开头。
          不放固定列首——钉在顶部会与下方内容争夺高度、被压得显小；放进滚动流后
          图表按自身高度完整渲染，往下滚时它自然向上滚出视野，不再占位。
          minHeight:0：flex 项默认 min-height:auto 会被内容顶高，导致 ScrollArea 撑破卡片而非内滚。 */}
      {/* 窄屏：选中制品时不再把仓库级趋势图排在前面——手机上打开详情的第一眼应该是
          路径/校验和/下载，而不是先滚过一张图；趋势图保留在「使用说明」那一次展开里。 */}
      {downloadTrend && (!isNarrow || selected === null) ? (
        <Box data-testid="repo-detail-trend" mb="sm">
          {downloadTrend.loading ? (
            <Skeleton height={160} radius="md" />
          ) : downloadTrend.points ? (
            <TrendChart
              title={t("repoDetail.downloadTrendTitle")}
              summary={t("repoDetail.downloadTrendPrimary")}
              primary={downloadTrend.points}
              primaryLabel={t("repoDetail.downloadTrendPrimary")}
              compact
            />
          ) : downloadTrend.error ? (
            <Text size="xs" c="dimmed">
              {t("repoDetail.downloadTrendUnavailable")}
            </Text>
          ) : null}
        </Box>
      ) : null}
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
  );

  return (
    <Stack gap={isNarrow ? "sm" : "md"} style={{ height: "100%", overflow: "hidden" }}>
      {/* FR-81：format/type/visibility 徽章由详情页页头统一渲染，此处不再重复一层。 */}

      {/* 上传区默认收起：点击按钮展开（Raw / Maven hosted），不挤占文件树空间 */}
      {(canUpload || canMavenUpload) && (
        <Box>
          <Button
            size="xs"
            variant={uploadOpen ? "filled" : "light"}
            leftSection={<IconUpload size={14} />}
            rightSection={uploadOpen ? <IconChevronUp size={14} /> : <IconChevronDown size={14} />}
            onClick={() => setUploadOpen((o) => !o)}
          >
            {t("repoDetail.uploadToggle", { defaultValue: "上传制品" })}
          </Button>
          <Collapse expanded={uploadOpen}>
            <Box mt="xs">
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
                          <Button
                            {...props}
                            leftSection={<IconUpload size={16} />}
                            loading={uploading}
                          >
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
                <MavenUploadCard
                  repoName={repoName}
                  onUploaded={() => setReloadNonce((n) => n + 1)}
                />
              )}
            </Box>
          </Collapse>
        </Box>
      )}

      {/* FR-74：客户端发布提示收纳为紧凑小字 + 跳使用说明链接，不占大块。
          窄屏用短文案（完整版要折两行、约 48px；短版本一行约 22px），链接照旧可点。 */}
      {allowUpload && !canUpload && !canMavenUpload && (
        <Text size="xs" c="dimmed">
          {isNarrow
            ? t("repoDetail.uploadClientOnlyShort", { defaultValue: "仅支持客户端发布" })
            : t("repoDetail.uploadClientOnly")}{" "}
          <Anchor size="xs" component="button" type="button" onClick={() => setSelected(null)}>
            {t("repoDetail.uploadClientOnlyLink")}
          </Anchor>
        </Text>
      )}

      {treeError && (
        <Alert color="red">
          <Group justify="space-between" wrap="wrap">
            <Text size="sm">{treeError}</Text>
            <Button
              size="xs"
              variant="light"
              onClick={() => setReloadNonce((value) => value + 1)}
              leftSection={<IconRefresh size={14} />}
            >
              {t("common.retry")}
            </Button>
          </Group>
        </Alert>
      )}

      {/* 首载：目录树还没到。用结构化骨架而不是居中转圈——慢接口下居中转圈会让
          右侧详情区看起来是空的，与列表页的首载口径保持一致。 */}
      {treeLoading && treeNodes.length === 0 && <ContentSkeleton rows={6} />}

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
            // 窄屏改为上下堆叠（见 isNarrow 说明），宽屏保持左右并排。
            flexDirection: isNarrow ? "column" : "row",
            gap: "var(--mantine-spacing-md)",
            // FR-74：填满页面固定高外壳的剩余空间，树/详情各自内滚。
            flex: 1,
            minHeight: 240,
          }}
        >
          {/* 左侧：文件树 + 搜索。
              窄屏详情改由底部抽屉承载（见下方 Drawer），树卡因此独占整屏剩余高度、由内层 ScrollArea 滚动——
              此前树卡被限死在 45vh，选中制品后详情只剩残余高度，手机上一句话都读不全。 */}
          <Card
            withBorder
            padding={density.cardPadding}
            radius="md"
            style={{
              // 窄屏：全宽 + 占满可用高度（内层 ScrollArea 负责滚动）；宽屏：可拖拽的固定宽侧栏。
              width: isNarrow ? "100%" : treeWidth,
              minWidth: isNarrow ? 0 : 280,
              maxWidth: isNarrow ? "100%" : 720,
              flex: isNarrow ? 1 : undefined,
              overflow: isNarrow ? "hidden" : undefined,
              flexShrink: isNarrow ? 1 : 0,
              minHeight: 0,
              display: "flex",
              flexDirection: "column",
              position: "relative",
            }}
          >
            <Box
              style={{
                display: "flex",
                flexDirection: "column",
                flex: 1,
                minHeight: 0,
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
                placeholder={t("repoDetail.searchPlaceholder", {
                  defaultValue: "搜索制品，支持 -排除词 ext:jar 等表达式",
                })}
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
                <Group justify="space-between" align="center" gap="xs" mb="xs" wrap="nowrap">
                  <Text size="xs" c="dimmed" truncate>
                    {searchLoaded.length < searchTotal
                      ? t("repoDetail.searchPartial", {
                          shown: searchLoaded.length,
                          total: searchTotal,
                          defaultValue: `已显示 ${searchLoaded.length} / 共 ${searchTotal} 条结果`,
                        })
                      : t("repoDetail.searchResultCount", {
                          count: searchLoaded.length,
                          defaultValue: `找到 ${searchLoaded.length} 条结果`,
                        })}
                  </Text>
                  {searchLoaded.length < searchTotal ? (
                    <Button
                      size="compact-xs"
                      variant="light"
                      loading={searching}
                      onClick={handleLoadMoreSearch}
                    >
                      {t("repoDetail.searchLoadMore", { defaultValue: "加载更多" })}
                    </Button>
                  ) : null}
                </Group>
              )}
              {treeActionError && (
                <Alert color="red" py="xs" mb="xs">
                  {treeActionError}
                </Alert>
              )}
              {/* 树卡高度受约束（宽屏由树宽/窄屏由 flex: 1 决定），超出部分必须靠内层滚动，
                所以这里必须 minHeight: 0，否则 ScrollArea 会被内容顶高、撑破卡片而不是内滚。 */}
              <ScrollArea style={{ flex: 1, minHeight: 0 }} type="auto" offsetScrollbars>
                <RepoAssetTree
                  key={searchResults === null ? "browse" : `search:${searchQuery}`}
                  nodes={displayNodes}
                  selectedPath={selected?.path ?? null}
                  onSelectFile={onSelectFile}
                  onSelectDir={onSelectDir}
                  onExpandDir={searchResults === null ? handleExpandDir : undefined}
                  maxHeight="none"
                  defaultExpanded={searchResults !== null}
                  highlightPath={highlightPath}
                  showSize={searchResults !== null}
                  selectable={isAdmin}
                  selectedPaths={selectedPaths}
                  onNodeInteraction={isAdmin ? onNodeInteraction : undefined}
                  onContextMenu={isAdmin ? handleContextMenu : undefined}
                />
              </ScrollArea>
            </Box>
          </Card>

          {/* FR-99: 拖拽分割条——左右面板宽度自由调整（窄屏上下堆叠时无意义，不渲染） */}
          {isNarrow ? null : (
            <Box
              onMouseDown={onSplitterMouseDown}
              aria-label="调整文件树宽度"
              role="separator"
              aria-orientation="vertical"
              style={{
                width: 8,
                marginInline: -4,
                cursor: "col-resize",
                alignSelf: "stretch",
                flexShrink: 0,
                borderRadius: 4,
              }}
            />
          )}

          {/* 窄屏：未选中文件时给一行紧凑入口，保证「使用说明」不随详情卡退场而不可达。 */}
          {isNarrow && !selected && (
            <Button
              size="xs"
              variant="light"
              data-testid="repo-usage-entry"
              style={{ alignSelf: "flex-start" }}
              onClick={() => setUsageDrawerOpen(true)}
            >
              {t("repoDetail.usageTitle")}
            </Button>
          )}

          {/* 右侧：文件详情 / 使用说明。
              窄屏不渲染（也不占布局空间）：详情与使用说明改由下方底部抽屉承载，
              两者共用 detailPanelContent 这一份内容，避免两处实现漂移。 */}
          {isNarrow ? null : (
            <Card
              withBorder
              padding={density.cardPadding}
              radius="md"
              data-testid="repo-detail-panel"
              style={{
                flex: 1,
                minHeight: 0,
                display: "flex",
                flexDirection: "column",
                overflow: "hidden",
              }}
            >
              {detailPanelContent}
            </Card>
          )}
        </Box>
      )}

      {contextMenu && (
        <Box
          role="menu"
          onMouseLeave={() => setContextMenu(null)}
          style={{
            position: "fixed",
            left: contextMenu.x,
            top: contextMenu.y,
            zIndex: 1000,
            display: "flex",
            flexDirection: "column",
            gap: 2,
            padding: 6,
            minWidth: 132,
            background: "var(--mantine-color-body)",
            border: "1px solid var(--mantine-color-default-border)",
            borderRadius: 6,
            boxShadow: "var(--mantine-shadow-md)",
          }}
        >
          <Button
            role="menuitem"
            variant="subtle"
            size="xs"
            justify="flex-start"
            onClick={() => {
              setContextMenu(null);
              handleBatchDelete();
            }}
            leftSection={<IconTrash size={14} />}
          >
            {t("common.delete")}
          </Button>
          {repoType === "hosted" && operationSupportsPathMutation(format) && (
            <>
              <Button
                role="menuitem"
                variant="subtle"
                size="xs"
                justify="flex-start"
                onClick={() => openPathOperation("move")}
              >
                {t("repoDetail.assetMove")}
              </Button>
              <Button
                role="menuitem"
                variant="subtle"
                size="xs"
                justify="flex-start"
                disabled={selectedPaths.size !== 1}
                onClick={() => openPathOperation("rename")}
              >
                {t("repoDetail.assetRename")}
              </Button>
            </>
          )}
        </Box>
      )}

      <Modal
        opened={pathOperation !== null}
        onClose={() => setPathOperation(null)}
        title={pathOperation === "move" ? t("repoDetail.assetMove") : t("repoDetail.assetRename")}
      >
        <Stack gap="sm">
          <TextInput
            label={
              pathOperation === "move"
                ? t("repoDetail.assetDestinationPath")
                : t("repoDetail.assetNewPath")
            }
            value={pathInput}
            onChange={(event) => setPathInput(event.currentTarget.value)}
            disabled={operationBusy}
          />
          <Group justify="flex-end">
            <Button variant="default" onClick={() => setPathOperation(null)}>
              {t("common.cancel")}
            </Button>
            <Button loading={operationBusy} onClick={submitPathOperation}>
              {pathOperation === "move" ? t("repoDetail.assetMove") : t("repoDetail.assetRename")}
            </Button>
          </Group>
        </Stack>
      </Modal>

      {/* 窄屏：制品详情 / 使用说明改由底部抽屉承载（约 85dvh，内容区自身滚动、关闭按钮常驻）。
          选中文件即自动弹出，关闭则清空选中并回到文件树（树的展开与滚动状态不变）。 */}
      <Drawer
        opened={narrowDrawerOpen}
        onClose={closeNarrowDrawer}
        position="bottom"
        size="85dvh"
        closeButtonProps={{ "aria-label": t("common.close") }}
        styles={{
          // 抽屉内容区固定为 85dvh 高、内部纵向弹性布局，滚动交给共用的 ScrollArea，
          // 头部（含关闭按钮）保持固定；不这样做则滚动条会落到含头部的整块内容上。
          content: {
            display: "flex",
            flexDirection: "column",
            overflow: "hidden",
          },
          body: {
            flex: 1,
            minHeight: 0,
            display: "flex",
            flexDirection: "column",
            overflow: "hidden",
          },
        }}
      >
        {detailPanelContent}
      </Drawer>
    </Stack>
  );
}

/**
 * 使用说明分组顺序（固定）：认证 → 解析依赖 → 发布制品 → 其他。
 * 顺序即契约 `UsageSnippet.group` 的展示序；空组不渲染。
 */
const USAGE_GROUPS = ["auth", "resolve", "publish", "other"] as const;
type UsageGroup = (typeof USAGE_GROUPS)[number];

/**
 * 分组标签的 i18n 键（与 USAGE_GROUPS 一一对应）。
 */
const USAGE_GROUP_LABEL_KEYS: Record<UsageGroup, string> = {
  auth: "repoDetail.usageGroupAuth",
  resolve: "repoDetail.usageGroupResolve",
  publish: "repoDetail.usageGroupPublish",
  other: "repoDetail.usageGroupOther",
};

/**
 * 工具标识 → 显示名 i18n 键。工具标识是契约里的**结构化标记**（不参与本地化），
 * 界面按键取本地化显示名；未收录的标识直接原样显示（防御异常数据）。
 */
const USAGE_TOOL_LABEL_KEYS: Record<string, string> = {
  maven: "repoDetail.usageToolMaven",
  gradle: "repoDetail.usageToolGradle",
  "gradle-kts": "repoDetail.usageToolGradleKts",
  sbt: "repoDetail.usageToolSbt",
  ivy: "repoDetail.usageToolIvy",
  ant: "repoDetail.usageToolAnt",
  npm: "repoDetail.usageToolNpm",
  pnpm: "repoDetail.usageToolPnpm",
  yarn: "repoDetail.usageToolYarn",
  bun: "repoDetail.usageToolBun",
  curl: "repoDetail.usageToolCurl",
  wget: "repoDetail.usageToolWget",
};

/**
 * 归类单个片段：缺失 `group` 或值不在预期集合内（老 mock / 异常数据）一律归入 `other`，
 * 只折叠不错放——直接丢弃会让用户看不到既有片段。
 */
function usageGroupOf(snippet: UsageSnippet): UsageGroup {
  const group = (snippet as { group?: string }).group;
  return USAGE_GROUPS.includes(group as UsageGroup) ? (group as UsageGroup) : "other";
}

/**
 * 读取片段的工具标识；缺失或非字符串（老 mock / 异常数据）回落为空串，
 * 只做分组内切换、绝不丢弃片段——异常数据仍会显示在「未知工具」这一档下。
 */
function usageToolOf(snippet: UsageSnippet): string {
  const tool = (snippet as { tool?: string }).tool;
  return typeof tool === "string" ? tool : "";
}

/**
 * 使用说明面板：右侧无选中文件时展示。
 *
 * 顶部**一个工具切换器**统一筛选（Maven / Gradle / Gradle Kotlin DSL / sbt / Ivy / Ant /
 * npm / pnpm / Yarn / bun / curl / wget 等）：几种工具通常是针对同一个仓库并行使用的（不同项目
 * 各自选型），不该让用户在每个分组里各选一次；切换一次，下方各分组的内容同时跟着变。
 *
 * 「认证 / 解析依赖 / 发布制品」三组**全部展开**一次看全——它们本就是配置一个仓库时要一起做的事，
 * 折叠反而多一次点击。当前工具在某组没有片段时该组隐藏（避免只剩空标题），故切换工具后分组数会变，
 * 这是预期行为（例如选 sbt 时只剩「解析依赖」一组）。
 * 每段一张小卡片，代码块右上角提供复制。
 */
function UsagePanel({ usageState }: { usageState: ReturnType<typeof useAsync<UsageInfo>> }) {
  const { t } = useTranslation();
  // 全局选中的工具；数据变化导致选择失效时回落第一个可用工具，避免受控空值导致内容空白。
  const [activeTool, setActiveTool] = useState("");
  // 工具显示名：已知工具走 i18n，未知工具直接显示原始标识（不丢弃片段）。
  const toolLabel = (tool: string) => {
    const key = USAGE_TOOL_LABEL_KEYS[tool];
    return key ? t(key, { defaultValue: tool }) : tool;
  };
  return (
    <AsyncBoundary state={usageState}>
      {(usage: UsageInfo) => {
        const grouped = USAGE_GROUPS.map((group) => ({
          group,
          snippets: usage.snippets.filter((snippet) => usageGroupOf(snippet) === group),
        })).filter((entry) => entry.snippets.length > 0);
        // 全部工具（跨分组的并集，按首次出现顺序去重）——即切换器的选项；选项只显示工具名。
        const tools: string[] = [];
        for (const snippet of usage.snippets) {
          const tool = usageToolOf(snippet);
          if (!tools.includes(tool)) {
            tools.push(tool);
          }
        }
        const active = tools.includes(activeTool) ? activeTool : (tools[0] ?? "");
        // 按当前工具过滤各分组；空组整组隐藏（切换工具后分组数会变，属预期）。
        const sections = grouped
          .map((entry) => ({
            group: entry.group,
            snippets: entry.snippets.filter((snippet) => usageToolOf(snippet) === active),
          }))
          .filter((entry) => entry.snippets.length > 0);
        return (
          <Stack gap="sm" data-testid="repo-usage-panel">
            <Title order={5}>{t("repoDetail.usageTitle")}</Title>
            {grouped.length === 0 ? (
              <Text size="xs" c="dimmed">
                {t("repoDetail.usageEmpty", { defaultValue: "该仓库暂无使用说明" })}
              </Text>
            ) : (
              <>
                {tools.length > 1 && (
                  <Select
                    size="xs"
                    data-testid="repo-usage-tool-select"
                    aria-label={t("repoDetail.usageToolSelectLabel", {
                      defaultValue: "选择使用方式",
                    })}
                    data={tools.map((tool) => ({
                      value: tool,
                      label: toolLabel(tool),
                    }))}
                    value={active}
                    onChange={(value) => {
                      if (value !== null) {
                        setActiveTool(value);
                      }
                    }}
                    allowDeselect={false}
                    comboboxProps={{ withinPortal: true }}
                  />
                )}
                {sections.map((entry) => (
                  <Stack key={entry.group} gap="xs" data-testid={`repo-usage-group-${entry.group}`}>
                    <Divider label={t(USAGE_GROUP_LABEL_KEYS[entry.group])} labelPosition="left" />
                    {entry.snippets.map((snippet, index) => (
                      <Card key={index} withBorder padding="sm" radius="md">
                        <Group
                          justify="space-between"
                          align="flex-start"
                          gap="xs"
                          mb={4}
                          wrap="nowrap"
                        >
                          <Text fw={600} size="sm">
                            {snippet.title}
                          </Text>
                          <CopyTextButton value={snippet.code} />
                        </Group>
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
                ))}
              </>
            )}
          </Stack>
        );
      }}
    </AsyncBoundary>
  );
}
