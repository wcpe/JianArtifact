// 包内容 vs 当前系统的差异视图。
//
// # 要回答的问题
//
// 恢复是整份覆盖。用户在这一刻最想知道的是"会改什么"：
// 哪些会新增、哪些会消失、哪些会被替换。之前只能看到"包内有什么"，
// 看不到"和现在差在哪"，等于还是在赌。
//
// # 性能是这个组件的核心约束
//
// 真实实例可达几十万制品。因此：
//   - 打开时**只请求摘要**（服务端只跑聚合查询，返四类计数）；
//   - 用户点开某一类才拉明细，且**分页**（单页 50，上限 200）；
//   - 明细里明确显示"第几页 / 共多少条"，绝不假装已经看全。
//
// # 诚实性
//
// 只比元数据（路径/大小/哈希），不比对文件内容。这句话必须出现在界面上——
// 否则"对比无差异"会被误读成"字节完全一致"，而那是深度校验才回答的问题。
import { Alert, Badge, Box, Button, Group, Loader, Stack, Table, Text } from "@mantine/core";
import { IconChevronDown, IconChevronRight, IconInfoCircle } from "@tabler/icons-react";
import { useCallback, useEffect, useState } from "react";
import { useTranslation } from "react-i18next";

import { getBackupDiff } from "../../api/endpoints";
import type { BackupDiff, BackupDiffDetailItem, DiffCounts } from "../../api/types";

type Entity = "repositories" | "assets" | "users";
type DiffKind = "onlyInPackage" | "onlyLocal" | "changed" | "same";

const ENTITIES: { key: Entity; labelKey: string }[] = [
  { key: "assets", labelKey: "backups.diffEntityAssets" },
  { key: "repositories", labelKey: "backups.diffEntityRepos" },
  { key: "users", labelKey: "backups.diffEntityUsers" },
];

const KINDS: { key: DiffKind; labelKey: string; hintKey: string; color: string }[] = [
  {
    key: "onlyInPackage",
    labelKey: "backups.diffKindOnlyInPackage",
    hintKey: "backups.diffKindOnlyInPackageHint",
    color: "teal",
  },
  {
    key: "onlyLocal",
    labelKey: "backups.diffKindOnlyLocal",
    hintKey: "backups.diffKindOnlyLocalHint",
    color: "red",
  },
  {
    key: "changed",
    labelKey: "backups.diffKindChanged",
    hintKey: "backups.diffKindChangedHint",
    color: "orange",
  },
  {
    key: "same",
    labelKey: "backups.diffKindSame",
    hintKey: "backups.diffKindSameHint",
    color: "gray",
  },
];

/** 页面大小：50 足够看清一段，且让"还有多少"这件事尽快显现。 */
const PAGE_SIZE = 50;

export function BackupDiffView({ packageId }: { packageId: string }) {
  const { t } = useTranslation();
  const [summary, setSummary] = useState<BackupDiff | null>(null);
  const [summaryError, setSummaryError] = useState<string | null>(null);
  const [loadingSummary, setLoadingSummary] = useState(false);

  // 展开的类目：null = 全部收起。
  const [expanded, setExpanded] = useState<{ entity: Entity; kind: DiffKind } | null>(null);
  const [detail, setDetail] = useState<BackupDiffDetailItem[]>([]);
  const [detailTotal, setDetailTotal] = useState(0);
  const [detailPage, setDetailPage] = useState(1);
  const [loadingDetail, setLoadingDetail] = useState(false);
  const [detailError, setDetailError] = useState<string | null>(null);

  // 摘要：只在 packageId 变化时拉一次（聚合查询，代价可控）。
  useEffect(() => {
    let cancelled = false;
    setLoadingSummary(true);
    setSummaryError(null);
    setSummary(null);
    setExpanded(null);
    getBackupDiff(packageId, { scope: "summary" })
      .then((res) => {
        if (!cancelled) setSummary(res);
      })
      .catch((err: unknown) => {
        if (!cancelled) setSummaryError(err instanceof Error ? err.message : String(err));
      })
      .finally(() => {
        if (!cancelled) setLoadingSummary(false);
      });
    return () => {
      cancelled = true;
    };
  }, [packageId]);

  const loadDetail = useCallback(
    async (entity: Entity, kind: DiffKind, page: number) => {
      setLoadingDetail(true);
      setDetailError(null);
      try {
        const res = await getBackupDiff(packageId, {
          scope: entity,
          kind,
          page,
          pageSize: PAGE_SIZE,
        });
        // 服务端保证非 summary 模式下 detail 存在；缺了就当空页处理，
        // 而不是崩掉整个弹窗。
        setDetail(res.detail?.items ?? []);
        setDetailTotal(res.detail?.total ?? 0);
        setDetailPage(page);
      } catch (err: unknown) {
        setDetailError(err instanceof Error ? err.message : String(err));
        setDetail([]);
        setDetailTotal(0);
      } finally {
        setLoadingDetail(false);
      }
    },
    [packageId],
  );

  const toggle = (entity: Entity, kind: DiffKind) => {
    const isSame = expanded?.entity === entity && expanded?.kind === kind;
    if (isSame) {
      setExpanded(null);
      return;
    }
    setExpanded({ entity, kind });
    void loadDetail(entity, kind, 1);
  };

  const countsOf = (entity: Entity): DiffCounts | null => {
    if (!summary) return null;
    if (entity === "assets") return summary.assets ?? null;
    if (entity === "repositories") return summary.repositories ?? null;
    return summary.users ?? null;
  };

  const totalPages = Math.max(1, Math.ceil(detailTotal / PAGE_SIZE));

  return (
    <Stack gap="xs" data-testid="diff-view">
      <Text size="sm" fw={600}>
        {t("backups.diffTitle")}
      </Text>
      <Text size="xs" c="dimmed">
        {t("backups.diffSubtitle")}
      </Text>

      {/* 诚实性声明：只比元数据。缺了这句，"无差异"会被误读成"字节一致"。 */}
      <Alert color="blue" variant="light" icon={<IconInfoCircle size={14} />} py={6}>
        <Text size="xs">{t("backups.diffMetadataOnly")}</Text>
      </Alert>

      {loadingSummary ? (
        <Group gap="xs">
          <Loader size="xs" />
          <Text size="xs" c="dimmed">
            {t("backups.diffLoading")}
          </Text>
        </Group>
      ) : null}

      {summaryError ? (
        <Alert color="red" variant="light" py={6}>
          <Text size="xs">{t("backups.diffFailed", { message: summaryError })}</Text>
        </Alert>
      ) : null}

      {summary
        ? ENTITIES.map(({ key: entity, labelKey }) => {
            const counts = countsOf(entity);
            if (!counts) return null;
            const total = counts.onlyInPackage + counts.onlyLocal + counts.changed + counts.same;
            if (total === 0) return null;
            return (
              <Box key={entity}>
                <Text size="xs" fw={600} mb={4}>
                  {t(labelKey)}
                </Text>
                <Stack gap={4}>
                  {KINDS.map(({ key: kind, labelKey: kindLabel, hintKey, color }) => {
                    const n = counts[kind];
                    // 计数为 0 的类不占位置：屏幕有限，先给"会发生什么"。
                    if (n === 0) return null;
                    const isOpen = expanded?.entity === entity && expanded?.kind === kind;
                    return (
                      <Box key={kind}>
                        <Group gap={6} wrap="nowrap">
                          <Button
                            size="compact-xs"
                            variant="subtle"
                            leftSection={
                              isOpen ? (
                                <IconChevronDown size={12} />
                              ) : (
                                <IconChevronRight size={12} />
                              )
                            }
                            onClick={() => toggle(entity, kind)}
                            title={t(hintKey)}
                            data-testid={`diff-toggle-${entity}-${kind}`}
                          >
                            <Group gap={6} wrap="nowrap">
                              <Badge size="xs" color={color} variant="light">
                                {n}
                              </Badge>
                              <Text size="xs">{t(kindLabel)}</Text>
                            </Group>
                          </Button>
                        </Group>

                        {isOpen ? (
                          <Box mt={4} pl="md">
                            {loadingDetail ? (
                              <Group gap="xs">
                                <Loader size="xs" />
                                <Text size="xs" c="dimmed">
                                  {t("backups.diffDetailLoading")}
                                </Text>
                              </Group>
                            ) : detailError ? (
                              <Text size="xs" c="red">
                                {detailError}
                              </Text>
                            ) : detail.length === 0 ? (
                              <Text size="xs" c="dimmed">
                                {t("backups.diffDetailEmpty")}
                              </Text>
                            ) : (
                              <>
                                <Table striped verticalSpacing={2} fz="xs">
                                  <Table.Tbody>
                                    {detail.map((it) => (
                                      <Table.Tr key={it.key}>
                                        <Table.Td style={{ wordBreak: "break-all" }}>
                                          {it.key}
                                        </Table.Td>
                                        <Table.Td style={{ whiteSpace: "nowrap" }} c="dimmed">
                                          {it.detail}
                                        </Table.Td>
                                      </Table.Tr>
                                    ))}
                                  </Table.Tbody>
                                </Table>
                                {/* 分页与"还有多少"：绝不让用户以为已经看全。 */}
                                <Group justify="space-between" mt={4}>
                                  <Text size="xs" c="dimmed">
                                    {t("backups.diffDetailPage", {
                                      page: detailPage,
                                      pages: totalPages,
                                      total: detailTotal,
                                    })}
                                  </Text>
                                  <Group gap={4}>
                                    <Button
                                      size="compact-xs"
                                      variant="default"
                                      disabled={detailPage <= 1 || loadingDetail}
                                      onClick={() =>
                                        expanded &&
                                        void loadDetail(
                                          expanded.entity,
                                          expanded.kind,
                                          detailPage - 1,
                                        )
                                      }
                                    >
                                      {t("backups.diffPrev")}
                                    </Button>
                                    <Button
                                      size="compact-xs"
                                      variant="default"
                                      disabled={detailPage >= totalPages || loadingDetail}
                                      onClick={() =>
                                        expanded &&
                                        void loadDetail(
                                          expanded.entity,
                                          expanded.kind,
                                          detailPage + 1,
                                        )
                                      }
                                    >
                                      {t("backups.diffNext")}
                                    </Button>
                                  </Group>
                                </Group>
                              </>
                            )}
                          </Box>
                        ) : null}
                      </Box>
                    );
                  })}
                </Stack>
              </Box>
            );
          })
        : null}
    </Stack>
  );
}
