// 内联导入区：从 URL 导入与分片上传两个表单直接展开在页面里，不再进弹窗。
//
// 为什么不再用弹窗：弹窗把「导入记录」和「发起导入」隔成了两层——记录在弹窗的标签页里、
// 备份包在页面里，用户要对照一次搬迁的进度就得来回开关弹窗。现在三者同屏：
// 上面的按钮发起导入、中间展开表单、下面的统一事件表立刻多出一行，按事件顺序直接读。
//
// 为什么必须常驻挂载（隐藏只置 display:none，不能条件卸载）：
// 分片上传面板要在挂载时探测**上次未完成的上传会话**（localStorage 里的 uploadId → GET
// 拉回 uploadedChunks），条件卸载会让探测永远跑不到，续传提示与"只补缺失片"全部失效。
// 这与它此前在 Collapse / 弹窗里踩过的坑是同一个：父级一旦真卸载，子树的副作用就停摆。
//
// 上传进行中禁止收起：进度条与取消按钮都在面板里，收起来等于把唯一的进度反馈藏掉。
// 注意「收起」有**两条路径**（本组件自己的收起按钮、动作条上那个已展开的通道按钮），
// 因此 busy 状态经 onBusyChange 上报给页面，由页面把两个通道按钮一起置灰——只在本组件内
// 挡住收起按钮是不够的，动作条仍能把面板收掉（该漏洞已由用例守住，见 BackupsUpload）。
import { ActionIcon, Group, Paper, Text, ThemeIcon } from "@mantine/core";
import {
  IconChevronUp,
  IconDatabaseImport,
  IconUpload,
  IconWorldDownload,
} from "@tabler/icons-react";
import { useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";

import { BackupUploadPanel } from "./BackupUploadPanel";
import { UnmanagedBackupPanel } from "./UnmanagedBackupPanel";
import { UrlImportPanel } from "./UrlImportPanel";

/** 两个导入通道；null 表示整块收起。 */
export type ImportChannel = "url" | "upload" | "adopt";

export function ImportInline({
  channel,
  onChannelChange,
  onBusyChange,
}: {
  /** 当前展开的通道；null = 收起。 */
  channel: ImportChannel | null;
  /** 收起：调用方需在上传进行中忽略该请求（见文件头）。 */
  onChannelChange: (next: ImportChannel | null) => void;
  /** 上传进行中状态上报，供调用方同时管住动作条那条收起路径。 */
  onBusyChange?: (busy: boolean) => void;
}) {
  const { t } = useTranslation();
  /** 上传进行中：此时不允许收起面板。 */
  const [busy, setBusy] = useState(false);

  const opened = channel !== null;

  // 状态变化才上报：调用方传进来的通常是 setState，重复上报会让父组件做无谓的渲染。
  // 用 ref 持有最新回调（内联箭头函数每次渲染都是新引用，放进依赖会导致无限循环）。
  const onBusyChangeRef = useRef(onBusyChange);
  onBusyChangeRef.current = onBusyChange;
  useEffect(() => {
    onBusyChangeRef.current?.(busy);
  }, [busy]);

  return (
    <Paper
      withBorder
      radius="md"
      p="md"
      data-testid="import-inline"
      // 收起时保留挂载、仅隐藏：见文件头，上传续传探测依赖子树持续挂载。
      // 这里不用 Collapse 也不条件渲染，避免任何"父级停掉子树副作用"的可能。
      style={opened ? undefined : { display: "none" }}
    >
      <Group justify="space-between" align="center" wrap="nowrap" mb="sm">
        <Group gap="xs" wrap="nowrap">
          <ThemeIcon size={22} radius="sm" variant="light" color="blue">
            {channel ? channelIcon(channel) : null}
          </ThemeIcon>
          <Text fw={700} size="sm">
            {channel ? channelTitle(channel, t) : null}
          </Text>
        </Group>
        <ActionIcon
          variant="subtle"
          size="sm"
          aria-label={t("backups.importInlineCollapse")}
          title={busy ? t("backups.importInlineBusyHint") : t("backups.importInlineCollapse")}
          disabled={busy}
          onClick={() => onChannelChange(null)}
        >
          <IconChevronUp size={16} />
        </ActionIcon>
      </Group>

      {/*
        两个通道都常驻挂载、按需显示（见文件头）。不条件渲染的额外好处：切到另一个通道
        再切回来，已填的 URL / 校验摘要都还在，不会被清空。
      */}
      <div
        style={channel === "url" ? undefined : { display: "none" }}
        data-testid="import-channel-url"
      >
        {/* 提交成功后的刷新走全局 REFRESH_EVENT（useAsync 监听），此处不另设回调——
            否则同一件事有两个刷新通道，容易出现一个刷了另一个没刷的错位。 */}
        <UrlImportPanel />
      </div>
      <div
        style={channel === "upload" ? undefined : { display: "none" }}
        data-testid="import-channel-upload"
      >
        {/*
          上传完成后的刷新走全局 REFRESH_EVENT（useAsync 监听），此处不另设回调——
          否则同一件事有两个刷新通道，容易出现一个刷了另一个没刷的错位。
        */}
        <BackupUploadPanel onBusyChange={setBusy} />
      </div>
      <div
        style={channel === "adopt" ? undefined : { display: "none" }}
        data-testid="import-channel-adopt"
      >
        {/* 未管理包导入：成功后面板自己派发 REFRESH_EVENT，与其它通道一致。 */}
        <UnmanagedBackupPanel onDone={() => onChannelChange(null)} />
      </div>
    </Paper>
  );
}

/** 通道图标：三个通道各用一眼可辨的图形。 */
function channelIcon(channel: ImportChannel) {
  switch (channel) {
    case "upload":
      return <IconUpload size={14} />;
    case "adopt":
      return <IconDatabaseImport size={14} />;
    default:
      return <IconWorldDownload size={14} />;
  }
}

/** 通道标题：与动作条上的按钮文案保持同一 key，避免两处措辞漂移。 */
function channelTitle(channel: ImportChannel, t: (key: string) => string) {
  switch (channel) {
    case "upload":
      return t("backups.uploadButton");
    case "adopt":
      return t("backups.adoptButton");
    default:
      return t("backups.importButton");
  }
}
