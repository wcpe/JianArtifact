// 手搓时序折线图（FR-99）：纯 SVG，零依赖。
// 把一串时序点（ts/value）归一到 viewBox 画折线 + 半透明面积；点稀疏时画逐点圆点、点密集
// （5s 轮询累积）时省略圆点只留干净折线 + 悬停标记，避免圆点堆成密集团块。
// 悬停经单一覆盖区取最近点显示竖直参考线 + 浮层文案，空数据走空态文案。颜色经 CSS 变量适配主题。

import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Box, Text } from '@mantine/core';
import type { MetricPoint } from '../../api/types';
import { VIEW_WIDTH, computePlot, nearestIndex } from './lineChartGeometry';

/** 逐点圆点的最大点数：超过则只画折线 + 悬停标记，避免密集团块（FR-99 优化）。 */
const MARKER_LIMIT = 40;

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
  const { t } = useTranslation('common');
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
  // 点稀疏才逐点画圆点，密集时省略避免团块（FR-99 优化）。
  const showMarkers = plot.length <= MARKER_LIMIT;
  // 折线下方半透明面积填充，观感更像监控图。
  const areaPoints = `${linePoints[0].x.toFixed(1)},${height} ${polyline} ${linePoints[linePoints.length - 1].x.toFixed(1)},${height}`;
  const hoveredPoint = hovered !== null ? points[hovered] : null;
  const hoveredPlot = hovered !== null ? plot[hovered] : null;

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
        aria-label={t('timeSeriesChart')}
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
        {/* 折线下方半透明面积填充（观感优化，FR-99） */}
        <polygon
          points={areaPoints}
          fill="var(--mantine-primary-color-filled)"
          fillOpacity={0.12}
          stroke="none"
        />
        {/* 折线 */}
        <polyline
          points={polyline}
          fill="none"
          stroke="var(--mantine-primary-color-filled)"
          strokeWidth={2}
          vectorEffect="non-scaling-stroke"
        />
        {/* 悬停竖直参考线 */}
        {hoveredPlot && (
          <line
            x1={hoveredPlot.x}
            y1={0}
            x2={hoveredPlot.x}
            y2={height}
            stroke="var(--mantine-color-dimmed)"
            strokeWidth={1}
            strokeDasharray="3 3"
            vectorEffect="non-scaling-stroke"
          />
        )}
        {/* 数据点圆点：仅点稀疏时逐点画（密集时省略避免团块）；以 aria-label 承载取值供测试 / 读屏 */}
        {showMarkers &&
          plot.map((pt, i) => (
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
        {/* 密集模式下只高亮当前悬停点 */}
        {!showMarkers && hoveredPlot && (
          <circle
            cx={hoveredPlot.x}
            cy={hoveredPlot.y}
            r={4}
            fill="var(--mantine-primary-color-filled)"
            aria-label={hoveredPoint ? pointLabel(hoveredPoint, valueFormat) : undefined}
            vectorEffect="non-scaling-stroke"
          />
        )}
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
