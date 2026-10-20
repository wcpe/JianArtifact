// 统一事件表（FR-40 追加四）：把「本机生成的备份包」与「导入记录」按事件时间合并进同一张表。
//
// 为什么合并：这两类对象此前各占一张表——备份包在页面里、导入记录藏在「导入与传输」弹窗的
// 一个标签页里。它们回答的其实是同一个问题：「这台机器上的备份与搬迁，先后发生过什么」。
// 拆在两处只会逼用户来回对照；按 createdAt 倒序混排后，一次搬迁的时间线（打一份包 → 传到
// 那边 → 那边导入 → 待重启生效）能在一屏里顺着读下来。
//
// 混排为什么不会出现"同一件事两行"：两类对象**不相交**。备份包是本机产出的（登记在
// backup_package 表）；导入记录是把别处的包收进来（走 RestoreService.Stage 暂存并写
// restore.pending），记录里的 packageId 是包内携带的**源机器**标识，不会进入本机备份包列表。
//
// 类型（包 / 导入）由标识单元格第二行的徽章显式标注，不靠标识前缀（bk- / imp-）让人猜；
// 来源列则进一步给出 手动 / 自动 / URL / 上传 / CLI——它单独也能区分两类：
// 手动与自动只可能是本机包，URL / 上传 / CLI 只可能是导入记录。
//
// 操作列（见下方注释）：全部动作收进「⋯」菜单，行内只留一个按钮。导入记录没有可执行动作
// ——契约里没有删记录端点，记录本身就是这条流水线的账。
import { Badge, Box, Button, Group, Loader, Menu, Progress, Table, Text } from "@mantine/core";
import { useMediaQuery } from "@mantine/hooks";
import {
  IconCircleCheck,
  IconEye,
  IconRestore,
  IconDotsVertical,
  IconDownload,
  IconLink,
  IconReload,
  IconStethoscope,
  IconTrash,
} from "@tabler/icons-react";
import type { ReactNode } from "react";
import { useTranslation } from "react-i18next";

import type { BackupImport, BackupPackage } from "../../api/types";
import { copyToClipboard } from "../../lib/clipboard";
import { formatBytes, formatStamp } from "../../lib/format";
import { notifyError, notifySuccess } from "../../lib/feedback";
import { parseUtc } from "../../lib/timeFormat";
import { StatusPill } from "../ops/OpsKit";
import {
  backupInProgress,
  backupModeTone,
  backupOriginTone,
  backupStatusTone,
  importStatusTone,
} from "./status";

/** 表里的一行：要么是本机备份包，要么是一条导入记录。 */
export type BackupEvent =
  | { kind: "package"; id: string; at: string; pkg: BackupPackage }
  | { kind: "import"; id: string; at: string; imp: BackupImport };

/** 时间戳解析为毫秒：后端可能返回 SQLite naive UTC（`YYYY-MM-DD HH:MM:SS`）或 RFC3339。 */
function atMs(iso: string): number {
  return parseUtc(iso)?.getTime() ?? 0;
}

/**
 * 合并两个列表为按时间倒序的事件序列。
 *
 * 同一时刻（秒级精度下很常见）按标识降序兜底：与后端 `ORDER BY created_at DESC, id DESC`
 * 同序，且**不依赖数组到达顺序**——两个列表各自异步返回，先后不确定，若平局时沿用输入顺序，
 * 同一份数据可能两次渲染出不同次序。
 */
export function mergeBackupEvents(items: BackupPackage[], imports: BackupImport[]): BackupEvent[] {
  const events: BackupEvent[] = [
    ...items.map((pkg) => ({
      kind: "package" as const,
      id: pkg.packageId,
      at: pkg.createdAt,
      pkg,
    })),
    ...imports.map((imp) => ({
      kind: "import" as const,
      id: imp.importId,
      at: imp.createdAt,
      imp,
    })),
  ];
  return events.sort((a, b) => {
    const delta = atMs(b.at) - atMs(a.at);
    if (delta !== 0) return delta;
    return b.id < a.id ? -1 : b.id > a.id ? 1 : 0;
  });
}

export interface BackupEventTableProps {
  /** 本机备份包（后端按创建时间倒序返回）。 */
  items: BackupPackage[];
  /** 导入记录（后端按创建时间倒序返回）。 */
  imports: BackupImport[];
  /** 正在进行校验的包标识。 */
  verifyingId?: string | null;
  onGetLink: (id: string) => void;
  onVerify: (id: string, deep: boolean) => void;
  /** 「查看包内容」入口（打开预览弹窗，FR-40 追加十一）。 */
  onPreview: (packageId: string) => void;
  /** 「用这个包恢复」入口（打开确认弹窗）。 */
  onRestore: (item: BackupPackage) => void;
  /** 打开演练报告弹窗（含发起演练入口）。 */
  onDrill: (id: string) => void;
  onDelete: (item: BackupPackage) => void;
}

/** 标识单元格：等宽标识 + 第二行「类型徽章 + 摘要（备注 / 内容规模 / 来源地址）」。 */
function IdentityCell({
  id,
  badge,
  summary,
}: {
  id: string;
  badge: ReactNode;
  summary?: string | null;
}) {
  return (
    <>
      <Text ff="monospace" size="sm" truncate title={id}>
        {id}
      </Text>
      <Group gap={6} wrap="nowrap" mt={4} style={{ minWidth: 0 }}>
        {badge}
        {summary ? (
          <Text size="xs" c="dimmed" truncate title={summary} style={{ flex: 1, minWidth: 0 }}>
            {summary}
          </Text>
        ) : null}
      </Group>
    </>
  );
}

export function BackupEventTable({
  items,
  imports,
  verifyingId,
  onGetLink,
  onVerify,
  onPreview,
  onRestore,
  onDrill,
  onDelete,
}: BackupEventTableProps) {
  const { t } = useTranslation();
  const isNarrow = useMediaQuery("(max-width: 48em)") ?? false;
  const events = mergeBackupEvents(items, imports);

  /**
   * 类型徽章：outline 而非 light，是为了不被误读成状态或来源——它回答的是"这是什么"，
   * 与"它现在怎么样（状态）""它从哪来（来源）"是两个维度。用文字而非仅图标，是因为
   * 混排表里最容易混淆的恰是这两类对象。
   */
  const typeBadge = (kind: BackupEvent["kind"]) => (
    <Badge
      size="xs"
      variant="outline"
      tt="none"
      color={kind === "package" ? "blue" : "violet"}
      data-testid={`event-type-${kind}`}
    >
      {t(`backups.eventType_${kind}`)}
    </Badge>
  );

  return (
    <Box style={{ width: "100%", overflowX: "auto" }}>
      <Table
        layout={isNarrow ? "auto" : "fixed"}
        highlightOnHover
        stickyHeader
        verticalSpacing="sm"
        horizontalSpacing="sm"
        fz="sm"
        data-testid="backup-event-table"
      >
        <Table.Thead>
          <Table.Tr>
            {/*
              列宽预算（1280px 视口下表格可用宽约 984px）：

              弹性列 41%（标识 26% + 错误 15%）≈ 403px，固定列合计 568px，总计 971px，
              恰好在 984px 内不出横向滚动条。再宽一点就会顶出横向滚动，故比例不要随手调大。

              三条固定宽是按**英文文案实测**取的，不能压（含单元格水平内边距 sm=24px）：
                来源  88（`Automatic` 需 60px，旧值只剩 48px 时实测被截断）；
                方式  96（`Hot backup` 需 67px）；
                操作  92（表头 `Actions` 需 65px）；
                状态 124（`Snapshotting` 需 94px）。
              本次新增「错误」列是从「包含」列让出来的：失败原因要能就地读到（见下行），
              而"包含多少"是次要信息，已并入标识单元格第二行，信息一条没丢。
            */}
            <Table.Th w="26%">{t("backups.colIdentity")}</Table.Th>
            <Table.Th w={88}>{t("backups.colOrigin")}</Table.Th>
            <Table.Th w={96}>{t("backups.colMode")}</Table.Th>
            <Table.Th w={124}>{t("backups.colStatus")}</Table.Th>
            <Table.Th w={76}>{t("backups.colSize")}</Table.Th>
            <Table.Th w={92}>{t("backups.colCreated")}</Table.Th>
            <Table.Th w="15%">{t("backups.colError")}</Table.Th>
            <Table.Th w={92}>{t("backups.colActions")}</Table.Th>
          </Table.Tr>
        </Table.Thead>
        <Table.Tbody>
          {events.map((ev) => {
            // —— 本机备份包 ——
            if (ev.kind === "package") {
              const item = ev.pkg;
              const done = item.status === "done";
              const running = backupInProgress(item.status);
              // 内容规模并入标识单元格第二行（原「包含」列）。若包带了备注，两者并列；
              // 备注是用户自己写的标识，优先展示，规模紧随其后。
              const counts = item.counts
                ? t("backups.countsSummary", {
                    assets: item.counts.assets ?? 0,
                    repos: item.counts.repositories ?? 0,
                  })
                : null;
              const summary = [item.label, counts].filter(Boolean).join(" · ");
              return (
                <Table.Tr key={`package:${item.packageId}`} data-row-kind="package">
                  <Table.Td>
                    <IdentityCell
                      id={item.packageId}
                      badge={typeBadge("package")}
                      summary={summary}
                    />
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
                  </Table.Td>
                  <Table.Td>
                    <Text size="sm">{done ? formatBytes(item.sizeBytes) : "—"}</Text>
                  </Table.Td>
                  <Table.Td>
                    <Text size="sm" c="dimmed">
                      {formatStamp(item.createdAt)}
                    </Text>
                  </Table.Td>
                  <Table.Td>
                    {item.status === "failed" && item.errorSummary ? (
                      <Text size="xs" c="red" truncate title={item.errorSummary}>
                        {item.errorSummary}
                      </Text>
                    ) : (
                      <Text size="xs" c="dimmed">
                        —
                      </Text>
                    )}
                  </Table.Td>
                  <Table.Td>
                    {/*
                      全部动作收进「⋯」菜单，行内只留这一个按钮。

                      为什么不再保留行内主按钮：实测「获取链接 + 校验 + ⋯」带文字需要约
                      207px，而 1280px 屏幕下表格可用宽 984px、其余列已占满，挤不出这个
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
                        {/* 查看内容排在最前：它是"要不要用这个包恢复"的第一手依据。
                            此前只能看到抽象计数（多少制品），用户无从判断里面是什么，
                            恢复因此变成一次赌博——而恢复是不可撤销的。 */}
                        <Menu.Item
                          leftSection={<IconEye size={14} />}
                          disabled={!done}
                          title={t("backups.previewHint")}
                          onClick={() => onPreview(item.packageId)}
                          data-testid={`preview-${item.packageId}`}
                        >
                          {t("backups.previewOpen")}
                        </Menu.Item>
                        {/* 三项都带 title：菜单项文字只有几个字，用户看不出"校验 / 深度校验 /
                            演练"的分工与代价（快的只验结构、慢的逐字节核对、演练还多一步真解包）。
                            悬停即知用途，不必点开试错。 */}
                        <Menu.Item
                          leftSection={<IconCircleCheck size={14} />}
                          disabled={!done}
                          title={t("backups.verifyHint")}
                          onClick={() => onVerify(item.packageId, false)}
                        >
                          {t("backups.verify")}
                        </Menu.Item>
                        <Menu.Item
                          leftSection={<IconReload size={14} />}
                          disabled={!done}
                          title={t("backups.verifyDeepHint")}
                          onClick={() => onVerify(item.packageId, true)}
                        >
                          {t("backups.verifyDeep")}
                        </Menu.Item>
                        <Menu.Item
                          leftSection={<IconStethoscope size={14} />}
                          disabled={!done}
                          title={t("backups.drillOpenHint")}
                          onClick={() => onDrill(item.packageId)}
                        >
                          {t("backups.drillOpen")}
                        </Menu.Item>
                        <Menu.Divider />
                        {/* 用这个包恢复：补上"页面上明明有包却没法用它恢复"的缺口。
                            放在校验/演练之后、删除之前——它是最"重"的动作（会替换本机数据），
                            顺序上贴合"先验证再恢复，最后才是删除"。 */}
                        <Menu.Item
                          leftSection={<IconRestore size={14} />}
                          disabled={!done}
                          title={t("backups.restoreHint")}
                          onClick={() => onRestore(item)}
                        >
                          {t("backups.restoreFromPackage")}
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
            }

            // —— 导入记录 ——
            const item = ev.imp;
            const total = item.totalBytes ?? 0;
            const fetched = item.fetchedBytes ?? 0;
            const ratio = total > 0 ? Math.min(100, (fetched / total) * 100) : 0;
            const errorSummary = item.errorCode
              ? t(`backups.backupImportError.${item.errorCode}`, { defaultValue: item.error ?? "" })
              : item.error;
            return (
              <Table.Tr key={`import:${item.importId}`} data-row-kind="import">
                <Table.Td>
                  {/* 非 URL 通道（上传 / CLI）没有来源地址，第二行只剩类型徽章。 */}
                  <IdentityCell
                    id={item.importId}
                    badge={typeBadge("import")}
                    summary={item.sourceUrl}
                  />
                </Table.Td>
                <Table.Td>
                  <Badge variant="light" tt="none" color="violet">
                    {t(`backups.import_origin_${item.origin}`)}
                  </Badge>
                </Table.Td>
                <Table.Td>
                  <Text size="xs" c="dimmed">
                    —
                  </Text>
                </Table.Td>
                <Table.Td>
                  {/*
                    进度与「需重启」提示都落在状态单元格内（原导入记录表有独立的进度列，
                    合并后没有这一列）：它们都是状态的补充说明，跟着状态徽章走才不会被误读。
                  */}
                  <StatusPill
                    tone={importStatusTone(item.status)}
                    text={t(`backups.import_status_${item.status}`)}
                  />
                  {item.status === "fetching" ? (
                    <Progress value={ratio} size="sm" mt={4} />
                  ) : item.status === "pending_restart" ? (
                    <Text size="xs" c="orange" mt={2}>
                      {t("backups.importPendingRestartHint")}
                    </Text>
                  ) : null}
                </Table.Td>
                <Table.Td>
                  <Text size="sm">{total > 0 ? formatBytes(total) : "—"}</Text>
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
                <Table.Td>
                  {/* 导入记录没有可执行动作：契约里没有删除端点，记录本身是这条流水线的账。 */}
                  <Text size="xs" c="dimmed">
                    —
                  </Text>
                </Table.Td>
              </Table.Tr>
            );
          })}
        </Table.Tbody>
      </Table>
    </Box>
  );
}
