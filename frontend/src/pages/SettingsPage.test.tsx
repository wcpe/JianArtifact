// 设置页组件测试（FR-87）：加载展示脱敏配置、检查更新展示版本对比、有更新触发升级确认流、
// enabled=false 展示「未启用」且禁用升级、各错误码（409/502/422）友好提示。

import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/react';
import { MantineProvider } from '@mantine/core';
import { SettingsPage } from './SettingsPage';
import * as api from '../api/endpoints';
import { ApiError } from '../api/client';
import type { SettingsView, UpdateCheck } from '../api/types';

/** 在 Mantine Provider 下渲染设置页。 */
function renderPage() {
  return render(
    <MantineProvider>
      <SettingsPage />
    </MantineProvider>,
  );
}

/** 一份启用在线更新、含脱敏代理的设置样例。 */
const 启用样例: SettingsView = {
  current_version: '0.3.0',
  network_proxy: {
    http: 'http://proxy.internal:8080',
    https: 'https://proxy.internal:8443',
    no_proxy: 'localhost,127.0.0.1',
  },
  update: {
    enabled: true,
    repo: 'wcpe/JianArtifact',
    api_base_url: 'https://api.github.com',
    restart_mode: 'self',
    has_token: true,
  },
};

/** 一份未启用在线更新、无代理凭据的设置样例。 */
const 未启用样例: SettingsView = {
  current_version: '0.3.0',
  network_proxy: { http: null, https: null, no_proxy: null },
  update: {
    enabled: false,
    repo: 'wcpe/JianArtifact',
    api_base_url: 'https://api.github.com',
    restart_mode: 'self',
    has_token: false,
  },
};

describe('SettingsPage', () => {
  afterEach(() => vi.restoreAllMocks());

  it('加载后展示脱敏后的网络代理与在线更新配置', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    renderPage();

    await waitFor(() => expect(screen.getByText('网络代理')).toBeInTheDocument());
    // 代理 URL 脱敏后原样展示（不含凭据）
    expect(screen.getByText('http://proxy.internal:8080')).toBeInTheDocument();
    expect(screen.getByText('https://proxy.internal:8443')).toBeInTheDocument();
    // 在线更新区：已启用、仓库源、当前版本
    expect(screen.getByText('已启用')).toBeInTheDocument();
    expect(screen.getByText('wcpe/JianArtifact')).toBeInTheDocument();
    expect(screen.getByText('0.3.0')).toBeInTheDocument();
    // 令牌仅展示「已配置」徽章，绝不回显本体
    expect(screen.getByText('已配置')).toBeInTheDocument();
  });

  it('点检查更新展示版本对比；有更新时出现升级按钮', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    const checkResult: UpdateCheck = {
      current_version: '0.3.0',
      latest_version: '0.4.0',
      update_available: true,
      asset_name: 'jianartifact-x86_64.tar.gz',
      notes: '修复若干问题',
    };
    vi.spyOn(api, 'checkUpdate').mockResolvedValue(checkResult);
    renderPage();

    await waitFor(() => expect(screen.getByText('网络代理')).toBeInTheDocument());
    fireEvent.click(screen.getByText('检查更新'));

    // 版本对比展示 + 有可用更新徽章
    await waitFor(() => expect(screen.getByText('有可用更新')).toBeInTheDocument());
    expect(screen.getByText('0.4.0')).toBeInTheDocument();
    expect(screen.getByText('修复若干问题')).toBeInTheDocument();
    // 有更新时升级按钮出现
    expect(screen.getByText('升级到 v0.4.0')).toBeInTheDocument();
  });

  it('有更新时点升级走二次确认弹窗并调 apply，成功后进入正在重启态', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    vi.spyOn(api, 'checkUpdate').mockResolvedValue({
      current_version: '0.3.0',
      latest_version: '0.4.0',
      update_available: true,
      asset_name: 'asset',
      notes: '',
    });
    const apply = vi
      .spyOn(api, 'applyUpdate')
      .mockResolvedValue({ status: '已更新，正在重启', new_version: '0.4.0' });
    renderPage();

    await waitFor(() => expect(screen.getByText('网络代理')).toBeInTheDocument());
    fireEvent.click(screen.getByText('检查更新'));
    await waitFor(() => expect(screen.getByText('升级到 v0.4.0')).toBeInTheDocument());

    // 点升级 → 弹出确认框 → 点确认升级 → 调 apply
    fireEvent.click(screen.getByText('升级到 v0.4.0'));
    await waitFor(() => expect(screen.getByText('确认升级到新版本')).toBeInTheDocument());
    fireEvent.click(screen.getByText('确认升级'));

    await waitFor(() => expect(apply).toHaveBeenCalledTimes(1));
    // 成功后进入正在重启提示态
    await waitFor(() => expect(screen.getByText('已触发升级')).toBeInTheDocument());
  });

  it('enabled=false 时展示「未启用」且升级相关按钮禁用 / 不可用', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(未启用样例);
    renderPage();

    await waitFor(() => expect(screen.getByText('未启用')).toBeInTheDocument());
    // 未启用提示文案
    expect(screen.getByText('在线更新未启用')).toBeInTheDocument();
    // 检查更新按钮禁用
    const btn = screen.getByText('检查更新').closest('button');
    expect(btn).toBeDisabled();
    // 代理未配置展示
    expect(screen.getAllByText('未配置').length).toBeGreaterThan(0);
  });

  it('检查更新返回 409（未启用）时展示友好提示', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    vi.spyOn(api, 'checkUpdate').mockRejectedValue(new ApiError(409, 'conflict', '在线更新未启用'));
    renderPage();

    await waitFor(() => expect(screen.getByText('网络代理')).toBeInTheDocument());
    fireEvent.click(screen.getByText('检查更新'));
    await waitFor(() => expect(screen.getByText('在线更新未启用')).toBeInTheDocument());
  });

  it('检查更新返回 502（上游不可达）时展示友好提示', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    vi.spyOn(api, 'checkUpdate').mockRejectedValue(
      new ApiError(502, 'bad_gateway', '上游拉取失败'),
    );
    renderPage();

    await waitFor(() => expect(screen.getByText('网络代理')).toBeInTheDocument());
    fireEvent.click(screen.getByText('检查更新'));
    await waitFor(() => expect(screen.getByText('上游拉取失败')).toBeInTheDocument());
  });

  it('应用更新返回 422（校验失败）时展示友好提示且不进入重启态', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    vi.spyOn(api, 'checkUpdate').mockResolvedValue({
      current_version: '0.3.0',
      latest_version: '0.4.0',
      update_available: true,
      asset_name: 'asset',
      notes: '',
    });
    vi.spyOn(api, 'applyUpdate').mockRejectedValue(
      new ApiError(422, 'unprocessable_entity', '下载内容校验和不一致，已拒绝替换'),
    );
    renderPage();

    await waitFor(() => expect(screen.getByText('网络代理')).toBeInTheDocument());
    fireEvent.click(screen.getByText('检查更新'));
    await waitFor(() => expect(screen.getByText('升级到 v0.4.0')).toBeInTheDocument());
    fireEvent.click(screen.getByText('升级到 v0.4.0'));
    await waitFor(() => expect(screen.getByText('确认升级到新版本')).toBeInTheDocument());
    fireEvent.click(screen.getByText('确认升级'));

    await waitFor(() =>
      expect(screen.getByText('下载内容校验和不一致，已拒绝替换')).toBeInTheDocument(),
    );
    // 失败不应进入「已触发升级」重启态
    expect(screen.queryByText('已触发升级')).not.toBeInTheDocument();
  });

  it('加载失败时展示错误提示', async () => {
    vi.spyOn(api, 'getSettings').mockRejectedValue(
      new ApiError(403, 'forbidden', '无权执行该操作'),
    );
    renderPage();
    await waitFor(() => expect(screen.getByText('无权执行该操作')).toBeInTheDocument());
  });
});
