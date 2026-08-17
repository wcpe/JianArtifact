// 同步历史变更明细视图（FR-98）：某次同步（repl_sync_log:id）拉取/应用的具体变更列表。
// 供集群页「同步历史」详情模态框与独立详情页复用：分类 SegmentedControl + 变更表 + 分页。
import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { Badge, Box, Group, Pagination, SegmentedControl, Stack, Table, Text } from "@mantine/core";
import {
  getClusterSyncLogChanges,
  getClusterSyncLogs,
  type ReplChangeEntry,
  type SyncLogEntry,
} from "../../api/endpoints";
import { EmptyState } from "@jianartifact/ui";
import { useAsync, REFRESH_EVENT } from "../../hooks/useAsync";
import { AsyncBoundary } from "../../components/AsyncBoundary";

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

/**
 * 同步历史变更明细：传入某次同步的 logId，展示该次同步的具体变更（分类 + 分页）。
 * 用于集群页详情模态框（FR-98 增强：点击详情弹出层，可正常关闭返回）。
 */
export function SyncLogChangesView({ logId }: { logId: number }) {
  const { t } = useTranslation();
  const [page, setPage] = useState(1);
  // 按实体类型分类展示（空串=全部）
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
    <Stack gap="sm" style={{ height: "100%", minHeight: 0, display: "flex", flexDirection: "column" }}>
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
              message={t("cluster.syncLogChangesEmpty", { defaultValue: "该次同步无变更记录" })}
            />
          ) : (
            <>
              {/* 按实体类型分类展示 */}
              <SegmentedControl
                size="xs"
                value={entityType}
                onChange={(v) => {
                  setEntityType(v);
                  setPage(1);
                }}
                data={typeOptions}
              />
              {/* 内容区：表格区内滚 + sticky 表头，分页固定底部（内容大小自适应）。 */}
              <Box style={{ flex: 1, minHeight: 0, overflowY: "auto" }}>
                <Table striped highlightOnHover withTableBorder stickyHeader>
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
              </Box>
              {list.total > PAGE_SIZE && (
                <Pagination
                  total={Math.ceil(list.total / PAGE_SIZE)}
                  value={page}
                  onChange={setPage}
                  size="sm"
                  style={{ flexShrink: 0 }}
                />
              )}
            </>
          )
        }
      </AsyncBoundary>
    </Stack>
  );
}
