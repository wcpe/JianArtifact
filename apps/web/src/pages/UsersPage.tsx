// 用户管理：列表 + 新建 / 改角色状态 / 重置口令 / 删除。
import {
  Alert,
  Badge,
  Box,
  Button,
  Group,
  Modal,
  MultiSelect,
  PasswordInput,
  Select,
  NumberInput,
  Stack,
  Switch,
  Table,
  Textarea,
  TextInput,
  Text,
  Tooltip,
} from "@mantine/core";
import { useForm } from "@mantine/form";
import { useDisclosure, useMediaQuery } from "@mantine/hooks";
import {
  IconAdjustments,
  IconKey,
  IconLock,
  IconPlus,
  IconTrash,
  IconUserOff,
  IconUserShield,
  IconUsers,
} from "@tabler/icons-react";
import { EmptyState } from "@jianartifact/ui";
import { useState } from "react";
import { useTranslation } from "react-i18next";

import { PageShell } from "../app/PageShell";
import { AsyncBoundary } from "../components/AsyncBoundary";
import { OpsHelpButton, OpsKpiBand } from "../components/ops/OpsKit";
import {
  changePassword,
  createUser,
  deleteUser,
  getPublishPolicy,
  listRepositories,
  listUsers,
  updatePublishPolicies,
  updateUser,
  type PublishPolicy,
} from "../api/endpoints";
import type {
  PublishPolicyBatchResult,
  Repository,
  User,
  UserRole,
  UserStatus,
} from "../api/types";
import { useAsync } from "../hooks/useAsync";
import { confirmDanger, notifyError, notifySuccess } from "../lib/feedback";

/** FR-66 内置匿名主体用户名（与后端 domain.AnonymousUsername 一致）。 */
const ANONYMOUS_USERNAME = "anonymous";

/** 把文本域的「每行一个」路径前缀解析成数组（去空白与空行）。 */
function parsePrefixes(value: string): string[] {
  return value
    .split(/\r?\n/)
    .map((prefix) => prefix.trim())
    .filter(Boolean);
}

export function UsersPage() {
  const { t } = useTranslation();
  const state = useAsync(() => listUsers({ page_size: 100 }), [], { cacheKey: "users:list" });

  // 顶部概览带口径：直接由当前列表派生，保证与表格同源；数据未就绪时显示「—」而不是假 0。
  // 内置匿名主体不计入「管理员 / 禁止网页登录」，它是授权占位而不是可管理的账号。
  const rows = state.data?.items ?? [];
  const ready = state.data !== null;
  const adminCount = rows.filter(
    (user) => user.role === "admin" && user.username !== ANONYMOUS_USERNAME,
  ).length;
  const disabledCount = rows.filter((user) => user.status === "disabled").length;
  const noWebLoginCount = rows.filter(
    (user) => user.webLoginDisabled && user.username !== ANONYMOUS_USERNAME,
  ).length;

  const [createOpened, createModal] = useDisclosure(false);
  const [creating, setCreating] = useState(false);
  const [pwdUser, setPwdUser] = useState<User | null>(null);
  const [savingPwd, setSavingPwd] = useState(false);
  const [policyUser, setPolicyUser] = useState<User | null>(null);
  const [policyOpened, setPolicyOpened] = useState(false);
  const [policyRepos, setPolicyRepos] = useState<Repository[]>([]);
  // 发布策略支持多仓库（FR-109）：选择集是仓库名列表，保存时整体下发到批量端点。
  const [policyRepoNames, setPolicyRepoNames] = useState<string[]>([]);
  const [policy, setPolicy] = useState<PublishPolicy | null>(null);
  const [policyPrefixes, setPolicyPrefixes] = useState("");
  // 所选仓库的现有策略不一致时给出提示（保存会统一覆盖）。
  const [policyInconsistent, setPolicyInconsistent] = useState(false);
  const [policyLoading, setPolicyLoading] = useState(false);
  const [policySaving, setPolicySaving] = useState(false);
  // 最近一次批量保存的逐仓库结果；null 表示尚未保存过。
  const [policyResults, setPolicyResults] = useState<PublishPolicyBatchResult[] | null>(null);
  // 窄屏（< 48em）：6 列在手机上会把角色下拉与操作图标一起挤到换行，
  // 此时只留「用户名 / 角色 / 操作」，被裁的 id 与启用状态改由用户名下方副文本承载。
  const isNarrow = useMediaQuery("(max-width: 48em)") ?? false;

  const createForm = useForm({
    initialValues: { username: "", password: "", role: "user" as UserRole },
    validate: {
      username: (v) => (v.trim() ? null : t("users.username")),
      password: (v) => (v.length >= 8 ? null : t("auth.passwordRule")),
    },
  });
  const pwdForm = useForm({
    initialValues: { password: "" },
    validate: { password: (v) => (v.length >= 8 ? null : t("auth.passwordRule")) },
  });

  const roleOptions = [
    { value: "admin", label: t("users.roleAdmin") },
    { value: "user", label: t("users.roleUser") },
  ];

  const handleCreate = createForm.onSubmit((values) => {
    setCreating(true);
    createUser(values)
      .then(() => {
        createModal.close();
        createForm.reset();
        notifySuccess(t("common.created"));
        state.reload();
      })
      .catch(notifyError)
      .finally(() => setCreating(false));
  });

  const handleToggleStatus = (user: User) => {
    const next: UserStatus = user.status === "active" ? "disabled" : "active";
    updateUser(user.id, { status: next })
      .then(() => {
        notifySuccess(t("common.updated"));
        state.reload();
      })
      .catch(notifyError);
  };

  const handleChangeRole = (user: User, role: UserRole) => {
    updateUser(user.id, { role })
      .then(() => {
        notifySuccess(t("common.updated"));
        state.reload();
      })
      .catch(notifyError);
  };

  const handleToggleWebLogin = (user: User, checked: boolean) => {
    updateUser(user.id, { webLoginDisabled: checked })
      .then(() => {
        notifySuccess(t("common.updated"));
        state.reload();
      })
      .catch(notifyError);
  };

  // 发布策略的「实际约束」字段：不可变 Release 属于仓库配置（只读展示），不参与比较，
  // 否则不同仓库的仓库级配置差异会被误报成「策略不一致」。
  const samePolicy = (a: PublishPolicy, b: PublishPolicy) =>
    (a.allowedPrefixes ?? []).join("\n") === (b.allowedPrefixes ?? []).join("\n") &&
    a.maxAssetsHour === b.maxAssetsHour &&
    a.maxBytesDay === b.maxBytesDay &&
    a.maxFileBytes === b.maxFileBytes;

  // 多仓口径：以**首个所选仓库**的策略作为编辑基线（保持与原来「先选先编辑」一致），
  // 其余所选仓库与之不一致时置提示位——保存会把这份基线统一覆盖到全部所选仓库。
  const loadPolicies = (userID: number, repoNames: string[]) => {
    if (repoNames.length === 0) {
      // 清空选择时保留最后一次基线，避免表单整体消失导致无法回到「再选一个仓库」；
      // 只有提示与保存按钮的可用性随之变化。
      setPolicyInconsistent(false);
      return;
    }
    setPolicyLoading(true);
    setPolicyResults(null);
    Promise.all(repoNames.map((name) => getPublishPolicy(userID, name)))
      .then((policies) => {
        const baseline = policies[0]!;
        setPolicy(baseline);
        // 契约把 allowedPrefixes 定为 required 数组，但服务端历史上可能返回 null
        // （nil 切片被序列化成 null），此处兜底成空数组，避免 .join 抛 TypeError。
        setPolicyPrefixes((baseline.allowedPrefixes ?? []).join("\n"));
        setPolicyInconsistent(policies.some((item) => !samePolicy(item, baseline)));
      })
      .catch(notifyError)
      .finally(() => setPolicyLoading(false));
  };

  const openPolicy = (user: User) => {
    setPolicyUser(user);
    setPolicyOpened(true);
    setPolicy(null);
    setPolicyRepoNames([]);
    setPolicyResults(null);
    setPolicyInconsistent(false);
    setPolicyLoading(true);
    listRepositories({ page_size: 100 })
      .then((list) => {
        const hosted = list.items.filter((repo) => repo.type === "hosted");
        setPolicyRepos(hosted);
        // 默认只选第一个仓库：与改动前的单仓库行为等价（保存等价于原单仓库保存）。
        const first = hosted[0]?.name ?? "";
        setPolicyRepoNames(first ? [first] : []);
        if (first) {
          loadPolicies(user.id, [first]);
        }
      })
      .catch(notifyError)
      .finally(() => setPolicyLoading(false));
  };

  const savePolicy = () => {
    if (!policyUser || !policy) return;
    if (policyRepoNames.length === 0) {
      notifyError(t("users.policyRepoRequired"));
      return;
    }
    setPolicySaving(true);
    updatePublishPolicies(policyUser.id, policyRepoNames, {
      webLoginDisabled: policy.webLoginDisabled,
      allowedPrefixes: parsePrefixes(policyPrefixes),
      maxAssetsHour: policy.maxAssetsHour,
      maxBytesDay: policy.maxBytesDay,
      maxFileBytes: policy.maxFileBytes,
    })
      .then((value) => {
        setPolicyResults(value.results);
        // 保存成功后所选仓库已统一为同一份策略，不一致提示随之消失。
        setPolicyInconsistent(false);
        const failed = value.results.filter((item) => !item.ok);
        if (failed.length === 0) {
          notifySuccess(t("common.saved"));
        } else {
          // 逐仓库结果会列在弹窗内；这里再给一条汇总提示，避免失败被静默。
          notifyError(t("users.policyPartialFailure", { count: failed.length }));
        }
        state.reload();
      })
      .catch(notifyError)
      .finally(() => setPolicySaving(false));
  };

  const handleDelete = (user: User) => {
    confirmDanger({
      title: t("common.delete"),
      message: t("users.deleteConfirm"),
      confirmLabel: t("common.delete"),
      cancelLabel: t("common.cancel"),
      onConfirm: () => {
        deleteUser(user.id)
          .then(() => {
            notifySuccess(t("common.deleted"));
            state.reload();
          })
          .catch(notifyError);
      },
    });
  };

  const handleChangePassword = pwdForm.onSubmit((values) => {
    if (!pwdUser) {
      return;
    }
    setSavingPwd(true);
    changePassword(pwdUser.id, values.password)
      .then(() => {
        setPwdUser(null);
        pwdForm.reset();
        notifySuccess(t("common.saved"));
      })
      .catch(notifyError)
      .finally(() => setSavingPwd(false));
  });

  return (
    <>
      {/* 列表页统一范式：页头操作固定，表格在视口内滚动 + 表头吸顶。
          此前整页下滚，长列表滚动后列名会丢出视口。 */}
      <PageShell>
        <Stack gap="md" style={{ flex: 1, minHeight: 0 }}>
          {/* 顶部概览带：页内大标题移除后，由它承担原来标题的视觉重量——左侧是账号构成
              （口径来自当前列表，未就绪时显示「—」而不是假的 0），右侧是唯一的主操作。 */}
          <OpsKpiBand
            variant="strip"
            label={t("users.summaryLabel")}
            cols={{ base: 2, sm: 4 }}
            items={[
              {
                label: t("users.summaryTotal"),
                value: ready ? String(rows.length) : "—",
                icon: <IconUsers size={16} />,
                tone: "blue",
                hint: t("users.summaryAnonymousHint"),
              },
              {
                label: t("users.summaryAdmin"),
                value: ready ? String(adminCount) : "—",
                icon: <IconUserShield size={16} />,
                tone: "indigo",
                hint: t("users.rolesAdminHint"),
              },
              {
                label: t("users.summaryDisabled"),
                value: ready ? String(disabledCount) : "—",
                icon: <IconUserOff size={16} />,
                tone: "gray",
              },
              {
                label: t("users.summaryNoWebLogin"),
                value: ready ? String(noWebLoginCount) : "—",
                icon: <IconLock size={16} />,
                tone: "yellow",
                hint: t("users.webLoginDisabledHint"),
              },
            ]}
            actions={
              <>
                {/* 角色口径说明搬到工具栏按钮上（气泡里），不再占页面布局。 */}
                <OpsHelpButton
                  title={t("users.rolesTitle")}
                  items={[
                    { label: t("users.rolesAdmin"), value: t("users.rolesAdminHint") },
                    { label: t("users.rolesUser"), value: t("users.rolesUserHint") },
                    { label: t("users.rolesAnonymous"), value: t("users.rolesAnonymousHint") },
                  ]}
                />
                <Button leftSection={<IconPlus size={14} />} onClick={createModal.open}>
                  {t("users.create")}
                </Button>
              </>
            }
          />

          <Box style={{ flex: 1, minHeight: 0, overflowY: "auto" }}>
            <AsyncBoundary state={state}>
              {(list) =>
                list.items.length === 0 ? (
                  <EmptyState message={t("users.empty")} />
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
                        {/* 窄屏只留「用户名 / 角色 / 操作」：6 列在手机上会把角色下拉与操作
                            图标一起挤到换行；被裁的 id 与启用状态改由用户名下方副文本承载。 */}
                        {isNarrow ? null : <Table.Th>{t("users.id")}</Table.Th>}
                        <Table.Th>{t("users.username")}</Table.Th>
                        <Table.Th>{t("users.role")}</Table.Th>
                        {isNarrow ? null : <Table.Th>{t("users.status")}</Table.Th>}
                        {isNarrow ? null : <Table.Th>{t("users.webLogin")}</Table.Th>}
                        {isNarrow ? null : <Table.Th>{t("common.actions")}</Table.Th>}
                      </Table.Tr>
                    </Table.Thead>
                    <Table.Tbody>
                      {list.items.map((user) => {
                        // FR-66：内置匿名主体不可登录/管理，隐藏其管理操作。
                        const isAnonymous = user.username === ANONYMOUS_USERNAME;
                        // 行内操作统一「图标 + 文字」；抽成变量是因为窄屏要把它从「操作」列
                        // 挪到用户名下方——那里有整行宽度，三个按钮能一行放下，不必竖排撑高。
                        const rowActions = !isAnonymous ? (
                          <Group gap={isNarrow ? 8 : 4} wrap="wrap">
                            <Button
                              size={isNarrow ? "xs" : "compact-xs"}
                              variant="subtle"
                              leftSection={<IconAdjustments size={14} />}
                              aria-label={t("users.publishPolicy")}
                              onClick={() => openPolicy(user)}
                            >
                              {t("users.publishPolicy")}
                            </Button>
                            <Button
                              size={isNarrow ? "xs" : "compact-xs"}
                              variant="subtle"
                              leftSection={<IconKey size={14} />}
                              aria-label={t("users.changePassword")}
                              onClick={() => setPwdUser(user)}
                            >
                              {t("users.changePassword")}
                            </Button>
                            <Button
                              size={isNarrow ? "xs" : "compact-xs"}
                              variant="subtle"
                              color="red"
                              leftSection={<IconTrash size={14} />}
                              aria-label={t("common.delete")}
                              onClick={() => handleDelete(user)}
                            >
                              {t("common.delete")}
                            </Button>
                          </Group>
                        ) : null;
                        return (
                          <Table.Tr key={user.id}>
                            {isNarrow ? null : <Table.Td>{user.id}</Table.Td>}
                            <Table.Td>
                              <Stack gap={isNarrow ? 12 : 2}>
                                <Group gap="xs" wrap="nowrap">
                                  {user.username}
                                  {isAnonymous && (
                                    <Tooltip label={t("users.anonymousRowHint")}>
                                      <Badge variant="light" color="gray">
                                        {t("common.anonymous")}
                                      </Badge>
                                    </Tooltip>
                                  )}
                                </Group>
                                {/* 窄屏被裁的 id / 状态 / 网页登录开关集中到这里。 */}
                                {isNarrow ? (
                                  <Text size="xs" c="dimmed" truncate>
                                    #{user.id} ·{" "}
                                    {user.status === "active"
                                      ? t("users.statusActive")
                                      : t("users.statusDisabled")}
                                    {" · "}
                                    {user.webLoginDisabled
                                      ? t("users.webLoginDisabled")
                                      : t("users.webLoginEnabled")}
                                  </Text>
                                ) : null}
                                {isNarrow ? rowActions : null}
                              </Stack>
                            </Table.Td>
                            <Table.Td>
                              {isAnonymous ? (
                                <Badge variant="light" color="gray">
                                  {t("users.roleUser")}
                                </Badge>
                              ) : (
                                <Select
                                  size="xs"
                                  w={isNarrow ? 96 : 120}
                                  data={roleOptions}
                                  value={user.role}
                                  allowDeselect={false}
                                  onChange={(v) => v && handleChangeRole(user, v as UserRole)}
                                />
                              )}
                            </Table.Td>
                            {isNarrow ? null : (
                              <Table.Td>
                                <Badge
                                  color={user.status === "active" ? "green" : "gray"}
                                  variant="light"
                                  style={isAnonymous ? undefined : { cursor: "pointer" }}
                                  onClick={isAnonymous ? undefined : () => handleToggleStatus(user)}
                                >
                                  {user.status === "active"
                                    ? t("users.statusActive")
                                    : t("users.statusDisabled")}
                                </Badge>
                              </Table.Td>
                            )}
                            {isNarrow ? null : (
                              <Table.Td>
                                <Switch
                                  size="sm"
                                  aria-label={`${t("users.webLoginEnabled")} ${user.username}`}
                                  checked={!user.webLoginDisabled}
                                  disabled={isAnonymous}
                                  onChange={(event) =>
                                    handleToggleWebLogin(user, !event.currentTarget.checked)
                                  }
                                />
                              </Table.Td>
                            )}
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

      <Modal opened={createOpened} onClose={createModal.close} title={t("users.create")}>
        <form onSubmit={handleCreate}>
          <TextInput
            label={t("users.username")}
            withAsterisk
            {...createForm.getInputProps("username")}
          />
          <PasswordInput
            mt="sm"
            label={t("auth.password")}
            withAsterisk
            {...createForm.getInputProps("password")}
          />
          <Select
            mt="sm"
            label={t("users.role")}
            data={roleOptions}
            allowDeselect={false}
            {...createForm.getInputProps("role")}
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
        opened={pwdUser !== null}
        onClose={() => setPwdUser(null)}
        title={t("users.changePassword")}
      >
        <form onSubmit={handleChangePassword}>
          <PasswordInput
            label={t("auth.password")}
            withAsterisk
            {...pwdForm.getInputProps("password")}
          />
          <Group justify="flex-end" mt="md">
            <Button variant="default" onClick={() => setPwdUser(null)}>
              {t("common.cancel")}
            </Button>
            <Button type="submit" loading={savingPwd}>
              {t("common.save")}
            </Button>
          </Group>
        </form>
      </Modal>

      <Modal
        opened={policyOpened}
        onClose={() => setPolicyOpened(false)}
        title={t("users.publishPolicy")}
        size="lg"
      >
        {!policyUser || policyRepos.length === 0 ? (
          <EmptyState message={t("users.noHostedRepositories")} />
        ) : (
          <Stack gap="sm">
            {/* 策略用途说明：用户问过「发布策略干什么用」——先讲清约束什么，再给表单。 */}
            <Text size="xs" c="dimmed">
              {t("users.policyPurpose")}
            </Text>
            <MultiSelect
              label={t("users.publishRepository")}
              size="xs"
              data={policyRepos.map((repo) => ({ value: repo.name, label: repo.name }))}
              value={policyRepoNames}
              searchable
              clearable
              onChange={(values) => {
                setPolicyRepoNames(values);
                loadPolicies(policyUser.id, values);
              }}
            />
            <Text size="xs" c="dimmed">
              {t("users.policyMultipleHint")}
            </Text>
            {policyInconsistent ? (
              <Alert color="yellow" variant="light" p="xs">
                <Text size="xs">{t("users.policyInconsistent")}</Text>
              </Alert>
            ) : null}
            {policyLoading ? (
              <Text size="sm">{t("common.loading")}</Text>
            ) : policy ? (
              <>
                <Switch
                  label={t("users.webLoginDisabled")}
                  description={t("users.webLoginDisabledHint")}
                  checked={policy.webLoginDisabled}
                  onChange={(event) =>
                    setPolicy({ ...policy, webLoginDisabled: event.currentTarget.checked })
                  }
                />
                <Textarea
                  label={t("users.allowedPrefixes")}
                  description={t("users.allowedPrefixesHint")}
                  minRows={3}
                  value={policyPrefixes}
                  onChange={(event) => setPolicyPrefixes(event.currentTarget.value)}
                />
                <Group grow>
                  <NumberInput
                    label={t("users.maxAssetsHour")}
                    min={0}
                    value={policy.maxAssetsHour}
                    onChange={(value) =>
                      setPolicy({ ...policy, maxAssetsHour: Number(value) || 0 })
                    }
                  />
                  <NumberInput
                    label={t("users.maxBytesDay")}
                    min={0}
                    value={policy.maxBytesDay}
                    onChange={(value) => setPolicy({ ...policy, maxBytesDay: Number(value) || 0 })}
                  />
                  <NumberInput
                    label={t("users.maxFileBytes")}
                    min={0}
                    value={policy.maxFileBytes}
                    onChange={(value) => setPolicy({ ...policy, maxFileBytes: Number(value) || 0 })}
                  />
                </Group>
                {/* 不可变 Release 属于仓库配置（后端写入即拒绝），这里只读展示其当前值，
                    不能做成可切换——否则用户以为改了、实际每次保存都会被拒。 */}
                <Switch
                  label={t("users.immutableRelease")}
                  description={t("users.policyImmutableReadOnly")}
                  checked={policy.immutableRelease}
                  disabled
                />
                {policyRepoNames.length === 0 ? (
                  <Text size="xs" c="red">
                    {t("users.policyRepoRequired")}
                  </Text>
                ) : null}
                <Group justify="flex-end">
                  <Button variant="default" onClick={() => setPolicyOpened(false)}>
                    {t("common.cancel")}
                  </Button>
                  <Button
                    onClick={savePolicy}
                    loading={policySaving}
                    disabled={policyRepoNames.length === 0}
                  >
                    {t("common.save")}
                  </Button>
                </Group>
              </>
            ) : null}
            {/* 逐仓库结果：部分失败必须可见，而不是只报一句「保存失败」。 */}
            {policyResults ? (
              <Stack gap={4}>
                <Text size="xs" fw={600}>
                  {t("users.policyResults")}
                </Text>
                {policyResults.map((item) => (
                  <Group key={item.repository} gap={6} wrap="nowrap">
                    <Badge size="xs" variant="light" color={item.ok ? "green" : "red"}>
                      {item.ok ? t("users.policyResultOK") : t("users.policyResultFailed")}
                    </Badge>
                    <Text size="xs" ff="monospace" truncate>
                      {item.repository}
                    </Text>
                    {item.error ? (
                      <Text size="xs" c="red">
                        {item.error}
                      </Text>
                    ) : null}
                  </Group>
                ))}
              </Stack>
            ) : null}
          </Stack>
        )}
      </Modal>
    </>
  );
}
