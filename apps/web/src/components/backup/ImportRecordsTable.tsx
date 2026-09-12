// 导入记录表（FR-137）：Mantine Table（layout fixed + highlightOnHover + 吸顶表头）。
// 状态用 StatusPill；fetching 阶段用 Progress 展示拉取进度；
// pending_restart 行内给「需重启服务后生效」提示；失败行显示错误摘要。
import { Box, Progress, Table, Text } from "@mantine/core";
import { EmptyState } from "@jianartifact/ui";
import { useTranslation } from "react-i18next";

import type { BackupImport } from "../../api/types";
import { formatStamp } from "../../lib/format";
import { StatusPill } from "../ops/OpsKit";
import { importStatusTone } from "./status";

export interface ImportRecordsTableProps {
  items: BackupImport[];
  loading?: boolean;
}

export function ImportRecordsTable({ items }: ImportRecordsTableProps) {
  const { t } = useTranslation();

  if (items.length === 0) {
    return (
      <Box p="xl">
        <EmptyState message={t("backups.empty")} description={t("backups.emptyHint")} />
      </Box>
    );
  }

  return (
    <Table
      layout="fixed"
      highlightOnHover
      stickyHeader
      verticalSpacing="sm"
      horizontalSpacing="md"
      fz="sm"
    >
      <Table.Thead>
        <Table.Tr>
          <Table.Th w="22%">{t("backups.importColId")}</Table.Th>
          <Table.Th w={90}>{t("backups.importColSource")}</Table.Th>
          <Table.Th w={110}>{t("backups.importColStatus")}</Table.Th>
          <Table.Th>{t("backups.importColProgress")}</Table.Th>
          <Table.Th w="20%">{t("backups.importColPackage")}</Table.Th>
          <Table.Th w={150}>{t("backups.importColCreated")}</Table.Th>
          <Table.Th w={200}>{t("backups.importColError")}</Table.Th>
        </Table.Tr>
      </Table.Thead>
      <Table.Tbody>
        {items.map((item) => {
          const total = item.totalBytes ?? 0;
          const fetched = item.fetchedBytes ?? 0;
          const ratio = total > 0 ? Math.min(100, (fetched / total) * 100) : 0;
          const errorSummary = item.errorCode
            ? t(`backups.backupImportError.${item.errorCode}`, { defaultValue: item.error ?? "" })
            : item.error;
          return (
            <Table.Tr key={item.importId}>
              <Table.Td>
                <Text ff="monospace" size="sm" truncate title={item.importId}>
                  {item.importId}
                </Text>
              </Table.Td>
              <Table.Td>
                <Text size="sm">{t(`backups.import_origin_${item.origin}`)}</Text>
              </Table.Td>
              <Table.Td>
                <StatusPill
                  tone={importStatusTone(item.status)}
                  text={t(`backups.import_status_${item.status}`)}
                />
              </Table.Td>
              <Table.Td>
                {item.status === "fetching" ? (
                  <Progress value={ratio} size="sm" />
                ) : item.status === "pending_restart" ? (
                  <Text size="xs" c="orange">
                    {t("backups.importPendingRestartHint")}
                  </Text>
                ) : (
                  <Text size="xs" c="dimmed">
                    —
                  </Text>
                )}
              </Table.Td>
              <Table.Td>
                <Text ff="monospace" size="sm" truncate title={item.packageId ?? ""}>
                  {item.packageId || "—"}
                </Text>
              </Table.Td>
              <Table.Td>
                <Text size="sm" c="dimmed">
                  {formatStamp(item.createdAt)}
                </Text>
              </Table.Td>
              <Table.Td>
                {item.status === "failed" && errorSummary ? (
                  <Text size="xs" c="red" truncate title={errorSummary}>
                    {errorSummary}
                  </Text>
                ) : (
                  <Text size="xs" c="dimmed">
                    —
                  </Text>
                )}
              </Table.Td>
            </Table.Tr>
          );
        })}
      </Table.Tbody>
    </Table>
  );
}
