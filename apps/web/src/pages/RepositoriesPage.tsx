// 仓库管理：列表 + 新建（格式/类型/可见性）+ 切换可见性 + 删除 + 进入访问控制。
import {
  ActionIcon,
  Badge,
  Button,
  Code,
  Group,
  Modal,
  MultiSelect,
  Select,
  Table,
  Text,
  TextInput,
} from "@mantine/core";
import { useForm } from "@mantine/form";
import { useDisclosure } from "@mantine/hooks";
import { IconTrash } from "@tabler/icons-react";
import { EmptyState, PageHeader } from "@jianartifact/ui";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { useNavigate } from "react-router-dom";

import { AsyncBoundary } from "../components/AsyncBoundary";
import { CopyTextButton } from "../components/CopyTextButton";
import {
  createRepository,
  deleteRepository,
  listRepositories,
  updateRepository,
} from "../api/endpoints";
import type { RepoFormat, RepoType, Repository, RepoVisibility } from "../api/types";
import { useAsync } from "../hooks/useAsync";
import { confirmDanger, notifyError, notifySuccess } from "../lib/feedback";
import { formatBytes } from "../lib/assetTree";

const FORMAT_OPTIONS = ["raw", "maven", "npm"];
const TYPE_OPTIONS = ["hosted", "proxy", "group"];

const TYPE_COLOR: Record<string, string> = {
  hosted: "green",
  proxy: "cyan",
  group: "violet",
};

function protocolBaseFor(repo: Pick<Repository, "format" | "name">): string {
  return repo.format === "npm"
    ? `${window.location.origin}/npm/${encodeURIComponent(repo.name)}/`
    : `${window.location.origin}/repository/${encodeURIComponent(repo.name)}`;
}

export function RepositoriesPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const state = useAsync(() => listRepositories({ page_size: 100 }), []);
  const [createOpened, createModal] = useDisclosure(false);
  const [creating, setCreating] = useState(false);

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

  // group 成员候选：已加载仓库中与当前所选格式一致、且非自身者。
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

  return (
    <>
      <PageHeader
        title={t("repositories.title")}
        description={t("repositories.description")}
        actions={<Button onClick={createModal.open}>{t("repositories.create")}</Button>}
      />

      <AsyncBoundary state={state}>
        {(list) =>
          list.items.length === 0 ? (
            <EmptyState message={t("repositories.empty")} />
          ) : (
            <Table striped highlightOnHover>
              <Table.Thead>
                <Table.Tr>
                  <Table.Th>{t("repositories.name")}</Table.Th>
                  <Table.Th>{t("repositories.format")}</Table.Th>
                  <Table.Th>{t("repositories.type")}</Table.Th>
                  <Table.Th>{t("repositories.visibility")}</Table.Th>
                  <Table.Th>{t("repositories.url")}</Table.Th>
                  <Table.Th>{t("repositories.members")}</Table.Th>
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
                        <Text fw={600}>{repo.name}</Text>
                      </Table.Td>
                      <Table.Td>
                        <Badge variant="light">{repo.format}</Badge>
                      </Table.Td>
                      <Table.Td>
                        <Badge variant="light" color={TYPE_COLOR[repo.type] ?? "gray"}>
                          {repo.type}
                        </Badge>
                      </Table.Td>
                      <Table.Td>
                        <Badge
                          color={repo.visibility === "public" ? "blue" : "gray"}
                          variant="light"
                          style={{ cursor: "pointer" }}
                          onClick={() => handleToggleVisibility(repo)}
                        >
                          {repo.visibility === "public"
                            ? t("repositories.visibilityPublic")
                            : t("repositories.visibilityPrivate")}
                        </Badge>
                      </Table.Td>
                      <Table.Td>
                        <Group gap={4} wrap="nowrap">
                          <Code style={{ fontSize: 11 }}>{protoUrl}</Code>
                          <CopyTextButton value={protoUrl} variant="icon" />
                        </Group>
                      </Table.Td>
                      <Table.Td>
                        {repo.type === "group" && repo.members && repo.members.length > 0 ? (
                          <Group gap={4}>
                            {repo.members.map((m) => (
                              <Badge key={m} size="sm" variant="outline" color="violet">
                                {m}
                              </Badge>
                            ))}
                          </Group>
                        ) : repo.type === "proxy" && repo.remoteUrl ? (
                          <Text size="xs" c="dimmed" lineClamp={1} title={repo.remoteUrl}>
                            {repo.remoteUrl}
                          </Text>
                        ) : (
                          <Text size="xs" c="dimmed">
                            —
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
                        <Group gap="xs">
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
                          <Button
                            size="xs"
                            variant="light"
                            onClick={() => navigate(`/repositories/${repo.name}/acl`)}
                          >
                            {t("repositories.manageAcl")}
                          </Button>
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
