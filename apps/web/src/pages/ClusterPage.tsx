// FR-86/88/90: 集群页——「集群」tab（同步状态 + 立即同步按钮；对端配置与自动同步
// 开关已迁至设置页「集群」tab）与「同步历史」tab（复制记录与进度可视化，仅记录有
// 变更/失败的事件）。仅管理员。
// 数据来自非契约端点 GET /api/v1/cluster、POST /api/v1/cluster/sync-now、GET /api/v1/cluster/sync-logs（复制引擎见 FR-83~85）。
import {
  Anchor,
  Badge,
  Box,
  Button,
  Card,
  Divider,
  Group,
  Modal,
  Pagination,
  Stack,
  Table,
  Tabs,
  Text,
  Title,
} from "@mantine/core";
import { EmptyState, PageHeader } from "@jianartifact/ui";
import { useEffect, useReducer, useState } from "react";
import { useTranslation } from "react-i18next";
import { useLocation } from "react-router-dom";

import { AsyncBoundary } from "../components/AsyncBoundary";
import { SyncLogChangesView } from "../components/repo/SyncLogChangesView";
import {
  getClusterStatus,
  getClusterSyncLogs,
  triggerClusterSync,
  type SyncLogEntry,
} from "../api/endpoints";
import { useAsync } from "../hooks/useAsync";
import { density } from "../theme/density";

// 同步历史每页条数。
const SYNC_PAGE_SIZE = 20;

// 实体类型 → i18n key 的映射（变更构成列展示"用户2 · 仓库1 · 制品5"）。
const ENTITY_KEY_TO_T: Record<string, string> = {
  user: "cluster.entityUser",
  repository: "cluster.entityRepository",
  acl: "cluster.entityAcl",
  token: "cluster.entityToken",
  asset: "cluster.entityAsset",
  setting: "cluster.entitySetting",
};

function StatRow({ label, value }: { label: string; value: string }) {
  return (
    <Group justify="space-between" gap="md" wrap="nowrap">
      <Text size="sm" c="dimmed">
        {label}
      </Text>
      <Text size="sm" fw={500} ta="right">
        {value}
      </Text>
    </Group>
  );
}

/** 解析同步日志的变更实体构成 JSON（如 {"asset":5,"repository":2}）为中文摘要。 */
function entitySummary(entityCounts: string, t: (key: string) => string): string {
  try {
    const counts: Record<string, number> = JSON.parse(entityCounts);
    return Object.entries(counts)
      .filter(([, n]) => n > 0)
      .map(([k, n]) => `${t(ENTITY_KEY_TO_T[k] ?? k)}${n}`)
      .join(" · ");
  } catch {
    return entityCounts;
  }
}

/** 同步状态徽章：进行中（success=null）/ 成功 / 失败。 */
function statusBadge(entry: SyncLogEntry, t: (key: string) => string) {
  if (entry.success === null) {
    return (
      <Badge color="blue" variant="light">
        {t("cluster.syncLogRunning")}
      </Badge>
    );
  }
  return entry.success ? (
    <Badge color="green" variant="light">
      {t("cluster.syncLogSuccess")}
    </Badge>
  ) : (
    <Badge color="red" variant="light">
      {t("cluster.syncLogFailedStatus")}
    </Badge>
  );
}

export function ClusterPage() {
  const { t } = useTranslation();
  const location = useLocation();
  const state = useAsync(getClusterStatus, []);
  const [, force] = useReducer((x) => x + 1, 0);
  const [syncing, setSyncing] = useState(false);
  // tab 接 URL 锚点（#cluster / #history）：刷新/回退/分享可定位到对应 tab。
  const [tab, setTab] = useState(() => location.hash.replace("#", "") || "cluster");
  useEffect(() => {
    const onHash = () => setTab(window.location.hash.replace("#", "") || "cluster");
    window.addEventListener("hashchange", onHash);
    return () => window.removeEventListener("hashchange", onHash);
  }, []);
  // FR-98：同步历史详情以模态框/弹出层展示（点击详情打开，可正常关闭返回，不做路由跳转）。
  const [detailLog, setDetailLog] = useState<Pick<SyncLogEntry, "id" | "changes"> | null>(null);

  // 同步历史（FR-88）：独立分页加载，不阻塞主状态。
  const [syncPage, setSyncPage] = useState(0);
  const syncState = useAsync(
    () => getClusterSyncLogs(SYNC_PAGE_SIZE, syncPage * SYNC_PAGE_SIZE),
    [syncPage],
  );

  return (
    /* 固定布局：页面撑满内容区高度，body 不滚；页头/tab 固定，内容区 flex 自适应（对齐仓库列表页 FR-68）。 */
    <Stack
      gap="sm"
      style={{
        height:
          "calc(100vh - var(--app-shell-header-offset, 56px) - 2 * var(--app-shell-padding, 12px))",
        overflow: "hidden",
      }}
    >
      <PageHeader title={t("cluster.title")} description={t("cluster.description")} />
      {/* 内容区宽度由全局 contentMaxWidth 控制，页面内不再二次限宽（FR-86 布局修复）。 */}
      <Tabs
        value={tab}
        onChange={(v) => {
          const next = v ?? "cluster";
          setTab(next);
          window.location.hash = next;
        }}
        style={{ flex: 1, minHeight: 0, display: "flex", flexDirection: "column" }}
      >
        <Tabs.List>
          <Tabs.Tab value="cluster">{t("cluster.title")}</Tabs.Tab>
          <Tabs.Tab value="history">{t("cluster.syncHistoryTitle")}</Tabs.Tab>
        </Tabs.List>

        {/* 集群 tab：同步状态 + 立即同步（对端配置与自动同步开关已迁设置页）。 */}
        <Tabs.Panel value="cluster" pt="md" style={{ flex: 1, minHeight: 0, overflowY: "auto" }}>
          <AsyncBoundary state={state}>
            {(st) => (
              <Stack gap="md" maw={900}>
                {!st.peerUrl ? (
                  <Card withBorder radius="md" padding={density.cardPadding}>
                    <EmptyState message={t("cluster.notConfigured")} />
                  </Card>
                ) : (
                  <Card withBorder radius="md" padding={density.cardPadding}>
                    <Stack gap="xs">
                      <Group justify="space-between" align="center" wrap="nowrap">
                        <Title order={5}>{t("cluster.title")}</Title>
                        <Button
                          size="xs"
                          variant="default"
                          loading={syncing}
                          disabled={!st.peerUrl}
                          onClick={async () => {
                            setSyncing(true);
                            try {
                              await triggerClusterSync();
                              force();
                            } finally {
                              setSyncing(false);
                            }
                          }}
                        >
                          {t("cluster.syncNow")}
                        </Button>
                      </Group>
                      <StatRow label={t("cluster.nodeId")} value={st.nodeId} />
                      <StatRow label={t("cluster.peerUrl")} value={st.peerUrl} />
                      <StatRow
                        label={t("cluster.token")}
                        value={
                          st.tokenSet ? t("cluster.tokenConfigured") : t("cluster.tokenMissing")
                        }
                      />
                      <StatRow
                        label={t("cluster.watermark")}
                        value={st.hasWatermark ? String(st.watermark) : t("cluster.watermarkNone")}
                      />
                      <StatRow
                        label={t("cluster.lastSync")}
                        value={st.lastSyncAt ?? t("cluster.lastSyncNever")}
                      />
                      {st.lastError && (
                        <StatRow label={t("cluster.lastError")} value={st.lastError} />
                      )}
                      {st.lastSync && (
                        <>
                          <Divider my={4} />
                          <Text size="sm" fw={600}>
                            {t("cluster.lastSyncSummary")}
                          </Text>
                          <StatRow
                            label={t("cluster.syncLogChanges")}
                            value={String(st.lastSync.changes)}
                          />
                          <StatRow
                            label={t("cluster.syncLogApplied")}
                            value={String(st.lastSync.applied)}
                          />
                          <StatRow
                            label={t("cluster.syncLogFailed")}
                            value={String(st.lastSync.failed)}
                          />
                          <StatRow
                            label={t("cluster.syncLogBlobs")}
                            value={String(st.lastSync.blobs)}
                          />
                          {st.lastSync.entityCounts !== "{}" && (
                            <StatRow
                              label={t("cluster.syncLogEntities")}
                              value={entitySummary(st.lastSync.entityCounts, t)}
                            />
                          )}
                          <Group gap="xs" mt={4}>
                            {statusBadge(
                              {
                                success: st.lastSync.success,
                                startedAt: st.lastSync.startedAt,
                                finishedAt: st.lastSync.finishedAt,
                                fromSeq: st.lastSync.fromSeq,
                                toSeq: st.lastSync.toSeq,
                                changes: st.lastSync.changes,
                                applied: st.lastSync.applied,
                                failed: st.lastSync.failed,
                                blobs: st.lastSync.blobs,
                                entityCounts: st.lastSync.entityCounts,
                                errorText: st.lastSync.errorText,
                              } as SyncLogEntry,
                              t,
                            )}
                            {st.lastSync.errorText && (
                              <Text size="xs" c="red">
                                {st.lastSync.errorText}
                              </Text>
                            )}
                          </Group>
                        </>
                      )}
                    </Stack>
                  </Card>
                )}
              </Stack>
            )}
          </AsyncBoundary>
        </Tabs.Panel>

        {/* 同步历史 tab（FR-88）：完整记录 + 进度 + 详细信息可视化。 */}
        <Tabs.Panel value="history" pt="md" style={{ flex: 1, minHeight: 0, display: "flex", flexDirection: "column" }}>
          <Card withBorder radius="md" padding={density.cardPadding} style={{ flex: 1, minHeight: 0, display: "flex", flexDirection: "column", overflow: "hidden" }}>
            <Stack gap="sm" style={{ flex: 1, minHeight: 0, display: "flex", flexDirection: "column" }}>
              <Text size="xs" c="dimmed">
                {t("cluster.syncHistoryDesc")}
              </Text>
              <AsyncBoundary
                state={syncState}
                style={{ flex: 1, minHeight: 0, display: "flex", flexDirection: "column" }}
              >
                {(list) =>
                  (list.items ?? []).length === 0 ? (
                    <Box style={{ flex: 1, minHeight: 0, display: "grid", placeItems: "center" }}>
                      <EmptyState message={t("cluster.syncLogEmpty")} />
                    </Box>
                  ) : (
                    <Box style={{ flex: 1, minHeight: 0, overflow: "auto" }}>
                      <Table striped highlightOnHover withTableBorder stickyHeader style={{ minWidth: 1280 }}>
                        <Table.Thead>
                          <Table.Tr>
                            <Table.Th style={{ whiteSpace: "nowrap" }}>{t("cluster.syncLogTime")}</Table.Th>
                            <Table.Th style={{ whiteSpace: "nowrap" }}>{t("cluster.syncLogPeer")}</Table.Th>
                            <Table.Th style={{ whiteSpace: "nowrap" }}>{t("cluster.syncLogStatus")}</Table.Th>
                            <Table.Th style={{ whiteSpace: "nowrap" }}>{t("cluster.syncLogEntities")}</Table.Th>
                            <Table.Th style={{ whiteSpace: "nowrap" }}>{t("cluster.syncLogChanges")}</Table.Th>
                            <Table.Th style={{ whiteSpace: "nowrap" }}>{t("cluster.syncLogApplied")}</Table.Th>
                            <Table.Th style={{ whiteSpace: "nowrap" }}>{t("cluster.syncLogFailed")}</Table.Th>
                            <Table.Th style={{ whiteSpace: "nowrap" }}>{t("cluster.syncLogBlobs")}</Table.Th>
                            <Table.Th style={{ whiteSpace: "nowrap" }}>{t("cluster.syncLogWatermark")}</Table.Th>
                            <Table.Th style={{ whiteSpace: "nowrap" }}>{t("cluster.syncLogDetail", { defaultValue: "详情" })}</Table.Th>
                            <Table.Th style={{ whiteSpace: "nowrap" }}>{t("cluster.lastError")}</Table.Th>
                          </Table.Tr>
                        </Table.Thead>
                        <Table.Tbody>
                          {list.items.map((e) => (
                            <Table.Tr key={e.id}>
                              <Table.Td style={{ whiteSpace: "nowrap" }}>{new Date(e.startedAt).toLocaleString()}</Table.Td>
                              <Table.Td>
                                <Text size="xs" title={e.peerUrl} truncate="end" style={{ maxWidth: 220 }}>
                                  {e.peerUrl}
                                </Text>
                              </Table.Td>
                              <Table.Td>{statusBadge(e, t)}</Table.Td>
                              <Table.Td>
                                <Text size="xs" style={{ minWidth: 140 }}>
                                  {entitySummary(e.entityCounts, t)}
                                </Text>
                              </Table.Td>
                              <Table.Td>{e.changes}</Table.Td>
                              <Table.Td>{e.applied}</Table.Td>
                              <Table.Td>{e.failed}</Table.Td>
                              <Table.Td>{e.blobs}</Table.Td>
                              <Table.Td style={{ whiteSpace: "nowrap" }}>{`${e.fromSeq} → ${e.toSeq}`}</Table.Td>
                              <Table.Td>
                                {/* FR-98：详情以模态框弹出（该次同步的具体变更），可正常关闭返回 */}
                                <Anchor
                                  size="xs"
                                  component="button"
                                  type="button"
                                  onClick={() => setDetailLog({ id: e.id, changes: e.changes })}
                                >
                                  {t("cluster.syncLogDetail", { defaultValue: "详情" })}
                                </Anchor>
                              </Table.Td>
                              <Table.Td>
                                {e.errorText ? (
                                  <Text
                                    size="xs"
                                    c="red"
                                    title={e.errorText}
                                    truncate="end"
                                    style={{ maxWidth: 200 }}
                                  >
                                    {e.errorText}
                                  </Text>
                                ) : null}
                              </Table.Td>
                            </Table.Tr>
                          ))}
                        </Table.Tbody>
                      </Table>
                    </Box>
                  )
                }
              </AsyncBoundary>
              {syncState.data && (
                <Group justify="flex-end" style={{ flexShrink: 0 }}>
                  <Pagination
                    value={syncPage + 1}
                    total={Math.max(1, Math.ceil(syncState.data.total / SYNC_PAGE_SIZE))}
                    onChange={(p) => setSyncPage(p - 1)}
                    size="xs"
                  />
                </Group>
              )}
            </Stack>
          </Card>
        </Tabs.Panel>
      </Tabs>

      {/* FR-98：同步历史详情模态框（弹出层展示该次同步的具体变更，可正常关闭返回） */}
      <Modal
        opened={detailLog !== null}
        onClose={() => setDetailLog(null)}
        title={t("cluster.syncLogDetailTitle", { defaultValue: "同步历史详情" })}
        size="xl"
        centered
        styles={{ body: { height: "70vh", overflow: "hidden", display: "flex", flexDirection: "column" } }}
      >
        {detailLog !== null && (
          <SyncLogChangesView logId={detailLog.id} hasChanges={detailLog.changes > 0} />
        )}
      </Modal>
    </Stack>
  );
}
