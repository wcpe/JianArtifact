// 手搓环形占比图测试（FR-99，零依赖 SVG）：渲染中心百分比文案、按 value 钳制到 0~100。

import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';
import { RingChart } from './RingChart';

describe('RingChart', () => {
  it('渲染中心百分比与标签', () => {
    render(<RingChart value={42} label="CPU" />);
    expect(screen.getByText('42%')).toBeInTheDocument();
    expect(screen.getByText('CPU')).toBeInTheDocument();
  });

  it('value 超出 0~100 时钳制展示', () => {
    const { rerender } = render(<RingChart value={150} label="内存" />);
    expect(screen.getByText('100%')).toBeInTheDocument();
    rerender(<RingChart value={-10} label="内存" />);
    expect(screen.getByText('0%')).toBeInTheDocument();
  });

  it('提供无障碍名（role=img + aria-label）', () => {
    render(<RingChart value={30} label="磁盘" />);
    expect(screen.getByRole('img', { name: /磁盘/ })).toBeInTheDocument();
  });
});
