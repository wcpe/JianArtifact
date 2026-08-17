// 集群同步历史二级页（FR-98）：查看某次同步（repl_sync_log:id）拉取/应用的具体变更列表。
// 变更从 repl_change 按该次同步的 fromSeq→toSeq 区间反推，分页展示。
import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { useNavigate, useParams } from "react-router-dom";
import {
  Anchor,
  Badge,
  Box,
  Button,
  Card,
  Group,
  Pagination,
  SegmentedControl,
  Stack,
  Table,
  Text,
} from "@mantine/core";
import {
  getClusterSyncLogChanges,
  getClusterSyncLogs,
  type ReplChangeEntry,
  type SyncLogEntry,
} from "../api/endpoints";
import { PageHeader } from "@jianartifact/ui";
import { EmptyState } from "@jianartifact/ui";
import { density } from "../theme/density";
import { useAsync, REFRESH_EVENT } from "../hooks/useAsync";
import { AsyncBoundary } from "../components/AsyncBoundary";

const PAGE_SIZE = 100;

/** 变更操作徽章：put / delete。 */
function opBadge(op: string) {
  const color = op === "delete" ? "red" : "blue";
  return <Badge color={color} size="xs">{op}</Badge>;
}

/** 变更实体类型徽章（简化显示）。 */
function entityBadge(entityType: string) {
  const color =
    entityType === "asset" ? "blue" : entityType === "repository" ? "grape" : entityType === "user" ? "teal" : "gray";
  return <Badge color={color} variant="light" size="xs">{entityType}</Badge>;
}

export function ClusterSyncLogDetailPage() {
  const { t } = useTranslation();
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const logId = Number(id);
  const [page, setPage] = useState(1);
  // FR-98：按实体类型分类展示（空串=全部）
  const [entityType, setEntityType] = useState("");
  // 同步记录本身（取标题信息：时间/对端/水位/统计）
  const logsState = useAsync(() => getClusterSyncLogs(200, 0), []);
  // 该次同步的变更列表（分页 + 按实体类型过滤）
  const changesState = useAsync(
    () => getClusterSyncLogChanges(logId, PAGE_SIZE, (page - 1) * PAGE_SIZE, entityType || undefined),
    [logId, page, entityType],
  );
  useEffect(() => {
    const reload = () => {
      changesState.reload();
      logsState.reload();
    };
    window.addEventListener(REFRESH_EVENT, reload);
    return () => window.removeEventListener(REFRESH_EVENT, reload);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // 实体类型分类选项（全部 + 各实体）
  const typeOptions = [
    { value: "", label: t("cluster.syncLogAllTypes", { defaultValue: "全部" }) },
    { value: "asset", label: t("cluster.entityAsset", { defaultValue: "制品" }) },
    { value: "repository", label: t("cluster.entityRepository", { defaultValue: "仓库" }) },
    { value: "user", label: t("cluster.entityUser", { defaultValue: "用户" }) },
    { value: "acl", label: t("cluster.entityAcl", { defaultValue: "ACL" }) },
    { value: "token", label: t("cluster.entityToken", { defaultValue: "令牌" }) },
    { value: "setting", label: t("cluster.entitySetting", { defaultValue: "配置" }) },
  ];

  const entry: SyncLogEntry | undefined =
    logsState.data?.items.find((e) => e.id === logId) ?? undefined;

  return (
    <>
      <PageHeader
        title={t("cluster.syncLogDetailTitle", { defaultValue: "同步历史详情" })}
        description={
          entry
            ? `${entry.peerUrl} · ${new Date(entry.startedAt).toLocaleString()} · ${entry.fromSeq} → ${entry.toSeq}`
            : undefined
        }
      />
      <Card withBorder radius="md" padding={density.cardPadding}>
        <Stack gap="sm">
          {entry && (
            <Group gap="sm">
              <Text size="xs" c="dimmed">
                {t("cluster.syncLogStatus", { defaultValue: "状态" })}:{" "}
                {entry.success === null
                  ? t("cluster.syncLogRunning", { defaultValue: "进行中" })
                  : entry.success
                    ? t("cluster.syncLogSuccess", { defaultValue: "成功" })
                    : t("cluster.syncLogFailed", { defaultValue: "失败" })}
              </Text>
              <Text size="xs" c="dimmed">
                {t("cluster.syncLogChanges", { defaultValue: "变更" })}: {entry.changes}
              </Text>
              <Text size="xs" c="dimmed">
                {t("cluster.syncLogApplied", { defaultValue: "应用" })}: {entry.applied}
              </Text>
              <Text size="xs" c="dimmed">
                {t("cluster.syncLogFailed", { defaultValue: "失败" })}: {entry.failed}
              </Text>
              <Text size="xs" c="dimmed">
                {t("cluster.syncLogBlobs", { defaultValue: "Blob" })}: {entry.blobs}
              </Text>
            </Group>
          )}
          <AsyncBoundary state={changesState}>
            {(list) =>
              (list.items ?? []).length === 0 ? (
                <EmptyState
                  message={t("cluster.syncLogChangesEmpty", {
                    defaultValue: "该次同步无变更记录",
                  })}
                />
              ) : (
                <>
                  {/* FR-98：按实体类型分类展示 */}
                  <SegmentedControl
                    size="xs"
                    value={entityType}
                    onChange={(v) => {
                      setEntityType(v);
                      setPage(1);
                    }}
                    data={typeOptions}
                  />
                  <Table striped highlightOnHover withTableBorder>
                    <Table.Thead>
                      <Table.Tr>
                        <Table.Th>{t("cluster.syncLogSeq", { defaultValue: "seq" })}</Table.Th>
                        <Table.Th>{t("cluster.syncLogOp", { defaultValue: "操作" })}</Table.Th>
                        <Table.Th>{t("cluster.syncLogEntityType", { defaultValue: "实体" })}</Table.Th>
                        <Table.Th>{t("cluster.syncLogEntityKey", { defaultValue: "对象" })}</Table.Th>
                        <Table.Th>{t("cluster.syncLogTs", { defaultValue: "时间" })}</Table.Th>
                      </Table.Tr>
                    </Table.Thead>
                    <Table.Tbody>
                      {list.items.map((ch: ReplChangeEntry) => (
                        <Table.Tr key={ch.seq}>
                          <Table.Td>{ch.seq}</Table.Td>
                          <Table.Td>{opBadge(ch.op)}</Table.Td>
                          <Table.Td>{entityBadge(ch.entityType)}</Table.Td>
                          <Table.Td>
                            <Text size="xs" style={{ wordBreak: "break-all" }}>
                              {ch.entityKey}
                            </Text>
                          </Table.Td>
                          <Table.Td>{new Date(ch.ts).toLocaleString()}</Table.Td>
                        </Table.Tr>
                      ))}
                    </Table.Tbody>
                  </Table>
                  {list.total > PAGE_SIZE && (
                    <Pagination
                      total={Math.ceil(list.total / PAGE_SIZE)}
                      value={page}
                      onChange={setPage}
                      size="sm"
                    />
                  )}
                </>
              )
            }
          </AsyncBoundary>
          <Group>
            <Button size="xs" variant="default" onClick={() => navigate("/cluster")}>
              {t("cluster.backToCluster", { defaultValue: "返回集群页" })}
            </Button>
          </Group>
        </Stack>
      </Card>
    </>
  );
}
