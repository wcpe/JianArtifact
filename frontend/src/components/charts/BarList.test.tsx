// 手搓横向条形列表测试（FR-99，零依赖 CSS）：渲染各项标签与数值，空数据走空态。

import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';
import { MantineProvider } from '@mantine/core';
import { BarList } from './BarList';

/** 在 Mantine Provider 下渲染（条形用主题色）。 */
function renderBars(items: { label: string; value: number }[]) {
  return render(
    <MantineProvider>
      <BarList items={items} emptyText="暂无数据" />
    </MantineProvider>,
  );
}

describe('BarList', () => {
  it('渲染每一项的标签与数值', () => {
    renderBars([
      { label: 'libs', value: 9 },
      { label: 'npm-proxy', value: 3 },
    ]);
    expect(screen.getByText('libs')).toBeInTheDocument();
    expect(screen.getByText('9')).toBeInTheDocument();
    expect(screen.getByText('npm-proxy')).toBeInTheDocument();
    expect(screen.getByText('3')).toBeInTheDocument();
  });

  it('空数据展示空态文案', () => {
    renderBars([]);
    expect(screen.getByText('暂无数据')).toBeInTheDocument();
  });
});
