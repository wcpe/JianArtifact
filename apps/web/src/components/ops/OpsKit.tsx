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
  "green" | "blue" | "orange" | "red" | "gray" | "teal" | "cyan" | "yellow" | "indigo" | "grape";

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
  testId,
}: {
  title?: ReactNode;
  meta?: ReactNode;
  actions?: ReactNode;
  children: ReactNode;
  bodyPadding?: string | number;
  tone?: OpsTone;
  style?: CSSProperties;
  bodyStyle?: CSSProperties;
  /** 供测试定位该分区卡（卡片本身没有语义角色，靠标题文案定位会随文案漂移）。 */
  testId?: string;
}) {
  return (
    <Card
      withBorder
      radius="md"
      padding={0}
      data-testid={testId}
      style={{
        ...(tone === "orange" ? { borderColor: "var(--mantine-color-orange-3)" } : {}),
        ...style,
      }}
    >
      {title || meta || actions ? (
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
      ) : null}
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
  /**
   * 是否为「主指标」：窄屏（strip 变体折叠态）只展示主指标，其余收进「更多指标」。
   *
   * 为什么用标记而不是下标数组：此前审计页用 `NARROW_KPI_INDEXES = [0, 2, 3, 4, 5, 10]`
   * 硬编码位置，任何一次指标增删或重排都会静默错位（把耗时当失败指标展示），
   * 而且看不出「哪些指标重要」这个意图本身。
   */
  primary?: boolean;
  /**
   * 点击指标后的联动动作。给了就是可点的指标（卡片整块可点，带 hover 与焦点态），
   * 没给则保持纯展示。
   */
  onSelect?: () => void;
  /** 当前该指标对应的筛选是否已生效：生效时卡片描边高亮，避免「点了但没反应」的错觉。 */
  active?: boolean;
  /** 可点指标的提示文案（解释点击后会做什么）。 */
  selectHint?: string;
  /**
   * 供测试定位单项指标。
   *
   * 卡片本身没有语义角色，按文案定位会随文案（含语言切换）漂移；
   * 调用方必须给**每一项**一个稳定且唯一的 id——早期维护页按「有无清理动作」
   * 推导 id，导致六个指标里三个都落在同一个 `kpi-total` 上，测试无法区分。
   */
  testId?: string;
}

/**
 * 可点 KPI 卡片的交互样式：整块可点、指针形状、键盘焦点可见，
 * 生效中（active）时用主色描边 + 极浅主色底，让「当前筛选来自哪个指标」一目了然。
 */
function kpiSelectableStyle(active: boolean): CSSProperties {
  return {
    cursor: "pointer",
    textAlign: "left",
    width: "100%",
    border: active ? "1px solid var(--mantine-color-blue-5)" : "1px solid transparent",
    borderRadius: 8,
    background: active ? "var(--mantine-color-blue-0)" : undefined,
    transition: "background 120ms ease, border-color 120ms ease",
  };
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
              {items.map((item) => {
                const body = (
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
                );
                return (
                  <Tooltip
                    key={item.label}
                    label={item.selectHint ?? item.hint ?? item.label}
                    position="top"
                    openDelay={300}
                  >
                    {item.onSelect ? (
                      <Box
                        component="button"
                        type="button"
                        onClick={item.onSelect}
                        aria-pressed={item.active}
                        style={kpiSelectableStyle(Boolean(item.active))}
                      >
                        {body}
                      </Box>
                    ) : (
                      <Box>{body}</Box>
                    )}
                  </Tooltip>
                );
              })}
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
        {items.map((item) => {
          const card = (
            <Card
              withBorder
              radius="md"
              // 卡片内边距与图标尺寸都收紧：6 列布局下每张卡只有约 154px 宽，
              // 图标 36px + 内边距 12×2 + 图标与文字间距 12 就吃掉 72px，
              // 留给数值只剩约 80px——而 `170.9 MB` 实测需要 85px，
              // 于是最后一位被截成 `170.9 ME`（实测确认，不是理论风险）。
              // 把图标缩到 32、内边距收到 xs，可腾出约 20px，中英两种语言的
              // 常见体积文案都能完整显示。
              padding="xs"
              data-testid={item.testId}
              onClick={item.onSelect}
              role={item.onSelect ? "button" : undefined}
              tabIndex={item.onSelect ? 0 : undefined}
              aria-pressed={item.onSelect ? Boolean(item.active) : undefined}
              onKeyDown={
                item.onSelect
                  ? (keyEvent) => {
                      if (keyEvent.key === "Enter" || keyEvent.key === " ") {
                        keyEvent.preventDefault();
                        item.onSelect?.();
                      }
                    }
                  : undefined
              }
              style={{
                height: "100%",
                // 交互样式先铺：danger 的后置覆盖才能保住左侧红线与浅红底，
                // 否则 kpiSelectableStyle 的 border 简写会把红线整体抹掉。
                ...(item.onSelect ? kpiSelectableStyle(Boolean(item.active)) : null),
                ...(item.danger
                  ? {
                      // 危险指标（失败 / 高危 / 待确认 > 0）用左侧色条 + 浅红底强化：
                      // 12 张同形卡片平铺时，纯靠红色数字很难一眼定位真正要看的那几个。
                      borderLeft: "3px solid var(--mantine-color-red-6)",
                      background: "var(--mantine-color-red-0)",
                    }
                  : null),
              }}
            >
              <Group gap="xs" wrap="nowrap" align="center">
                <ThemeIcon variant="light" color={item.tone ?? "gray"} size={32} radius="md">
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
                    // 数值**不加 truncate**：它是指标卡的主体信息，被省略号截掉
                    // 就等于这张卡失去意义。宁可让字号自适应（见下方 style），
                    // 也不让用户看到 `170.9 ME` 这种残缺数字。
                    c={item.danger ? "red" : undefined}
                    style={{
                      fontVariantNumeric: "tabular-nums",
                      // 允许在极窄容器里略微缩小，而不是截断；`1em` 是上限，
                      // 正常宽度下不会触发。
                      fontSize: "clamp(0.8rem, 1.15vw, 1.125rem)",
                      whiteSpace: "nowrap",
                    }}
                  >
                    {item.value}
                  </Text>
                </Stack>
              </Group>
            </Card>
          );
          return (
            <Tooltip
              key={item.label}
              label={item.selectHint ?? item.hint ?? item.label}
              position="top"
              openDelay={300}
            >
              {card}
            </Tooltip>
          );
        })}
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
