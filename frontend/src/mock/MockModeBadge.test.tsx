// Mock 模式徽标测试（FR-119）：开启时渲染徽标 + 关闭按钮，关闭即清开关并刷新；未开启不渲染。

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MantineProvider } from '@mantine/core';
import { MockModeBadge } from './MockModeBadge';

const FLAG_KEY = 'jianartifact.mock';

const originalLocationDescriptor = Object.getOwnPropertyDescriptor(window, 'location');

function renderBadge() {
  return render(
    <MantineProvider>
      <MockModeBadge />
    </MantineProvider>,
  );
}

describe('MockModeBadge（FR-119）', () => {
  beforeEach(() => {
    localStorage.clear();
    vi.unstubAllEnvs();
  });
  afterEach(() => {
    vi.restoreAllMocks();
    vi.unstubAllEnvs();
    if (originalLocationDescriptor) {
      Object.defineProperty(window, 'location', originalLocationDescriptor);
    }
  });

  it('Mock 模式关闭时不渲染徽标', () => {
    renderBadge();
    expect(screen.queryByText('Mock 模式')).not.toBeInTheDocument();
    expect(screen.queryByLabelText('关闭 Mock 模式')).not.toBeInTheDocument();
  });

  it('Mock 模式开启时渲染醒目徽标', () => {
    localStorage.setItem(FLAG_KEY, 'on');
    renderBadge();
    expect(screen.getByText('Mock 模式')).toBeInTheDocument();
  });

  it('点关闭按钮：清开关并触发刷新', async () => {
    localStorage.setItem(FLAG_KEY, 'on');
    const reload = vi.fn();
    Object.defineProperty(window, 'location', {
      value: { ...window.location, reload },
      writable: true,
      configurable: true,
    });
    const user = userEvent.setup();
    renderBadge();

    await user.click(screen.getByLabelText('关闭 Mock 模式'));
    expect(localStorage.getItem(FLAG_KEY)).toBeNull();
    expect(reload).toHaveBeenCalled();
  });
});
