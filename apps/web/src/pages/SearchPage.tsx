// 全局搜索结果页：/search?q=keyword
// 支持高级表达式（-排除词 / repo: / format: / ext: / 引号短语），配筛选面板双向同步；
// 结果以的包级聚合（group_by=package&order=relevance）为主数据源，
// 渲染成「左栏包/版本/文件折叠树 + 右栏文件详情」的主从双栏。
// 窄屏（<820px）降维为单栏两级视图：列表 ⇄ 详情，浏览器返回键可回列表。
import {
  Box,
  Button,
  Checkbox,
  Chip,
  Code,
  Collapse,
  Group,
  Pagination,
  Paper,
  Popover,
  Select,
  SimpleGrid,
  Stack,
  Text,
  TextInput,
} from "@mantine/core";
import { useLocalStorage, useMediaQuery } from "@mantine/hooks";
import {
  IconAlertTriangle,
  IconChevronDown,
  IconChevronUp,
  IconFilter,
  IconHelp,
  IconSearch,
} from "@tabler/icons-react";
import { ErrorState, ForbiddenState } from "@jianartifact/ui";
import { useCallback, useEffect, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { useNavigate, useSearchParams } from "react-router-dom";

import { PageShell } from "../app/PageShell";
import { ContentSkeleton } from "../components/AsyncBoundary";
import { PackageSearchTree } from "../components/search/PackageSearchTree";
import { SearchDetailPanel } from "../components/search/SearchDetailPanel";
import { SearchProgress } from "../components/search/SearchProgress";
import { SearchSuggestInput } from "../components/search/SearchSuggestInput";
import { searchAssets, type SearchPackage, type SearchPackageFile } from "../api/endpoints";
import { ApiError } from "../api/client";
import { useAsync } from "../hooks/useAsync";
import {
  defaultOrderFor,
  firstFileOfPackage,
  firstFileOfVersion,
  initialViewState,
  packageKey,
  resolveSelection,
  sortPackages,
  toggleInSet,
  versionKey,
  type SearchSortKey,
  type SearchSortOrder,
  type SearchViewState,
} from "../lib/searchPackage";
import {
  buildSearchExpression,
  CHECKSUM_EXTS,
  parseSearchExpression,
  splitCsv,
  type SearchExpression,
} from "../lib/searchQuery";
import { currentLocaleTag } from "../i18n/current";

const PAGE_SIZE = 50;
const FORMAT_OPTIONS = ["maven", "raw", "npm"];

/** 本页唯一的窄屏断点：820px（规格 §3.10 要求同页不并存两套断点）。 */
const NARROW_QUERY = "(max-width: 51.25em)";

type SortKey = SearchSortKey;

type SortOrder = SearchSortOrder;

/**
 * 把后端 / 客户端两处超时统一判出来：
 * - 后端搜索预算耗尽返回 HTTP 504 + `search_timeout`；
 * - 客户端 15s 兜底（api/client）抛 `code=timeout`、status 0。
 * 两者都该给「超时」明确文案，而不是泛化的「加载失败」——后者会让人以为可以无脑重试。
 */
function isTimeoutError(error: ApiError): boolean {
  return error.status === 504 || error.code === "search_timeout" || error.code === "timeout";
}

interface SortPreference {
  by: SortKey;
  order: SortOrder;
}

/** 排序偏好持久化键：沿用全站 `jianartifact.*` 前缀（见 i18n/language.ts、usePinnedRepos.ts）。 */
export const SEARCH_SORT_KEY = "jianartifact.search.sort";

/** 默认排序：相关度（服务端聚合序）。 */
const DEFAULT_SORT: SortPreference = { by: "relevance", order: "relevance" };

const SORT_KEYS: readonly SortKey[] = [
  "relevance",
  "name",
  "repo",
  "path",
  "size",
  "updated",
];

function isSortKey(value: unknown): value is SortKey {
  return typeof value === "string" && (SORT_KEYS as readonly string[]).includes(value);
}

/**
 * 校验持久化排序值：localStorage 可能残留脏值 / 旧版本值（结构不符或键越界），
 * 一律回退到默认排序，避免非法排序语义在前端重排出莫名其妙的结果。
 *
 * `order:"relevance"` 只与 `by:"relevance"` 搭配时才合法（规格 §3.8）：
 * 旧值（如 `{by:"updated",order:"desc"}`）继续有效，不会被一次性清空。
 */
function sanitizeSort(value: unknown): SortPreference {
  if (value !== null && typeof value === "object") {
    const candidate = value as { by?: unknown; order?: unknown };
    if (isSortKey(candidate.by)) {
      if (candidate.by === "relevance") {
        // 相关度只有一个方向；order 是脏值也归一到默认，不给前端重排留歧义。
        return DEFAULT_SORT;
      }
      if (candidate.order === "asc" || candidate.order === "desc") {
        return { by: candidate.by, order: candidate.order };
      }
    }
  }
  return DEFAULT_SORT;
}

// 语法帮助条目：[表达式示例, 文案 key, 兜底文案]
const SYNTAX_ROWS: [string, string, string][] = [
  ["spring core", "search.helpTerms", "同时包含多个关键词"],
  ["-sources", "search.helpExclude", "排除包含该词的路径"],
  ['"my lib"', "search.helpPhrase", "引号内空格不切分"],
  ["repo:maven-releases", "search.helpRepo", "限定仓库（-repo: 排除）"],
  ["format:maven", "search.helpFormat", "限定仓库格式 raw/maven/npm"],
  ["ext:jar", "search.helpExt", "按扩展名筛选（-ext:sha1 排除）"],
];

export function SearchPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const [searchParams] = useSearchParams();
  const q = searchParams.get("q") ?? "";
  const [page, setPage] = useState(1);
  const [input, setInput] = useState(q);
  const [filtersOpen, setFiltersOpen] = useState(false);
  const isNarrow = useMediaQuery(NARROW_QUERY) ?? false;
  // 排序偏好跨会话记忆：默认相关度，脏值回退默认。
  const [sortPref, setSortPref] = useLocalStorage<SortPreference>({
    key: SEARCH_SORT_KEY,
    defaultValue: DEFAULT_SORT,
    getInitialValueInEffect: false,
  });
  const { by: sortBy, order: sortOrder } = sanitizeSort(sortPref);
  // 排序变化回到第一页：文件级分页与包级聚合无关，但口径变了就不该停在旧页。
  const changeSort = (value: string | null) => {
    const next = isSortKey(value) ? value : "relevance";
    setSortPref({ by: next, order: defaultOrderFor(next) });
    setPage(1);
  };

  // URL 的 q 变化（如 Header 搜索栏跳转）时同步输入框并回到第一页
  useEffect(() => {
    setInput(q);
    setPage(1);
  }, [q]);

  // 取数口径固定为「包级聚合 + 相关度」：其余排序键全部在前端重排（规格 §3.9），
  // 因此切换排序不触发请求，也就不用进依赖数组。
  //
  // abortable：把 signal 透传到 searchAssets → api/client → fetch。新一次搜索发起前
  // （q / page 变化或点「取消搜索」）都会真的中止上一次请求，而不是只靠 active 标志丢弃响应。
  // 这里选择给 useAsync 加一个可选开关，而不是让本页自管请求状态：缓存回放、keepPreviousData、
  // 刷新失败非阻断、reload 重试这些语义本页都在用，自管等于复制一份并迟早与全局口径漂移。
  const state = useAsync(
    (signal: AbortSignal | undefined) =>
      q
        ? searchAssets({
            q,
            group_by: "package",
            order: "relevance",
            page,
            page_size: PAGE_SIZE,
            signal,
          })
        : Promise.resolve({ items: [], total: 0, facets: [] }),
    [q, page],
    // keepPreviousData：换排序 / 翻页时保留旧结果、仅后台刷新，避免整块内容被骨架替换。
    { cacheKey: `search:package:${q}:${page}`, keepPreviousData: true, abortable: true },
  );

  // 「搜索进行中」：首载（骨架屏）与换排序 / 翻页的后台刷新都算，二者共用同一条进度条。
  const searching = state.loading || state.refreshing;

  const totalPages = Math.ceil((state.data?.total ?? 0) / PAGE_SIZE);

  // 筛选面板与输入框双向同步：面板改动即重拼表达式写回输入框
  const expr = useMemo(() => parseSearchExpression(input), [input]);
  const updateExpr = (patch: Partial<SearchExpression>) => {
    setInput(buildSearchExpression({ ...expr, ...patch }));
  };
  const checksumsHidden = CHECKSUM_EXTS.every((x) => expr.notExts.includes(x));
  const toggleChecksums = (hide: boolean) => {
    const rest = expr.notExts.filter((x) => !CHECKSUM_EXTS.includes(x));
    updateExpr({ notExts: hide ? [...rest, ...CHECKSUM_EXTS] : rest });
  };

  // 确认一次搜索。override 供 suggest 的「包名候选」直接带着候选值发起搜索使用——
  // 那条路径上 setInput 与提交发生在同一帧，读 input 状态会读到旧值。
  const handleSubmit = (override?: string) => {
    const next = (override ?? input).trim();
    if (!next) {
      return;
    }
    if (next === q) {
      // 同口径再次提交（典型场景：取消搜索后立刻重搜、或失败后原地重试）：URL 不变就不会
      // 触发路由变化与依赖变化，必须显式重新拉取，否则点了按钮毫无反应。
      state.reload();
      return;
    }
    navigate(`/search?q=${encodeURIComponent(next)}`);
    setPage(1);
  };

  // 仓库名候选的确认语义（规格 §3.9）：复用既有的表达式拼装写回输入框，**不立即搜索**——
  // 用户可以继续追加关键词，与筛选面板 ↔ 输入框的双向同步是同一套机制，不新造状态。
  const pickRepository = (name: string) => {
    setInput(buildSearchExpression({ ...expr, repos: [name] }));
  };

  // 仓库聚合钻取：点击 chip 直接以新表达式发起搜索（单选，再点「全部」还原）
  const activeQ = useMemo(() => parseSearchExpression(q), [q]);
  const activeRepo = activeQ.repos.length === 1 ? activeQ.repos[0] : null;
  const drillRepo = (repo: string | null) => {
    const next = buildSearchExpression({ ...activeQ, repos: repo ? [repo] : [] });
    if (next.trim()) {
      navigate(`/search?q=${encodeURIComponent(next)}`);
    }
  };

  const facets = state.data?.facets ?? [];
  const facetTotal = facets.reduce((sum, f) => sum + f.count, 0);

  const openRepo = useCallback(
    (repository: string, highlightPath?: string) => {
      // 携带命中位置进入仓库详情，避免进仓库后再搜一遍。
      const query = highlightPath ? `?highlight=${encodeURIComponent(highlightPath)}` : "";
      navigate(`/repositories/${encodeURIComponent(repository)}${query}`);
    },
    [navigate],
  );

  // 高亮关键词：只用正向关键词（排除词不该高亮），且不区分大小写匹配。
  const keywords = useMemo(() => activeQ.terms, [activeQ]);

  // 包数组必须稳定引用：`state.data?.packages ?? []` 会在数据未就绪时每次渲染都造一个新空数组，
  // 让下游 useMemo（重排）与默认展开 effect 的依赖永远"在变"，进而 setState → 重渲染 → 再变，
  // 形成无限渲染循环。这里按数据对象本身缓存兜底空数组。
  const packages = useMemo(() => state.data?.packages ?? [], [state.data]);
  const sortedPackages = useMemo(
    () => sortPackages(packages, sortBy, sortOrder),
    [packages, sortBy, sortOrder],
  );

  // —— 主从联动状态 ——
  const [view, setView] = useState<SearchViewState>(() => initialViewState([]));
  const [detailOpen, setDetailOpen] = useState(false);

  // 数据 / 排序变化后重新套用默认展开与选中（规格 §3.9）；
  // 同一次数据内的手动展开与选中不会被这个 effect 覆盖（它只随 sortedPackages 变化触发）。
  useEffect(() => {
    setView(initialViewState(sortedPackages));
  }, [sortedPackages]);

  // 跨过断点回宽屏时收起单栏详情：双栏下右栏恒可见，详情态没有意义。
  useEffect(() => {
    if (!isNarrow) {
      setDetailOpen(false);
    }
  }, [isNarrow]);

  // 浏览器返回键回列表：进入详情时压入一条带标记的历史项，popstate 时按标记判定。
  useEffect(() => {
    if (!isNarrow) {
      return;
    }
    const onPop = () => {
      if ((window.history.state as { searchDetail?: boolean } | null)?.searchDetail !== true) {
        setDetailOpen(false);
      }
    };
    window.addEventListener("popstate", onPop);
    return () => window.removeEventListener("popstate", onPop);
  }, [isNarrow]);

  const openDetail = () => {
    // 已进入详情时不重复压栈：详情内点「同版本文件」是就地切换，不该产生额外历史项。
    if (!isNarrow || detailOpen) {
      return;
    }
    setDetailOpen(true);
    // 与路由无关的一条同 URL 历史项：只为让浏览器返回键有"上一屏"可回。
    window.history.pushState({ ...(window.history.state ?? {}), searchDetail: true }, "");
  };
  const closeDetail = () => {
    setDetailOpen(false);
    if ((window.history.state as { searchDetail?: boolean } | null)?.searchDetail === true) {
      window.history.back();
    }
  };

  const selection = view.selection;
  const resolved = useMemo(
    () => resolveSelection(sortedPackages, selection),
    [sortedPackages, selection],
  );

  const selectFile = (pkg: SearchPackage, version: string | null, file: SearchPackageFile) => {
    setView((current) => ({
      ...current,
      selection: {
        packageKey: packageKey(pkg),
        version,
        repository: file.repository,
        path: file.path,
      },
    }));
    openDetail();
  };

  const selectVersion = (pkg: SearchPackage, version: string) => {
    const pkgKey = packageKey(pkg);
    const file = firstFileOfVersion(pkg, version);
    setView((current) => ({
      ...current,
      // 选中版本时顺带展开该版本（否则"选中了但看不到文件"），且不折叠其它节点。
      expandedVersions: new Set([...current.expandedVersions, versionKey(pkgKey, version)]),
      selection: file
        ? { packageKey: pkgKey, version, repository: file.repository, path: file.path }
        : current.selection,
    }));
    openDetail();
  };

  const selectFirstInPackage = (pkg: SearchPackage) => {
    const pkgKey = packageKey(pkg);
    const firstVersion = pkg.versions[0]?.version ?? null;
    const file = firstFileOfPackage(pkg);
    setView((current) => ({
      ...current,
      selection: file
        ? {
            packageKey: pkgKey,
            version: firstVersion,
            repository: file.repository,
            path: file.path,
          }
        : current.selection,
    }));
  };

  const togglePackage = (pkg: SearchPackage) => {
    const pkgKey = packageKey(pkg);
    const willExpand = !view.expandedPackages.has(pkgKey);
    setView((current) => ({
      ...current,
      expandedPackages: toggleInSet(current.expandedPackages, pkgKey),
    }));
    // 展开一个包时把选中项带进它：否则右栏还停在别的包上，主从不联动。
    if (willExpand && selection?.packageKey !== pkgKey) {
      selectFirstInPackage(pkg);
    }
  };

  const toggleVersion = (pkg: SearchPackage, version: string) => {
    const key = versionKey(packageKey(pkg), version);
    setView((current) => ({
      ...current,
      expandedVersions: toggleInSet(current.expandedVersions, key),
    }));
  };

  const toggleVersionChecksums = (pkg: SearchPackage, version: string) => {
    const key = versionKey(packageKey(pkg), version);
    setView((current) => ({
      ...current,
      expandedChecksums: toggleInSet(current.expandedChecksums, key),
    }));
  };

  const hasResult = packages.length > 0;
  const truncated = state.data?.aggregation?.truncated ?? false;
  const scanned = state.data?.aggregation?.scanned ?? 0;
  const packageTotal = state.data?.packageTotal ?? 0;
  const totalFiles = state.data?.total ?? 0;

  // 结果区（四态只替换这一块，搜索框与 facet 条始终在）：
  // 无权 / 失败 / 首载骨架 / 空结果 / 双栏。
  const resultArea = (() => {
    if (state.forbidden) {
      return <ForbiddenState message={t("common.forbidden")} />;
    }
    if (state.error) {
      // 超时与其它失败分开给文案：504 / search_timeout / 客户端 timeout 三类都落到
      // 「搜索超时」，其余仍走通用失败提示，避免把限流、权限、网络问题一律说成超时。
      const timedOut = isTimeoutError(state.error);
      return (
        <ErrorState
          message={timedOut ? t("search.timeoutTitle") : t("common.error")}
          description={timedOut ? t("search.timeoutDescription") : state.error.message}
          onRetry={state.reload}
          retryLabel={t("common.retry")}
        />
      );
    }
    if (state.data === null) {
      // 用户取消（不是失败）：静默回到空闲态，既不显示骨架也不报错，提示可重新发起。
      if (state.cancelled) {
        return (
          <Stack align="center" gap={4} py="xl" data-testid="search-cancelled">
            <Text c="dimmed">{t("search.cancelledHint")}</Text>
          </Stack>
        );
      }
      // 双栏各自骨架：左栏 6 行、右栏 3 块（规格 §3.11）。
      return (
        <Box style={{ display: "flex", gap: 16, flex: 1, minHeight: 0 }}>
          <Box style={leftColumnStyle(isNarrow, false)}>
            <ContentSkeleton rows={6} />
          </Box>
          {!isNarrow && (
            <Box style={{ flex: 1, minWidth: 0, minHeight: 0, overflow: "auto" }}>
              <ContentSkeleton rows={3} />
            </Box>
          )}
        </Box>
      );
    }
    if (!hasResult) {
      return (
        <Stack align="center" gap={4} py="xl" data-testid="search-empty">
          <Text c="dimmed">{t("search.noResults")}</Text>
          <Text c="dimmed" size="xs">
            {t("search.emptyHint")}
          </Text>
        </Stack>
      );
    }
    return (
      <Box style={{ display: "flex", gap: 16, flex: 1, minHeight: 0 }}>
        {/* 左栏：包级折叠树。窄屏进详情时只隐藏不卸载——展开态与滚动位置得以保留。 */}
        <Box style={leftColumnStyle(isNarrow, isNarrow && detailOpen)}>
          {/* 工具条吸顶：计数 + 排序。滚动容器内的 sticky，树从它下面滚过。 */}
          <Box
            style={{
              position: "sticky",
              top: 0,
              zIndex: 2,
              background: "var(--mantine-color-body)",
              borderBottom: "1px solid var(--mantine-color-default-border)",
              paddingBottom: 6,
            }}
          >
            <Group justify="space-between" gap="xs" wrap="nowrap" align="flex-end">
              <Stack gap={2} style={{ minWidth: 0 }}>
                <Text size="sm" c="dimmed">
                  {t("search.resultSummary", {
                    files: totalFiles.toLocaleString(currentLocaleTag()),
                    packages: `${truncated ? "≥" : ""}${packageTotal.toLocaleString(currentLocaleTag())}`,
                  })}
                </Text>
                {truncated && (
                  <Group gap={4} wrap="nowrap">
                    <IconAlertTriangle size={12} color="var(--mantine-color-yellow-7)" />
                    <Text size="xs" c="yellow.8">
                      {t("search.aggregationTruncated", { scanned })}
                    </Text>
                  </Group>
                )}
              </Stack>
              <Select
                size="xs"
                w={110}
                aria-label={t("search.sortLabel")}
                value={sortBy}
                onChange={changeSort}
                allowDeselect={false}
                data={[
                  { value: "relevance", label: t("search.sortRelevance") },
                  { value: "name", label: t("search.sortName") },
                  { value: "repo", label: t("search.sortRepo") },
                  { value: "path", label: t("search.sortPath") },
                  { value: "size", label: t("search.sortSize") },
                  { value: "updated", label: t("search.sortUpdated") },
                ]}
              />
            </Group>
          </Box>
          <PackageSearchTree
            packages={sortedPackages}
            keywords={keywords}
            expandedPackages={view.expandedPackages}
            expandedVersions={view.expandedVersions}
            expandedChecksums={view.expandedChecksums}
            selection={selection}
            onTogglePackage={togglePackage}
            onToggleVersion={toggleVersion}
            onToggleChecksums={toggleVersionChecksums}
            onSelectVersion={selectVersion}
            onSelectFile={selectFile}
          />
          {totalPages > 1 && (
            <Group justify="center" mt="sm">
              <Pagination value={page} onChange={setPage} total={totalPages} size="sm" />
            </Group>
          )}
        </Box>

        {/* 右栏：选中文件详情。窄屏未进详情时隐藏但不卸载。
            窄屏进详情、以及"选中项已失效"这两种情况都要给出可读的落点。 */}
        <Box style={rightColumnStyle(isNarrow, detailOpen)}>
          {resolved ? (
            <SearchDetailPanel
              pkg={resolved.pkg}
              version={resolved.version}
              file={resolved.file}
              siblings={resolved.siblings}
              onBack={isNarrow ? closeDetail : undefined}
              onOpenRepository={openRepo}
              onSelectFile={(file) =>
                selectFile(resolved.pkg, resolved.version?.version ?? null, file)
              }
            />
          ) : (
            <Stack align="center" gap={4} py="xl">
              <Text c="dimmed" size="sm">
                {t("search.selectFileHint")}
              </Text>
            </Stack>
          )}
        </Box>
      </Box>
    );
  })();

  return (
    // 列表页统一范式：整页固定在视口内，结果区在容器内滚动（纵向）+ 工具条吸顶。
    <PageShell>
      <Box style={{ flex: 1, minHeight: 0, display: "flex", flexDirection: "column", gap: 12 }}>
        {/* wrap：窄屏把按钮换到第二行，否则输入框会被挤到只剩几十像素宽没法输入。
            输入框给 minWidth 保底，放不下时让按钮先换行而不是压缩输入框。
            输入框是带候选下拉的自管组件（防抖 + 中止上一次 + 键盘 ↑↓/Enter/Esc）。
            回车与「包名候选」确认都走 handleSubmit；仓库名候选只改写表达式、不触发搜索。 */}
        <Group gap="xs" wrap="wrap" align="flex-start">
          <SearchSuggestInput
            placeholder={t("search.placeholder")}
            value={input}
            onChange={setInput}
            onSearch={handleSubmit}
            onPickRepository={pickRepository}
            style={{ flex: 1, minWidth: isNarrow ? 160 : 200, maxWidth: 560 }}
          />
          {/* 语法帮助从输入框右侧的裸图标移出为「图标 + 文字」按钮：右槽里塞不下文字，
              而裸图标（问号）看不出点开是语法说明还是别的帮助。 */}
          <Popover width={340} position="bottom-end" shadow="md">
            <Popover.Target>
              <Button
                variant="light"
                leftSection={<IconHelp size={14} />}
                aria-label={t("search.syntaxHelp")}
              >
                {t("search.syntaxHelp")}
              </Button>
            </Popover.Target>
            <Popover.Dropdown>
              <Stack gap={6}>
                <Text size="sm" fw={600}>
                  {t("search.syntaxHelp")}
                </Text>
                {SYNTAX_ROWS.map(([example, key, fallback]) => (
                  <Group key={example} gap={8} wrap="nowrap" align="flex-start">
                    <Code style={{ flexShrink: 0 }}>{example}</Code>
                    <Text size="xs" c="dimmed">
                      {t(key, { defaultValue: fallback })}
                    </Text>
                  </Group>
                ))}
              </Stack>
            </Popover.Dropdown>
          </Popover>
          <Button
            variant={filtersOpen ? "filled" : "light"}
            leftSection={<IconFilter size={14} />}
            rightSection={
              filtersOpen ? <IconChevronUp size={14} /> : <IconChevronDown size={14} />
            }
            onClick={() => setFiltersOpen((o) => !o)}
          >
            {t("search.filters")}
          </Button>
          <Button onClick={() => handleSubmit()} leftSection={<IconSearch size={16} />}>
            {t("common.search")}
          </Button>
        </Group>

        <Collapse expanded={filtersOpen}>
          <Paper withBorder radius="md" p="sm">
            <SimpleGrid cols={{ base: 1, sm: 2, lg: 3 }} spacing="sm">
              <Select
                label={t("search.filterFormat")}
                placeholder={t("search.filterFormatAll")}
                data={FORMAT_OPTIONS}
                value={expr.formats[0] ?? null}
                onChange={(v) => updateExpr({ formats: v ? [v] : [] })}
                clearable
                size="xs"
              />
              <TextInput
                label={t("search.filterRepo")}
                placeholder={t("search.filterCsvHint")}
                value={expr.repos.join(", ")}
                onChange={(e) => updateExpr({ repos: splitCsv(e.currentTarget.value) })}
                size="xs"
              />
              <TextInput
                label={t("search.filterNotRepo")}
                placeholder={t("search.filterCsvHint")}
                value={expr.notRepos.join(", ")}
                onChange={(e) => updateExpr({ notRepos: splitCsv(e.currentTarget.value) })}
                size="xs"
              />
              <TextInput
                label={t("search.filterExcludeTerms")}
                placeholder={t("search.filterCsvHint")}
                value={expr.excludeTerms.join(", ")}
                onChange={(e) => updateExpr({ excludeTerms: splitCsv(e.currentTarget.value) })}
                size="xs"
              />
              <TextInput
                label={t("search.filterIncludeExt")}
                placeholder={t("search.filterExtHint")}
                value={expr.exts.join(", ")}
                onChange={(e) => updateExpr({ exts: splitCsv(e.currentTarget.value) })}
                size="xs"
              />
              <TextInput
                label={t("search.filterExcludeExt")}
                placeholder={t("search.filterExtHint")}
                value={expr.notExts.join(", ")}
                onChange={(e) => updateExpr({ notExts: splitCsv(e.currentTarget.value) })}
                size="xs"
              />
            </SimpleGrid>
            <Checkbox
              mt="sm"
              size="xs"
              label={t("search.hideChecksums")}
              checked={checksumsHidden}
              onChange={(e) => toggleChecksums(e.currentTarget.checked)}
            />
          </Paper>
        </Collapse>

        {/* 仓库聚合钻取条：全部 + 各仓库命中数（点击限定 / 还原） */}
        {q && facets.length > 0 && (
          <Group gap={8}>
            <Chip
              checked={!activeRepo}
              onChange={() => drillRepo(null)}
              size="xs"
              variant="light"
            >
              {t("search.facetAll")} {facetTotal.toLocaleString(currentLocaleTag())}
            </Chip>
            {facets.map((f) => (
              <Chip
                key={f.repository}
                checked={activeRepo === f.repository}
                onChange={() => drillRepo(activeRepo === f.repository ? null : f.repository)}
                size="xs"
                variant="light"
              >
                {f.repository} {f.count.toLocaleString(currentLocaleTag())}
              </Chip>
            ))}
          </Group>
        )}

        {/* 进度条固定在结果区**内部顶部**：搜索框与 facet 条不被整体卸载（规格要求），
            首载时骨架屏照常显示、换排序 / 翻页时旧结果留在原位，二者与进度条上下共存。
            取消按钮挂在进度条上，点击即 abort 在途请求并静默回到空闲态。 */}
        {q ? (
          <Box style={{ flex: 1, minHeight: 0, display: "flex", flexDirection: "column", gap: 8 }}>
            {searching && <SearchProgress onCancel={state.cancel} />}
            {resultArea}
          </Box>
        ) : (
          <Box style={{ flex: 1 }} />
        )}
      </Box>
    </PageShell>
  );
}

/**
 * 左栏样式（规格 §3.9）：`flex: 0 0 40%`、`minWidth: 320`、`maxWidth: 460`、独立滚动。
 * 窄屏改为占满宽度；`hidden` 时只 `display:none`，DOM 与滚动位置都保留。
 */
function leftColumnStyle(isNarrow: boolean, hidden: boolean) {
  return {
    display: hidden ? "none" : "flex",
    flexDirection: "column" as const,
    gap: 8,
    minHeight: 0,
    overflow: "auto",
    ...(isNarrow
      ? { flex: "1 1 100%", minWidth: 0 }
      : { flex: "0 0 40%", minWidth: 320, maxWidth: 460 }),
  };
}

/** 右栏样式：`flex: 1`、`minWidth: 0`、独立滚动；窄屏未进详情时隐藏。 */
function rightColumnStyle(isNarrow: boolean, detailOpen: boolean) {
  return {
    display: isNarrow && !detailOpen ? "none" : "block",
    flex: 1,
    minWidth: 0,
    minHeight: 0,
    overflow: "auto",
  };
}
