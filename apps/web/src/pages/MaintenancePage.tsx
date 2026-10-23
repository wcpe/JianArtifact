// 维护与清理页（FR-158 / FR-159 / FR-161）——仅管理员。
//
// 四个区块：存储体检 / 清理任务 / 回收站 / 维护作业。
//
// 破坏性动作的防护分层是本页的核心设计：
//   - `cleanup`（移入回收站）与 `purge`（彻底删除）走**三层防护**：
//     预览清单（可逐项勾选）→ 二次确认 → 输入确认词「清理」；
//   - `restore`（恢复）只做**一次点击**：它是反向操作，误删后想取回还要过三道关
//     只会助长误操作；
//   - 「立即执行」触发既有作业是**物理删除、不进回收站**，界面上必须明确标注不可恢复。
import {
  Alert,
  Badge,
  Box,
  Button,
  Card,
  Checkbox,
  Code,
  Group,
  Progress,
  ScrollArea,
  Stack,
  Table,
  Text,
  Title,
} from "@mantine/core";
import { notifications } from "@mantine/notifications";
import { IconAlertTriangle, IconRefresh, IconTrash } from "@tabler/icons-react";
import { useCallback, useEffect, useState } from "react";
import { useTranslation } from "react-i18next";

import { PageShell } from "../app/PageShell";
import { AsyncBoundary } from "../components/AsyncBoundary";
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
import { density } from "../theme/density";

/** 四类可清理项。 */
const CLEANUP_KINDS: { kind: CleanupKind; labelKey: string }[] = [
  { kind: "empty_dirs", labelKey: "maintenance.kindEmptyDirs" },
  { kind: "stale_uploads", labelKey: "maintenance.kindStaleUploads" },
  { kind: "pre_restore", labelKey: "maintenance.kindPreRestore" },
  { kind: "quarantine", labelKey: "maintenance.kindQuarantine" },
];

function formatBytes(bytes: number): string {
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

function Stat({ label, value, sub }: { label: string; value: string; sub?: string }) {
  return (
    <Box>
      <Text size="xs" c="dimmed">
        {label}
      </Text>
      <Text size="lg" fw={600}>
        {value}
      </Text>
      {sub ? (
        <Text size="xs" c="dimmed">
          {sub}
        </Text>
      ) : null}
    </Box>
  );
}

/** 存储体检卡：只读展示 + 每类一个「预览清理」。 */
function StorageHealthCard({ onCleanup }: { onCleanup: (kind: CleanupKind) => void }) {
  const { t } = useTranslation();
  const state = useAsync(getStorageHealth, [], { cacheKey: "storage-health" });

  return (
    <Card withBorder radius="md" padding={density.cardPadding}>
      <Stack gap="sm">
        <Group justify="space-between" wrap="wrap">
          <Title order={5}>{t("maintenance.healthTitle")}</Title>
          <Text size="xs" c="dimmed">
            {t("maintenance.healthHint")}
          </Text>
        </Group>
        <AsyncBoundary state={state}>
          {(health: StorageHealth) => (
            <Stack gap="sm">
              {health.partialError ? (
                <Alert color="yellow" variant="light" icon={<IconAlertTriangle size={16} />}>
                  {t("maintenance.partialError", { reason: health.partialError })}
                </Alert>
              ) : null}
              <Group gap="lg" wrap="wrap">
                <Stat
                  label={t("maintenance.orphanBlobs")}
                  value={`${health.orphanBlobs}`}
                  sub={formatBytes(health.orphanBlobBytes)}
                />
                <Stat
                  label={t("maintenance.quarantined")}
                  value={`${health.quarantinedBlobs}`}
                  sub={formatBytes(health.quarantinedBytes)}
                />
                <Stat label={t("maintenance.emptyDirs")} value={`${health.emptyDirs}`} />
                <Stat
                  label={t("maintenance.preRestore")}
                  value={`${health.preRestoreDirs}`}
                  sub={formatBytes(health.preRestoreBytes)}
                />
                <Stat
                  label={t("maintenance.staleUploads")}
                  value={formatBytes(health.backupUploadsBytes)}
                />
                <Stat label={t("maintenance.total")} value={formatBytes(health.totalBytes)} />
              </Group>
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
                    {t(labelKey)} · {t("maintenance.previewCleanup")}
                  </Button>
                ))}
              </Group>
            </Stack>
          )}
        </AsyncBoundary>
      </Stack>
    </Card>
  );
}

/** 清理任务面板：进行中进度 + 历史。 */
function CleanTasksCard({ refreshKey }: { refreshKey: number }) {
  const { t } = useTranslation();
  const [tasks, setTasks] = useState<CleanTask[]>([]);
  const [total, setTotal] = useState(0);

  const load = useCallback(() => {
    listCleanTasks(1, 20)
      .then((res) => {
        setTasks(res.items);
        setTotal(res.total);
      })
      .catch(() => setTasks([]));
  }, []);

  useEffect(load, [load, refreshKey]);

  // 仅在有进行中任务时轮询，终态立即停止（避免把运维页变成持续负载）。
  const hasRunning = tasks.some((task) => task.Status === "running" || task.Status === "queued");
  useEffect(() => {
    if (!hasRunning) return;
    const timer = window.setInterval(load, 1500);
    return () => window.clearInterval(timer);
  }, [hasRunning, load]);

  return (
    <Card withBorder radius="md" padding={density.cardPadding}>
      <Stack gap="sm">
        <Group justify="space-between" wrap="wrap">
          <Title order={5}>{t("maintenance.tasksTitle")}</Title>
          <Group gap="xs">
            <Text size="xs" c="dimmed">
              {t("maintenance.tasksTotal", { count: total })}
            </Text>
            <Button
              size="xs"
              variant="subtle"
              leftSection={<IconRefresh size={14} />}
              onClick={load}
            >
              {t("maintenance.refresh")}
            </Button>
          </Group>
        </Group>
        {tasks.length === 0 ? (
          <Text size="sm" c="dimmed">
            {t("maintenance.tasksEmpty")}
          </Text>
        ) : (
          <Table striped verticalSpacing={6}>
            <Table.Thead>
              <Table.Tr>
                <Table.Th>{t("maintenance.colKind")}</Table.Th>
                <Table.Th>{t("maintenance.colStatus")}</Table.Th>
                <Table.Th>{t("maintenance.colProgress")}</Table.Th>
                <Table.Th>{t("maintenance.colCreated")}</Table.Th>
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
                      <Progress
                        value={(task.ProcessedItems / task.TotalItems) * 100}
                        size="sm"
                        aria-label={t("maintenance.colProgress")}
                      />
                    ) : (
                      <Text size="sm" c="dimmed">
                        {task.ProcessedItems}/{task.TotalItems}
                      </Text>
                    )}
                  </Table.Td>
                  <Table.Td>
                    <Text size="sm">{formatStamp(task.CreatedAt)}</Text>
                    {task.ErrorMessage ? (
                      <Text size="xs" c="red">
                        {task.ErrorMessage}
                      </Text>
                    ) : null}
                  </Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        )}
      </Stack>
    </Card>
  );
}

/** 回收站：列表 + 恢复 / 彻底删除 / 清空。 */
function TrashCard({ refreshKey, onChanged }: { refreshKey: number; onChanged: () => void }) {
  const { t } = useTranslation();
  const [entries, setEntries] = useState<TrashEntry[]>([]);
  const [stats, setStats] = useState({ entries: 0, bytes: 0 });
  const [selected, setSelected] = useState<Set<number>>(new Set());
  // 待确认的彻底删除：真正执行在受控弹窗里（第三层防护）。
  const [pendingPurge, setPendingPurge] = useState<{ ids: number[]; all: boolean } | null>(null);

  const load = useCallback(() => {
    listTrash(1, 50)
      .then((res) => {
        setEntries(res.items);
        setStats(res.stats);
        setSelected(new Set());
      })
      .catch(() => setEntries([]));
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
    if (!pendingPurge) return;
    const action = pendingPurge.all ? purgeAllTrash() : purgeTrash(pendingPurge.ids);
    action
      .then(() => {
        notifications.show({ color: "green", message: t("maintenance.purged") });
        setPendingPurge(null);
        load();
        onChanged();
      })
      .catch(() => notifications.show({ color: "red", message: t("maintenance.actionFailed") }));
  };

  return (
    <Card withBorder radius="md" padding={density.cardPadding}>
      <Stack gap="sm">
        <Group justify="space-between" wrap="wrap">
          <Group gap="xs">
            <Title order={5}>{t("maintenance.trashTitle")}</Title>
            <Badge size="sm" variant="light">
              {t("maintenance.trashUsage", {
                count: stats.entries,
                bytes: formatBytes(stats.bytes),
              })}
            </Badge>
          </Group>
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
        </Group>
        <Text size="xs" c="dimmed">
          {t("maintenance.trashHint")}
        </Text>
        {entries.length === 0 ? (
          <Text size="sm" c="dimmed">
            {t("maintenance.trashEmpty")}
          </Text>
        ) : (
          <ScrollArea.Autosize mah={280}>
            <Table striped verticalSpacing={6}>
              <Table.Thead>
                <Table.Tr>
                  <Table.Th style={{ width: 36 }} />
                  <Table.Th>{t("maintenance.colKind")}</Table.Th>
                  <Table.Th>{t("maintenance.colPath")}</Table.Th>
                  <Table.Th>{t("maintenance.colSize")}</Table.Th>
                  <Table.Th>{t("maintenance.colCreated")}</Table.Th>
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
          </ScrollArea.Autosize>
        )}
      </Stack>
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
      />
    </Card>
  );
}

/** 维护作业：清单 + 立即执行（**物理删除，不可恢复**）。 */
function MaintenanceJobsCard() {
  const { t } = useTranslation();
  const state = useAsync(listMaintenanceJobs, [], { cacheKey: "maintenance-jobs" });
  const [pendingJob, setPendingJob] = useState<MaintenanceJob | null>(null);

  const runPendingJob = () => {
    if (!pendingJob) return;
    runMaintenanceJob(pendingJob.name)
      .then(() => {
        notifications.show({ color: "green", message: t("maintenance.jobAccepted") });
        setPendingJob(null);
      })
      .catch(() => notifications.show({ color: "red", message: t("maintenance.actionFailed") }));
  };

  return (
    <Card withBorder radius="md" padding={density.cardPadding}>
      <Stack gap="sm">
        <Group justify="space-between" wrap="wrap">
          <Title order={5}>{t("maintenance.jobsTitle")}</Title>
          <Text size="xs" c="dimmed">
            {t("maintenance.jobsHint")}
          </Text>
        </Group>
        <AsyncBoundary state={state}>
          {(res: { items: MaintenanceJob[] }) =>
            res.items.length === 0 ? (
              <Text size="sm" c="dimmed">
                {t("maintenance.jobsEmpty")}
              </Text>
            ) : (
              <Table striped verticalSpacing={6}>
                <Table.Thead>
                  <Table.Tr>
                    <Table.Th>{t("maintenance.colJob")}</Table.Th>
                    <Table.Th>{t("maintenance.colInterval")}</Table.Th>
                    <Table.Th>{t("maintenance.colLastRun")}</Table.Th>
                    <Table.Th>{t("maintenance.colFailures")}</Table.Th>
                    <Table.Th>{t("maintenance.colActions")}</Table.Th>
                  </Table.Tr>
                </Table.Thead>
                <Table.Tbody>
                  {res.items.map((job) => (
                    <Table.Tr key={job.name} data-job-name={job.name}>
                      <Table.Td>{job.name}</Table.Td>
                      <Table.Td>{job.intervalSeconds}s</Table.Td>
                      <Table.Td>{formatStamp(job.lastFinishedAt ?? job.lastStartedAt)}</Table.Td>
                      <Table.Td>{job.failures ?? 0}</Table.Td>
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
            )
          }
        </AsyncBoundary>
      </Stack>
      <DestructiveConfirmModal
        opened={pendingJob !== null}
        title={t("maintenance.runJobTitle", { name: pendingJob?.name ?? "" })}
        message={t("maintenance.runJobIrreversible")}
        onConfirm={runPendingJob}
        onClose={() => setPendingJob(null)}
      />
    </Card>
  );
}

export function MaintenancePage() {
  const { t } = useTranslation();
  const [preview, setPreview] = useState<CleanupPreview | null>(null);
  const [selectedPaths, setSelectedPaths] = useState<Set<string>>(new Set());
  const [confirming, setConfirming] = useState(false);
  const [refreshKey, setRefreshKey] = useState(0);

  const bump = () => setRefreshKey((n) => n + 1);

  // 第一层防护：预览清单（可逐项勾选）。预览本身**只读**。
  const openPreview = (kind: CleanupKind) => {
    previewCleanup(kind)
      .then((res) => {
        setPreview(res);
        setSelectedPaths(new Set(res.items.map((item) => item.path)));
      })
      .catch(() => notifications.show({ color: "red", message: t("maintenance.actionFailed") }));
  };

  const chosen = preview ? preview.items.filter((item) => selectedPaths.has(item.path)) : [];
  const chosenBytes = chosen.reduce((sum, item) => sum + item.bytes, 0);

  const runCleanup = () => {
    if (!preview || chosen.length === 0) return;
    executeCleanup({ kind: preview.kind, items: chosen.map((item) => item.path) })
      .then(() => {
        notifications.show({ color: "green", message: t("maintenance.cleanupDone") });
        setConfirming(false);
        setPreview(null);
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
      });
  };

  return (
    <PageShell>
      <Box style={{ flex: 1, minHeight: 0, overflowY: "auto" }}>
        <Stack gap="md">
          <StorageHealthCard onCleanup={openPreview} />
          {preview ? (
            <Card
              withBorder
              radius="md"
              padding={density.cardPadding}
              data-preview-kind={preview.kind}
            >
              <Stack gap="sm">
                <Group justify="space-between" wrap="wrap">
                  <Title order={5}>{t("maintenance.previewTitle", { kind: preview.kind })}</Title>
                  <Text size="xs" c="dimmed">
                    {t("maintenance.previewSummary", {
                      count: chosen.length,
                      bytes: formatBytes(chosenBytes),
                    })}
                  </Text>
                </Group>
                <Text size="xs" c="dimmed">
                  {t("maintenance.previewHint")}
                </Text>
                {preview.skipped && preview.skipped.length > 0 ? (
                  <Alert color="yellow" variant="light" icon={<IconAlertTriangle size={16} />}>
                    {t("maintenance.previewSkipped", { count: preview.skipped.length })}
                  </Alert>
                ) : null}
                <ScrollArea.Autosize mah={240}>
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
                        checked={selectedPaths.has(item.path)}
                        onChange={() =>
                          setSelectedPaths((prev) => {
                            const next = new Set(prev);
                            if (next.has(item.path)) next.delete(item.path);
                            else next.add(item.path);
                            return next;
                          })
                        }
                      />
                    ))}
                  </Stack>
                </ScrollArea.Autosize>
                <Group gap="xs">
                  <Button
                    size="sm"
                    color="red"
                    disabled={chosen.length === 0}
                    onClick={() => setConfirming(true)}
                  >
                    {t("maintenance.cleanupConfirm")}
                  </Button>
                  <Button size="sm" variant="default" onClick={() => setPreview(null)}>
                    {t("common.cancel")}
                  </Button>
                </Group>
              </Stack>
            </Card>
          ) : null}
          <CleanTasksCard refreshKey={refreshKey} />
          <TrashCard refreshKey={refreshKey} onChanged={bump} />
          <MaintenanceJobsCard />
        </Stack>
      </Box>
      <DestructiveConfirmModal
        opened={confirming}
        title={t("maintenance.cleanupTitle")}
        message={t("maintenance.cleanupMessage", {
          count: chosen.length,
          bytes: formatBytes(chosenBytes),
        })}
        onConfirm={runCleanup}
        onClose={() => setConfirming(false)}
      />
    </PageShell>
  );
}
