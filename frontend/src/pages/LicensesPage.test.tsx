// 开源许可页测试（FR-102）：
// 1) 四张统计卡数值正确；
// 2) 运行时 / 开发分组表格各自渲染对应依赖；
// 3) 按包名搜索过滤；
// 4) generated=false 显「未生成」空态降级。

import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MantineProvider } from '@mantine/core';
import { LicensesPage } from './LicensesPage';
import * as api from '../api/endpoints';
import type { LicenseManifest } from '../api/types';

/** 在 Mantine Provider 下渲染许可页。 */
function renderPage() {
  return render(
    <MantineProvider>
      <LicensesPage />
    </MantineProvider>,
  );
}

const 样例清单: LicenseManifest = {
  generated: true,
  entries: [
    {
      name: 'serde',
      version: '1.0.0',
      license: 'MIT OR Apache-2.0',
      author: 'dtolnay',
      kind: 'runtime',
      source: 'rust',
    },
    {
      name: '@mantine/core',
      version: '7.17.0',
      license: 'MIT',
      author: 'Vitaly Rtishchev',
      kind: 'runtime',
      source: 'frontend',
    },
    {
      name: 'tempfile',
      version: '3.27.0',
      license: 'MIT OR Apache-2.0',
      author: 'Steven Allen',
      kind: 'dev',
      source: 'rust',
    },
    {
      name: 'vitest',
      version: '3.0.5',
      license: 'MIT',
      author: 'Anthony Fu',
      kind: 'dev',
      source: 'frontend',
    },
  ],
  summary: { total: 4, runtime: 2, dev: 2, licenses: 2 },
};

describe('LicensesPage', () => {
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('展示四张统计卡数值', async () => {
    vi.spyOn(api, 'getLicenses').mockResolvedValue(样例清单);
    renderPage();

    await waitFor(() => expect(screen.getByText('依赖总数')).toBeInTheDocument());
    // 统计卡：总数 4 / 运行时 2 / 开发 2 / 许可证 2
    expect(screen.getByText('依赖总数').parentElement).toHaveTextContent('4');
    expect(screen.getByText('运行时依赖').parentElement).toHaveTextContent('2');
    expect(screen.getByText('开发依赖').parentElement).toHaveTextContent('2');
    expect(screen.getByText('许可证种类').parentElement).toHaveTextContent('2');
  });

  it('运行时 / 开发分组表格各自渲染', async () => {
    vi.spyOn(api, 'getLicenses').mockResolvedValue(样例清单);
    renderPage();

    await waitFor(() => expect(screen.getByText('serde')).toBeInTheDocument());
    // 运行时依赖在运行时表、开发依赖在开发表
    expect(screen.getByText('serde')).toBeInTheDocument();
    expect(screen.getByText('@mantine/core')).toBeInTheDocument();
    expect(screen.getByText('tempfile')).toBeInTheDocument();
    expect(screen.getByText('vitest')).toBeInTheDocument();
  });

  it('按包名搜索过滤', async () => {
    vi.spyOn(api, 'getLicenses').mockResolvedValue(样例清单);
    renderPage();

    await waitFor(() => expect(screen.getByText('serde')).toBeInTheDocument());
    await userEvent.type(screen.getByLabelText('按包名过滤'), 'serde');

    await waitFor(() => expect(screen.queryByText('tempfile')).not.toBeInTheDocument());
    expect(screen.getByText('serde')).toBeInTheDocument();
    expect(screen.queryByText('vitest')).not.toBeInTheDocument();
  });

  it('过滤大小写不敏感、匹配子串', async () => {
    vi.spyOn(api, 'getLicenses').mockResolvedValue(样例清单);
    renderPage();

    await waitFor(() => expect(screen.getByText('serde')).toBeInTheDocument());
    await userEvent.type(screen.getByLabelText('按包名过滤'), 'MANTINE');

    await waitFor(() => expect(screen.getByText('@mantine/core')).toBeInTheDocument());
    expect(screen.queryByText('serde')).not.toBeInTheDocument();
  });

  it('generated=false 显未生成空态', async () => {
    vi.spyOn(api, 'getLicenses').mockResolvedValue({
      generated: false,
      entries: [],
      summary: { total: 0, runtime: 0, dev: 0, licenses: 0 },
    });
    renderPage();

    await waitFor(() => expect(screen.getByText('许可清单未生成')).toBeInTheDocument());
  });

  it('加载失败显错误提示', async () => {
    vi.spyOn(api, 'getLicenses').mockRejectedValue(new Error('网络错误'));
    renderPage();

    await waitFor(() => expect(screen.getByText(/网络错误/)).toBeInTheDocument());
  });
});
