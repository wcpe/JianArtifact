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
import { useTranslation } from 'react-i18next';
import * as api from '../api/endpoints';
import type { Role, UserView } from '../api/types';
import { errorMessage } from '../lib/format';
import { notifyError, notifySuccess } from '../lib/notify';
import { ErrorAlert } from '../components/ErrorAlert';

/** 用户管理页面。 */
export function UsersPage() {
  const { t } = useTranslation('users');
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
      notifySuccess(t('roleUpdated'));
      reload();
    } catch (err) {
      notifyError(errorMessage(err));
    }
  };

  const handleToggleDisabled = async (user: UserView) => {
    try {
      await api.updateUser(user.id, { disabled: !user.disabled });
      notifySuccess(user.disabled ? t('userEnabled') : t('userDisabled'));
      reload();
    } catch (err) {
      notifyError(errorMessage(err));
    }
  };

  const handleDelete = async (user: UserView) => {
    if (!window.confirm(t('confirmDelete', { username: user.username }))) return;
    try {
      await api.deleteUser(user.id);
      notifySuccess(t('userDeleted'));
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
        <Title order={2}>{t('title')}</Title>
        <Button leftSection={<IconPlus size={16} />} onClick={modal.open}>
          {t('createUser')}
        </Button>
      </Group>
      {error && <ErrorAlert message={error} />}

      <Table.ScrollContainer minWidth={620}>
        <Table striped highlightOnHover>
          <Table.Thead>
            <Table.Tr>
              <Table.Th>{t('colUsername')}</Table.Th>
              <Table.Th>{t('colRole')}</Table.Th>
              <Table.Th>{t('colStatus')}</Table.Th>
              <Table.Th>{t('colCreatedAt')}</Table.Th>
              <Table.Th>{t('colActions')}</Table.Th>
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
                      { value: 'admin', label: t('common:roleAdmin') },
                      { value: 'user', label: t('common:roleUser') },
                    ]}
                    value={user.role}
                    onChange={(v) => v && handleRoleChange(user, v as Role)}
                    allowDeselect={false}
                  />
                </Table.Td>
                <Table.Td>
                  <Badge color={user.disabled ? 'red' : 'green'} variant="light">
                    {user.disabled ? t('statusDisabled') : t('statusNormal')}
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
                      {user.disabled ? t('enable') : t('disable')}
                    </Button>
                    <ActionIcon
                      variant="subtle"
                      color="red"
                      onClick={() => handleDelete(user)}
                      aria-label={t('deleteUserAria')}
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
  const { t } = useTranslation('users');
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
      notifySuccess(t('userCreated'));
      reset();
      onCreated();
    } catch (err) {
      notifyError(errorMessage(err));
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <Modal opened={opened} onClose={onClose} title={t('createModalTitle')} centered>
      <Stack>
        <TextInput
          label={t('fieldUsername')}
          value={username}
          onChange={(e) => setUsername(e.currentTarget.value)}
          required
        />
        <PasswordInput
          label={t('fieldPassword')}
          value={password}
          onChange={(e) => setPassword(e.currentTarget.value)}
          required
        />
        <Select
          label={t('fieldRole')}
          data={[
            { value: 'user', label: t('common:roleUser') },
            { value: 'admin', label: t('common:roleAdmin') },
          ]}
          value={role}
          onChange={(v) => setRole((v as Role) ?? 'user')}
          allowDeselect={false}
        />
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>
            {t('common:cancel')}
          </Button>
          <Button onClick={handleSubmit} loading={submitting} disabled={!username || !password}>
            {t('common:create')}
          </Button>
        </Group>
      </Stack>
    </Modal>
  );
}
