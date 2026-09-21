// 仓库管理：列表（分页 + format 图标）+ 新建 + 切换可见性 + 删除 + 清理。
// FR-68：固定布局（页头/筛选/分页固定，表格区内滚 + sticky 表头）；匿名视图隐藏管理操作。
// 每页条数随表格滚动区高度自适应（下限 10，见 rowsPerPage），高屏不再留大片空白。
import {
  Badge,
  Box,
  Button,
  Center,
  Group,
  Modal,
  MultiSelect,
  Pagination,
  Select,
  Stack,
  Table,
  Text,
  Textarea,
  TextInput,
  Tooltip,
  UnstyledButton,
} from "@mantine/core";
import { useForm } from "@mantine/form";
import { useDisclosure, useLocalStorage, useMediaQuery } from "@mantine/hooks";
import {
  IconArrowDown,
  IconArrowUp,
  IconBan,
  IconBrandNpm,
  IconCircleCheck,
  IconCircleX,
  IconCloudOff,
  IconDatabase,
  IconEraser,
  IconEye,
  IconFile,
  IconPackage,
  IconPinned,
  IconPinnedOff,
  IconPlug,
  IconPlus,
  IconSearch,
  IconTrash,
} from "@tabler/icons-react";
import { EmptyState } from "@jianartifact/ui";
import { useEffect, useMemo, useState } from "react";
import type { CSSProperties, ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { useNavigate } from "react-router-dom";

import { PageShell } from "../app/PageShell";
import { currentLocaleTag } from "../i18n/current";
import { AsyncBoundary } from "../components/AsyncBoundary";
import { CopyTextButton } from "../components/CopyTextButton";
import {
  cleanupEmptyArtifacts,
  createRepository,
  deleteRepository,
  getEnabledFormats,
  listAllRepositories,
  updateRepository,
} from "../api/endpoints";
import type {
  ConnectionStatusValue,
  RepoFormat,
  RepoType,
  Repository,
  RepoVisibility,
} from "../api/types";
import { useAuth } from "../auth/AuthContext";
import { usePinnedRepos } from "../hooks/usePinnedRepos";
import { useAsync } from "../hooks/useAsync";
import { CONN_COLOR, CONN_LABEL_KEY } from "../lib/connectionStatus";
import { confirmDanger, notifyError, notifySuccess } from "../lib/feedback";
import { formatBytes } from "../lib/assetTree";
import { formatUtcToLocal, formatUtcToLocalDate } from "../lib/timeFormat";

const PAGE_SIZE = 10;

/**
 * 每页条数自适应的上限：高屏也不一次渲染超过这个行数（渲染护栏，防止 4K 屏一次铺 40+ 行）。
 */
const MAX_ROWS_PER_PAGE = 50;
const TYPE_OPTIONS = ["hosted", "proxy", "group"];

/** 仓库列表视图偏好（排序/方向/分组/名称筛选），持久化到 localStorage 跨会话记忆。 */
interface RepoViewPrefs {
  sortBy: string;
  sortOrder: "asc" | "desc";
  groupBy: string;
  nameFilter: string;
}
const REPO_VIEW_KEY = "jianartifact.repoView";

const TYPE_COLOR: Record<string, string> = {
  hosted: "green",
  proxy: "cyan",
  group: "violet",
};

/**
 * 连接状态 → 状态图标（与 `CONN_COLOR` 同色系）。
 *
 * 徽章里单靠「可用/不可用」文字扫读要逐字读，加一枚语义图标后一眼可辨；
 * 图标本身不承载唯一信息（文字仍在、Tooltip 仍给完整口径），色盲用户不受影响。
 */
const CONN_ICON: Record<ConnectionStatusValue, typeof IconCircleCheck> = {
  AVAILABLE: IconCircleCheck,
  AUTO_BLOCKED: IconBan,
  UNAVAILABLE: IconCircleX,
  OFFLINE: IconCloudOff,
  READY: IconPlug,
};

/**
 * 置顶行的视觉高亮（FR-56 置顶语义的可见反馈）。
 *
 * 为什么用 `backgroundImage` 而不是 `backgroundColor`：
 * Mantine v7 的表格把「斑马纹底」与 `highlightOnHover` 都写成**类选择器**的
 * `background-color`，行内 `background-color` 会以更高优先级把两者一起顶掉——
 * 置顶行就再也看不到 hover 反馈了。改成画一层半透明琥珀渐变（`--mantine-color-yellow-light`
 * 本身是 rgba），行底色与 hover 仍由类选择器生效、透在渐变下面。
 * hover 用 `--table-highlight-on-hover-color` 覆写成深一档的琥珀，置顶行悬停时加深而非变灰。
 * 左侧色条用 inset 阴影画，不占布局宽度（避免整行位移）。
 *
 * 颜色一律用 `yellow`（Mantine 默认色板里**没有 amber**，`var(--mantine-color-amber-6)`
 * 会静默失效、`color="amber"` 也解析不到——这正是此前置顶「没有高亮」的原因）。
 */
const PINNED_ROW_STYLE = {
  // Mantine 9 起 light 变体不再带透明度（M9 changelog：统一「去透明」以提升对比度），
  // 直接铺满整行会明显刺眼——这正是升级后置顶行「太亮」的原因；用 color-mix 兑回
  // 半透明，恢复升级前的柔和底色观感。hover 加深一档仍由变量覆写（同样兑回透明度）。
  backgroundImage:
    "linear-gradient(color-mix(in srgb, var(--mantine-color-yellow-light) 45%, transparent), color-mix(in srgb, var(--mantine-color-yellow-light) 45%, transparent))",
  "--table-highlight-on-hover-color":
    "color-mix(in srgb, var(--mantine-color-yellow-light-hover) 70%, transparent)",
  boxShadow: "inset 3px 0 0 0 var(--mantine-color-yellow-6)",
} as CSSProperties;

/**
 * 徽章列不许被压到「P...」「HOST...」。
 *
 * Mantine `Badge` 根节点是 `inline-flex` + `overflow: hidden`，其 label 的自动最小尺寸因此为 0——
 * 表格自动布局一旦发现总宽不够，就会把类型/可见性这两列一路压到只剩一两个字母
 * （实测 1152 视口：「PROXY」→「P…」、「公开」→「公.」）。`min-width: max-content`
 * 把这两列的收缩下限钉在完整文字宽度上，多出来的宽度从**仓库名列的空余量**里出
 * （实测：加它之后类型列 63→86、可见性 54→64，仓库名列 232 不变、依旧无截断）。
 */
const BADGE_NO_SHRINK: CSSProperties = { minWidth: "max-content" };

/** Format 图标映射 */
function FormatIcon({ format }: { format: string }) {
  switch (format) {
    case "maven":
      return <IconPackage size={16} color="var(--mantine-color-orange-6)" />;
    case "npm":
      return <IconBrandNpm size={16} color="var(--mantine-color-red-6)" />;
    default:
      return <IconFile size={16} color="var(--mantine-color-gray-5)" />;
  }
}

function protocolBaseFor(repo: Pick<Repository, "format" | "name">): string {
  return repo.format === "npm"
    ? `${window.location.origin}/npm/${encodeURIComponent(repo.name)}/`
    : `${window.location.origin}/repository/${encodeURIComponent(repo.name)}`;
}

/**
 * 数值列的紧凑排版：`图标 + 数值`，完整口径走 Tooltip。
 *
 * 为什么不用纯数字：表头（制品数/总大小）在窄列下会被挤到换行，图标让列身份不依赖表头也认得出来；
 * `whiteSpace: nowrap` 保证「1.2 MB」「日期」这类不折行——列宽不足时宁可整表横向滚动，
 * 也不要折成「2026-01-」+「05」这种半截日期（用户实测反馈：看起来像只显示了年月）。
 */
function MetricCell({ icon, hint, value }: { icon: ReactNode; hint: string; value: ReactNode }) {
  return (
    <Tooltip label={hint} position="top" withArrow>
      <Group gap={4} wrap="nowrap" justify="flex-end" style={{ whiteSpace: "nowrap" }}>
        <Box c="dimmed" style={{ display: "flex" }} aria-hidden>
          {icon}
        </Box>
        <Text size="sm">{value}</Text>
      </Group>
    </Tooltip>
  );
}

/** FR-114：连接状态列（状态图标 + 悬浮说明；hosted 仓库无上游连接概念 → 占位符「—」）。 */
function ConnectionStatusCell({ status }: { status?: ConnectionStatusValue }) {
  const { t } = useTranslation();
  const title = t("repositories.connectionStatus", { defaultValue: "连接状态" });
  if (!status) {
    // 保持列位对齐，不整列消失。
    return (
      <Text size="sm" c="dimmed">
        {t("repositories.statusNone", { defaultValue: "—" })}
      </Text>
    );
  }
  const statusLabel = t(CONN_LABEL_KEY[status]);
  const label = `${title}：${statusLabel}`;
  const Icon = CONN_ICON[status];
  return (
    <Tooltip label={label} position="top" withArrow>
      {/* 只用图标：徽章文字（「自动阻止」四个字）要占 ~100px，8 列在 1152 视口里只有 ~870px，
          正是它把类型/可见性徽章压成了「P...」「公.」。状态语义改由图标承载，
          悬浮与读屏（aria-label）补全文字，色盲用户也不吃亏（图标形状本身可辨）。 */}
      <span role="img" aria-label={label} style={{ display: "inline-flex" }}>
        <Icon size={18} color={`var(--mantine-color-${CONN_COLOR[status] ?? "gray"}-6)`} />
      </span>
    </Tooltip>
  );
}

/** 可排序表头：点击切换排序（同列翻转升降序），激活列显示方向箭头并加粗。 */
function SortableTh({
  label,
  field,
  sortBy,
  sortOrder,
  onSort,
}: {
  label: string;
  field: string;
  sortBy: string;
  sortOrder: "asc" | "desc";
  onSort: (field: string) => void;
}) {
  const active = sortBy === field;
  return (
    <Table.Th aria-sort={active ? (sortOrder === "asc" ? "ascending" : "descending") : "none"}>
      <UnstyledButton
        onClick={() => onSort(field)}
        style={{ display: "inline-flex", alignItems: "center", gap: 4 }}
      >
        <Text size="sm" fw={active ? 700 : 500}>
          {label}
        </Text>
        {active ? (
          sortOrder === "asc" ? (
            <IconArrowUp size={12} />
          ) : (
            <IconArrowDown size={12} />
          )
        ) : null}
      </UnstyledButton>
    </Table.Th>
  );
}

export function RepositoriesPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const { user } = useAuth();
  // 仓库创建、配置、清理与删除均为全局管理员操作，前端仅按会话快照展示。
  const canManage = user?.role === "admin";
  // 置顶：本地偏好（localStorage），置顶仓库排到列表最前（分页语义内）。
  const { isPinned, toggle: togglePin, sortPinnedFirst } = usePinnedRepos();
  const [page, setPage] = useState(1);
  // 窄屏（< 48em）：8 列表格在手机上会把每列压到换行甚至截断（类型徽章只剩「P..」）。
  // 此时只保留「名称 + 操作」，被裁掉的类型/制品数/大小改由名称下方副文本承载。
  const isNarrow = useMediaQuery("(max-width: 48em)") ?? false;

  // 每页条数随表格滚动区高度自适应（FR-68 固定布局的补全）：容器高度固定后，
  // 写死 10 行会在高屏上留大片空白（"高度铺满了但数据还是固定的"）。
  // 量出滚动区实际高度与真实行高换算每页行数；下限仍是 PAGE_SIZE（矮屏回落为内滚），
  // 上限 MAX_ROWS_PER_PAGE 防止超高屏一次渲染过多行。jsdom 无布局（行高量出 0）时
  // 回落 PAGE_SIZE，测试行为不变。
  //
  // 滚动区是数据就绪后才挂载的，不能用只在 mount 时 observe 一次的 useElementSize——
  // 这里用回调 ref 持有节点、节点变化时重建 ResizeObserver。
  const [scrollerNode, setScrollerNode] = useState<HTMLDivElement | null>(null);
  const [scrollerHeight, setScrollerHeight] = useState(0);
  useEffect(() => {
    if (!scrollerNode || typeof ResizeObserver === "undefined") return;
    const observer = new ResizeObserver((entries) => {
      const height = entries[0]?.contentRect.height ?? scrollerNode.clientHeight;
      setScrollerHeight(height > 0 ? height : 0);
    });
    observer.observe(scrollerNode);
    // 首挂即量一次，避免先渲染 10 行再跳变。
    setScrollerHeight(scrollerNode.clientHeight);
    return () => observer.disconnect();
  }, [scrollerNode]);
  const rowsPerPage = useMemo(() => {
    if (scrollerHeight <= 0) return PAGE_SIZE;
    const theadH = scrollerNode?.querySelector("thead")?.getBoundingClientRect().height ?? 44;
    const rowH = scrollerNode?.querySelector("tbody tr")?.getBoundingClientRect().height ?? 0;
    if (rowH <= 0) return PAGE_SIZE;
    const capacity = Math.floor((scrollerHeight - theadH) / rowH);
    return Math.min(Math.max(capacity, PAGE_SIZE), MAX_ROWS_PER_PAGE);
    // isNarrow 变化会改变行高（副文本两行），需重算。
  }, [scrollerHeight, scrollerNode, isNarrow]);
  // FR-56: 排序与分组；视图偏好（排序/方向/分组/名称筛选）持久化到 localStorage，跨会话记忆。
  const [view, setView] = useLocalStorage<RepoViewPrefs>({
    key: REPO_VIEW_KEY,
    defaultValue: { sortBy: "name", sortOrder: "asc", groupBy: "none", nameFilter: "" },
    getInitialValueInEffect: false,
  });
  const { sortBy, sortOrder, groupBy, nameFilter } = view;
  const patchView = (patch: Partial<RepoViewPrefs>): void =>
    setView((current) => ({ ...current, ...patch }));
  /** 表头排序：同列切换升降序，新列默认升序，并回到第 1 页。 */
  const handleHeaderSort = (field: string): void => {
    if (sortBy === field) {
      patchView({ sortBy: field, sortOrder: sortOrder === "asc" ? "desc" : "asc" });
    } else {
      patchView({ sortBy: field, sortOrder: "asc" });
    }
    setPage(1);
  };
  // 一次拉全量（`listAllRepositories` 内部按契约单页上限拼接），客户端做「置顶 → 筛选 → 分页」。
  //
  // 为什么不用服务端分页：置顶是**本地偏好**（localStorage）、名称筛选是客户端条件，
  // 服务端都无从参与。继续用服务端分页会让置顶只在当前页内生效、筛选漏掉其它页的匹配项。
  const state = useAsync(
    () => listAllRepositories({ sort: sortBy, order: sortOrder }),
    // FR-67：登录态变化（模态框登录成功/登出）后重拉列表，匿名与全量集合不同。
    [sortBy, sortOrder, user],
    { cacheKey: `repositories:all:${sortBy}:${sortOrder}:${user?.id ?? "anon"}` },
  );
  const [createOpened, createModal] = useDisclosure(false);
  const [creating, setCreating] = useState(false);
  const enabledFormatsState = useAsync(
    // 启用格式是 admin 端点：匿名 / 普通用户本就不能建仓库，不该去打它——
    // 否则每次进仓库页都会在网络面板留下一条无意义的 401。
    () => (canManage ? getEnabledFormats() : Promise.resolve(null)),
    [canManage],
    { cacheKey: canManage ? `repositories:formats:${user?.id}` : undefined },
  );
  const formatOptions = enabledFormatsState.data?.formats ?? [];

  // 客户端派生：置顶前置 → 名称筛选 → 分页切片。
  // 不做 memo：依赖里的 sortPinnedFirst 每次渲染都是新函数，memo 形同虚设；
  // 仓库这个数据量级（几十~几百条）直接算即可。
  const pinnedFirst = sortPinnedFirst(state.data?.items ?? []);
  const filterKeyword = nameFilter.trim().toLowerCase();
  const filteredItems = filterKeyword
    ? pinnedFirst.filter((repo) => repo.name.toLowerCase().includes(filterKeyword))
    : pinnedFirst;
  const totalPages = Math.ceil(filteredItems.length / rowsPerPage);
  const visibleItems = filteredItems.slice((page - 1) * rowsPerPage, page * rowsPerPage);

  // 页码越界回退：筛选或数据量变化后当前页可能已不存在。
  if (page > 1 && page > totalPages) {
    setPage(1);
  }

  const form = useForm({
    initialValues: {
      name: "",
      format: "maven" as RepoFormat,
      type: "hosted" as RepoType,
      visibility: "private" as RepoVisibility,
      description: "",
      remoteUrl: "",
      members: [] as string[],
    },
    validate: {
      name: (v) => (v.trim() ? null : t("repositories.name")),
      remoteUrl: (v, values) =>
        values.type === "proxy" && !/^https?:\/\/.+/.test(v.trim())
          ? t("repositories.remoteUrlRequired")
          : null,
      members: (v, values) =>
        values.type === "group" && v.length === 0 ? t("repositories.membersRequired") : null,
    },
  });

  useEffect(() => {
    const first = formatOptions[0];
    if (first && !formatOptions.includes(form.values.format)) {
      form.setFieldValue("format", first);
    }
  }, [formatOptions, form]);

  const visibilityOptions = [
    { value: "public", label: t("repositories.visibilityPublic") },
    { value: "private", label: t("repositories.visibilityPrivate") },
  ];

  // group 成员候选：当前列表中同格式仓库
  const memberOptions = (state.data?.items ?? [])
    .filter((r) => r.format === form.values.format && r.name !== form.values.name)
    .map((r) => r.name);

  const handleCreate = form.onSubmit((values) => {
    setCreating(true);
    const payload = {
      name: values.name,
      format: values.format,
      type: values.type,
      visibility: values.visibility,
      // FR-81：仓库描述（可选），详情页页头展示。
      ...(values.description.trim() ? { description: values.description.trim() } : {}),
      ...(values.type === "proxy" ? { remoteUrl: values.remoteUrl.trim() } : {}),
      ...(values.type === "group" ? { members: values.members } : {}),
    };
    createRepository(payload)
      .then(() => {
        createModal.close();
        form.reset();
        notifySuccess(t("common.created"));
        state.reload();
      })
      .catch(notifyError)
      .finally(() => setCreating(false));
  });

  const handleToggleVisibility = (repo: Repository) => {
    const next: RepoVisibility = repo.visibility === "public" ? "private" : "public";
    updateRepository(repo.name, { visibility: next })
      .then(() => {
        notifySuccess(t("common.updated"));
        state.reload();
      })
      .catch(notifyError);
  };

  const handleDelete = (repo: Repository) => {
    confirmDanger({
      title: t("common.delete"),
      message: t("repositories.deleteConfirm"),
      confirmLabel: t("common.delete"),
      cancelLabel: t("common.cancel"),
      onConfirm: () => {
        deleteRepository(repo.name)
          .then(() => {
            notifySuccess(t("common.deleted"));
            state.reload();
          })
          .catch(notifyError);
      },
    });
  };

  const handleCleanup = (repo: Repository) => {
    confirmDanger({
      title: t("repositories.cleanupTitle", { defaultValue: "清理无 Jar 制品" }),
      message: t("repositories.cleanupConfirm", {
        defaultValue:
          "将删除仓库中没有 .jar 文件的 Maven 制品目录（仅保留含 jar 的完整构件）。此操作不可撤销。",
      }),
      confirmLabel: t("repositories.cleanupConfirmBtn", { defaultValue: "执行清理" }),
      cancelLabel: t("common.cancel"),
      onConfirm: () => {
        cleanupEmptyArtifacts(repo.name)
          .then((res) => {
            notifySuccess(
              t("repositories.cleanupDone", {
                n: res.deleted,
                defaultValue: `已清理 ${res.deleted} 个空制品目录`,
              }),
            );
            state.reload();
          })
          .catch(notifyError);
      },
    });
  };

  return (
    <>
      {/* FR-68 固定布局：页面撑满内容区高度，body 不滚；页头/筛选/分页固定，表格区内滚。
          高度口径统一由 PageShell 提供（dvh + header-offset），页面不再各自手算。 */}
      <PageShell>
        {/* FR-56: 排序与分组控件 + 名称筛选；右侧为新建仓库入口（与筛选同一行，避免多余空白带） */}
        <Group gap="sm" mb="md" wrap="wrap" justify="space-between">
          <Group gap="sm" wrap="wrap">
            <Select
              size="xs"
              label={t("repositories.sortBy", { defaultValue: "排序" })}
              data={[
                { value: "name", label: t("repositories.sortName", { defaultValue: "名称" }) },
                { value: "type", label: t("repositories.type", { defaultValue: "类型" }) },
                {
                  value: "artifact_count",
                  label: t("repositories.artifactCount", { defaultValue: "制品数" }),
                },
                {
                  value: "total_size",
                  label: t("repositories.totalSize", { defaultValue: "总大小" }),
                },
                {
                  value: "created_at",
                  label: t("repositories.sortCreatedAt", { defaultValue: "创建时间" }),
                },
              ]}
              value={sortBy}
              onChange={(v) => {
                if (!v) return;
                patchView({ sortBy: v });
                setPage(1);
              }}
              allowDeselect={false}
              w={isNarrow ? undefined : 120}
              // 窄屏：两个控件等分整行（原来固定 120 + 190 会在右侧留一条 44px 空白带，
              // 看起来像没排完版），「新建仓库」随后整行落下。
              style={isNarrow ? { flex: "1 1 0", minWidth: 0 } : undefined}
            />
            {/* 窄屏收起「方向 / 分组」：升降序可由表头点击切换，分组在手机上收益低，
                省下的两行高度留给表格本身。 */}
            {isNarrow ? null : (
              <Select
                size="xs"
                label={t("repositories.sortOrder", { defaultValue: "方向" })}
                data={[
                  { value: "asc", label: t("repositories.orderAsc", { defaultValue: "升序" }) },
                  { value: "desc", label: t("repositories.orderDesc", { defaultValue: "降序" }) },
                ]}
                value={sortOrder}
                onChange={(v) => {
                  if (!v) return;
                  patchView({ sortOrder: v as RepoViewPrefs["sortOrder"] });
                  setPage(1);
                }}
                allowDeselect={false}
                w={100}
              />
            )}
            {isNarrow ? null : (
              <Select
                size="xs"
                label={t("repositories.groupBy", { defaultValue: "分组" })}
                data={[
                  { value: "none", label: t("repositories.groupNone", { defaultValue: "不分组" }) },
                  {
                    value: "format",
                    label: t("repositories.groupFormat", { defaultValue: "按格式" }),
                  },
                  { value: "type", label: t("repositories.groupType", { defaultValue: "按类型" }) },
                ]}
                value={groupBy}
                onChange={(v) => v && patchView({ groupBy: v })}
                allowDeselect={false}
                w={120}
              />
            )}
            <TextInput
              size="xs"
              // 与同行 Select 一样带 label：此前只有 placeholder，控件顶边比其它控件矮一截，
              // 整排看起来就是「歪的」。
              label={t("repositories.filterLabel", { defaultValue: "筛选" })}
              leftSection={<IconSearch size={14} />}
              placeholder={t("repositories.filterName", { defaultValue: "按名称筛选" })}
              value={nameFilter}
              onChange={(e) => {
                patchView({ nameFilter: e.currentTarget.value });
                // 筛选后结果集变化，回到第 1 页避免停在越界页。
                setPage(1);
              }}
              w={isNarrow ? undefined : 190}
              style={isNarrow ? { flex: "1 1 0", minWidth: 0 } : undefined}
            />
          </Group>
          {canManage ? (
            <Button
              leftSection={<IconPlus size={16} />}
              // 窄屏主操作占整行：挤在筛选行尾部会把它自己换到下一行且只有半个宽度。
              w={isNarrow ? "100%" : undefined}
              onClick={createModal.open}
            >
              {t("repositories.create")}
            </Button>
          ) : null}
        </Group>

        {/* 表格区：占余高、内部滚动；空态/加载态同区展示 */}
        <Box style={{ flex: 1, minHeight: 0, display: "flex", flexDirection: "column" }}>
          <AsyncBoundary state={state}>
            {() => {
              // 置顶 / 筛选 / 分页都在组件顶层算好（见 filteredItems / visibleItems）。
              return state.data?.items.length === 0 && page === 1 ? (
                <Stack gap="sm" align="center" justify="center" style={{ flex: 1 }}>
                  <EmptyState
                    message={t("repositories.empty")}
                    description={
                      canManage
                        ? t("repositories.emptyCreateHint", {
                            defaultValue: "创建第一个仓库开始托管制品。",
                          })
                        : undefined
                    }
                  />
                  {canManage ? (
                    <Button leftSection={<IconPlus size={16} />} onClick={createModal.open}>
                      {t("repositories.create")}
                    </Button>
                  ) : null}
                </Stack>
              ) : filteredItems.length === 0 ? (
                <Center style={{ flex: 1 }}>
                  <Text size="sm" c="dimmed">
                    {t("repositories.filterNoMatch", { defaultValue: "没有匹配的仓库" })}
                  </Text>
                </Center>
              ) : (
                <Stack gap="md" style={{ flex: 1, minHeight: 0 }}>
                  <Box ref={setScrollerNode} style={{ flex: 1, minHeight: 0, overflowY: "auto" }}>
                    <Table
                      // 窄屏去掉斑马纹：相邻行底色相接会显得"挤成一片"，改用行边框 + 更大行距分隔。
                      striped={!isNarrow}
                      withRowBorders
                      highlightOnHover
                      stickyHeader
                      // 8 列在 1152px 附近会被压到「创建时间」只剩 86px（日期折成「2026-01-」+「05」）。
                      // 默认 horizontalSpacing=md 每列左右各 16px、8 列共 256px 纯内边距，
                      // 收到 sm 腾出 64px 给内容列——比缩短日期/砍列更无损。
                      horizontalSpacing={isNarrow ? "xs" : "sm"}
                      verticalSpacing={isNarrow ? "md" : "xs"}
                    >
                      <Table.Thead>
                        <Table.Tr>
                          <SortableTh
                            label={t("repositories.name")}
                            field="name"
                            sortBy={sortBy}
                            sortOrder={sortOrder}
                            onSort={handleHeaderSort}
                          />
                          {/* 窄屏只保留「名称 + 可见性 + 操作」：8 列在手机上会把每列压到
                              换行甚至截断（类型徽章只剩「P..」），被裁信息改由名称下方副文本承载。 */}
                          {isNarrow ? null : (
                            <SortableTh
                              label={t("repositories.type")}
                              field="type"
                              sortBy={sortBy}
                              sortOrder={sortOrder}
                              onSort={handleHeaderSort}
                            />
                          )}
                          {/* 匿名访客看到的本来就是公开仓库、连接状态属运维信息，这两列
                              对匿名视图没有信息量；登录用户（含普通用户）保留可见性以区分公开/私有。
                              窄屏连可见性列也收起来——它已并入名称下方副文本（徽章仍可点切换）。 */}
                          {user && !isNarrow ? (
                            // 表头 `nowrap`：徽章列被钉在文字宽度上，表头若允许折行会先牺牲成「可见」/「性」两行。
                            <Table.Th style={{ whiteSpace: "nowrap" }}>
                              {t("repositories.visibility")}
                            </Table.Th>
                          ) : null}
                          {user && !isNarrow ? (
                            // 表头 `nowrap`：单元格只剩一枚 18px 状态图标，若不给表头兜底，
                            // 列宽会被压到 61px → 「连接状态」折成「连接状」/「态」两行。
                            <Table.Th style={{ whiteSpace: "nowrap" }}>
                              {t("repositories.connectionStatus", { defaultValue: "连接状态" })}
                            </Table.Th>
                          ) : null}
                          {isNarrow ? null : (
                            <SortableTh
                              label={t("repositories.artifactCount")}
                              field="artifact_count"
                              sortBy={sortBy}
                              sortOrder={sortOrder}
                              onSort={handleHeaderSort}
                            />
                          )}
                          {isNarrow ? null : (
                            <SortableTh
                              label={t("repositories.totalSize")}
                              field="total_size"
                              sortBy={sortBy}
                              sortOrder={sortOrder}
                              onSort={handleHeaderSort}
                            />
                          )}
                          {isNarrow ? null : (
                            <SortableTh
                              label={t("repositories.createdAt", { defaultValue: "创建时间" })}
                              field="created_at"
                              sortBy={sortBy}
                              sortOrder={sortOrder}
                              onSort={handleHeaderSort}
                            />
                          )}
                          {isNarrow ? null : <Table.Th>{t("common.actions")}</Table.Th>}
                        </Table.Tr>
                      </Table.Thead>
                      <Table.Tbody>
                        {visibleItems.map((repo) => {
                          const protoUrl = protocolBaseFor(repo);
                          const pinned = isPinned(repo.name);
                          const visibilityLabel =
                            repo.visibility === "public"
                              ? t("repositories.visibilityPublic")
                              : t("repositories.visibilityPrivate");
                          // 行内操作统一「图标 + 文字」：纯图标看不出是什么操作（浏览/置顶/清理/删除）。
                          // 抽成变量是因为窄屏要把它从「操作」列挪到主标识下方：那里有整行宽度，
                          // 三四个按钮能一行放下，不会像窄列那样竖着排成四行把行撑高。
                          // 窄屏行内操作放大一号：compact-xs 只有 22px，放在手机上太小。
                          const actionSize = isNarrow ? "xs" : "compact-xs";
                          const rowActions = (
                            <Group gap={isNarrow ? 8 : 4} wrap="wrap">
                              <Button
                                size={actionSize}
                                variant="light"
                                leftSection={<IconEye size={14} />}
                                onClick={() => navigate(`/repositories/${repo.name}`)}
                              >
                                {t("repositories.browse")}
                              </Button>
                              {user ? (
                                <Tooltip
                                  label={pinned ? t("repositories.unpin") : t("repositories.pin")}
                                >
                                  <Button
                                    size={actionSize}
                                    variant="subtle"
                                    color={pinned ? "yellow" : "gray"}
                                    leftSection={
                                      pinned ? (
                                        <IconPinned size={14} />
                                      ) : (
                                        <IconPinnedOff size={14} />
                                      )
                                    }
                                    onClick={() => togglePin(repo.name)}
                                    aria-label={
                                      pinned ? t("repositories.unpin") : t("repositories.pin")
                                    }
                                  >
                                    {pinned ? t("repositories.unpin") : t("repositories.pin")}
                                  </Button>
                                </Tooltip>
                              ) : null}
                              {canManage && repo.format === "maven" && repo.type === "hosted" && (
                                <Tooltip
                                  label={t("repositories.cleanupTooltip", {
                                    defaultValue: "清理无 Jar 制品",
                                  })}
                                >
                                  <Button
                                    size={actionSize}
                                    variant="subtle"
                                    color="orange"
                                    leftSection={<IconEraser size={14} />}
                                    onClick={() => handleCleanup(repo)}
                                    aria-label={t("repositories.cleanupTooltip", {
                                      defaultValue: "清理无 Jar 制品",
                                    })}
                                  >
                                    {t("repositories.cleanupAction", { defaultValue: "清理" })}
                                  </Button>
                                </Tooltip>
                              )}
                              {canManage && (
                                <Button
                                  size={actionSize}
                                  variant="subtle"
                                  color="red"
                                  leftSection={<IconTrash size={14} />}
                                  onClick={() => handleDelete(repo)}
                                  aria-label={t("common.delete")}
                                >
                                  {t("common.delete")}
                                </Button>
                              )}
                            </Group>
                          );
                          return (
                            <Table.Tr
                              key={repo.id}
                              // 置顶行整行高亮（淡琥珀底 + 左侧色条）。
                              style={pinned ? PINNED_ROW_STYLE : undefined}
                            >
                              <Table.Td>
                                <Stack gap={isNarrow ? 12 : 2}>
                                  <Group
                                    gap={8}
                                    wrap="nowrap"
                                    justify={isNarrow ? "space-between" : undefined}
                                  >
                                    <Group gap={8} wrap="nowrap" style={{ minWidth: 0 }}>
                                      <FormatIcon format={repo.format} />
                                      {pinned ? (
                                        <IconPinned
                                          size={14}
                                          color="var(--mantine-color-yellow-6)"
                                          aria-hidden
                                        />
                                      ) : null}
                                      <Text
                                        fw={600}
                                        size="sm"
                                        c="blue"
                                        truncate
                                        style={{ cursor: "pointer" }}
                                        onClick={() => navigate(`/repositories/${repo.name}`)}
                                      >
                                        {repo.name}
                                      </Text>
                                    </Group>
                                    {/* 复制用「图标 + 文字」而不是裸图标：单独一个复制图标看不出
                                        复制的是协议地址；外层 Tooltip 仍展示完整 URL。 */}
                                    <Tooltip label={protoUrl} position="top" withArrow>
                                      <span>
                                        <CopyTextButton value={protoUrl} size="compact-xs" />
                                      </span>
                                    </Tooltip>
                                  </Group>
                                  {/* 窄屏被裁掉的列信息集中到这里（含可见性徽章，保持可点切换），
                                      并把行内操作一并挪下来：全宽排布，避免窄列竖排把行撑高。 */}
                                  {isNarrow ? (
                                    <Group gap={6} wrap="wrap">
                                      {user ? (
                                        <Badge
                                          size="xs"
                                          color={repo.visibility === "public" ? "blue" : "gray"}
                                          variant="light"
                                          style={canManage ? { cursor: "pointer" } : undefined}
                                          onClick={
                                            canManage
                                              ? () => handleToggleVisibility(repo)
                                              : undefined
                                          }
                                        >
                                          {visibilityLabel}
                                        </Badge>
                                      ) : null}
                                      <Text size="xs" c="dimmed" truncate>
                                        {repo.type} ·{" "}
                                        {t("repoDetail.assetCount", {
                                          n: repo.artifactCount ?? 0,
                                        })}{" "}
                                        · {formatBytes(repo.totalSize ?? 0)}
                                      </Text>
                                    </Group>
                                  ) : null}
                                  {isNarrow ? rowActions : null}
                                </Stack>
                              </Table.Td>
                              {isNarrow ? null : (
                                <Table.Td>
                                  <Badge
                                    variant="light"
                                    color={TYPE_COLOR[repo.type] ?? "gray"}
                                    size="sm"
                                    style={BADGE_NO_SHRINK}
                                  >
                                    {repo.type}
                                  </Badge>
                                </Table.Td>
                              )}
                              {user && !isNarrow ? (
                                <Table.Td>
                                  <Badge
                                    color={repo.visibility === "public" ? "blue" : "gray"}
                                    variant="light"
                                    size="sm"
                                    style={
                                      canManage
                                        ? { ...BADGE_NO_SHRINK, cursor: "pointer" }
                                        : BADGE_NO_SHRINK
                                    }
                                    onClick={
                                      canManage ? () => handleToggleVisibility(repo) : undefined
                                    }
                                  >
                                    {visibilityLabel}
                                  </Badge>
                                </Table.Td>
                              ) : null}
                              {user && !isNarrow ? (
                                <Table.Td>
                                  {/* FR-114：连接状态（仅 proxy/group 有 connectionStatus） */}
                                  <ConnectionStatusCell status={repo.connectionStatus?.status} />
                                </Table.Td>
                              ) : null}
                              {isNarrow ? null : (
                                <Table.Td>
                                  <MetricCell
                                    icon={<IconPackage size={14} />}
                                    hint={t("repositories.artifactCountTooltip", {
                                      count: repo.artifactCount ?? 0,
                                    })}
                                    value={repo.artifactCount ?? 0}
                                  />
                                </Table.Td>
                              )}
                              {isNarrow ? null : (
                                <Table.Td>
                                  <MetricCell
                                    icon={<IconDatabase size={14} />}
                                    hint={t("repositories.totalSizeTooltip", {
                                      size: formatBytes(repo.totalSize ?? 0),
                                      bytes: (repo.totalSize ?? 0).toLocaleString(
                                        currentLocaleTag(),
                                      ),
                                    })}
                                    value={formatBytes(repo.totalSize ?? 0)}
                                  />
                                </Table.Td>
                              )}
                              {isNarrow ? null : (
                                <Table.Td>
                                  {/* 完整日期 + 悬浮精确到秒；`nowrap` 防止列宽不足时折成「2026-01-」+「05」。 */}
                                  <Tooltip
                                    label={repo.createdAt ? formatUtcToLocal(repo.createdAt) : "-"}
                                    position="top"
                                    withArrow
                                  >
                                    <Text
                                      size="xs"
                                      c="dimmed"
                                      style={{ whiteSpace: "nowrap", cursor: "default" }}
                                    >
                                      {repo.createdAt ? formatUtcToLocalDate(repo.createdAt) : "-"}
                                    </Text>
                                  </Tooltip>
                                </Table.Td>
                              )}
                              {isNarrow ? null : <Table.Td>{rowActions}</Table.Td>}
                            </Table.Tr>
                          );
                        })}
                      </Table.Tbody>
                    </Table>
                  </Box>
                  {totalPages > 1 && (
                    <Group justify="center">
                      <Pagination value={page} onChange={setPage} total={totalPages} />
                    </Group>
                  )}
                </Stack>
              );
            }}
          </AsyncBoundary>
        </Box>
      </PageShell>

      <Modal opened={createOpened} onClose={createModal.close} title={t("repositories.create")}>
        <form onSubmit={handleCreate}>
          <TextInput label={t("repositories.name")} withAsterisk {...form.getInputProps("name")} />
          <Select
            mt="sm"
            label={t("repositories.format")}
            data={formatOptions}
            allowDeselect={false}
            {...form.getInputProps("format")}
          />
          <Select
            mt="sm"
            label={t("repositories.type")}
            data={TYPE_OPTIONS}
            allowDeselect={false}
            {...form.getInputProps("type")}
          />
          {form.values.type === "proxy" && (
            <TextInput
              mt="sm"
              label={t("repositories.remoteUrl")}
              placeholder={t("repositories.remoteUrlPlaceholder")}
              withAsterisk
              {...form.getInputProps("remoteUrl")}
            />
          )}
          {form.values.type === "group" && (
            <MultiSelect
              mt="sm"
              label={t("repositories.membersLabel")}
              description={t("repositories.membersHint")}
              data={memberOptions}
              searchable
              withAsterisk
              {...form.getInputProps("members")}
            />
          )}
          <Select
            mt="sm"
            label={t("repositories.visibility")}
            data={visibilityOptions}
            allowDeselect={false}
            {...form.getInputProps("visibility")}
          />
          {/* FR-81：仓库描述（可选），详情页页头展示 */}
          <Textarea
            mt="sm"
            label={t("repositories.descriptionLabel")}
            placeholder={t("repositories.descriptionPlaceholder")}
            autosize
            minRows={2}
            maxRows={4}
            {...form.getInputProps("description")}
          />
          <Group justify="flex-end" mt="md">
            <Button variant="default" onClick={createModal.close}>
              {t("common.cancel")}
            </Button>
            <Button type="submit" loading={creating}>
              {t("common.create")}
            </Button>
          </Group>
        </form>
      </Modal>
    </>
  );
}
