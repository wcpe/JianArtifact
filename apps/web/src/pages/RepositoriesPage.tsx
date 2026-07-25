// 仓库管理：列表（分页 + format 图标）+ 新建 + 切换可见性 + 删除 + 清理。
import {
  ActionIcon,
  Badge,
  Button,
  Group,
  Modal,
  MultiSelect,
  Pagination,
  Select,
  Stack,
  Table,
  Text,
  TextInput,
  Tooltip,
} from "@mantine/core";
import { useForm } from "@mantine/form";
import { useDisclosure } from "@mantine/hooks";
import {
  IconBrandNpm,
  IconEraser,
  IconFile,
  IconPackage,
  IconTrash,
} from "@tabler/icons-react";
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
  listRepositories,
  updateRepository,
} from "../api/endpoints";
import type { RepoFormat, RepoType, Repository, RepoVisibility } from "../api/types";
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
  const [page, setPage] = useState(1);
  const state = useAsync(() => listRepositories({ page, page_size: PAGE_SIZE }), [page]);
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
        defaultValue: "将删除仓库中没有 .jar 文件的 Maven 制品目录（仅保留含 jar 的完整构件）。此操作不可撤销。",
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
      <PageHeader
        title={t("repositories.title")}
        description={t("repositories.description")}
        actions={<Button onClick={createModal.open}>{t("repositories.create")}</Button>}
      />

      <AsyncBoundary state={state}>
        {(list) =>
          list.items.length === 0 && page === 1 ? (
            <EmptyState message={t("repositories.empty")} />
          ) : (
            <Stack gap="md">
              <Table striped highlightOnHover>
                <Table.Thead>
                  <Table.Tr>
                    <Table.Th>{t("repositories.name")}</Table.Th>
                    <Table.Th>{t("repositories.type")}</Table.Th>
                    <Table.Th>{t("repositories.visibility")}</Table.Th>
                    <Table.Th>{t("repositories.artifactCount")}</Table.Th>
                    <Table.Th>{t("repositories.totalSize")}</Table.Th>
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
                            <Text fw={600} size="sm">
                              {repo.name}
                            </Text>
                            <Tooltip label={protoUrl} position="top" withArrow>
                              <span>
                                <CopyTextButton value={protoUrl} variant="icon" />
                              </span>
                            </Tooltip>
                          </Group>
                        </Table.Td>
                        <Table.Td>
                          <Badge variant="light" color={TYPE_COLOR[repo.type] ?? "gray"} size="sm">
                            {repo.type}
                          </Badge>
                        </Table.Td>
                        <Table.Td>
                          <Badge
                            color={repo.visibility === "public" ? "blue" : "gray"}
                            variant="light"
                            size="sm"
                            style={{ cursor: "pointer" }}
                            onClick={() => handleToggleVisibility(repo)}
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
                          <Group gap="xs" wrap="nowrap">
                            <Button
                              size="xs"
                              variant="light"
                              onClick={() => navigate(`/repositories/${repo.name}`)}
                            >
                              {t("repositories.browse")}
                            </Button>
                            {repo.visibility === "public" && (
                              <Button
                                size="xs"
                                variant="subtle"
                                onClick={() =>
                                  window.open(`/p/${encodeURIComponent(repo.name)}`, "_blank")
                                }
                              >
                                {t("repositories.publicLink")}
                              </Button>
                            )}
                            {repo.format === "maven" && repo.type === "hosted" && (
                              <Tooltip
                                label={t("repositories.cleanupTooltip", { defaultValue: "清理无 Jar 制品" })}
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
                            <ActionIcon
                              color="red"
                              variant="subtle"
                              onClick={() => handleDelete(repo)}
                              aria-label={t("common.delete")}
                            >
                              <IconTrash size={16} />
                            </ActionIcon>
                          </Group>
                        </Table.Td>
                      </Table.Tr>
                    );
                  })}
                </Table.Tbody>
              </Table>
              {totalPages > 1 && (
                <Group justify="center">
                  <Pagination value={page} onChange={setPage} total={totalPages} />
                </Group>
              )}
            </Stack>
          )
        }
      </AsyncBoundary>

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
