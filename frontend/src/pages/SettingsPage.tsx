// 设置页（FR-87 只读 + FR-88 可编辑热替换 + FR-96 页内 tab 重排，仅管理员）：
// 左侧页内 tab（网络代理 / 在线更新 / 关于·版本）+ 右侧高密度可编辑表单，
// 编辑网络代理（FR-84）+ 在线更新（FR-85）配置，保存调 PATCH /api/v1/settings 即时生效、
// 无须重启；并提供「检查更新 / 应用更新」入口。
//
// 数据来自后端 GET /api/v1/settings（已脱敏：代理 URL 去凭据、更新 token 仅以 has_token 暴露）。
// 保存走 PATCH（运行时热替换，守 ADR-0022）：代理凭据与 token 只入内存槽、不写回 TOML / 不回显，
// 重启回落文件 / env 配置。token 三态：留空=保留现有，清空动作=清除，填新值=设置。
// FR-96 仅重排呈现：数据加载 / 保存 / 检查 / 应用逻辑原样复用，不改 GET/PATCH 契约。

import { useEffect, useState } from 'react';
import {
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
  Tabs,
} from '@mantine/core';
import { useDisclosure } from '@mantine/hooks';
import {
  IconRefresh,
  IconArrowUp,
  IconInfoCircle,
  IconDeviceFloppy,
  IconWorld,
  IconCloudDownload,
  IconInfoSquareRounded,
} from '@tabler/icons-react';
import * as api from '../api/endpoints';
import type { SettingsView, UpdateCheck } from '../api/types';
import { errorMessage } from '../lib/format';
import { ErrorAlert } from '../components/ErrorAlert';
import { density } from '../theme/density';

/** 设置页。 */
export function SettingsPage() {
  const [settings, setSettings] = useState<SettingsView | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  // —— 可编辑表单态（FR-88）——
  // 代理 URL：编辑框展示的是脱敏值；保存时如未改动则原样回传（脱敏值无凭据，回传不会泄露已有凭据，
  // 但也无法恢复原凭据——运维如需保留代理凭据应在 config.toml / env 配置）。
  const [httpProxy, setHttpProxy] = useState('');
  const [httpsProxy, setHttpsProxy] = useState('');
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

  // 用一份设置填充表单态。
  function fillForm(s: SettingsView) {
    setSettings(s);
    setHttpProxy(s.network_proxy.http ?? '');
    setHttpsProxy(s.network_proxy.https ?? '');
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

  async function handleSave() {
    setSaving(true);
    setSaveError(null);
    setSaved(false);
    try {
      const updated = await api.updateSettings({
        network_proxy: {
          http: httpProxy.trim() ? httpProxy.trim() : null,
          https: httpsProxy.trim() ? httpsProxy.trim() : null,
          no_proxy: noProxy.trim() ? noProxy.trim() : null,
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

      {/* FR-96：左侧页内 tab 导航 + 右侧高密度可编辑表单；keepMounted 保表单态不随切换丢失 */}
      <Tabs defaultValue="proxy" orientation="vertical" variant="outline">
        <Tabs.List>
          <Tabs.Tab value="proxy" leftSection={<IconWorld size={16} />}>
            网络代理
          </Tabs.Tab>
          <Tabs.Tab value="update" leftSection={<IconCloudDownload size={16} />}>
            在线更新
          </Tabs.Tab>
          <Tabs.Tab value="about" leftSection={<IconInfoSquareRounded size={16} />}>
            关于·版本
          </Tabs.Tab>
        </Tabs.List>

        {/* —— 网络代理 —— */}
        <Tabs.Panel value="proxy" keepMounted pl="md">
          <Stack gap={density.gridSpacing}>
            <Card withBorder padding={density.cardPadding} radius="md">
              <Text size="sm" c="dimmed" mb="sm">
                统一出站代理（回源 / 迁移 / 漏洞库 / OIDC / 在线更新共用）。可含 user:pass@
                凭据（不回显）；留空表示不配置。
              </Text>
              <Stack gap="sm">
                <TextInput
                  label="HTTP 代理"
                  placeholder="http://proxy.internal:8080"
                  value={httpProxy}
                  onChange={(e) => setHttpProxy(e.currentTarget.value)}
                />
                <TextInput
                  label="HTTPS 代理"
                  placeholder="http://proxy.internal:8080"
                  value={httpsProxy}
                  onChange={(e) => setHttpsProxy(e.currentTarget.value)}
                />
                <TextInput
                  label="直连绕过（no_proxy）"
                  placeholder="localhost,127.0.0.1,.internal"
                  value={noProxy}
                  onChange={(e) => setNoProxy(e.currentTarget.value)}
                />
              </Stack>
            </Card>
          </Stack>
        </Tabs.Panel>

        {/* —— 在线更新 —— */}
        <Tabs.Panel value="update" keepMounted pl="md">
          <Stack gap={density.gridSpacing}>
            <Card withBorder padding={density.cardPadding} radius="md">
              <Title order={4}>在线更新</Title>
              <Text size="sm" c="dimmed" mb="sm">
                管理员手动触发的自更新。当前版本见「关于·版本」。
              </Text>
              <Stack gap="sm">
                <Switch
                  label="启用在线更新（出站开关）"
                  checked={updateEnabled}
                  onChange={(e) => setUpdateEnabled(e.currentTarget.checked)}
                />
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
                      {check.notes && (
                        <Text size="sm" c="dimmed" mt="sm" style={{ whiteSpace: 'pre-wrap' }}>
                          {check.notes}
                        </Text>
                      )}
                    </Card>
                  )}
                </Stack>
              )}
            </Card>
          </Stack>
        </Tabs.Panel>

        {/* —— 关于·版本 —— */}
        <Tabs.Panel value="about" keepMounted pl="md">
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
        </Tabs.Panel>
      </Tabs>

      {/* —— 保存（网络代理 + 在线更新共用一次 PATCH，沿用 FR-88 既有逻辑）—— */}
      <Group>
        <Button leftSection={<IconDeviceFloppy size={16} />} onClick={handleSave} loading={saving}>
          保存
        </Button>
        {saved && (
          <Text c="green" size="sm">
            已保存，配置已即时生效。
          </Text>
        )}
      </Group>
      {saveError && <ErrorAlert message={saveError} />}

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
