// 仓库访问控制：编辑某仓库的 ACL 条目（用户 ID + 权限），整表 PUT 保存。
import { ActionIcon, Button, Group, NumberInput, Select, Table } from "@mantine/core";
import { IconTrash } from "@tabler/icons-react";
import { EmptyState, PageHeader } from "@jianartifact/ui";
import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { useNavigate, useParams } from "react-router-dom";

import { AsyncBoundary } from "../components/AsyncBoundary";
import { getAcl, setAcl } from "../api/endpoints";
import type { AclAction, AclEntry } from "../api/types";
import { useAsync } from "../hooks/useAsync";
import { notifyError, notifySuccess } from "../lib/feedback";

export function AclPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const { name = "" } = useParams();
  const state = useAsync(() => getAcl(name), [name]);
  const [entries, setEntries] = useState<AclEntry[]>([]);
  const [saving, setSaving] = useState(false);

  useEffect(() => {
    if (state.data) {
      setEntries(state.data.items);
    }
  }, [state.data]);

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
    setEntries((prev) => [...prev, { subjectId: 0, action: "read" }]);
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
                    <Table.Th>{t("acl.subjectId")}</Table.Th>
                    <Table.Th>{t("acl.action")}</Table.Th>
                    <Table.Th>{t("common.actions")}</Table.Th>
                  </Table.Tr>
                </Table.Thead>
                <Table.Tbody>
                  {entries.map((entry, index) => (
                    <Table.Tr key={index}>
                      <Table.Td>
                        <NumberInput
                          w={140}
                          min={1}
                          value={entry.subjectId}
                          onChange={(v) => updateEntry(index, { subjectId: Number(v) || 0 })}
                        />
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
                        <ActionIcon color="red" variant="subtle" onClick={() => removeEntry(index)} aria-label={t("common.delete")}>
                          <IconTrash size={16} />
                        </ActionIcon>
                      </Table.Td>
                    </Table.Tr>
                  ))}
                </Table.Tbody>
              </Table>
            )}
            <Button mt="md" variant="light" onClick={addEntry}>
              {t("acl.addEntry")}
            </Button>
          </>
        )}
      </AsyncBoundary>
    </>
  );
}
