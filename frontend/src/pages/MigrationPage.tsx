// Nexus 迁移管理页面（FR-81 / FR-82，对接 ADR-0006 已有迁移端点，仅 Admin）。
//
// 多步流程（Mantine Stepper）：
//   ① 选迁移形态（在线 REST / 离线 blob store）+ 填源 → 预览可迁移仓库列表（不搬运）。
//   ② 勾选要搬运的仓库 + 选迁移方式：
//        · 在线拉取（FR-82）：经 REST 枚举 + HTTP 下载同步，无需离线目录；
//          可为每个仓库选填目标仓库名（默认与源同名）；仅 maven2 hosted 有效。
//        · 离线目录（FR-81）：填离线 blob store 路径，执行 proxy / hosted 搬运。
//   ③ 展示迁移报告（每仓库已迁 / 跳过数、整仓跳过列表）。
//
// 凭据脱敏（红线）：源 Nexus 凭据仅以「引用名 auth_ref」形式输入，用口令型输入框承载、
// 真值在后端 env 解析，前端绝不回显明文、不持久化。

import { useEffect, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import type { TFunction } from 'i18next';
import {
  Stack,
  Title,
  Text,
  Stepper,
  SegmentedControl,
  TextInput,
  PasswordInput,
  Button,
  Group,
  Table,
  Checkbox,
  Badge,
  Card,
  Loader,
  Center,
  Alert,
  Progress,
} from '@mantine/core';
import * as api from '../api/endpoints';
import { ApiError } from '../api/client';
import type {
  MigrationReport,
  NexusRepoSummary,
  OfflineRepoSummary,
  OnlinePullJob,
} from '../api/types';
import { errorMessage } from '../lib/format';
import { ErrorAlert } from '../components/ErrorAlert';
import { notifySuccess } from '../lib/notify';

/** 迁移形态：在线 REST 入口 / 离线 blob store 入口。 */
type SourceMode = 'online' | 'offline';

/** 迁移方式：在线拉取（HTTP 下载，无需本地目录）/ 离线目录（直接访问 blob store 目录）。 */
type MigrateMethod = 'online' | 'offline';

/** 离线目录搬运目标类型：proxy 仓库 / hosted 仓库。 */
type MigrateKind = 'proxy' | 'hosted';

/** 预览到的仓库名集合（在线与离线归一为「仓库名 + 类型/计数」用于展示与勾选）。 */
interface PreviewRow {
  /** 仓库名（在线取 name，离线取 repo_name）。 */
  name: string;
  /** 在线：格式；离线：'-'。 */
  format: string;
  /** 在线：hosted/proxy；离线：blob 数量文案。 */
  detail: string;
}

/** 把在线预览结果归一为展示行。 */
function fromOnline(repos: NexusRepoSummary[]): PreviewRow[] {
  return repos.map((r) => ({ name: r.name, format: r.format, detail: r.type }));
}

/** 把离线预览结果归一为展示行（t 由调用方组件传入，模块级函数自身无法使用 hook）。 */
function fromOffline(repos: OfflineRepoSummary[], t: TFunction): PreviewRow[] {
  return repos.map((r) => ({
    name: r.repo_name,
    format: '-',
    detail: t('preview.blobCount', { count: r.blob_count }),
  }));
}

/** 在线拉取任务轮询间隔（毫秒）。 */
const ONLINE_POLL_INTERVAL_MS = 1500;

/** 在线拉取任务 job_id 的 localStorage 键名（供客户端重连续看）。 */
const ONLINE_JOB_STORAGE_KEY = 'jian.migrate.online.jobId';

/** 任务是否处于终态（done / failed / cancelled）：终态即停止轮询并展示结果。 */
function isTerminalPhase(phase: OnlinePullJob['phase']): boolean {
  return phase === 'done' || phase === 'failed' || phase === 'cancelled';
}

/** 阶段中文标签（t 由调用方组件传入，模块级函数自身无法使用 hook）。 */
function phaseLabel(phase: OnlinePullJob['phase'], t: TFunction): string {
  switch (phase) {
    case 'enumerating':
      return t('phase.enumerating');
    case 'downloading':
      return t('phase.downloading');
    case 'paused':
      return t('phase.paused');
    case 'cancelled':
      return t('phase.cancelled');
    case 'done':
      return t('phase.done');
    case 'failed':
      return t('phase.failed');
  }
}

/** 阶段对应的 Badge 颜色。 */
function phaseColor(phase: OnlinePullJob['phase']): string {
  switch (phase) {
    case 'failed':
      return 'red';
    case 'done':
      return 'green';
    case 'paused':
      return 'yellow';
    case 'cancelled':
      return 'gray';
    default:
      return 'blue';
  }
}

/** Nexus 迁移管理页面。 */
export function MigrationPage() {
  const { t } = useTranslation('migration');
  const [active, setActive] = useState(0);

  // —— 源配置（步骤 ①）——
  const [mode, setMode] = useState<SourceMode>('online');
  const [baseUrl, setBaseUrl] = useState('');
  const [authRef, setAuthRef] = useState('');
  const [offlinePath, setOfflinePath] = useState('');

  // —— 预览结果（步骤 ①→②）——
  const [rows, setRows] = useState<PreviewRow[]>([]);
  const [previewing, setPreviewing] = useState(false);
  const [previewError, setPreviewError] = useState<string | null>(null);

  // —— 勾选与搬运（步骤 ②）——
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [method, setMethod] = useState<MigrateMethod>('online');
  const [migratePath, setMigratePath] = useState('');
  // 在线拉取：源仓库名 → 目标仓库名改名映射（空 / 缺省即与源同名）。
  const [renames, setRenames] = useState<Record<string, string>>({});
  const [migrating, setMigrating] = useState(false);
  const [migrateError, setMigrateError] = useState<string | null>(null);

  // —— 报告（步骤 ③）——
  // 离线目录报告同步返回；在线拉取改为异步任务，以进度快照承载（含终态报告）。
  const [report, setReport] = useState<MigrationReport | null>(null);
  // 在线拉取任务的当前进度快照（轮询刷新；null 表示尚无在线任务）。
  const [onlineJob, setOnlineJob] = useState<OnlinePullJob | null>(null);
  // 正在轮询的任务 id（非 null 即开启轮询；终态或清理时置 null）。
  const [pollingJobId, setPollingJobId] = useState<string | null>(null);

  /** auth_ref 为空白时按未提供处理（匿名源）。 */
  const authRefValue = authRef.trim() === '' ? undefined : authRef.trim();

  /** 执行预览：据形态调用在线 / 离线预览端点，归一展示行。 */
  const handlePreview = async () => {
    setPreviewError(null);
    setPreviewing(true);
    try {
      if (mode === 'online') {
        const repos = await api.previewNexusRepositories({
          base_url: baseUrl.trim(),
          auth_ref: authRefValue,
        });
        setRows(fromOnline(repos));
      } else {
        const repos = await api.previewNexusOffline({ path: offlinePath.trim() });
        setRows(fromOffline(repos, t));
      }
      setSelected(new Set());
      setRenames({});
    } catch (err) {
      setPreviewError(errorMessage(err));
    } finally {
      setPreviewing(false);
    }
  };

  /** 切换单个仓库勾选。 */
  const toggleSelect = (name: string) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(name)) {
        next.delete(name);
      } else {
        next.add(name);
      }
      return next;
    });
  };

  /** 设置某源仓库的目标改名。 */
  const setRename = (source: string, target: string) => {
    setRenames((prev) => ({ ...prev, [source]: target }));
  };

  /** 执行离线目录搬运：按目标类型调用 proxy / hosted 端点，得到报告并进入步骤 ③。 */
  const handleMigrateOffline = async (kind: MigrateKind) => {
    setMigrateError(null);
    setMigrating(true);
    try {
      const req = {
        base_url: baseUrl.trim(),
        auth_ref: authRefValue,
        offline_path: migratePath.trim(),
      };
      const result =
        kind === 'proxy' ? await api.migrateNexusProxy(req) : await api.migrateNexusHosted(req);
      setReport(result);
      // 离线目录执行时清理在线任务态，报告区只显示离线结果。
      setOnlineJob(null);
      setPollingJobId(null);
      notifySuccess(t('toast.offlineDone'));
      setActive(2);
    } catch (err) {
      setMigrateError(errorMessage(err));
    } finally {
      setMigrating(false);
    }
  };

  /**
   * 发起在线拉取迁移（异步）：把所选仓库（含改名）映射为请求，调用在线端点得 job_id；
   * 立即把 job_id 存档（供重连）、开启轮询并进入步骤 ③ 看进度队列。
   * 同步阶段失败（未选仓库 / 源不存在 / 凭据未配置 / 源不可达）就地展示错误，不进入步骤 ③。
   */
  const handleMigrateOnline = async () => {
    setMigrateError(null);
    setMigrating(true);
    try {
      const repositories = rows
        .filter((row) => selected.has(row.name))
        .map((row) => {
          const target = (renames[row.name] ?? '').trim();
          // 目标为空即与源同名：省略 target 字段交后端默认处理。
          return target === '' ? { source: row.name } : { source: row.name, target };
        });
      const { job_id } = await api.migrateNexusOnline({
        base_url: baseUrl.trim(),
        auth_ref: authRefValue,
        repositories,
      });
      // 存档 job_id（重连续看）；清理离线报告；以初始快照占位并开启轮询。
      localStorage.setItem(ONLINE_JOB_STORAGE_KEY, job_id);
      setReport(null);
      setOnlineJob(null);
      setPollingJobId(job_id);
      notifySuccess(t('toast.onlineStarted'));
      setActive(2);
    } catch (err) {
      setMigrateError(errorMessage(err));
    } finally {
      setMigrating(false);
    }
  };

  // 拉取一次任务进度快照：写入状态；终态则停止轮询并清存档；404 视为存档失效，清理。
  const fetchJobRef = useRef<(id: string) => Promise<void>>(async () => {});
  fetchJobRef.current = async (id: string) => {
    try {
      const job = await api.getMigrationJob(id);
      setOnlineJob(job);
      if (isTerminalPhase(job.phase)) {
        setPollingJobId(null);
        localStorage.removeItem(ONLINE_JOB_STORAGE_KEY);
      }
    } catch (err) {
      // 未知 job_id（任务已过期 / 被清理）：停止轮询并清存档，不再续看。
      if (err instanceof ApiError && err.status === 404) {
        setPollingJobId(null);
        setOnlineJob(null);
        localStorage.removeItem(ONLINE_JOB_STORAGE_KEY);
        return;
      }
      // 其他瞬时错误（如网络抖动）：保留轮询，下个周期重试。
      setMigrateError(errorMessage(err));
    }
  };

  // 轮询在线拉取任务进度：pollingJobId 非空即立即拉一次并定时刷新；终态 / 卸载时清理定时器。
  useEffect(() => {
    if (pollingJobId === null) {
      return;
    }
    void fetchJobRef.current(pollingJobId);
    const timer = setInterval(() => {
      void fetchJobRef.current(pollingJobId);
    }, ONLINE_POLL_INTERVAL_MS);
    return () => clearInterval(timer);
  }, [pollingJobId]);

  // 客户端重连：页面加载时若有存档 job_id，拉取其进度——
  // 仍在进行则恢复轮询续看（并切到报告步骤），终态则展示结果，404 则清存档。
  useEffect(() => {
    const archived = localStorage.getItem(ONLINE_JOB_STORAGE_KEY);
    if (!archived) {
      return;
    }
    let active = true;
    void (async () => {
      try {
        const job = await api.getMigrationJob(archived);
        if (!active) {
          return;
        }
        setOnlineJob(job);
        setActive(2);
        if (isTerminalPhase(job.phase)) {
          localStorage.removeItem(ONLINE_JOB_STORAGE_KEY);
        } else {
          setPollingJobId(archived);
        }
      } catch (err) {
        if (err instanceof ApiError && err.status === 404) {
          localStorage.removeItem(ONLINE_JOB_STORAGE_KEY);
        }
      }
    })();
    return () => {
      active = false;
    };
    // 仅在首次挂载时尝试重连（依赖均为稳定的 setter，无需列入依赖）。
  }, []);

  // 任务控制（FR-91）：取消 / 暂停 / 继续。控制端点幂等返回 200，调用后立即拉一次快照刷新按钮态。
  // 取消后任务转终态、轮询自停；暂停 / 继续后轮询照旧反映新态。
  const [controlling, setControlling] = useState(false);
  const runControl = async (action: (id: string) => Promise<void>, id: string) => {
    setControlling(true);
    setMigrateError(null);
    try {
      await action(id);
      await fetchJobRef.current(id);
    } catch (err) {
      // 未知 id（任务已过期 / 被清理）：清存档并停止轮询，不再续看。
      if (err instanceof ApiError && err.status === 404) {
        setPollingJobId(null);
        setOnlineJob(null);
        localStorage.removeItem(ONLINE_JOB_STORAGE_KEY);
      } else {
        setMigrateError(errorMessage(err));
      }
    } finally {
      setControlling(false);
    }
  };

  // 在线拉取：选中仓库且有源地址即可执行（无需离线路径）。
  const canMigrateOnline = selected.size > 0 && baseUrl.trim() !== '' && !migrating;
  // 离线目录：选中仓库 + 离线路径 + 源地址。
  const canMigrateOffline =
    selected.size > 0 && migratePath.trim() !== '' && baseUrl.trim() !== '' && !migrating;
  const canPreview =
    (mode === 'online' ? baseUrl.trim() !== '' : offlinePath.trim() !== '') && !previewing;

  return (
    <Stack>
      <Title order={2}>{t('title')}</Title>
      <Text c="dimmed">{t('intro')}</Text>

      <Stepper active={active} onStepClick={setActive}>
        <Stepper.Step label={t('step.sourceLabel')} description={t('step.sourceDesc')}>
          <Stack mt="md">
            <SegmentedControl
              value={mode}
              onChange={(v) => setMode(v as SourceMode)}
              data={[
                { label: t('mode.online'), value: 'online' },
                { label: t('mode.offline'), value: 'offline' },
              ]}
            />

            {mode === 'online' ? (
              <>
                <TextInput
                  label={t('source.baseUrlLabel')}
                  placeholder="https://nexus.example"
                  value={baseUrl}
                  onChange={(e) => setBaseUrl(e.currentTarget.value)}
                  required
                />
                <PasswordInput
                  label={t('source.authRefLabel')}
                  description={t('source.authRefDesc')}
                  placeholder={t('source.authRefPlaceholder')}
                  value={authRef}
                  onChange={(e) => setAuthRef(e.currentTarget.value)}
                />
              </>
            ) : (
              <TextInput
                label={t('source.offlinePathLabel')}
                description={t('source.offlinePathDesc')}
                placeholder="/data/nexus/blobs/default"
                value={offlinePath}
                onChange={(e) => setOfflinePath(e.currentTarget.value)}
                required
              />
            )}

            {previewError && <ErrorAlert message={previewError} />}

            <Group>
              <Button onClick={handlePreview} disabled={!canPreview} loading={previewing}>
                {t('preview.button')}
              </Button>
              {rows.length > 0 && (
                <Button variant="default" onClick={() => setActive(1)}>
                  {t('preview.next')}
                </Button>
              )}
            </Group>

            {previewing ? (
              <Center h={120}>
                <Loader />
              </Center>
            ) : (
              rows.length > 0 && (
                <Card withBorder padding="md" radius="md">
                  <Text fw={600} mb="sm">
                    {t('preview.count', { count: rows.length })}
                  </Text>
                  <Table.ScrollContainer minWidth={420}>
                    <Table striped highlightOnHover>
                      <Table.Thead>
                        <Table.Tr>
                          <Table.Th>{t('preview.thRepo')}</Table.Th>
                          <Table.Th>{t('preview.thFormat')}</Table.Th>
                          <Table.Th>{t('preview.thDetail')}</Table.Th>
                        </Table.Tr>
                      </Table.Thead>
                      <Table.Tbody>
                        {rows.map((row) => (
                          <Table.Tr key={row.name}>
                            <Table.Td>{row.name}</Table.Td>
                            <Table.Td>{row.format}</Table.Td>
                            <Table.Td>{row.detail}</Table.Td>
                          </Table.Tr>
                        ))}
                      </Table.Tbody>
                    </Table>
                  </Table.ScrollContainer>
                </Card>
              )
            )}
          </Stack>
        </Stepper.Step>

        <Stepper.Step label={t('step.selectLabel')} description={t('step.selectDesc')}>
          <Stack mt="md">
            {rows.length === 0 ? (
              <Text c="dimmed">{t('select.empty')}</Text>
            ) : (
              <>
                <div>
                  <Text fw={600} mb="xs">
                    {t('select.method')}
                  </Text>
                  <SegmentedControl
                    value={method}
                    onChange={(v) => setMethod(v as MigrateMethod)}
                    data={[
                      { label: t('method.online'), value: 'online' },
                      { label: t('method.offline'), value: 'offline' },
                    ]}
                  />
                </div>

                <Card withBorder padding="md" radius="md">
                  <Text fw={600} mb="sm">
                    {t('select.chosen', { count: selected.size })}
                  </Text>
                  {method === 'online' ? (
                    <Stack gap="sm">
                      {rows.map((row) => (
                        <Group key={row.name} align="center" wrap="nowrap">
                          <Checkbox
                            label={`${row.name}（${row.format} / ${row.detail}）`}
                            checked={selected.has(row.name)}
                            onChange={() => toggleSelect(row.name)}
                            style={{ flex: 1 }}
                          />
                          <TextInput
                            aria-label={t('source.targetRepoAria', { name: row.name })}
                            placeholder={t('source.targetRepoPlaceholder', { name: row.name })}
                            value={renames[row.name] ?? ''}
                            onChange={(e) => setRename(row.name, e.currentTarget.value)}
                            disabled={!selected.has(row.name)}
                            w={220}
                          />
                        </Group>
                      ))}
                    </Stack>
                  ) : (
                    <Stack gap="xs">
                      {rows.map((row) => (
                        <Checkbox
                          key={row.name}
                          label={`${row.name}（${row.format} / ${row.detail}）`}
                          checked={selected.has(row.name)}
                          onChange={() => toggleSelect(row.name)}
                        />
                      ))}
                    </Stack>
                  )}
                </Card>

                {method === 'online' ? (
                  <Alert color="blue" variant="light">
                    {t('select.onlineHint')}
                  </Alert>
                ) : (
                  <TextInput
                    label={t('source.migratePathLabel')}
                    description={t('source.migratePathDesc')}
                    placeholder="/data/nexus/blobs/default"
                    value={migratePath}
                    onChange={(e) => setMigratePath(e.currentTarget.value)}
                    required
                  />
                )}

                {migrateError && <ErrorAlert message={migrateError} />}

                {method === 'online' ? (
                  <>
                    <Group>
                      <Button onClick={() => setActive(0)} variant="default">
                        {t('select.prevStep')}
                      </Button>
                      <Button
                        onClick={handleMigrateOnline}
                        disabled={!canMigrateOnline}
                        loading={migrating}
                      >
                        {t('select.runOnline')}
                      </Button>
                    </Group>
                    <Text size="xs" c="dimmed">
                      {t('select.onlineFootnote')}
                    </Text>
                  </>
                ) : (
                  <>
                    <Group>
                      <Button onClick={() => setActive(0)} variant="default">
                        {t('select.prevStep')}
                      </Button>
                      <Button
                        onClick={() => handleMigrateOffline('proxy')}
                        disabled={!canMigrateOffline}
                        loading={migrating}
                      >
                        {t('select.runProxy')}
                      </Button>
                      <Button
                        onClick={() => handleMigrateOffline('hosted')}
                        disabled={!canMigrateOffline}
                        loading={migrating}
                        color="grape"
                      >
                        {t('select.runHosted')}
                      </Button>
                    </Group>
                    <Text size="xs" c="dimmed">
                      {t('select.offlineFootnote')}
                    </Text>
                  </>
                )}
              </>
            )}
          </Stack>
        </Stepper.Step>

        <Stepper.Step label={t('step.reportLabel')} description={t('step.reportDesc')}>
          <Stack mt="md">
            {onlineJob ? (
              <OnlineJobPanel
                job={onlineJob}
                polling={pollingJobId !== null}
                controlling={controlling}
                onCancel={() => runControl(api.cancelMigrationJob, onlineJob.job_id)}
                onPause={() => runControl(api.pauseMigrationJob, onlineJob.job_id)}
                onResume={() => runControl(api.resumeMigrationJob, onlineJob.job_id)}
              />
            ) : !report ? (
              <Text c="dimmed">{t('report.empty')}</Text>
            ) : (
              <Card withBorder padding="md" radius="md">
                <Text fw={600} mb="sm">
                  {t('report.title')}
                </Text>
                {report.repos.length === 0 ? (
                  <Text c="dimmed" size="sm">
                    {t('report.noRepos')}
                  </Text>
                ) : (
                  <Table.ScrollContainer minWidth={520}>
                    <Table striped>
                      <Table.Thead>
                        <Table.Tr>
                          <Table.Th>{t('report.thRepo')}</Table.Th>
                          <Table.Th>{t('report.thFormat')}</Table.Th>
                          <Table.Th>{t('report.thCreated')}</Table.Th>
                          <Table.Th ta="right">{t('report.thMigrated')}</Table.Th>
                          <Table.Th ta="right">{t('report.thSkipped')}</Table.Th>
                        </Table.Tr>
                      </Table.Thead>
                      <Table.Tbody>
                        {report.repos.map((r) => (
                          <Table.Tr key={r.repo_name}>
                            <Table.Td>{r.repo_name}</Table.Td>
                            <Table.Td>{r.format}</Table.Td>
                            <Table.Td>
                              <Badge color={r.created ? 'green' : 'gray'} variant="light">
                                {r.created ? t('report.createdYes') : t('report.createdExisting')}
                              </Badge>
                            </Table.Td>
                            <Table.Td ta="right">{r.migrated_artifacts}</Table.Td>
                            <Table.Td ta="right">{r.skipped_artifacts}</Table.Td>
                          </Table.Tr>
                        ))}
                      </Table.Tbody>
                    </Table>
                  </Table.ScrollContainer>
                )}

                {report.skipped_repos.length > 0 && (
                  <Group mt="sm" gap="xs">
                    <Text size="sm" c="dimmed">
                      {t('report.skippedRepos')}
                    </Text>
                    {report.skipped_repos.map((name) => (
                      <Badge key={name} color="orange" variant="light">
                        {name}
                      </Badge>
                    ))}
                  </Group>
                )}
              </Card>
            )}
            <Group>
              <Button variant="default" onClick={() => setActive(1)}>
                {t('report.backToSelect')}
              </Button>
            </Group>
          </Stack>
        </Stepper.Step>
      </Stepper>
    </Stack>
  );
}

/** 在线拉取任务进度面板：进行中展示导入队列进度，终态展示最终报告，并提供取消 / 暂停 / 继续。 */
function OnlineJobPanel({
  job,
  polling,
  controlling,
  onCancel,
  onPause,
  onResume,
}: {
  job: OnlinePullJob;
  polling: boolean;
  controlling: boolean;
  onCancel: () => void;
  onPause: () => void;
  onResume: () => void;
}) {
  const { t } = useTranslation('migration');
  // 进度百分比：无资产（尚在枚举）时为 0，避免除零。
  const percent = job.total_assets > 0 ? Math.round((job.done_assets / job.total_assets) * 100) : 0;
  const terminal = isTerminalPhase(job.phase);
  // 按钮可用性（FR-91）：仅进行中（未暂停的活动态）可暂停 / 取消；仅已暂停可继续；终态全禁用。
  const canPause = !terminal && !job.paused && !controlling;
  const canResume = !terminal && job.paused && !controlling;
  const canCancel = !terminal && !controlling;

  return (
    <Card withBorder padding="md" radius="md">
      <Group justify="space-between" mb="sm">
        <Text fw={600}>{t('job.queueTitle')}</Text>
        <Group gap="xs">
          {polling && <Loader size="xs" />}
          <Badge color={phaseColor(job.phase)} variant="light">
            {phaseLabel(job.phase, t)}
          </Badge>
        </Group>
      </Group>

      {/* 任务控制：取消 / 暂停 / 继续（按任务态启停） */}
      <Group gap="xs" mb="sm">
        {job.paused ? (
          <Button size="xs" variant="light" onClick={onResume} disabled={!canResume}>
            {t('job.resume')}
          </Button>
        ) : (
          <Button size="xs" variant="light" onClick={onPause} disabled={!canPause}>
            {t('job.pause')}
          </Button>
        )}
        <Button size="xs" variant="light" color="red" onClick={onCancel} disabled={!canCancel}>
          {t('job.cancel')}
        </Button>
      </Group>

      {/* 进度条 + 资产计数 */}
      <Progress value={percent} aria-label={t('job.progressAria')} animated={!terminal && !job.paused} />
      <Group justify="space-between" mt="xs">
        <Text size="sm" c="dimmed">
          {t('job.progress', { done: job.done_assets, total: job.total_assets, percent })}
        </Text>
        <Group gap="md">
          <Text size="sm">{t('job.migrated', { count: job.migrated })}</Text>
          <Text size="sm" c="dimmed">
            {t('job.skipped', { count: job.skipped })}
          </Text>
        </Group>
      </Group>

      {/* 当前仓库 / 当前文件（进行中显示） */}
      {!terminal && (job.current_repo || job.current_path) && (
        <Stack gap={2} mt="sm">
          {job.current_repo && (
            <Text size="sm">
              {t('job.currentRepo')}
              <Text span fw={500}>
                {job.current_repo}
              </Text>
            </Text>
          )}
          {job.current_path && (
            <Text size="xs" c="dimmed" style={{ wordBreak: 'break-all' }}>
              {t('job.currentPath')}
              {job.current_path}
            </Text>
          )}
        </Stack>
      )}

      {/* 失败：展示错误文案 */}
      {job.phase === 'failed' && job.error && (
        <Alert color="red" variant="light" mt="sm" title={t('job.failedTitle')}>
          {job.error}
        </Alert>
      )}

      {/* 终态：展示各仓库迁移明细 */}
      {terminal && (
        <>
          {job.repos.length === 0 ? (
            <Text c="dimmed" size="sm" mt="sm">
              {t('job.noRepos')}
            </Text>
          ) : (
            <Table.ScrollContainer minWidth={560}>
              <Table striped mt="sm">
                <Table.Thead>
                  <Table.Tr>
                    <Table.Th>{t('job.thSourceRepo')}</Table.Th>
                    <Table.Th>{t('job.thTargetRepo')}</Table.Th>
                    <Table.Th>{t('job.thFormat')}</Table.Th>
                    <Table.Th>{t('job.thCreated')}</Table.Th>
                    <Table.Th ta="right">{t('job.thMigrated')}</Table.Th>
                    <Table.Th ta="right">{t('job.thSkipped')}</Table.Th>
                  </Table.Tr>
                </Table.Thead>
                <Table.Tbody>
                  {job.repos.map((r) => (
                    <Table.Tr key={`${r.source_repo}->${r.target_repo}`}>
                      <Table.Td>{r.source_repo}</Table.Td>
                      <Table.Td>{r.target_repo}</Table.Td>
                      <Table.Td>{r.format}</Table.Td>
                      <Table.Td>
                        <Badge color={r.created ? 'green' : 'gray'} variant="light">
                          {r.created ? t('job.createdYes') : t('job.createdExisting')}
                        </Badge>
                      </Table.Td>
                      <Table.Td ta="right">{r.migrated_artifacts}</Table.Td>
                      <Table.Td ta="right">{r.skipped_artifacts}</Table.Td>
                    </Table.Tr>
                  ))}
                </Table.Tbody>
              </Table>
            </Table.ScrollContainer>
          )}

          {job.skipped_repos.length > 0 && (
            <Group mt="sm" gap="xs">
              <Text size="sm" c="dimmed">
                {t('job.skippedRepos')}
              </Text>
              {job.skipped_repos.map((name) => (
                <Badge key={name} color="orange" variant="light">
                  {name}
                </Badge>
              ))}
            </Group>
          )}
        </>
      )}
    </Card>
  );
}
