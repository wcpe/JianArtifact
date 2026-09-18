// 趋势图：直接基于 recharts 的渐变面积折线图 + 时段剖析（方案 B 拖选聚焦）。
// - 主系列渐变面积 + 折线，次系列对比折线；第三系列（如失败）走独立右轴，避免被主量纲压平；
// - 浮动 tooltip：鼠标悬停显示时间与各系列数值卡片（视觉主通道）；
// - 悬停读数行（trend-hover-summary）保留为读屏专用（sr-only），供 aria-live 与测试定位；
// - 区间剖析条：当前可视序列的均值 /（累计型）总量 / 峰值 / 谷值 / 首尾相对变动；
// - 拖选聚焦：图面横向拖选即聚焦子时段——数据切片、剖析条跟随、胶囊一键还原（双击同样还原）；
// - 键盘聚焦：图面可聚焦，方向键移光标、Shift+方向键选区间、Enter 提交、Esc 还原；
// - 明暗双主题：SVG attribute 不支持 CSS var，按当前配色方案切换两套 hex；
// - 对外契约保持不变：role="img" + aria-label、trend-hover-summary 常驻占位。
import {
  Area,
  AreaChart,
  CartesianGrid,
  Line,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts";
import {
  Box,
  Group,
  SimpleGrid,
  Stack,
  Text,
  VisuallyHidden,
  useComputedColorScheme,
} from "@mantine/core";
import { useCallback, useEffect, useId, useMemo, useRef, useState } from "react";
import type {
  KeyboardEvent as ReactKeyboardEvent,
  MouseEvent as ReactMouseEvent,
  ReactNode,
} from "react";
import { useTranslation } from "react-i18next";

import { downsampleIndexes, pickByIndexes } from "../../lib/downsample";
import { formatBytes, formatCount } from "../../lib/format";
import type { PreviewTrendPoint } from "../../mocks/observabilityPreview";

/** 数值单位：count 千分位整数；bytes 按 B/KB/MB/GB/TB；percent 百分比（一位小数）。 */
type TrendUnit = "count" | "bytes" | "percent";

interface TrendChartProps {
  title: string;
  summary: string;
  primary: PreviewTrendPoint[];
  secondary?: PreviewTrendPoint[];
  tertiary?: PreviewTrendPoint[];
  primaryLabel: string;
  secondaryLabel?: string;
  tertiaryLabel?: string;
  /** 数值单位：count 千分位整数；bytes 按 B/KB/MB/GB/TB 格式化（容量图）；percent 百分比。 */
  unit?: TrendUnit;
  /** 标题行右侧附加内容（如图表当前值大数字），渲染在图例之前。 */
  headerRight?: ReactNode;
  /**
   * 窄卡片模式（如 1/3 宽并排的三联图）：剖析条固定收成 2 列。
   * 剖析条的列数按视口断点排会失配窄容器——视口够宽但卡片本身只有 250px 时数值会被截断。
   */
  compact?: boolean;
}

const CHART_HEIGHT = 200;

/**
 * 悬停读数节流阈值：点数超过该值才把 pointermove 按帧合并。
 * 小序列逐事件处理即可（recharts 重算成本与点数成正比），保持既有同步行为；
 * 只有大序列才需要节流，否则"移动鼠标就卡"。
 */
const HOVER_THROTTLE_THRESHOLD = 60;

/** 主/次系列在 data 行中的字段名（必须彼此不同且不与 label 冲突）。 */
const PRIMARY_KEY = "__primary__";
const SECONDARY_KEY = "__secondary__";
const TERTIARY_KEY = "__tertiary__";

interface Palette {
  primary: string;
  secondary: string;
  tertiary: string;
  grid: string;
  tick: string;
  /** 数据点描边环：与图表卡片底色一致，让活跃点从折线上「透出」。 */
  dotRing: string;
  /** 拖选预览高亮底色。 */
  brush: string;
}

// 亮色主题：Mantine 默认色板 hex（SVG attribute 不支持 CSS var，需落成具体颜色）。
const LIGHT_PALETTE: Palette = {
  primary: "#1c7ed6",
  secondary: "#40c057",
  tertiary: "#fa5252",
  grid: "#e9ecef",
  tick: "#909296",
  dotRing: "#ffffff",
  brush: "rgba(133, 183, 235, 0.28)",
};

// 深色主题：同样取自 Mantine 深色色板，保证网格与刻度在暗底上仍有对比。
const DARK_PALETTE: Palette = {
  primary: "#4dabf7",
  secondary: "#51cf66",
  tertiary: "#ff8787",
  grid: "#373a40",
  tick: "#909296",
  dotRing: "#1a1b1e",
  brush: "rgba(77, 171, 247, 0.22)",
};

function formatTrendValue(value: number, unit: TrendUnit): string {
  if (unit === "bytes") return formatBytes(value);
  if (unit === "percent") return `${value.toFixed(1)}%`;
  return formatCount(value);
}

/** 区间剖析统计：基于当前可视（全量或聚焦切片）主序列计算。 */
interface TrendStats {
  mean: string;
  /** 样本值之和；仅累计型（count）有意义，bytes / percent 为 null 不展示。 */
  total: string | null;
  peak: PreviewTrendPoint;
  trough: PreviewTrendPoint;
  /** 首尾相对变动（如 "+12.3% ▲"）；首值不可用或为 0 时为 "—"。 */
  delta: string;
  /** 首尾绝对差（title 提示用）。 */
  deltaAbs: string;
}

function computeStats(
  points: PreviewTrendPoint[],
  unit: TrendUnit,
  flatLabel: string,
): TrendStats | null {
  const finite = points.filter(
    (point) => typeof point.value === "number" && Number.isFinite(point.value),
  );
  const [firstPoint] = finite;
  if (!firstPoint) return null;
  const sum = finite.reduce((acc, point) => acc + point.value, 0);
  const meanValue = sum / finite.length;
  // 均值保留一位小数（bytes 交给 formatBytes 自行取舍）。
  const mean = formatTrendValue(
    unit === "bytes" ? meanValue : Math.round(meanValue * 10) / 10,
    unit,
  );
  let peak = firstPoint;
  let trough = firstPoint;
  for (const point of finite) {
    if (point.value > peak.value) peak = point;
    if (point.value < trough.value) trough = point;
  }
  const first = firstPoint.value;
  const lastPoint = finite[finite.length - 1];
  const last = lastPoint ? lastPoint.value : first;
  let delta = "—";
  let deltaAbs = "—";
  if (first !== 0) {
    const pct = ((last - first) / Math.abs(first)) * 100;
    if (Math.abs(pct) < 0.05) {
      delta = flatLabel;
    } else {
      delta = `${pct > 0 ? "+" : ""}${pct.toFixed(1)}% ${pct > 0 ? "▲" : "▼"}`;
    }
    const abs = last - first;
    deltaAbs = `${abs > 0 ? "+" : ""}${formatTrendValue(abs, unit)}`;
  }
  return {
    mean,
    total: unit === "count" ? formatTrendValue(sum, unit) : null,
    peak,
    trough,
    delta,
    deltaAbs,
  };
}

/** 浮动 tooltip 卡片：时间 + 各系列（色点 / 名称 / 数值）。 */
function TrendTooltipContent({
  active,
  payload,
  label,
  unit,
}: {
  active?: boolean;
  payload?: Array<{ name?: string; value?: number | string; color?: string; stroke?: string }>;
  label?: string | number;
  unit: TrendUnit;
}) {
  if (!active || !payload || payload.length === 0) return null;
  const items = payload.filter(
    (entry) => typeof entry.value === "number" && Number.isFinite(entry.value),
  );
  if (items.length === 0) return null;
  return (
    <Stack
      gap={4}
      px={10}
      py={8}
      style={{
        background: "var(--mantine-color-body)",
        border: "1px solid var(--mantine-color-gray-2)",
        borderRadius: "var(--mantine-radius-md)",
        boxShadow: "var(--mantine-shadow-md)",
      }}
    >
      <Text size="xs" fw={700}>
        {label}
      </Text>
      {items.map((entry) => (
        <Group key={entry.name} justify="space-between" gap="lg" wrap="nowrap">
          <Group gap={6} wrap="nowrap">
            <Box w={8} h={8} style={{ borderRadius: 4, background: entry.color ?? entry.stroke }} />
            <Text size="xs" c="dimmed" component="span">
              {entry.name}
            </Text>
          </Group>
          <Text size="xs" fw={700} component="span">
            {formatTrendValue(Number(entry.value), unit)}
          </Text>
        </Group>
      ))}
    </Stack>
  );
}

export function TrendChart({
  title,
  summary,
  primary,
  secondary,
  tertiary,
  primaryLabel,
  secondaryLabel,
  tertiaryLabel,
  unit = "count",
  headerRight,
  compact = false,
}: TrendChartProps) {
  const { t } = useTranslation();
  const palette = useComputedColorScheme("light") === "dark" ? DARK_PALETTE : LIGHT_PALETTE;
  const [hoverIndex, setHoverIndex] = useState<number | null>(null);
  /** 已提交的聚焦区间（原始 primary 索引，闭区间）。 */
  const [focus, setFocus] = useState<{ start: number; end: number } | null>(null);
  /** 拖选进行中的预览区间（相对当前视图的 anchor 起点索引 + end 当前索引）。 */
  const [drag, setDrag] = useState<{ anchor: number; end: number } | null>(null);
  /** 键盘选择锚点（相对当前视图索引）；与 hoverIndex 配对组成待提交区间。 */
  const [keyboardAnchor, setKeyboardAnchor] = useState<number | null>(null);
  // drag 的同步镜像：mouse 事件可能同一宏任务连续派发（闭包陈旧），ref 保证读到最新值。
  const dragRef = useRef<{ anchor: number; end: number } | null>(null);
  /** 悬停读数的 rAF 句柄：把同一帧内的多次 pointermove 合并成一次 setState。 */
  const hoverFrameRef = useRef<number | null>(null);
  const gradientId = useId();
  // useId 返回值含 ":"，不宜直接当查询用 id，这里去掉冒号。
  const keyboardHintId = `${gradientId.replace(/:/g, "")}-keyboard`;

  // 卸载时撤销未执行的悬停帧，避免对已卸载组件 setState。
  useEffect(
    () => () => {
      if (hoverFrameRef.current !== null) window.cancelAnimationFrame(hoverFrameRef.current);
    },
    [],
  );

  const hasSecondary = Boolean(secondary && secondaryLabel);
  const hasTertiary = Boolean(tertiary && tertiaryLabel);

  // 渲染护栏：三条序列先按**同一组索引**降采样，再做聚焦切片。
  // 顺序不可颠倒——聚焦区间与拖选索引都以"当前序列"为基准，先降采样才能保持索引自洽。
  // 点数不超限时原样透传（零开销），超限时把主线程成本封顶，与后端采样粒度解耦。
  const series = useMemo(() => {
    const indexes = downsampleIndexes(primary.length);
    if (indexes.length === primary.length) {
      return { primary, secondary, tertiary };
    }
    return {
      primary: pickByIndexes(primary, indexes) ?? [],
      secondary: pickByIndexes(secondary, indexes),
      tertiary: pickByIndexes(tertiary, indexes),
    };
  }, [primary, secondary, tertiary]);

  /** 聚焦切片在渲染序列中的起始索引；未聚焦时为 0。 */
  const viewOffset = useMemo(() => {
    if (!focus || series.primary.length === 0) return 0;
    const lo = Math.min(focus.start, focus.end);
    return Math.max(0, Math.min(lo, series.primary.length - 1));
  }, [focus, series.primary.length]);

  // 聚焦提交后把主/次/第三系列同步切片（索引对齐保持不变）。
  const view = useMemo(() => {
    if (!focus || series.primary.length === 0) {
      return series;
    }
    const start = viewOffset;
    const end = Math.max(
      start,
      Math.min(Math.max(focus.start, focus.end), series.primary.length - 1),
    );
    return {
      primary: series.primary.slice(start, end + 1),
      secondary: series.secondary?.slice(start, end + 1),
      tertiary: series.tertiary?.slice(start, end + 1),
    };
  }, [focus, series, viewOffset]);

  const stats = useMemo(
    () => computeStats(view.primary, unit, t("dashboard.deltaFlat")),
    [view.primary, unit, t],
  );

  // 合并主/次/第三系列为 recharts 的行数据（按索引对齐，标签以主系列为准）。
  const data = useMemo(
    () =>
      view.primary.map((point, index) => ({
        label: point.label,
        [PRIMARY_KEY]: point.value,
        [SECONDARY_KEY]: view.secondary?.[index]?.value ?? null,
        [TERTIARY_KEY]: view.tertiary?.[index]?.value ?? null,
      })),
    [view],
  );

  const hovered = hoverIndex !== null ? (view.primary[hoverIndex] ?? null) : null;
  const secondaryHovered =
    hasSecondary && hoverIndex !== null ? (view.secondary?.[hoverIndex] ?? null) : null;
  const tertiaryHovered =
    hasTertiary && hoverIndex !== null ? (view.tertiary?.[hoverIndex] ?? null) : null;

  const extraSummary =
    (secondaryHovered !== null && secondaryHovered !== undefined && secondaryLabel
      ? ` · ${secondaryLabel}：${formatTrendValue(secondaryHovered.value, unit)}`
      : "") +
    (tertiaryHovered !== null && tertiaryHovered !== undefined && tertiaryLabel
      ? ` · ${tertiaryLabel}：${formatTrendValue(tertiaryHovered.value, unit)}`
      : "");

  const empty = view.primary.length === 0;
  const viewLength = view.primary.length;
  const canFocus = viewLength > 1;
  /** 仅大序列启用悬停节流；小序列保持逐事件同步响应。 */
  const throttleHover = viewLength > HOVER_THROTTLE_THRESHOLD;
  /** 合法下标上界：clientX → 索引换算的乘数（单点时为 0，任何位置都落在唯一点上）。 */
  const maxIndex = Math.max(0, viewLength - 1);
  /** 百分比换算的除数：单点时退化为 1，仅用于高亮宽度，避免除零。 */
  const plotRatio = Math.max(1, maxIndex);

  // 拖选预览高亮：与渲染切片同基准，聚焦后再拖选也能对上位置。
  const dragStart = drag ? Math.min(drag.anchor, drag.end) : 0;
  const dragEnd = drag ? Math.max(drag.anchor, drag.end) : 0;
  const highlightLeft = canFocus && drag ? `${(dragStart / plotRatio) * 100}%` : "0";
  const highlightWidth = canFocus && drag ? `${((dragEnd - dragStart) / plotRatio) * 100}%` : "0";

  // 键盘待提交区间：与拖选共用同一套高亮渲染。
  const keyboardRange =
    keyboardAnchor !== null && hoverIndex !== null && keyboardAnchor !== hoverIndex
      ? {
          left: (Math.min(keyboardAnchor, hoverIndex) / plotRatio) * 100,
          width: (Math.abs(keyboardAnchor - hoverIndex) / plotRatio) * 100,
        }
      : null;

  const indexFromPointer = (event: ReactMouseEvent<HTMLDivElement>) => {
    const rect = event.currentTarget.getBoundingClientRect();
    const ratio = Math.min(Math.max((event.clientX - rect.left) / rect.width, 0), 1);
    return Math.round(ratio * maxIndex);
  };

  /** 提交拖选：区间跨度 ≥1 个点才聚焦（换算回原始索引），否则视为点击并复位。 */
  const commitDrag = () => {
    const current = dragRef.current;
    dragRef.current = null;
    setDrag(null);
    if (current && Math.abs(current.anchor - current.end) >= 1) {
      setFocus({
        start: Math.min(current.anchor, current.end) + viewOffset,
        end: Math.max(current.anchor, current.end) + viewOffset,
      });
    }
  };

  /** 清除全部选择态（聚焦 + 拖选 + 悬停 + 键盘锚点），一次还原。 */
  const clearAll = useCallback(() => {
    dragRef.current = null;
    setDrag(null);
    setFocus(null);
    setHoverIndex(null);
    setKeyboardAnchor(null);
  }, []);

  /** 键盘：方向键移光标、Shift+方向键扩选、Enter 提交聚焦、Esc 还原。 */
  const handleKeyDown = (event: ReactKeyboardEvent<HTMLDivElement>) => {
    if (!canFocus) return;
    const last = viewLength - 1;
    if (event.key === "ArrowLeft" || event.key === "ArrowRight") {
      event.preventDefault();
      const step = event.key === "ArrowRight" ? 1 : -1;
      const current = hoverIndex ?? (step > 0 ? 0 : last);
      const next = Math.max(0, Math.min(last, current + step));
      // 首次 Shift+方向键把当前光标设为锚点，后续保持锚点只移动另一端。
      setKeyboardAnchor((anchor) => (event.shiftKey ? (anchor ?? current) : null));
      setHoverIndex(next);
      return;
    }
    if (event.key === "Enter") {
      if (keyboardAnchor !== null && hoverIndex !== null && keyboardAnchor !== hoverIndex) {
        event.preventDefault();
        setFocus({
          start: Math.min(keyboardAnchor, hoverIndex) + viewOffset,
          end: Math.max(keyboardAnchor, hoverIndex) + viewOffset,
        });
        setKeyboardAnchor(null);
        setHoverIndex(null);
      }
      return;
    }
    if (event.key === "Escape") {
      event.preventDefault();
      clearAll();
    }
  };

  return (
    <Stack gap="xs">
      <Group justify="space-between" align="center" wrap="wrap">
        <Text fw={600}>{title}</Text>
        <Group gap="sm" wrap="nowrap" align="center">
          {focus && !empty ? (
            <Group
              gap={2}
              wrap="nowrap"
              px={8}
              py={2}
              style={{
                borderRadius: 999,
                border: "1px solid var(--mantine-color-blue-2)",
                background: "var(--mantine-color-blue-0)",
              }}
            >
              <Text size="xs" fw={600} c="blue.7">
                {t("trendChart.focusChip", {
                  from: view.primary[0]?.label ?? "—",
                  to: view.primary[view.primary.length - 1]?.label ?? "—",
                })}
              </Text>
              <Box
                component="button"
                type="button"
                onClick={clearAll}
                aria-label={t("trendChart.focusClear", { defaultValue: "退出聚焦" })}
                style={{
                  border: "none",
                  background: "transparent",
                  cursor: "pointer",
                  padding: "0 2px",
                  fontSize: 12,
                  lineHeight: 1,
                  color: "var(--mantine-color-blue-7)",
                }}
              >
                ✕
              </Box>
            </Group>
          ) : null}
          {headerRight}
          <ChartLegend color={palette.primary} label={primaryLabel} />
          {hasSecondary ? <ChartLegend color={palette.secondary} label={secondaryLabel!} /> : null}
          {hasTertiary ? <ChartLegend color={palette.tertiary} label={tertiaryLabel!} /> : null}
        </Group>
      </Group>

      {empty ? (
        <Box
          h={CHART_HEIGHT}
          style={{
            display: "flex",
            alignItems: "center",
            justifyContent: "center",
            border: "1px dashed var(--mantine-color-gray-3)",
            borderRadius: "var(--mantine-radius-md)",
          }}
        >
          <Text size="sm" c="dimmed">
            {t("trendChart.noData", { defaultValue: "暂无样本数据" })}
          </Text>
        </Box>
      ) : (
        <Box
          role="img"
          aria-label={`${title}：${summary}`}
          aria-describedby={keyboardHintId}
          tabIndex={0}
          title={t("trendChart.keyboardHint", {
            defaultValue: "方向键移动光标，Shift+方向键选区间，Enter 聚焦，Esc 还原",
          })}
          onKeyDown={handleKeyDown}
          style={{
            position: "relative",
            cursor: drag ? "grabbing" : "crosshair",
            userSelect: "none",
            outlineOffset: 2,
          }}
          onMouseDown={(event) => {
            if (!canFocus) return;
            // 阻止浏览器默认的拖动选择文字行为（拖选在这里专指聚焦时段）。
            event.preventDefault();
            const index = indexFromPointer(event);
            setHoverIndex(null);
            setKeyboardAnchor(null);
            dragRef.current = { anchor: index, end: index };
            setDrag(dragRef.current);
          }}
          onMouseMove={(event) => {
            const current = dragRef.current;
            if (current) {
              const next = { ...current, end: indexFromPointer(event) };
              dragRef.current = next;
              setDrag(next);
              return;
            }
            // 大序列才按帧合并悬停读数：指针事件频率远高于渲染帧，逐事件 setState 会让
            // recharts 每帧重算整条 path——这是大序列下"移动鼠标就卡"的直接来源。
            const rect = event.currentTarget.getBoundingClientRect();
            const ratio = Math.min(Math.max((event.clientX - rect.left) / rect.width, 0), 1);
            const index = Math.round(ratio * maxIndex);
            if (!throttleHover) {
              setHoverIndex((currentIndex) => (currentIndex === index ? currentIndex : index));
              return;
            }
            if (hoverFrameRef.current !== null) return;
            hoverFrameRef.current = window.requestAnimationFrame(() => {
              hoverFrameRef.current = null;
              setHoverIndex((currentIndex) => (currentIndex === index ? currentIndex : index));
            });
          }}
          onMouseUp={() => commitDrag()}
          onMouseLeave={() => {
            // 拖选中移出图面视为选到边界附近，直接提交；否则清悬停读数。
            if (dragRef.current) {
              commitDrag();
              return;
            }
            if (hoverFrameRef.current !== null) {
              window.cancelAnimationFrame(hoverFrameRef.current);
              hoverFrameRef.current = null;
            }
            setHoverIndex(null);
          }}
          onDoubleClick={clearAll}
        >
          {/* debounce 合并 ResizeObserver 抖动：容器高度由 flex 决定时，
              连续 resize 会让 recharts 反复重建 svg（与固定视口布局叠加时尤其明显）。 */}
          <ResponsiveContainer width="100%" height={CHART_HEIGHT} debounce={120}>
            <AreaChart
              data={data}
              margin={{ top: 10, right: hasTertiary ? 12 : 8, bottom: 0, left: 0 }}
            >
              <defs>
                <linearGradient id={gradientId} x1="0" y1="0" x2="0" y2="1">
                  <stop offset="0%" stopColor={palette.primary} stopOpacity={0.34} />
                  <stop offset="100%" stopColor={palette.primary} stopOpacity={0.04} />
                </linearGradient>
              </defs>
              <CartesianGrid strokeDasharray="3 3" vertical={false} stroke={palette.grid} />
              <XAxis
                dataKey="label"
                tickLine={false}
                axisLine={false}
                tickMargin={10}
                minTickGap={28}
                tick={{ fontSize: 11, fill: palette.tick }}
              />
              <YAxis
                width={unit === "bytes" ? 78 : unit === "percent" ? 64 : 56}
                tickLine={false}
                axisLine={false}
                tick={{ fontSize: 11, fill: palette.tick }}
                tickFormatter={(value: number) => formatTrendValue(Number(value), unit)}
              />
              {hasTertiary ? (
                <YAxis
                  yAxisId="right"
                  orientation="right"
                  width={44}
                  tickLine={false}
                  axisLine={false}
                  allowDecimals={false}
                  // 第三系列全为 0 时兜底上界，避免右轴塌缩成 0..0 把主图压平。
                  domain={[0, (dataMax: number) => (dataMax > 0 ? Math.ceil(dataMax) : 1)]}
                  tick={{ fontSize: 11, fill: palette.tick }}
                />
              ) : null}
              <Tooltip
                cursor={{ stroke: palette.primary, strokeWidth: 1, strokeDasharray: "4 4" }}
                content={<TrendTooltipContent unit={unit} />}
                isAnimationActive={false}
              />
              <Area
                type="monotone"
                dataKey={PRIMARY_KEY}
                name={primaryLabel}
                stroke={palette.primary}
                strokeWidth={2}
                fill={`url(#${gradientId})`}
                dot={false}
                activeDot={{ r: 4, strokeWidth: 2, stroke: palette.dotRing }}
                isAnimationActive={false}
              />
              {hasSecondary ? (
                <Line
                  type="monotone"
                  dataKey={SECONDARY_KEY}
                  name={secondaryLabel}
                  stroke={palette.secondary}
                  strokeWidth={1.5}
                  dot={false}
                  activeDot={{ r: 3, strokeWidth: 2, stroke: palette.dotRing }}
                  isAnimationActive={false}
                />
              ) : null}
              {hasTertiary ? (
                <Line
                  yAxisId="right"
                  type="monotone"
                  dataKey={TERTIARY_KEY}
                  name={tertiaryLabel}
                  stroke={palette.tertiary}
                  strokeWidth={1.8}
                  strokeDasharray="4 3"
                  dot={false}
                  activeDot={{ r: 3, strokeWidth: 2, stroke: palette.dotRing }}
                  isAnimationActive={false}
                />
              ) : null}
            </AreaChart>
          </ResponsiveContainer>
          {drag ? (
            <Box
              style={{
                position: "absolute",
                top: 0,
                height: CHART_HEIGHT,
                left: highlightLeft,
                width: highlightWidth,
                pointerEvents: "none",
                background: palette.brush,
                borderLeft: "1px dashed var(--mantine-color-blue-6)",
                borderRight: "1px dashed var(--mantine-color-blue-6)",
              }}
            />
          ) : null}
          {!drag && keyboardRange ? (
            <Box
              style={{
                position: "absolute",
                top: 0,
                height: CHART_HEIGHT,
                left: `${keyboardRange.left}%`,
                width: `${keyboardRange.width}%`,
                pointerEvents: "none",
                background: palette.brush,
                borderLeft: "1px dashed var(--mantine-color-blue-6)",
                borderRight: "1px dashed var(--mantine-color-blue-6)",
              }}
            />
          ) : null}
          {/* 悬停读数行：读屏专用（视觉 tooltip 已接管），保留 testid 与常驻占位。 */}
          <VisuallyHidden>
            <Text id={keyboardHintId} size="xs">
              {t("trendChart.keyboardHint", {
                defaultValue: "方向键移动光标，Shift+方向键选区间，Enter 聚焦，Esc 还原",
              })}
            </Text>
            <Text
              data-testid="trend-hover-summary"
              size="xs"
              fw={600}
              mt={4}
              aria-live="polite"
              style={{ minHeight: 18 }}
            >
              {hovered
                ? `${hovered.label} · ${primaryLabel}：${formatTrendValue(hovered.value, unit)}${extraSummary}`
                : " "}
            </Text>
          </VisuallyHidden>
        </Box>
      )}

      {/* 区间剖析条：跟随聚焦切片；拖选提示仅在未聚焦时显示。 */}
      {stats ? (
        <Stack gap={6}>
          <Group gap="sm" wrap="wrap">
            <Text size="xs" fw={600} c="dimmed">
              {t("trendChart.analysisTitle", { defaultValue: "区间剖析" })}
            </Text>
            <Text size="xs" c="dimmed">
              {focus
                ? t("trendChart.analysisFocused", { defaultValue: "统计聚焦时段，双击图面还原" })
                : t("trendChart.focusHint", { defaultValue: "在图面横向拖选可聚焦子时段" })}
            </Text>
          </Group>
          <SimpleGrid cols={compact ? 2 : { base: 2, sm: 3, md: stats.total ? 5 : 4 }} spacing="xs">
            <StatCell
              label={t("trendChart.analysisMean", { defaultValue: "均值" })}
              value={stats.mean}
            />
            {stats.total ? (
              <StatCell
                label={t("trendChart.analysisTotal", { defaultValue: "总量" })}
                value={stats.total}
              />
            ) : null}
            <StatCell
              label={t("trendChart.analysisPeak", { defaultValue: "峰值" })}
              value={formatTrendValue(stats.peak.value, unit)}
              hint={`@ ${stats.peak.label}`}
            />
            <StatCell
              label={t("trendChart.analysisTrough", { defaultValue: "谷值" })}
              value={formatTrendValue(stats.trough.value, unit)}
              hint={`@ ${stats.trough.label}`}
            />
            <StatCell
              label={t("trendChart.analysisChange", { defaultValue: "区间变动" })}
              value={stats.delta}
              hint={`${t("trendChart.analysisDeltaAbs", { defaultValue: "绝对" })} ${stats.deltaAbs}`}
            />
          </SimpleGrid>
        </Stack>
      ) : null}
    </Stack>
  );
}

function StatCell({ label, value, hint }: { label: string; value: string; hint?: string }) {
  return (
    <Stack
      gap={2}
      px="sm"
      py={6}
      style={{
        borderRadius: "var(--mantine-radius-md)",
        background: "var(--mantine-color-gray-0)",
      }}
    >
      <Text size="xs" c="dimmed">
        {label}
      </Text>
      <Text size="sm" fw={600} truncate>
        {value}
      </Text>
      {hint ? (
        <Text size="xs" c="dimmed" truncate>
          {hint}
        </Text>
      ) : null}
    </Stack>
  );
}

function ChartLegend({ color, label }: { color: string; label: string }) {
  return (
    <Group gap={4} wrap="nowrap">
      <Box w={8} h={8} style={{ background: color, borderRadius: "50%" }} />
      <Text size="xs" c="dimmed">
        {label}
      </Text>
    </Group>
  );
}
