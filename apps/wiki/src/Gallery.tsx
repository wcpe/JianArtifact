// 验收站画廊：AppShell 布局，左侧导航切换展台，右侧渲染当前展台内容。
// 以受控 state 切换分区，无需路由依赖，保持验收站轻量。
import { useState } from "react";
import { AppShell, Badge, Group, NavLink, ScrollArea, Stack, Text, Title } from "@mantine/core";
import { PageHeader } from "@jianartifact/ui";

import { sections } from "./sections";

export function Gallery() {
  const [activeId, setActiveId] = useState(sections[0]?.id ?? "");
  const active = sections.find((section) => section.id === activeId) ?? sections[0];
  if (!active) {
    throw new Error("验收站未注册任何展台");
  }
  const ActiveComponent = active.component;

  return (
    <AppShell
      header={{ height: 56 }}
      navbar={{ width: 240, breakpoint: "sm" }}
      padding="md"
    >
      <AppShell.Header>
        <Group h="100%" px="md" justify="space-between">
          <Title order={4}>JianArtifact 验收站</Title>
          <Badge variant="light">UI 组件 / 业务模式</Badge>
        </Group>
      </AppShell.Header>

      <AppShell.Navbar p="xs">
        <ScrollArea>
          <Stack gap={4}>
            {sections.map((section) => (
              <NavLink
                key={section.id}
                active={section.id === activeId}
                label={section.label}
                description={section.description}
                onClick={() => setActiveId(section.id)}
              />
            ))}
          </Stack>
        </ScrollArea>
      </AppShell.Navbar>

      <AppShell.Main>
        <PageHeader title={active.label} description={active.description} />
        <ActiveComponent />
        <Text c="dimmed" size="xs" mt="xl">
          验收站脱离后端运行，仅核验共享 UI 组件与业务交互模式，非真实数据。
        </Text>
      </AppShell.Main>
    </AppShell>
  );
}
