// 页头展台：核验 PageHeader 的标题 / 描述 / 右侧动作三态布局，
// 与管理端各页面顶部结构保持一致。
import { Button, Card, Stack } from "@mantine/core";
import { PageHeader } from "@jianartifact/ui";

export function PageHeaderSection() {
  return (
    <Stack gap="lg" data-testid="section-page-header">
      <Card withBorder padding="md">
        <PageHeader
          title="仓库"
          description="管理 hosted / proxy / group 仓库与可见性。"
          actions={<Button size="xs">新建仓库</Button>}
        />
      </Card>
      <Card withBorder padding="md">
        <PageHeader title="仪表盘" description="实例运行时状态概览。" />
      </Card>
      <Card withBorder padding="md">
        <PageHeader title="仅标题（无描述无动作）" />
      </Card>
    </Stack>
  );
}
