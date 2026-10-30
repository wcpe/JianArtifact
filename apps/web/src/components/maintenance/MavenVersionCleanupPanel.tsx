// Maven 版本清理面板：「按坐标一次性找出全部命名锚点 → 预览 → 三层防护删除」。
//
// 为什么要有这个面板（而不是复用文件树的版本目录删除）：
// 文件树只能让用户看到"当前目录下有哪些文件"，而版本清理的真正难点是**看见全部**——
// 同一版本目录下会同时存在主档、多个 classifier、各种校验和、版本级 metadata，
// SNAPSHOT 还会累积几十个时间戳历史构建。靠肉眼在树里逐层数，一定会漏；
// 而漏掉一个文件的后果是 metadata 或校验和指向不存在的内容。
//
// 因此这里的核心是**服务端按目录前缀展开后的分类清单**：用户看到的是
// "这个版本一共 N 个文件、M 字节、按类别如何分布、有哪几次构建"，
// 而不是一堆需要自己数路径的行。
//
// 防护分层沿用本页既有范式，不另造一套：
//   预览清单（可逐项勾选）→ 二次确认 → 输入确认词「清理」。
// 但这里有一处**必须与存储清理不同**的口径：删完后东西**还在回收站里**，
// 所以确认文案不能写"不可恢复"，否则用户会以为没有退路而不敢用。
import {
  Alert,
  Badge,
  Box,
  Button,
  Checkbox,
  Code,
  Group,
  ScrollArea,
  SimpleGrid,
  Stack,
  Table,
  Text,
  TextInput,
} from "@mantine/core";
import { notifications } from "@mantine/notifications";
import { IconAlertTriangle, IconSearch, IconTrash } from "@tabler/icons-react";
import { useCallback, useEffect, useMemo, useState } from "react";
import {
  planMavenVersion,
  trashMavenVersion,
  type MavenVersionFile,
  type MavenVersionPlan,
} from "../../api/endpoints";
import { DestructiveConfirmModal, DESTRUCTIVE_CONFIRM_WORD } from "./DestructiveConfirmModal";
import { formatBytes } from "../../lib/format";

/** 类别到中文标签。顺序即展示顺序，与后端 Categories 的固定顺序一致。 */
const CATEGORY_LABELS: Record<MavenVersionFile["category"], string> = {
  primary: "主档",
  classifier: "Classifiers",
  checksum: "校验和",
  metadata: "元数据",
  other: "其它",
};

export interface MavenVersionCleanupPanelProps {
  /** 可选仓库（当前实例的 Maven hosted 仓库）。 */
  repositories: { name: string }[];
  /** 回收站有变化后通知父级刷新。 */
  onTrashed: () => void;
}

/** 解析 `group:artifact:version` 或 `group:artifact:version:buildId` 形式的输入。 */
type ParsedCoordinate = {
  groupId: string;
  artifactId: string;
  version: string;
  buildId?: string;
};

function parseCoordinate(raw: string): ParsedCoordinate | null {
  const parts = raw
    .trim()
    .split(":")
    .map((p) => p.trim());
  if (parts.length < 3) return null;
  const [groupId, artifactId, version, buildId] = parts;
  if (!groupId || !artifactId || !version) return null;
  const coord: ParsedCoordinate = { groupId, artifactId, version };
  if (buildId) coord.buildId = buildId;
  return coord;
}

export function MavenVersionCleanupPanel({ repositories, onTrashed }: MavenVersionCleanupPanelProps) {
  // 同 MavenPackageBatchPanel：仓库列表异步加载，初始值不能只取一次，
  // 否则组件首次挂载时列表为空、repo 永远停在空串。
  const [repo, setRepo] = useState(repositories[0]?.name ?? "");
  useEffect(() => {
    const first = repositories[0];
    if (!first) return;
    setRepo((current) =>
      current && repositories.some((r) => r.name === current) ? current : first.name,
    );
  }, [repositories]);
  const [coordinate, setCoordinate] = useState("");
  const [plan, setPlan] = useState<MavenVersionPlan | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // 逐项勾选：默认全选（"删这个版本"是默认意图），但用户可以剔掉想保留的文件。
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [confirmOpen, setConfirmOpen] = useState(false);
  const [trashing, setTrashing] = useState(false);

  const parsed = useMemo(() => parseCoordinate(coordinate), [coordinate]);

  const loadPlan = useCallback(() => {
    if (!repo || !parsed) {
      setError("请填写形如 group:artifact:version 的坐标");
      return;
    }
    setLoading(true);
    setError(null);
    planMavenVersion(repo, parsed)
      .then((res) => {
        setPlan(res);
        // 只删单次构建时，默认勾选范围应与范围语义一致：
        // 服务端已按 buildId 收敛，这里直接全选它给出的文件即可。
        setSelected(new Set(res.files.map((f) => f.path)));
      })
      .catch((err) => {
        setPlan(null);
        setError(err instanceof Error ? err.message : "预览失败");
      })
      .finally(() => setLoading(false));
  }, [repo, parsed]);

  const toggle = (path: string) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(path)) next.delete(path);
      else next.add(path);
      return next;
    });
  };

  const selectedFiles = plan?.files.filter((f) => selected.has(f.path)) ?? [];
  const selectedBytes = selectedFiles.reduce((sum, f) => sum + f.size, 0);

  const runTrash = () => {
    if (!parsed || trashing) return;
    setTrashing(true);
    trashMavenVersion(repo, {
      groupId: parsed.groupId,
      artifactId: parsed.artifactId,
      version: parsed.version,
      ...(parsed.buildId ? { buildId: parsed.buildId } : {}),
      paths: selectedFiles.map((f) => f.path),
      overrideReason: `从维护页清理 Maven 版本 ${parsed.groupId}:${parsed.artifactId}:${parsed.version}`,
    })
      .then((res) => {
        notifications.show({
          color: "green",
          message: `已移入回收站：${res.trashed} 个文件 / ${formatBytes(res.bytes)}（${res.batches} 批）`,
        });
        setConfirmOpen(false);
        // 删完重新预览：让用户立刻看到该版本还剩什么（可能是部分删除或已空）。
        loadPlan();
        onTrashed();
      })
      .catch((err) =>
        notifications.show({
          color: "red",
          message: err instanceof Error ? err.message : "删除失败",
        }),
      )
      .finally(() => setTrashing(false));
  };

  return (
    <Stack gap="sm" style={{ flex: 1, minHeight: 0 }}>
      <Alert color="blue" variant="light" icon={<IconAlertTriangle size={16} />}>
        删除是<strong>可恢复</strong>的：文件会进入下方「资产回收站」，blob 保留到你彻底清理或保留期到期。
      </Alert>

      <Group align="flex-end" gap="sm" wrap="wrap">
        <Box style={{ minWidth: 200 }}>
          <Text size="xs" c="dimmed" mb={4}>
            仓库
          </Text>
          <select
            value={repo}
            onChange={(e) => setRepo(e.currentTarget.value)}
            aria-label="Maven 仓库"
            style={{ width: "100%", height: 36, borderRadius: 4, padding: "0 8px" }}
          >
            {repositories.map((r) => (
              <option key={r.name} value={r.name}>
                {r.name}
              </option>
            ))}
          </select>
        </Box>
        <TextInput
          label="坐标"
          placeholder="com.example:demo:1.17-SNAPSHOT"
          description="group:artifact:version，SNAPSHOT 可在第 4 段指定单次构建"
          value={coordinate}
          onChange={(e) => setCoordinate(e.currentTarget.value)}
          style={{ flex: 1, minWidth: 280 }}
        />
        <Button
          leftSection={<IconSearch size={16} />}
          onClick={loadPlan}
          loading={loading}
          disabled={!repo || !parsed}
        >
          预览清单
        </Button>
      </Group>

      {error && (
        <Alert color="red" variant="light">
          {error}
        </Alert>
      )}

      {plan && !plan.exists && (
        <Alert color="yellow" variant="light">
          该坐标在 <Code>{repo}</Code> 下没有制品。请检查 group/artifact/version 是否正确。
        </Alert>
      )}

      {plan && plan.exists && (
        <>
          <SimpleGrid cols={{ base: 2, sm: 4 }} spacing="sm">
            <Stat label="文件总数" value={String(plan.totalFiles)} />
            <Stat label="总大小" value={formatBytes(plan.totalBytes)} />
            <Stat label="已选" value={String(selectedFiles.length)} />
            <Stat label="已选大小" value={formatBytes(selectedBytes)} />
          </SimpleGrid>

          {plan.categories && plan.categories.length > 0 && (
            <Group gap={4}>
              {plan.categories.map((c) => (
                <Badge key={c.category} variant="light">
                  {CATEGORY_LABELS[c.category as MavenVersionFile["category"]] ?? c.category}{" "}
                  {c.fileCount}
                </Badge>
              ))}
            </Group>
          )}

          {plan.snapshotBuilds && plan.snapshotBuilds.length > 0 && (
            <Box>
              <Text size="sm" fw={600} mb={4}>
                快照历史构建（{plan.snapshotBuilds.length}）
              </Text>
              <ScrollArea h={92} type="auto">
                <Group gap={4}>
                  {plan.snapshotBuilds.map((b) => (
                    <Badge key={b.buildId} variant={b.buildId === "SNAPSHOT" ? "filled" : "outline"}>
                      {b.buildId} · {b.fileCount} 文件
                    </Badge>
                  ))}
                </Group>
              </ScrollArea>
            </Box>
          )}

          <Box style={{ flex: 1, minHeight: 120, overflow: "auto" }}>
            <Table striped highlightOnHover miw={720} stickyHeader>
              <Table.Thead>
                <Table.Tr>
                  <Table.Th w={40} />
                  <Table.Th>路径</Table.Th>
                  <Table.Th w={110}>类别</Table.Th>
                  <Table.Th w={110}>构建</Table.Th>
                  <Table.Th w={100}>大小</Table.Th>
                </Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {plan.files.map((file) => (
                  <Table.Tr key={file.path}>
                    <Table.Td>
                      <Checkbox
                        checked={selected.has(file.path)}
                        onChange={() => toggle(file.path)}
                        aria-label={`选择 ${file.path}`}
                      />
                    </Table.Td>
                    <Table.Td>
                      <Code style={{ wordBreak: "break-all" }}>{file.path}</Code>
                    </Table.Td>
                    <Table.Td>
                      <Badge size="sm" variant="light">
                        {CATEGORY_LABELS[file.category] ?? file.category}
                      </Badge>
                    </Table.Td>
                    <Table.Td>
                      <Text size="xs" c="dimmed">
                        {file.buildId ?? "—"}
                      </Text>
                    </Table.Td>
                    <Table.Td>{formatBytes(file.size)}</Table.Td>
                  </Table.Tr>
                ))}
              </Table.Tbody>
            </Table>
          </Box>

          <Group justify="space-between">
            <Text size="xs" c="dimmed">
              目录：<Code>{plan.directory}</Code>
            </Text>
            <Button
              color="red"
              leftSection={<IconTrash size={16} />}
              disabled={selectedFiles.length === 0}
              onClick={() => setConfirmOpen(true)}
            >
              移入回收站（{selectedFiles.length}）
            </Button>
          </Group>
        </>
      )}

      <DestructiveConfirmModal
        opened={confirmOpen}
        title="确认移入回收站"
        message={
          `将从 ${repo} 移出 ${selectedFiles.length} 个文件 / ${formatBytes(selectedBytes)}，` +
          `坐标 ${parsed?.groupId ?? ""}:${parsed?.artifactId ?? ""}:${parsed?.version ?? ""}` +
          `${parsed?.buildId ? `（构建 ${parsed.buildId}）` : ""}。\n\n` +
          `这些文件会进入资产回收站，可随时恢复；只有彻底清理或保留期到期才会真正释放磁盘。`
        }
        requirePhrase
        confirmLabel={DESTRUCTIVE_CONFIRM_WORD}
        pending={trashing}
        onConfirm={runTrash}
        onClose={() => setConfirmOpen(false)}
      />
    </Stack>
  );
}

/** 紧凑统计块。 */
function Stat({ label, value }: { label: string; value: string }) {
  return (
    <Box>
      <Text size="xs" c="dimmed">
        {label}
      </Text>
      <Text fw={600}>{value}</Text>
    </Box>
  );
}

/** 资产回收站列表（与存储清理回收站并列在同一个页面）。 */
export function AssetTrashPanel({
  entries,
  stats,
  loadFailed,
  onRestore,
  onPurge,
}: {
  entries: {
    id: number;
    repositoryName?: string;
    path: string;
    size: number;
    trashedAt: string;
    scope?: string;
    sha1?: string;
  }[];
  stats: { entries: number; bytes: number };
  loadFailed: boolean;
  onRestore: (ids: number[]) => void;
  onPurge: (ids: number[]) => void;
}) {
  const [selected, setSelected] = useState<Set<number>>(new Set());
  // 条目集合变化后清理失效的勾选：恢复/彻底清理成功会让这些 id 从列表中消失，
  // 而选中集合不会自动跟着变——留着会让按钮显示「恢复所选（2）」但列表已空，
  // 点下去是一批不存在的 id。这里按当前列表做交集，只保留仍存在的条目。
  useEffect(() => {
    setSelected((prev) => {
      if (prev.size === 0) return prev;
      const alive = new Set(entries.map((e) => e.id));
      const next = new Set<number>();
      for (const id of prev) if (alive.has(id)) next.add(id);
      // 内容未变时返回原引用，避免无谓的重渲染。
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

  if (loadFailed) {
    // 失败不清空列表：与页内其它 Tab 同一口径，网络抖动不该看起来像回收站空了。
    return (
      <Alert color="yellow" variant="light">
        加载失败，列表可能不是最新状态。
      </Alert>
    );
  }

  return (
    <Stack gap="sm" style={{ flex: 1, minHeight: 0 }}>
      <Group justify="space-between">
        <Text size="sm" c="dimmed">
          共 {stats.entries} 条 / {formatBytes(stats.bytes)}
        </Text>
        <Group gap="xs">
          <Button
            size="xs"
            variant="light"
            disabled={selected.size === 0}
            onClick={() => onRestore([...selected])}
          >
            恢复所选（{selected.size}）
          </Button>
          <Button
            size="xs"
            color="red"
            variant="light"
            disabled={selected.size === 0}
            onClick={() => onPurge([...selected])}
          >
            彻底清理（{selected.size}）
          </Button>
        </Group>
      </Group>
      <Box style={{ flex: 1, minHeight: 120, overflow: "auto" }}>
        <Table striped highlightOnHover miw={760} stickyHeader>
          <Table.Thead>
            <Table.Tr>
              <Table.Th w={40} />
              <Table.Th w={140}>仓库</Table.Th>
              <Table.Th>路径</Table.Th>
              <Table.Th w={100}>大小</Table.Th>
              <Table.Th w={150}>入回收站时间</Table.Th>
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {entries.length === 0 ? (
              <Table.Tr>
                <Table.Td colSpan={5}>
                  <Text size="sm" c="dimmed" ta="center" py="md">
                    回收站为空
                  </Text>
                </Table.Td>
              </Table.Tr>
            ) : (
              entries.map((entry) => (
                <Table.Tr key={entry.id}>
                  <Table.Td>
                    <Checkbox
                      checked={selected.has(entry.id)}
                      onChange={() => toggle(entry.id)}
                      aria-label={`选择 ${entry.path}`}
                    />
                  </Table.Td>
                  <Table.Td>
                    <Text size="sm">{entry.repositoryName || "—"}</Text>
                  </Table.Td>
                  <Table.Td>
                    <Code style={{ wordBreak: "break-all" }}>{entry.path}</Code>
                  </Table.Td>
                  <Table.Td>{formatBytes(entry.size)}</Table.Td>
                  <Table.Td>
                    <Text size="xs" c="dimmed">
                      {entry.trashedAt}
                    </Text>
                  </Table.Td>
                </Table.Tr>
              ))
            )}
          </Table.Tbody>
        </Table>
      </Box>
    </Stack>
  );
}
