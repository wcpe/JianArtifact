// 手搓时序折线图（FR-99）：纯 SVG，零依赖。
// 把一串时序点（ts/value）归一到 viewBox 画折线 + 各数据点圆点；
// 悬停某点经受控 state 显示浮层文案（该点本地时间 + 格式化取值），空数据走空态文案。
// 颜色经 CSS 变量适配主题，不引图表库。

import { useState } from 'react';
import { Box, Text } from '@mantine/core';
import type { MetricPoint } from '../../api/types';

/** 折线图属性。 */
interface LineChartProps {
  /** 时序点（按 ts 升序）。 */
  points: MetricPoint[];
  /** 空数据占位文案。 */
  emptyText: string;
  /** 值格式化（默认原值字符串）。 */
  valueFormat?: (value: number) => string;
  /** 折线区高度（像素），默认 80。 */
  height?: number;
}

/** 内部坐标点（含归一后的 x/y 与原始 ts/value）。 */
interface PlotPoint {
  x: number;
  y: number;
  ts: number;
  value: number;
}

const VIEW_WIDTH = 300;

/** 把某点的 ts + value 组成 hover 浮层文案（本地时间 + 格式化取值）。 */
function pointLabel(point: MetricPoint, valueFormat: (v: number) => string): string {
  const time = new Date(point.ts).toLocaleString();
  return `${time}  ${valueFormat(point.value)}`;
}

/** 时序折线图。 */
export function LineChart({
  points,
  emptyText,
  valueFormat = (v) => `${v}`,
  height = 80,
}: LineChartProps) {
  // 悬停点索引（null = 未悬停）；受控 state 便于测试断言浮层文案
  const [hovered, setHovered] = useState<number | null>(null);

  if (points.length === 0) {
    return (
      <Box h={height} style={{ display: 'flex', alignItems: 'center', justifyContent: 'center' }}>
        <Text c="dimmed" size="sm">
          {emptyText}
        </Text>
      </Box>
    );
  }

  // 归一：x 按索引等距（避免不均匀 ts 间隔挤压），y 按 [min,max] 映射到 [pad, height-pad]
  const pad = 6;
  const values = points.map((p) => p.value);
  const minV = Math.min(...values);
  const maxV = Math.max(...values);
  const span = maxV - minV;
  const denom = points.length > 1 ? points.length - 1 : 1;

  const plot: PlotPoint[] = points.map((p, i) => {
    const x = points.length > 1 ? (i / denom) * VIEW_WIDTH : VIEW_WIDTH / 2;
    // span 为 0（全等值）时画在中线，避免除零
    const ratio = span > 0 ? (p.value - minV) / span : 0.5;
    const y = height - pad - ratio * (height - 2 * pad);
    return { x, y, ts: p.ts, value: p.value };
  });

  const polyline = plot.map((pt) => `${pt.x.toFixed(1)},${pt.y.toFixed(1)}`).join(' ');
  const hoveredPoint = hovered !== null ? points[hovered] : null;

  return (
    <Box>
      <svg
        width="100%"
        height={height}
        viewBox={`0 0 ${VIEW_WIDTH} ${height}`}
        preserveAspectRatio="none"
        role="img"
        aria-label="时序折线"
      >
        {/* 折线 */}
        <polyline
          points={polyline}
          fill="none"
          stroke="var(--mantine-primary-color-filled)"
          strokeWidth={2}
          vectorEffect="non-scaling-stroke"
        />
        {/* 数据点：圆点 + 不可见命中区（半径较大便于 hover）；以 aria-label 承载该点取值，测试可查询 */}
        {plot.map((pt, i) => (
          <circle
            key={pt.ts}
            cx={pt.x}
            cy={pt.y}
            r={3}
            fill="var(--mantine-primary-color-filled)"
            aria-label={pointLabel(points[i], valueFormat)}
            onMouseEnter={() => setHovered(i)}
            onMouseLeave={() => setHovered((h) => (h === i ? null : h))}
            style={{ cursor: 'pointer' }}
          />
        ))}
      </svg>
      {/* 悬停浮层：显示当前悬停点的时间 + 取值；未悬停不渲染 */}
      {hoveredPoint && (
        <Text size="xs" c="dimmed" role="status">
          {pointLabel(hoveredPoint, valueFormat)}
        </Text>
      )}
    </Box>
  );
}
