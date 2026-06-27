// 设置页（FR-87 只读 + FR-88 可编辑热替换 + FR-103 单页堆叠重排，仅管理员）：
// 单页纵向堆叠（网络代理 → 在线更新 → 关于·版本，去 tab、布局不抖）+ 高密度可编辑表单，
// 编辑网络代理（FR-84）+ 在线更新（FR-85）配置，保存调 PATCH /api/v1/settings 即时生效、
// 无须重启；并提供「检查更新 / 应用更新」入口。在线更新区默认仅显「更新通道」+ 检查更新，
// 低频高级项（仓库源 / API 基址 / 重启模式 / 访问令牌）收进默认收起的「高级设置」折叠区。
//
// 数据来自后端 GET /api/v1/settings（已脱敏：代理 URL 去凭据、更新 token 仅以 has_token 暴露）。
// 保存走 PATCH（运行时热替换，守 ADR-0022）：代理凭据与 token 只入内存槽、不写回 TOML / 不回显，
// 重启回落文件 / env 配置。token 三态：留空=保留现有，清空动作=清除，填新值=设置。
// FR-103 仅重排呈现：数据加载 / 保存 / 检查 / 应用逻辑原样复用，不改 GET/PATCH 契约。

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
  Divider,
  Code,
  Modal,
  Alert,
  TextInput,
  Switch,
  Select,
  PasswordInput,
  Collapse,
} from '@mantine/core';
import { useDisclosure } from '@mantine/hooks';
import {
  IconRefresh,
  IconArrowUp,
  IconInfoCircle,
  IconDeviceFloppy,
  IconChevronDown,
  IconChevronRight,
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

  return (
    <Stack gap={density.gridSpacing}>
      <Title order={2}>设置</Title>
      {error && <ErrorAlert message={error} />}

      {/* FR-103：单页纵向堆叠（网络代理 → 在线更新 → 关于·版本，去 tab、布局不抖） */}
      <Stack gap={density.gridSpacing}>
        {/* —— 网络代理 —— */}
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

        {/* —— 在线更新 —— */}
        <Card withBorder padding={density.cardPadding} radius="md">
          <Title order={4}>在线更新</Title>
          <Text size="sm" c="dimmed" mb="sm">
            管理员手动触发的自更新。当前版本见「关于·版本」。
          </Text>
          <Stack gap="sm">
            {/* 默认可见：启用开关 + 更新通道（高频项） */}
            <Switch
              label="启用在线更新（出站开关）"
              checked={updateEnabled}
              onChange={(e) => setUpdateEnabled(e.currentTarget.checked)}
            />
            <Select
              label="更新通道"
              description="stable 仅升级到最新稳定版；prerelease 可升级到最新预发布版（含测试版）。"
              data={[
                { value: 'stable', label: 'stable（仅稳定版）' },
                { value: 'prerelease', label: 'prerelease（含预发布版）' },
              ]}
              value={channel}
              onChange={(v) => setChannel(v ?? 'stable')}
              allowDeselect={false}
            />

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

        {/* —— 更新检查 / 应用 —— */}
        <Card withBorder padding={density.cardPadding} radius="md">
          <Title order={4}>检查与应用更新</Title>
          <Divider my="sm" />

          {!settings.update.enabled && (
            <Alert
              icon={<IconInfoCircle size={16} />}
              color="gray"
              variant="light"
              title="在线更新未启用"
            >
              在线更新出站开关当前关闭。请在上方启用并保存后，再检查 / 应用更新。
            </Alert>
          )}

          {restarting ? (
            <Alert
              icon={<IconInfoCircle size={16} />}
              color="blue"
              variant="light"
              title="已触发升级"
            >
              服务正在重启…当前连接将断开，请稍候片刻后手动刷新页面。
            </Alert>
          ) : (
            <Stack gap="sm">
              <Group>
                <Button
                  leftSection={<IconRefresh size={16} />}
                  onClick={handleCheck}
                  loading={checking}
                  disabled={!settings.update.enabled}
                >
                  检查更新
                </Button>
                {check?.update_available && (
                  <Button
                    color="orange"
                    leftSection={<IconArrowUp size={16} />}
                    onClick={confirmModal.open}
                  >
                    升级到 v{check.latest_version}
                  </Button>
                )}
              </Group>

              {checkError && <ErrorAlert message={checkError} />}
              {applyError && <ErrorAlert message={applyError} />}

              {check && (
                <Card withBorder padding="md" radius="sm" bg="var(--mantine-color-gray-0)">
                  <Group gap="xs">
                    <Text size="sm">当前版本</Text>
                    <Code>{check.current_version}</Code>
                    <Text size="sm">最新版本</Text>
                    <Code>{check.latest_version}</Code>
                    {check.update_available ? (
                      <Badge color="orange">有可用更新</Badge>
                    ) : (
                      <Badge color="green">已是最新</Badge>
                    )}
                  </Group>
                  {/* FR-103：展示拉取到的 release 发布说明（notes 即 release body），无说明优雅留空 */}
                  {check.notes && (
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
              )}
            </Stack>
          )}
        </Card>

        {/* —— 关于·版本 —— */}
        <Card withBorder padding={density.cardPadding} radius="md">
          <Title order={4}>关于·版本</Title>
          <Stack gap="xs" mt="sm">
            <Group gap="xs">
              <Text size="sm">当前版本</Text>
              <Badge variant="light">{settings.current_version}</Badge>
            </Group>
            <Text size="sm" c="dimmed">
              网络代理与在线更新配置，保存后运行时即时生效、无须重启。代理凭据与访问令牌只入内存、不回显、不写回配置文件，重启回落
              config.toml / 环境变量。
            </Text>
          </Stack>
        </Card>
      </Stack>

      {/* —— 保存（网络代理 + 在线更新共用一次 PATCH，沿用 FR-88 既有逻辑）——
          固定为 sticky 底部动作条：始终贴在滚动视口底部、不随内容 / 窗口缩放漂移；
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
    </Stack>
  );
}
