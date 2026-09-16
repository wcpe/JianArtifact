// FR-70：路由级代码分割的 Suspense 占位。
//
// 它出现在**切换页面时的 chunk 加载窗口**内，因此必须是「页面框架的形状」，而不是一个
// 居中转圈：慢网速下居中转圈会让内容区看起来是空的，用户会以为「点了导航切不过去」。
// 这里用「页头一行 + 内容若干行」的结构，与列表页 / 工作台页整体近似，
// 切换时视觉上是「骨架 → 内容」，而不是「空白 → 内容」。
import { Group, Skeleton, Stack } from "@mantine/core";

export function RouteFallback() {
  return (
    <Stack gap="sm" data-testid="route-fallback" aria-busy="true" aria-label="页面加载中">
      <Group justify="space-between" wrap="nowrap">
        <Skeleton height={28} width={200} radius="sm" />
        <Skeleton height={28} width={128} radius="sm" />
      </Group>
      <Skeleton height={44} radius="md" />
      <Stack gap="xs">
        {Array.from({ length: 6 }).map((_, index) => (
          <Skeleton key={index} height={40} radius="sm" />
        ))}
      </Stack>
    </Stack>
  );
}
