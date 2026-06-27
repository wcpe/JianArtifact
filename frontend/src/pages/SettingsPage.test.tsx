// 设置页组件测试（FR-87 只读 + FR-88 可编辑热替换 + FR-103 二级导航重设计）：
// 加载填充脱敏配置到表单、保存调 PATCH 即时生效、检查更新展示版本对比 + release 说明、
// 有更新 / 预发布 / 回滚各态、enabled=false 禁用升级、各错误码（400/409/502/422）友好提示；
// 并校验左侧二级导航（网络代理 / 在线更新两 tab）+ 在线更新「应用更新」卡片各态 + 高级项默认折叠可展开。

import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/react';
import { MantineProvider } from '@mantine/core';
import { SettingsPage } from './SettingsPage';
import * as api from '../api/endpoints';
import { ApiError } from '../api/client';
import type { SettingsView, UpdateCheck, DynamicConfig } from '../api/types';

/** 在 Mantine Provider 下渲染设置页。 */
function renderPage() {
  return render(
    <MantineProvider>
      <SettingsPage />
    </MantineProvider>,
  );
}

/** 切到「在线更新」二级 tab（默认在「网络代理」tab）。 */
function 切到在线更新() {
  fireEvent.click(screen.getByRole('tab', { name: '在线更新' }));
}

/** 切到「系统配置」二级 tab（FR-106 动态配置面板）。 */
function 切到系统配置() {
  fireEvent.click(screen.getByRole('tab', { name: '系统配置' }));
}

/** 一份默认动态配置样例（FR-106）。 */
const 动态配置样例: DynamicConfig = {
  limits: { max_artifact_size: null },
  audit: { retention_days: 90, max_rows: 1000000 },
  usage: { detail_enabled: false, max_detail_rows: 1000000 },
  metrics: { enabled: true, allow_anonymous: false },
  metrics_timeseries: {
    enabled: true,
    sample_interval_secs: 60,
    retention_days: 7,
    max_rows: 1000000,
  },
  vuln: {
    enabled: false,
    source_base_url: 'https://osv.example',
    ecosystems: [],
    refresh_interval_secs: 86400,
    download_timeout_secs: 600,
  },
  auth: { session_ttl_secs: 3600, login_max_failures: 5, login_lockout_secs: 900 },
};

/** 一份启用在线更新、含脱敏代理的设置样例。 */
const 启用样例: SettingsView = {
  current_version: '0.3.0',
  network_proxy: {
    http: { url: 'http://proxy.internal:8080', username: 'alice', has_password: true },
    https: { url: 'https://proxy.internal:8443', username: null, has_password: false },
    all: { url: 'socks5://proxy.internal:1080', username: null, has_password: false },
    no_proxy: 'localhost,127.0.0.1',
  },
  update: {
    enabled: true,
    repo: 'wcpe/JianArtifact',
    api_base_url: 'https://api.github.com',
    restart_mode: 'self',
    channel: 'stable',
    has_token: true,
    rollback_available: true,
  },
};

/** 一份未启用在线更新、无代理凭据的设置样例。 */
const 空代理项 = { url: null, username: null, has_password: false };
const 未启用样例: SettingsView = {
  current_version: '0.3.0',
  network_proxy: {
    http: { ...空代理项 },
    https: { ...空代理项 },
    all: { ...空代理项 },
    no_proxy: null,
  },
  update: {
    enabled: false,
    repo: 'wcpe/JianArtifact',
    api_base_url: 'https://api.github.com',
    restart_mode: 'self',
    channel: 'stable',
    has_token: false,
    rollback_available: false,
  },
};

describe('SettingsPage', () => {
  afterEach(() => vi.restoreAllMocks());

  it('加载后将脱敏后的网络代理与在线更新配置填入可编辑表单', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    // 代理 URL 脱敏后填入输入框（不含凭据）
    expect(screen.getByDisplayValue('http://proxy.internal:8080')).toBeInTheDocument();
    expect(screen.getByDisplayValue('https://proxy.internal:8443')).toBeInTheDocument();
    expect(screen.getByDisplayValue('localhost,127.0.0.1')).toBeInTheDocument();
    // 在线更新区：仓库源填入（高级项，DOM 中即可）
    expect(screen.getByDisplayValue('wcpe/JianArtifact')).toBeInTheDocument();
    // 令牌已配置：description 提示不回显本体
    expect(screen.getByText(/已配置令牌（不回显）/)).toBeInTheDocument();
  });

  it('编辑代理与启用更新后点保存调 updateSettings，成功展示已保存', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(未启用样例);
    const update = vi.spyOn(api, 'updateSettings').mockImplementation((p) =>
      Promise.resolve({
        current_version: '0.3.0',
        // 回放为视图形态（脱敏）：仅据 patch 的 url 是否填写决定回显，密码统一不回显
        network_proxy: {
          http: { url: p.network_proxy.http.url || null, username: null, has_password: false },
          https: { url: p.network_proxy.https.url || null, username: null, has_password: false },
          all: { url: p.network_proxy.all.url || null, username: null, has_password: false },
          no_proxy: p.network_proxy.no_proxy || null,
        },
        update: {
          enabled: p.update.enabled,
          repo: p.update.repo,
          api_base_url: p.update.api_base_url,
          restart_mode: p.update.restart_mode,
          channel: p.update.channel,
          has_token: Boolean(p.update.token),
          rollback_available: false,
        },
      }),
    );
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    // 填入新 HTTP 代理 URL（三个代理各有一个「URL」框，HTTP 为第一个）
    const urlInputs = screen.getAllByLabelText('URL');
    fireEvent.change(urlInputs[0], {
      target: { value: 'http://new-proxy.internal:3128' },
    });
    // 切到在线更新 tab 启用开关
    切到在线更新();
    fireEvent.click(screen.getByLabelText('启用在线更新（出站开关）'));
    fireEvent.click(screen.getByText('保存'));

    await waitFor(() => expect(update).toHaveBeenCalledTimes(1));
    // 断言 PATCH 载荷：新代理 URL + enabled 翻为 true；未填密码则省略 password 字段；未填 token 则省略 token
    const payload = update.mock.calls[0][0];
    expect(payload.network_proxy.http.url).toBe('http://new-proxy.internal:3128');
    expect(payload.network_proxy.http.password).toBeUndefined();
    expect(payload.update.enabled).toBe(true);
    expect(payload.update.token).toBeUndefined();
    // FR-89：默认通道 stable 随 PATCH 一并提交
    expect(payload.update.channel).toBe('stable');
    // 成功提示
    await waitFor(() => expect(screen.getByText('已保存，配置已即时生效。')).toBeInTheDocument());
  });

  it('填入新令牌保存时 PATCH 载荷带 token', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(未启用样例);
    const update = vi.spyOn(api, 'updateSettings').mockResolvedValue(未启用样例);
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    切到在线更新();
    // 令牌在「高级设置」折叠区内，先展开
    fireEvent.click(screen.getByRole('button', { name: /高级设置/ }));
    fireEvent.change(screen.getByLabelText('访问令牌（私有仓库可选）'), {
      target: { value: 'ghp_newtoken' },
    });
    fireEvent.click(screen.getByText('保存'));

    await waitFor(() => expect(update).toHaveBeenCalledTimes(1));
    expect(update.mock.calls[0][0].update.token).toBe('ghp_newtoken');
  });

  it('切「测试版」通道后保存，PATCH 载荷带 channel=prerelease', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(未启用样例);
    const update = vi.spyOn(api, 'updateSettings').mockResolvedValue(未启用样例);
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    切到在线更新();
    // 通道切换为 segmented：点「测试版」
    fireEvent.click(screen.getByText('测试版'));
    fireEvent.click(screen.getByText('保存'));

    await waitFor(() => expect(update).toHaveBeenCalledTimes(1));
    expect(update.mock.calls[0][0].update.channel).toBe('prerelease');
  });

  it('加载后将后端返回的 channel 填入通道切换（测试版选中）', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue({
      ...启用样例,
      update: { ...启用样例.update, channel: 'prerelease' },
    });
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    切到在线更新();
    // prerelease 通道：预发布徽标在场（说明 channel 已回显为 prerelease）
    expect(screen.getByText('预发布')).toBeInTheDocument();
  });

  it('保存返回 400（非法配置）时展示友好提示', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    vi.spyOn(api, 'updateSettings').mockRejectedValue(
      new ApiError(400, 'bad_request', '网络代理配置非法：出站 HTTP 代理配置无效'),
    );
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    fireEvent.click(screen.getByText('保存'));
    await waitFor(() =>
      expect(screen.getByText('网络代理配置非法：出站 HTTP 代理配置无效')).toBeInTheDocument(),
    );
  });

  it('点检查更新展示版本对比；有更新时出现立即更新按钮', async () => {
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

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    切到在线更新();
    fireEvent.click(screen.getByText('检查更新'));

    await waitFor(() => expect(screen.getByText('有可用更新')).toBeInTheDocument());
    expect(screen.getByText('0.4.0')).toBeInTheDocument();
    expect(screen.getByText('修复若干问题')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /立即更新并重启/ })).toBeInTheDocument();
  });

  it('有更新时点立即更新走二次确认弹窗并调 apply，成功后进入正在重启态', async () => {
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

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    切到在线更新();
    fireEvent.click(screen.getByText('检查更新'));
    await waitFor(() =>
      expect(screen.getByRole('button', { name: /立即更新并重启/ })).toBeInTheDocument(),
    );

    fireEvent.click(screen.getByRole('button', { name: /立即更新并重启/ }));
    await waitFor(() => expect(screen.getByText('确认升级到新版本')).toBeInTheDocument());
    fireEvent.click(screen.getByText('确认升级'));

    await waitFor(() => expect(apply).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(screen.getByText('已触发升级')).toBeInTheDocument());
  });

  it('enabled=false 时升级相关按钮禁用 / 不可用', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(未启用样例);
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    切到在线更新();
    await waitFor(() => expect(screen.getByText('在线更新未启用')).toBeInTheDocument());
    // 检查更新按钮禁用
    const btn = screen.getByText('检查更新').closest('button');
    expect(btn).toBeDisabled();
  });

  it('检查更新返回 409（未启用）时展示友好提示', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    vi.spyOn(api, 'checkUpdate').mockRejectedValue(new ApiError(409, 'conflict', '在线更新未启用'));
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    切到在线更新();
    fireEvent.click(screen.getByText('检查更新'));
    await waitFor(() => expect(screen.getByText('在线更新未启用')).toBeInTheDocument());
  });

  it('检查更新返回 502（上游不可达）时展示友好提示', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    vi.spyOn(api, 'checkUpdate').mockRejectedValue(
      new ApiError(502, 'bad_gateway', '上游拉取失败'),
    );
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    切到在线更新();
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

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    切到在线更新();
    fireEvent.click(screen.getByText('检查更新'));
    await waitFor(() =>
      expect(screen.getByRole('button', { name: /立即更新并重启/ })).toBeInTheDocument(),
    );
    fireEvent.click(screen.getByRole('button', { name: /立即更新并重启/ }));
    await waitFor(() => expect(screen.getByText('确认升级到新版本')).toBeInTheDocument());
    fireEvent.click(screen.getByText('确认升级'));

    await waitFor(() =>
      expect(screen.getByText('下载内容校验和不一致，已拒绝替换')).toBeInTheDocument(),
    );
    expect(screen.queryByText('已触发升级')).not.toBeInTheDocument();
  });

  it('FR-103：左侧二级导航有「网络代理」「在线更新」两 tab，默认网络代理面板可见', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    // 两个二级导航 tab 在场
    expect(screen.getByRole('tab', { name: '网络代理' })).toBeInTheDocument();
    expect(screen.getByRole('tab', { name: '在线更新' })).toBeInTheDocument();
    // 默认网络代理面板可见：代理 URL 输入框可见
    expect(screen.getByDisplayValue('http://proxy.internal:8080')).toBeVisible();
    // 在线更新面板默认未激活（Mantine Tabs 隐藏非激活面板）：应用更新卡片标题不可见
    expect(screen.getByText('应用更新')).not.toBeVisible();
  });

  it('FR-103：切到「在线更新」tab 显应用更新卡片（标题 + 通道切换 + 检查更新）', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    切到在线更新();
    // 应用更新卡片标题、通道切换（正式版 / 测试版）、检查更新按钮可见
    await waitFor(() => expect(screen.getByRole('heading', { name: '应用更新' })).toBeVisible());
    expect(screen.getByText('正式版')).toBeVisible();
    expect(screen.getByText('测试版')).toBeVisible();
    expect(screen.getByRole('button', { name: '检查更新' })).toBeVisible();
    expect(screen.getByLabelText('启用在线更新（出站开关）')).toBeVisible();
  });

  it('FR-103：通道切「测试版」显「预发布」徽标与预发布提示框', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    切到在线更新();
    // 默认 stable：无预发布徽标 / 提示
    expect(screen.queryByText('预发布')).not.toBeInTheDocument();
    // 切测试版
    fireEvent.click(screen.getByText('测试版'));
    await waitFor(() => expect(screen.getByText('预发布')).toBeInTheDocument());
    expect(screen.getByText(/滚动开发预览/)).toBeInTheDocument();
  });

  it('FR-103：在线更新高级项默认折叠不可见，点「高级设置」展开后可见', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    切到在线更新();
    // 高级项默认折叠不可见：仓库源 / 访问令牌
    expect(screen.queryByDisplayValue('wcpe/JianArtifact')).not.toBeVisible();
    expect(screen.queryByLabelText('访问令牌（私有仓库可选）')).not.toBeVisible();
    // 点高级设置展开
    fireEvent.click(screen.getByRole('button', { name: /高级设置/ }));
    await waitFor(() => expect(screen.getByDisplayValue('wcpe/JianArtifact')).toBeVisible());
    expect(screen.getByDisplayValue('https://api.github.com')).toBeVisible();
    expect(screen.getByDisplayValue('self（自拉起新进程）')).toBeVisible();
    expect(screen.getByLabelText('访问令牌（私有仓库可选）')).toBeVisible();
  });

  it('FR-103：切 tab 时底部保存条始终在场且 sticky 贴底（位置稳定）', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    // 网络代理 tab 下保存条在场且 sticky 贴底
    let saveBar = screen.getByTestId('settings-save-bar');
    expect(saveBar).toHaveStyle({ position: 'sticky', bottom: '0' });
    expect(saveBar).toContainElement(screen.getByText('保存').closest('button'));
    // 切到在线更新 tab 后保存条仍在场、仍 sticky 贴底
    切到在线更新();
    saveBar = screen.getByTestId('settings-save-bar');
    expect(saveBar).toHaveStyle({ position: 'sticky', bottom: '0' });
    expect(saveBar).toContainElement(screen.getByText('保存').closest('button'));
  });

  it('FR-100：三代理（HTTP / HTTPS / SOCKS5）各渲染 URL / 用户名 / 密码三字段', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    // 三组标题均在场
    expect(screen.getByText('HTTP 代理')).toBeInTheDocument();
    expect(screen.getByText('HTTPS 代理')).toBeInTheDocument();
    expect(screen.getByText(/SOCKS5 代理/)).toBeInTheDocument();
    // 每代理三字段：URL / 用户名 / 密码 各三个
    expect(screen.getAllByLabelText('URL')).toHaveLength(3);
    expect(screen.getAllByLabelText('用户名')).toHaveLength(3);
    expect(screen.getAllByLabelText('密码')).toHaveLength(3);
  });

  it('FR-100：has_password 为真时展示「密码已配置」徽标', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    // HTTP 代理 has_password=true → 徽标在场
    expect(screen.getByText('密码已配置')).toBeInTheDocument();
  });

  it('FR-100：密码框留空保存时 PATCH 载荷省略各代理 password', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    const update = vi.spyOn(api, 'updateSettings').mockResolvedValue(启用样例);
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    fireEvent.click(screen.getByText('保存'));

    await waitFor(() => expect(update).toHaveBeenCalledTimes(1));
    const np = update.mock.calls[0][0].network_proxy;
    expect(np.http.password).toBeUndefined();
    expect(np.https.password).toBeUndefined();
    expect(np.all.password).toBeUndefined();
    // 用户名照填回显值
    expect(np.http.username).toBe('alice');
  });

  it('FR-100：填入密码保存时对应代理 PATCH 载荷带 password', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    const update = vi.spyOn(api, 'updateSettings').mockResolvedValue(启用样例);
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    // 给 HTTP 代理（第一个密码框）填入新密码
    const passInputs = screen.getAllByLabelText('密码');
    fireEvent.change(passInputs[0], { target: { value: 's3cret' } });
    fireEvent.click(screen.getByText('保存'));

    await waitFor(() => expect(update).toHaveBeenCalledTimes(1));
    const np = update.mock.calls[0][0].network_proxy;
    expect(np.http.password).toBe('s3cret');
    // 其余代理仍省略 password
    expect(np.https.password).toBeUndefined();
    expect(np.all.password).toBeUndefined();
  });

  it('FR-100：SOCKS5 的 URL 能填入并组装到 all 槽', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(未启用样例);
    const update = vi.spyOn(api, 'updateSettings').mockResolvedValue(未启用样例);
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    // 第三个 URL 框为 SOCKS5(all)
    const urlInputs = screen.getAllByLabelText('URL');
    fireEvent.change(urlInputs[2], { target: { value: 'socks5://socks.internal:1080' } });
    fireEvent.click(screen.getByText('保存'));

    await waitFor(() => expect(update).toHaveBeenCalledTimes(1));
    expect(update.mock.calls[0][0].network_proxy.all.url).toBe('socks5://socks.internal:1080');
  });

  it('FR-100：点「清除密码」后保存，对应代理 PATCH 载荷带 password 空串', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    const update = vi.spyOn(api, 'updateSettings').mockResolvedValue(启用样例);
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    // HTTP 代理 has_password=true → 有「清除密码」按钮
    fireEvent.click(screen.getByText('清除密码'));
    fireEvent.click(screen.getByText('保存'));

    await waitFor(() => expect(update).toHaveBeenCalledTimes(1));
    expect(update.mock.calls[0][0].network_proxy.http.password).toBe('');
  });

  it('FR-104：有回滚备份时回滚按钮可用，点回滚走二次确认并调 rollback，成功后进入正在重启态', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    const rollback = vi
      .spyOn(api, 'rollbackUpdate')
      .mockResolvedValue({ status: '已回滚，正在重启' });
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    切到在线更新();
    const btn = screen.getByText('回滚到上一版').closest('button');
    expect(btn).not.toBeDisabled();

    fireEvent.click(screen.getByText('回滚到上一版'));
    await waitFor(() => expect(screen.getByText('确认回滚到上一版本')).toBeInTheDocument());
    fireEvent.click(screen.getByText('确认回滚'));

    await waitFor(() => expect(rollback).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(screen.getByText('已触发升级')).toBeInTheDocument());
  });

  it('FR-104：无回滚备份时回滚按钮禁用并提示暂无备份', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue({
      ...启用样例,
      update: { ...启用样例.update, rollback_available: false },
    });
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    切到在线更新();
    const btn = screen.getByText('回滚到上一版').closest('button');
    expect(btn).toBeDisabled();
    expect(screen.getByText(/暂无可回滚的备份版本/)).toBeInTheDocument();
  });

  it('FR-104：回滚返回 409（无备份）时展示友好提示且不进入重启态', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    vi.spyOn(api, 'rollbackUpdate').mockRejectedValue(
      new ApiError(409, 'conflict', '无可回滚的备份版本'),
    );
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    切到在线更新();
    fireEvent.click(screen.getByText('回滚到上一版'));
    await waitFor(() => expect(screen.getByText('确认回滚到上一版本')).toBeInTheDocument());
    fireEvent.click(screen.getByText('确认回滚'));

    await waitFor(() => expect(screen.getByText('无可回滚的备份版本')).toBeInTheDocument());
    expect(screen.queryByText('已触发升级')).not.toBeInTheDocument();
  });

  it('加载失败时展示错误提示', async () => {
    vi.spyOn(api, 'getSettings').mockRejectedValue(
      new ApiError(403, 'forbidden', '无权执行该操作'),
    );
    vi.spyOn(api, 'getDynamicConfig').mockResolvedValue(动态配置样例);
    renderPage();
    await waitFor(() => expect(screen.getByText('无权执行该操作')).toBeInTheDocument());
  });

  // ===== FR-106：系统配置（动态配置）tab =====

  it('FR-106：有「系统配置」tab，切入后渲染各节表单并显「保存后重启生效」标注', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    vi.spyOn(api, 'getDynamicConfig').mockResolvedValue(动态配置样例);
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    expect(screen.getByRole('tab', { name: '系统配置' })).toBeInTheDocument();
    切到系统配置();

    // 重启生效标注（区别于代理 / 更新即时生效）
    await waitFor(() => expect(screen.getByRole('heading', { name: '系统配置' })).toBeVisible());
    expect(screen.getByText('保存后重启生效')).toBeVisible();
    // 各节表单回显默认值：会话有效期 3600、审计保留 90、采样间隔 60
    expect(screen.getByDisplayValue('3600')).toBeVisible();
    expect(screen.getByDisplayValue('90')).toBeVisible();
    expect(screen.getByDisplayValue('60')).toBeVisible();
  });

  it('FR-106：编辑系统配置后点「保存系统配置」调 updateDynamicConfig，成功显已保存重启生效', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    vi.spyOn(api, 'getDynamicConfig').mockResolvedValue(动态配置样例);
    const update = vi
      .spyOn(api, 'updateDynamicConfig')
      .mockImplementation((c) => Promise.resolve(c));
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    切到系统配置();
    await waitFor(() => expect(screen.getByLabelText('会话有效期（秒）')).toBeVisible());

    // 改会话有效期为 7200
    fireEvent.change(screen.getByLabelText('会话有效期（秒）'), { target: { value: '7200' } });
    fireEvent.click(screen.getByRole('button', { name: '保存系统配置' }));

    await waitFor(() => expect(update).toHaveBeenCalledTimes(1));
    expect(update.mock.calls[0][0].auth.session_ttl_secs).toBe(7200);
    await waitFor(() => expect(screen.getByText('已保存，重启服务后生效。')).toBeInTheDocument());
  });

  it('FR-106：系统配置保存返回 400（非法值）时展示友好提示', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    vi.spyOn(api, 'getDynamicConfig').mockResolvedValue(动态配置样例);
    vi.spyOn(api, 'updateDynamicConfig').mockRejectedValue(
      new ApiError(
        400,
        'bad_request',
        '动态配置非法：会话有效期（auth.session_ttl_secs）必须大于 0',
      ),
    );
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    切到系统配置();
    await waitFor(() => expect(screen.getByRole('button', { name: '保存系统配置' })).toBeVisible());
    fireEvent.click(screen.getByRole('button', { name: '保存系统配置' }));

    await waitFor(() =>
      expect(
        screen.getByText('动态配置非法：会话有效期（auth.session_ttl_secs）必须大于 0'),
      ).toBeInTheDocument(),
    );
  });
});
