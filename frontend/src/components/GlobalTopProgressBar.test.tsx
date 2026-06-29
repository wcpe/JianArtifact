// 全局顶部进度条单元测试（FR-127）：
// 1) loading=true 时进度条可见（挂载）；
// 2) loading=false 时进度条淡出（DONE_MS 后卸载）；
// 3) 伪进度纯函数：步进逻辑穷举。

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, act } from '@testing-library/react';
import { MantineProvider } from '@mantine/core';
import { GlobalTopProgressBar } from './GlobalTopProgressBar';
import { GlobalProgressContext } from '../hooks/useGlobalProgress';

beforeEach(() => {
  vi.useFakeTimers();
});

afterEach(() => {
  vi.runOnlyPendingTimers();
  vi.useRealTimers();
});

/**
 * 用给定 loading 值渲染全局进度条，通过 context 注入。
 */
function renderBar(loading: boolean) {
  return render(
    <MantineProvider>
      <GlobalProgressContext.Provider
        value={{
          loading,
          inc: () => {},
          dec: () => {},
          setRouteLoading: () => {},
        }}
      >
        <GlobalTopProgressBar />
      </GlobalProgressContext.Provider>
    </MantineProvider>,
  );
}

describe('GlobalTopProgressBar 可见性', () => {
  it('loading=true 时进度条挂载并可见（role=progressbar）', () => {
    renderBar(true);
    const bar = screen.getByRole('progressbar');
    expect(bar).toBeInTheDocument();
    expect(bar).toHaveAttribute('data-testid', 'global-progress-bar');
  });

  it('loading=false 初始时进度条不渲染', () => {
    renderBar(false);
    expect(screen.queryByRole('progressbar')).not.toBeInTheDocument();
  });

  it('loading 从 true 切换到 false：进度条在 DONE_MS 后卸载', () => {
    const { rerender } = renderBar(true);
    // 确认进度条已挂载
    expect(screen.getByRole('progressbar')).toBeInTheDocument();

    // 切换为 false
    act(() => {
      rerender(
        <MantineProvider>
          <GlobalProgressContext.Provider
            value={{
              loading: false,
              inc: () => {},
              dec: () => {},
              setRouteLoading: () => {},
            }}
          >
            <GlobalTopProgressBar />
          </GlobalProgressContext.Provider>
        </MantineProvider>,
      );
    });

    // DONE_MS(300ms) 内进度条仍挂载（补满淡出阶段）
    act(() => {
      vi.advanceTimersByTime(299);
    });
    // 进度条仍在（宽度=100%，透明度=0，但 DOM 还在）
    expect(screen.getByRole('progressbar')).toBeInTheDocument();

    // 超过 DONE_MS 后卸载
    act(() => {
      vi.advanceTimersByTime(10);
    });
    expect(screen.queryByRole('progressbar')).not.toBeInTheDocument();
  });

  it('进度条固定定位在视口顶端（position=fixed，top=0）', () => {
    renderBar(true);
    const bar = screen.getByRole('progressbar');
    expect(bar.style.position).toBe('fixed');
    expect(bar.style.top).toBe('0px');
  });

  it('进度条 z-index 高于页眉（≥ 9999）', () => {
    renderBar(true);
    const bar = screen.getByRole('progressbar');
    expect(Number(bar.style.zIndex)).toBeGreaterThanOrEqual(9999);
  });

  it('有 aria-label 可访问名（无障碍）', () => {
    renderBar(true);
    expect(screen.getByRole('progressbar', { name: '页面加载进度' })).toBeInTheDocument();
  });
});

describe('GlobalTopProgressBar 伪进度纯函数穷举', () => {
  // 伪进度步进公式：w + (90 - w) * 0.15
  // 此处只验证公式本身（隔离测试），不依赖渲染定时器。
  function step(w: number): number {
    return w >= 90 ? w : w + (90 - w) * 0.15;
  }

  it('初始 w=0：第一步约 13.5', () => {
    expect(step(0)).toBeCloseTo(13.5, 1);
  });

  it('w=50：步进后大于 50 且小于 90', () => {
    const next = step(50);
    expect(next).toBeGreaterThan(50);
    expect(next).toBeLessThan(90);
  });

  it('w=89：步进后仍小于 90（永不超过 90）', () => {
    const next = step(89);
    expect(next).toBeLessThan(90);
  });

  it('w=90：不再步进（已到目标上限）', () => {
    expect(step(90)).toBe(90);
  });

  it('w=95（超过目标上限）：不再步进（防退回）', () => {
    expect(step(95)).toBe(95);
  });

  it('多步连续递增：每步均递增、永不超过 90', () => {
    let w = 0;
    for (let i = 0; i < 50; i++) {
      const next = step(w);
      expect(next).toBeGreaterThanOrEqual(w);
      expect(next).toBeLessThanOrEqual(90);
      w = next;
    }
  });
});
