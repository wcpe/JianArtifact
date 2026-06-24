// 使用分析数据面板组件测试：加载聚合数据后展示总览、热门制品与仓库用量；失败展示错误文案。

import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { MantineProvider } from '@mantine/core';
import { AnalyticsPage } from './AnalyticsPage';
import * as api from '../api/endpoints';
import { ApiError } from '../api/client';
import type { UsageAnalyticsDto } from '../api/types';

/** 在 Mantine Provider 下渲染分析页。 */
function renderPage() {
  return render(
    <MantineProvider>
      <AnalyticsPage />
    </MantineProvider>,
  );
}

const 样例数据: UsageAnalyticsDto = {
  total_access: 12,
  total_download: 34,
  top_downloads: [
    { repo_name: 'libs', repo_path: 'a/b.jar', count: 9, last_at: '2026-06-24T00:00:00Z' },
  ],
  repo_usage: [
    { repo_name: 'libs', count: 9 },
    { repo_name: 'npm-proxy', count: 3 },
  ],
};

describe('AnalyticsPage', () => {
  afterEach(() => vi.restoreAllMocks());

  it('加载后展示访问 / 下载总量与热门制品、仓库用量', async () => {
    vi.spyOn(api, 'usageAnalytics').mockResolvedValue(样例数据);
    renderPage();

    // 总览数值
    await waitFor(() => expect(screen.getByText('12')).toBeInTheDocument());
    expect(screen.getByText('34')).toBeInTheDocument();
    // 热门制品行
    expect(screen.getByText('a/b.jar')).toBeInTheDocument();
    // 仓库用量项
    expect(screen.getByText('npm-proxy')).toBeInTheDocument();
  });

  it('无下载记录时展示空态文案', async () => {
    vi.spyOn(api, 'usageAnalytics').mockResolvedValue({
      total_access: 0,
      total_download: 0,
      top_downloads: [],
      repo_usage: [],
    });
    renderPage();

    await waitFor(() => expect(screen.getAllByText('暂无下载记录').length).toBeGreaterThan(0));
  });

  it('请求失败时展示错误提示', async () => {
    vi.spyOn(api, 'usageAnalytics').mockRejectedValue(
      new ApiError(403, 'forbidden', '无权执行该操作'),
    );
    renderPage();

    await waitFor(() => expect(screen.getByText('无权执行该操作')).toBeInTheDocument());
  });
});
