// 运维区统一设计系统：页面头 / KPI / 状态徽章 / 进度条 / 分区卡 / 状态语义色。
// 所有运维页面（集群、审计、消息中心、主机监控）共用，保证视觉一致。
import { ReactNode, CSSProperties } from "react";

import {
  Badge,
  Box,
  Card,
  Group,
  SimpleGrid,
  Stack,
  Text,
  ThemeIcon,
  Tooltip,
} from "@mantine/core";

export type OpsTone =
  | "green"
  | "blue"
  | "orange"
  | "red"
  | "gray"
  | "teal"
  | "cyan"
  | "yellow"
  | "indigo";

/** 复制/同步状态 → 语义色与文案。 */
export const OPS_STATE_TEXT: Record<string, string> = {
  healthy: "健康",
  syncing: "同步中",
  stale: "已过期",
  failed: "失败",
  unpaired: "未配对",
  paused: "已暂停",
  disabled: "未启用",
  not_applicable: "不适用",
  unknown: "未知",
};

export const OPS_STATE_TONE: Record<string, OpsTone> = {
  healthy: "green",
  syncing: "blue",
  stale: "orange",
  failed: "red",
  unpaired: "orange",
  paused: "orange",
  disabled: "gray",
  not_applicable: "gray",
  unknown: "gray",
};

export function opsStateText(state: string): string {
  return OPS_STATE_TEXT[state] ?? state;
}

export function opsStateTone(state: string): OpsTone {
  return OPS_STATE_TONE[state] ?? "gray";
}

const TONE_BG: Record<OpsTone, string> = {
  green: "#ebfbee",
  blue: "#e7f5ff",
  orange: "#fff4e6",
  red: "#fff5f5",
  gray: "#f1f3f5",
  teal: "#e6fcf5",
  cyan: "#e3fafc",
  yellow: "#fff9db",
  indigo: "#edf2ff",
};
const TONE_FG: Record<OpsTone, string> = {
  green: "#2b8a3e",
  blue: "#1971c2",
  orange: "#e8590c",
  red: "#c92a2a",
  gray: "#495057",
  teal: "#0ca678",
  cyan: "#0c8599",
  yellow: "#f08c00",
  indigo: "#4263eb",
};

/** 状态徽章：统一走 Mantine Badge（light 底色随 tone），不再手写 span+圆点。 */
export function StatusPill({ tone, text }: { tone: OpsTone; text: string }) {
  return (
    <Badge color={tone === "gray" ? "gray" : tone} variant="light" size="lg">
      {text}
    </Badge>
  );
}

/** 页头：标题 + 说明 + 右侧操作区。 */
export function OpsPageHeader({
  title,
  meta,
  actions,
}: {
  title: string;
  meta?: string;
  actions?: ReactNode;
}) {
  return (
    <Group justify="space-between" align="center" wrap="wrap" gap="sm">
      <Stack gap={2}>
        <Text fw={700} size="lg">
          {title}
        </Text>
        {meta ? (
          <Text size="xs" c="dimmed">
            {meta}
          </Text>
        ) : null}
      </Stack>
      <Group gap="xs" wrap="wrap">
        {actions}
      </Group>
    </Group>
  );
}

/** 分区卡：统一标题行 + 右侧 meta/操作。style/bodyStyle 供分区内部滚动布局使用。 */
export function OpsSection({
  title,
  meta,
  actions,
  children,
  bodyPadding = "md",
  tone,
  style,
  bodyStyle,
}: {
  title: ReactNode;
  meta?: ReactNode;
  actions?: ReactNode;
  children: ReactNode;
  bodyPadding?: string | number;
  tone?: OpsTone;
  style?: CSSProperties;
  bodyStyle?: CSSProperties;
}) {
  return (
    <Card
      withBorder
      radius="md"
      padding={0}
      style={{
        ...(tone === "orange" ? { borderColor: "var(--mantine-color-orange-3)" } : {}),
        ...style,
      }}
    >
      <Group
        justify="space-between"
        align="center"
        wrap="wrap"
        gap="xs"
        px="md"
        py="sm"
        style={{ borderBottom: "1px solid var(--mantine-color-default-border)" }}
      >
        <Group gap="xs" wrap="nowrap">
          <Text fw={700} size="sm">
            {title}
          </Text>
          {meta ? (
            <Text size="xs" c="dimmed">
              {meta}
            </Text>
          ) : null}
        </Group>
        {actions}
      </Group>
      <Box p={bodyPadding} style={bodyStyle}>
        {children}
      </Box>
    </Card>
  );
}

/** KPI 卡。 */
export function OpsKpi({
  label,
  value,
  hint,
  tone = "gray",
  highlight,
}: {
  label: string;
  value: ReactNode;
  hint?: string;
  tone?: OpsTone;
  highlight?: boolean;
}) {
  return (
    <Card
      withBorder
      radius="md"
      padding="sm md"
      ta="center"
      style={
        highlight
          ? { borderColor: `var(--mantine-color-${tone === "gray" ? "blue" : tone}-3)` }
          : undefined
      }
    >
      <Text size="xs" c="dimmed">
        {label}
      </Text>
      <Text
        fw={700}
        size="xl"
        c={tone === "gray" ? undefined : TONE_FG[tone]}
        style={{ lineHeight: 1.3 }}
      >
        {value}
      </Text>
      {hint ? (
        <Text size="xs" c="dimmed">
          {hint}
        </Text>
      ) : null}
    </Card>
  );
}

/** KPI 指标带单项：tone 控制图标底色与数值语义色。 */
export interface OpsKpiItem {
  label: string;
  value: ReactNode;
  icon: ReactNode;
  hint?: string;
  tone?: OpsTone;
  /** 数值是否以警示色显示（如失败数 > 0）。 */
  danger?: boolean;
}

/**
 * KPI 指标带：每项为独立卡片（圆角浅底图标 + 标签 + 大数值），卡片之间留出间距，
 * 悬停显示口径提示。
 */
export function OpsKpiBand({
  items,
  label,
  cols = { base: 2, xs: 3, sm: 4, lg: 6 },
}: {
  items: OpsKpiItem[];
  label: string;
  cols?: { base?: number; xs?: number; sm?: number; lg?: number };
}) {
  return (
    <Box component="section" aria-label={label} style={{ flexShrink: 0 }}>
      <SimpleGrid cols={cols} spacing="md" verticalSpacing="md">
        {items.map((item) => (
          <Tooltip key={item.label} label={item.hint ?? item.label} position="top" openDelay={300}>
            <Card withBorder radius="md" padding="sm" style={{ height: "100%" }}>
              <Group gap="sm" wrap="nowrap" align="center">
                <ThemeIcon variant="light" color={item.tone ?? "gray"} size={36} radius="md">
                  {item.icon}
                </ThemeIcon>
                <Stack gap={0} style={{ minWidth: 0, flex: 1 }}>
                  <Text size="xs" c="dimmed" truncate lh={1.3}>
                    {item.label}
                  </Text>
                  <Text
                    size="lg"
                    fw={700}
                    lh={1.2}
                    truncate
                    c={item.danger ? "red" : undefined}
                    style={{ fontVariantNumeric: "tabular-nums" }}
                  >
                    {item.value}
                  </Text>
                </Stack>
              </Group>
            </Card>
          </Tooltip>
        ))}
      </SimpleGrid>
    </Box>
  );
}

/** 同步进度条（水位比）。 */
export function OpsBar({
  ratio,
  tone = "green",
  height = 9,
}: {
  ratio: number;
  tone?: OpsTone;
  height?: number;
}) {
  const clamped = Math.max(0, Math.min(1, ratio));
  return (
    <Box
      h={height}
      style={{
        background: "#edf0f3",
        borderRadius: 6,
        overflow: "hidden",
      }}
    >
      <Box
        h={height}
        w={`${clamped * 100}%`}
        style={{ background: `var(--mantine-color-${tone === "gray" ? "teal" : tone}-5)` }}
      />
    </Box>
  );
}

/** 边/节点摘要行展开的详情网格。 */
export function OpsDetailGrid({ items }: { items: Array<{ label: string; value: ReactNode }> }) {
  return <SimpleGridLike items={items} />;
}

function SimpleGridLike({ items }: { items: Array<{ label: string; value: ReactNode }> }) {
  return (
    <Box
      style={{
        display: "grid",
        gridTemplateColumns: "repeat(auto-fit, minmax(150px, 1fr))",
        gap: 10,
      }}
    >
      {items.map((item) => (
        <Box key={item.label} p="xs" style={{ background: "#fafbfc", borderRadius: 8 }}>
          <Text size="xs" c="dimmed">
            {item.label}
          </Text>
          <Text size="sm" style={{ marginTop: 2, overflowWrap: "anywhere" }}>
            {item.value}
          </Text>
        </Box>
      ))}
    </Box>
  );
}

export { TONE_BG, TONE_FG };
