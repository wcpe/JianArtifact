// 全局进度上下文单元测试（FR-127）：
// 1) GlobalProgressProvider 提供的 inc/dec 正确切换 loading；
// 2) 并发 inc 后需全部 dec 才变 false；
// 3) setRouteLoading 独立控制路由维度进度；
// 4) 两路独立：req 与 route 各自为 true 时 loading 均为 true，均为 false 才为 false。

import { describe, it, expect } from 'vitest';
import { render, screen, act } from '@testing-library/react';
import { GlobalProgressProvider, useGlobalProgress } from './useGlobalProgress';

/** 探针组件：把 loading 暴露到 DOM 供断言。 */
function Probe() {
  const { loading, inc, dec, setRouteLoading } = useGlobalProgress();
  return (
    <div>
      <span data-testid="loading">{loading ? 'true' : 'false'}</span>
      <button onClick={inc} data-testid="inc">
        inc
      </button>
      <button onClick={dec} data-testid="dec">
        dec
      </button>
      <button onClick={() => setRouteLoading(true)} data-testid="route-on">
        route-on
      </button>
      <button onClick={() => setRouteLoading(false)} data-testid="route-off">
        route-off
      </button>
    </div>
  );
}

function renderProbe() {
  return render(
    <GlobalProgressProvider>
      <Probe />
    </GlobalProgressProvider>,
  );
}

describe('GlobalProgressProvider 请求计数维度', () => {
  it('初始 loading=false', () => {
    renderProbe();
    expect(screen.getByTestId('loading')).toHaveTextContent('false');
  });

  it('inc 后 loading=true', () => {
    renderProbe();
    act(() => {
      screen.getByTestId('inc').click();
    });
    expect(screen.getByTestId('loading')).toHaveTextContent('true');
  });

  it('inc 后 dec 回到 loading=false', () => {
    renderProbe();
    act(() => {
      screen.getByTestId('inc').click();
    });
    act(() => {
      screen.getByTestId('dec').click();
    });
    expect(screen.getByTestId('loading')).toHaveTextContent('false');
  });

  it('并发两次 inc，需两次 dec 才变 false（计数不丢失）', () => {
    renderProbe();
    act(() => {
      screen.getByTestId('inc').click();
      screen.getByTestId('inc').click();
    });
    expect(screen.getByTestId('loading')).toHaveTextContent('true');
    // 第一次 dec：计数从 2→1，仍为 true
    act(() => {
      screen.getByTestId('dec').click();
    });
    expect(screen.getByTestId('loading')).toHaveTextContent('true');
    // 第二次 dec：计数从 1→0，变 false
    act(() => {
      screen.getByTestId('dec').click();
    });
    expect(screen.getByTestId('loading')).toHaveTextContent('false');
  });

  it('dec 不会把计数降到负数（防护：超额 dec 仍为 false）', () => {
    renderProbe();
    // 从未 inc，直接 dec
    act(() => {
      screen.getByTestId('dec').click();
    });
    expect(screen.getByTestId('loading')).toHaveTextContent('false');
  });
});

describe('GlobalProgressProvider 路由切换维度', () => {
  it('setRouteLoading(true) → loading=true', () => {
    renderProbe();
    act(() => {
      screen.getByTestId('route-on').click();
    });
    expect(screen.getByTestId('loading')).toHaveTextContent('true');
  });

  it('setRouteLoading(true) 后 setRouteLoading(false) → loading=false', () => {
    renderProbe();
    act(() => {
      screen.getByTestId('route-on').click();
    });
    act(() => {
      screen.getByTestId('route-off').click();
    });
    expect(screen.getByTestId('loading')).toHaveTextContent('false');
  });
});

describe('GlobalProgressProvider 两路 OR 逻辑', () => {
  it('req loading=true 时，即使 route=false，overall=true', () => {
    renderProbe();
    act(() => {
      screen.getByTestId('inc').click();
    });
    // route 仍为 false（初始），loading 应为 true
    expect(screen.getByTestId('loading')).toHaveTextContent('true');
  });

  it('route loading=true 时，即使 req=false，overall=true', () => {
    renderProbe();
    act(() => {
      screen.getByTestId('route-on').click();
    });
    expect(screen.getByTestId('loading')).toHaveTextContent('true');
  });

  it('req=true + route=true → dec + route-off → false（两路均归零后才 false）', () => {
    renderProbe();
    act(() => {
      screen.getByTestId('inc').click();
      screen.getByTestId('route-on').click();
    });
    // 只 dec req
    act(() => {
      screen.getByTestId('dec').click();
    });
    // route 仍 true → overall still true
    expect(screen.getByTestId('loading')).toHaveTextContent('true');
    // 再 route-off
    act(() => {
      screen.getByTestId('route-off').click();
    });
    expect(screen.getByTestId('loading')).toHaveTextContent('false');
  });
});
