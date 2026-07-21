// 用户管理：列表 + 新建 / 改角色状态 / 重置口令 / 删除。
import { ActionIcon, Badge, Button, Group, Modal, PasswordInput, Select, Table, TextInput } from "@mantine/core";
import { useForm } from "@mantine/form";
import { useDisclosure } from "@mantine/hooks";
import { IconKey, IconTrash } from "@tabler/icons-react";
import { EmptyState, PageHeader } from "@jianartifact/ui";
import { useState } from "react";
import { useTranslation } from "react-i18next";

import { AsyncBoundary } from "../components/AsyncBoundary";
import { createUser, deleteUser, changePassword, listUsers, updateUser } from "../api/endpoints";
import type { User, UserRole, UserStatus } from "../api/types";
import { useAsync } from "../hooks/useAsync";
import { confirmDanger, notifyError, notifySuccess } from "../lib/feedback";

export function UsersPage() {
  const { t } = useTranslation();
  const state = useAsync(() => listUsers({ page_size: 100 }), []);

  const [createOpened, createModal] = useDisclosure(false);
  const [creating, setCreating] = useState(false);
  const [pwdUser, setPwdUser] = useState<User | null>(null);
  const [savingPwd, setSavingPwd] = useState(false);

  const createForm = useForm({
    initialValues: { username: "", password: "", role: "user" as UserRole },
    validate: {
      username: (v) => (v.trim() ? null : t("users.username")),
      password: (v) => (v.length >= 8 ? null : t("auth.passwordRule")),
    },
  });
  const pwdForm = useForm({
    initialValues: { password: "" },
    validate: { password: (v) => (v.length >= 8 ? null : t("auth.passwordRule")) },
  });

  const roleOptions = [
    { value: "admin", label: t("users.roleAdmin") },
    { value: "user", label: t("users.roleUser") },
  ];

  const handleCreate = createForm.onSubmit((values) => {
    setCreating(true);
    createUser(values)
      .then(() => {
        createModal.close();
        createForm.reset();
        notifySuccess(t("common.created"));
        state.reload();
      })
      .catch(notifyError)
      .finally(() => setCreating(false));
  });

  const handleToggleStatus = (user: User) => {
    const next: UserStatus = user.status === "active" ? "disabled" : "active";
    updateUser(user.id, { status: next })
      .then(() => {
        notifySuccess(t("common.updated"));
        state.reload();
      })
      .catch(notifyError);
  };

  const handleChangeRole = (user: User, role: UserRole) => {
    updateUser(user.id, { role })
      .then(() => {
        notifySuccess(t("common.updated"));
        state.reload();
      })
      .catch(notifyError);
  };

  const handleDelete = (user: User) => {
    confirmDanger({
      title: t("common.delete"),
      message: t("users.deleteConfirm"),
      confirmLabel: t("common.delete"),
      cancelLabel: t("common.cancel"),
      onConfirm: () => {
        deleteUser(user.id)
          .then(() => {
            notifySuccess(t("common.deleted"));
            state.reload();
          })
          .catch(notifyError);
      },
    });
  };

  const handleChangePassword = pwdForm.onSubmit((values) => {
    if (!pwdUser) {
      return;
    }
    setSavingPwd(true);
    changePassword(pwdUser.id, values.password)
      .then(() => {
        setPwdUser(null);
        pwdForm.reset();
        notifySuccess(t("common.saved"));
      })
      .catch(notifyError)
      .finally(() => setSavingPwd(false));
  });

  return (
    <>
      <PageHeader
        title={t("users.title")}
        description={t("users.description")}
        actions={<Button onClick={createModal.open}>{t("users.create")}</Button>}
      />

      <AsyncBoundary state={state}>
        {(list) =>
          list.items.length === 0 ? (
            <EmptyState message={t("users.empty")} />
          ) : (
            <Table striped highlightOnHover>
              <Table.Thead>
                <Table.Tr>
                  <Table.Th>{t("users.id")}</Table.Th>
                  <Table.Th>{t("users.username")}</Table.Th>
                  <Table.Th>{t("users.role")}</Table.Th>
                  <Table.Th>{t("users.status")}</Table.Th>
                  <Table.Th>{t("common.actions")}</Table.Th>
                </Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {list.items.map((user) => (
                  <Table.Tr key={user.id}>
                    <Table.Td>{user.id}</Table.Td>
                    <Table.Td>{user.username}</Table.Td>
                    <Table.Td>
                      <Select
                        size="xs"
                        w={120}
                        data={roleOptions}
                        value={user.role}
                        allowDeselect={false}
                        onChange={(v) => v && handleChangeRole(user, v as UserRole)}
                      />
                    </Table.Td>
                    <Table.Td>
                      <Badge
                        color={user.status === "active" ? "green" : "gray"}
                        variant="light"
                        style={{ cursor: "pointer" }}
                        onClick={() => handleToggleStatus(user)}
                      >
                        {user.status === "active" ? t("users.statusActive") : t("users.statusDisabled")}
                      </Badge>
                    </Table.Td>
                    <Table.Td>
                      <Group gap="xs">
                        <ActionIcon variant="subtle" onClick={() => setPwdUser(user)} aria-label={t("users.changePassword")}>
                          <IconKey size={16} />
                        </ActionIcon>
                        <ActionIcon color="red" variant="subtle" onClick={() => handleDelete(user)} aria-label={t("common.delete")}>
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

      <Modal opened={createOpened} onClose={createModal.close} title={t("users.create")}>
        <form onSubmit={handleCreate}>
          <TextInput label={t("users.username")} withAsterisk {...createForm.getInputProps("username")} />
          <PasswordInput mt="sm" label={t("auth.password")} withAsterisk {...createForm.getInputProps("password")} />
          <Select mt="sm" label={t("users.role")} data={roleOptions} allowDeselect={false} {...createForm.getInputProps("role")} />
          <Group justify="flex-end" mt="md">
            <Button variant="default" onClick={createModal.close}>
              {t("common.cancel")}
            </Button>
            <Button type="submit" loading={creating}>{t("common.create")}</Button>
          </Group>
        </form>
      </Modal>

      <Modal opened={pwdUser !== null} onClose={() => setPwdUser(null)} title={t("users.changePassword")}>
        <form onSubmit={handleChangePassword}>
          <PasswordInput label={t("auth.password")} withAsterisk {...pwdForm.getInputProps("password")} />
          <Group justify="flex-end" mt="md">
            <Button variant="default" onClick={() => setPwdUser(null)}>
              {t("common.cancel")}
            </Button>
            <Button type="submit" loading={savingPwd}>{t("common.save")}</Button>
          </Group>
        </form>
      </Modal>
    </>
  );
}
