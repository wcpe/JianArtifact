// 备份包列表：Mantine Table（layout fixed + highlightOnHover + 吸顶表头）。
// 项目约定：列表一律用 Table 而非卡片堆，状态一律 StatusPill。
import { Badge, Button, Group, Loader, Table, Text } from "@mantine/core";
import {
  IconCircleCheck,
  IconDownload,
  IconLink,
  IconReload,
  IconTrash,
} from "@tabler/icons-react";
import { useTranslation } from "react-i18next";

import type { BackupPackage } from "../../api/types";
import { copyToClipboard } from "../../lib/clipboard";
import { formatBytes, formatStamp } from "../../lib/format";
import { notifyError, notifySuccess } from "../../lib/feedback";
import { StatusPill } from "../ops/OpsKit";
import { backupInProgress, backupModeTone, backupStatusTone } from "./status";

export interface BackupTableProps {
  items: BackupPackage[];
  /** 正在进行校验的包标识。 */
  verifyingId?: string | null;
  onGetLink: (id: string) => void;
  onVerify: (id: string, deep: boolean) => void;
  onDelete: (item: BackupPackage) => void;
}

export function BackupTable({
  items,
  verifyingId,
  onGetLink,
  onVerify,
  onDelete,
}: BackupTableProps) {
  const { t } = useTranslation();

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
          <Table.Th w="26%">{t("backups.colPackage")}</Table.Th>
          <Table.Th w={110}>{t("backups.colMode")}</Table.Th>
          <Table.Th w={130}>{t("backups.colStatus")}</Table.Th>
          <Table.Th w={100}>{t("backups.colSize")}</Table.Th>
          <Table.Th w="18%">{t("backups.colCounts")}</Table.Th>
          <Table.Th w={170}>{t("backups.colCreated")}</Table.Th>
          <Table.Th w={180}>{t("backups.colActions")}</Table.Th>
        </Table.Tr>
      </Table.Thead>
      <Table.Tbody>
        {items.map((item) => {
          const done = item.status === "done";
          const running = backupInProgress(item.status);
          return (
            <Table.Tr key={item.packageId}>
              <Table.Td>
                <Text ff="monospace" size="sm" truncate title={item.packageId}>
                  {item.packageId}
                </Text>
                {item.label ? (
                  <Text size="xs" c="dimmed" truncate>
                    {item.label}
                  </Text>
                ) : null}
              </Table.Td>
              <Table.Td>
                <Badge color={backupModeTone(item.mode)} variant="light" tt="none">
                  {t(`backups.mode_${item.mode}`)}
                </Badge>
              </Table.Td>
              <Table.Td>
                <Group gap={6} wrap="nowrap">
                  {running ? <Loader size="xs" /> : null}
                  <StatusPill
                    tone={backupStatusTone(item.status)}
                    text={t(`backups.status_${item.status}`)}
                  />
                </Group>
                {item.status === "failed" && item.errorSummary ? (
                  <Text size="xs" c="red" truncate title={item.errorSummary}>
                    {item.errorSummary}
                  </Text>
                ) : null}
              </Table.Td>
              <Table.Td>
                <Text size="sm">{done ? formatBytes(item.sizeBytes) : "—"}</Text>
              </Table.Td>
              <Table.Td>
                <Text size="sm" c="dimmed" truncate>
                  {item.counts
                    ? t("backups.countsSummary", {
                        assets: item.counts.assets ?? 0,
                        repos: item.counts.repositories ?? 0,
                      })
                    : "—"}
                </Text>
              </Table.Td>
              <Table.Td>
                <Text size="sm" c="dimmed">
                  {formatStamp(item.createdAt)}
                </Text>
              </Table.Td>
              <Table.Td>
                <Group gap={4} wrap="wrap">
                  <Button
                    size="compact-xs"
                    variant="subtle"
                    leftSection={<IconDownload size={14} />}
                    aria-label={t("backups.getLink")}
                    disabled={!done}
                    onClick={() => onGetLink(item.packageId)}
                  >
                    {t("backups.getLink")}
                  </Button>
                  <Button
                    size="compact-xs"
                    variant="subtle"
                    leftSection={<IconLink size={14} />}
                    aria-label={t("backups.copyCommand")}
                    disabled={!done}
                    onClick={() =>
                      void copyToClipboard(`jianartifact backup link ${item.packageId}`).then(
                        (ok) =>
                          ok
                            ? notifySuccess(t("backups.commandCopied"))
                            : notifyError(t("backups.copyFailed")),
                      )
                    }
                  >
                    {t("backups.copyCommand")}
                  </Button>
                  <Button
                    size="compact-xs"
                    variant="subtle"
                    leftSection={<IconCircleCheck size={14} />}
                    aria-label={t("backups.verify")}
                    disabled={!done}
                    loading={verifyingId === item.packageId}
                    onClick={() => onVerify(item.packageId, false)}
                  >
                    {t("backups.verify")}
                  </Button>
                  <Button
                    size="compact-xs"
                    variant="subtle"
                    leftSection={<IconReload size={14} />}
                    aria-label={t("backups.verifyDeep")}
                    disabled={!done}
                    onClick={() => onVerify(item.packageId, true)}
                  >
                    {t("backups.verifyDeep")}
                  </Button>
                  <Button
                    size="compact-xs"
                    variant="subtle"
                    color="red"
                    leftSection={<IconTrash size={14} />}
                    aria-label={t("backups.delete")}
                    disabled={running}
                    onClick={() => onDelete(item)}
                  >
                    {t("backups.delete")}
                  </Button>
                </Group>
              </Table.Td>
            </Table.Tr>
          );
        })}
      </Table.Tbody>
    </Table>
  );
}
