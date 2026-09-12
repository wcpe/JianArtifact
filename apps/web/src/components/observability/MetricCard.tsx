// 指标卡：图标 + 标签 + 主数值 + 辅助说明；危险态（失败数 > 0）主数值标红。
// v0.8.0 视觉升级：图标置于左侧主题底色块，信息层级更清晰。
import { Card, Group, Stack, Text, ThemeIcon } from "@mantine/core";
import { IconChartBar } from "@tabler/icons-react";
import type { ReactNode } from "react";

import { density } from "../../theme/density";

interface MetricCardProps {
  label: string;
  value: string;
  hint: string;
  tone?: "default" | "danger";
  /** 左侧图标（Tabler icon 元素）；缺省使用通用图表图标。 */
  icon?: ReactNode;
  /** 图标底色（Mantine color 名），默认 blue。 */
  iconColor?: string;
}

export function MetricCard({
  label,
  value,
  hint,
  tone = "default",
  icon = <IconChartBar size={18} />,
  iconColor = "blue",
}: MetricCardProps) {
  return (
    <Card withBorder radius="md" padding={density.cardPadding} h="100%">
      <Group gap="sm" wrap="nowrap" align="flex-start">
        <ThemeIcon variant="light" color={iconColor} size="lg" radius="md">
          {icon}
        </ThemeIcon>
        <Stack gap={2} miw={0}>
          <Text size="xs" c="dimmed" fw={600} tt="uppercase" lh={1.2}>
            {label}
          </Text>
          <Text
            size="xl"
            fw={700}
            lh={1.1}
            c={tone === "danger" ? "red" : undefined}
            style={{ whiteSpace: "nowrap", overflow: "hidden", textOverflow: "ellipsis" }}
          >
            {value}
          </Text>
          <Text size="xs" c="dimmed" lh={1.3}>
            {hint}
          </Text>
        </Stack>
      </Group>
    </Card>
  );
}
