// 统一回收站：一个入口，两个分区。
//
// 为什么合并（而不是并列两个 Tab）：
// 系统里存在**两类**被"删除但还能找回"的东西，它们的存储与恢复机制完全不同：
//
//   1. 制品（asset_trash）：你删掉的 Maven 版本。恢复 = 重建 asset 元数据行，
//      内容仍是内容寻址 blob。
//   2. 存储清理（trash_entry）：清理作业扫出的垃圾（空目录、过期上传临时目录、
//      回滚备份、隔离区待回收）。恢复 = 把磁盘上的文件/目录 rename 回原位置。
//
// 早先这两类各占一个 Tab，都叫「回收站」/「资产回收站」——用户删完东西不知道该去
// 哪个里面找，这是**入口的错**，不是用户的错。现在合并成一个 Tab、两个分区：
// 找东西只有一个地方，而"这两类为什么不同"由分区标题直接说明。
//
// 必须守住的既有防护（合并时一条都不能丢）：
//   - 列表加载失败**不清空**数据：一次网络抖动不该看起来像回收站空了；
//   - 恢复只做**一次点击**（反向操作加三道关只会让人不敢用）；
//   - 彻底清理走确认弹窗 + 输入确认词（不可逆）；
//   - 分区各自独立加载：一类失败不能连带另一类显示为空。
import {
  Alert,
  Badge,
  Box,
  Button,
  Checkbox,
  Code,
  Group,
  Pagination,
  Stack,
  Table,
  Tabs,
  Text,
} from "@mantine/core";
import { notifications } from "@mantine/notifications";
import { IconAlertTriangle } from "@tabler/icons-react";
import { useCallback, useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import {
  listAssetTrash,
  listTrash,
  purgeAllTrash,
  purgeAssetTrash,
  purgeTrash,
  restoreAssetTrash,
  restoreTrash,
  type AssetTrashEntry,
  type TrashEntry,
} from "../../api/endpoints";
import { DestructiveConfirmModal } from "./DestructiveConfirmModal";
import { formatBytes, formatStamp } from "../../lib/format";

export interface UnifiedTrashPanelProps {
  /** 外部触发的刷新信号（删除/清理动作后由父级 bump）。 */
  refreshKey: number;
  /** 任一回收站内容变化后通知父级。 */
  onChanged: () => void;
}

/** 会话内保留的分区选择，切走再回来不会跳回默认分区。 */
type TrashPartition = "artifacts" | "storage";

export function UnifiedTrashPanel({ refreshKey, onChanged }: UnifiedTrashPanelProps) {
  const { t } = useTranslation();
  const [partition, setPartition] = useState<TrashPartition>("artifacts");
  return (
    <Stack gap="sm" style={{ flex: 1, minHeight: 0 }}>
      <Alert color="blue" variant="light" icon={<IconAlertTriangle size={16} />}>
        {t("maintenance.trashStillOnDisk")}
      </Alert>

      <Tabs
        value={partition}
        onChange={(v) => setPartition((v as TrashPartition) ?? "artifacts")}
        keepMounted={false}
        style={{ flex: 1, minHeight: 0, display: "flex", flexDirection: "column" }}
      >
        <Tabs.List style={{ flexShrink: 0 }}>
          <Tabs.Tab value="artifacts">{t("maintenance.trashPartitionArtifacts")}</Tabs.Tab>
          <Tabs.Tab value="storage">{t("maintenance.trashPartitionStorage")}</Tabs.Tab>
        </Tabs.List>

        <Tabs.Panel
          value="artifacts"
          pt="md"
          style={{ flex: 1, minHeight: 0, display: "flex", flexDirection: "column" }}
        >
          <ArtifactTrashSection refreshKey={refreshKey} onChanged={onChanged} />
        </Tabs.Panel>

        <Tabs.Panel
          value="storage"
          pt="md"
          style={{ flex: 1, minHeight: 0, display: "flex", flexDirection: "column" }}
        >
          <StorageTrashSection refreshKey={refreshKey} onChanged={onChanged} />
        </Tabs.Panel>
      </Tabs>
    </Stack>
  );
}

// ───────────────────────── 分区一：制品 ─────────────────────────

/** 每页条数。后端 `AssetTrashRepo.List` 把 limit 钳到 100，取 20 与之无冲突。 */
const ASSET_TRASH_PAGE_SIZE = 20;

function ArtifactTrashSection({ refreshKey, onChanged }: UnifiedTrashPanelProps) {
  const { t } = useTranslation();
  const [entries, setEntries] = useState<AssetTrashEntry[]>([]);
  const [stats, setStats] = useState({ entries: 0, bytes: 0 });
  // 服务端返回的**全量** staged 条数（不是本页条数）。分页页数由它算，
  // 不能拿 `entries.length` 算——那永远只有一页。
  const [total, setTotal] = useState(0);
  const [page, setPage] = useState(1);
  const [selected, setSelected] = useState<Set<number>>(new Set());
  const [loadFailed, setLoadFailed] = useState(false);
  const [pendingPurge, setPendingPurge] = useState<number[] | null>(null);
  const [purging, setPurging] = useState(false);

  // 分页拉取：早先固定 `listAssetTrash(1, 100)`，超过 100 条的部分既看不到也选不到，
  // 而顶部仍显示真实总数——用户以为「就这些」。现在按页取，且总数如实来自后端。
  const load = useCallback(() => {
    listAssetTrash(page, ASSET_TRASH_PAGE_SIZE)
      .then((res) => {
        setEntries(res.items ?? []);
        setStats({ entries: res.stats?.entries ?? 0, bytes: res.stats?.bytes ?? 0 });
        setTotal(res.total ?? 0);
        setLoadFailed(false);
      })
      // 失败保留上次数据：清空会让"回收站真的空了"与"加载失败"看起来一样。
      .catch(() => setLoadFailed(true));
  }, [page]);

  useEffect(load, [load, refreshKey]);

  const totalPages = Math.max(1, Math.ceil(total / ASSET_TRASH_PAGE_SIZE));

  // 条目集合变化后清理失效勾选：恢复/清理会让 id 从列表消失，
  // 选中集合不自动收敛，会留下一个"显示 2 条但列表已空"的按钮。
  useEffect(() => {
    setSelected((prev) => {
      if (prev.size === 0) return prev;
      const alive = new Set(entries.map((e) => e.id));
      const next = new Set<number>();
      for (const id of prev) if (alive.has(id)) next.add(id);
      return next.size === prev.size ? prev : next;
    });
  }, [entries]);

  const toggle = (id: number) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  };

  const handleRestore = (ids: number[]) => {
    restoreAssetTrash(ids)
      .then((res) => {
        // 冲突与缺失必须摊开：它们是"以为恢复了、其实没有"的唯一线索。
        const problems = [...(res.conflicts ?? []), ...(res.missing ?? [])];
        if (problems.length > 0) {
          notifications.show({
            color: "yellow",
            title: t("maintenance.restoreConflictsTitle"),
            message: problems.map((c) => `${c.path}：${c.reason}`).join("\n"),
          });
        }
        if (res.restored > 0) {
          notifications.show({
            color: "green",
            message: t("maintenance.trashRestoredFiles", { count: res.restored }),
          });
        }
        load();
        onChanged();
      })
      .catch(() => notifications.show({ color: "red", message: t("maintenance.actionFailed") }));
  };

  const runPurge = () => {
    if (!pendingPurge || purging) return;
    setPurging(true);
    purgeAssetTrash(pendingPurge)
      .then((res) => {
        notifications.show({
          color: "green",
          message: t("maintenance.trashPurgedFiles", {
            count: res.purged,
            bytes: formatBytes(res.freedBytes),
          }),
        });
        setPendingPurge(null);
        load();
        onChanged();
      })
      .catch(() => notifications.show({ color: "red", message: t("maintenance.actionFailed") }))
      .finally(() => setPurging(false));
  };

  const selectedBytes = entries
    .filter((e) => selected.has(e.id))
    .reduce((sum, e) => sum + e.size, 0);

  return (
    <Box
      data-testid="asset-trash-section"
      style={{ flex: 1, minHeight: 0, display: "flex", flexDirection: "column" }}
    >
      <Group justify="space-between" mb="xs">
        <Text size="sm" c="dimmed">
          {/* 计数必须带出「当前页范围 / 全量」：只显示总数会让用户以为列表就是全部，
              而实际每页只有 ASSET_TRASH_PAGE_SIZE 条。 */}
          {t("maintenance.trashPageUsage", {
            from: total === 0 ? 0 : (page - 1) * ASSET_TRASH_PAGE_SIZE + 1,
            to: Math.min(page * ASSET_TRASH_PAGE_SIZE, total),
            total,
            bytes: formatBytes(stats.bytes),
          })}
          {selected.size > 0 &&
            t("maintenance.trashSelectedSuffix", {
              count: selected.size,
              bytes: formatBytes(selectedBytes),
            })}
        </Text>
        <Group gap="xs">
          <Button
            size="xs"
            variant="default"
            disabled={selected.size === 0}
            onClick={() => handleRestore([...selected])}
          >
            {t("maintenance.trashRestoreSelectedCount", { count: selected.size })}
          </Button>
          <Button
            size="xs"
            color="red"
            variant="light"
            disabled={selected.size === 0}
            onClick={() => setPendingPurge([...selected])}
          >
            {t("maintenance.trashPurgeSelectedCount", { count: selected.size })}
          </Button>
        </Group>
      </Group>

      {loadFailed && (
        <Alert color="yellow" variant="light" mb="xs">
          {t("maintenance.trashStaleHint")}
        </Alert>
      )}

      <Box style={{ flex: 1, minHeight: 120, overflow: "auto" }}>
        <Table striped highlightOnHover miw={820} stickyHeader verticalSpacing={6}>
          <Table.Thead>
            <Table.Tr>
              <Table.Th w={36} />
              <Table.Th w={130}>{t("maintenance.trashColRepository")}</Table.Th>
              <Table.Th>{t("maintenance.colPath")}</Table.Th>
              <Table.Th w={90}>{t("maintenance.colSize")}</Table.Th>
              <Table.Th w={150}>{t("maintenance.trashColTrashedAt")}</Table.Th>
              <Table.Th w={110}>{t("maintenance.trashColSource")}</Table.Th>
              <Table.Th w={150}>{t("maintenance.colActions")}</Table.Th>
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {entries.length === 0 ? (
              <Table.Tr>
                <Table.Td colSpan={7}>
                  <Text size="sm" c="dimmed" ta="center" py="lg">
                    {loadFailed ? "—" : t("maintenance.trashArtifactsEmpty")}
                  </Text>
                </Table.Td>
              </Table.Tr>
            ) : (
              entries.map((entry) => (
                <Table.Tr key={entry.id} data-asset-trash-id={entry.id}>
                  <Table.Td>
                    <Checkbox
                      aria-label={entry.path}
                      checked={selected.has(entry.id)}
                      onChange={() => toggle(entry.id)}
                    />
                  </Table.Td>
                  <Table.Td>{entry.repositoryName || "—"}</Table.Td>
                  <Table.Td>
                    <Code style={{ wordBreak: "break-all" }}>{entry.path}</Code>
                  </Table.Td>
                  <Table.Td>{formatBytes(entry.size)}</Table.Td>
                  <Table.Td>
                    <Text size="xs" c="dimmed">
                      {entry.trashedAt}
                    </Text>
                  </Table.Td>
                  <Table.Td>
                    <Badge size="sm" variant="light">
                      {entry.scope === "snapshot_build"
                        ? t("maintenance.trashScopeSnapshotBuild")
                        : t("maintenance.trashScopeWholeVersion")}
                    </Badge>
                  </Table.Td>
                  <Table.Td>
                    <Group gap={4}>
                      <Button
                        size="compact-xs"
                        variant="default"
                        onClick={() => handleRestore([entry.id])}
                      >
                        {t("maintenance.restore")}
                      </Button>
                      <Button
                        size="compact-xs"
                        color="red"
                        variant="subtle"
                        onClick={() => setPendingPurge([entry.id])}
                      >
                        {t("maintenance.purge")}
                      </Button>
                    </Group>
                  </Table.Td>
                </Table.Tr>
              ))
            )}
          </Table.Tbody>
        </Table>
      </Box>

      {/* 分页只在多于一页时出现：单页时留一排页码只会让人以为还有别处内容。 */}
      {totalPages > 1 ? (
        <Group justify="center" mt="xs">
          <Pagination
            value={page}
            onChange={setPage}
            total={totalPages}
            size="sm"
            data-testid="asset-trash-pagination"
          />
        </Group>
      ) : null}

      <DestructiveConfirmModal
        opened={pendingPurge !== null}
        title={t("maintenance.trashPurgeConfirmTitle")}
        message={t("maintenance.trashPurgeConfirmMessage", {
          count: pendingPurge?.length ?? 0,
        })}
        requirePhrase
        pending={purging}
        onConfirm={runPurge}
        onClose={() => setPendingPurge(null)}
      />
    </Box>
  );
}

// ───────────────────────── 分区二：存储清理 ─────────────────────────

/** 每页条数。与资产分区分开取值：两类条目形态不同，共用常量会互相牵制。 */
const STORAGE_TRASH_PAGE_SIZE = 20;

function StorageTrashSection({ refreshKey, onChanged }: UnifiedTrashPanelProps) {
  const { t } = useTranslation();
  const [entries, setEntries] = useState<TrashEntry[]>([]);
  const [stats, setStats] = useState({ entries: 0, bytes: 0 });
  const [total, setTotal] = useState(0);
  const [page, setPage] = useState(1);
  const [selected, setSelected] = useState<Set<number>>(new Set());
  const [pendingPurge, setPendingPurge] = useState<{ ids: number[]; all: boolean } | null>(null);
  const [loadFailed, setLoadFailed] = useState(false);
  const [purging, setPurging] = useState(false);

  // 早先固定 `listTrash(1, 50)`，而顶部用的是 `stats.entries`（全表计数）：
  // 真实有 200 条时界面显示「共 200 项」却只列 50 行，"清空回收站"又会真的删光 200 条
  // ——看到的和发生的不是一回事。现在按页取，计数改由 `total` 驱动。
  const load = useCallback(() => {
    listTrash(page, STORAGE_TRASH_PAGE_SIZE)
      .then((res) => {
        setEntries(res.items ?? []);
        setStats({ entries: res.stats?.entries ?? 0, bytes: res.stats?.bytes ?? 0 });
        setTotal(res.total ?? 0);
        setSelected(new Set());
        setLoadFailed(false);
      })
      .catch(() => setLoadFailed(true));
  }, [page]);

  useEffect(load, [load, refreshKey]);

  const totalPages = Math.max(1, Math.ceil(total / STORAGE_TRASH_PAGE_SIZE));

  const toggle = (id: number) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  };

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
    <Box
      data-testid="storage-trash-section"
      style={{ flex: 1, minHeight: 0, display: "flex", flexDirection: "column" }}
    >
      <Group justify="space-between" mb="xs">
        <Text size="sm" c="dimmed">
          {t("maintenance.trashPageUsage", {
            from: total === 0 ? 0 : (page - 1) * STORAGE_TRASH_PAGE_SIZE + 1,
            to: Math.min(page * STORAGE_TRASH_PAGE_SIZE, total),
            total,
            bytes: formatBytes(stats.bytes),
          })}
        </Text>
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

      {loadFailed && (
        <Alert
          color="yellow"
          variant="light"
          icon={<IconAlertTriangle size={16} />}
          title={t("common.refreshFailedTitle")}
          mb="xs"
        >
          {t("maintenance.loadFailedHint")}
        </Alert>
      )}

      <Box style={{ flex: 1, minHeight: 120, overflow: "auto" }}>
        {entries.length === 0 ? (
          <Box p="lg">
            <Stack gap={4}>
              <Text size="sm" c="dimmed" ta="center">
                {t("maintenance.trashEmpty")}
              </Text>
              <Text size="xs" c="dimmed" ta="center">
                {t("maintenance.trashHint")}
              </Text>
            </Stack>
          </Box>
        ) : (
          <Table striped verticalSpacing={6} stickyHeader miw={820}>
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
                    {/* 保留期只存在于服务端配置，接口不回该字段；硬编码天数会说错话。 */}
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
      </Box>

      {totalPages > 1 ? (
        <Group justify="center" mt="xs">
          <Pagination
            value={page}
            onChange={setPage}
            total={totalPages}
            size="sm"
            data-testid="storage-trash-pagination"
          />
        </Group>
      ) : null}

      <DestructiveConfirmModal
        opened={pendingPurge !== null}
        title={pendingPurge?.all ? t("maintenance.purgeAllTitle") : t("maintenance.purgeTitle")}
        message={
          pendingPurge?.all
            ? // 「清空」会删掉**全量**而非当前页，所以数必须用 `total`——
              // 用当前页条数会让用户以为只清这些，实际删得更多。
              t("maintenance.purgeAllMessage", { count: total })
            : t("maintenance.purgeMessage", { count: pendingPurge?.ids.length ?? 0 })
        }
        onConfirm={runPendingPurge}
        onClose={() => setPendingPurge(null)}
        pending={purging}
      />
    </Box>
  );
}
