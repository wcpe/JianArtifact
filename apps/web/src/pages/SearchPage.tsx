// 全局搜索结果页：/search?q=keyword
// 支持高级表达式（-排除词 / repo: / format: / ext: / 引号短语），配筛选面板双向同步；
// 结果按仓库聚合（facet 钻取条）+ Everything 风格扁平表格（名称/仓库/路径/大小/时间）。
import {
  ActionIcon,
  Badge,
  Button,
  Checkbox,
  Chip,
  Code,
  Collapse,
  Group,
  Pagination,
  Paper,
  Popover,
  ScrollArea,
  Select,
  SimpleGrid,
  Stack,
  Table,
  Text,
  TextInput,
  Tooltip,
} from "@mantine/core";
import {
  IconChevronDown,
  IconChevronUp,
  IconDownload,
  IconFile,
  IconFilter,
  IconHelp,
  IconSearch,
  IconSelector,
} from "@tabler/icons-react";
import { PageHeader } from "@jianartifact/ui";
import { useEffect, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { useNavigate, useSearchParams } from "react-router-dom";

import { searchAssets } from "../api/endpoints";
import { useAsync } from "../hooks/useAsync";
import { assetDownloadUrl, formatBytes } from "../lib/assetTree";
import {
  buildSearchExpression,
  CHECKSUM_EXTS,
  parseSearchExpression,
  splitCsv,
  type SearchExpression,
} from "../lib/searchQuery";

const PAGE_SIZE = 50;
const FORMAT_OPTIONS = ["maven", "raw", "npm"];

/** 可排序列（服务端下推排序，键值与后端白名单一致）。 */
type SortKey = "name" | "repo" | "path" | "size" | "updated";

// 语法帮助条目：[表达式示例, 文案 key, 兜底文案]
const SYNTAX_ROWS: [string, string, string][] = [
  ["spring core", "search.helpTerms", "同时包含多个关键词"],
  ["-sources", "search.helpExclude", "排除包含该词的路径"],
  ['"my lib"', "search.helpPhrase", "引号内空格不切分"],
  ["repo:maven-releases", "search.helpRepo", "限定仓库（-repo: 排除）"],
  ["format:maven", "search.helpFormat", "限定仓库格式 raw/maven/npm"],
  ["ext:jar", "search.helpExt", "按扩展名筛选（-ext:sha1 排除）"],
];

// splitPath 把完整路径拆为 [目录, 文件名]。
function splitPath(path: string): [string, string] {
  const idx = path.lastIndexOf("/");
  return idx === -1 ? ["", path] : [path.slice(0, idx), path.slice(idx + 1)];
}

export function SearchPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const [searchParams] = useSearchParams();
  const q = searchParams.get("q") ?? "";
  const [page, setPage] = useState(1);
  const [input, setInput] = useState(q);
  const [filtersOpen, setFiltersOpen] = useState(false);
  const [sortBy, setSortBy] = useState<SortKey>("path");
  const [sortOrder, setSortOrder] = useState<"asc" | "desc">("asc");

  // 点击表头切换排序：同列翻转方向，换列重置为升序；排序变化回到第一页
  const toggleSort = (key: SortKey) => {
    if (sortBy === key) {
      setSortOrder((o) => (o === "asc" ? "desc" : "asc"));
    } else {
      setSortBy(key);
      setSortOrder("asc");
    }
    setPage(1);
  };
  const sortIcon = (key: SortKey) =>
    sortBy !== key ? (
      <IconSelector size={12} opacity={0.4} />
    ) : sortOrder === "asc" ? (
      <IconChevronUp size={12} />
    ) : (
      <IconChevronDown size={12} />
    );

  // URL 的 q 变化（如 Header 搜索栏跳转）时同步输入框并回到第一页
  useEffect(() => {
    setInput(q);
    setPage(1);
  }, [q]);

  const state = useAsync(
    () =>
      q
        ? searchAssets({ q, sort: sortBy, order: sortOrder, page, page_size: PAGE_SIZE })
        : Promise.resolve({ items: [], total: 0, facets: [] }),
    [q, page, sortBy, sortOrder],
  );

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

  const handleSubmit = () => {
    if (input.trim()) {
      navigate(`/search?q=${encodeURIComponent(input.trim())}`);
      setPage(1);
    }
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

  const openRepo = (repository: string) => {
    navigate(`/repositories/${encodeURIComponent(repository)}`);
  };

  const items = state.data?.items ?? [];

  return (
    <>
      <PageHeader
        title={t("search.title", { defaultValue: "制品搜索" })}
        description={t("search.description", { defaultValue: "跨仓库搜索制品路径" })}
      />

      <Stack gap="md">
        <Group gap="xs" wrap="nowrap" align="flex-start">
          <TextInput
            placeholder={t("search.placeholder", { defaultValue: "搜索制品..." })}
            leftSection={<IconSearch size={16} />}
            rightSection={
              <Popover width={340} position="bottom-end" shadow="md">
                <Popover.Target>
                  <ActionIcon
                    variant="subtle"
                    color="gray"
                    aria-label={t("search.syntaxHelp", { defaultValue: "语法帮助" })}
                  >
                    <IconHelp size={16} />
                  </ActionIcon>
                </Popover.Target>
                <Popover.Dropdown>
                  <Stack gap={6}>
                    <Text size="sm" fw={600}>
                      {t("search.syntaxHelp", { defaultValue: "语法帮助" })}
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
            }
            value={input}
            onChange={(e) => setInput(e.currentTarget.value)}
            onKeyDown={(e) => e.key === "Enter" && handleSubmit()}
            style={{ flex: 1, maxWidth: 560 }}
          />
          <Button
            variant={filtersOpen ? "filled" : "light"}
            leftSection={<IconFilter size={14} />}
            rightSection={filtersOpen ? <IconChevronUp size={14} /> : <IconChevronDown size={14} />}
            onClick={() => setFiltersOpen((o) => !o)}
          >
            {t("search.filters", { defaultValue: "筛选" })}
          </Button>
          <Button onClick={handleSubmit}>{t("common.search", { defaultValue: "搜索" })}</Button>
        </Group>

        <Collapse in={filtersOpen}>
          <Paper withBorder radius="md" p="sm">
            <SimpleGrid cols={{ base: 1, sm: 2, lg: 3 }} spacing="sm">
              <Select
                label={t("search.filterFormat", { defaultValue: "仓库格式" })}
                placeholder={t("search.filterFormatAll", { defaultValue: "全部格式" })}
                data={FORMAT_OPTIONS}
                value={expr.formats[0] ?? null}
                onChange={(v) => updateExpr({ formats: v ? [v] : [] })}
                clearable
                size="xs"
              />
              <TextInput
                label={t("search.filterRepo", { defaultValue: "限定仓库" })}
                placeholder={t("search.filterCsvHint", { defaultValue: "多个用逗号分隔" })}
                value={expr.repos.join(", ")}
                onChange={(e) => updateExpr({ repos: splitCsv(e.currentTarget.value) })}
                size="xs"
              />
              <TextInput
                label={t("search.filterNotRepo", { defaultValue: "排除仓库" })}
                placeholder={t("search.filterCsvHint", { defaultValue: "多个用逗号分隔" })}
                value={expr.notRepos.join(", ")}
                onChange={(e) => updateExpr({ notRepos: splitCsv(e.currentTarget.value) })}
                size="xs"
              />
              <TextInput
                label={t("search.filterExcludeTerms", { defaultValue: "排除关键词" })}
                placeholder={t("search.filterCsvHint", { defaultValue: "多个用逗号分隔" })}
                value={expr.excludeTerms.join(", ")}
                onChange={(e) => updateExpr({ excludeTerms: splitCsv(e.currentTarget.value) })}
                size="xs"
              />
              <TextInput
                label={t("search.filterIncludeExt", { defaultValue: "包含扩展名" })}
                placeholder={t("search.filterExtHint", { defaultValue: "如 jar,pom" })}
                value={expr.exts.join(", ")}
                onChange={(e) => updateExpr({ exts: splitCsv(e.currentTarget.value) })}
                size="xs"
              />
              <TextInput
                label={t("search.filterExcludeExt", { defaultValue: "排除扩展名" })}
                placeholder={t("search.filterExtHint", { defaultValue: "如 sha1,md5" })}
                value={expr.notExts.join(", ")}
                onChange={(e) => updateExpr({ notExts: splitCsv(e.currentTarget.value) })}
                size="xs"
              />
            </SimpleGrid>
            <Checkbox
              mt="sm"
              size="xs"
              label={t("search.hideChecksums", {
                defaultValue: "隐藏校验和 / 签名文件（sha1、md5、sha256、sha512、asc）",
              })}
              checked={checksumsHidden}
              onChange={(e) => toggleChecksums(e.currentTarget.checked)}
            />
          </Paper>
        </Collapse>

        {/* 仓库聚合钻取条：全部 + 各仓库命中数（点击限定 / 还原） */}
        {q && facets.length > 0 && (
          <Group gap={8}>
            <Chip checked={!activeRepo} onChange={() => drillRepo(null)} size="xs" variant="light">
              {t("search.facetAll", { defaultValue: "全部" })} {facetTotal.toLocaleString()}
            </Chip>
            {facets.map((f) => (
              <Chip
                key={f.repository}
                checked={activeRepo === f.repository}
                onChange={() => drillRepo(activeRepo === f.repository ? null : f.repository)}
                size="xs"
                variant="light"
              >
                {f.repository} {f.count.toLocaleString()}
              </Chip>
            ))}
          </Group>
        )}

        {q && state.data && items.length > 0 && (
          <>
            <Text size="sm" c="dimmed">
              {t("search.resultCount", {
                count: state.data.total,
                defaultValue: `共 ${state.data.total} 条结果`,
              })}
            </Text>
            <Paper withBorder radius="md">
              <ScrollArea>
                <Table highlightOnHover verticalSpacing={4} horizontalSpacing="sm" fz="xs">
                  <Table.Thead>
                    <Table.Tr>
                      <Table.Th
                        style={{ cursor: "pointer", userSelect: "none" }}
                        onClick={() => toggleSort("name")}
                      >
                        <Group gap={4} wrap="nowrap">
                          {t("search.colName", { defaultValue: "名称" })}
                          {sortIcon("name")}
                        </Group>
                      </Table.Th>
                      <Table.Th
                        style={{ cursor: "pointer", userSelect: "none" }}
                        onClick={() => toggleSort("repo")}
                      >
                        <Group gap={4} wrap="nowrap">
                          {t("search.colRepo", { defaultValue: "仓库" })}
                          {sortIcon("repo")}
                        </Group>
                      </Table.Th>
                      <Table.Th
                        style={{ cursor: "pointer", userSelect: "none" }}
                        onClick={() => toggleSort("path")}
                      >
                        <Group gap={4} wrap="nowrap">
                          {t("search.colPath", { defaultValue: "路径" })}
                          {sortIcon("path")}
                        </Group>
                      </Table.Th>
                      <Table.Th
                        style={{ cursor: "pointer", userSelect: "none", whiteSpace: "nowrap" }}
                        onClick={() => toggleSort("size")}
                      >
                        <Group gap={4} wrap="nowrap" justify="flex-end">
                          {t("search.colSize", { defaultValue: "大小" })}
                          {sortIcon("size")}
                        </Group>
                      </Table.Th>
                      <Table.Th
                        style={{ cursor: "pointer", userSelect: "none", whiteSpace: "nowrap" }}
                        onClick={() => toggleSort("updated")}
                      >
                        <Group gap={4} wrap="nowrap">
                          {t("search.colUpdated", { defaultValue: "修改时间" })}
                          {sortIcon("updated")}
                        </Group>
                      </Table.Th>
                      <Table.Th w={36} />
                    </Table.Tr>
                  </Table.Thead>
                  <Table.Tbody>
                    {items.map((item) => {
                      const [dir, name] = splitPath(item.path);
                      return (
                        <Table.Tr
                          key={`${item.repository}/${item.path}`}
                          style={{ cursor: "pointer" }}
                          onClick={() => openRepo(item.repository)}
                        >
                          <Table.Td style={{ whiteSpace: "nowrap" }}>
                            <Group gap={6} wrap="nowrap">
                              <IconFile
                                size={14}
                                color="var(--mantine-color-gray-6)"
                                style={{ flexShrink: 0 }}
                              />
                              <Text size="xs" fw={500}>
                                {name}
                              </Text>
                            </Group>
                          </Table.Td>
                          <Table.Td style={{ whiteSpace: "nowrap" }}>
                            <Badge variant="light" size="xs">
                              {item.repository}
                            </Badge>
                          </Table.Td>
                          <Table.Td style={{ maxWidth: 420, overflow: "hidden" }} title={item.path}>
                            <Text size="xs" c="dimmed" truncate="start">
                              {dir || "/"}
                            </Text>
                          </Table.Td>
                          <Table.Td style={{ textAlign: "right", whiteSpace: "nowrap" }}>
                            {formatBytes(item.size)}
                          </Table.Td>
                          <Table.Td style={{ whiteSpace: "nowrap" }}>
                            <Text size="xs" c="dimmed">
                              {new Date(item.updatedAt).toLocaleString()}
                            </Text>
                          </Table.Td>
                          <Table.Td onClick={(e) => e.stopPropagation()}>
                            <Tooltip label={t("repoDetail.download", { defaultValue: "下载" })}>
                              <ActionIcon
                                component="a"
                                href={assetDownloadUrl(item.repository, "", item.path)}
                                target="_blank"
                                rel="noreferrer"
                                variant="subtle"
                                color="gray"
                                size="sm"
                              >
                                <IconDownload size={14} />
                              </ActionIcon>
                            </Tooltip>
                          </Table.Td>
                        </Table.Tr>
                      );
                    })}
                  </Table.Tbody>
                </Table>
              </ScrollArea>
            </Paper>
            {totalPages > 1 && (
              <Group justify="center">
                <Pagination value={page} onChange={setPage} total={totalPages} />
              </Group>
            )}
          </>
        )}

        {q && state.data && items.length === 0 && !state.loading && (
          <Text c="dimmed">{t("search.noResults", { defaultValue: "未找到匹配的制品" })}</Text>
        )}
      </Stack>
    </>
  );
}
