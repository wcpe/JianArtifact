// 运维区统一设计系统：状态徽章 / 分区卡 / KPI 指标带 / 详情网格。
// 所有运维页面（审计、备份迁移、主机监控、设置）共用，保证视觉一致。
//
// 收敛记录：原先并存 OpsKpi（居中单卡）与 OpsKpiBand（图标+数值带）两套 KPI 组件，
// 加上各页内联实现共 5 套；本次统一到 OpsKpiBand 一套，其余实现与仅被它们引用的
// 色表 / 状态文案映射一并删除（OpsPageHeader、OpsBar、TONE_BG/TONE_FG、
// OPS_STATE_TEXT/OPS_STATE_TONE、opsStateText/opsStateTone 全为零引用）。
import { ReactNode, CSSProperties } from "react";

import {
  Badge,
  Box,
  Button,
  Card,
  Group,
  Popover,
  SimpleGrid,
  Stack,
  Text,
  ThemeIcon,
  Tooltip,
} from "@mantine/core";
import { IconHelpCircle } from "@tabler/icons-react";
import { useTranslation } from "react-i18next";

export type OpsTone =
  "green" | "blue" | "orange" | "red" | "gray" | "teal" | "cyan" | "yellow" | "indigo";

/** 状态徽章：统一走 Mantine Badge（light 底色随 tone），不再手写 span+圆点。 */
export function StatusPill({ tone, text }: { tone: OpsTone; text: string }) {
  return (
    <Badge color={tone === "gray" ? "gray" : tone} variant="light" size="lg">
      {text}
    </Badge>
  );
}

/**
 * 分区卡：统一标题行 + 右侧 meta/操作。style/bodyStyle 供分区内部滚动布局使用。
 *
 * 说明类内容不放这里：它不该常驻占用卡片高度，改由 `OpsHelpButton` 挂在工具栏上（气泡展示）。
 */
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
            // meta 支持 ReactNode：文本走 dimmed Text，徽章等节点用 Box 承载，
            // 避免 <div>（Badge）被塞进 <p>（Text 默认标签）触发非法嵌套告警。
            typeof meta === "string" || typeof meta === "number" ? (
              <Text size="xs" c="dimmed">
                {meta}
              </Text>
            ) : (
              <Box>{meta}</Box>
            )
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
 * 悬停显示口径提示。全站唯一 KPI 呈现口径。
 *
 * `actions`：把该页的主操作（新建 / 保存）与摘要带并到同一行——页内大标题移除后，
 * 顶部只剩右对齐按钮会显得空，摘要带正好承担原来标题的视觉重量与信息密度。
 */
export function OpsKpiBand({
  items,
  label,
  cols = { base: 2, xs: 3, sm: 4, lg: 6 },
  variant = "cards",
  actions,
}: {
  items: OpsKpiItem[];
  label: string;
  cols?: { base?: number; xs?: number; sm?: number; lg?: number };
  /** cards = 每项独立卡片；strip = 整条共用一个卡片的紧凑横带（仪表盘首屏用）。 */
  variant?: "cards" | "strip";
  /** 该页主操作；strip 变体并到同一行右侧，cards 变体渲染为带上一行右对齐。 */
  actions?: ReactNode;
}) {
  // 紧凑横带：指标共享一个卡片，首屏高度最省；仪表盘与各管理页用它把信息密度前置。
  if (variant === "strip") {
    return (
      <Card withBorder radius="md" padding="sm" component="section" aria-label={label}>
        <Group justify="space-between" align="center" gap="md" wrap="wrap">
          {/* flexBasis：给摘要区一个最小基准宽度——否则窄屏上 actions（状态徽章 + 按钮）
              会把摘要区压成一条缝，标签与数值全被 ellipsis 截成「匿…」「未…」。
              放不下时 Group 的 wrap 会让 actions 换到下一行，摘要区独占整行。 */}
          <Box style={{ flex: "1 1 320px", minWidth: 0 }}>
            <SimpleGrid cols={cols} verticalSpacing="sm" spacing={0}>
              {items.map((item) => (
                <Tooltip
                  key={item.label}
                  label={item.hint ?? item.label}
                  position="top"
                  openDelay={300}
                >
                  <Stack gap={2} px="sm">
                    <Group gap={4} wrap="nowrap">
                      {item.icon}
                      <Text size="xs" c="dimmed" fw={600} lh={1.2} truncate>
                        {item.label}
                      </Text>
                    </Group>
                    <Text
                      size="lg"
                      fw={700}
                      lh={1.2}
                      c={item.danger ? "red" : undefined}
                      style={{ whiteSpace: "nowrap", overflow: "hidden", textOverflow: "ellipsis" }}
                    >
                      {item.value}
                    </Text>
                  </Stack>
                </Tooltip>
              ))}
            </SimpleGrid>
          </Box>
          {actions ? (
            <Group gap="xs" wrap="nowrap" style={{ flexShrink: 0 }}>
              {actions}
            </Group>
          ) : null}
        </Group>
      </Card>
    );
  }

  return (
    <Box component="section" aria-label={label} style={{ flexShrink: 0 }}>
      {actions ? (
        <Group justify="flex-end" gap="xs" mb="sm">
          {actions}
        </Group>
      ) : null}
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

/**
 * 「说明」按钮：图标 + 文字，点开在气泡里展示分区说明（既有的 label/value 明细）。
 *
 * 为什么不再用底部说明卡：说明内容对操作不是必需的，常驻卡片会一直占用纵向空间
 * （即使是默认收起也会占一行）；挂到工具栏按钮上则完全不占页面布局，需要时才展开。
 * 窄屏也适用：Popover 宽 340 与手机视口同量级，内容纵向排列即可读完。
 */
export function OpsHelpButton({
  title,
  items,
  label,
}: {
  title: string;
  items: Array<{ label: string; value: string }>;
  label?: string;
}) {
  const { t } = useTranslation();
  return (
    <Popover width={340} position="bottom-end" shadow="md" withinPortal>
      <Popover.Target>
        <Button
          size="compact-xs"
          variant="subtle"
          aria-label={title}
          leftSection={<IconHelpCircle size={14} />}
        >
          {label ?? t("common.help")}
        </Button>
      </Popover.Target>
      <Popover.Dropdown>
        <Stack gap="sm">
          <Text size="sm" fw={600}>
            {title}
          </Text>
          {items.map((item) => (
            <Stack key={item.label} gap={2}>
              <Text size="xs" fw={600}>
                {item.label}
              </Text>
              <Text size="xs" c="dimmed">
                {item.value}
              </Text>
            </Stack>
          ))}
        </Stack>
      </Popover.Dropdown>
    </Popover>
  );
}

/** 边/节点摘要行展开的详情网格。 */ export function OpsDetailGrid({
  items,
}: {
  items: Array<{ label: string; value: ReactNode }>;
}) {
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
