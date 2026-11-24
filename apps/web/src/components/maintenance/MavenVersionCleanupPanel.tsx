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
import { OpsKpiBand } from "../ops/OpsKit";
import { useCallback, useEffect, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import {
  planMavenVersion,
  trashMavenVersion,
  type MavenVersionFile,
  type MavenVersionPlan,
} from "../../api/endpoints";
import { DestructiveConfirmModal } from "./DestructiveConfirmModal";
import { formatBytes } from "../../lib/format";

/**
 * 类别 → 本地化标签。
 *
 * 不用模块级常量表：标签要跟随界面语言，常量表在模块加载时就定死了，
 * 切换语言后不会重算。顺序仍与后端 Categories 的固定顺序一致。
 */
function categoryLabel(t: (key: string) => string, category: MavenVersionFile["category"]): string {
  switch (category) {
    case "primary":
      return t("maintenance.verCategoryPrimary");
    case "classifier":
      return t("maintenance.verCategoryClassifier");
    case "checksum":
      return t("maintenance.verCategoryChecksum");
    case "metadata":
      return t("maintenance.verCategoryMetadata");
    default:
      return t("maintenance.verCategoryOther");
  }
}

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

export function MavenVersionCleanupPanel({
  repositories,
  onTrashed,
}: MavenVersionCleanupPanelProps) {
  const { t } = useTranslation();
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

  // 切换仓库必须作废上一仓库的预览清单与勾选：文件清单是按旧仓库
  // 生成的，直接复用会把未经预览的同坐标制品提交给新仓库。
  useEffect(() => {
    setPlan(null);
    setSelected(new Set());
    setError(null);
  }, [repo]);

  const parsed = useMemo(() => parseCoordinate(coordinate), [coordinate]);

  const loadPlan = useCallback(() => {
    if (!repo || !parsed) {
      setError(t("maintenance.verErrBadCoordinate"));
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
        setError(err instanceof Error ? err.message : t("maintenance.verErrPreviewFailed"));
      })
      .finally(() => setLoading(false));
  }, [t, repo, parsed]);

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
      overrideReason: t("maintenance.verTrashReason", {
        coordinate: `${parsed.groupId}:${parsed.artifactId}:${parsed.version}`,
      }),
    })
      .then((res) => {
        notifications.show({
          color: "green",
          message: t("maintenance.verTrashed", {
            files: res.trashed,
            bytes: formatBytes(res.bytes),
            batches: res.batches,
          }),
        });
        setConfirmOpen(false);
        // 删完重新预览：让用户立刻看到该版本还剩什么（可能是部分删除或已空）。
        loadPlan();
        onTrashed();
      })
      .catch((err) =>
        notifications.show({
          color: "red",
          message: err instanceof Error ? err.message : t("maintenance.verErrDeleteFailed"),
        }),
      )
      .finally(() => setTrashing(false));
  };

  return (
    <Stack gap="md" h="100%" style={{ minHeight: 0 }}>
      <Alert color="blue" variant="light" icon={<IconAlertTriangle size={16} />}>
        {t("maintenance.verReversibleNotice")}
      </Alert>

      <Card withBorder radius="md" padding="md">
        <Stack gap="sm">
          <SimpleGrid cols={{ base: 1, md: 3 }} spacing="sm">
            <Select
              label={t("maintenance.verRepo")}
              placeholder={t("maintenance.verRepoPlaceholder")}
              data={repositories.map((r) => ({ value: r.name, label: r.name }))}
              value={repo}
              onChange={setRepo}
              allowDeselect={false}
              checkIconPosition="right"
            />
            <TextInput
              label={t("maintenance.verCoordinate")}
              placeholder="com.example:demo:1.17-SNAPSHOT"
              description={t("maintenance.verCoordinateDescription")}
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
                {t("maintenance.verPreview")}
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
          {t("maintenance.verNotFound", { repo })}
        </Alert>
      )}

      {plan && plan.exists && (
        <>
          <OpsKpiBand
            label={t("maintenance.versionPlanTitle")}
            cols={{ base: 2, sm: 4 }}
            items={[
              {
                label: t("maintenance.planTotalFiles"),
                value: String(plan.totalFiles),
                icon: <IconFile size={20} />,
                tone: "blue",
                testId: "plan-total-files",
              },
              {
                label: t("maintenance.planTotalBytes"),
                value: formatBytes(plan.totalBytes),
                icon: <IconDatabase size={20} />,
                tone: "blue",
                testId: "plan-total-bytes",
              },
              {
                label: t("maintenance.planSelectedFiles"),
                value: String(selectedFiles.length),
                icon: <IconCheckbox size={20} />,
                tone: "teal",
                testId: "plan-selected-files",
              },
              {
                label: t("maintenance.planSelectedBytes"),
                value: formatBytes(selectedBytes),
                icon: <IconChartPie size={20} />,
                tone: "teal",
                testId: "plan-selected-bytes",
              },
            ]}
          />

          {(plan.categories?.length || plan.snapshotBuilds?.length) && (
            <Card withBorder radius="md" padding="md">
              <Stack gap="sm">
                {plan.categories && plan.categories.length > 0 && (
                  <Group gap="xs">
                    <Text size="sm" c="dimmed">
                      {t("maintenance.verByCategory")}
                    </Text>
                    {plan.categories.map((c) => (
                      <Badge key={c.category} variant="light">
                        {categoryLabel(t, c.category as MavenVersionFile["category"])} {c.fileCount}
                      </Badge>
                    ))}
                  </Group>
                )}
                {plan.snapshotBuilds && plan.snapshotBuilds.length > 0 && (
                  <Stack gap={4}>
                    <Text size="sm" c="dimmed">
                      {t("maintenance.verSnapshotBuilds", { count: plan.snapshotBuilds.length })}
                    </Text>
                    <ScrollArea h={72} type="auto">
                      <Group gap="xs">
                        {plan.snapshotBuilds.map((b) => (
                          <Badge
                            key={b.buildId}
                            variant={b.buildId === "SNAPSHOT" ? "filled" : "outline"}
                          >
                            {t("maintenance.verSnapshotBuild", {
                              buildId: b.buildId,
                              count: b.fileCount,
                            })}
                          </Badge>
                        ))}
                      </Group>
                    </ScrollArea>
                  </Stack>
                )}
              </Stack>
            </Card>
          )}

          <Card
            withBorder
            radius="md"
            padding={0}
            style={{ flex: 1, minHeight: 0, display: "flex", flexDirection: "column" }}
          >
            <Group justify="space-between" p="md" wrap="wrap" gap="sm">
              <Text size="sm" c="dimmed">
                {t("maintenance.verDirectory")} <Code>{plan.directory}</Code>
              </Text>
              <Button
                color="red"
                leftSection={<IconTrash size={16} />}
                disabled={selectedFiles.length === 0}
                onClick={() => setConfirmOpen(true)}
              >
                {t("maintenance.verTrashSelected", { count: selectedFiles.length })}
              </Button>
            </Group>
            <Divider />
            <ScrollArea style={{ flex: 1, minHeight: 0 }}>
              <Table striped highlightOnHover miw={720} stickyHeader verticalSpacing={6}>
                <Table.Thead>
                  <Table.Tr>
                    <Table.Th w={40} />
                    <Table.Th>{t("maintenance.verColPath")}</Table.Th>
                    <Table.Th w={110}>{t("maintenance.verColCategory")}</Table.Th>
                    <Table.Th w={140}>{t("maintenance.verColBuild")}</Table.Th>
                    <Table.Th w={100}>{t("maintenance.verColSize")}</Table.Th>
                  </Table.Tr>
                </Table.Thead>
                <Table.Tbody>
                  {plan.files.map((file) => (
                    <Table.Tr key={file.path}>
                      <Table.Td>
                        <Checkbox
                          checked={selected.has(file.path)}
                          onChange={() => toggle(file.path)}
                          aria-label={t("maintenance.verSelectRow", { path: file.path })}
                        />
                      </Table.Td>
                      <Table.Td>
                        <Code>{file.path}</Code>
                      </Table.Td>
                      <Table.Td>
                        <Badge size="sm" variant="light">
                          {categoryLabel(t, file.category)}
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
        title={t("maintenance.verConfirmTitle")}
        message={t("maintenance.verConfirmMessage", {
          repo: repo ?? "",
          files: selectedFiles.length,
          bytes: formatBytes(selectedBytes),
          coordinate: `${parsed?.groupId ?? ""}:${parsed?.artifactId ?? ""}:${parsed?.version ?? ""}`,
          buildPart: parsed?.buildId
            ? t("maintenance.verConfirmBuildPart", { buildId: parsed.buildId })
            : "",
        })}
        requirePhrase
        pending={trashing}
        onConfirm={runTrash}
        onClose={() => setConfirmOpen(false)}
      />
    </Stack>
  );
}
