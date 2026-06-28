// 统一错误提示条：展示从 API 错误提取的中文文案。

import { Alert } from '@mantine/core';
import { IconAlertCircle } from '@tabler/icons-react';
import { useTranslation } from 'react-i18next';

/** 错误提示条。 */
export function ErrorAlert({ message }: { message: string }) {
  const { t } = useTranslation('errors');
  return (
    <Alert icon={<IconAlertCircle size={16} />} color="red" variant="light" title={t('title')}>
      {message}
    </Alert>
  );
}
