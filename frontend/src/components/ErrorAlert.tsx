// 统一错误提示条：展示从 API 错误提取的中文文案。

import { Alert } from '@mantine/core';
import { IconAlertCircle } from '@tabler/icons-react';

/** 错误提示条。 */
export function ErrorAlert({ message }: { message: string }) {
  return (
    <Alert icon={<IconAlertCircle size={16} />} color="red" variant="light" title="出错了">
      {message}
    </Alert>
  );
}
