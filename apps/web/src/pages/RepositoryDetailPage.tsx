// 仓库详情：Tab 布局（浏览/配置/ACL）。管理员可见配置与 ACL，普通用户仅浏览。
import {
  ActionIcon,
  Badge,
  Button,
  Card,
  Group,
  Select,
  Stack,
  Table,
  Tabs,
  Text,
  Title,
} from "@mantine/core";
import { IconTrash } from "@tabler/icons-react";
import { EmptyState, PageHeader } from "@jianartifact/ui";
import { useEffect, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { useNavigate, useParams } from "react-router-dom";

import { RepoBrowser } from "../components/repo/RepoBrowser";
import { AsyncBoundary } from "../components/AsyncBoundary";
import { getAcl, listRepositories, listUsers, setAcl, updateRepository } from "../api/endpoints";
import type { AclAction, AclEntry, RepoVisibility, Repository, User } from "../api/types";
import { useAuth } from "../auth/AuthContext";
import { useAsync } from "../hooks/useAsync";
import { notifyError, notifySuccess } from "../lib/feedback";
import { density } from "../theme/density";

export function RepositoryDetailPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const { name = "" } = useParams();
  const { user } = useAuth();
  const isAdmin = user?.role === "admin";
  // 有登录即可尝试上传（后端校验 write）；公开页另走 PublicRepoPage
  const allowUpload = Boolean(user);

  // 拉取仓库信息（供页头 Badge 与配置 Tab 使用）
  const repoState = useAsync(
    () =>
      listRepositories({ page_size: 100 }).then(
        (list) => list.items.find((r) => r.name === name) ?? null,
      ),
    [name],
  );
  const repo = repoState.data ?? null;

  return (
    <>
      <PageHeader
        title={`${t("repoDetail.title")} · ${name}`}
        description={t("repoDetail.description")}
        actions={
          <Button variant="default" onClick={() => navigate("/repositories")}>
            {t("common.close")}
          </Button>
        }
      />

      {/* 页头 Badge：format/type/visibility */}
      {repo && (
        <Group gap="xs" mb="sm">
          <Badge variant="light">{repo.format}</Badge>
          <Badge variant="outline" color="gray">
            {repo.type}
          </Badge>
          <Badge variant="light" color={repo.visibility === "public" ? "blue" : "gray"}>
            {repo.visibility === "public"
              ? t("repositories.visibilityPublic")
              : t("repositories.visibilityPrivate")}
          </Badge>
        </Group>
      )}

      <Tabs defaultValue="browse">
        <Tabs.List>
          <Tabs.Tab value="browse">{t("repoDetail.tabBrowse")}</Tabs.Tab>
          {isAdmin && <Tabs.Tab value="config">{t("repoDetail.tabConfig")}</Tabs.Tab>}
          {isAdmin && <Tabs.Tab value="acl">{t("repoDetail.tabAcl")}</Tabs.Tab>}
        </Tabs.List>

        {/* 浏览 Tab：嵌入现有 RepoBrowser */}
        <Tabs.Panel value="browse" pt="md">
          <RepoBrowser repoName={name} allowUpload={allowUpload} publicMode={false} />
        </Tabs.Panel>

        {/* 配置 Tab：仅管理员，展示仓库信息 + 可修改 visibility */}
        {isAdmin && (
          <Tabs.Panel value="config" pt="md">
            <ConfigTab repoName={name} repo={repo} onUpdated={repoState.reload} />
          </Tabs.Panel>
        )}

        {/* ACL Tab：仅管理员，内联 ACL 管理 */}
        {isAdmin && (
          <Tabs.Panel value="acl" pt="md">
            <AclPanel repoName={name} />
          </Tabs.Panel>
        )}
      </Tabs>
    </>
  );
}

/** 配置 Tab：展示仓库基本信息，可修改 visibility 并保存。 */
function ConfigTab({
  repoName,
  repo,
  onUpdated,
}: {
  repoName: string;
  repo: Repository | null;
  onUpdated: () => void;
}) {
  const { t } = useTranslation();
  const [visibility, setVisibility] = useState<RepoVisibility>("private");
  const [saving, setSaving] = useState(false);

  // 仓库信息就绪后同步初始 visibility
  useEffect(() => {
    if (repo) {
      setVisibility(repo.visibility);
    }
  }, [repo]);

  const handleSave = () => {
    setSaving(true);
    updateRepository(repoName, { visibility })
      .then(() => {
        notifySuccess(t("common.saved"));
        onUpdated();
      })
      .catch(notifyError)
      .finally(() => setSaving(false));
  };

  if (!repo) {
    return <Text c="dimmed">{t("common.loading")}</Text>;
  }

  return (
    <Card withBorder padding={density.cardPadding} radius="md" maw={520}>
      <Stack gap="md">
        <Title order={5}>{t("repoDetail.configTitle")}</Title>

        {/* 基本信息（只读展示） */}
        <Stack gap="xs">
          <Text size="sm" c="dimmed">
            {t("repoDetail.configName")}：{repo.name}
          </Text>
          <Text size="sm" c="dimmed">
            {t("repoDetail.configFormat")}：{repo.format}
          </Text>
          <Text size="sm" c="dimmed">
            {t("repoDetail.configType")}：{repo.type}
          </Text>
        </Stack>

        {/* 可见性修改 */}
        <Select
          label={t("repoDetail.configVisibility")}
          data={[
            { value: "private", label: t("repoDetail.configVisibilityPrivate") },
            { value: "public", label: t("repoDetail.configVisibilityPublic") },
          ]}
          value={visibility}
          onChange={(v) => v && setVisibility(v as RepoVisibility)}
          allowDeselect={false}
        />

        <Group justify="flex-end">
          <Button onClick={handleSave} loading={saving}>
            {t("repoDetail.configSave")}
          </Button>
        </Group>
      </Stack>
    </Card>
  );
}

/**
 * ACL 管理面板：从 AclPage 提取的核心逻辑，接收 repoName prop。
 * 拉取 ACL 条目与用户列表，支持增删改后整表 PUT 保存。
 */
function AclPanel({ repoName }: { repoName: string }) {
  const { t } = useTranslation();

  // 并行拉取 ACL 条目与用户列表（page_size: 100 足够覆盖常见规模）
  const state = useAsync(
    () =>
      Promise.all([getAcl(repoName), listUsers({ page_size: 100 })]).then(([acl, users]) => ({
        acl,
        users,
      })),
    [repoName],
  );

  const [entries, setEntries] = useState<AclEntry[]>([]);
  const [users, setUsers] = useState<User[]>([]);
  const [saving, setSaving] = useState(false);

  // 新增条目输入：用户 Select + 权限 Select，点击添加才落入 entries
  const [newSubjectId, setNewSubjectId] = useState<string | null>(null);
  const [newAction, setNewAction] = useState<AclAction>("read");

  useEffect(() => {
    if (state.data) {
      setEntries(state.data.acl.items);
      setUsers(state.data.users.items);
    }
  }, [state.data]);

  // id -> username 映射；列表中用用户名展示，映射缺失时回退到 id
  const nameById = useMemo(() => {
    const m = new Map<number, string>();
    for (const u of users) m.set(u.id, u.username);
    return m;
  }, [users]);

  // 用户下拉选项：值用字符串 id（Mantine Select 统一字符串），提交时转回 number
  const userOptions = useMemo(
    () => users.map((u) => ({ value: String(u.id), label: u.username })),
    [users],
  );

  // 已添加条目中已被占用的用户 id，新增时从下拉里排除，避免重复授权
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
    setAcl(repoName, entries)
      .then(() => notifySuccess(t("common.saved")))
      .catch(notifyError)
      .finally(() => setSaving(false));
  };

  return (
    <Stack gap="md">
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
            <Group align="flex-end" gap="sm">
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

            <Group justify="flex-end">
              <Button onClick={handleSave} loading={saving}>
                {t("acl.save")}
              </Button>
            </Group>
          </>
        )}
      </AsyncBoundary>
    </Stack>
  );
}
