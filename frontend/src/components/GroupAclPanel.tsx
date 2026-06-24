// 每仓库「组 ACL」管理面板（FR-49 / FR-50，仅管理员）：对用户组授予 / 撤销四级动作授权。
// 组 ACL 条目仅含 group_id，故拉取用户组列表把 group_id 解析为组名展示。

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
import type { GroupAclView, GroupView, Permission } from '../api/types';
import { errorMessage } from '../lib/format';
import { PERMISSION_OPTIONS, permissionColor, permissionLabel } from '../lib/permissions';
import { notifyError, notifySuccess } from '../lib/notify';
import { ErrorAlert } from './ErrorAlert';

/** 组 ACL 管理面板。 */
export function GroupAclPanel({ repoId }: { repoId: string }) {
  const [acls, setAcls] = useState<GroupAclView[]>([]);
  const [groups, setGroups] = useState<GroupView[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [selectedGroup, setSelectedGroup] = useState<string | null>(null);
  const [permission, setPermission] = useState<Permission>('read');
  const [submitting, setSubmitting] = useState(false);

  const reload = () => {
    setLoading(true);
    Promise.all([api.listGroupAcl(repoId), api.listGroups()])
      .then(([aclList, groupList]) => {
        setAcls(aclList);
        setGroups(groupList);
      })
      .catch((err) => setError(errorMessage(err)))
      .finally(() => setLoading(false));
  };

  useEffect(reload, [repoId]);

  const groupName = (id: string) => groups.find((g) => g.id === id)?.name ?? id;

  const handleAdd = async () => {
    if (!selectedGroup) return;
    setSubmitting(true);
    try {
      await api.createGroupAcl(repoId, selectedGroup, permission);
      notifySuccess('已对组新增授权');
      setSelectedGroup(null);
      reload();
    } catch (err) {
      notifyError(errorMessage(err));
    } finally {
      setSubmitting(false);
    }
  };

  const handleRemove = async (aclId: string) => {
    try {
      await api.deleteGroupAcl(repoId, aclId);
      notifySuccess('已撤销组授权');
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
          label="用户组"
          placeholder="选择用户组"
          data={groups.map((g) => ({ value: g.id, label: g.name }))}
          value={selectedGroup}
          onChange={setSelectedGroup}
          searchable
          maw={240}
        />
        <Select
          label="权限"
          data={PERMISSION_OPTIONS}
          value={permission}
          onChange={(v) => setPermission((v as Permission) ?? 'read')}
          allowDeselect={false}
          maw={160}
        />
        <Button onClick={handleAdd} loading={submitting} disabled={!selectedGroup}>
          授权
        </Button>
      </Group>

      {acls.length === 0 ? (
        <Text c="dimmed">该仓库暂无组 ACL 授权条目。</Text>
      ) : (
        <Table striped>
          <Table.Thead>
            <Table.Tr>
              <Table.Th>用户组</Table.Th>
              <Table.Th>权限</Table.Th>
              <Table.Th>操作</Table.Th>
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {acls.map((acl) => (
              <Table.Tr key={acl.id}>
                <Table.Td>{groupName(acl.group_id)}</Table.Td>
                <Table.Td>
                  <Badge variant="light" color={permissionColor(acl.permission)}>
                    {permissionLabel(acl.permission)}
                  </Badge>
                </Table.Td>
                <Table.Td>
                  <ActionIcon
                    variant="subtle"
                    color="red"
                    onClick={() => handleRemove(acl.id)}
                    aria-label="撤销组授权"
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
