// FR-86/88/90: 集群页——「集群」tab（同步状态 + 立即同步按钮；对端配置与自动同步
// 开关已迁至设置页「集群」tab）与「同步历史」tab（复制记录与进度可视化，仅记录有
// 变更/失败的事件）。仅管理员。
// 数据来自非契约端点 GET /api/v1/cluster、POST /api/v1/cluster/sync-now、GET /api/v1/cluster/sync-logs（复制引擎见 FR-83~85）。
import {
  Anchor,
  Badge,
  Button,
  Card,
  Divider,
  Group,
  Pagination,
  Stack,
  Table,
  Tabs,
  Text,
  Title,
} from "@mantine/core";
import { EmptyState, PageHeader } from "@jianartifact/ui";
import { useReducer, useState } from "react";
import { useTranslation } from "react-i18next";
import { useNavigate } from "react-router-dom";

import { AsyncBoundary } from "../components/AsyncBoundary";
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
  const state = useAsync(getClusterStatus, []);
  const [, force] = useReducer((x) => x + 1, 0);
  const navigate = useNavigate(); // FR-98：跳转同步历史详情二级页
  const [syncing, setSyncing] = useState(false);

  // 同步历史（FR-88）：独立分页加载，不阻塞主状态。
  const [syncPage, setSyncPage] = useState(0);
  const syncState = useAsync(
    () => getClusterSyncLogs(SYNC_PAGE_SIZE, syncPage * SYNC_PAGE_SIZE),
    [syncPage],
  );

  return (
    <>
      <PageHeader title={t("cluster.title")} description={t("cluster.description")} />
      {/* 内容区宽度由全局 contentMaxWidth 控制，页面内不再二次限宽（FR-86 布局修复）。 */}
      <Tabs defaultValue="cluster">
        <Tabs.List>
          <Tabs.Tab value="cluster">{t("cluster.title")}</Tabs.Tab>
          <Tabs.Tab value="history">{t("cluster.syncHistoryTitle")}</Tabs.Tab>
        </Tabs.List>

        {/* 集群 tab：同步状态 + 立即同步（对端配置与自动同步开关已迁设置页）。 */}
        <Tabs.Panel value="cluster" pt="md">
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
        <Tabs.Panel value="history" pt="md">
          <Card withBorder radius="md" padding={density.cardPadding}>
            <Stack gap="sm">
              <Text size="xs" c="dimmed">
                {t("cluster.syncHistoryDesc")}
              </Text>
              <AsyncBoundary state={syncState}>
                {(list) =>
                  (list.items ?? []).length === 0 ? (
                    <EmptyState message={t("cluster.syncLogEmpty")} />
                  ) : (
                    <>
                      <Table striped highlightOnHover withTableBorder>
                        <Table.Thead>
                          <Table.Tr>
                            <Table.Th>{t("cluster.syncLogTime")}</Table.Th>
                            <Table.Th>{t("cluster.syncLogPeer")}</Table.Th>
                            <Table.Th>{t("cluster.syncLogStatus")}</Table.Th>
                            <Table.Th>{t("cluster.syncLogEntities")}</Table.Th>
                            <Table.Th>{t("cluster.syncLogChanges")}</Table.Th>
                            <Table.Th>{t("cluster.syncLogApplied")}</Table.Th>
                            <Table.Th>{t("cluster.syncLogFailed")}</Table.Th>
                            <Table.Th>{t("cluster.syncLogBlobs")}</Table.Th>
                            <Table.Th>{t("cluster.syncLogWatermark")}</Table.Th>
                            <Table.Th>{t("cluster.syncLogDetail", { defaultValue: "详情" })}</Table.Th>
                            <Table.Th>{t("cluster.lastError")}</Table.Th>
                          </Table.Tr>
                        </Table.Thead>
                        <Table.Tbody>
                          {list.items.map((e) => (
                            <Table.Tr key={e.id}>
                              <Table.Td>{new Date(e.startedAt).toLocaleString()}</Table.Td>
                              <Table.Td>{e.peerUrl}</Table.Td>
                              <Table.Td>{statusBadge(e, t)}</Table.Td>
                              <Table.Td>{entitySummary(e.entityCounts, t)}</Table.Td>
                              <Table.Td>{e.changes}</Table.Td>
                              <Table.Td>{e.applied}</Table.Td>
                              <Table.Td>{e.failed}</Table.Td>
                              <Table.Td>{e.blobs}</Table.Td>
                              <Table.Td>{`${e.fromSeq} → ${e.toSeq}`}</Table.Td>
                              <Table.Td>
                                {/* FR-98：跳转同步历史详情二级页（该次同步的具体变更） */}
                                <Anchor
                                  size="xs"
                                  component="button"
                                  type="button"
                                  onClick={() => navigate(`/cluster/sync-logs/${e.id}`)}
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
                                    truncate
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
                      <Group justify="flex-end">
                        <Pagination
                          value={syncPage + 1}
                          total={Math.max(1, Math.ceil(list.total / SYNC_PAGE_SIZE))}
                          onChange={(p) => setSyncPage(p - 1)}
                          size="xs"
                        />
                      </Group>
                    </>
                  )
                }
              </AsyncBoundary>
            </Stack>
          </Card>
        </Tabs.Panel>
      </Tabs>
    </>
  );
}
