// 监控总览页测试（FR-99 重设计）：
// 1) KPI 行渲染各指标当前值（末点 value）；
// 2) 分类切换过滤展示哪些指标（主机 / 防护 / 全部）；
// 3) 时序网格消费 GET /monitor/metrics（mock），每指标一张含折线的卡；
// 4) 时间范围切换改变传给 getMetricSeries 的 from/to/step；
// 5) 无数据源指标（缓存命中率）显「暂无数据」、不发请求；
// 6) 监控页不再含审计内容（审计已拆出独立页）。

import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, waitFor, act } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MantineProvider } from '@mantine/core';
import { MonitorPage, MONITOR_REFRESH_MS } from './MonitorPage';
import * as api from '../api/endpoints';
import type { MetricSeries } from '../api/types';

/** 在 Mantine Provider 下渲染监控页。 */
function renderPage() {
  return render(
    <MantineProvider>
      <MonitorPage />
    </MantineProvider>,
  );
}

/** 桩 getMetricSeries：按指标键返回一段固定时序（末点值由 key 决定，便于断言）。 */
function 桩指标(lastValueByKey: Record<string, number> = {}) {
  vi.spyOn(api, 'getMetricSeries').mockImplementation((metric: string): Promise<MetricSeries> => {
    const last = lastValueByKey[metric] ?? 50;
    return Promise.resolve({
      metric,
      points: [
        { ts: 1_700_000_000_000, value: last - 10 },
        { ts: 1_700_000_060_000, value: last },
      ],
    });
  });
}

describe('MonitorPage（KPI + 时序网格）', () => {
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('渲染分类切换与时间范围切换控件', async () => {
    桩指标();
    renderPage();

    expect(screen.getByLabelText('指标分类')).toBeInTheDocument();
    expect(screen.getByLabelText('时间范围')).toBeInTheDocument();
    // 分类选项含「全部 / 主机 / 防护」等
    expect(screen.getByText('主机')).toBeInTheDocument();
    expect(screen.getByText('防护')).toBeInTheDocument();
  });

  it('默认「全部」分类：KPI 行渲染各指标当前值（末点）', async () => {
    桩指标({ 'host.cpu_percent': 42, 'usage.access_total': 123 });
    renderPage();

    // CPU 使用率当前值 42% 与累计访问量 123 出现（KPI + 网格当前值各一处）
    await waitFor(() => expect(screen.getAllByText('42%').length).toBeGreaterThan(0));
    expect(screen.getAllByText('123').length).toBeGreaterThan(0);
  });

  it('每个可见指标渲染一张含折线的时序卡', async () => {
    桩指标();
    const { container } = renderPage();

    // 取数完成后出现多条折线（每个有数据源指标一条）
    await waitFor(() => expect(container.querySelectorAll('polyline').length).toBeGreaterThan(1));
  });

  it('切到「主机」分类只展示 host.* 指标，隐藏防护指标', async () => {
    桩指标();
    renderPage();

    // 指标名在 KPI 卡与时序卡各出现一次，用 getAllByText 避免「多元素」歧义
    await waitFor(() => expect(screen.getAllByText('CPU 使用率').length).toBeGreaterThan(0));

    await userEvent.click(screen.getByText('主机'));

    // 主机三项可见
    expect(screen.getAllByText('CPU 使用率').length).toBeGreaterThan(0);
    expect(screen.getAllByText('内存使用率').length).toBeGreaterThan(0);
    expect(screen.getAllByText('磁盘使用率').length).toBeGreaterThan(0);
    // 防护 / 使用分析指标不在主机分类
    expect(screen.queryByText('活跃封禁 IP')).not.toBeInTheDocument();
    expect(screen.queryByText('累计访问量')).not.toBeInTheDocument();
  });

  it('时间范围切换改变 getMetricSeries 的 from/to/step', async () => {
    桩指标();
    const spy = vi.mocked(api.getMetricSeries);
    renderPage();

    await waitFor(() => expect(spy).toHaveBeenCalled());
    // 取首次（默认 24h）某次调用的 step
    const first = spy.mock.calls[0][1];
    spy.mockClear();

    // 切到 1h（step=0 不降采样）
    await userEvent.click(screen.getByText('1 小时'));
    await waitFor(() => expect(spy).toHaveBeenCalled());
    const afterSwitch = spy.mock.calls[0][1];

    // 1h 档 step 为 0，与默认 24h 档（非零）不同
    expect(afterSwitch?.step).toBe(0);
    expect(afterSwitch?.step).not.toBe(first?.step);
  });

  it('无数据源指标（缓存命中率）显「暂无数据（待埋点）」且不发请求', async () => {
    桩指标();
    const spy = vi.mocked(api.getMetricSeries);
    renderPage();

    // 切到缓存分类
    await userEvent.click(screen.getByText('缓存'));

    await waitFor(() => expect(screen.getAllByText('缓存命中率').length).toBeGreaterThan(0));
    expect(screen.getByText('暂无数据（待埋点）')).toBeInTheDocument();
    // 缓存命中率无数据源 → 不应对其发查询
    const calledKeys = spy.mock.calls.map((c) => c[0]);
    expect(calledKeys).not.toContain('cache.hit_ratio');
  });

  it('折线某点 hover 显示该时刻取值浮层', async () => {
    桩指标({ 'host.cpu_percent': 66 });
    renderPage();

    await waitFor(() => expect(screen.getAllByText('CPU 使用率').length).toBeGreaterThan(0));

    // 切到主机分类，缩小可见指标，定位 CPU 卡内的数据点
    await userEvent.click(screen.getByText('主机'));
    const cpuPoint = await screen.findByLabelText(/66%/);
    await userEvent.hover(cpuPoint);

    const tip = await screen.findByRole('status');
    expect(tip).toHaveTextContent('66%');
  });

  it('Bug-2 刷新：加载后按固定周期自动重取（无须手动切类目/区间）', async () => {
    vi.useFakeTimers();
    try {
      桩指标();
      const spy = vi.mocked(api.getMetricSeries);
      renderPage();

      // 首次加载发起一轮查询
      await vi.waitFor(() => expect(spy).toHaveBeenCalled());
      const firstRound = spy.mock.calls.length;
      expect(firstRound).toBeGreaterThan(0);
      spy.mockClear();

      // 推进一个刷新周期，应自动再取一轮（依赖未变）
      await act(async () => {
        await vi.advanceTimersByTimeAsync(MONITOR_REFRESH_MS);
      });
      expect(spy.mock.calls.length).toBeGreaterThan(0);
    } finally {
      vi.useRealTimers();
    }
  });

  it('Bug-2 刷新：卸载后不再触发定时取数（清理定时器、无泄漏）', async () => {
    vi.useFakeTimers();
    try {
      桩指标();
      const spy = vi.mocked(api.getMetricSeries);
      const { unmount } = renderPage();

      await vi.waitFor(() => expect(spy).toHaveBeenCalled());
      unmount();
      spy.mockClear();

      await act(async () => {
        await vi.advanceTimersByTimeAsync(MONITOR_REFRESH_MS * 2);
      });
      // 卸载后定时器已清，不应再发查询
      expect(spy.mock.calls.length).toBe(0);
    } finally {
      vi.useRealTimers();
    }
  });

  it('监控页不再含审计内容（审计已拆出独立页）', async () => {
    桩指标();
    renderPage();

    await waitFor(() => expect(screen.getByText('监控')).toBeInTheDocument());
    // 不再有审计 tab / 审计标题
    expect(screen.queryByRole('tab', { name: '审计' })).not.toBeInTheDocument();
    expect(screen.queryByText('暂无审计记录。')).not.toBeInTheDocument();
  });
});
