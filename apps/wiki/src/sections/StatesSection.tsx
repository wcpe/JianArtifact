// 状态态展台：并列展示加载 / 空 / 错误 / 越权四类业务状态态，
// 核验它们在验收站与管理端呈现一致（对齐 FR-12 业务模式验收）。
import { Card, SimpleGrid, Stack, Text } from "@mantine/core";
import { EmptyState, ErrorState, ForbiddenState, LoadingState } from "@jianartifact/ui";
import type { ReactNode } from "react";

function StateCard({ title, children }: { title: string; children: ReactNode }) {
  return (
    <Card withBorder padding="md">
      <Text fw={600} mb="sm">
        {title}
      </Text>
      {children}
    </Card>
  );
}

export function StatesSection() {
  return (
    <Stack gap="lg" data-testid="section-states">
      <SimpleGrid cols={{ base: 1, md: 2 }}>
        <StateCard title="加载态 LoadingState">
          <LoadingState message="正在拉取仓库列表…" />
        </StateCard>
        <StateCard title="空态 EmptyState">
          <EmptyState message="暂无仓库" description="点击右上角“新建仓库”创建首个仓库。" />
        </StateCard>
        <StateCard title="错误态 ErrorState（可重试）">
          <ErrorState
            message="加载失败"
            description="服务暂时不可用，请稍后重试。"
            onRetry={() => undefined}
          />
        </StateCard>
        <StateCard title="越权态 ForbiddenState">
          <ForbiddenState description="当前账号无权访问该仓库，请联系管理员授予 ACL。" />
        </StateCard>
      </SimpleGrid>
    </Stack>
  );
}
