// 设置页（FR-87 只读 + FR-88 可编辑热替换 + FR-103 二级导航重设计，仅管理员）：
// 左侧二级导航（网络代理 / 在线更新）+ 右侧内容区，切 tab 布局不抖、保存条位置稳定；
// 在线更新做成一张「应用更新」卡片（右上角通道切换 + 检查更新、版本对比 + 徽标、预发布提示、
// 版本明细 + release 说明、底部「立即更新并重启 / 回滚到上一版」），低频项收进卡内「高级设置」折叠区。
// 编辑网络代理（FR-100）+ 在线更新（FR-85/89）配置，保存调 PATCH /api/v1/settings 即时生效、无须重启。
//
// 数据来自后端 GET /api/v1/settings（已脱敏：代理 URL 去凭据、更新 token 仅以 has_token 暴露）。
// 保存走 PATCH（运行时热替换，守 ADR-0022）：代理凭据与 token 只入内存槽、不写回 TOML / 不回显，
// 重启回落文件 / env 配置。token 三态：留空=保留现有，清空动作=清除，填新值=设置。
// FR-103 仅重排呈现：数据加载 / 保存 / 检查 / 应用 / 回滚逻辑原样复用，不改 GET/PATCH 与更新端点契约。

import { useEffect, useState } from 'react';
import {
  Box,
  Stack,
  Title,
  Text,
  Card,
  Group,
  Badge,
  Button,
  Loader,
  Center,
  Code,
  Modal,
  Alert,
  TextInput,
  Switch,
  Select,
  SegmentedControl,
  PasswordInput,
  Collapse,
  Tabs,
} from '@mantine/core';
import { useDisclosure } from '@mantine/hooks';
import {
  IconRefresh,
  IconArrowUp,
  IconArrowBackUp,
  IconInfoCircle,
  IconDeviceFloppy,
  IconChevronDown,
  IconChevronRight,
  IconNetwork,
  IconCloudDownload,
} from '@tabler/icons-react';
import * as api from '../api/endpoints';
import type { SettingsView, UpdateCheck, ProxyEntryPatch } from '../api/types';
import { errorMessage } from '../lib/format';
import { ErrorAlert } from '../components/ErrorAlert';
import { density } from '../theme/density';

/** 单代理三字段（URL / 用户名 / 密码）一组（FR-100）。密码框始终空、不回显；已配置时标徽标 + 提供清除密码。 */
interface ProxyFieldsProps {
  title: string;
  urlPlaceholder: string;
  url: string;
  onUrlChange: (v: string) => void;
  username: string;
  onUsernameChange: (v: string) => void;
  password: string;
  onPasswordChange: (v: string) => void;
  hasPassword: boolean;
  passwordCleared: boolean;
  onClearPassword: () => void;
}

function ProxyFields(props: ProxyFieldsProps) {
  const {
    title,
    urlPlaceholder,
    url,
    onUrlChange,
    username,
    onUsernameChange,
    password,
    onPasswordChange,
    hasPassword,
    passwordCleared,
    onClearPassword,
  } = props;
  return (
    <Stack gap="xs">
      <Group gap="xs">
        <Text size="sm" fw={600}>
          {title}
        </Text>
        {/* 已配置密码标识（绝不回显密码本体）；点过清除则提示本次保存将清空 */}
        {hasPassword && !passwordCleared && (
          <Badge size="sm" color="blue" variant="light">
            密码已配置
          </Badge>
        )}
        {passwordCleared && (
          <Badge size="sm" color="orange" variant="light">
            保存后清除密码
          </Badge>
        )}
      </Group>
      <TextInput
        label="URL"
        placeholder={urlPlaceholder}
        value={url}
        onChange={(e) => onUrlChange(e.currentTarget.value)}
      />
      <TextInput
        label="用户名"
        placeholder="可选"
        value={username}
        onChange={(e) => onUsernameChange(e.currentTarget.value)}
      />
      <PasswordInput
        label="密码"
        placeholder="留空保留现有密码"
        value={password}
        onChange={(e) => onPasswordChange(e.currentTarget.value)}
      />
      {/* 仅在已配置密码时提供「清除密码」动作（发 password: ""）；填了新密码或已点清除则不再显示 */}
      {hasPassword && !passwordCleared && !password && (
        <Group>
          <Button size="xs" variant="subtle" color="red" onClick={onClearPassword}>
            清除密码
          </Button>
        </Group>
      )}
    </Stack>
  );
}

/** 设置页。 */
export function SettingsPage() {
  const [settings, setSettings] = useState<SettingsView | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  // —— 可编辑表单态（FR-88 + FR-100）——
  // 每代理（http / https / all）拆三字段：URL（脱敏 host，无凭据）、用户名（回显）、密码（不回显）。
  // 密码框始终空：留空保存=省略 password 字段（保留现有）；填值=设置；另有「清除密码」动作发空串清空。
  const [httpUrl, setHttpUrl] = useState('');
  const [httpUser, setHttpUser] = useState('');
  const [httpPass, setHttpPass] = useState('');
  const [httpHasPass, setHttpHasPass] = useState(false);
  const [httpsUrl, setHttpsUrl] = useState('');
  const [httpsUser, setHttpsUser] = useState('');
  const [httpsPass, setHttpsPass] = useState('');
  const [httpsHasPass, setHttpsHasPass] = useState(false);
  const [allUrl, setAllUrl] = useState('');
  const [allUser, setAllUser] = useState('');
  const [allPass, setAllPass] = useState('');
  const [allHasPass, setAllHasPass] = useState(false);
  // 三个「清除密码」动作的标记：点了清除即在本次 PATCH 发 password: "" 清空对应代理密码。
  const [httpClearPass, setHttpClearPass] = useState(false);
  const [httpsClearPass, setHttpsClearPass] = useState(false);
  const [allClearPass, setAllClearPass] = useState(false);
  const [noProxy, setNoProxy] = useState('');
  const [updateEnabled, setUpdateEnabled] = useState(false);
  const [repo, setRepo] = useState('');
  const [apiBaseUrl, setApiBaseUrl] = useState('');
  const [restartMode, setRestartMode] = useState('self');
  const [channel, setChannel] = useState('stable');
  // token 输入框：留空 = 保留现有（提交时省略 token 字段）；填值 = 设置新 token
  const [tokenInput, setTokenInput] = useState('');
  const [hasToken, setHasToken] = useState(false);
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);

  // 更新检查 / 应用相关状态
  const [check, setCheck] = useState<UpdateCheck | null>(null);
  const [checking, setChecking] = useState(false);
  const [checkError, setCheckError] = useState<string | null>(null);
  const [applying, setApplying] = useState(false);
  const [applyError, setApplyError] = useState<string | null>(null);
  const [restarting, setRestarting] = useState(false);
  const [confirmOpened, confirmModal] = useDisclosure(false);
  // 回滚相关状态（FR-104）：进行中标志、错误、二次确认弹窗开合
  const [rollingBack, setRollingBack] = useState(false);
  const [rollbackError, setRollbackError] = useState<string | null>(null);
  const [rollbackConfirmOpened, rollbackConfirmModal] = useDisclosure(false);
  // 在线更新「高级设置」折叠区开合（FR-103）：默认收起，低频项（仓库源 / API 基址 / 重启模式 / 访问令牌）点开才显
  const [advancedOpened, advancedToggle] = useDisclosure(false);

  // 用一份设置填充表单态。
  function fillForm(s: SettingsView) {
    setSettings(s);
    // 网络代理三槽：URL / 用户名回显预填，密码框始终空、仅以 has_password 标识是否已配置（FR-100）。
    setHttpUrl(s.network_proxy.http.url ?? '');
    setHttpUser(s.network_proxy.http.username ?? '');
    setHttpPass('');
    setHttpHasPass(s.network_proxy.http.has_password);
    setHttpClearPass(false);
    setHttpsUrl(s.network_proxy.https.url ?? '');
    setHttpsUser(s.network_proxy.https.username ?? '');
    setHttpsPass('');
    setHttpsHasPass(s.network_proxy.https.has_password);
    setHttpsClearPass(false);
    setAllUrl(s.network_proxy.all.url ?? '');
    setAllUser(s.network_proxy.all.username ?? '');
    setAllPass('');
    setAllHasPass(s.network_proxy.all.has_password);
    setAllClearPass(false);
    setNoProxy(s.network_proxy.no_proxy ?? '');
    setUpdateEnabled(s.update.enabled);
    setRepo(s.update.repo);
    setApiBaseUrl(s.update.api_base_url);
    setRestartMode(s.update.restart_mode);
    setChannel(s.update.channel);
    setHasToken(s.update.has_token);
    setTokenInput('');
  }

  useEffect(() => {
    api
      .getSettings()
      .then(fillForm)
      .catch((err) => setError(errorMessage(err)))
      .finally(() => setLoading(false));
  }, []);

  if (loading) {
    return (
      <Center h={200}>
        <Loader />
      </Center>
    );
  }

  if (!settings) {
    return (
      <Stack>
        <Title order={2}>设置</Title>
        {error && <ErrorAlert message={error} />}
      </Stack>
    );
  }

  // 组装单代理 PATCH 项（FR-100）：url / username 照填；
  // 密码三态——填了新密码 → 带 password（设置）；点了「清除密码」→ 带 password: ""（清空）；
  // 否则省略 password 字段（保留现有）。
  function buildProxyPatch(
    url: string,
    username: string,
    password: string,
    clearPass: boolean,
  ): ProxyEntryPatch {
    const entry: ProxyEntryPatch = { url: url.trim(), username: username.trim() };
    if (password) {
      entry.password = password;
    } else if (clearPass) {
      entry.password = '';
    }
    return entry;
  }

  async function handleSave() {
    setSaving(true);
    setSaveError(null);
    setSaved(false);
    try {
      const updated = await api.updateSettings({
        network_proxy: {
          http: buildProxyPatch(httpUrl, httpUser, httpPass, httpClearPass),
          https: buildProxyPatch(httpsUrl, httpsUser, httpsPass, httpsClearPass),
          all: buildProxyPatch(allUrl, allUser, allPass, allClearPass),
          no_proxy: noProxy.trim(),
        },
        update: {
          enabled: updateEnabled,
          repo: repo.trim(),
          api_base_url: apiBaseUrl.trim(),
          restart_mode: restartMode,
          channel,
          // token 留空则省略（保留现有）；填值则设置新 token
          ...(tokenInput.trim() ? { token: tokenInput.trim() } : {}),
        },
      });
      fillForm(updated);
      setSaved(true);
    } catch (err) {
      setSaveError(errorMessage(err));
    } finally {
      setSaving(false);
    }
  }

  async function handleCheck() {
    setChecking(true);
    setCheckError(null);
    setCheck(null);
    try {
      const result = await api.checkUpdate();
      setCheck(result);
    } catch (err) {
      setCheckError(errorMessage(err));
    } finally {
      setChecking(false);
    }
  }

  async function handleApply() {
    setApplying(true);
    setApplyError(null);
    try {
      await api.applyUpdate();
      // apply 成功即服务将停机重启，当前连接会断；进入「正在重启」提示态、引导手动刷新
      confirmModal.close();
      setRestarting(true);
    } catch (err) {
      setApplyError(errorMessage(err));
      confirmModal.close();
    } finally {
      setApplying(false);
    }
  }

  async function handleRollback() {
    setRollingBack(true);
    setRollbackError(null);
    try {
      await api.rollbackUpdate();
      // 回滚成功即服务将停机重启，当前连接会断；进入「正在重启」提示态、引导手动刷新
      rollbackConfirmModal.close();
      setRestarting(true);
    } catch (err) {
      setRollbackError(errorMessage(err));
      rollbackConfirmModal.close();
    } finally {
      setRollingBack(false);
    }
  }

  return (
    <Stack gap={density.gridSpacing}>
      <Title order={2}>设置</Title>
      {error && <ErrorAlert message={error} />}

      {/* FR-103：左侧二级导航（网络代理 / 在线更新）+ 右侧内容区。
          Tabs 垂直布局，切 tab 仅切右侧面板、左导航与底部保存条不动；Tabs 默认保留挂载，表单态不丢。 */}
      <Tabs defaultValue="proxy" orientation="vertical" variant="pills">
        <Tabs.List w={180} mr="md">
          <Tabs.Tab value="proxy" leftSection={<IconNetwork size={16} />}>
            网络代理
          </Tabs.Tab>
          <Tabs.Tab value="update" leftSection={<IconCloudDownload size={16} />}>
            在线更新
          </Tabs.Tab>
        </Tabs.List>

        {/* —— 网络代理面板 —— */}
        <Tabs.Panel value="proxy">
          <Card withBorder padding={density.cardPadding} radius="md">
            <Title order={4}>网络代理</Title>
            <Text size="sm" c="dimmed" mb="sm">
              统一出站代理（回源 / 迁移 / 漏洞库 / OIDC / 在线更新共用）。每代理可填用户名 + 密码；
              用户名回显、密码不回显（留空保留现有），URL 留空表示不配置该代理。
            </Text>
            <Stack gap="md">
              {/* HTTP / HTTPS 各自 scheme 专属代理；SOCKS5 填 all（兜底全 scheme，FR-100） */}
              <ProxyFields
                title="HTTP 代理"
                urlPlaceholder="http://host:3128"
                url={httpUrl}
                onUrlChange={setHttpUrl}
                username={httpUser}
                onUsernameChange={setHttpUser}
                password={httpPass}
                onPasswordChange={setHttpPass}
                hasPassword={httpHasPass}
                passwordCleared={httpClearPass}
                onClearPassword={() => setHttpClearPass(true)}
              />
              <ProxyFields
                title="HTTPS 代理"
                urlPlaceholder="http://host:3128"
                url={httpsUrl}
                onUrlChange={setHttpsUrl}
                username={httpsUser}
                onUsernameChange={setHttpsUser}
                password={httpsPass}
                onPasswordChange={setHttpsPass}
                hasPassword={httpsHasPass}
                passwordCleared={httpsClearPass}
                onClearPassword={() => setHttpsClearPass(true)}
              />
              <ProxyFields
                title="SOCKS5 代理（all，兜底全 scheme）"
                urlPlaceholder="socks5://host:1080"
                url={allUrl}
                onUrlChange={setAllUrl}
                username={allUser}
                onUsernameChange={setAllUser}
                password={allPass}
                onPasswordChange={setAllPass}
                hasPassword={allHasPass}
                passwordCleared={allClearPass}
                onClearPassword={() => setAllClearPass(true)}
              />
              <TextInput
                label="直连绕过（no_proxy）"
                placeholder="localhost,127.0.0.1,.internal"
                value={noProxy}
                onChange={(e) => setNoProxy(e.currentTarget.value)}
              />
            </Stack>
          </Card>
        </Tabs.Panel>

        {/* —— 在线更新面板：一张「应用更新」卡片（FR-103） —— */}
        <Tabs.Panel value="update">
          <Card withBorder padding={density.cardPadding} radius="md">
            {/* 卡片头：左标题，右侧通道切换（正式版 / 测试版）+ 检查更新 */}
            <Group justify="space-between" align="flex-start" mb="sm" wrap="nowrap">
              <Box>
                <Title order={4}>应用更新</Title>
                <Text size="sm" c="dimmed">
                  管理员手动触发的自更新，配置即时生效、无须重启。
                </Text>
              </Box>
              <Group gap="xs" wrap="nowrap">
                <SegmentedControl
                  size="xs"
                  value={channel}
                  onChange={setChannel}
                  data={[
                    { value: 'stable', label: '正式版' },
                    { value: 'prerelease', label: '测试版' },
                  ]}
                />
                <Button
                  size="xs"
                  variant="light"
                  leftSection={<IconRefresh size={16} />}
                  onClick={handleCheck}
                  loading={checking}
                  disabled={!settings.update.enabled}
                >
                  检查更新
                </Button>
              </Group>
            </Group>

            <Stack gap="sm">
              {/* 启用在线更新（出站开关）：高频项留卡内可见处 */}
              <Switch
                label="启用在线更新（出站开关）"
                checked={updateEnabled}
                onChange={(e) => setUpdateEnabled(e.currentTarget.checked)}
              />

              {/* 测试版（prerelease）通道提示：滚动开发预览，可能不稳定 */}
              {channel === 'prerelease' && (
                <Alert
                  icon={<IconInfoCircle size={16} />}
                  color="yellow"
                  variant="light"
                  title="测试版通道"
                >
                  滚动开发预览，由 main 最新构建，可能不稳定。仅用于尝鲜 /
                  灰度，生产环境建议用正式版。
                </Alert>
              )}

              {/* 版本对比 + 徽标：当前 ↔ 最新（检查后），预发布徽标随通道显隐 */}
              <Card withBorder padding="sm" radius="sm" bg="var(--mantine-color-gray-0)">
                <Group gap="xs">
                  <Text size="sm">当前版本</Text>
                  <Badge variant="light">
                    {check?.current_version ?? settings.current_version}
                  </Badge>
                  {check && (
                    <>
                      <Text size="sm">→ 最新版本</Text>
                      <Code>{check.latest_version}</Code>
                      {check.update_available ? (
                        <Badge color="orange">有可用更新</Badge>
                      ) : (
                        <Badge color="green">已是最新</Badge>
                      )}
                    </>
                  )}
                  {channel === 'prerelease' && (
                    <Badge color="yellow" variant="light">
                      预发布
                    </Badge>
                  )}
                </Group>
                {/* 检查到的 release 发布说明（notes 即 release body），无说明优雅留空 */}
                {check?.notes && (
                  <>
                    <Text size="xs" c="dimmed" fw={600} mt="sm">
                      发布说明
                    </Text>
                    <Text size="sm" c="dimmed" mt={4} style={{ whiteSpace: 'pre-wrap' }}>
                      {check.notes}
                    </Text>
                  </>
                )}
              </Card>

              {!settings.update.enabled && (
                <Alert
                  icon={<IconInfoCircle size={16} />}
                  color="gray"
                  variant="light"
                  title="在线更新未启用"
                >
                  在线更新出站开关当前关闭。请启用并保存后，再检查 / 应用更新。
                </Alert>
              )}

              {checkError && <ErrorAlert message={checkError} />}
              {applyError && <ErrorAlert message={applyError} />}
              {rollbackError && <ErrorAlert message={rollbackError} />}

              {restarting && (
                <Alert
                  icon={<IconInfoCircle size={16} />}
                  color="blue"
                  variant="light"
                  title="已触发升级"
                >
                  服务正在重启…当前连接将断开，请稍候片刻后手动刷新页面。
                </Alert>
              )}

              {!restarting && (
                <Group>
                  {/* 立即更新并重启：有可用更新时高亮可点；否则禁用（无更新无可应用对象） */}
                  <Button
                    color="orange"
                    leftSection={<IconArrowUp size={16} />}
                    onClick={confirmModal.open}
                    disabled={!check?.update_available}
                  >
                    立即更新并重启
                  </Button>
                  {/* 回滚到上一版（FR-104）：无备份时禁用；回滚是本地操作、不依赖在线更新开关 */}
                  <Button
                    variant="default"
                    leftSection={<IconArrowBackUp size={16} />}
                    onClick={rollbackConfirmModal.open}
                    disabled={!settings.update.rollback_available}
                  >
                    回滚到上一版
                  </Button>
                </Group>
              )}

              {!settings.update.rollback_available && (
                <Text size="xs" c="dimmed">
                  暂无可回滚的备份版本（成功升级一次后才会生成回滚备份）。
                </Text>
              )}

              {/* 高级设置折叠区（FR-103）：低频项默认收起，点开才显示编辑 */}
              <Box>
                <Button
                  variant="subtle"
                  size="xs"
                  px={0}
                  leftSection={
                    advancedOpened ? <IconChevronDown size={16} /> : <IconChevronRight size={16} />
                  }
                  onClick={advancedToggle.toggle}
                  aria-expanded={advancedOpened}
                >
                  高级设置（仓库源 / API 基址 / 重启模式 / 访问令牌）
                </Button>
                <Collapse in={advancedOpened}>
                  <Stack gap="sm" mt="sm">
                    <TextInput
                      label="仓库源（owner/repo）"
                      placeholder="wcpe/JianArtifact"
                      value={repo}
                      onChange={(e) => setRepo(e.currentTarget.value)}
                    />
                    <TextInput
                      label="API 基址"
                      placeholder="https://api.github.com"
                      value={apiBaseUrl}
                      onChange={(e) => setApiBaseUrl(e.currentTarget.value)}
                    />
                    <Select
                      label="重启模式"
                      data={[
                        { value: 'self', label: 'self（自拉起新进程）' },
                        { value: 'exit', label: 'exit（交外部进程管理器重启）' },
                      ]}
                      value={restartMode}
                      onChange={(v) => setRestartMode(v ?? 'self')}
                      allowDeselect={false}
                    />
                    <PasswordInput
                      label="访问令牌（私有仓库可选）"
                      description={
                        hasToken
                          ? '已配置令牌（不回显）。留空保留现有，填新值则替换。'
                          : '未配置。留空表示不设置，填值则设置。'
                      }
                      placeholder={hasToken ? '保留现有令牌' : '可选'}
                      value={tokenInput}
                      onChange={(e) => setTokenInput(e.currentTarget.value)}
                    />
                  </Stack>
                </Collapse>
              </Box>
            </Stack>
          </Card>
        </Tabs.Panel>
      </Tabs>

      {/* —— 保存（网络代理 + 在线更新共用一次 PATCH，沿用 FR-88 既有逻辑）——
          固定为 sticky 底部动作条：始终贴在滚动视口底部、不随内容 / 窗口缩放 / 切 tab 漂移；
          负的左右 / 下外边距抵消 AppShell.Main 的内边距，使其横向铺满、紧贴底缘；
          顶部描边 + 背景 + 内边距与内容区分隔，避免遮挡正文。仅改定位呈现，保存逻辑不变。 */}
      <Box
        data-testid="settings-save-bar"
        style={{
          position: 'sticky',
          bottom: 0,
          zIndex: 1,
          marginInline: `calc(-1 * var(--mantine-spacing-sm))`,
          marginBottom: `calc(-1 * var(--mantine-spacing-sm))`,
          padding: `var(--mantine-spacing-sm)`,
          backgroundColor: 'var(--mantine-color-body)',
          borderTop: '1px solid var(--mantine-color-default-border)',
        }}
      >
        <Group>
          <Button
            leftSection={<IconDeviceFloppy size={16} />}
            onClick={handleSave}
            loading={saving}
          >
            保存
          </Button>
          {saved && (
            <Text c="green" size="sm">
              已保存，配置已即时生效。
            </Text>
          )}
        </Group>
        {saveError && (
          <Box mt="sm">
            <ErrorAlert message={saveError} />
          </Box>
        )}
      </Box>

      {/* —— 升级二次确认弹窗 —— */}
      <Modal opened={confirmOpened} onClose={confirmModal.close} title="确认升级到新版本" centered>
        <Stack>
          <Text>
            将升级到 <Code>v{check?.latest_version}</Code>
            。升级成功后服务会立即重启，当前连接将断开。确认继续？
          </Text>
          <Group justify="flex-end">
            <Button variant="default" onClick={confirmModal.close} disabled={applying}>
              取消
            </Button>
            <Button color="orange" onClick={handleApply} loading={applying}>
              确认升级
            </Button>
          </Group>
        </Stack>
      </Modal>

      {/* —— 回滚二次确认弹窗（FR-104）—— */}
      <Modal
        opened={rollbackConfirmOpened}
        onClose={rollbackConfirmModal.close}
        title="确认回滚到上一版本"
        centered
      >
        <Stack>
          <Text>
            将用备份还原到上一版本的二进制。回滚成功后服务会立即重启，当前连接将断开。确认继续？
          </Text>
          <Group justify="flex-end">
            <Button variant="default" onClick={rollbackConfirmModal.close} disabled={rollingBack}>
              取消
            </Button>
            <Button color="red" onClick={handleRollback} loading={rollingBack}>
              确认回滚
            </Button>
          </Group>
        </Stack>
      </Modal>
    </Stack>
  );
}
