// 通知辅助：成功 / 失败的统一提示封装。

import { notifications } from '@mantine/notifications';

/** 成功提示。 */
export function notifySuccess(message: string): void {
  notifications.show({ color: 'green', message });
}

/** 失败提示。 */
export function notifyError(message: string): void {
  notifications.show({ color: 'red', title: '操作失败', message });
}
