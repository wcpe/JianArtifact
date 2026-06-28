// 折线图几何计算（FR-99）：从 LineChart 抽出的无副作用纯函数，便于穷举测试、
// 也避免在组件文件内导出非组件成员（React Fast Refresh 友好）。

import type { MetricPoint } from '../../api/types';

/** 折线视图宽度（viewBox 逻辑像素）。 */
export const VIEW_WIDTH = 300;
/** 折线区内边距（像素）。 */
export const PAD = 6;

/** 内部坐标点（含归一后的 x/y 与原始 ts/value）。 */
export interface PlotPoint {
  x: number;
  y: number;
  ts: number;
  value: number;
}

/**
 * 把时序点归一为绘图坐标（纯函数，便于穷举测试）。
 * x 按索引等距铺满 [0, VIEW_WIDTH]（避免不均匀 ts 间隔挤压），单点居中；
 * y 按 [min,max] 线性映射到 [PAD, height-PAD]，全等值（span=0）落中线避免除零 / NaN。
 */
export function computePlot(points: MetricPoint[], height: number): PlotPoint[] {
  const values = points.map((p) => p.value);
  const minV = Math.min(...values);
  const maxV = Math.max(...values);
  const span = maxV - minV;
  const denom = points.length > 1 ? points.length - 1 : 1;

  return points.map((p, i) => {
    const x = points.length > 1 ? (i / denom) * VIEW_WIDTH : VIEW_WIDTH / 2;
    // span 为 0（全等值）时画在中线，避免除零
    const ratio = span > 0 ? (p.value - minV) / span : 0.5;
    const y = height - PAD - ratio * (height - 2 * PAD);
    return { x, y, ts: p.ts, value: p.value };
  });
}

/**
 * 据鼠标在 SVG 内的归一 x（0~VIEW_WIDTH）寻最近数据点索引（纯函数，便于测试）。
 * 用于单一覆盖命中区按"最近点"判定，避免逐点小命中圆之间的空隙导致浮层闪烁。
 */
export function nearestIndex(plot: PlotPoint[], svgX: number): number {
  let best = 0;
  let bestDist = Infinity;
  for (let i = 0; i < plot.length; i += 1) {
    const d = Math.abs(plot[i].x - svgX);
    if (d < bestDist) {
      bestDist = d;
      best = i;
    }
  }
  return best;
}
