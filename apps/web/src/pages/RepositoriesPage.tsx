// 仓库管理：列表 + 新建（格式/类型/可见性）+ 切换可见性 + 删除 + 进入访问控制。
import { ActionIcon, Badge, Button, Group, Modal, Select, Table, TextInput } from "@mantine/core";
import { useForm } from "@mantine/form";
import { useDisclosure } from "@mantine/hooks";
import { IconTrash } from "@tabler/icons-react";
import { EmptyState, PageHeader } from "@jianartifact/ui";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { useNavigate } from "react-router-dom";

import { AsyncBoundary } from "../components/AsyncBoundary";
import { createRepository, deleteRepository, listRepositories, updateRepository } from "../api/endpoints";
import type { RepoFormat, RepoType, Repository, RepoVisibility } from "../api/types";
import { useAsync } from "../hooks/useAsync";
import { confirmDanger, notifyError, notifySuccess } from "../lib/feedback";

const FORMAT_OPTIONS = ["raw", "maven", "npm"];
const TYPE_OPTIONS = ["hosted", "proxy", "group"];

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
    },
    validate: { name: (v) => (v.trim() ? null : t("repositories.name")) },
  });

  const visibilityOptions = [
    { value: "public", label: t("repositories.visibilityPublic") },
    { value: "private", label: t("repositories.visibilityPrivate") },
  ];

  const handleCreate = form.onSubmit((values) => {
    setCreating(true);
    createRepository(values)
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
                  <Table.Th>{t("common.actions")}</Table.Th>
                </Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {list.items.map((repo) => (
                  <Table.Tr key={repo.id}>
                    <Table.Td>{repo.name}</Table.Td>
                    <Table.Td>
                      <Badge variant="light">{repo.format}</Badge>
                    </Table.Td>
                    <Table.Td>{repo.type}</Table.Td>
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
                      <Group gap="xs">
                        <Button size="xs" variant="light" onClick={() => navigate(`/repositories/${repo.name}/acl`)}>
                          {t("repositories.manageAcl")}
                        </Button>
                        <ActionIcon color="red" variant="subtle" onClick={() => handleDelete(repo)} aria-label={t("common.delete")}>
                          <IconTrash size={16} />
                        </ActionIcon>
                      </Group>
                    </Table.Td>
                  </Table.Tr>
                ))}
              </Table.Tbody>
            </Table>
          )
        }
      </AsyncBoundary>

      <Modal opened={createOpened} onClose={createModal.close} title={t("repositories.create")}>
        <form onSubmit={handleCreate}>
          <TextInput label={t("repositories.name")} withAsterisk {...form.getInputProps("name")} />
          <Select mt="sm" label={t("repositories.format")} data={FORMAT_OPTIONS} allowDeselect={false} {...form.getInputProps("format")} />
          <Select mt="sm" label={t("repositories.type")} data={TYPE_OPTIONS} allowDeselect={false} {...form.getInputProps("type")} />
          <Select mt="sm" label={t("repositories.visibility")} data={visibilityOptions} allowDeselect={false} {...form.getInputProps("visibility")} />
          <Group justify="flex-end" mt="md">
            <Button variant="default" onClick={createModal.close}>
              {t("common.cancel")}
            </Button>
            <Button type="submit" loading={creating}>{t("common.create")}</Button>
          </Group>
        </form>
      </Modal>
    </>
  );
}
