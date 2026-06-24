// 用户组管理界面（FR-49 / FR-50，仅管理员）：建组 / 删组、加移成员。
// 对组授予仓库 ACL 在「仓库详情 → 权限」页签内完成（见 GroupAclPanel）。

import { useEffect, useState } from 'react';
import {
  Table,
  Button,
  Group,
  Title,
  Stack,
  Modal,
  TextInput,
  Select,
  ActionIcon,
  Text,
  Loader,
  Center,
  Badge,
} from '@mantine/core';
import { useDisclosure } from '@mantine/hooks';
import { IconPlus, IconTrash, IconUsersGroup } from '@tabler/icons-react';
import * as api from '../api/endpoints';
import type { GroupMemberView, GroupView, UserView } from '../api/types';
import { errorMessage } from '../lib/format';
import { notifyError, notifySuccess } from '../lib/notify';
import { ErrorAlert } from '../components/ErrorAlert';

/** 用户组管理页面。 */
export function GroupsPage() {
  const [groups, setGroups] = useState<GroupView[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [createOpened, createModal] = useDisclosure(false);
  const [membersOf, setMembersOf] = useState<GroupView | null>(null);

  const reload = () => {
    setLoading(true);
    api
      .listGroups()
      .then(setGroups)
      .catch((err) => setError(errorMessage(err)))
      .finally(() => setLoading(false));
  };

  useEffect(reload, []);

  const handleDelete = async (group: GroupView) => {
    if (!window.confirm(`确认删除用户组「${group.name}」？将同时清理其成员与组授权。`)) return;
    try {
      await api.deleteGroup(group.id);
      notifySuccess('用户组已删除');
      reload();
    } catch (err) {
      notifyError(errorMessage(err));
    }
  };

  if (loading) {
    return (
      <Center h={200}>
        <Loader />
      </Center>
    );
  }

  return (
    <Stack>
      <Group justify="space-between">
        <Title order={2}>用户组管理</Title>
        <Button leftSection={<IconPlus size={16} />} onClick={createModal.open}>
          新增用户组
        </Button>
      </Group>
      {error && <ErrorAlert message={error} />}

      {groups.length === 0 ? (
        <Text c="dimmed">暂无用户组。</Text>
      ) : (
        <Table.ScrollContainer minWidth={520}>
          <Table striped highlightOnHover>
            <Table.Thead>
              <Table.Tr>
                <Table.Th>组名</Table.Th>
                <Table.Th>创建时间</Table.Th>
                <Table.Th>操作</Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {groups.map((g) => (
                <Table.Tr key={g.id}>
                  <Table.Td>{g.name}</Table.Td>
                  <Table.Td>
                    <Text size="sm" c="dimmed">
                      {g.created_at}
                    </Text>
                  </Table.Td>
                  <Table.Td>
                    <Group gap="xs">
                      <Button
                        size="xs"
                        variant="default"
                        leftSection={<IconUsersGroup size={14} />}
                        onClick={() => setMembersOf(g)}
                      >
                        成员
                      </Button>
                      <ActionIcon
                        variant="subtle"
                        color="red"
                        onClick={() => handleDelete(g)}
                        aria-label="删除用户组"
                      >
                        <IconTrash size={18} />
                      </ActionIcon>
                    </Group>
                  </Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        </Table.ScrollContainer>
      )}

      <CreateGroupModal
        opened={createOpened}
        onClose={createModal.close}
        onCreated={() => {
          createModal.close();
          reload();
        }}
      />
      <MembersModal group={membersOf} onClose={() => setMembersOf(null)} />
    </Stack>
  );
}

/** 新增用户组弹窗。 */
function CreateGroupModal({
  opened,
  onClose,
  onCreated,
}: {
  opened: boolean;
  onClose: () => void;
  onCreated: () => void;
}) {
  const [name, setName] = useState('');
  const [submitting, setSubmitting] = useState(false);

  const handleSubmit = async () => {
    setSubmitting(true);
    try {
      await api.createGroup(name);
      notifySuccess('用户组已创建');
      setName('');
      onCreated();
    } catch (err) {
      notifyError(errorMessage(err));
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <Modal opened={opened} onClose={onClose} title="新增用户组" centered>
      <Stack>
        <TextInput
          label="组名"
          placeholder="如 dev-team"
          value={name}
          onChange={(e) => setName(e.currentTarget.value)}
          required
        />
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>
            取消
          </Button>
          <Button onClick={handleSubmit} loading={submitting} disabled={!name}>
            创建
          </Button>
        </Group>
      </Stack>
    </Modal>
  );
}

/** 组成员管理弹窗：列出 / 加入 / 移出成员。 */
function MembersModal({ group, onClose }: { group: GroupView | null; onClose: () => void }) {
  const [members, setMembers] = useState<GroupMemberView[]>([]);
  const [users, setUsers] = useState<UserView[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [selectedUser, setSelectedUser] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);

  const groupId = group?.id ?? '';

  useEffect(() => {
    if (!group) return;
    setLoading(true);
    setError(null);
    Promise.all([api.listGroupMembers(group.id), api.listUsers()])
      .then(([memberList, userList]) => {
        setMembers(memberList);
        setUsers(userList);
      })
      .catch((err) => setError(errorMessage(err)))
      .finally(() => setLoading(false));
  }, [group]);

  const reloadMembers = async () => {
    try {
      setMembers(await api.listGroupMembers(groupId));
    } catch (err) {
      notifyError(errorMessage(err));
    }
  };

  const handleAdd = async () => {
    if (!selectedUser) return;
    setSubmitting(true);
    try {
      await api.addGroupMember(groupId, selectedUser);
      notifySuccess('已加入成员');
      setSelectedUser(null);
      await reloadMembers();
    } catch (err) {
      notifyError(errorMessage(err));
    } finally {
      setSubmitting(false);
    }
  };

  const handleRemove = async (userId: string) => {
    try {
      await api.removeGroupMember(groupId, userId);
      notifySuccess('已移出成员');
      await reloadMembers();
    } catch (err) {
      notifyError(errorMessage(err));
    }
  };

  // 候选用户：排除已是成员者，避免重复加入 409
  const candidates = users.filter((u) => !members.some((m) => m.user_id === u.id));

  return (
    <Modal
      opened={group !== null}
      onClose={onClose}
      title={group ? `「${group.name}」成员管理` : '成员管理'}
      centered
      size="lg"
    >
      {loading ? (
        <Center h={120}>
          <Loader />
        </Center>
      ) : error ? (
        <ErrorAlert message={error} />
      ) : (
        <Stack>
          <Group align="flex-end">
            <Select
              label="添加成员"
              placeholder="选择用户"
              data={candidates.map((u) => ({ value: u.id, label: u.username }))}
              value={selectedUser}
              onChange={setSelectedUser}
              searchable
              maw={260}
            />
            <Button onClick={handleAdd} loading={submitting} disabled={!selectedUser}>
              加入
            </Button>
          </Group>

          {members.length === 0 ? (
            <Text c="dimmed">该组暂无成员。</Text>
          ) : (
            <Table striped>
              <Table.Thead>
                <Table.Tr>
                  <Table.Th>成员</Table.Th>
                  <Table.Th>操作</Table.Th>
                </Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {members.map((m) => (
                  <Table.Tr key={m.user_id}>
                    <Table.Td>
                      <Badge variant="light">{m.username}</Badge>
                    </Table.Td>
                    <Table.Td>
                      <ActionIcon
                        variant="subtle"
                        color="red"
                        onClick={() => handleRemove(m.user_id)}
                        aria-label="移出成员"
                      >
                        <IconTrash size={18} />
                      </ActionIcon>
                    </Table.Td>
                  </Table.Tr>
                ))}
              </Table.Tbody>
            </Table>
          )}
        </Stack>
      )}
    </Modal>
  );
}
