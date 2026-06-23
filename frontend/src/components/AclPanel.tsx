// 每仓库 ACL 管理面板（FR-20，仅管理员）：列出 / 新增 / 移除读写授权。
// ACL 条目仅含 user_id，故拉取用户列表把 user_id 解析为用户名展示。

import { useEffect, useState } from 'react';
import {
  Table,
  Group,
  Stack,
  Select,
  Button,
  ActionIcon,
  Badge,
  Text,
  Loader,
  Center,
} from '@mantine/core';
import { IconTrash } from '@tabler/icons-react';
import * as api from '../api/endpoints';
import type { AclDto, Permission, UserView } from '../api/types';
import { errorMessage } from '../lib/format';
import { notifyError, notifySuccess } from '../lib/notify';
import { ErrorAlert } from './ErrorAlert';

/** ACL 管理面板。 */
export function AclPanel({ repoId }: { repoId: string }) {
  const [acls, setAcls] = useState<AclDto[]>([]);
  const [users, setUsers] = useState<UserView[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [selectedUser, setSelectedUser] = useState<string | null>(null);
  const [permission, setPermission] = useState<Permission>('read');
  const [submitting, setSubmitting] = useState(false);

  const reload = () => {
    setLoading(true);
    Promise.all([api.listAcl(repoId), api.listUsers()])
      .then(([aclList, userList]) => {
        setAcls(aclList);
        setUsers(userList);
      })
      .catch((err) => setError(errorMessage(err)))
      .finally(() => setLoading(false));
  };

  useEffect(reload, [repoId]);

  const userName = (id: string) => users.find((u) => u.id === id)?.username ?? id;

  const handleAdd = async () => {
    if (!selectedUser) return;
    setSubmitting(true);
    try {
      await api.createAcl(repoId, selectedUser, permission);
      notifySuccess('已新增授权');
      setSelectedUser(null);
      reload();
    } catch (err) {
      notifyError(errorMessage(err));
    } finally {
      setSubmitting(false);
    }
  };

  const handleRemove = async (aclId: string) => {
    try {
      await api.deleteAcl(repoId, aclId);
      notifySuccess('已移除授权');
      reload();
    } catch (err) {
      notifyError(errorMessage(err));
    }
  };

  if (loading) {
    return (
      <Center h={120}>
        <Loader />
      </Center>
    );
  }
  if (error) return <ErrorAlert message={error} />;

  return (
    <Stack>
      <Group align="flex-end">
        <Select
          label="用户"
          placeholder="选择用户"
          data={users.map((u) => ({ value: u.id, label: u.username }))}
          value={selectedUser}
          onChange={setSelectedUser}
          searchable
          maw={240}
        />
        <Select
          label="权限"
          data={[
            { value: 'read', label: '读（read）' },
            { value: 'write', label: '写（write）' },
          ]}
          value={permission}
          onChange={(v) => setPermission((v as Permission) ?? 'read')}
          allowDeselect={false}
          maw={160}
        />
        <Button onClick={handleAdd} loading={submitting} disabled={!selectedUser}>
          授权
        </Button>
      </Group>

      {acls.length === 0 ? (
        <Text c="dimmed">该仓库暂无 ACL 授权条目。</Text>
      ) : (
        <Table striped>
          <Table.Thead>
            <Table.Tr>
              <Table.Th>用户</Table.Th>
              <Table.Th>权限</Table.Th>
              <Table.Th>操作</Table.Th>
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {acls.map((acl) => (
              <Table.Tr key={acl.id}>
                <Table.Td>{userName(acl.user_id)}</Table.Td>
                <Table.Td>
                  <Badge variant="light" color={acl.permission === 'write' ? 'orange' : 'blue'}>
                    {acl.permission === 'write' ? '写' : '读'}
                  </Badge>
                </Table.Td>
                <Table.Td>
                  <ActionIcon
                    variant="subtle"
                    color="red"
                    onClick={() => handleRemove(acl.id)}
                    aria-label="移除授权"
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
  );
}
