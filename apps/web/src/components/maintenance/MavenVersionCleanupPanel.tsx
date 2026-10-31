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
  Button,
  Card,
  Checkbox,
  Code,
  Divider,
  Group,
  ScrollArea,
  Select,
  SimpleGrid,
  Stack,
  Table,
  Text,
  TextInput,
} from "@mantine/core";
import { notifications } from "@mantine/notifications";
import {
  IconAlertTriangle,
  IconChartPie,
  IconCheckbox,
  IconDatabase,
  IconFile,
  IconSearch,
  IconTrash,
} from "@tabler/icons-react";
import { KpiCard } from "./KpiCard";
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
  // Select 的值是 string|null（可清空），因此状态也用可空类型，
  // 避免"用空串表示未选"这种约定与组件库语义打架。
  const [repo, setRepo] = useState<string | null>(repositories[0]?.name ?? null);
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
    if (!parsed || !repo || trashing) return;
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
    <Stack gap="md" h="100%" style={{ minHeight: 0 }}>
      <Alert color="blue" variant="light" icon={<IconAlertTriangle size={16} />}>
        删除是<strong>可恢复</strong>的：文件会进入「回收站」，blob 保留到你彻底清理或保留期到期。
      </Alert>

      <Card withBorder radius="md" padding="md">
        <Stack gap="sm">
          <SimpleGrid cols={{ base: 1, md: 3 }} spacing="sm">
            <Select
              label="仓库"
              placeholder="选择仓库"
              data={repositories.map((r) => ({ value: r.name, label: r.name }))}
              value={repo}
              onChange={setRepo}
              allowDeselect={false}
              checkIconPosition="right"
            />
            <TextInput
              label="坐标"
              placeholder="com.example:demo:1.17-SNAPSHOT"
              description="group:artifact:version，SNAPSHOT 可在第 4 段指定单次构建"
              value={coordinate}
              onChange={(e) => setCoordinate(e.currentTarget.value)}
            />
            <Stack gap={4} justify="flex-end">
              <Button
                leftSection={<IconSearch size={16} />}
                onClick={loadPlan}
                loading={loading}
                disabled={!repo || !parsed}
                fullWidth
              >
                预览清单
              </Button>
            </Stack>
          </SimpleGrid>
        </Stack>
      </Card>

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
          <SimpleGrid cols={{ base: 2, sm: 4 }} spacing="md">
            <KpiCard icon={<IconFile size={20} />} label="文件总数" value={String(plan.totalFiles)} />
            <KpiCard icon={<IconDatabase size={20} />} label="总大小" value={formatBytes(plan.totalBytes)} />
            <KpiCard icon={<IconCheckbox size={20} />} label="已选文件" value={String(selectedFiles.length)} color="teal" />
            <KpiCard icon={<IconChartPie size={20} />} label="已选大小" value={formatBytes(selectedBytes)} color="teal" />
          </SimpleGrid>

          {(plan.categories?.length || plan.snapshotBuilds?.length) && (
            <Card withBorder radius="md" padding="md">
              <Stack gap="sm">
                {plan.categories && plan.categories.length > 0 && (
                  <Group gap="xs">
                    <Text size="sm" c="dimmed">
                      按类别：
                    </Text>
                    {plan.categories.map((c) => (
                      <Badge key={c.category} variant="light">
                        {CATEGORY_LABELS[c.category as MavenVersionFile["category"]] ?? c.category}{" "}
                        {c.fileCount}
                      </Badge>
                    ))}
                  </Group>
                )}
                {plan.snapshotBuilds && plan.snapshotBuilds.length > 0 && (
                  <Stack gap={4}>
                    <Text size="sm" c="dimmed">
                      快照历史构建（{plan.snapshotBuilds.length}）
                    </Text>
                    <ScrollArea h={72} type="auto">
                      <Group gap="xs">
                        {plan.snapshotBuilds.map((b) => (
                          <Badge
                            key={b.buildId}
                            variant={b.buildId === "SNAPSHOT" ? "filled" : "outline"}
                          >
                            {b.buildId} · {b.fileCount} 文件
                          </Badge>
                        ))}
                      </Group>
                    </ScrollArea>
                  </Stack>
                )}
              </Stack>
            </Card>
          )}

          <Card withBorder radius="md" padding={0} style={{ flex: 1, minHeight: 0, display: "flex", flexDirection: "column" }}>
            <Group justify="space-between" p="md" wrap="wrap" gap="sm">
              <Text size="sm" c="dimmed">
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
            <Divider />
            <ScrollArea style={{ flex: 1, minHeight: 0 }}>
              <Table striped highlightOnHover miw={720} stickyHeader verticalSpacing={6}>
                <Table.Thead>
                  <Table.Tr>
                    <Table.Th w={40} />
                    <Table.Th>路径</Table.Th>
                    <Table.Th w={110}>类别</Table.Th>
                    <Table.Th w={140}>构建</Table.Th>
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
                        <Code>{file.path}</Code>
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
            </ScrollArea>
          </Card>
        </>
      )}

      <DestructiveConfirmModal
        opened={confirmOpen}
        title="确认移入回收站"
        message={
          `将从 ${repo ?? ""} 移出 ${selectedFiles.length} 个文件 / ${formatBytes(selectedBytes)}，` +
          `坐标 ${parsed?.groupId ?? ""}:${parsed?.artifactId ?? ""}:${parsed?.version ?? ""}` +
          `${parsed?.buildId ? `（构建 ${parsed.buildId}）` : ""}。\n\n` +
          `这些文件会进入回收站，可随时恢复；只有彻底清理或保留期到期才会真正释放磁盘。`
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

