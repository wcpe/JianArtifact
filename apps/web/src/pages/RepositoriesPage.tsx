// 仓库管理：列表（分页 + format 图标）+ 新建 + 切换可见性 + 删除 + 清理。
// FR-68：固定布局（页头/筛选/分页固定，表格区内滚 + sticky 表头）；匿名视图隐藏管理操作。
import {
  ActionIcon,
  Badge,
  Box,
  Button,
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
} from "@mantine/core";
import { useForm } from "@mantine/form";
import { useDisclosure } from "@mantine/hooks";
import { IconBrandNpm, IconEraser, IconFile, IconPackage, IconTrash } from "@tabler/icons-react";
import { EmptyState, PageHeader } from "@jianartifact/ui";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { useNavigate } from "react-router-dom";

import { AsyncBoundary } from "../components/AsyncBoundary";
import { CopyTextButton } from "../components/CopyTextButton";
import {
  cleanupEmptyArtifacts,
  createRepository,
  deleteRepository,
  listRepositoriesSorted,
  updateRepository,
} from "../api/endpoints";
import type { RepoFormat, RepoType, Repository, RepoVisibility } from "../api/types";
import { useAuth } from "../auth/AuthContext";
import { useAsync } from "../hooks/useAsync";
import { confirmDanger, notifyError, notifySuccess } from "../lib/feedback";
import { formatBytes } from "../lib/assetTree";

const PAGE_SIZE = 10;
const FORMAT_OPTIONS = ["raw", "maven", "npm"];
const TYPE_OPTIONS = ["hosted", "proxy", "group"];

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

export function RepositoriesPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const { user } = useAuth();
  // FR-68：匿名视图隐藏管理操作（新建/删除/清理/可见性切换），后端仍兜底鉴权。
  const canManage = Boolean(user);
  const [page, setPage] = useState(1);
  // FR-56: 排序与分组
  const [sortBy, setSortBy] = useState<string>("name");
  const [sortOrder, setSortOrder] = useState<string>("asc");
  const [groupBy, setGroupBy] = useState<string>("none");
  const state = useAsync(
    () => listRepositoriesSorted({ page, page_size: PAGE_SIZE, sort: sortBy, order: sortOrder }),
    // FR-67：登录态变化（模态框登录成功/登出）后重拉列表，匿名与全量集合不同。
    [page, sortBy, sortOrder, user],
  );
  const [createOpened, createModal] = useDisclosure(false);
  const [creating, setCreating] = useState(false);

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
        <PageHeader
          title={canManage ? t("repositories.title") : t("repositories.publicTitle")}
          description={
            canManage ? t("repositories.description") : t("repositories.publicDescription")
          }
          actions={
            canManage ? (
              <Button onClick={createModal.open}>{t("repositories.create")}</Button>
            ) : undefined
          }
        />

        {/* FR-56: 排序与分组控件 */}
        <Group gap="sm" mb="md" wrap="wrap">
          <Select
            size="xs"
            label={t("repositories.sortBy", { defaultValue: "排序" })}
            data={[
              { value: "name", label: t("repositories.sortName", { defaultValue: "名称" }) },
              {
                value: "created_at",
                label: t("repositories.sortCreatedAt", { defaultValue: "创建时间" }),
              },
            ]}
            value={sortBy}
            onChange={(v) => v && setSortBy(v)}
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
            onChange={(v) => v && setSortOrder(v)}
            allowDeselect={false}
            w={100}
          />
          <Select
            size="xs"
            label={t("repositories.groupBy", { defaultValue: "分组" })}
            data={[
              { value: "none", label: t("repositories.groupNone", { defaultValue: "不分组" }) },
              { value: "format", label: t("repositories.groupFormat", { defaultValue: "按格式" }) },
              { value: "type", label: t("repositories.groupType", { defaultValue: "按类型" }) },
            ]}
            value={groupBy}
            onChange={(v) => v && setGroupBy(v)}
            allowDeselect={false}
            w={120}
          />
        </Group>

        {/* 表格区：占余高、内部滚动；空态/加载态同区展示 */}
        <Box style={{ flex: 1, minHeight: 0, display: "flex", flexDirection: "column" }}>
          <AsyncBoundary state={state}>
            {(list) =>
              list.items.length === 0 && page === 1 ? (
                <EmptyState message={t("repositories.empty")} />
              ) : (
                <Stack gap="md" style={{ flex: 1, minHeight: 0 }}>
                  <Box style={{ flex: 1, minHeight: 0, overflowY: "auto" }}>
                    <Table striped highlightOnHover stickyHeader>
                      <Table.Thead>
                        <Table.Tr>
                          <Table.Th>{t("repositories.name")}</Table.Th>
                          <Table.Th>{t("repositories.type")}</Table.Th>
                          <Table.Th>{t("repositories.visibility")}</Table.Th>
                          <Table.Th>{t("repositories.artifactCount")}</Table.Th>
                          <Table.Th>{t("repositories.totalSize")}</Table.Th>
                          <Table.Th>
                            {t("repositories.createdAt", { defaultValue: "创建时间" })}
                          </Table.Th>
                          <Table.Th>{t("common.actions")}</Table.Th>
                        </Table.Tr>
                      </Table.Thead>
                      <Table.Tbody>
                        {list.items.map((repo) => {
                          const protoUrl = protocolBaseFor(repo);
                          return (
                            <Table.Tr key={repo.id}>
                              <Table.Td>
                                <Group gap={8} wrap="nowrap">
                                  <FormatIcon format={repo.format} />
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
                                    ? new Date(repo.createdAt).toLocaleDateString()
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
              )
            }
          </AsyncBoundary>
        </Box>
      </Stack>

      <Modal opened={createOpened} onClose={createModal.close} title={t("repositories.create")}>
        <form onSubmit={handleCreate}>
          <TextInput label={t("repositories.name")} withAsterisk {...form.getInputProps("name")} />
          <Select
            mt="sm"
            label={t("repositories.format")}
            data={FORMAT_OPTIONS}
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
