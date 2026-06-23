// 用户管理界面（FR-20，仅管理员）：列出 / 新增 / 改角色 / 启用禁用 / 删除用户。

import { useEffect, useState } from 'react';
import {
  Table,
  Button,
  Group,
  Title,
  Stack,
  Badge,
  Modal,
  TextInput,
  PasswordInput,
  Select,
  ActionIcon,
  Text,
  Loader,
  Center,
} from '@mantine/core';
import { useDisclosure } from '@mantine/hooks';
import { IconPlus, IconTrash } from '@tabler/icons-react';
import * as api from '../api/endpoints';
import type { Role, UserView } from '../api/types';
import { errorMessage } from '../lib/format';
import { notifyError, notifySuccess } from '../lib/notify';
import { ErrorAlert } from '../components/ErrorAlert';

/** 用户管理页面。 */
export function UsersPage() {
  const [users, setUsers] = useState<UserView[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [modalOpened, modal] = useDisclosure(false);

  const reload = () => {
    setLoading(true);
    api
      .listUsers()
      .then(setUsers)
      .catch((err) => setError(errorMessage(err)))
      .finally(() => setLoading(false));
  };

  useEffect(reload, []);

  const handleRoleChange = async (user: UserView, role: Role) => {
    try {
      await api.updateUser(user.id, { role });
      notifySuccess('已更新角色');
      reload();
    } catch (err) {
      notifyError(errorMessage(err));
    }
  };

  const handleToggleDisabled = async (user: UserView) => {
    try {
      await api.updateUser(user.id, { disabled: !user.disabled });
      notifySuccess(user.disabled ? '已启用用户' : '已禁用用户');
      reload();
    } catch (err) {
      notifyError(errorMessage(err));
    }
  };

  const handleDelete = async (user: UserView) => {
    if (!window.confirm(`确认删除用户「${user.username}」？`)) return;
    try {
      await api.deleteUser(user.id);
      notifySuccess('用户已删除');
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
        <Title order={2}>用户管理</Title>
        <Button leftSection={<IconPlus size={16} />} onClick={modal.open}>
          新增用户
        </Button>
      </Group>
      {error && <ErrorAlert message={error} />}

      <Table.ScrollContainer minWidth={620}>
        <Table striped highlightOnHover>
          <Table.Thead>
            <Table.Tr>
              <Table.Th>用户名</Table.Th>
              <Table.Th>角色</Table.Th>
              <Table.Th>状态</Table.Th>
              <Table.Th>创建时间</Table.Th>
              <Table.Th>操作</Table.Th>
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {users.map((user) => (
              <Table.Tr key={user.id}>
                <Table.Td>{user.username}</Table.Td>
                <Table.Td>
                  <Select
                    size="xs"
                    maw={120}
                    data={[
                      { value: 'admin', label: '管理员' },
                      { value: 'user', label: '用户' },
                    ]}
                    value={user.role}
                    onChange={(v) => v && handleRoleChange(user, v as Role)}
                    allowDeselect={false}
                  />
                </Table.Td>
                <Table.Td>
                  <Badge color={user.disabled ? 'red' : 'green'} variant="light">
                    {user.disabled ? '已禁用' : '正常'}
                  </Badge>
                </Table.Td>
                <Table.Td>
                  <Text size="sm" c="dimmed">
                    {user.created_at}
                  </Text>
                </Table.Td>
                <Table.Td>
                  <Group gap="xs">
                    <Button size="xs" variant="default" onClick={() => handleToggleDisabled(user)}>
                      {user.disabled ? '启用' : '禁用'}
                    </Button>
                    <ActionIcon
                      variant="subtle"
                      color="red"
                      onClick={() => handleDelete(user)}
                      aria-label="删除用户"
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

      <CreateUserModal
        opened={modalOpened}
        onClose={modal.close}
        onCreated={() => {
          modal.close();
          reload();
        }}
      />
    </Stack>
  );
}

/** 新增用户弹窗。 */
function CreateUserModal({
  opened,
  onClose,
  onCreated,
}: {
  opened: boolean;
  onClose: () => void;
  onCreated: () => void;
}) {
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [role, setRole] = useState<Role>('user');
  const [submitting, setSubmitting] = useState(false);

  const reset = () => {
    setUsername('');
    setPassword('');
    setRole('user');
  };

  const handleSubmit = async () => {
    setSubmitting(true);
    try {
      await api.createUser({ username, password, role });
      notifySuccess('用户已创建');
      reset();
      onCreated();
    } catch (err) {
      notifyError(errorMessage(err));
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <Modal opened={opened} onClose={onClose} title="新增用户" centered>
      <Stack>
        <TextInput
          label="用户名"
          value={username}
          onChange={(e) => setUsername(e.currentTarget.value)}
          required
        />
        <PasswordInput
          label="口令"
          value={password}
          onChange={(e) => setPassword(e.currentTarget.value)}
          required
        />
        <Select
          label="角色"
          data={[
            { value: 'user', label: '用户' },
            { value: 'admin', label: '管理员' },
          ]}
          value={role}
          onChange={(v) => setRole((v as Role) ?? 'user')}
          allowDeselect={false}
        />
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>
            取消
          </Button>
          <Button onClick={handleSubmit} loading={submitting} disabled={!username || !password}>
            创建
          </Button>
        </Group>
      </Stack>
    </Modal>
  );
}
