// 仓库访问控制：编辑某仓库的 ACL 条目（用户 + 权限），整表 PUT 保存。
import { ActionIcon, Button, Group, Select, Table } from "@mantine/core";
import { IconTrash } from "@tabler/icons-react";
import { EmptyState, PageHeader } from "@jianartifact/ui";
import { useEffect, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { useNavigate, useParams } from "react-router-dom";

import { AsyncBoundary } from "../components/AsyncBoundary";
import { getAcl, listUsers, setAcl } from "../api/endpoints";
import type { AclAction, AclEntry, User } from "../api/types";
import { useAsync } from "../hooks/useAsync";
import { notifyError, notifySuccess } from "../lib/feedback";

export function AclPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const { name = "" } = useParams();

  // 并行拉取 ACL 条目与用户列表（page_size: 100 足够覆盖常见规模）。
  const state = useAsync(
    () =>
      Promise.all([getAcl(name), listUsers({ page_size: 100 })]).then(([acl, users]) => ({
        acl,
        users,
      })),
    [name],
  );

  const [entries, setEntries] = useState<AclEntry[]>([]);
  const [users, setUsers] = useState<User[]>([]);
  const [saving, setSaving] = useState(false);

  // 新增条目输入：用户 Select + 权限 Select，点击添加才落入 entries。
  const [newSubjectId, setNewSubjectId] = useState<string | null>(null);
  const [newAction, setNewAction] = useState<AclAction>("read");

  useEffect(() => {
    if (state.data) {
      setEntries(state.data.acl.items);
      setUsers(state.data.users.items);
    }
  }, [state.data]);

  // id → username 映射；列表中用用户名展示，映射缺失时回退到 id。
  const nameById = useMemo(() => {
    const m = new Map<number, string>();
    for (const u of users) m.set(u.id, u.username);
    return m;
  }, [users]);

  // 用户下拉选项：值用字符串 id（Mantine Select 统一字符串），提交时转回 number。
  const userOptions = useMemo(
    () => users.map((u) => ({ value: String(u.id), label: u.username })),
    [users],
  );

  // 已添加条目中已被占用的用户 id，新增时从下拉里排除，避免重复授权。
  const usedIds = useMemo(() => new Set(entries.map((e) => e.subjectId)), [entries]);
  const availableUserOptions = useMemo(
    () => userOptions.filter((o) => !usedIds.has(Number(o.value))),
    [userOptions, usedIds],
  );

  const actionOptions = [
    { value: "read", label: t("acl.actionRead") },
    { value: "write", label: t("acl.actionWrite") },
    { value: "admin", label: t("acl.actionAdmin") },
  ];

  const updateEntry = (index: number, patch: Partial<AclEntry>) => {
    setEntries((prev) => prev.map((e, i) => (i === index ? { ...e, ...patch } : e)));
  };

  const removeEntry = (index: number) => {
    setEntries((prev) => prev.filter((_, i) => i !== index));
  };

  const addEntry = () => {
    if (!newSubjectId) return;
    setEntries((prev) => [...prev, { subjectId: Number(newSubjectId), action: newAction }]);
    setNewSubjectId(null);
    setNewAction("read");
  };

  const handleSave = () => {
    setSaving(true);
    setAcl(name, entries)
      .then(() => {
        notifySuccess(t("common.saved"));
      })
      .catch(notifyError)
      .finally(() => {
        setSaving(false);
      });
  };

  return (
    <>
      <PageHeader
        title={`${t("acl.title")} · ${name}`}
        actions={
          <Group gap="xs">
            <Button variant="default" onClick={() => navigate("/repositories")}>
              {t("common.close")}
            </Button>
            <Button onClick={handleSave} loading={saving}>
              {t("acl.save")}
            </Button>
          </Group>
        }
      />

      <AsyncBoundary state={state}>
        {() => (
          <>
            {entries.length === 0 ? (
              <EmptyState message={t("acl.empty")} />
            ) : (
              <Table>
                <Table.Thead>
                  <Table.Tr>
                    <Table.Th>{t("acl.user")}</Table.Th>
                    <Table.Th>{t("acl.action")}</Table.Th>
                    <Table.Th>{t("common.actions")}</Table.Th>
                  </Table.Tr>
                </Table.Thead>
                <Table.Tbody>
                  {entries.map((entry, index) => (
                    <Table.Tr key={entry.subjectId}>
                      <Table.Td>
                        {/* 展示用户名；若用户列表中查不到（如已删除）则回退显示 id */}
                        {nameById.get(entry.subjectId) ?? `#${entry.subjectId}`}
                      </Table.Td>
                      <Table.Td>
                        <Select
                          w={140}
                          data={actionOptions}
                          allowDeselect={false}
                          value={entry.action}
                          onChange={(v) => v && updateEntry(index, { action: v as AclAction })}
                        />
                      </Table.Td>
                      <Table.Td>
                        <ActionIcon
                          color="red"
                          variant="subtle"
                          onClick={() => removeEntry(index)}
                          aria-label={t("common.delete")}
                        >
                          <IconTrash size={16} />
                        </ActionIcon>
                      </Table.Td>
                    </Table.Tr>
                  ))}
                </Table.Tbody>
              </Table>
            )}

            {/* 新增条目行：选用户 + 选权限 + 添加按钮 */}
            <Group mt="md" align="flex-end" gap="sm">
              <Select
                label={t("acl.user")}
                placeholder={t("acl.userPlaceholder")}
                data={availableUserOptions}
                value={newSubjectId}
                onChange={setNewSubjectId}
                searchable
                w={240}
                nothingFoundMessage={t("common.empty")}
              />
              <Select
                label={t("acl.action")}
                data={actionOptions}
                allowDeselect={false}
                value={newAction}
                onChange={(v) => v && setNewAction(v as AclAction)}
                w={140}
              />
              <Button variant="light" onClick={addEntry} disabled={!newSubjectId}>
                {t("acl.addEntry")}
              </Button>
            </Group>
          </>
        )}
      </AsyncBoundary>
    </>
  );
}
