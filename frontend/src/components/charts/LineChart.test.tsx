// 手搓时序折线图测试（FR-99，零依赖 SVG）：
// 空数据走空态文案；多点渲染折线 + 各点承载取值；悬停某点显示该点时间 + 取值浮层。

import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MantineProvider } from '@mantine/core';
import { LineChart } from './LineChart';
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
});
