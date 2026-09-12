// 业务仪表盘开发预览：固定数据仅在开发态按需加载，生产不携带这些样例。
import { Alert, Card, Group, SimpleGrid, Stack } from "@mantine/core";
import { IconCircleCheck } from "@tabler/icons-react";
import { useState } from "react";

import { dashboardPreview, type PreviewRange } from "../../mocks/observabilityPreview";
import { density } from "../../theme/density";
import { DevelopmentPreviewState, useDevelopmentPreviewScenario } from "./DevelopmentPreviewState";
import { MetricCard } from "./MetricCard";
import { PreviewNotice } from "./PreviewNotice";
import { PreviewRangeControls } from "./PreviewRangeControls";
import { TrendChart } from "./TrendChart";

function DashboardPreviewContent() {
  const [range, setRange] = useState<PreviewRange>("24h");
  const preview = dashboardPreview[range];

  return (
    <Stack gap={density.gridSpacing}>
      <PreviewNotice />
      <Group justify="flex-end">
        <PreviewRangeControls value={range} onChange={setRange} />
      </Group>
      <SimpleGrid cols={{ base: 1, xs: 2, lg: 4 }} spacing={density.gridSpacing}>
        {preview.metrics.map((metric) => (
          <MetricCard key={metric.label} {...metric} />
        ))}
      </SimpleGrid>
      <SimpleGrid cols={{ base: 1, md: 2 }} spacing={density.gridSpacing}>
        <Card withBorder radius="md" padding={density.cardPadding}>
          <TrendChart
            title="请求与下载趋势"
            summary={preview.requestSummary}
            primary={preview.requests}
            secondary={preview.downloads}
            primaryLabel="请求"
            secondaryLabel="下载"
          />
        </Card>
        <Card withBorder radius="md" padding={density.cardPadding}>
          <TrendChart
            title="失败请求趋势"
            summary={preview.failureSummary}
            primary={preview.failures}
            primaryLabel="失败请求"
          />
        </Card>
      </SimpleGrid>
      <Card withBorder radius="md" padding={density.cardPadding}>
        <TrendChart
          title="容量增长趋势"
          summary={preview.capacitySummary}
          primary={preview.capacity}
          primaryLabel="逻辑制品体积"
        />
      </Card>
      <Alert
        variant="light"
        color="green"
        title="当前无需要处理的站内告警"
        icon={<IconCircleCheck size={18} />}
      >
        预览场景中实例就绪、当前节点同步正常，且没有上游自动阻止状态。
      </Alert>
    </Stack>
  );
}

export function DashboardPreview() {
  const { scenario, recover } = useDevelopmentPreviewScenario();
  return (
    <DevelopmentPreviewState subject="业务仪表盘" scenario={scenario} onRetry={recover}>
      <DashboardPreviewContent />
    </DevelopmentPreviewState>
  );
}
