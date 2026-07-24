// 访问令牌：列表 + 新建（明文仅回显一次）+ 吊销。
import { ActionIcon, Alert, Button, Code, Group, Modal, Table, TextInput } from "@mantine/core";
import { useForm } from "@mantine/form";
import { useDisclosure } from "@mantine/hooks";
import { IconTrash } from "@tabler/icons-react";
import { EmptyState, PageHeader } from "@jianartifact/ui";
import { useState } from "react";
import { useTranslation } from "react-i18next";

import { AsyncBoundary } from "../components/AsyncBoundary";
import { CopyTextButton } from "../components/CopyTextButton";
import { createToken, deleteToken, listTokens } from "../api/endpoints";
import type { TokenCreated } from "../api/types";
import { useAsync } from "../hooks/useAsync";
import { confirmDanger, notifyError, notifySuccess } from "../lib/feedback";

export function TokensPage() {
  const { t } = useTranslation();
  const state = useAsync(listTokens, []);
  const [createOpened, createModal] = useDisclosure(false);
  const [creating, setCreating] = useState(false);
  const [created, setCreated] = useState<TokenCreated | null>(null);

  const form = useForm({
    initialValues: { name: "" },
    validate: { name: (v) => (v.trim() ? null : t("tokens.name")) },
  });

  const handleCreate = form.onSubmit((values) => {
    setCreating(true);
    createToken(values.name)
      .then((token) => {
        createModal.close();
        form.reset();
        setCreated(token);
        state.reload();
      })
      .catch(notifyError)
      .finally(() => setCreating(false));
  });

  const handleDelete = (id: number) => {
    confirmDanger({
      title: t("common.delete"),
      message: t("tokens.deleteConfirm"),
      confirmLabel: t("common.delete"),
      cancelLabel: t("common.cancel"),
      onConfirm: () => {
        deleteToken(id)
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
        title={t("tokens.title")}
        description={t("tokens.description")}
        actions={<Button onClick={createModal.open}>{t("tokens.create")}</Button>}
      />

      <AsyncBoundary state={state}>
        {(list) =>
          list.items.length === 0 ? (
            <EmptyState message={t("tokens.empty")} />
          ) : (
            <Table striped highlightOnHover>
              <Table.Thead>
                <Table.Tr>
                  <Table.Th>{t("tokens.id")}</Table.Th>
                  <Table.Th>{t("tokens.name")}</Table.Th>
                  <Table.Th>{t("tokens.createdAt")}</Table.Th>
                  <Table.Th>{t("common.actions")}</Table.Th>
                </Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {list.items.map((token) => (
                  <Table.Tr key={token.id}>
                    <Table.Td>{token.id}</Table.Td>
                    <Table.Td>{token.name}</Table.Td>
                    <Table.Td>{new Date(token.createdAt).toLocaleString()}</Table.Td>
                    <Table.Td>
                      <ActionIcon
                        color="red"
                        variant="subtle"
                        onClick={() => handleDelete(token.id)}
                        aria-label={t("common.delete")}
                      >
                        <IconTrash size={16} />
                      </ActionIcon>
                    </Table.Td>
                  </Table.Tr>
                ))}
              </Table.Tbody>
            </Table>
          )
        }
      </AsyncBoundary>

      <Modal opened={createOpened} onClose={createModal.close} title={t("tokens.create")}>
        <form onSubmit={handleCreate}>
          <TextInput label={t("tokens.name")} withAsterisk {...form.getInputProps("name")} />
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

      <Modal
        opened={created !== null}
        onClose={() => setCreated(null)}
        title={t("tokens.plaintextTitle")}
      >
        <Alert color="yellow" variant="light" mb="sm">
          {t("tokens.plaintextHint")}
        </Alert>
        <Group justify="space-between" wrap="nowrap">
          <Code style={{ wordBreak: "break-all" }}>{created?.token}</Code>
          <CopyTextButton value={created?.token ?? ""} />
        </Group>
        <Group justify="flex-end" mt="md">
          <Button onClick={() => setCreated(null)}>{t("common.close")}</Button>
        </Group>
      </Modal>
    </>
  );
}
