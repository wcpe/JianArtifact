// 通用复制按钮：HTTP 下用 execCommand 降级，避免 navigator.clipboard 不可用。
import { ActionIcon, Button, Tooltip } from "@mantine/core";
import { IconCheck, IconCopy } from "@tabler/icons-react";
import { useCallback, useState } from "react";
import { useTranslation } from "react-i18next";

import { copyToClipboard } from "../lib/clipboard";

type Variant = "button" | "icon";

interface Props {
  value: string;
  /** button：文字按钮；icon：仅图标（列表 URL 等） */
  variant?: Variant;
  size?: "xs" | "sm" | "md";
  /** 按钮文案（复制前）；默认 common.copy */
  label?: string;
  /** 复制成功文案；默认 common.copied */
  copiedLabel?: string;
  timeoutMs?: number;
  "aria-label"?: string;
}

/** 带降级复制的按钮 / 图标按钮。 */
export function CopyTextButton({
  value,
  variant = "button",
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

  const text = copied
    ? (copiedLabel ?? t("common.copied"))
    : (label ?? t("common.copy"));
  const a11y = ariaLabel ?? t("common.copy");

  if (variant === "icon") {
    return (
      <Tooltip label={text} withArrow>
        <ActionIcon
          size={size === "xs" ? "sm" : size}
          variant="subtle"
          color={copied ? "teal" : "gray"}
          onClick={onCopy}
          aria-label={a11y}
        >
          {copied ? <IconCheck size={14} /> : <IconCopy size={14} />}
        </ActionIcon>
      </Tooltip>
    );
  }

  return (
    <Button
      size={size}
      variant={copied ? "filled" : "light"}
      onClick={onCopy}
      aria-label={a11y}
      leftSection={
        copied ? <IconCheck size={14} /> : <IconCopy size={14} />
      }
    >
      {text}
    </Button>
  );
}
