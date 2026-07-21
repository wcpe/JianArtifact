// 业务状态态组件：加载 / 空 / 错误 / 越权。管理端各视图与 wiki 验收站复用同一套状态呈现，
// 保证空态与权限态在全站一致（对齐 FR-12 业务模式验收）。
import { Button, Center, Loader, Stack, Text, ThemeIcon } from "@mantine/core";
import type { ReactNode } from "react";

interface BaseStateProps {
  /** 主文案。 */
  message: ReactNode;
  /** 辅助说明。 */
  description?: ReactNode;
}

/** 加载中：居中转圈 + 文案。 */
export function LoadingState({ message = "加载中…" }: { message?: ReactNode }) {
  return (
    <Center py="xl" data-testid="state-loading">
      <Stack align="center" gap="xs">
        <Loader />
        <Text c="dimmed">{message}</Text>
      </Stack>
    </Center>
  );
}

/** 空态：无数据时的占位与引导。 */
export function EmptyState({ message, description }: BaseStateProps) {
  return (
    <Center py="xl" data-testid="state-empty">
      <Stack align="center" gap={4}>
        <Text fw={600}>{message}</Text>
        {description ? (
          <Text c="dimmed" size="sm">
            {description}
          </Text>
        ) : null}
      </Stack>
    </Center>
  );
}

/** 错误态：展示错误文案，可选重试动作。 */
export function ErrorState({
  message,
  description,
  onRetry,
  retryLabel = "重试",
}: BaseStateProps & { onRetry?: () => void; retryLabel?: string }) {
  return (
    <Center py="xl" data-testid="state-error">
      <Stack align="center" gap="xs">
        <ThemeIcon color="red" variant="light" size="lg" radius="xl">
          !
        </ThemeIcon>
        <Text fw={600}>{message}</Text>
        {description ? (
          <Text c="dimmed" size="sm">
            {description}
          </Text>
        ) : null}
        {onRetry ? (
          <Button variant="light" size="xs" onClick={onRetry}>
            {retryLabel}
          </Button>
        ) : null}
      </Stack>
    </Center>
  );
}

/** 越权态：ACL / 角色不足时的统一提示（后端仍是授权真源）。 */
export function ForbiddenState({ message = "无访问权限", description }: Partial<BaseStateProps>) {
  return (
    <Center py="xl" data-testid="state-forbidden">
      <Stack align="center" gap={4}>
        <ThemeIcon color="yellow" variant="light" size="lg" radius="xl">
          ⛔
        </ThemeIcon>
        <Text fw={600}>{message}</Text>
        {description ? (
          <Text c="dimmed" size="sm">
            {description}
          </Text>
        ) : null}
      </Stack>
    </Center>
  );
}
