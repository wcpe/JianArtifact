// FR-86/88: 集群页——「集群」tab（对端配置、自动同步开关、立即同步按钮、同步状态）与
// 「同步历史」tab（复制记录与进度可视化，仅记录有变更/失败的事件）。仅管理员。
// 数据来自非契约端点 GET/PUT /api/v1/cluster、POST /api/v1/cluster/sync-now、GET /api/v1/cluster/sync-logs（复制引擎见 FR-83~85）。
import {
  Badge,
  Button,
  Card,
  Group,
  Pagination,
  Stack,
  Switch,
  Table,
  Tabs,
  Text,
  TextInput,
  Title,
} from "@mantine/core";
import { EmptyState, PageHeader } from "@jianartifact/ui";
import { useReducer, useState } from "react";
import { useTranslation } from "react-i18next";

import { AsyncBoundary } from "../components/AsyncBoundary";
import {
  getClusterStatus,
  getClusterSyncLogs,
  setClusterConfig,
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

  // 对端配置表单（令牌不回显，留空保持不变）。
  const [peerUrl, setPeerUrl] = useState("");
  const [peerToken, setPeerToken] = useState("");
  const [saving, setSaving] = useState(false);
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
      <Tabs defaultValue="cluster" maw={900}>
        <Tabs.List>
          <Tabs.Tab value="cluster">{t("cluster.title")}</Tabs.Tab>
          <Tabs.Tab value="history">{t("cluster.syncHistoryTitle")}</Tabs.Tab>
        </Tabs.List>

        {/* 集群 tab：对端配置 / 状态 / 同步控制。 */}
        <Tabs.Panel value="cluster" pt="md">
          <AsyncBoundary state={state}>
            {(st) => (
              <Stack gap="md" maw={640}>
                {/* 对端配置表单（FR-88）：保存仅入库，不自动开始同步。 */}
                <Card withBorder radius="md" padding={density.cardPadding}>
                  <Stack gap="sm">
                    <Title order={5}>{t("cluster.peerConfigTitle")}</Title>
                    <TextInput
                      label={t("cluster.peerUrlLabel")}
                      placeholder="https://repo1.wcpe.top"
                      value={peerUrl || st.peerUrl || ""}
                      onChange={(e) => setPeerUrl(e.currentTarget.value)}
                    />
                    <TextInput
                      label={t("cluster.peerTokenLabel")}
                      placeholder={t("cluster.peerTokenPlaceholder")}
                      value={peerToken}
                      onChange={(e) => setPeerToken(e.currentTarget.value)}
                    />
                    <Group justify="space-between" align="center">
                      <Text size="xs" c="dimmed">
                        {t("cluster.peerConfigHint")}
                      </Text>
                      <Button
                        size="xs"
                        variant="default"
                        loading={saving}
                        onClick={async () => {
                          setSaving(true);
                          try {
                            await setClusterConfig({
                              peerUrl: peerUrl || undefined,
                              peerToken: peerToken || undefined,
                            });
                            setPeerToken(""); // 令牌不回显
                            force();
                          } finally {
                            setSaving(false);
                          }
                        }}
                      >
                        {t("cluster.saveConfig")}
                      </Button>
                    </Group>
                  </Stack>
                </Card>

                {!st.peerUrl ? (
                  <Card withBorder radius="md" padding={density.cardPadding}>
                    <EmptyState message={t("cluster.notConfigured")} />
                  </Card>
                ) : (
                  <Card withBorder radius="md" padding={density.cardPadding}>
                    <Stack gap="xs">
                      <Title order={5}>{t("cluster.title")}</Title>
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
                    </Stack>
                  </Card>
                )}

                {/* 同步控制：自动同步开关 + 立即同步（FR-88）。 */}
                <Card withBorder radius="md" padding={density.cardPadding}>
                  <Group justify="space-between" gap="md" wrap="nowrap">
                    <Stack gap={0}>
                      <Text size="sm" fw={500}>
                        {t("cluster.enabledLabel")}
                      </Text>
                      <Text size="xs" c="dimmed">
                        {t("cluster.enabledHint")}
                      </Text>
                    </Stack>
                    <Badge color={st.enabled ? "green" : "gray"} variant="light">
                      {st.enabled ? t("common.yes") : t("common.no")}
                    </Badge>
                    <Group gap="xs" wrap="nowrap">
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
                      <Switch
                        checked={st.enabled}
                        disabled={saving}
                        aria-label={t("cluster.enabledLabel")}
                        onChange={async (e) => {
                          setSaving(true);
                          try {
                            await setClusterConfig({ enabled: e.currentTarget.checked });
                            force();
                          } finally {
                            setSaving(false);
                          }
                        }}
                      />
                    </Group>
                  </Group>
                </Card>
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
                  list.items.length === 0 ? (
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
