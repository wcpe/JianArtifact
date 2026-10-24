// 维护与清理页——仅管理员。
//
// 布局遵循备份页（MigrationsPage / BackupsTab）的固定视口范式，**不做向下平铺**：
//   - 页面 = `PageShell` + 一条 `Tabs`，Tab 切换走查询参数 `?tab=`（直链与刷新保持位置）；
//   - 每个 Tab 内是「关键事实（固定高度）+ 一个 `OpsSection` 表格（flex:1 独占剩余高度）」；
//   - 折叠/展开类交互（预览清单、确认、详情）一律**弹模态框**，不把页面越撑越长。
//
// 表格统一带 `miw={720}`：不给最小宽度时，窄屏下列会被压到一字一行（原路径被拆成竖排），
// 这不属于横向溢出所以不会被溢出检查发现，但可读性已经崩了。给最小宽度后，窄屏改为在
// 表格容器内横向滚动，宽屏仍撑满容器。新增列不再抬高这个下限：列多了就让内容自然变宽、
// 容器横向滚动，把 min-width 一路调大反而会让宽屏也白留一片。
//
// 列表加载失败**不清空数据**（清理任务与回收站两个 Tab 的共同约定）：清空过一次任务列表，
// 轮询驱动的那条链就断了——`hasRunning` 变 false → 定时器停 → 进行中的任务从界面消失，
// 一次网络抖动看起来像"任务丢了"。失败只置一条提示，下次成功自动恢复。
//
// 破坏性动作的防护分层是本页的核心设计：
//   - `cleanup`（移入回收站）与 `purge`（彻底删除）走**三层防护**：
//     预览清单（可逐项勾选）→ 二次确认 → 输入确认词「清理」；
//   - `restore`（恢复）只做**一次点击**：它是反向操作，误删后想取回还要过三道关
//     只会助长误操作；
//   - 「立即执行」触发既有作业是**物理删除、不进回收站**，界面上必须标注不可恢复。
import {
  Alert,
  Badge,
  Box,
  Button,
  Card,
  Checkbox,
  Code,
  Group,
  Modal,
  Progress,
  ScrollArea,
  SimpleGrid,
  Stack,
  Table,
  Tabs,
  Text,
} from "@mantine/core";
import { notifications } from "@mantine/notifications";
import { IconAlertTriangle, IconRefresh, IconTrash } from "@tabler/icons-react";
import { useCallback, useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { useSearchParams } from "react-router-dom";

import { PageShell } from "../app/PageShell";
import { AsyncBoundary } from "../components/AsyncBoundary";
import { OpsSection } from "../components/ops/OpsKit";
import { formatDuration } from "../components/audit/labels";
import { DestructiveConfirmModal } from "../components/maintenance/DestructiveConfirmModal";
import {
  executeCleanup,
  getStorageHealth,
  listCleanTasks,
  listMaintenanceJobs,
  listTrash,
  previewCleanup,
  purgeAllTrash,
  purgeTrash,
  restoreTrash,
  runMaintenanceJob,
} from "../api/endpoints";
import type {
  CleanTask,
  CleanupKind,
  CleanupPreview,
  MaintenanceJob,
  StorageHealth,
  TrashEntry,
} from "../api/endpoints";
import { useAsync } from "../hooks/useAsync";
import { parseUtc } from "../lib/timeFormat";
import { density } from "../theme/density";

const CLEANUP_KINDS: { kind: CleanupKind; labelKey: string }[] = [
  { kind: "empty_dirs", labelKey: "maintenance.kindEmptyDirs" },
  { kind: "stale_uploads", labelKey: "maintenance.kindStaleUploads" },
  { kind: "pre_restore", labelKey: "maintenance.kindPreRestore" },
  { kind: "quarantine", labelKey: "maintenance.kindQuarantine" },
];

function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes <= 0) return "0 B";
  if (bytes < 1024) return `${bytes} B`;
  const units = ["KB", "MB", "GB", "TB"];
  let value = bytes / 1024;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit++;
  }
  return `${value.toFixed(1)} ${units[unit]}`;
}

function formatStamp(stamp: string | null | undefined): string {
  if (!stamp) return "—";
  const parsed = new Date(stamp.includes("T") ? stamp : `${stamp.replace(" ", "T")}Z`);
  if (Number.isNaN(parsed.getTime())) return stamp;
  return parsed.toLocaleString();
}

/**
 * 任务耗时：`StartedAt → FinishedAt`。
 *
 * 两端任一为空（排队中 / 仍在跑）就是算不出来，如实显示「—」，不用当前时间硬减——
 * 那会把"还在跑"渲染成一个不断变大的假耗时。时间解析复用 `lib/timeFormat` 的
 * `parseUtc`（后端既有 naive UTC 与 RFC3339 两种形状都由它统一），毫秒转文案复用
 * 审计工作台既有的 `formatDuration`，避免同一产品里出现第三套耗时写法。
 */
function formatElapsed(startedAt: string | null, finishedAt: string | null): string {
  const start = parseUtc(startedAt);
  const finish = parseUtc(finishedAt);
  if (!start || !finish) return "—";
  const ms = finish.getTime() - start.getTime();
  if (!Number.isFinite(ms) || ms < 0) return "—";
  return formatDuration(ms);
}

/** 关键事实条：一行固定高度指标，不参与滚动。 */
function FactsStrip({
  health,
  onCleanup,
}: {
  health: StorageHealth | null;
  onCleanup: (k: CleanupKind) => void;
}) {
  const { t } = useTranslation();
  const items: { label: string; value: string; kind?: CleanupKind }[] = [
    { label: t("maintenance.orphanBlobs"), value: `${health?.orphanBlobs ?? 0}` },
    { label: t("maintenance.quarantined"), value: `${health?.quarantinedBlobs ?? 0}` },
    { label: t("maintenance.emptyDirs"), value: `${health?.emptyDirs ?? 0}`, kind: "empty_dirs" },
    {
      label: t("maintenance.preRestore"),
      value: `${health?.preRestoreDirs ?? 0}`,
      kind: "pre_restore",
    },
    {
      label: t("maintenance.staleUploads"),
      value: formatBytes(health?.backupUploadsBytes ?? 0),
      kind: "stale_uploads",
    },
    { label: t("maintenance.total"), value: formatBytes(health?.totalBytes ?? 0) },
  ];
  return (
    <Card withBorder radius="md" padding={density.cardPadding}>
      <Group justify="space-between" align="flex-end" wrap="wrap" gap="md">
        <SimpleGrid cols={{ base: 2, sm: 3, lg: 6 }} spacing="lg" style={{ flex: 1, minWidth: 0 }}>
          {items.map((item) => (
            <Box key={item.label}>
              <Text size="xs" c="dimmed">
                {item.label}
              </Text>
              <Text size="lg" fw={600}>
                {item.value}
              </Text>
            </Box>
          ))}
        </SimpleGrid>
        <Group gap="xs" wrap="wrap">
          {CLEANUP_KINDS.map(({ kind, labelKey }) => (
            <Button
              key={kind}
              size="xs"
              variant="light"
              leftSection={<IconTrash size={14} />}
              onClick={() => onCleanup(kind)}
              data-cleanup-kind={kind}
            >
              {t(labelKey)}
            </Button>
          ))}
        </Group>
      </Group>
    </Card>
  );
}

/** 清理任务 Tab：历史表独占剩余高度。 */
function CleanTasksTab({ refreshKey }: { refreshKey: number }) {
  const { t } = useTranslation();
  const [tasks, setTasks] = useState<CleanTask[]>([]);
  const [total, setTotal] = useState(0);
  const [loadFailed, setLoadFailed] = useState(false);

  const load = useCallback(() => {
    listCleanTasks(1, 20)
      .then((res) => {
        setTasks(res.items ?? []);
        setTotal(res.total ?? 0);
        setLoadFailed(false);
      })
      // 失败时**绝不**清空列表：本 Tab 由轮询驱动，清空会让 `hasRunning` 立刻变 false、
      // 定时器随之停掉，进行中的任务就此从界面消失——一次网络抖动看起来就像"任务丢了"。
      // 保留上次数据并置一条失败提示，下一轮成功即自动恢复。
      .catch(() => setLoadFailed(true));
  }, []);

  useEffect(load, [load, refreshKey]);

  // 仅在有进行中任务时轮询，终态立即停止。
  const hasRunning = tasks.some((task) => task.Status === "running" || task.Status === "queued");
  useEffect(() => {
    if (!hasRunning) return;
    const timer = window.setInterval(load, 1500);
    return () => window.clearInterval(timer);
  }, [hasRunning, load]);

  return (
    <OpsSection
      title={t("maintenance.tasksTitle")}
      meta={t("maintenance.tasksTotal", { count: total })}
      testId="clean-tasks-section"
      style={{
        flex: 1,
        minHeight: 150,
        display: "flex",
        flexDirection: "column",
        overflow: "hidden",
      }}
      bodyStyle={{ flex: 1, minHeight: 0, overflow: "auto" }}
      bodyPadding={0}
      actions={
        <Button size="xs" variant="subtle" leftSection={<IconRefresh size={14} />} onClick={load}>
          {t("maintenance.refresh")}
        </Button>
      }
    >
      {loadFailed ? (
        <Box px="md" pt="sm">
          <Alert
            color="yellow"
            variant="light"
            icon={<IconAlertTriangle size={16} />}
            title={t("common.refreshFailedTitle")}
          >
            {t("maintenance.loadFailedHint")}
          </Alert>
        </Box>
      ) : null}
      {tasks.length === 0 ? (
        <Box p="xl">
          <Text size="sm" c="dimmed">
            {t("maintenance.tasksEmpty")}
          </Text>
        </Box>
      ) : (
        <Table striped verticalSpacing={6} stickyHeader miw={720}>
          <Table.Thead>
            <Table.Tr>
              <Table.Th>{t("maintenance.colKind")}</Table.Th>
              <Table.Th>{t("maintenance.colStatus")}</Table.Th>
              <Table.Th>{t("maintenance.colProgress")}</Table.Th>
              <Table.Th>{t("maintenance.colFreedBytes")}</Table.Th>
              <Table.Th>{t("maintenance.colElapsed")}</Table.Th>
              <Table.Th>{t("maintenance.colCreated")}</Table.Th>
              <Table.Th>{t("maintenance.colFailure")}</Table.Th>
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {tasks.map((task) => (
              <Table.Tr key={task.ID} data-task-kind={task.Kind} data-task-status={task.Status}>
                <Table.Td>{task.Kind}</Table.Td>
                <Table.Td>
                  <Badge
                    size="sm"
                    variant="light"
                    color={
                      task.Status === "succeeded"
                        ? "green"
                        : task.Status === "failed"
                          ? "red"
                          : task.Status === "interrupted"
                            ? "orange"
                            : "blue"
                    }
                  >
                    {task.Status}
                  </Badge>
                </Table.Td>
                <Table.Td>
                  {task.Status === "running" && task.TotalItems > 0 ? (
                    <Progress value={(task.ProcessedItems / task.TotalItems) * 100} size="sm" />
                  ) : (
                    <Text size="sm" c="dimmed">
                      {task.ProcessedItems}/{task.TotalItems}
                    </Text>
                  )}
                </Table.Td>
                <Table.Td>{formatBytes(task.FreedBytes)}</Table.Td>
                <Table.Td>{formatElapsed(task.StartedAt, task.FinishedAt)}</Table.Td>
                <Table.Td>
                  <Text size="sm">{formatStamp(task.CreatedAt)}</Text>
                </Table.Td>
                <Table.Td>
                  {task.ErrorMessage ? (
                    <Text size="sm" c="red" style={{ wordBreak: "break-word" }}>
                      {task.ErrorMessage}
                    </Text>
                  ) : (
                    <Text size="sm" c="dimmed">
                      —
                    </Text>
                  )}
                </Table.Td>
              </Table.Tr>
            ))}
          </Table.Tbody>
        </Table>
      )}
    </OpsSection>
  );
}

/** 回收站 Tab：表格独占剩余高度，恢复/彻底删除走行内按钮 + 模态框确认。 */
function TrashTab({ refreshKey, onChanged }: { refreshKey: number; onChanged: () => void }) {
  const { t } = useTranslation();
  const [entries, setEntries] = useState<TrashEntry[]>([]);
  const [stats, setStats] = useState({ entries: 0, bytes: 0 });
  const [selected, setSelected] = useState<Set<number>>(new Set());
  const [pendingPurge, setPendingPurge] = useState<{ ids: number[]; all: boolean } | null>(null);
  const [loadFailed, setLoadFailed] = useState(false);
  // 彻底删除请求在途：用于禁用确认按钮，避免连点发出多次不可逆请求。
  const [purging, setPurging] = useState(false);

  const load = useCallback(() => {
    listTrash(1, 50)
      .then((res) => {
        setEntries(res.items ?? []);
        setStats({ entries: res.stats?.entries ?? 0, bytes: res.stats?.bytes ?? 0 });
        setSelected(new Set());
        setLoadFailed(false);
      })
      // 与清理任务 Tab 同一口径：失败保留上次数据，只提示不抹掉。
      // 列表被清空会连带让"清空回收站"按钮变成禁用——一次网络抖动看起来像回收站真的空了。
      .catch(() => setLoadFailed(true));
  }, []);

  useEffect(load, [load, refreshKey]);

  const toggle = (id: number) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  };

  // 恢复只做一次点击：反向操作加三道关只会让人更不敢用。
  const handleRestore = (ids: number[]) => {
    restoreTrash(ids)
      .then((res) => {
        if (res.conflicts && res.conflicts.length > 0) {
          notifications.show({
            color: "yellow",
            title: t("maintenance.restoreConflictsTitle"),
            message: res.conflicts.map((c) => `${c.originalPath}：${c.reason}`).join("\n"),
          });
        }
        if (res.restored > 0) {
          notifications.show({
            color: "green",
            message: t("maintenance.restored", { count: res.restored }),
          });
        }
        load();
        onChanged();
      })
      .catch(() => notifications.show({ color: "red", message: t("maintenance.actionFailed") }));
  };

  const runPendingPurge = () => {
    if (!pendingPurge || purging) return;
    setPurging(true);
    const action = pendingPurge.all ? purgeAllTrash() : purgeTrash(pendingPurge.ids);
    action
      .then(() => {
        notifications.show({ color: "green", message: t("maintenance.purged") });
        setPendingPurge(null);
        load();
        onChanged();
      })
      .catch(() => notifications.show({ color: "red", message: t("maintenance.actionFailed") }))
      .finally(() => setPurging(false));
  };

  return (
    <>
      <OpsSection
        title={t("maintenance.trashTitle")}
        meta={t("maintenance.trashUsage", {
          count: stats.entries,
          bytes: formatBytes(stats.bytes),
        })}
        testId="trash-section"
        style={{
          flex: 1,
          minHeight: 150,
          display: "flex",
          flexDirection: "column",
          overflow: "hidden",
        }}
        bodyStyle={{ flex: 1, minHeight: 0, overflow: "auto" }}
        bodyPadding={0}
        actions={
          <Group gap="xs">
            <Button
              size="xs"
              variant="default"
              disabled={selected.size === 0}
              onClick={() => handleRestore([...selected])}
            >
              {t("maintenance.restoreSelected")}
            </Button>
            <Button
              size="xs"
              color="red"
              variant="light"
              disabled={selected.size === 0}
              onClick={() => setPendingPurge({ ids: [...selected], all: false })}
            >
              {t("maintenance.purgeSelected")}
            </Button>
            <Button
              size="xs"
              color="red"
              variant="subtle"
              disabled={stats.entries === 0}
              onClick={() => setPendingPurge({ ids: [], all: true })}
            >
              {t("maintenance.purgeAll")}
            </Button>
          </Group>
        }
      >
        {loadFailed ? (
          <Box px="md" pt="sm">
            <Alert
              color="yellow"
              variant="light"
              icon={<IconAlertTriangle size={16} />}
              title={t("common.refreshFailedTitle")}
            >
              {t("maintenance.loadFailedHint")}
            </Alert>
          </Box>
        ) : null}
        {entries.length === 0 ? (
          <Box p="xl">
            <Stack gap={4}>
              <Text size="sm" c="dimmed">
                {t("maintenance.trashEmpty")}
              </Text>
              <Text size="xs" c="dimmed">
                {t("maintenance.trashHint")}
              </Text>
            </Stack>
          </Box>
        ) : (
          <Table striped verticalSpacing={6} stickyHeader miw={720}>
            <Table.Thead>
              <Table.Tr>
                <Table.Th style={{ width: 36 }} />
                <Table.Th>{t("maintenance.colKind")}</Table.Th>
                <Table.Th>{t("maintenance.colPath")}</Table.Th>
                <Table.Th>{t("maintenance.colSize")}</Table.Th>
                <Table.Th>{t("maintenance.colCreated")}</Table.Th>
                <Table.Th>{t("maintenance.colRetention")}</Table.Th>
                <Table.Th>{t("maintenance.colActions")}</Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {entries.map((entry) => (
                <Table.Tr key={entry.ID} data-trash-id={entry.ID}>
                  <Table.Td>
                    <Checkbox
                      aria-label={entry.OriginalPath}
                      checked={selected.has(entry.ID)}
                      onChange={() => toggle(entry.ID)}
                    />
                  </Table.Td>
                  <Table.Td>{entry.Kind}</Table.Td>
                  <Table.Td>
                    <Code style={{ wordBreak: "break-all" }}>{entry.OriginalPath}</Code>
                  </Table.Td>
                  <Table.Td>{formatBytes(entry.Bytes)}</Table.Td>
                  <Table.Td>{formatStamp(entry.CreatedAt)}</Table.Td>
                  <Table.Td>
                    {/* 后端未返回保留期，前端不猜：保留天数只存在于服务端配置
                        `JIAN_TRASH_RETENTION_DAYS`，而 GET /maintenance/trash 既不回保留天数、
                        也不回过期时间；照默认值硬编码 7 天，在运维把它改成别的天数或改成 0
                        （不自动清理）时就会直接说错话。接口补出该字段前一律显示「—」。 */}
                    <Text size="sm" c="dimmed">
                      —
                    </Text>
                  </Table.Td>
                  <Table.Td>
                    <Group gap={4}>
                      <Button
                        size="compact-xs"
                        variant="default"
                        onClick={() => handleRestore([entry.ID])}
                      >
                        {t("maintenance.restore")}
                      </Button>
                      <Button
                        size="compact-xs"
                        color="red"
                        variant="subtle"
                        onClick={() => setPendingPurge({ ids: [entry.ID], all: false })}
                      >
                        {t("maintenance.purge")}
                      </Button>
                    </Group>
                  </Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        )}
      </OpsSection>
      <DestructiveConfirmModal
        opened={pendingPurge !== null}
        title={pendingPurge?.all ? t("maintenance.purgeAllTitle") : t("maintenance.purgeTitle")}
        message={
          pendingPurge?.all
            ? t("maintenance.purgeAllMessage", { count: stats.entries })
            : t("maintenance.purgeMessage", { count: pendingPurge?.ids.length ?? 0 })
        }
        onConfirm={runPendingPurge}
        onClose={() => setPendingPurge(null)}
        pending={purging}
      />
    </>
  );
}

/** 维护作业 Tab：清单表独占剩余高度，执行走确认弹窗（物理删除，不可恢复）。 */
function JobsTab() {
  const { t } = useTranslation();
  const state = useAsync(listMaintenanceJobs, [], { cacheKey: "maintenance-jobs" });
  const [pendingJob, setPendingJob] = useState<MaintenanceJob | null>(null);
  // 触发请求在途：禁用确认按钮，避免连点把同一作业触发多轮（作业是物理删除）。
  const [starting, setStarting] = useState(false);

  const runPendingJob = () => {
    if (!pendingJob || starting) return;
    setStarting(true);
    runMaintenanceJob(pendingJob.name)
      .then(() => {
        notifications.show({ color: "green", message: t("maintenance.jobAccepted") });
        setPendingJob(null);
      })
      .catch(() => notifications.show({ color: "red", message: t("maintenance.actionFailed") }))
      .finally(() => setStarting(false));
  };

  return (
    <>
      <OpsSection
        title={t("maintenance.jobsTitle")}
        meta={t("maintenance.jobsHint")}
        testId="jobs-section"
        style={{
          flex: 1,
          minHeight: 150,
          display: "flex",
          flexDirection: "column",
          overflow: "hidden",
        }}
        bodyStyle={{ flex: 1, minHeight: 0, overflow: "auto" }}
        bodyPadding={0}
      >
        <AsyncBoundary state={state}>
          {(res: { jobs?: MaintenanceJob[] }) => {
            // `jobs` 而非 `items`（后端既有形状）；用 `?? []` 兜底，
            // 避免字段改名时整页白屏——作业区块渲染不出来不该拖垮其余区块。
            const jobs = res?.jobs ?? [];
            if (jobs.length === 0) {
              return (
                <Box p="xl">
                  <Text size="sm" c="dimmed">
                    {t("maintenance.jobsEmpty")}
                  </Text>
                </Box>
              );
            }
            return (
              <Table striped verticalSpacing={6} stickyHeader miw={720}>
                <Table.Thead>
                  <Table.Tr>
                    <Table.Th>{t("maintenance.colJob")}</Table.Th>
                    <Table.Th>{t("maintenance.colInterval")}</Table.Th>
                    <Table.Th>{t("maintenance.colLastRun")}</Table.Th>
                    <Table.Th>{t("maintenance.colFailures")}</Table.Th>
                    <Table.Th>{t("maintenance.colRunning")}</Table.Th>
                    <Table.Th>{t("maintenance.colActions")}</Table.Th>
                  </Table.Tr>
                </Table.Thead>
                <Table.Tbody>
                  {jobs.map((job) => (
                    <Table.Tr key={job.name} data-job-name={job.name}>
                      <Table.Td>{job.name}</Table.Td>
                      <Table.Td>{job.intervalSeconds}s</Table.Td>
                      <Table.Td>{formatStamp(job.lastFinishedAt ?? job.lastStartedAt)}</Table.Td>
                      <Table.Td>{job.failures ?? 0}</Table.Td>
                      <Table.Td>
                        {/* 运行中态直接取自接口的 `running`（进程内实际状态），
                            不靠"最近执行时间是否很近"去猜——猜错会把空闲作业画成在跑。 */}
                        <Badge
                          size="sm"
                          variant="light"
                          color={job.running ? "blue" : "gray"}
                          data-job-running={job.running ? "true" : "false"}
                        >
                          {job.running ? t("maintenance.jobRunningNow") : t("maintenance.jobIdle")}
                        </Badge>
                      </Table.Td>
                      <Table.Td>
                        <Button
                          size="compact-xs"
                          variant="light"
                          color="red"
                          leftSection={<IconTrash size={12} />}
                          onClick={() => setPendingJob(job)}
                        >
                          {t("maintenance.runJob")}
                        </Button>
                      </Table.Td>
                    </Table.Tr>
                  ))}
                </Table.Tbody>
              </Table>
            );
          }}
        </AsyncBoundary>
      </OpsSection>
      <DestructiveConfirmModal
        opened={pendingJob !== null}
        title={t("maintenance.runJobTitle", { name: pendingJob?.name ?? "" })}
        message={t("maintenance.runJobIrreversible")}
        onConfirm={runPendingJob}
        onClose={() => setPendingJob(null)}
        pending={starting}
      />
    </>
  );
}

/** 预览清单模态框：第一层防护（可逐项勾选），确认后进入第二、三层。 */
function CleanupPreviewModal({
  preview,
  onCancel,
  onConfirm,
}: {
  preview: CleanupPreview | null;
  onCancel: () => void;
  onConfirm: (items: string[]) => void;
}) {
  const { t } = useTranslation();
  const [selected, setSelected] = useState<Set<string>>(new Set());

  // 每次打开默认全选：用户点「预览清理」的意图通常就是"清理这一类"，
  // 全选后让他们按需取消，比从零勾选快。
  useEffect(() => {
    if (preview) setSelected(new Set(preview.items.map((item) => item.path)));
  }, [preview]);

  if (!preview) return null;
  const chosen = preview.items.filter((item) => selected.has(item.path));
  const bytes = chosen.reduce((sum, item) => sum + item.bytes, 0);

  return (
    <Modal
      opened
      onClose={onCancel}
      centered
      size="lg"
      title={t("maintenance.previewTitle", { kind: preview.kind })}
    >
      <CleanupPreviewBody
        preview={preview}
        selected={selected}
        onToggle={(path) =>
          setSelected((prev) => {
            const next = new Set(prev);
            if (next.has(path)) next.delete(path);
            else next.add(path);
            return next;
          })
        }
        chosenCount={chosen.length}
        bytes={bytes}
        onCancel={onCancel}
        onConfirm={() => onConfirm(chosen.map((item) => item.path))}
      />
    </Modal>
  );
}

/** 预览弹窗内容（不含 Modal 外壳，便于在弹窗内直接渲染）。 */
function CleanupPreviewBody({
  preview,
  selected,
  onToggle,
  chosenCount,
  bytes,
  onCancel,
  onConfirm,
}: {
  preview: CleanupPreview;
  selected: Set<string>;
  onToggle: (path: string) => void;
  chosenCount: number;
  bytes: number;
  onCancel: () => void;
  onConfirm: () => void;
}) {
  const { t } = useTranslation();
  return (
    <Stack gap="sm">
      <Text size="xs" c="dimmed">
        {t("maintenance.previewHint")}
      </Text>
      {preview.skipped && preview.skipped.length > 0 ? (
        <Alert color="yellow" variant="light" icon={<IconAlertTriangle size={16} />}>
          {t("maintenance.previewSkipped", { count: preview.skipped.length })}
        </Alert>
      ) : null}
      <ScrollArea.Autosize mah={320}>
        <Stack gap={4}>
          {preview.items.map((item) => (
            <Checkbox
              key={item.path}
              label={
                <Group gap="xs">
                  <Code style={{ wordBreak: "break-all" }}>{item.path}</Code>
                  <Text size="xs" c="dimmed">
                    {formatBytes(item.bytes)}
                  </Text>
                </Group>
              }
              checked={selected.has(item.path)}
              onChange={() => onToggle(item.path)}
            />
          ))}
        </Stack>
      </ScrollArea.Autosize>
      <Group justify="space-between" align="center">
        <Text size="xs" c="dimmed">
          {t("maintenance.previewSummary", { count: chosenCount, bytes: formatBytes(bytes) })}
        </Text>
        <Group gap="xs">
          <Button variant="default" onClick={onCancel}>
            {t("common.cancel")}
          </Button>
          <Button color="red" disabled={chosenCount === 0} onClick={onConfirm}>
            {t("maintenance.cleanupConfirm")}
          </Button>
        </Group>
      </Group>
    </Stack>
  );
}

export function MaintenancePage() {
  const { t } = useTranslation();
  const [params, setParams] = useSearchParams();
  // Tab 走查询参数（与迁移与备份页 ?tab= 同约定），直链与刷新后保持位置。
  const raw = params.get("tab");
  const tab = raw === "tasks" || raw === "trash" || raw === "jobs" ? raw : "health";

  const changeTab = (value: string | null) => {
    const next = new URLSearchParams(params);
    if (value && value !== "health") next.set("tab", value);
    else next.delete("tab");
    setParams(next, { replace: true });
  };

  const healthState = useAsync(getStorageHealth, [], { cacheKey: "storage-health" });
  const [preview, setPreview] = useState<CleanupPreview | null>(null);
  const [confirming, setConfirming] = useState<{
    kind: CleanupKind;
    items: string[];
    bytes: number;
  } | null>(null);
  const [refreshKey, setRefreshKey] = useState(0);
  // 清理请求在途：用于禁用确认按钮。这条请求会移走上百个目录，
  // 手抖连点会让第二、三次点击撞 409，界面看起来像"清理失败"。
  const [cleaning, setCleaning] = useState(false);

  const bump = () => setRefreshKey((n) => n + 1);

  // 第一层防护：预览清单（弹窗内可逐项勾选）。预览本身**只读**。
  const openPreview = (kind: CleanupKind) => {
    previewCleanup(kind)
      .then((res) => setPreview(res))
      .catch(() => notifications.show({ color: "red", message: t("maintenance.actionFailed") }));
  };

  const runCleanup = () => {
    if (!confirming || cleaning) return;
    setCleaning(true);
    executeCleanup({ kind: confirming.kind, items: confirming.items })
      .then(() => {
        notifications.show({ color: "green", message: t("maintenance.cleanupDone") });
        setConfirming(null);
        healthState.reload();
        bump();
      })
      .catch((err: unknown) => {
        // 单并发冲突是**正常业务结果**（另一轮正在跑），如实提示而不是报"失败"。
        const text = err instanceof Error ? err.message : String(err);
        notifications.show({
          color: "red",
          message: text.includes("cleanup_running")
            ? t("maintenance.cleanupRunning")
            : t("maintenance.actionFailed"),
        });
      })
      .finally(() => setCleaning(false));
  };

  return (
    <PageShell testId="maintenance-page" gap={12}>
      <Tabs
        value={tab}
        onChange={changeTab}
        keepMounted={false}
        style={{ flex: 1, minHeight: 0, display: "flex", flexDirection: "column" }}
      >
        <Tabs.List style={{ flexShrink: 0 }}>
          <Tabs.Tab value="health">{t("maintenance.tabHealth")}</Tabs.Tab>
          <Tabs.Tab value="trash">{t("maintenance.tabTrash")}</Tabs.Tab>
          <Tabs.Tab value="tasks">{t("maintenance.tabTasks")}</Tabs.Tab>
          <Tabs.Tab value="jobs">{t("maintenance.tabJobs")}</Tabs.Tab>
        </Tabs.List>

        <Tabs.Panel
          value="health"
          pt="md"
          style={{ flex: 1, minHeight: 0, display: "flex", flexDirection: "column", gap: 12 }}
        >
          <Box style={{ flexShrink: 0 }}>
            <AsyncBoundary state={healthState}>
              {(health: StorageHealth) => (
                <Stack gap="xs">
                  {health.partialError ? (
                    <Alert color="yellow" variant="light" icon={<IconAlertTriangle size={16} />}>
                      {t("maintenance.partialError", { reason: health.partialError })}
                    </Alert>
                  ) : null}
                  <FactsStrip health={health} onCleanup={openPreview} />
                </Stack>
              )}
            </AsyncBoundary>
          </Box>
          <OpsSection
            title={t("maintenance.healthDetailTitle")}
            meta={t("maintenance.healthHint")}
            testId="health-section"
            style={{
              flex: 1,
              minHeight: 150,
              display: "flex",
              flexDirection: "column",
              overflow: "hidden",
            }}
            bodyStyle={{ flex: 1, minHeight: 0, overflow: "auto" }}
            bodyPadding="md"
            actions={
              <Button
                size="xs"
                variant="subtle"
                leftSection={<IconRefresh size={14} />}
                onClick={healthState.reload}
              >
                {t("maintenance.refresh")}
              </Button>
            }
          >
            <Text size="sm" c="dimmed">
              {t("maintenance.healthBodyHint")}
            </Text>
          </OpsSection>
        </Tabs.Panel>

        <Tabs.Panel
          value="trash"
          pt="md"
          style={{ flex: 1, minHeight: 0, display: "flex", flexDirection: "column" }}
        >
          <TrashTab refreshKey={refreshKey} onChanged={bump} />
        </Tabs.Panel>

        <Tabs.Panel
          value="tasks"
          pt="md"
          style={{ flex: 1, minHeight: 0, display: "flex", flexDirection: "column" }}
        >
          <CleanTasksTab refreshKey={refreshKey} />
        </Tabs.Panel>

        <Tabs.Panel
          value="jobs"
          pt="md"
          style={{ flex: 1, minHeight: 0, display: "flex", flexDirection: "column" }}
        >
          <JobsTab />
        </Tabs.Panel>
      </Tabs>

      <CleanupPreviewModal
        preview={preview}
        onCancel={() => setPreview(null)}
        onConfirm={(items) => {
          if (!preview) return;
          const bytes = preview.items
            .filter((item) => items.includes(item.path))
            .reduce((sum, item) => sum + item.bytes, 0);
          setConfirming({ kind: preview.kind, items, bytes });
          setPreview(null);
        }}
      />

      <DestructiveConfirmModal
        opened={confirming !== null}
        title={t("maintenance.cleanupTitle")}
        message={t("maintenance.cleanupMessage", {
          count: confirming?.items.length ?? 0,
          bytes: formatBytes(confirming?.bytes ?? 0),
        })}
        onConfirm={runCleanup}
        onClose={() => setConfirming(null)}
        pending={cleaning}
      />
    </PageShell>
  );
}
