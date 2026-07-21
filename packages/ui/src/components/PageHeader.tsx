// 页头：标题 + 可选描述 + 右侧动作区。管理端各页面统一顶部结构。
import { Group, Stack, Text, Title } from "@mantine/core";
import type { ReactNode } from "react";

export interface PageHeaderProps {
  title: ReactNode;
  description?: ReactNode;
  /** 右侧动作（如“新建”按钮）。 */
  actions?: ReactNode;
}

/** 统一页头结构，左侧标题/描述、右侧动作。 */
export function PageHeader({ title, description, actions }: PageHeaderProps) {
  return (
    <Group justify="space-between" align="flex-start" mb="md" wrap="nowrap">
      <Stack gap={2}>
        <Title order={2}>{title}</Title>
        {description ? (
          <Text c="dimmed" size="sm">
            {description}
          </Text>
        ) : null}
      </Stack>
      {actions ? <Group gap="xs">{actions}</Group> : null}
    </Group>
  );
}
