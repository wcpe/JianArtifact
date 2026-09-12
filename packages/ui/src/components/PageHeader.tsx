// 页头：可选面包屑 + 标题 + 可选描述 + 右侧动作区。管理端各页面统一顶部结构。
import { Breadcrumbs, Group, Stack, Text, Title } from "@mantine/core";
import type { ReactNode } from "react";

export interface PageHeaderProps {
  /** 页面标题；与 breadcrumbs 二选一或并存（面包屑在上、标题在下）。 */
  title?: ReactNode;
  description?: ReactNode;
  /** 右侧动作（如“新建”按钮）。 */
  actions?: ReactNode;
  /** 面包屑（可选）：渲染在标题上方，表达页面在导航中的层级位置。 */
  breadcrumbs?: ReactNode[];
}

/** 统一页头结构，左侧面包屑/标题/描述、右侧动作。 */
export function PageHeader({ title, description, actions, breadcrumbs }: PageHeaderProps) {
  return (
    <Group justify="space-between" align="flex-start" mb="md" wrap="nowrap">
      <Stack gap={2}>
        {breadcrumbs && breadcrumbs.length > 0 ? (
          <Breadcrumbs separator="/" mb={title ? 2 : 0}>
            {breadcrumbs.map((crumb, index) => (
              <Text key={index} size="xs" c="dimmed">
                {crumb}
              </Text>
            ))}
          </Breadcrumbs>
        ) : null}
        {title ? <Title order={2}>{title}</Title> : null}
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
