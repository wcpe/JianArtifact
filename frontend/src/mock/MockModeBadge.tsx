// 运行时 Mock 模式可见标识（FR-119，ADR-0035）。
//
// Mock 模式开启时在右下角悬浮一枚醒目徽标，提醒「当前由前端内存后端模拟、非真实数据」，
// 并提供一键关闭（关闭即清开关并刷新回真实后端）。未开启时不渲染。

import { Badge, Group, ActionIcon, Tooltip } from '@mantine/core';
import { IconFlask, IconX } from '@tabler/icons-react';
import { useTranslation } from 'react-i18next';
import { isMockEnabled, setMockEnabled } from './runtime';

/** Mock 模式悬浮徽标（仅在 Mock 模式开启时渲染）。 */
export function MockModeBadge() {
  const { t } = useTranslation('mock');
  if (!isMockEnabled()) {
    return null;
  }
  return (
    <Group
      gap={6}
      style={{
        position: 'fixed',
        right: 16,
        bottom: 16,
        zIndex: 1000,
      }}
    >
      <Badge
        size="lg"
        color="orange"
        variant="filled"
        leftSection={<IconFlask size={14} />}
        title={t('hint')}
      >
        {t('label')}
      </Badge>
      <Tooltip label={t('disable')}>
        <ActionIcon
          size="sm"
          color="orange"
          variant="filled"
          aria-label={t('disable')}
          onClick={() => setMockEnabled(false)}
        >
          <IconX size={14} />
        </ActionIcon>
      </Tooltip>
    </Group>
  );
}
