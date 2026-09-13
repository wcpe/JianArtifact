// 仓库管理：列表（分页 + format 图标）+ 新建 + 切换可见性 + 删除 + 清理。
// FR-68：固定布局（页头/筛选/分页固定，表格区内滚 + sticky 表头）；匿名视图隐藏管理操作。
import {
  ActionIcon,
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
import { useDisclosure, useLocalStorage } from "@mantine/hooks";
import {
  IconArrowDown,
  IconArrowUp,
  IconBrandNpm,
  IconEraser,
  IconFile,
  IconPackage,
  IconPinned,
  IconPinnedOff,
  IconSearch,
  IconTrash,
} from "@tabler/icons-react";
import { EmptyState } from "@jianartifact/ui";
import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { useNavigate } from "react-router-dom";

import { AsyncBoundary } from "../components/AsyncBoundary";
import { CopyTextButton } from "../components/CopyTextButton";
import {
  cleanupEmptyArtifacts,
  createRepository,
  deleteRepository,
  getEnabledFormats,
  listRepositoriesSorted,
  updateRepository,
} from "../api/endpoints";
import type { RepoFormat, RepoType, Repository, RepoVisibility } from "../api/types";
import { useAuth } from "../auth/AuthContext";
import { usePinnedRepos } from "../hooks/usePinnedRepos";
import { useAsync } from "../hooks/useAsync";
import { CONN_COLOR, CONN_LABEL_KEY } from "../lib/connectionStatus";
import { confirmDanger, notifyError, notifySuccess } from "../lib/feedback";
import { formatBytes } from "../lib/assetTree";
import { formatUtcToLocalDate } from "../lib/timeFormat";

const PAGE_SIZE = 10;
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
  const state = useAsync(
    () => listRepositoriesSorted({ page, page_size: PAGE_SIZE, sort: sortBy, order: sortOrder }),
    // FR-67：登录态变化（模态框登录成功/登出）后重拉列表，匿名与全量集合不同。
    [page, sortBy, sortOrder, user],
    { cacheKey: `repositories:list:${page}:${sortBy}:${sortOrder}:${user?.id ?? "anon"}` },
  );
  const [createOpened, createModal] = useDisclosure(false);
  const [creating, setCreating] = useState(false);
  const enabledFormatsState = useAsync(() => getEnabledFormats(), [user], {
    cacheKey: `repositories:formats:${user?.id ?? "anon"}`,
  });
  const formatOptions = enabledFormatsState.data?.formats ?? [];

  const totalPages = Math.ceil((state.data?.total ?? 0) / PAGE_SIZE);

  // 页码越界回退
  if (state.data && page > 1 && state.data.items.length === 0) {
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
      {/* FR-68 固定布局：页面撑满内容区高度，body 不滚；页头/筛选/分页固定，表格区内滚。 */}
      <Stack
        gap={0}
        style={{
          height:
            "calc(100vh - var(--app-shell-header-offset, 56px) - 2 * var(--app-shell-padding, 12px))",
          overflow: "hidden",
        }}
      >
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
              w={120}
            />
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
            <TextInput
              size="xs"
              leftSection={<IconSearch size={14} />}
              placeholder={t("repositories.filterName", { defaultValue: "按名称筛选" })}
              value={nameFilter}
              onChange={(e) => patchView({ nameFilter: e.currentTarget.value })}
              w={190}
            />
          </Group>
          {canManage ? (
            <Button onClick={createModal.open}>{t("repositories.create")}</Button>
          ) : null}
        </Group>

        {/* 表格区：占余高、内部滚动；空态/加载态同区展示 */}
        <Box style={{ flex: 1, minHeight: 0, display: "flex", flexDirection: "column" }}>
          <AsyncBoundary state={state}>
            {(list) => {
              const visibleItems = sortPinnedFirst(list.items).filter((repo) =>
                repo.name.toLowerCase().includes(nameFilter.trim().toLowerCase()),
              );
              return list.items.length === 0 && page === 1 ? (
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
                    <Button onClick={createModal.open}>{t("repositories.create")}</Button>
                  ) : null}
                </Stack>
              ) : visibleItems.length === 0 ? (
                <Center style={{ flex: 1 }}>
                  <Text size="sm" c="dimmed">
                    {t("repositories.filterNoMatch", { defaultValue: "没有匹配的仓库" })}
                  </Text>
                </Center>
              ) : (
                <Stack gap="md" style={{ flex: 1, minHeight: 0 }}>
                  <Box style={{ flex: 1, minHeight: 0, overflowY: "auto" }}>
                    <Table striped highlightOnHover stickyHeader>
                      <Table.Thead>
                        <Table.Tr>
                          <SortableTh
                            label={t("repositories.name")}
                            field="name"
                            sortBy={sortBy}
                            sortOrder={sortOrder}
                            onSort={handleHeaderSort}
                          />
                          <SortableTh
                            label={t("repositories.type")}
                            field="type"
                            sortBy={sortBy}
                            sortOrder={sortOrder}
                            onSort={handleHeaderSort}
                          />
                          <Table.Th>{t("repositories.visibility")}</Table.Th>
                          <Table.Th>
                            {t("repositories.connectionStatus", { defaultValue: "连接状态" })}
                          </Table.Th>
                          <SortableTh
                            label={t("repositories.artifactCount")}
                            field="artifact_count"
                            sortBy={sortBy}
                            sortOrder={sortOrder}
                            onSort={handleHeaderSort}
                          />
                          <SortableTh
                            label={t("repositories.totalSize")}
                            field="total_size"
                            sortBy={sortBy}
                            sortOrder={sortOrder}
                            onSort={handleHeaderSort}
                          />
                          <SortableTh
                            label={t("repositories.createdAt", { defaultValue: "创建时间" })}
                            field="created_at"
                            sortBy={sortBy}
                            sortOrder={sortOrder}
                            onSort={handleHeaderSort}
                          />
                          <Table.Th>{t("common.actions")}</Table.Th>
                        </Table.Tr>
                      </Table.Thead>
                      <Table.Tbody>
                        {visibleItems.map((repo) => {
                          const protoUrl = protocolBaseFor(repo);
                          const pinned = isPinned(repo.name);
                          return (
                            <Table.Tr key={repo.id}>
                              <Table.Td>
                                <Group gap={8} wrap="nowrap">
                                  <FormatIcon format={repo.format} />
                                  {pinned ? (
                                    <IconPinned
                                      size={14}
                                      color="var(--mantine-color-amber-6)"
                                      aria-hidden
                                    />
                                  ) : null}
                                  <Text
                                    fw={600}
                                    size="sm"
                                    c="blue"
                                    style={{ cursor: "pointer" }}
                                    onClick={() => navigate(`/repositories/${repo.name}`)}
                                  >
                                    {repo.name}
                                  </Text>
                                  <Tooltip label={protoUrl} position="top" withArrow>
                                    <span>
                                      <CopyTextButton
                                        value={protoUrl}
                                        variant="icon"
                                        withTooltip={false}
                                      />
                                    </span>
                                  </Tooltip>
                                </Group>
                              </Table.Td>
                              <Table.Td>
                                <Badge
                                  variant="light"
                                  color={TYPE_COLOR[repo.type] ?? "gray"}
                                  size="sm"
                                >
                                  {repo.type}
                                </Badge>
                              </Table.Td>
                              <Table.Td>
                                <Badge
                                  color={repo.visibility === "public" ? "blue" : "gray"}
                                  variant="light"
                                  size="sm"
                                  style={canManage ? { cursor: "pointer" } : undefined}
                                  onClick={
                                    canManage ? () => handleToggleVisibility(repo) : undefined
                                  }
                                >
                                  {repo.visibility === "public"
                                    ? t("repositories.visibilityPublic")
                                    : t("repositories.visibilityPrivate")}
                                </Badge>
                              </Table.Td>
                              <Table.Td>
                                {/* FR-114：连接状态徽章（仅 proxy/group 有 connectionStatus） */}
                                {repo.connectionStatus ? (
                                  <Badge
                                    variant="light"
                                    color={CONN_COLOR[repo.connectionStatus.status] ?? "gray"}
                                    size="sm"
                                  >
                                    {t(
                                      CONN_LABEL_KEY[repo.connectionStatus.status] ??
                                        "repositories.statusReady",
                                    )}
                                  </Badge>
                                ) : (
                                  <Text size="sm" c="dimmed">
                                    {t("repositories.statusNone", { defaultValue: "—" })}
                                  </Text>
                                )}
                              </Table.Td>
                              <Table.Td>
                                <Text size="sm" ta="right">
                                  {repo.artifactCount ?? 0}
                                </Text>
                              </Table.Td>
                              <Table.Td>
                                <Text size="sm" ta="right">
                                  {formatBytes(repo.totalSize ?? 0)}
                                </Text>
                              </Table.Td>
                              <Table.Td>
                                <Text size="xs" c="dimmed">
                                  {repo.createdAt
                                    ? formatUtcToLocalDate(repo.createdAt)
                                    : "-"}
                                </Text>
                              </Table.Td>
                              <Table.Td>
                                <Group gap="xs" wrap="nowrap">
                                  <Button
                                    size="xs"
                                    variant="light"
                                    onClick={() => navigate(`/repositories/${repo.name}`)}
                                  >
                                    {t("repositories.browse")}
                                  </Button>
                                  {user ? (
                                    <Tooltip
                                      label={
                                        pinned ? t("repositories.unpin") : t("repositories.pin")
                                      }
                                    >
                                      <ActionIcon
                                        variant="subtle"
                                        color={pinned ? "amber" : "gray"}
                                        onClick={() => togglePin(repo.name)}
                                        aria-label={
                                          pinned ? t("repositories.unpin") : t("repositories.pin")
                                        }
                                      >
                                        {pinned ? (
                                          <IconPinned size={16} />
                                        ) : (
                                          <IconPinnedOff size={16} />
                                        )}
                                      </ActionIcon>
                                    </Tooltip>
                                  ) : null}
                                  {canManage &&
                                    repo.format === "maven" &&
                                    repo.type === "hosted" && (
                                      <Tooltip
                                        label={t("repositories.cleanupTooltip", {
                                          defaultValue: "清理无 Jar 制品",
                                        })}
                                      >
                                        <ActionIcon
                                          color="orange"
                                          variant="subtle"
                                          onClick={() => handleCleanup(repo)}
                                          aria-label={t("repositories.cleanupTooltip", {
                                            defaultValue: "清理无 Jar 制品",
                                          })}
                                        >
                                          <IconEraser size={16} />
                                        </ActionIcon>
                                      </Tooltip>
                                    )}
                                  {canManage && (
                                    <ActionIcon
                                      color="red"
                                      variant="subtle"
                                      onClick={() => handleDelete(repo)}
                                      aria-label={t("common.delete")}
                                    >
                                      <IconTrash size={16} />
                                    </ActionIcon>
                                  )}
                                </Group>
                              </Table.Td>
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
      </Stack>

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
