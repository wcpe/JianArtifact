// Maven 包名批量清理面板：「按包名搜版本 → 勾选 → 批量移入回收站」。
//
// 为什么需要它（而不是继续用逐版本清理面板）：
// 逐版本面板要求用户已经知道确切的 `group:artifact:version`。实际维护时想的是
// 「把 shopcart 下所有 1.x 清掉」「把带 SNAPSHOT 的版本都找出来」这类**包级意图**，
// 逐个版本输入坐标要几十次，且每次都得自己记哪些删过了。
//
// 四类筛选对应四类真实维护场景：坐标通配 / 含 SNAPSHOT / 时间范围 / 体积区间。
//
// **只用 Mantine 组件，不写内联样式**：外壳一律用 Stack/Group/Card/SimpleGrid 布局，
// 尺寸走 Mantine 的 size/spacing 体系；此前用原生 <select> + style={{}} 的做法
// 会绕开主题（暗色模式下配色、聚焦环、禁用态都不一致），也拿不到组件库的键盘可达性。
import {
  Alert,
  Badge,
  Button,
  Card,
  Checkbox,
  Code,
  Divider,
  Group,
  NumberInput,
  Select,
  SimpleGrid,
  Stack,
  Table,
  Text,
  TextInput,
  ScrollArea,
} from "@mantine/core";
import { notifications } from "@mantine/notifications";
import { IconAlertTriangle, IconSearch, IconTrash } from "@tabler/icons-react";
import { useCallback, useEffect, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import {
  searchMavenPackageVersions,
  trashMavenVersionsBatch,
  type MavenPackageVersion,
} from "../../api/endpoints";
import { DestructiveConfirmModal } from "./DestructiveConfirmModal";
import { formatBytes } from "../../lib/format";

export interface MavenPackageBatchPanelProps {
  /** 可选的 Maven hosted 仓库。 */
  repositories: { name: string }[];
  /** 回收站有变化后通知父级刷新。 */
  onTrashed: () => void;
}

/** 版本条目的唯一键（坐标即唯一）。 */
function versionKey(v: MavenPackageVersion): string {
  const c = v.coordinate;
  return `${c.groupId}:${c.artifactId}:${c.version}`;
}

export function MavenPackageBatchPanel({ repositories, onTrashed }: MavenPackageBatchPanelProps) {
  const { t } = useTranslation();
  // 仓库列表异步加载：初始值不能只取一次，否则首次挂载时列表为空、repo 永远停在空串。
  const [repo, setRepo] = useState<string | null>(repositories[0]?.name ?? null);
  useEffect(() => {
    const first = repositories[0];
    if (!first) return;
    setRepo((current) =>
      current && repositories.some((r) => r.name === current) ? current : first.name,
    );
  }, [repositories]);

  const [groupId, setGroupId] = useState("");
  const [artifactId, setArtifactId] = useState("");
  const [versionPattern, setVersionPattern] = useState("");
  const [snapshotOnly, setSnapshotOnly] = useState(false);
  const [updatedBefore, setUpdatedBefore] = useState("");
  const [minBytes, setMinBytes] = useState<number | string>("");
  const [maxBytes, setMaxBytes] = useState<number | string>("");

  const [versions, setVersions] = useState<MavenPackageVersion[]>([]);
  const [truncated, setTruncated] = useState(false);
  const [totalVersions, setTotalVersions] = useState(0);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [searched, setSearched] = useState(false);

  // 勾选以坐标字符串为键：版本对象每次搜索后是新引用，按对象做键会失效。
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [confirmOpen, setConfirmOpen] = useState(false);
  const [trashing, setTrashing] = useState(false);

  const search = useCallback(() => {
    if (!repo) {
      setError(t("maintenance.pkgErrNoRepo"));
      return;
    }
    if (!groupId.trim() && !artifactId.trim()) {
      setError(t("maintenance.pkgErrNoCoordinate"));
      return;
    }
    setLoading(true);
    setError(null);
    searchMavenPackageVersions(repo, {
      groupId: groupId.trim(),
      artifactId: artifactId.trim(),
      version: versionPattern.trim() || undefined,
      snapshotOnly,
      updatedBefore: updatedBefore.trim() || undefined,
      minBytes: typeof minBytes === "number" ? minBytes : undefined,
      maxBytes: typeof maxBytes === "number" ? maxBytes : undefined,
      limit: 500,
    })
      .then((res) => {
        setVersions(res.versions ?? []);
        setTruncated(res.truncated);
        setTotalVersions(res.totalVersions);
        setSearched(true);
        // 默认全不选：批量删除的危险来自"手滑全选"，让用户主动勾更安全。
        setSelected(new Set());
      })
      .catch((err) => {
        setVersions([]);
        setSearched(true);
        setError(err instanceof Error ? err.message : t("maintenance.pkgErrSearchFailed"));
      })
      .finally(() => setLoading(false));
  }, [
    t,
    repo,
    groupId,
    artifactId,
    versionPattern,
    snapshotOnly,
    updatedBefore,
    minBytes,
    maxBytes,
  ]);

  const toggle = (key: string) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });
  };

  const selectedVersions = useMemo(
    () => versions.filter((v) => selected.has(versionKey(v))),
    [versions, selected],
  );
  const selectedBytes = selectedVersions.reduce((sum, v) => sum + v.bytes, 0);
  const selectedFiles = selectedVersions.reduce((sum, v) => sum + v.fileCount, 0);

  const toggleAll = () => {
    if (selected.size === versions.length) setSelected(new Set());
    else setSelected(new Set(versions.map(versionKey)));
  };

  const runTrash = () => {
    if (!repo || selectedVersions.length === 0 || trashing) return;
    setTrashing(true);
    trashMavenVersionsBatch(repo, {
      versions: selectedVersions.map((v) => ({
        groupId: v.coordinate.groupId,
        artifactId: v.coordinate.artifactId,
        version: v.coordinate.version,
      })),
      overrideReason: t("maintenance.pkgTrashReason", { count: selectedVersions.length }),
    })
      .then((res) => {
        // 部分成功是正常结果：必须把失败明细摊开，否则用户以为全删了。
        if (res.failures && res.failures.length > 0) {
          notifications.show({
            color: "yellow",
            title: t("maintenance.pkgPartialTitle", {
              ok: res.trashedVersions,
              failed: res.failures.length,
            }),
            message: res.failures
              .slice(0, 5)
              .map(
                (f) =>
                  `${f.coordinate.groupId}:${f.coordinate.artifactId}:${f.coordinate.version} — ${f.reason}`,
              )
              .join("\n"),
          });
        } else {
          notifications.show({
            color: "green",
            message: t("maintenance.pkgTrashed", {
              versions: res.trashedVersions,
              files: res.trashedFiles,
              bytes: formatBytes(res.freedBytes),
            }),
          });
        }
        setConfirmOpen(false);
        search();
        onTrashed();
      })
      .catch((err) =>
        notifications.show({
          color: "red",
          message: err instanceof Error ? err.message : t("maintenance.pkgErrTrashFailed"),
        }),
      )
      .finally(() => setTrashing(false));
  };

  return (
    <Stack gap="md" h="100%" style={{ minHeight: 0 }}>
      <Alert color="blue" variant="light" icon={<IconAlertTriangle size={16} />}>
        {t("maintenance.pkgReversibleNotice")}
      </Alert>

      <Card withBorder radius="md" padding="md">
        <Stack gap="sm">
          <SimpleGrid cols={{ base: 1, sm: 2, lg: 4 }} spacing="sm">
            <Select
              label={t("maintenance.pkgRepo")}
              placeholder={t("maintenance.pkgRepoPlaceholder")}
              data={repositories.map((r) => ({ value: r.name, label: r.name }))}
              value={repo}
              onChange={setRepo}
              allowDeselect={false}
              checkIconPosition="right"
            />
            <TextInput
              label="groupId"
              placeholder={t("maintenance.pkgGroupIdPlaceholder")}
              value={groupId}
              onChange={(e) => setGroupId(e.currentTarget.value)}
            />
            <TextInput
              label="artifactId"
              placeholder={t("maintenance.pkgArtifactIdPlaceholder")}
              value={artifactId}
              onChange={(e) => setArtifactId(e.currentTarget.value)}
            />
            <TextInput
              label={t("maintenance.pkgVersion")}
              placeholder={t("maintenance.pkgVersionPlaceholder")}
              value={versionPattern}
              onChange={(e) => setVersionPattern(e.currentTarget.value)}
            />
          </SimpleGrid>

          <SimpleGrid cols={{ base: 1, sm: 2, lg: 4 }} spacing="sm">
            <TextInput
              label={t("maintenance.pkgUpdatedBefore")}
              placeholder={t("maintenance.pkgUpdatedBeforePlaceholder")}
              value={updatedBefore}
              onChange={(e) => setUpdatedBefore(e.currentTarget.value)}
            />
            <NumberInput
              label={t("maintenance.pkgMinBytes")}
              placeholder={t("maintenance.pkgBytesUnlimited")}
              value={minBytes}
              onChange={setMinBytes}
              min={0}
              thousandSeparator=","
            />
            <NumberInput
              label={t("maintenance.pkgMaxBytes")}
              placeholder={t("maintenance.pkgBytesUnlimited")}
              value={maxBytes}
              onChange={setMaxBytes}
              min={0}
              thousandSeparator=","
            />
            <Stack gap={4} justify="flex-end">
              <Checkbox
                label={t("maintenance.pkgSnapshotOnly")}
                checked={snapshotOnly}
                onChange={(e) => setSnapshotOnly(e.currentTarget.checked)}
              />
            </Stack>
          </SimpleGrid>

          <Group justify="flex-end">
            <Button leftSection={<IconSearch size={16} />} onClick={search} loading={loading}>
              {t("maintenance.pkgSearch")}
            </Button>
          </Group>
        </Stack>
      </Card>

      {error && (
        <Alert color="red" variant="light">
          {error}
        </Alert>
      )}

      {truncated && (
        <Alert color="yellow" variant="light">
          {t("maintenance.pkgTruncated", { total: totalVersions, shown: versions.length })}
        </Alert>
      )}

      {searched && versions.length === 0 && !error && (
        <Alert color="gray" variant="light">
          {t("maintenance.pkgEmpty")}
        </Alert>
      )}

      {versions.length > 0 && (
        <Card
          withBorder
          radius="md"
          padding={0}
          style={{ flex: 1, minHeight: 0, display: "flex", flexDirection: "column" }}
        >
          <Group justify="space-between" p="md" wrap="wrap" gap="sm">
            <Group gap="sm">
              <Button size="xs" variant="subtle" onClick={toggleAll}>
                {selected.size === versions.length
                  ? t("maintenance.pkgDeselectAll")
                  : t("maintenance.pkgSelectAll", { count: versions.length })}
              </Button>
              <Text size="sm" c="dimmed">
                {t("maintenance.pkgSelection", {
                  versions: selectedVersions.length,
                  files: selectedFiles,
                  bytes: formatBytes(selectedBytes),
                })}
              </Text>
            </Group>
            <Button
              color="red"
              leftSection={<IconTrash size={16} />}
              disabled={selectedVersions.length === 0}
              onClick={() => setConfirmOpen(true)}
            >
              {t("maintenance.pkgTrashSelected", { count: selectedVersions.length })}
            </Button>
          </Group>
          <Divider />
          <ScrollArea style={{ flex: 1, minHeight: 0 }}>
            <Table striped highlightOnHover miw={860} stickyHeader verticalSpacing={6}>
              <Table.Thead>
                <Table.Tr>
                  <Table.Th w={40} />
                  <Table.Th>{t("maintenance.pkgColVersion")}</Table.Th>
                  <Table.Th w={90}>{t("maintenance.pkgColFiles")}</Table.Th>
                  <Table.Th w={100}>{t("maintenance.pkgColSize")}</Table.Th>
                  <Table.Th w={150}>{t("maintenance.pkgColUpdated")}</Table.Th>
                  <Table.Th w={130}>{t("maintenance.pkgColFlags")}</Table.Th>
                </Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {versions.map((v) => {
                  const key = versionKey(v);
                  return (
                    <Table.Tr key={key}>
                      <Table.Td>
                        <Checkbox
                          checked={selected.has(key)}
                          onChange={() => toggle(key)}
                          aria-label={t("maintenance.pkgSelectRow", { key })}
                        />
                      </Table.Td>
                      <Table.Td>
                        <Code>{key}</Code>
                      </Table.Td>
                      <Table.Td>{v.fileCount}</Table.Td>
                      <Table.Td>{formatBytes(v.bytes)}</Table.Td>
                      <Table.Td>
                        <Text size="xs" c="dimmed">
                          {v.updatedAt}
                        </Text>
                      </Table.Td>
                      <Table.Td>
                        <Group gap={4} wrap="nowrap">
                          {v.suspect && (
                            <Badge size="sm" color="red" variant="light">
                              {t("maintenance.pkgFlagGhostSnapshot")}
                            </Badge>
                          )}
                          {!v.suspect && v.literalSnapshotFiles > 0 && (
                            <Badge size="sm" color="yellow" variant="light">
                              {t("maintenance.pkgFlagLiteralSnapshot")}
                            </Badge>
                          )}
                          {v.timestampBuilds > 0 && (
                            <Badge size="sm" variant="outline">
                              {t("maintenance.pkgTimestampBuilds", { count: v.timestampBuilds })}
                            </Badge>
                          )}
                        </Group>
                      </Table.Td>
                    </Table.Tr>
                  );
                })}
              </Table.Tbody>
            </Table>
          </ScrollArea>
        </Card>
      )}

      <DestructiveConfirmModal
        opened={confirmOpen}
        title={t("maintenance.pkgConfirmTitle")}
        message={t("maintenance.pkgConfirmMessage", {
          repo: repo ?? "",
          versions: selectedVersions.length,
          files: selectedFiles,
          bytes: formatBytes(selectedBytes),
        })}
        requirePhrase
        pending={trashing}
        onConfirm={runTrash}
        onClose={() => setConfirmOpen(false)}
      />
    </Stack>
  );
}
