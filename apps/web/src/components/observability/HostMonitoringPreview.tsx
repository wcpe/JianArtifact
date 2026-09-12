// 当前主机开发预览：只呈现本机固定指标和状态，不包含任何远端节点信息。
import {
  Alert,
  Badge,
  Button,
  Card,
  Group,
  Progress,
  SimpleGrid,
  Stack,
  Text,
  ThemeIcon,
  Title,
} from "@mantine/core";
import {
  IconAlertTriangle,
  IconCircleCheck,
  IconCpu,
  IconDatabase,
  IconNetworkOff,
  IconServer,
} from "@tabler/icons-react";
import { useState } from "react";

import { density } from "../../theme/density";
import { DevelopmentPreviewState, useDevelopmentPreviewScenario } from "./DevelopmentPreviewState";
import { PreviewNotice } from "./PreviewNotice";

type Range = "24h" | "7d" | "30d";
type MetricState = "healthy" | "warning" | "unknown" | "error";

interface MetricPreview {
  title: string;
  value: string;
  detail: string;
  state: MetricState;
  progress?: number;
}

const rangeLabels: Record<Range, string> = {
  "24h": "24 小时",
  "7d": "7 天",
  "30d": "30 天",
};

const rangeSummaries: Record<Range, string> = {
  "24h": "过去 24 小时 CPU 平均 41%，未出现持续过载。",
  "7d": "过去 7 天 CPU 平均 39%，晚间批处理存在短时峰值。",
  "30d": "过去 30 天 CPU 平均 37%，整体趋势稳定。",
};

const metrics: MetricPreview[] = [
  {
    title: "CPU 使用率",
    value: "43%",
    detail: "1 分钟平均负载 1.24",
    state: "healthy",
    progress: 43,
  },
  {
    title: "内存使用率",
    value: "62%",
    detail: "已用 9.8 GB / 15.8 GB",
    state: "healthy",
    progress: 62,
  },
  { title: "数据目录", value: "78%", detail: "容量接近关注阈值", state: "warning", progress: 78 },
  { title: "网络流量", value: "暂不可用", detail: "未收集到上一个有效采样点", state: "unknown" },
  {
    title: "文件句柄",
    value: "采样权限不足",
    detail: "系统未授予读取此指标的权限",
    state: "error",
  },
];

const stateMeta: Record<MetricState, { label: string; color: string }> = {
  healthy: { label: "健康", color: "teal" },
  warning: { label: "需要关注", color: "yellow" },
  unknown: { label: "暂不可用", color: "gray" },
  error: { label: "采样异常", color: "red" },
};

function MetricIcon({ title }: { title: string }) {
  if (title === "CPU 使用率") return <IconCpu size={18} />;
  if (title === "数据目录") return <IconDatabase size={18} />;
  if (title === "网络流量") return <IconNetworkOff size={18} />;
  return <IconAlertTriangle size={18} />;
}

function MetricCard({ metric }: { metric: MetricPreview }) {
  const meta = stateMeta[metric.state];
  return (
    <Card withBorder radius="md" padding={density.cardPadding}>
      <Group justify="space-between" align="flex-start" wrap="nowrap">
        <Group gap="xs" wrap="nowrap">
          <ThemeIcon variant="light" color={meta.color} radius="md">
            <MetricIcon title={metric.title} />
          </ThemeIcon>
          <Text size="sm" c="dimmed">
            {metric.title}
          </Text>
        </Group>
        <Badge color={meta.color} variant="light">
          {meta.label}
        </Badge>
      </Group>
      <Text fw={700} size="xl" mt="md">
        {metric.value}
      </Text>
      <Text size="xs" c="dimmed" mt={4}>
        {metric.detail}
      </Text>
      {metric.progress === undefined ? null : (
        <Progress value={metric.progress} color={meta.color} mt="sm" size="sm" radius="xl" />
      )}
    </Card>
  );
}

function HostMonitoringPreviewContent() {
  const [range, setRange] = useState<Range>("24h");
  return (
    <Stack gap={density.gridSpacing}>
      <PreviewNotice />
      <Card withBorder radius="md" padding={density.cardPadding}>
        <Group justify="space-between" align="flex-start" wrap="wrap">
          <Group gap="sm" wrap="nowrap">
            <ThemeIcon size={40} radius="md" variant="light" color="blue">
              <IconServer size={22} />
            </ThemeIcon>
            <div>
              <Group gap="xs">
                <Title order={4}>当前主机</Title>
                <Badge color="teal" variant="light" leftSection={<IconCircleCheck size={14} />}>
                  可采样
                </Badge>
              </Group>
              <Text size="sm" c="dimmed" mt={2}>
                仅本机 · 最近采样于开发态固定时间点
              </Text>
            </div>
          </Group>
          <Badge color="grape" variant="light">
            开发预览数据
          </Badge>
        </Group>
      </Card>
      <Group justify="space-between" align="center" wrap="wrap">
        <div>
          <Title order={4}>资源概览</Title>
          <Text size="sm" c="dimmed">
            未接入的指标会明确标记为未知或采样异常。
          </Text>
        </div>
        <Button.Group>
          {(Object.keys(rangeLabels) as Range[]).map((item) => (
            <Button
              key={item}
              size="xs"
              variant={item === range ? "filled" : "default"}
              onClick={() => setRange(item)}
            >
              {rangeLabels[item]}
            </Button>
          ))}
        </Button.Group>
      </Group>
      <SimpleGrid cols={{ base: 1, sm: 2, lg: 3 }} spacing={density.gridSpacing}>
        {metrics.map((metric) => (
          <MetricCard key={metric.title} metric={metric} />
        ))}
      </SimpleGrid>
      <Alert color="blue" variant="light" title="CPU 使用趋势">
        {rangeSummaries[range]}
      </Alert>
    </Stack>
  );
}

export function HostMonitoringPreview() {
  const { scenario, recover } = useDevelopmentPreviewScenario();
  return (
    <DevelopmentPreviewState subject="主机监控" scenario={scenario} onRetry={recover}>
      <HostMonitoringPreviewContent />
    </DevelopmentPreviewState>
  );
}
