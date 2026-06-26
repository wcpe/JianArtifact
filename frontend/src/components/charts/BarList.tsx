// 手搓横向条形列表（FR-99）：纯 CSS（宽度百分比），零依赖。
// 各项按最大值归一展示横条 + 右侧数值；空数据走空态文案。用于热门制品下载量等对比。

import { Stack, Group, Text, Box } from '@mantine/core';

/** 单条数据项。 */
export interface BarItem {
  /** 项标签。 */
  label: string;
  /** 项数值（用于横条长度与右侧展示）。 */
  value: number;
}

/** 条形列表属性。 */
interface BarListProps {
  items: BarItem[];
  /** 空数据时的占位文案。 */
  emptyText: string;
}

/** 横向条形列表。 */
export function BarList({ items, emptyText }: BarListProps) {
  if (items.length === 0) {
    return (
      <Text c="dimmed" size="sm">
        {emptyText}
      </Text>
    );
  }
  // 按最大值归一，便于横向对比各项占比
  const max = items.reduce((m, it) => Math.max(m, it.value), 0);
  return (
    <Stack gap="xs">
      {items.map((it) => (
        <div key={it.label}>
          <Group justify="space-between" mb={2}>
            <Text size="sm" style={{ wordBreak: 'break-all' }}>
              {it.label}
            </Text>
            <Text size="sm" c="dimmed">
              {it.value}
            </Text>
          </Group>
          {/* 横条：外层灰轨 + 内层按占比填充主题色 */}
          <Box
            style={{
              height: 8,
              borderRadius: 4,
              backgroundColor: 'var(--mantine-color-gray-3)',
              overflow: 'hidden',
            }}
          >
            <Box
              style={{
                height: '100%',
                width: `${max > 0 ? (it.value / max) * 100 : 0}%`,
                backgroundColor: 'var(--mantine-primary-color-filled)',
              }}
            />
          </Box>
        </div>
      ))}
    </Stack>
  );
}
