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
function opBadge(op: string, t: (key: string, options?: { defaultValue: string }) => string) {
  const color = op === "delete" ? "red" : "blue";
  const label = op === "delete"
    ? t("cluster.syncLogOpDelete", { defaultValue: "删除" })
    : t("cluster.syncLogOpPut", { defaultValue: "写入" });
  return <Badge color={color} size="xs">{label}</Badge>;
}

/** 变更实体类型徽章。 */
function entityBadge(entityType: string, t: (key: string, options?: { defaultValue: string }) => string) {
  const color =
    entityType === "asset" ? "blue" : entityType === "repository" ? "grape" : entityType === "user" ? "teal" : "gray";
  const labelKey = `cluster.entity${entityType.slice(0, 1).toUpperCase()}${entityType.slice(1)}`;
  return <Badge color={color} variant="light" size="xs">{t(labelKey, { defaultValue: entityType })}</Badge>;
}

/**
 * 同步历史变更明细：传入某次同步的 logId，展示该次同步的具体变更（分类 + 分页）。
 * 用于集群页详情模态框（FR-98 增强：点击详情弹出层，可正常关闭返回）。
 */
export function SyncLogChangesView({
  logId,
  hasChanges = true,
}: {
  logId: number;
  hasChanges?: boolean;
}) {
  const { t } = useTranslation();
  const [page, setPage] = useState(1);
  // 按实体类型分类展示（空串=全部）
  const [entityType, setEntityType] = useState("");
  // 同步记录本身（取标题信息：时间/对端/水位/统计）
  const logsState = useAsync(() => getClusterSyncLogs(200, 0), []);
  // 该次同步的变更列表（分页 + 按实体类型过滤）
  const changesState = useAsync(
    () =>
      hasChanges
        ? getClusterSyncLogChanges(logId, PAGE_SIZE, (page - 1) * PAGE_SIZE, entityType || undefined)
        : Promise.resolve({ items: [], total: 0 }),
    [logId, page, entityType, hasChanges],
  );
  useEffect(() => {
    const reload = () => {
      changesState.reload();
      logsState.reload();
    };
    window.addEventListener(REFRESH_EVENT, reload);
    return () => window.removeEventListener(REFRESH_EVENT, reload);
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
    <Stack gap="sm" style={{ flex: 1, minHeight: 0, display: "flex", flexDirection: "column" }}>
      {entry && (
        <Group gap="sm">
          <Text size="xs" c="dimmed">
            {t("cluster.syncLogStatus", { defaultValue: "状态" })}: {" "}
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
      <Box style={{ flexShrink: 0, overflowX: "auto" }}>
        <SegmentedControl
          size="xs"
          value={entityType}
          onChange={(v) => {
            setEntityType(v);
            setPage(1);
          }}
          data={typeOptions}
          style={{ minWidth: 460 }}
        />
      </Box>
      <AsyncBoundary
        state={changesState}
        style={{ flex: 1, minHeight: 0, display: "flex", flexDirection: "column" }}
      >
        {(list) =>
          (list.items ?? []).length === 0 ? (
            <Box style={{ flex: 1, minHeight: 0, display: "grid", placeItems: "center" }}>
              <EmptyState
                message={t("cluster.syncLogChangesEmpty", { defaultValue: "该次同步无变更记录" })}
              />
            </Box>
          ) : (
            <Box style={{ flex: 1, minHeight: 0, overflow: "auto" }}>
              <Table striped highlightOnHover withTableBorder stickyHeader style={{ minWidth: 720 }}>
                <Table.Thead>
                  <Table.Tr>
                    <Table.Th style={{ whiteSpace: "nowrap" }}>{t("cluster.syncLogSeq", { defaultValue: "seq" })}</Table.Th>
                    <Table.Th style={{ whiteSpace: "nowrap" }}>{t("cluster.syncLogOp", { defaultValue: "操作" })}</Table.Th>
                    <Table.Th style={{ whiteSpace: "nowrap" }}>{t("cluster.syncLogEntityType", { defaultValue: "实体" })}</Table.Th>
                    <Table.Th style={{ whiteSpace: "nowrap" }}>{t("cluster.syncLogEntityKey", { defaultValue: "对象" })}</Table.Th>
                    <Table.Th style={{ whiteSpace: "nowrap" }}>{t("cluster.syncLogTs", { defaultValue: "时间" })}</Table.Th>
                  </Table.Tr>
                </Table.Thead>
                <Table.Tbody>
                  {list.items.map((ch: ReplChangeEntry) => (
                    <Table.Tr key={ch.seq}>
                      <Table.Td style={{ whiteSpace: "nowrap" }}>{ch.seq}</Table.Td>
                      <Table.Td>{opBadge(ch.op, t)}</Table.Td>
                      <Table.Td>{entityBadge(ch.entityType, t)}</Table.Td>
                      <Table.Td>
                        <Text size="xs" style={{ minWidth: 240, wordBreak: "break-word" }}>
                          {ch.entityKey}
                        </Text>
                      </Table.Td>
                      <Table.Td style={{ whiteSpace: "nowrap" }}>{new Date(ch.ts).toLocaleString()}</Table.Td>
                    </Table.Tr>
                  ))}
                </Table.Tbody>
              </Table>
            </Box>
          )
        }
      </AsyncBoundary>
      {changesState.data && (
        <Group justify="flex-end" style={{ flexShrink: 0 }}>
          <Pagination
            total={Math.max(1, Math.ceil(changesState.data.total / PAGE_SIZE))}
            value={page}
            onChange={setPage}
            size="xs"
          />
        </Group>
      )}
    </Stack>
  );
}
