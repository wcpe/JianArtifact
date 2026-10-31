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
  const [partition, setPartition] = useState<TrashPartition>("artifacts");
  return (
    <Stack gap="sm" style={{ flex: 1, minHeight: 0 }}>
      <Alert color="blue" variant="light" icon={<IconAlertTriangle size={16} />}>
        这里的条目都<strong>仍占用磁盘</strong>，且都能恢复。只有「彻底清理」或保留期到期才会真正释放空间。
      </Alert>

      <Tabs
        value={partition}
        onChange={(v) => setPartition((v as TrashPartition) ?? "artifacts")}
        keepMounted={false}
        style={{ flex: 1, minHeight: 0, display: "flex", flexDirection: "column" }}
      >
        <Tabs.List style={{ flexShrink: 0 }}>
          <Tabs.Tab value="artifacts">制品（删除的版本）</Tabs.Tab>
          <Tabs.Tab value="storage">存储清理（作业扫出的垃圾）</Tabs.Tab>
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

function ArtifactTrashSection({ refreshKey, onChanged }: UnifiedTrashPanelProps) {
  const { t } = useTranslation();
  const [entries, setEntries] = useState<AssetTrashEntry[]>([]);
  const [stats, setStats] = useState({ entries: 0, bytes: 0 });
  const [selected, setSelected] = useState<Set<number>>(new Set());
  const [loadFailed, setLoadFailed] = useState(false);
  const [pendingPurge, setPendingPurge] = useState<number[] | null>(null);
  const [purging, setPurging] = useState(false);

  const load = useCallback(() => {
    listAssetTrash(1, 100)
      .then((res) => {
        setEntries(res.items ?? []);
        setStats({ entries: res.stats?.entries ?? 0, bytes: res.stats?.bytes ?? 0 });
        setLoadFailed(false);
      })
      // 失败保留上次数据：清空会让"回收站真的空了"与"加载失败"看起来一样。
      .catch(() => setLoadFailed(true));
  }, []);

  useEffect(load, [load, refreshKey]);

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
            title: "部分条目未恢复",
            message: problems.map((c) => `${c.path}：${c.reason}`).join("\n"),
          });
        }
        if (res.restored > 0) {
          notifications.show({ color: "green", message: `已恢复 ${res.restored} 个文件` });
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
          message: `已彻底清理 ${res.purged} 个文件，释放 ${formatBytes(res.freedBytes)}`,
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
    <Box data-testid="asset-trash-section" style={{ flex: 1, minHeight: 0, display: "flex", flexDirection: "column" }}>
      <Group justify="space-between" mb="xs">
        <Text size="sm" c="dimmed">
          共 {stats.entries} 条 / {formatBytes(stats.bytes)}
          {selected.size > 0 && `（已选 ${selected.size} 条 / ${formatBytes(selectedBytes)}）`}
        </Text>
        <Group gap="xs">
          <Button
            size="xs"
            variant="default"
            disabled={selected.size === 0}
            onClick={() => handleRestore([...selected])}
          >
            恢复所选（{selected.size}）
          </Button>
          <Button
            size="xs"
            color="red"
            variant="light"
            disabled={selected.size === 0}
            onClick={() => setPendingPurge([...selected])}
          >
            彻底清理（{selected.size}）
          </Button>
        </Group>
      </Group>

      {loadFailed && (
        <Alert color="yellow" variant="light" mb="xs">
          加载失败，列表可能不是最新状态；下方内容为上次成功加载的结果。
        </Alert>
      )}

      <Box style={{ flex: 1, minHeight: 120, overflow: "auto" }}>
        <Table striped highlightOnHover miw={820} stickyHeader verticalSpacing={6}>
          <Table.Thead>
            <Table.Tr>
              <Table.Th w={36} />
              <Table.Th w={130}>仓库</Table.Th>
              <Table.Th>路径</Table.Th>
              <Table.Th w={90}>大小</Table.Th>
              <Table.Th w={150}>删除时间</Table.Th>
              <Table.Th w={110}>来源</Table.Th>
              <Table.Th w={150}>操作</Table.Th>
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {entries.length === 0 ? (
              <Table.Tr>
                <Table.Td colSpan={7}>
                  <Text size="sm" c="dimmed" ta="center" py="lg">
                    {loadFailed ? "—" : "没有已删除的制品。在「Maven 包清理」里删除的版本会出现在这里。"}
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
                      {entry.scope === "snapshot_build" ? "快照构建" : "整个版本"}
                    </Badge>
                  </Table.Td>
                  <Table.Td>
                    <Group gap={4}>
                      <Button size="compact-xs" variant="default" onClick={() => handleRestore([entry.id])}>
                        恢复
                      </Button>
                      <Button
                        size="compact-xs"
                        color="red"
                        variant="subtle"
                        onClick={() => setPendingPurge([entry.id])}
                      >
                        彻底清理
                      </Button>
                    </Group>
                  </Table.Td>
                </Table.Tr>
              ))
            )}
          </Table.Tbody>
        </Table>
      </Box>

      <DestructiveConfirmModal
        opened={pendingPurge !== null}
        title="确认彻底清理"
        message={
          `将永久释放 ${pendingPurge?.length ?? 0} 个文件占用的磁盘空间，此操作**不可恢复**。\n\n` +
          `若只是想腾出可见空间、仍可能用到这些文件，请改用「恢复」。`
        }
        requirePhrase
        pending={purging}
        onConfirm={runPurge}
        onClose={() => setPendingPurge(null)}
      />
    </Box>
  );
}

// ───────────────────────── 分区二：存储清理 ─────────────────────────

function StorageTrashSection({ refreshKey, onChanged }: UnifiedTrashPanelProps) {
  const { t } = useTranslation();
  const [entries, setEntries] = useState<TrashEntry[]>([]);
  const [stats, setStats] = useState({ entries: 0, bytes: 0 });
  const [selected, setSelected] = useState<Set<number>>(new Set());
  const [pendingPurge, setPendingPurge] = useState<{ ids: number[]; all: boolean } | null>(null);
  const [loadFailed, setLoadFailed] = useState(false);
  const [purging, setPurging] = useState(false);

  const load = useCallback(() => {
    listTrash(1, 50)
      .then((res) => {
        setEntries(res.items ?? []);
        setStats({ entries: res.stats?.entries ?? 0, bytes: res.stats?.bytes ?? 0 });
        setSelected(new Set());
        setLoadFailed(false);
      })
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
    <Box data-testid="storage-trash-section" style={{ flex: 1, minHeight: 0, display: "flex", flexDirection: "column" }}>
      <Group justify="space-between" mb="xs">
        <Text size="sm" c="dimmed">
          共 {stats.entries} 条 / {formatBytes(stats.bytes)}
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
                      <Button size="compact-xs" variant="default" onClick={() => handleRestore([entry.ID])}>
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
    </Box>
  );
}
