// 手搓时序折线图测试（FR-99，零依赖 SVG）：
// 空数据走空态文案；多点渲染折线 + 各点承载取值；悬停某点显示该点时间 + 取值浮层。

import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MantineProvider } from '@mantine/core';
import { LineChart } from './LineChart';
import { computePlot } from './lineChartGeometry';
import type { MetricPoint } from '../../api/types';

/** 在 Mantine Provider 下渲染折线图。 */
function renderChart(points: MetricPoint[], valueFormat?: (v: number) => string) {
  return render(
    <MantineProvider>
      <LineChart points={points} emptyText="暂无数据" valueFormat={valueFormat} />
    </MantineProvider>,
  );
}

describe('LineChart', () => {
  it('空数据展示空态文案、不画折线', () => {
    const { container } = renderChart([]);
    expect(screen.getByText('暂无数据')).toBeInTheDocument();
    expect(container.querySelector('polyline')).toBeNull();
  });

  it('多点渲染折线与各数据点', () => {
    const points: MetricPoint[] = [
      { ts: 1_700_000_000_000, value: 10 },
      { ts: 1_700_000_060_000, value: 20 },
      { ts: 1_700_000_120_000, value: 15 },
    ];
    const { container } = renderChart(points);
    // 一条折线
    expect(container.querySelector('polyline')).not.toBeNull();
    // 三个数据点圆
    expect(container.querySelectorAll('circle')).toHaveLength(3);
  });

  it('各数据点以 aria-label 承载该点取值（用格式化器）', () => {
    const points: MetricPoint[] = [{ ts: 1_700_000_000_000, value: 42 }];
    renderChart(points, (v) => `${v}%`);
    // aria-label 含格式化后的值
    expect(screen.getByLabelText(/42%/)).toBeInTheDocument();
  });

  it('悬停某点显示该点时间 + 取值浮层', async () => {
    const user = userEvent.setup();
    const points: MetricPoint[] = [
      { ts: 1_700_000_000_000, value: 10 },
      { ts: 1_700_000_060_000, value: 88 },
    ];
    renderChart(points, (v) => `${v} 次`);

    // 未悬停时无 status 浮层
    expect(screen.queryByRole('status')).not.toBeInTheDocument();

    // 悬停第二个点（值 88）
    const point = screen.getByLabelText(/88 次/);
    await user.hover(point);

    const tip = await screen.findByRole('status');
    expect(tip).toHaveTextContent('88 次');
  });

  // —— Bug-2 复现：渲染不对 ——

  it('Bug-2 渲染：不得用 preserveAspectRatio="none"（否则圆点被横向拉伸成椭圆、折线失真）', () => {
    const points: MetricPoint[] = [
      { ts: 1_700_000_000_000, value: 10 },
      { ts: 1_700_000_060_000, value: 20 },
    ];
    const { container } = renderChart(points);
    const svg = container.querySelector('svg');
    expect(svg).not.toBeNull();
    // 等比保持，避免 x 方向被拉伸导致圆点变椭圆、视觉失真
    expect(svg?.getAttribute('preserveAspectRatio')).not.toBe('none');
  });

  it('Bug-2 渲染：单点也要画出折线（单点 polyline 不可见，应退化为水平线段）', () => {
    const points: MetricPoint[] = [{ ts: 1_700_000_000_000, value: 42 }];
    const { container } = renderChart(points);
    const polyline = container.querySelector('polyline');
    expect(polyline).not.toBeNull();
    // 单点时 polyline 至少含两个坐标对，才有可见线段
    const coords = (polyline?.getAttribute('points') ?? '').trim().split(/\s+/).filter(Boolean);
    expect(coords.length).toBeGreaterThanOrEqual(2);
  });
});

describe('computePlot（纯坐标计算）', () => {
  const height = 80;

  it('多点：x 按索引等距铺满、首点贴左末点贴右', () => {
    const points: MetricPoint[] = [
      { ts: 1, value: 0 },
      { ts: 2, value: 5 },
      { ts: 3, value: 10 },
    ];
    const plot = computePlot(points, height);
    expect(plot[0].x).toBeCloseTo(0);
    expect(plot[2].x).toBeCloseTo(300);
    expect(plot[1].x).toBeCloseTo(150);
  });

  it('等值序列：不产生 NaN、y 落在可视区内（中线）', () => {
    const points: MetricPoint[] = [
      { ts: 1, value: 7 },
      { ts: 2, value: 7 },
    ];
    const plot = computePlot(points, height);
    for (const pt of plot) {
      expect(Number.isNaN(pt.y)).toBe(false);
      expect(pt.y).toBeGreaterThanOrEqual(0);
      expect(pt.y).toBeLessThanOrEqual(height);
    }
  });

  it('y 映射：最大值贴顶、最小值贴底（y 轴向下，顶部 y 更小）', () => {
    const points: MetricPoint[] = [
      { ts: 1, value: 0 },
      { ts: 2, value: 100 },
    ];
    const plot = computePlot(points, height);
    // value=0 在底部（y 较大），value=100 在顶部（y 较小）
    expect(plot[0].y).toBeGreaterThan(plot[1].y);
  });
});
