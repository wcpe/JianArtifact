// 通用复制按钮：HTTP 下用 execCommand 降级，避免 navigator.clipboard 不可用。
//
// 统一为「图标 + 文字」按钮：曾经有个 `variant="icon"` 的纯图标分支，但裸图标看不出
// 复制的是什么（协议地址？文件路径？哈希？），已删除——需要更明确的场景请直接给 label。
import { Button } from "@mantine/core";
import { IconCheck, IconCopy } from "@tabler/icons-react";
import { useCallback, useState } from "react";
import { useTranslation } from "react-i18next";

import { copyToClipboard } from "../lib/clipboard";

interface Props {
  value: string;
  size?: "compact-xs" | "xs" | "sm" | "md";
  /** 按钮文案（复制前）；默认 common.copy */
  label?: string;
  /** 复制成功文案；默认 common.copied */
  copiedLabel?: string;
  timeoutMs?: number;
  "aria-label"?: string;
}

/** 带降级复制的「图标 + 文字」按钮。 */
export function CopyTextButton({
  value,
  size = "xs",
  label,
  copiedLabel,
  timeoutMs = 1500,
  "aria-label": ariaLabel,
}: Props) {
  const { t } = useTranslation();
  const [copied, setCopied] = useState(false);

  const onCopy = useCallback(() => {
    void copyToClipboard(value).then((ok) => {
      if (!ok) {
        return;
      }
      setCopied(true);
      window.setTimeout(() => setCopied(false), timeoutMs);
    });
  }, [value, timeoutMs]);

  const text = copied ? (copiedLabel ?? t("common.copied")) : (label ?? t("common.copy"));
  const a11y = ariaLabel ?? t("common.copy");

  return (
    <Button
      size={size}
      variant={copied ? "filled" : "light"}
      onClick={onCopy}
      aria-label={a11y}
      leftSection={copied ? <IconCheck size={14} /> : <IconCopy size={14} />}
    >
      {text}
    </Button>
  );
}
