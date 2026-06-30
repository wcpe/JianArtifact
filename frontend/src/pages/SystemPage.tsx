// 系统管理页（FR-109，仅管理员）：用 Tabs 分三页——
// 「在线更新」（从设置页迁来的应用更新卡片：通道切换 / 检查更新 / 版本对比 / 预发布提示 /
//   release 说明 / 启用开关 / 真实阶段进度 / 应用并重启 / 回滚 / 高级设置折叠，含本 tab 自己的保存按钮，
//   保存只发 update 块的部分 PATCH /settings）、
// 「重启」（重启服务，二次确认后调 POST /system/restart）、
// 「关闭」（关闭服务，危险操作，二次确认后调 POST /system/shutdown）。
//
// 在线更新配置来自 GET /api/v1/settings（已脱敏：更新 token 仅以 has_token 暴露）；
// 保存走部分 PATCH /settings（只发 update 块），token 三态：留空=保留、清空动作不适用此处、填新值=设置。
//
// FR-126 异步化：检查 / 应用 / 回滚改为后台 job——触发得 job_id，setInterval 轮询
// GET /update/jobs/{id} 渲染**真实阶段进度**（替换旧客户端假进度），终态停轮询。检查结果经
// GET /update/check 留存（进页即显上次结果，不必每次重检）；apply 终态跨重启留存，进页经
// GET /update/jobs 回填「上次更新结果」续看，apply 的 job_id 记 localStorage 供刷新重连。

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
import { useTranslation } from 'react-i18next';
import { ApiError } from '../api/client';
import * as api from '../api/endpoints';
import type { SettingsView, UpdateCheck, UpdateJob, UpdatePhase } from '../api/types';
import { errorMessage } from '../lib/format';
import { notifyError, notifySuccess } from '../lib/notify';
import { ErrorAlert } from '../components/ErrorAlert';
import { density } from '../theme/density';

/** 应用更新任务 job_id 的 localStorage 键名（供刷新 / 重连续看，FR-126）。 */
const UPDATE_JOB_STORAGE_KEY = 'jian.update.applyJobId';

/** 更新任务阶段是否为终态（轮询见终态即停）。 */
function isTerminalPhase(phase: UpdatePhase): boolean {
  return phase === 'restarting' || phase === 'done' || phase === 'failed';
}

/** 阶段→中文进度文案（按阶段反馈，不做假百分比，FR-126）。 */
function phaseLabel(t: (k: string) => string, phase: UpdatePhase): string {
  switch (phase) {
    case 'checking':
      return t('phaseChecking');
    case 'downloading':
      return t('phaseDownloading');
    case 'verifying':
      return t('phaseVerifying');
    case 'replacing':
      return t('phaseReplacing');
    case 'restarting':
      return t('phaseRestarting');
    default:
      return t('phaseDoneCheck');
  }
}

/** 系统管理页。 */
export function SystemPage() {
  const { t } = useTranslation('system');
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

  // 更新检查 / 应用相关状态（FR-126 异步化：检查 / 应用 / 回滚均为后台 job，前端轮询真实阶段进度）
  const [check, setCheck] = useState<UpdateCheck | null>(null);
  const [checkedAt, setCheckedAt] = useState<number | null>(null);
  const [checking, setChecking] = useState(false);
  const [checkError, setCheckError] = useState<string | null>(null);
  const [applying, setApplying] = useState(false);
  const [applyError, setApplyError] = useState<string | null>(null);
  const [restarting, setRestarting] = useState(false);
  // 当前在途 / 近期更新 job 的进度快照（null = 无活动 job）；非检查类终态进入「正在重启」/「失败」提示。
  const [job, setJob] = useState<UpdateJob | null>(null);
  // 轮询定时器：触发检查 / 应用 / 回滚后 setInterval 拉 GET /update/jobs/{id}，见终态即停。
  const pollTimerRef = useRef<ReturnType<typeof setInterval> | null>(null);
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
    // 进页读留存的上次检查结果（不联网），免去每次重点检查
    api
      .getCachedCheck()
      .then((cached) => {
        if (cached.result) {
          setCheck(cached.result);
          setCheckedAt(cached.checked_at);
        }
      })
      .catch(() => {
        // 留存读取失败不阻断页面（仅影响「上次检查结果」展示）
      });
    // 重连续看：若有进行中 / 重启后回填的更新 job，挑最新一条恢复进度展示
    api
      .listUpdateJobs()
      .then((list) => {
        if (list.length === 0) return;
        const latest = list[list.length - 1];
        applyJobSnapshot(latest);
        // 未到终态则继续轮询其进度
        if (!isTerminalPhase(latest.phase)) {
          startPolling(latest.job_id);
        }
      })
      .catch(() => {
        // 列表读取失败不阻断页面
      });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // 组件卸载时清理轮询定时器，避免泄漏。
  useEffect(() => {
    return () => clearPollTimer();
  }, []);

  /** 清理轮询定时器（成功 / 失败 / 卸载共用）。 */
  function clearPollTimer() {
    if (pollTimerRef.current) {
      clearInterval(pollTimerRef.current);
      pollTimerRef.current = null;
    }
  }

  /** 据 job 进度快照刷新页面态：检查类终态填检查结果，应用 / 回滚终态进入重启 / 失败提示。 */
  function applyJobSnapshot(snapshot: UpdateJob) {
    setJob(snapshot);
    if (snapshot.kind === 'check') {
      if (snapshot.phase === 'done' && snapshot.check) {
        setCheck(snapshot.check);
        setCheckedAt(Math.floor(Date.now() / 1000));
      } else if (snapshot.phase === 'failed') {
        setCheckError(snapshot.error ?? t('jobFailedTitle'));
      }
    } else {
      // apply / rollback：本进程内 restarting 视为「已触发更新、连接将断」（蓝色提示）；
      // 但 restarted=true 是重启后从状态文件回填的历史终态——新进程已起来，改显「上次更新结果」
      //（绿色），不再提示「正在重启」。
      if (snapshot.phase === 'restarting' && !snapshot.restarted) {
        setRestarting(true);
      } else if (snapshot.phase === 'failed') {
        setApplyError(snapshot.error ?? t('jobFailedTitle'));
      }
    }
  }

  /** 启动轮询：每 800ms 拉一次 job 进度，见终态即停。 */
  function startPolling(jobId: string) {
    clearPollTimer();
    pollTimerRef.current = setInterval(() => {
      api
        .getUpdateJob(jobId)
        .then((snapshot) => {
          applyJobSnapshot(snapshot);
          if (isTerminalPhase(snapshot.phase)) {
            clearPollTimer();
            setChecking(false);
            setApplying(false);
            setRollingBack(false);
          }
        })
        .catch(() => {
          // 轮询失败（如服务正在重启导致连接断）：停轮询，保留当前进度态供用户手动刷新
          clearPollTimer();
          setChecking(false);
          setApplying(false);
        });
    }, 800);
  }

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

  // 触发联网检查（FR-126 异步）：起后台检查 job，得 job_id 后轮询真实阶段进度，终态填检查结果。
  async function handleCheck() {
    setChecking(true);
    setCheckError(null);
    setJob(null);
    try {
      const created = await api.triggerCheckUpdate();
      startPolling(created.job_id);
    } catch (err) {
      // 触发失败（如未启用 409）即时回显，不进轮询
      setChecking(false);
      setCheckError(errorMessage(err));
    }
  }

  // 触发应用更新（FR-126 异步）：起后台 apply job，记 job_id 于 localStorage 供刷新重连，轮询进度。
  async function handleApply() {
    setApplying(true);
    setApplyError(null);
    confirmModal.close();
    try {
      const created = await api.applyUpdate();
      try {
        localStorage.setItem(UPDATE_JOB_STORAGE_KEY, created.job_id);
      } catch {
        // localStorage 不可用（隐私模式等）不影响本次轮询
      }
      startPolling(created.job_id);
    } catch (err) {
      setApplying(false);
      setApplyError(errorMessage(err));
    }
  }

  // 触发回滚（FR-104 + FR-126 异步）：起后台 rollback job，轮询进度，终态进入重启 / 失败提示。
  async function handleRollback() {
    setRollingBack(true);
    setRollbackError(null);
    rollbackConfirmModal.close();
    try {
      const created = await api.rollbackUpdate();
      startPolling(created.job_id);
    } catch (err) {
      setRollingBack(false);
      setRollbackError(errorMessage(err));
    }
  }

  // 把系统操作错误转成面向用户的中文文案：409（更新进行中）单独提示。
  function systemActionMessage(err: unknown): string {
    if (err instanceof ApiError && err.status === 409) {
      return t('updateInProgress');
    }
    return errorMessage(err);
  }

  async function handleRestartService() {
    setRestartSubmitting(true);
    try {
      await api.systemRestart();
      restartConfirmModal.close();
      notifySuccess(t('restartingNotice'));
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
      notifySuccess(t('shuttingDownNotice'));
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
      <Title order={2}>{t('pageTitle')}</Title>
      {error && <ErrorAlert message={error} />}

      <Tabs defaultValue="update">
        <Tabs.List>
          <Tabs.Tab value="update" leftSection={<IconCloudDownload size={16} />}>
            {t('tabUpdate')}
          </Tabs.Tab>
          <Tabs.Tab value="restart" leftSection={<IconReload size={16} />}>
            {t('tabRestart')}
          </Tabs.Tab>
          <Tabs.Tab value="shutdown" leftSection={<IconPower size={16} />}>
            {t('tabShutdown')}
          </Tabs.Tab>
        </Tabs.List>

        {/* —— 在线更新 tab —— */}
        <Tabs.Panel value="update" pt="md">
          {!settings ? (
            <ErrorAlert message={error ?? t('loadConfigFailed')} />
          ) : (
            <Stack gap={density.gridSpacing}>
              <Card withBorder padding={density.cardPadding} radius="md">
                {/* 卡片头：左标题，右侧通道切换（正式版 / 测试版）+ 检查更新 */}
                <Group justify="space-between" align="flex-start" mb="sm" wrap="nowrap">
                  <Box>
                    <Title order={4}>{t('updateCardTitle')}</Title>
                    <Text size="sm" c="dimmed">
                      {t('updateCardDesc')}
                    </Text>
                  </Box>
                  <Group gap="xs" wrap="nowrap">
                    <SegmentedControl
                      size="xs"
                      value={channel}
                      onChange={setChannel}
                      data={[
                        { value: 'stable', label: t('channelStable') },
                        { value: 'prerelease', label: t('channelPrerelease') },
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
                      {t('checkUpdate')}
                    </Button>
                  </Group>
                </Group>

                <Stack gap="sm">
                  {/* 启用在线更新（出站开关）：高频项留卡内可见处 */}
                  <Switch
                    label={t('enableUpdateSwitch')}
                    checked={updateEnabled}
                    onChange={(e) => setUpdateEnabled(e.currentTarget.checked)}
                  />

                  {/* 测试版（prerelease）通道提示：滚动开发预览，可能不稳定 */}
                  {channel === 'prerelease' && (
                    <Alert
                      icon={<IconInfoCircle size={16} />}
                      color="yellow"
                      variant="light"
                      title={t('prereleaseAlertTitle')}
                    >
                      {t('prereleaseAlertBody')}
                    </Alert>
                  )}

                  {/* 版本对比 + 徽标：当前 ↔ 最新（检查后），预发布徽标随通道显隐 */}
                  <Card withBorder padding="sm" radius="sm" bg="var(--mantine-color-gray-0)">
                    <Group gap="xs">
                      <Text size="sm">{t('currentVersion')}</Text>
                      <Badge variant="light">
                        {check?.current_version ?? settings.current_version}
                      </Badge>
                      {check && (
                        <>
                          <Text size="sm">{t('latestVersionArrow')}</Text>
                          <Code>{check.latest_version}</Code>
                          {check.update_available ? (
                            <Badge color="orange">{t('updateAvailableBadge')}</Badge>
                          ) : (
                            <Badge color="green">{t('upToDateBadge')}</Badge>
                          )}
                        </>
                      )}
                      {channel === 'prerelease' && (
                        <Badge color="yellow" variant="light">
                          {t('prereleaseBadge')}
                        </Badge>
                      )}
                    </Group>
                    {/* 上次检查时刻（FR-126：留存读回时展示，提示结果非实时） */}
                    {check && checkedAt && (
                      <Text size="xs" c="dimmed" mt={4}>
                        {t('lastCheckedAt', {
                          time: new Date(checkedAt * 1000).toLocaleString(),
                        })}
                      </Text>
                    )}
                    {/* 检查到的 release 发布说明（notes 即 release body），无说明优雅留空 */}
                    {check?.notes && (
                      <>
                        <Text size="xs" c="dimmed" fw={600} mt="sm">
                          {t('releaseNotes')}
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
                      title={t('updateDisabledAlertTitle')}
                    >
                      {t('updateDisabledAlertBody')}
                    </Alert>
                  )}

                  {checkError && <ErrorAlert message={checkError} />}
                  {applyError && <ErrorAlert message={applyError} />}
                  {rollbackError && <ErrorAlert message={rollbackError} />}

                  {/* 真实阶段进度（FR-126）：有活动 job 且未到终态时，按阶段展示进度（不做假百分比）。
                      下载阶段若有资产名则附资产说明。终态由下方重启 / 失败提示接管。 */}
                  {job && !isTerminalPhase(job.phase) && (
                    <Box data-testid="update-progress">
                      <Text size="sm" mb={4}>
                        {t('updateInProgressTitle')}：{phaseLabel(t, job.phase)}
                      </Text>
                      <Progress value={100} animated striped />
                      {job.phase === 'downloading' && check?.asset_name && (
                        <Text size="xs" c="dimmed" mt={4}>
                          {t('progressHintWithAsset', { name: check.asset_name })}
                        </Text>
                      )}
                    </Box>
                  )}

                  {/* 重启后续看（FR-126）：从状态文件回填的上次 apply 终态，提示已升级到的版本 */}
                  {job?.restarted && job.new_version && !restarting && (
                    <Alert
                      icon={<IconInfoCircle size={16} />}
                      color="green"
                      variant="light"
                      title={t('lastUpdateResultTitle')}
                    >
                      {t('lastUpdateRestarted', { version: job.new_version })}
                    </Alert>
                  )}

                  {restarting && (
                    <Alert
                      icon={<IconInfoCircle size={16} />}
                      color="blue"
                      variant="light"
                      title={t('upgradeTriggeredAlertTitle')}
                    >
                      {t('upgradeTriggeredAlertBody')}
                    </Alert>
                  )}

                  {!restarting && (
                    <Group>
                      {/* 立即更新并重启：有可用更新时高亮可点；否则禁用（无更新无可应用对象） */}
                      <Button
                        color="orange"
                        leftSection={<IconArrowUp size={16} />}
                        onClick={confirmModal.open}
                        disabled={!check?.update_available || applying || rollingBack}
                      >
                        {t('applyNow')}
                      </Button>
                      {/* 回滚到上一版（FR-104）：无备份时禁用；回滚是本地操作、不依赖在线更新开关 */}
                      <Button
                        variant="default"
                        leftSection={<IconArrowBackUp size={16} />}
                        onClick={rollbackConfirmModal.open}
                        disabled={!settings.update.rollback_available || applying || rollingBack}
                      >
                        {t('rollbackNow')}
                      </Button>
                    </Group>
                  )}

                  {!settings.update.rollback_available && (
                    <Text size="xs" c="dimmed">
                      {t('noRollbackBackup')}
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
                      {t('advancedSettingsToggle')}
                    </Button>
                    <Collapse in={advancedOpened}>
                      <Stack gap="sm" mt="sm">
                        <TextInput
                          label={t('repoLabel')}
                          placeholder="wcpe/JianArtifact"
                          value={repo}
                          onChange={(e) => setRepo(e.currentTarget.value)}
                        />
                        <TextInput
                          label={t('apiBaseUrlLabel')}
                          placeholder="https://api.github.com"
                          value={apiBaseUrl}
                          onChange={(e) => setApiBaseUrl(e.currentTarget.value)}
                        />
                        <Select
                          label={t('restartModeLabel')}
                          data={[
                            { value: 'self', label: t('restartModeSelf') },
                            { value: 'exit', label: t('restartModeExit') },
                          ]}
                          value={restartMode}
                          onChange={(v) => setRestartMode(v ?? 'self')}
                          allowDeselect={false}
                        />
                        <PasswordInput
                          label={t('tokenLabel')}
                          description={
                            hasToken ? t('tokenDescConfigured') : t('tokenDescUnconfigured')
                          }
                          placeholder={
                            hasToken
                              ? t('tokenPlaceholderConfigured')
                              : t('tokenPlaceholderUnconfigured')
                          }
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
                  {t('common:save')}
                </Button>
                {saved && (
                  <Text c="green" size="sm">
                    {t('saved')}
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
              <Title order={4}>{t('restartCardTitle')}</Title>
              <Text size="sm" c="dimmed">
                {t('restartCardDesc')}
              </Text>
              <Group>
                <Button
                  color="orange"
                  leftSection={<IconReload size={16} />}
                  onClick={restartConfirmModal.open}
                >
                  {t('restartButton')}
                </Button>
              </Group>
            </Stack>
          </Card>
        </Tabs.Panel>

        {/* —— 关闭 tab —— */}
        <Tabs.Panel value="shutdown" pt="md">
          <Card withBorder padding={density.cardPadding} radius="md">
            <Stack gap="sm">
              <Title order={4}>{t('shutdownCardTitle')}</Title>
              <Text size="sm" c="dimmed">
                {t('shutdownCardDesc')}
              </Text>
              <Group>
                <Button
                  color="red"
                  leftSection={<IconPower size={16} />}
                  onClick={shutdownConfirmModal.open}
                >
                  {t('shutdownButton')}
                </Button>
              </Group>
            </Stack>
          </Card>
        </Tabs.Panel>
      </Tabs>

      {/* —— 升级二次确认弹窗 —— */}
      <Modal
        opened={confirmOpened}
        onClose={confirmModal.close}
        title={t('upgradeModalTitle')}
        centered
      >
        <Stack>
          <Text>
            {t('upgradeConfirmPrefix')}
            <Code>v{check?.latest_version}</Code>
            {t('upgradeConfirmSuffix')}
          </Text>
          <Group justify="flex-end">
            <Button variant="default" onClick={confirmModal.close} disabled={applying}>
              {t('common:cancel')}
            </Button>
            <Button color="orange" onClick={handleApply} loading={applying}>
              {t('confirmUpgrade')}
            </Button>
          </Group>
        </Stack>
      </Modal>

      {/* —— 回滚二次确认弹窗（FR-104）—— */}
      <Modal
        opened={rollbackConfirmOpened}
        onClose={rollbackConfirmModal.close}
        title={t('rollbackModalTitle')}
        centered
      >
        <Stack>
          <Text>{t('rollbackConfirmBody')}</Text>
          <Group justify="flex-end">
            <Button variant="default" onClick={rollbackConfirmModal.close} disabled={rollingBack}>
              {t('common:cancel')}
            </Button>
            <Button color="red" onClick={handleRollback} loading={rollingBack}>
              {t('confirmRollback')}
            </Button>
          </Group>
        </Stack>
      </Modal>

      {/* —— 重启二次确认弹窗（FR-109）—— */}
      <Modal
        opened={restartConfirmOpened}
        onClose={restartConfirmModal.close}
        title={t('restartModalTitle')}
        centered
      >
        <Stack>
          <Text>{t('restartConfirmBody')}</Text>
          <Group justify="flex-end">
            <Button
              variant="default"
              onClick={restartConfirmModal.close}
              disabled={restartSubmitting}
            >
              {t('common:cancel')}
            </Button>
            <Button color="orange" onClick={handleRestartService} loading={restartSubmitting}>
              {t('confirmRestart')}
            </Button>
          </Group>
        </Stack>
      </Modal>

      {/* —— 关闭二次确认弹窗（FR-109）—— */}
      <Modal
        opened={shutdownConfirmOpened}
        onClose={shutdownConfirmModal.close}
        title={t('shutdownModalTitle')}
        centered
      >
        <Stack>
          <Alert
            icon={<IconAlertTriangle size={16} />}
            color="red"
            variant="light"
            title={t('shutdownWarningTitle')}
          >
            {t('shutdownWarningBody')}
          </Alert>
          <Text>{t('shutdownConfirmBody')}</Text>
          <Group justify="flex-end">
            <Button
              variant="default"
              onClick={shutdownConfirmModal.close}
              disabled={shutdownSubmitting}
            >
              {t('common:cancel')}
            </Button>
            <Button color="red" onClick={handleShutdownService} loading={shutdownSubmitting}>
              {t('confirmShutdown')}
            </Button>
          </Group>
        </Stack>
      </Modal>
    </Stack>
  );
}
