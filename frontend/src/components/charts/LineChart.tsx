// 手搓时序折线图（FR-99）：纯 SVG，零依赖。
// 把一串时序点（ts/value）归一到 viewBox 画折线 + 各数据点圆点；
// 悬停某点经受控 state 显示浮层文案（该点本地时间 + 格式化取值），空数据走空态文案。
// 颜色经 CSS 变量适配主题，不引图表库。

import { useState } from 'react';
import { Box, Text } from '@mantine/core';
import type { MetricPoint } from '../../api/types';
import { VIEW_WIDTH, computePlot, nearestIndex } from './lineChartGeometry';

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

  const plot = computePlot(points, height);

  // 单点时把同一坐标复制一份，让 polyline 退化为可见的水平线段（单坐标 polyline 不渲染）
  const linePoints = plot.length === 1 ? [plot[0], plot[0]] : plot;
  const polyline = linePoints.map((pt) => `${pt.x.toFixed(1)},${pt.y.toFixed(1)}`).join(' ');
  const hoveredPoint = hovered !== null ? points[hovered] : null;

  // 单一透明覆盖矩形承载 hover：按鼠标位置取最近点，消除逐点命中圆空隙导致的浮层闪烁
  function handleMove(e: React.MouseEvent<SVGRectElement>) {
    const rect = e.currentTarget.getBoundingClientRect();
    if (rect.width === 0) {
      return;
    }
    const svgX = ((e.clientX - rect.left) / rect.width) * VIEW_WIDTH;
    setHovered(nearestIndex(plot, svgX));
  }

  return (
    <Box>
      <svg
        width="100%"
        height={height}
        viewBox={`0 0 ${VIEW_WIDTH} ${height}`}
        role="img"
        aria-label="时序折线"
        // 单一离开判定：仅当鼠标真正移出整张图才清空浮层，避免逐点命中区空隙引发的抖动 / 闪烁
        onMouseLeave={() => setHovered(null)}
      >
        {/* 底层单一透明覆盖区：按鼠标位置取最近点统一接管 hover（填补点间空隙，消除闪烁） */}
        <rect
          x={0}
          y={0}
          width={VIEW_WIDTH}
          height={height}
          fill="transparent"
          onMouseMove={handleMove}
          style={{ cursor: 'pointer' }}
        />
        {/* 折线 */}
        <polyline
          points={polyline}
          fill="none"
          stroke="var(--mantine-primary-color-filled)"
          strokeWidth={2}
          vectorEffect="non-scaling-stroke"
        />
        {/* 数据点圆点（叠在覆盖区之上）：以 aria-label 承载该点取值，测试 / 读屏可查询；
            mouseenter 给出精确命中点，高亮当前悬停点 */}
        {plot.map((pt, i) => (
          <circle
            key={pt.ts}
            cx={pt.x}
            cy={pt.y}
            r={hovered === i ? 4 : 2.5}
            fill="var(--mantine-primary-color-filled)"
            aria-label={pointLabel(points[i], valueFormat)}
            vectorEffect="non-scaling-stroke"
            onMouseEnter={() => setHovered(i)}
            style={{ cursor: 'pointer' }}
          />
        ))}
      </svg>
      {/* 悬停浮层：外层固定占位一行（min-height）避免文案出现 / 消失时高度跳动引发抖动；
          浮层文案仅悬停时渲染（未悬停无 status 节点，契约不变） */}
      <Box style={{ minHeight: '1.2em' }}>
        {hoveredPoint && (
          <Text size="xs" c="dimmed" role="status">
            {pointLabel(hoveredPoint, valueFormat)}
          </Text>
        )}
      </Box>
    </Box>
  );
}
