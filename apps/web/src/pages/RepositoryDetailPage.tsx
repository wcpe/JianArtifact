// 仓库详情：Tab 布局（浏览/配置/ACL）。管理员可见配置与 ACL，普通用户仅浏览。
import {
  Badge,
  Button,
  Card,
  Group,
  MultiSelect,
  Select,
  Skeleton,
  Stack,
  Switch,
  Table,
  Tabs,
  Text,
  Textarea,
  Title,
} from "@mantine/core";
import { IconDeviceFloppy, IconPlus, IconRefresh, IconTrash, IconX } from "@tabler/icons-react";
import { useMediaQuery } from "@mantine/hooks";
import { EmptyState } from "@jianartifact/ui";
import { useEffect, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { useNavigate, useParams, useSearchParams } from "react-router-dom";

import { PageShell } from "../app/PageShell";
import { currentLocaleTag } from "../i18n/current";
import { RepoBrowser } from "../components/repo/RepoBrowser";
import { AsyncBoundary } from "../components/AsyncBoundary";
import {
  getAcl,
  listRepositories,
  listUsers,
  recheckConnection,
  setAcl,
  setRepositoryOnline,
  updateRepository,
} from "../api/endpoints";
import type {
  ConnectionStatusValue,
  AclAction,
  AclEntry,
  RepoVisibility,
  Repository,
  User,
} from "../api/types";
import { useAuth } from "../auth/AuthContext";
import { useAsync } from "../hooks/useAsync";
import { CONN_COLOR, CONN_LABEL_KEY } from "../lib/connectionStatus";
import { formatBytes } from "../lib/format";
import { notifyError, notifySuccess } from "../lib/feedback";
import { formatUtcToLocalDate } from "../lib/timeFormat";
import { density } from "../theme/density";

export function RepositoryDetailPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const { name = "" } = useParams();
  // FR-145：搜索直达的命中路径（仅作进入时的初始定位提示，不随后续交互写回）。
  const [searchParams] = useSearchParams();
  const highlightPath = searchParams.get("highlight") ?? undefined;
  const { user } = useAuth();
  const isAdmin = user?.role === "admin";
  // 有登录即可尝试上传（后端校验 write）
  const allowUpload = Boolean(user);

  // 拉取仓库信息：统一走公开列表 API——匿名可读且响应带 artifactCount/totalSize 统计。
  // 此前匿名走 getRepositoryUsage（该端点只含 format/type/description），匿名浏览公开
  // 仓库详情时概览带显示「制品数 0 · 体积 0 B」（用户反馈）；列表项字段足以覆盖所需。
  // 匿名访问 private 仓库时列表不含该项 → null，由下方「未认证」分支处理。
  const repoState = useAsync(
    () =>
      listRepositories({ page_size: 100 }).then(
        (list) => list.items.find((r) => r.name === name) ?? null,
      ),
    [name],
    { cacheKey: `repo:detail:${name}` },
  );
  const repo = repoState.data ?? null;
  // 窄屏（< 48em）：页签与徽章必然折成两行，面板上边距也收一档，尽可能把高度留给内容。
  const isNarrow = useMediaQuery("(max-width: 48em)") ?? false;

  return (
    <PageShell testId="repo-detail-shell">
      <Tabs
        defaultValue="browse"
        styles={{ tab: { paddingTop: 6, paddingBottom: 6 } }}
        style={{ flex: 1, minHeight: 0, display: "flex", flexDirection: "column" }}
      >
        {/* 页头与页签并排：宽屏一行放得下（省掉整整一行），窄屏自动折成
            「页签 / 徽章 + 关闭」两行。仓库名由页眉面包屑承担，此处只表达仓库属性。 */}
        <Group justify="space-between" align="center" wrap="wrap" gap="xs" mb="xs">
          <Tabs.List
            data-testid="repo-detail-tabs"
            style={{
              position: "sticky",
              top: 0,
              zIndex: 1,
              flexWrap: "nowrap",
              overflowX: "auto",
              background: "var(--mantine-color-body)",
            }}
          >
            <Tabs.Tab value="browse">{t("repoDetail.tabBrowse")}</Tabs.Tab>
            {isAdmin && <Tabs.Tab value="config">{t("repoDetail.tabConfig")}</Tabs.Tab>}
            {isAdmin && <Tabs.Tab value="acl">{t("repoDetail.tabAcl")}</Tabs.Tab>}
          </Tabs.List>

          {/* 中间概览：页签与右侧徽章之间原本是 900px 空白带（1440 实测），
              用仓库自身的只读统计填上，顺便让「这个仓库多大/多新/上游通不通」一眼可见。
              窄屏（<48em）隐藏——那里空间要留给页签与徽章。 */}
          {repo ? (
            <Group
              gap="lg"
              wrap="nowrap"
              align="center"
              visibleFrom="md"
              style={{ flex: 1, justifyContent: "center", minWidth: 0 }}
            >
              <DetailStat
                label={t("repoDetail.statArtifacts")}
                value={(repo.artifactCount ?? 0).toLocaleString(currentLocaleTag())}
              />
              <DetailStat
                label={t("repoDetail.statSize")}
                value={formatBytes(repo.totalSize ?? 0)}
              />
              {/* 上游连接状态不在这里重复展示：它属于「配置」页签的正交信息，
                  同一文案在两个位置出现既冗余、也让按文本定位的用例产生歧义。 */}
              <DetailStat
                label={t("repoDetail.statCreatedAt")}
                value={formatUtcToLocalDate(repo.createdAt)}
              />
            </Group>
          ) : null}

          {/* 徽章与「关闭」都取紧凑尺寸：它们只是属性标注与返回入口，不该占掉一行。 */}
          <Group gap={4} wrap="wrap" align="center">
            {repo ? (
              <>
                <Badge size="xs" variant="light">
                  {repo.format}
                </Badge>
                <Badge size="xs" variant="outline" color="gray">
                  {repo.type}
                </Badge>
                <Badge
                  size="xs"
                  variant="light"
                  color={repo.visibility === "public" ? "blue" : "gray"}
                >
                  {repo.visibility === "public"
                    ? t("repositories.visibilityPublic")
                    : t("repositories.visibilityPrivate")}
                </Badge>
              </>
            ) : (
              // 慢接口下也保持页头有形，避免左侧长时间是空的。
              <Skeleton height={20} width={150} radius="sm" />
            )}
            {/* FR-74：未登录的登录入口收敛到页眉（AppLayout），此处仅保留返回。 */}
            <Button
              size="compact-xs"
              variant="default"
              leftSection={<IconX size={14} />}
              onClick={() => navigate("/repositories")}
            >
              {t("common.close")}
            </Button>
          </Group>
        </Group>

        {/* FR-81：描述独立成一行，最多一行高（不再用页头大标题）。 */}
        {repo?.description ? (
          <Text size="xs" c="dimmed" lineClamp={1} mb="xs">
            {repo.description}
          </Text>
        ) : null}

        {/* 浏览 Tab：嵌入现有 RepoBrowser（填满剩余高度，内部面板各自滚动） */}
        <Tabs.Panel
          value="browse"
          pt={isNarrow ? "xs" : "sm"}
          style={{ flex: 1, minHeight: 0, overflow: "hidden" }}
        >
          <RepoBrowser
            repoName={name}
            allowUpload={allowUpload}
            publicMode={!user}
            highlightPath={highlightPath}
          />
        </Tabs.Panel>

        {/* 配置 Tab：仅管理员，展示仓库信息 + 可修改 visibility */}
        {isAdmin && (
          <Tabs.Panel
            value="config"
            pt={isNarrow ? "xs" : "sm"}
            style={{ flex: 1, minHeight: 0, overflowY: "auto" }}
          >
            <ConfigTab repoName={name} repo={repo} onUpdated={repoState.reload} />
          </Tabs.Panel>
        )}

        {/* ACL Tab：仅管理员，内联 ACL 管理 */}
        {isAdmin && (
          <Tabs.Panel
            value="acl"
            pt={isNarrow ? "xs" : "sm"}
            style={{ flex: 1, minHeight: 0, overflowY: "auto" }}
          >
            <AclPanel repoName={name} />
          </Tabs.Panel>
        )}
      </Tabs>
    </PageShell>
  );
}

/** 页头中间的紧凑统计项：小字灰标签 + 加粗值，用于填掉页签与徽章之间的空白带。 */
function DetailStat({ label, value, color }: { label: string; value: string; color?: string }) {
  return (
    <Group gap={6} wrap="nowrap" align="baseline">
      <Text size="xs" c="dimmed">
        {label}
      </Text>
      <Text size="sm" fw={600} c={color}>
        {value}
      </Text>
    </Group>
  );
}

/** 配置 Tab：展示仓库基本信息，可修改 visibility 与描述并保存。 */
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
  const [description, setDescription] = useState("");
  const [members, setMembers] = useState<string[]>([]);
  const [saving, setSaving] = useState(false);
  const [online, setOnline] = useState(true);
  const [togglingOnline, setTogglingOnline] = useState(false);
  const [rechecking, setRechecking] = useState(false);
  // FR-114：连接状态（本地 state 承载重测后的即时更新，避免等待整页刷新）
  const [connStatus, setConnStatus] = useState<ConnectionStatusValue | null>(null);
  // group 成员候选：当前列表中同格式、非本仓的仓库名。
  const reposState = useAsync(() => listRepositories({ page_size: 100 }), [], {
    cacheKey: "repositories:options",
  });
  const memberOptions = (reposState.data?.items ?? [])
    .filter((r) => r.format === repo?.format && r.name !== repo?.name)
    .map((r) => r.name);

  // 仓库信息就绪后同步初始 visibility/描述/members/online/连接状态
  useEffect(() => {
    if (repo) {
      setVisibility(repo.visibility);
      setDescription(repo.description ?? "");
      setMembers(repo.members ?? []);
      setOnline(repo.online ?? true);
      setConnStatus(repo.connectionStatus?.status ?? null);
    }
  }, [repo]);

  // FR-113：online/offline 开关（仅管理员；本地运维状态，不参与复制）。
  const handleToggleOnline = (next: boolean) => {
    setTogglingOnline(true);
    setRepositoryOnline(repoName, next)
      .then(() => {
        setOnline(next);
        // FR-114：离线后状态优先显示「离线」；上线后回到内存态。
        setConnStatus(next ? (repo?.connectionStatus?.status ?? "READY") : "OFFLINE");
        onUpdated();
        notifySuccess(t("common.saved"));
      })
      .catch(notifyError)
      .finally(() => setTogglingOnline(false));
  };

  // FR-114：手动重测上游连接（仅 online proxy），成功后用返回状态即时更新。
  const handleRecheck = () => {
    setRechecking(true);
    recheckConnection(repoName)
      .then((status) => {
        setConnStatus(status.status);
        onUpdated();
        notifySuccess(t("repoDetail.configRecheckOk"));
      })
      .catch(notifyError)
      .finally(() => setRechecking(false));
  };

  const handleSave = () => {
    setSaving(true);
    const patch: { visibility: RepoVisibility; description: string; members?: string[] } = {
      visibility,
      description,
      ...(repo?.type === "group" ? { members } : {}),
    };
    updateRepository(repoName, patch)
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

        {/* FR-113：online/offline 开关（本地运维状态，不参与复制） */}
        <Switch
          label={t("repoDetail.configOnlineLabel", { defaultValue: "在线" })}
          description={t("repoDetail.configOnlineHint", {
            defaultValue: "离线后 group 读将跳过该仓库；本状态不随复制传播",
          })}
          checked={online}
          disabled={togglingOnline}
          onChange={(e) => handleToggleOnline(e.currentTarget.checked)}
        />

        {/* FR-114：连接状态展示 + 手动重测（仅 proxy 仓库可重测） */}
        <Group gap="sm" align="center" wrap="wrap">
          <Text size="sm" c="dimmed">
            {t("repoDetail.configConnection", { defaultValue: "上游连接状态" })}：
          </Text>
          {connStatus ? (
            <Badge variant="light" color={CONN_COLOR[connStatus] ?? "gray"} size="sm">
              {t(CONN_LABEL_KEY[connStatus] ?? "repositories.statusReady")}
            </Badge>
          ) : (
            <Text size="sm" c="dimmed">
              {t("repositories.statusNone", { defaultValue: "—" })}
            </Text>
          )}
          {repo.type === "proxy" && (
            <Button
              size="xs"
              variant="light"
              loading={rechecking}
              disabled={!online}
              onClick={handleRecheck}
              leftSection={<IconRefresh size={14} />}
            >
              {rechecking
                ? t("repoDetail.configRechecking", { defaultValue: "探测中…" })
                : t("repoDetail.configRecheck", { defaultValue: "重新探测" })}
            </Button>
          )}
        </Group>

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

        {/* FR-81：仓库描述，详情页页头展示 */}
        <Textarea
          label={t("repoDetail.configDescription")}
          placeholder={t("repoDetail.configDescriptionPlaceholder")}
          autosize
          minRows={2}
          maxRows={4}
          value={description}
          onChange={(e) => setDescription(e.currentTarget.value)}
        />

        {/* group 仓库：成员仓库（members）编辑 */}
        {repo.type === "group" && (
          <MultiSelect
            label={t("repoDetail.configMembers", { defaultValue: "成员仓库" })}
            description={t("repoDetail.configMembersHint", {
              defaultValue: "选择聚合进本 group 的仓库",
            })}
            data={memberOptions}
            searchable
            value={members}
            onChange={setMembers}
          />
        )}

        <Group justify="flex-end">
          <Button
            onClick={handleSave}
            loading={saving}
            leftSection={<IconDeviceFloppy size={16} />}
          >
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
    { cacheKey: `repo:acl-editor:${repoName}` },
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
                        <Button
                          size="compact-xs"
                          variant="subtle"
                          color="red"
                          leftSection={<IconTrash size={14} />}
                          aria-label={t("common.delete")}
                          onClick={() => removeEntry(index)}
                        >
                          {t("common.delete")}
                        </Button>
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
              <Button
                variant="light"
                onClick={addEntry}
                disabled={!newSubjectId}
                leftSection={<IconPlus size={14} />}
              >
                {t("acl.addEntry")}
              </Button>
            </Group>

            <Group justify="flex-end">
              <Button
                onClick={handleSave}
                loading={saving}
                leftSection={<IconDeviceFloppy size={16} />}
              >
                {t("acl.save")}
              </Button>
            </Group>
          </>
        )}
      </AsyncBoundary>
    </Stack>
  );
}
