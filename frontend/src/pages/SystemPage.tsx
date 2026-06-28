// 系统管理页（FR-109，仅管理员）：用 Tabs 分三页——
// 「在线更新」（从设置页迁来的应用更新卡片：通道切换 / 检查更新 / 版本对比 / 预发布提示 /
//   release 说明 / 启用开关 / 应用进度条 / 应用并重启 / 回滚 / 高级设置折叠，含本 tab 自己的保存按钮，
//   保存只发 update 块的部分 PATCH /settings）、
// 「重启」（重启服务，二次确认后调 POST /system/restart）、
// 「关闭」（关闭服务，危险操作，二次确认后调 POST /system/shutdown）。
//
// 在线更新数据来自 GET /api/v1/settings（已脱敏：更新 token 仅以 has_token 暴露）；
// 保存走部分 PATCH /settings（只发 update 块），token 三态：留空=保留、清空动作不适用此处、填新值=设置。
// apply 仍为单次阻塞 POST、无字节级回传，进度条为客户端模拟推进（契约不改）。

import { useEffect, useRef, useState } from 'react';
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
  Progress,
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
  IconCloudDownload,
  IconReload,
  IconPower,
  IconAlertTriangle,
} from '@tabler/icons-react';
import { ApiError } from '../api/client';
import * as api from '../api/endpoints';
import type { SettingsView, UpdateCheck } from '../api/types';
import { errorMessage } from '../lib/format';
import { notifyError, notifySuccess } from '../lib/notify';
import { ErrorAlert } from '../components/ErrorAlert';
import { density } from '../theme/density';

/** 系统管理页。 */
export function SystemPage() {
  const [settings, setSettings] = useState<SettingsView | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  // —— 在线更新表单态（自设置页迁来）——
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
  // 应用更新进度：null = 未在应用；0~95 = 客户端模拟推进百分比（封顶不到 100，待请求 resolve 进入重启态）。
  const [applyProgress, setApplyProgress] = useState<number | null>(null);
  const applyTimerRef = useRef<ReturnType<typeof setInterval> | null>(null);
  const [confirmOpened, confirmModal] = useDisclosure(false);
  // 回滚相关状态（FR-104）：进行中标志、错误、二次确认弹窗开合
  const [rollingBack, setRollingBack] = useState(false);
  const [rollbackError, setRollbackError] = useState<string | null>(null);
  const [rollbackConfirmOpened, rollbackConfirmModal] = useDisclosure(false);
  // 在线更新「高级设置」折叠区开合：默认收起，低频项（仓库源 / API 基址 / 重启模式 / 访问令牌）点开才显
  const [advancedOpened, advancedToggle] = useDisclosure(false);

  // —— 系统操作（重启 / 关闭）态 ——
  const [restartConfirmOpened, restartConfirmModal] = useDisclosure(false);
  const [restartSubmitting, setRestartSubmitting] = useState(false);
  const [shutdownConfirmOpened, shutdownConfirmModal] = useDisclosure(false);
  const [shutdownSubmitting, setShutdownSubmitting] = useState(false);

  // 用一份设置填充在线更新表单态。
  function fillForm(s: SettingsView) {
    setSettings(s);
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

  // 组件卸载时清理应用进度定时器，避免泄漏。
  useEffect(() => {
    return () => {
      if (applyTimerRef.current) {
        clearInterval(applyTimerRef.current);
      }
    };
  }, []);

  // 保存在线更新配置：只发 update 块的部分 PATCH /settings（部分更新已支持）。
  // token 留空则省略（保留现有）；填值则设置新 token。
  async function handleSaveUpdate() {
    setSaving(true);
    setSaveError(null);
    setSaved(false);
    try {
      const updated = await api.updateSettings({
        update: {
          enabled: updateEnabled,
          repo: repo.trim(),
          api_base_url: apiBaseUrl.trim(),
          restart_mode: restartMode,
          channel,
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

  /** 清理应用进度定时器（成功 / 失败 / 卸载共用，避免重复样板）。 */
  function clearApplyTimer() {
    if (applyTimerRef.current) {
      clearInterval(applyTimerRef.current);
      applyTimerRef.current = null;
    }
  }

  async function handleApply() {
    setApplying(true);
    setApplyError(null);
    // 启动客户端模拟进度：apply 是单次阻塞 POST、无字节级回传，进度为时间驱动体感反馈。
    // 平滑爬升至 95% 封顶（不到 100），待请求 resolve（进入重启态）或失败再收尾。
    confirmModal.close();
    setApplyProgress(0);
    clearApplyTimer();
    applyTimerRef.current = setInterval(() => {
      setApplyProgress((prev) => {
        if (prev === null) return prev;
        // 越接近上限步进越小，模拟收尾放缓
        const next = prev + Math.max(1, Math.round((95 - prev) / 12));
        return next >= 95 ? 95 : next;
      });
    }, 400);
    try {
      await api.applyUpdate();
      // apply 成功即服务将停机重启，当前连接会断；进入「正在重启」提示态、引导手动刷新
      clearApplyTimer();
      setRestarting(true);
    } catch (err) {
      clearApplyTimer();
      setApplyProgress(null);
      setApplyError(errorMessage(err));
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

  // 把系统操作错误转成面向用户的中文文案：409（更新进行中）单独提示。
  function systemActionMessage(err: unknown): string {
    if (err instanceof ApiError && err.status === 409) {
      return '更新进行中，请稍后';
    }
    return errorMessage(err);
  }

  async function handleRestartService() {
    setRestartSubmitting(true);
    try {
      await api.systemRestart();
      restartConfirmModal.close();
      notifySuccess('正在重启…当前连接将断开，请稍候片刻后手动刷新页面');
    } catch (err) {
      notifyError(systemActionMessage(err));
    } finally {
      setRestartSubmitting(false);
    }
  }

  async function handleShutdownService() {
    setShutdownSubmitting(true);
    try {
      await api.systemShutdown();
      shutdownConfirmModal.close();
      notifySuccess('正在关闭…服务将停止，需在服务器上重新启动');
    } catch (err) {
      notifyError(systemActionMessage(err));
    } finally {
      setShutdownSubmitting(false);
    }
  }

  if (loading) {
    return (
      <Center h={200}>
        <Loader />
      </Center>
    );
  }

  return (
    <Stack gap={density.gridSpacing}>
      <Title order={2}>系统</Title>
      {error && <ErrorAlert message={error} />}

      <Tabs defaultValue="update">
        <Tabs.List>
          <Tabs.Tab value="update" leftSection={<IconCloudDownload size={16} />}>
            在线更新
          </Tabs.Tab>
          <Tabs.Tab value="restart" leftSection={<IconReload size={16} />}>
            重启
          </Tabs.Tab>
          <Tabs.Tab value="shutdown" leftSection={<IconPower size={16} />}>
            关闭
          </Tabs.Tab>
        </Tabs.List>

        {/* —— 在线更新 tab —— */}
        <Tabs.Panel value="update" pt="md">
          {!settings ? (
            <ErrorAlert message={error ?? '无法加载在线更新配置'} />
          ) : (
            <Stack gap={density.gridSpacing}>
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

                  {/* 应用进度条：apply 在途时显示客户端模拟进度（封顶不到 100），
                      有 asset_name 时附资产名说明。成功进入重启态即撤下。 */}
                  {applyProgress !== null && !restarting && (
                    <Box data-testid="apply-progress">
                      <Group justify="space-between" mb={4} gap="xs">
                        <Text size="sm">正在下载并替换新版本…</Text>
                        <Text size="sm" c="dimmed">
                          {applyProgress}%
                        </Text>
                      </Group>
                      <Progress value={applyProgress} animated />
                      <Text size="xs" c="dimmed" mt={4}>
                        {check?.asset_name
                          ? `资产 ${check.asset_name}（进度为体感估算，实际以服务端替换为准）`
                          : '进度为体感估算，实际以服务端替换为准。'}
                      </Text>
                    </Box>
                  )}

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
                        disabled={!check?.update_available || applying}
                      >
                        立即更新并重启
                      </Button>
                      {/* 回滚到上一版（FR-104）：无备份时禁用；回滚是本地操作、不依赖在线更新开关 */}
                      <Button
                        variant="default"
                        leftSection={<IconArrowBackUp size={16} />}
                        onClick={rollbackConfirmModal.open}
                        disabled={!settings.update.rollback_available || applying}
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

                  {/* 高级设置折叠区：低频项默认收起，点开才显示编辑 */}
                  <Box>
                    <Button
                      variant="subtle"
                      size="xs"
                      px={0}
                      leftSection={
                        advancedOpened ? (
                          <IconChevronDown size={16} />
                        ) : (
                          <IconChevronRight size={16} />
                        )
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

              {/* 在线更新保存：只发 update 块的部分 PATCH /settings（即时生效） */}
              <Group>
                <Button
                  leftSection={<IconDeviceFloppy size={16} />}
                  onClick={handleSaveUpdate}
                  loading={saving}
                >
                  保存
                </Button>
                {saved && (
                  <Text c="green" size="sm">
                    已保存。在线更新配置即时生效。
                  </Text>
                )}
              </Group>
              {saveError && <ErrorAlert message={saveError} />}
            </Stack>
          )}
        </Tabs.Panel>

        {/* —— 重启 tab —— */}
        <Tabs.Panel value="restart" pt="md">
          <Card withBorder padding={density.cardPadding} radius="md">
            <Stack gap="sm">
              <Title order={4}>重启服务</Title>
              <Text size="sm" c="dimmed">
                重启服务进程。重启期间服务将短暂不可用，当前连接会断开，重启完成后需手动刷新页面。
              </Text>
              <Group>
                <Button
                  color="orange"
                  leftSection={<IconReload size={16} />}
                  onClick={restartConfirmModal.open}
                >
                  重启服务
                </Button>
              </Group>
            </Stack>
          </Card>
        </Tabs.Panel>

        {/* —— 关闭 tab —— */}
        <Tabs.Panel value="shutdown" pt="md">
          <Card withBorder padding={density.cardPadding} radius="md">
            <Stack gap="sm">
              <Title order={4}>关闭服务</Title>
              <Text size="sm" c="dimmed">
                关闭服务进程。关闭后服务将停止，无法从本控制台再次启动，需在服务器上经进程管理器
                （systemd / docker 等）重新拉起。
              </Text>
              <Group>
                <Button
                  color="red"
                  leftSection={<IconPower size={16} />}
                  onClick={shutdownConfirmModal.open}
                >
                  关闭服务
                </Button>
              </Group>
            </Stack>
          </Card>
        </Tabs.Panel>
      </Tabs>

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

      {/* —— 重启二次确认弹窗（FR-109）—— */}
      <Modal
        opened={restartConfirmOpened}
        onClose={restartConfirmModal.close}
        title="确认重启服务"
        centered
      >
        <Stack>
          <Text>
            将重启服务进程。重启期间服务短暂不可用，当前连接会断开，完成后需手动刷新页面。确认继续？
          </Text>
          <Group justify="flex-end">
            <Button
              variant="default"
              onClick={restartConfirmModal.close}
              disabled={restartSubmitting}
            >
              取消
            </Button>
            <Button color="orange" onClick={handleRestartService} loading={restartSubmitting}>
              确认重启
            </Button>
          </Group>
        </Stack>
      </Modal>

      {/* —— 关闭二次确认弹窗（FR-109）—— */}
      <Modal
        opened={shutdownConfirmOpened}
        onClose={shutdownConfirmModal.close}
        title="确认关闭服务"
        centered
      >
        <Stack>
          <Alert icon={<IconAlertTriangle size={16} />} color="red" variant="light" title="警告">
            关闭后服务将停止，无法从本控制台再次启动，需在服务器上经进程管理器（systemd / docker
            等）重新拉起。
          </Alert>
          <Text>确认关闭服务？</Text>
          <Group justify="flex-end">
            <Button
              variant="default"
              onClick={shutdownConfirmModal.close}
              disabled={shutdownSubmitting}
            >
              取消
            </Button>
            <Button color="red" onClick={handleShutdownService} loading={shutdownSubmitting}>
              确认关闭
            </Button>
          </Group>
        </Stack>
      </Modal>
    </Stack>
  );
}
