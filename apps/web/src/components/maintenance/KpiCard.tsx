// 存储体检的 KPI 卡片：图标 + 标签 + 数据。
//
// 早先是六个「标签/数值」纯文本块堆在一张 Card 里，视觉上没有主次、也不便一眼扫读；
// 现在每个指标一张卡，图标提供形状记忆（比读文字快），数值用大字号形成层级。
//
// 一条注意：数值为 0 的指标不隐藏、也不弱化成灰——"孤儿 blob 为 0"本身就是
// 有价值的结论（说明没有可回收垃圾），隐藏它反而让人怀疑"是不是没测"。
import { Card, Group, Stack, Text, ThemeIcon } from "@mantine/core";
import type { ReactNode } from "react";
import { density } from "../../theme/density";

export interface KpiCardProps {
  /** 图标节点（由调用方传入，避免本组件绑定具体图标库版本）。 */
  icon: ReactNode;
  label: string;
  value: string;
  /** 语义色：正常用 blue，需要关注用 orange/red。 */
  color?: string;
  /** 次要说明，如"其中 3 个待回收"。 */
  hint?: string;
  testId?: string;
}

export function KpiCard({ icon, label, value, color = "blue", hint, testId }: KpiCardProps) {
  return (
    <Card withBorder radius="md" padding={density.cardPadding} data-testid={testId}>
      <Group gap="sm" wrap="nowrap" align="flex-start">
        <ThemeIcon variant="light" color={color} size={38} radius="md">
          {icon}
        </ThemeIcon>
        <Stack gap={2} style={{ minWidth: 0, flex: 1 }}>
          <Text size="xs" c="dimmed" truncate>
            {label}
          </Text>
          <Text size="xl" fw={700} lh={1.1} truncate>
            {value}
          </Text>
          {hint ? (
            <Text size="xs" c="dimmed" truncate>
              {hint}
            </Text>
          ) : null}
        </Stack>
      </Group>
    </Card>
  );
}
