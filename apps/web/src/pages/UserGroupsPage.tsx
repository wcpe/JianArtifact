// 用户组管理（FR-36）：组的增删改查 + 组内成员管理。
//
// 组是授权主体之一：仓库访问控制可以把权限授给「用户」或「用户组」，组内成员自动获得
// 该组的权限。页面本身不展示权限，只维护「组 × 用户」的成员关系。
import {
  Badge,
  Box,
  Button,
  Group,
  Modal,
  Select,
  Stack,
  Table,
  Text,
  TextInput,
  Textarea,
} from "@mantine/core";
import { useForm } from "@mantine/form";
import { useDisclosure, useMediaQuery } from "@mantine/hooks";
import { IconPlus, IconTrash, IconUserPlus, IconUsers, IconUsersGroup } from "@tabler/icons-react";
import { EmptyState } from "@jianartifact/ui";
import { useEffect, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";

import { PageShell } from "../app/PageShell";
import { AsyncBoundary } from "../components/AsyncBoundary";
import { OpsHelpButton, OpsKpiBand } from "../components/ops/OpsKit";
import {
  addUserGroupMember,
  createUserGroup,
  deleteUserGroup,
  listUserGroupMembers,
  listUserGroups,
  listUsers,
  removeUserGroupMember,
  updateUserGroup,
} from "../api/endpoints";
import type { User, UserGroup, UserGroupMember } from "../api/types";
import { useAsync, invalidateAsyncCache } from "../hooks/useAsync";
import { confirmDanger, notifyError, notifySuccess } from "../lib/feedback";
import { formatStamp } from "../lib/format";

/** FR-66 内置匿名主体用户名（与 UsersPage 口径一致）。 */
const ANONYMOUS_USERNAME = "anonymous";

/** 一行组数据：契约的 UserGroup 不带成员数，故与成员列表并在一起持有。 */
interface GroupRow {
  group: UserGroup;
  members: UserGroupMember[];
}

/** 新建 / 编辑表单字段；编辑时留空表示不改该字段。 */
interface GroupFormValues {
  name: string;
  description: string;
}

export function UserGroupsPage() {
  const { t } = useTranslation();
  // 并行拉取：组列表 + 用户列表（成员候选），再逐组并发取成员列表。
  //
  // 契约的 UserGroup 不带成员数，逐组再取一次就是为了补上这一列；拿到的同一份成员关系
  // 也直接喂给成员弹窗，打开弹窗不必再拉一次。
  const state = useAsync(
    () =>
      Promise.all([listUserGroups({ page_size: 100 }), listUsers({ page_size: 100 })]).then(
        ([groups, users]) =>
          Promise.all(
            groups.items.map((group) =>
              listUserGroupMembers(group.id).then((list) => ({ group, members: list.items })),
            ),
          ).then((rows) => ({ users, rows })),
      ),
    [],
    { cacheKey: "user-groups:list" },
  );

  // 拉到的列表落到本地可变状态：增/删成员只改对应行，避免「移出一个成员 → 整表重拉」
  // 把弹窗与滚动位置一起重置。列表级变更（新建/改名/删组）仍走 state.reload()。
  const [rows, setRows] = useState<GroupRow[]>([]);
  const [users, setUsers] = useState<User[]>([]);
  const [ready, setReady] = useState(false);

  const [createOpened, createModal] = useDisclosure(false);
  const [creating, setCreating] = useState(false);
  const [editGroup, setEditGroup] = useState<UserGroup | null>(null);
  const [savingEdit, setSavingEdit] = useState(false);
  // 成员弹窗：只存组 id，成员与名称从本地 rows 里取。
  const [memberGroupId, setMemberGroupId] = useState<number | null>(null);
  const [newMemberId, setNewMemberId] = useState<string | null>(null);
  const [addingMember, setAddingMember] = useState(false);
  // 窄屏（< 48em）：4 列在手机上会把操作按钮挤到换行，此时合并成两列。
  const isNarrow = useMediaQuery("(max-width: 48em)") ?? false;

  useEffect(() => {
    if (state.data) {
      setRows(state.data.rows);
      setUsers(state.data.users.items);
      setReady(true);
    }
  }, [state.data]);

  const createForm = useForm<GroupFormValues>({
    initialValues: { name: "", description: "" },
    validate: { name: (v) => (v.trim() ? null : t("userGroups.name")) },
  });
  const editForm = useForm<GroupFormValues>({
    initialValues: { name: "", description: "" },
    validate: { name: (v) => (v.trim() ? null : t("userGroups.name")) },
  });

  // 打开编辑弹窗时把当前值灌进表单：留空表示不改，故初始值直接取原值。
  // 在点击处赋值而不是 useEffect：表单初值只依赖「点了哪个组」，放进 effect 反而要在
  // 依赖里带上 form 对象，多一轮渲染且容易覆盖用户正在输入的内容。
  const openEdit = (group: UserGroup) => {
    setEditGroup(group);
    editForm.setValues({ name: group.name, description: group.description });
  };

  const memberRow = rows.find((row) => row.group.id === memberGroupId) ?? null;
  const members = memberRow?.members ?? [];

  /** 候选用户：已在组内的不再出现，避免重复添加（后端会报冲突）。 */
  const candidateUsers = useMemo(() => {
    const joined = new Set(members.map((member) => member.userId));
    return users.filter((user) => !joined.has(user.id));
  }, [members, users]);

  /** 成员变更后就地更新对应行，不重拉整表。 */
  const patchMembers = (groupId: number, next: UserGroupMember[]) => {
    setRows((prev) =>
      prev.map((row) => (row.group.id === groupId ? { ...row, members: next } : row)),
    );
  };

  const handleCreate = createForm.onSubmit((values) => {
    setCreating(true);
    createUserGroup({ name: values.name.trim(), description: values.description.trim() })
      .then(() => {
        createModal.close();
        createForm.reset();
        notifySuccess(t("common.created"));
        // 组列表变了就失效缓存，否则回到本页会回放旧列表。
        invalidateAsyncCache("user-groups:list");
        state.reload();
      })
      .catch(notifyError)
      .finally(() => setCreating(false));
  });

  const handleEdit = editForm.onSubmit((values) => {
    if (!editGroup) return;
    setSavingEdit(true);
    updateUserGroup(editGroup.id, {
      name: values.name.trim(),
      description: values.description.trim(),
    })
      .then((updated) => {
        setEditGroup(null);
        // 先就地改名，再后台重拉对齐成员：弹窗关闭后不会闪回旧名字。
        setRows((prev) =>
          prev.map((row) => (row.group.id === updated.id ? { ...row, group: updated } : row)),
        );
        notifySuccess(t("common.updated"));
        invalidateAsyncCache("user-groups:list");
        state.reload();
      })
      .catch(notifyError)
      .finally(() => setSavingEdit(false));
  });

  const handleDelete = (group: UserGroup) => {
    confirmDanger({
      title: t("common.delete"),
      message: t("userGroups.deleteConfirm"),
      confirmLabel: t("common.delete"),
      cancelLabel: t("common.cancel"),
      onConfirm: () => {
        deleteUserGroup(group.id)
          .then(() => {
            // 先本地移除：整表重拉前列表就不再显示它，避免「删了还看见」的一帧。
            setRows((prev) => prev.filter((row) => row.group.id !== group.id));
            notifySuccess(t("common.deleted"));
            invalidateAsyncCache("user-groups:list");
            state.reload();
          })
          .catch(notifyError);
      },
    });
  };

  const handleAddMember = () => {
    if (!memberGroupId || !newMemberId) return;
    const groupId = memberGroupId;
    setAddingMember(true);
    addUserGroupMember(groupId, { userId: Number(newMemberId) })
      .then((member) => {
        const row = rows.find((item) => item.group.id === groupId);
        patchMembers(groupId, [...(row?.members ?? []), member]);
        setNewMemberId(null);
        notifySuccess(t("common.updated"));
      })
      .catch(notifyError)
      .finally(() => setAddingMember(false));
  };

  const handleRemoveMember = (member: UserGroupMember) => {
    if (!memberGroupId) return;
    const groupId = memberGroupId;
    confirmDanger({
      title: t("userGroups.removeMember"),
      message: t("userGroups.removeMemberConfirm"),
      confirmLabel: t("userGroups.removeMember"),
      cancelLabel: t("common.cancel"),
      onConfirm: () => {
        removeUserGroupMember(groupId, member.userId)
          .then(() => {
            const row = rows.find((item) => item.group.id === groupId);
            patchMembers(
              groupId,
              (row?.members ?? []).filter((item) => item.userId !== member.userId),
            );
            notifySuccess(t("common.updated"));
          })
          .catch(notifyError);
      },
    });
  };

  const memberTotal = rows.reduce((sum, row) => sum + row.members.length, 0);
  const emptyGroups = rows.filter((row) => row.members.length === 0).length;

  return (
    <>
      <PageShell>
        <Stack gap="md" style={{ flex: 1, minHeight: 0 }}>
          <OpsKpiBand
            variant="strip"
            label={t("userGroups.summaryLabel")}
            cols={{ base: 2, sm: 3 }}
            items={[
              {
                label: t("userGroups.summaryTotal"),
                value: ready ? String(rows.length) : "—",
                icon: <IconUsersGroup size={16} />,
                tone: "blue",
              },
              {
                label: t("userGroups.summaryMembers"),
                value: ready ? String(memberTotal) : "—",
                icon: <IconUsers size={16} />,
                tone: "indigo",
              },
              {
                label: t("userGroups.summaryEmptyGroups"),
                value: ready ? String(emptyGroups) : "—",
                icon: <IconUserPlus size={16} />,
                tone: "gray",
                hint: t("userGroups.summaryEmptyGroupsHint"),
              },
            ]}
            actions={
              <>
                <OpsHelpButton
                  title={t("userGroups.helpTitle")}
                  items={[
                    { label: t("userGroups.helpGroup"), value: t("userGroups.helpGroupHint") },
                    { label: t("userGroups.helpMember"), value: t("userGroups.helpMemberHint") },
                    { label: t("userGroups.helpDelete"), value: t("userGroups.helpDeleteHint") },
                  ]}
                />
                <Button leftSection={<IconPlus size={14} />} onClick={createModal.open}>
                  {t("userGroups.create")}
                </Button>
              </>
            }
          />

          <Box style={{ flex: 1, minHeight: 0, overflowY: "auto" }}>
            <AsyncBoundary state={state}>
              {() =>
                rows.length === 0 ? (
                  <EmptyState message={t("userGroups.empty")} />
                ) : (
                  <Table
                    striped={!isNarrow}
                    withRowBorders
                    highlightOnHover
                    stickyHeader
                    verticalSpacing={isNarrow ? "md" : "xs"}
                  >
                    <Table.Thead>
                      <Table.Tr>
                        {isNarrow ? null : <Table.Th>{t("userGroups.name")}</Table.Th>}
                        <Table.Th>{t("userGroups.memberCount")}</Table.Th>
                        {isNarrow ? null : <Table.Th>{t("userGroups.createdAt")}</Table.Th>}
                        {isNarrow ? null : <Table.Th>{t("common.actions")}</Table.Th>}
                      </Table.Tr>
                    </Table.Thead>
                    <Table.Tbody>
                      {rows.map(({ group, members: groupMembers }) => {
                        const rowActions = (
                          <Group gap={isNarrow ? 8 : 4} wrap="wrap">
                            <Button
                              size={isNarrow ? "xs" : "compact-xs"}
                              variant="subtle"
                              leftSection={<IconUsers size={14} />}
                              aria-label={t("userGroups.members")}
                              onClick={() => setMemberGroupId(group.id)}
                            >
                              {t("userGroups.members")}
                            </Button>
                            <Button
                              size={isNarrow ? "xs" : "compact-xs"}
                              variant="subtle"
                              aria-label={t("common.edit")}
                              onClick={() => openEdit(group)}
                            >
                              {t("common.edit")}
                            </Button>
                            <Button
                              size={isNarrow ? "xs" : "compact-xs"}
                              variant="subtle"
                              color="red"
                              leftSection={<IconTrash size={14} />}
                              aria-label={t("common.delete")}
                              onClick={() => handleDelete(group)}
                            >
                              {t("common.delete")}
                            </Button>
                          </Group>
                        );
                        return (
                          <Table.Tr key={group.id}>
                            {isNarrow ? null : (
                              <Table.Td>
                                <Stack gap={2}>
                                  <Text size="sm" truncate>
                                    {group.name}
                                  </Text>
                                  {group.description ? (
                                    <Text size="xs" c="dimmed" truncate>
                                      {group.description}
                                    </Text>
                                  ) : null}
                                </Stack>
                              </Table.Td>
                            )}
                            <Table.Td>
                              {isNarrow ? (
                                // 窄屏：组名与说明合并进成员列，操作按钮挪到同一格下方。
                                <Stack gap={10}>
                                  <Text size="sm" fw={600} truncate>
                                    {group.name}
                                  </Text>
                                  <Text size="xs" c="dimmed" truncate>
                                    {t("userGroups.memberCount")}：{groupMembers.length}
                                    {group.description ? ` · ${group.description}` : ""}
                                  </Text>
                                  {rowActions}
                                </Stack>
                              ) : (
                                String(groupMembers.length)
                              )}
                            </Table.Td>
                            {isNarrow ? null : <Table.Td>{formatStamp(group.createdAt)}</Table.Td>}
                            {isNarrow ? null : <Table.Td>{rowActions}</Table.Td>}
                          </Table.Tr>
                        );
                      })}
                    </Table.Tbody>
                  </Table>
                )
              }
            </AsyncBoundary>
          </Box>
        </Stack>
      </PageShell>

      <Modal opened={createOpened} onClose={createModal.close} title={t("userGroups.create")}>
        <form onSubmit={handleCreate}>
          <TextInput
            label={t("userGroups.name")}
            placeholder={t("userGroups.namePlaceholder")}
            withAsterisk
            {...createForm.getInputProps("name")}
          />
          <Textarea
            mt="sm"
            label={t("userGroups.descriptionField")}
            placeholder={t("userGroups.descriptionPlaceholder")}
            {...createForm.getInputProps("description")}
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

      <Modal
        opened={editGroup !== null}
        onClose={() => setEditGroup(null)}
        title={t("userGroups.edit")}
      >
        <form onSubmit={handleEdit}>
          <TextInput
            label={t("userGroups.name")}
            withAsterisk
            {...editForm.getInputProps("name")}
          />
          <Textarea
            mt="sm"
            label={t("userGroups.descriptionField")}
            {...editForm.getInputProps("description")}
          />
          <Text size="xs" c="dimmed" mt={4}>
            {t("userGroups.editKeepHint")}
          </Text>
          <Group justify="flex-end" mt="md">
            <Button variant="default" onClick={() => setEditGroup(null)}>
              {t("common.cancel")}
            </Button>
            <Button type="submit" loading={savingEdit}>
              {t("common.save")}
            </Button>
          </Group>
        </form>
      </Modal>

      <Modal
        opened={memberGroupId !== null}
        onClose={() => setMemberGroupId(null)}
        title={
          memberRow
            ? t("userGroups.membersOf", { name: memberRow.group.name })
            : t("userGroups.members")
        }
        size="lg"
      >
        <Stack gap="sm">
          {members.length === 0 ? (
            <EmptyState message={t("userGroups.memberEmpty")} />
          ) : (
            <Table withRowBorders>
              <Table.Thead>
                <Table.Tr>
                  <Table.Th>{t("userGroups.memberUsername")}</Table.Th>
                  <Table.Th>{t("userGroups.memberJoinedAt")}</Table.Th>
                  <Table.Th>{t("common.actions")}</Table.Th>
                </Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {members.map((member) => (
                  <Table.Tr key={member.userId}>
                    <Table.Td>
                      <Group gap="xs" wrap="nowrap">
                        {member.username}
                        {member.username === ANONYMOUS_USERNAME ? (
                          <Badge variant="light" color="gray">
                            {t("common.anonymous")}
                          </Badge>
                        ) : null}
                      </Group>
                    </Table.Td>
                    <Table.Td>{formatStamp(member.createdAt)}</Table.Td>
                    <Table.Td>
                      <Button
                        size="compact-xs"
                        variant="subtle"
                        color="red"
                        leftSection={<IconTrash size={14} />}
                        aria-label={t("userGroups.removeMember")}
                        onClick={() => handleRemoveMember(member)}
                      >
                        {t("userGroups.removeMember")}
                      </Button>
                    </Table.Td>
                  </Table.Tr>
                ))}
              </Table.Tbody>
            </Table>
          )}

          <Group align="flex-end" gap="sm">
            <Select
              label={t("userGroups.addMember")}
              placeholder={t("userGroups.memberPlaceholder")}
              data={candidateUsers.map((user) => ({
                value: String(user.id),
                label: user.username,
              }))}
              value={newMemberId}
              onChange={setNewMemberId}
              searchable
              w={240}
              nothingFoundMessage={t("userGroups.memberNoCandidate")}
            />
            <Button
              variant="light"
              onClick={handleAddMember}
              disabled={!newMemberId}
              loading={addingMember}
              leftSection={<IconUserPlus size={14} />}
            >
              {t("userGroups.addMember")}
            </Button>
          </Group>
        </Stack>
      </Modal>
    </>
  );
}
