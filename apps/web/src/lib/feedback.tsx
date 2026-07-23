// 通知与确认弹窗的集中封装：统一列表 / 表单页的成功、失败反馈与危险操作二次确认，
// 避免各页重复样板并保持交互一致。
import { Text } from "@mantine/core";
import { modals } from "@mantine/modals";
import { notifications } from "@mantine/notifications";

import { ApiError } from "../api/client";

/** 失败提示：ApiError 取其后端消息，其余转字符串。 */
export function notifyError(err: unknown): void {
  notifications.show({
    color: "red",
    message: err instanceof ApiError ? err.message : String(err),
  });
}

/** 成功提示（绿色）。 */
export function notifySuccess(message: string): void {
  notifications.show({ color: "green", message });
}

export interface ConfirmOptions {
  title: string;
  message: string;
  confirmLabel: string;
  cancelLabel: string;
  onConfirm: () => void;
}

/**
 * 危险操作二次确认弹窗：确认按钮红色，替代原生 window.confirm，
 * 与 Mantine 主题 / 暗色一致，且可本地化按钮文案。
 */
export function confirmDanger({
  title,
  message,
  confirmLabel,
  cancelLabel,
  onConfirm,
}: ConfirmOptions): void {
  modals.openConfirmModal({
    title,
    centered: true,
    children: <Text size="sm">{message}</Text>,
    labels: { confirm: confirmLabel, cancel: cancelLabel },
    confirmProps: { color: "red" },
    onConfirm,
  });
}
