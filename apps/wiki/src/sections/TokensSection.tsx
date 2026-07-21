// 设计令牌展台：可视化 packages/ui 的品牌色、圆角与间距令牌，
// 供评审核对令牌取值与管理端实际呈现一致。
import { Box, Card, Group, SimpleGrid, Stack, Text } from "@mantine/core";
import { tokens } from "@jianartifact/ui";

export function TokensSection() {
  return (
    <Stack gap="lg" data-testid="section-tokens">
      <Card withBorder padding="md">
        <Text fw={600} mb="sm">
          品牌主色
        </Text>
        <Group>
          <Box
            w={64}
            h={64}
            style={{ background: tokens.brandColor, borderRadius: tokens.radius.md }}
          />
          <Text ff="monospace">{tokens.brandColor}</Text>
        </Group>
      </Card>

      <Card withBorder padding="md">
        <Text fw={600} mb="sm">
          圆角令牌（radius）
        </Text>
        <Group>
          {(Object.keys(tokens.radius) as Array<keyof typeof tokens.radius>).map((key) => (
            <Stack key={key} align="center" gap={4}>
              <Box
                w={56}
                h={56}
                style={{ background: tokens.brandColor, borderRadius: tokens.radius[key] }}
              />
              <Text size="sm">
                {key}·{tokens.radius[key]}
              </Text>
            </Stack>
          ))}
        </Group>
      </Card>

      <Card withBorder padding="md">
        <Text fw={600} mb="sm">
          间距令牌（spacing）
        </Text>
        <SimpleGrid cols={{ base: 2, sm: 4 }}>
          {(Object.keys(tokens.spacing) as Array<keyof typeof tokens.spacing>).map((key) => (
            <Group key={key} gap="xs" align="center">
              <Box h={16} w={tokens.spacing[key]} style={{ background: tokens.brandColor }} />
              <Text size="sm">
                {key}·{tokens.spacing[key]}
              </Text>
            </Group>
          ))}
        </SimpleGrid>
      </Card>
    </Stack>
  );
}
