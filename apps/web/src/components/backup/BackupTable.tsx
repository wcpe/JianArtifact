// 备份包列表：Mantine Table（layout fixed + highlightOnHover + 吸顶表头）。
// 项目约定：列表一律用 Table 而非卡片堆，状态一律 StatusPill。
//
// 操作列分两层：行内只留"看一眼就要点"的常用动作（取链接、校验），其余收进
// 「⋯」溢出菜单。这不是审美偏好——六个按钮平铺会把行高从 40px 撑到 98px
// （按钮折成三行），一屏少看四条记录，而其中三个是低频动作。
import { Badge, Box, Button, Group, Loader, Menu, Table, Text } from "@mantine/core";
import { useMediaQuery } from "@mantine/hooks";
import {
  IconCircleCheck,
  IconDotsVertical,
  IconDownload,
  IconLink,
  IconReload,
  IconStethoscope,
  IconTrash,
} from "@tabler/icons-react";
import { useTranslation } from "react-i18next";

import type { BackupPackage } from "../../api/types";
import { copyToClipboard } from "../../lib/clipboard";
import { formatBytes, formatStamp } from "../../lib/format";
import { notifyError, notifySuccess } from "../../lib/feedback";
import { StatusPill } from "../ops/OpsKit";
import { backupInProgress, backupModeTone, backupOriginTone, backupStatusTone } from "./status";

export interface BackupTableProps {
  items: BackupPackage[];
  /** 正在进行校验的包标识。 */
  verifyingId?: string | null;
  onGetLink: (id: string) => void;
  onVerify: (id: string, deep: boolean) => void;
  /** 打开演练报告弹窗（含发起演练入口）。 */
  onDrill: (id: string) => void;
  onDelete: (item: BackupPackage) => void;
}

export function BackupTable({
  items,
  verifyingId,
  onGetLink,
  onVerify,
  onDrill,
  onDelete,
}: BackupTableProps) {
  const { t } = useTranslation();
  const isNarrow = useMediaQuery("(max-width: 48em)") ?? false;

  return (
    <Box style={{ width: "100%", overflowX: "auto" }}>
      <Table
        layout={isNarrow ? "auto" : "fixed"}
        highlightOnHover
        stickyHeader
        verticalSpacing="sm"
        horizontalSpacing="sm"
        fz="sm"
      >
        <Table.Thead>
          <Table.Tr>
            {/*
              列宽预算：操作列收成一个「⋯」按钮后只需约 55px，省下的空间还给
              「包标识」——它此前被挤到 168px 而内容需要 232px，尾部随机段被截断。
              其余固定列按各自内容的自然宽度取值（含单元格水平内边距 sm=24px）。
            */}
            <Table.Th w="30%">{t("backups.colPackage")}</Table.Th>
            <Table.Th w={72}>{t("backups.colOrigin")}</Table.Th>
            <Table.Th w={80}>{t("backups.colMode")}</Table.Th>
            <Table.Th w={124}>{t("backups.colStatus")}</Table.Th>
            <Table.Th w={76}>{t("backups.colSize")}</Table.Th>
            <Table.Th w={104}>{t("backups.colCounts")}</Table.Th>
            <Table.Th w={92}>{t("backups.colCreated")}</Table.Th>
            <Table.Th w={56}>{t("backups.colActions")}</Table.Th>
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
                  <Badge color={backupOriginTone(item.origin)} variant="light" tt="none">
                    {t(`backups.origin_${item.origin}`, item.origin)}
                  </Badge>
                </Table.Td>
                <Table.Td>
                  <Badge color={backupModeTone(item.mode)} variant="light" tt="none">
                    {t(`backups.mode_${item.mode}`)}
                  </Badge>
                </Table.Td>
                <Table.Td>
                  <Group gap={6} wrap="wrap">
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
                  {/*
                    全部动作收进「⋯」菜单，行内只留这一个按钮。

                    为什么不再保留行内主按钮：实测「获取链接 + 校验 + ⋯」带文字需要约
                    207px，而 1280px 屏幕下表格可用宽 984px、其余七列已占满，挤不出这个
                    空间。此前把按钮写窄会触发 flex 压缩，Mantine 的 Button-label
                    （overflow:hidden）就把文字静默压成一条缝（「校验」实测可见宽 3px），
                    表现为"按钮被裁切"；不压缩则整表横向滚动。收进菜单后操作列只需容纳
                    一个按钮，裁切与横滚两个问题同时消失，菜单项也保得住完整文字。
                  */}
                  <Menu shadow="md" position="bottom-end" withinPortal>
                    <Menu.Target>
                      <Button
                        size="compact-xs"
                        variant="subtle"
                        aria-label={t("backups.moreActions")}
                        title={t("backups.moreActions")}
                        // 校验进行中在「⋯」上显示 loading：动作已收进菜单，但"这一行正在
                        // 校验"的状态必须仍然可见，否则用户点完校验看不到任何进行中反馈。
                        loading={verifyingId === item.packageId}
                      >
                        <IconDotsVertical size={16} />
                      </Button>
                    </Menu.Target>
                    <Menu.Dropdown>
                      <Menu.Item
                        leftSection={<IconDownload size={14} />}
                        disabled={!done}
                        onClick={() => onGetLink(item.packageId)}
                      >
                        {t("backups.getLink")}
                      </Menu.Item>
                      <Menu.Item
                        leftSection={<IconLink size={14} />}
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
                      </Menu.Item>
                      <Menu.Divider />
                      <Menu.Item
                        leftSection={<IconCircleCheck size={14} />}
                        disabled={!done}
                        onClick={() => onVerify(item.packageId, false)}
                      >
                        {t("backups.verify")}
                      </Menu.Item>
                      <Menu.Item
                        leftSection={<IconReload size={14} />}
                        disabled={!done}
                        onClick={() => onVerify(item.packageId, true)}
                      >
                        {t("backups.verifyDeep")}
                      </Menu.Item>
                      <Menu.Item
                        leftSection={<IconStethoscope size={14} />}
                        disabled={!done}
                        onClick={() => onDrill(item.packageId)}
                      >
                        {t("backups.drillOpen")}
                      </Menu.Item>
                      <Menu.Divider />
                      <Menu.Item
                        color="red"
                        leftSection={<IconTrash size={14} />}
                        disabled={running}
                        onClick={() => onDelete(item)}
                      >
                        {t("backups.delete")}
                      </Menu.Item>
                    </Menu.Dropdown>
                  </Menu>
                </Table.Td>
              </Table.Tr>
            );
          })}
        </Table.Tbody>
      </Table>
    </Box>
  );
}
