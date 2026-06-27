// 设置页组件测试（FR-87 只读 + FR-88 可编辑热替换 + FR-103 单页堆叠重排）：
// 加载填充脱敏配置到表单、保存调 PATCH 即时生效、检查更新展示版本对比 + release 说明、
// 有更新触发升级确认流、enabled=false 禁用升级、各错误码（409/502/422）友好提示；
// 并校验单页纵向堆叠（去 tab）三块同屏、在线更新高级项默认折叠且可展开。

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
  },
};

describe('SettingsPage', () => {
  afterEach(() => vi.restoreAllMocks());

  it('加载后将脱敏后的网络代理与在线更新配置填入可编辑表单', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    renderPage();

    await waitFor(() => expect(screen.getByText('网络代理')).toBeInTheDocument());
    // 代理 URL 脱敏后填入输入框（不含凭据）
    expect(screen.getByDisplayValue('http://proxy.internal:8080')).toBeInTheDocument();
    expect(screen.getByDisplayValue('https://proxy.internal:8443')).toBeInTheDocument();
    expect(screen.getByDisplayValue('localhost,127.0.0.1')).toBeInTheDocument();
    // 在线更新区：仓库源填入、当前版本展示
    expect(screen.getByDisplayValue('wcpe/JianArtifact')).toBeInTheDocument();
    expect(screen.getByText('0.3.0')).toBeInTheDocument();
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
        },
      }),
    );
    renderPage();

    await waitFor(() => expect(screen.getByText('网络代理')).toBeInTheDocument());
    // 填入新 HTTP 代理 URL（三个代理各有一个「URL」框，HTTP 为第一个）并启用在线更新
    const urlInputs = screen.getAllByLabelText('URL');
    fireEvent.change(urlInputs[0], {
      target: { value: 'http://new-proxy.internal:3128' },
    });
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

    await waitFor(() => expect(screen.getByText('网络代理')).toBeInTheDocument());
    fireEvent.change(screen.getByLabelText('访问令牌（私有仓库可选）'), {
      target: { value: 'ghp_newtoken' },
    });
    fireEvent.click(screen.getByText('保存'));

    await waitFor(() => expect(update).toHaveBeenCalledTimes(1));
    expect(update.mock.calls[0][0].update.token).toBe('ghp_newtoken');
  });

  it('选择 prerelease 通道后保存，PATCH 载荷带 channel=prerelease', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(未启用样例);
    const update = vi.spyOn(api, 'updateSettings').mockResolvedValue(未启用样例);
    renderPage();

    await waitFor(() => expect(screen.getByText('网络代理')).toBeInTheDocument());
    // 打开「更新通道」下拉并选 prerelease（默认 stable，点输入框展开选项）
    fireEvent.click(screen.getByDisplayValue('stable（仅稳定版）'));
    fireEvent.click(await screen.findByText('prerelease（含预发布版）'));
    fireEvent.click(screen.getByText('保存'));

    await waitFor(() => expect(update).toHaveBeenCalledTimes(1));
    expect(update.mock.calls[0][0].update.channel).toBe('prerelease');
  });

  it('加载后将后端返回的 channel 填入更新通道下拉', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue({
      ...启用样例,
      update: { ...启用样例.update, channel: 'prerelease' },
    });
    renderPage();

    await waitFor(() => expect(screen.getByText('网络代理')).toBeInTheDocument());
    // 通道下拉回显 prerelease（Select 输入框展示选中项 label）
    expect(screen.getByDisplayValue('prerelease（含预发布版）')).toBeInTheDocument();
  });

  it('保存返回 400（非法配置）时展示友好提示', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    vi.spyOn(api, 'updateSettings').mockRejectedValue(
      new ApiError(400, 'bad_request', '网络代理配置非法：出站 HTTP 代理配置无效'),
    );
    renderPage();

    await waitFor(() => expect(screen.getByText('网络代理')).toBeInTheDocument());
    fireEvent.click(screen.getByText('保存'));
    await waitFor(() =>
      expect(screen.getByText('网络代理配置非法：出站 HTTP 代理配置无效')).toBeInTheDocument(),
    );
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

    await waitFor(() => expect(screen.getByText('有可用更新')).toBeInTheDocument());
    expect(screen.getByText('0.4.0')).toBeInTheDocument();
    expect(screen.getByText('修复若干问题')).toBeInTheDocument();
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

    fireEvent.click(screen.getByText('升级到 v0.4.0'));
    await waitFor(() => expect(screen.getByText('确认升级到新版本')).toBeInTheDocument());
    fireEvent.click(screen.getByText('确认升级'));

    await waitFor(() => expect(apply).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(screen.getByText('已触发升级')).toBeInTheDocument());
  });

  it('enabled=false 时升级相关按钮禁用 / 不可用', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(未启用样例);
    renderPage();

    await waitFor(() => expect(screen.getByText('在线更新未启用')).toBeInTheDocument());
    // 检查更新按钮禁用
    const btn = screen.getByText('检查更新').closest('button');
    expect(btn).toBeDisabled();
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
    expect(screen.queryByText('已触发升级')).not.toBeInTheDocument();
  });

  it('FR-103：单页纵向堆叠去 tab，网络代理 / 在线更新 / 关于·版本 三块同屏可见', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    renderPage();

    await waitFor(() => expect(screen.getByText('网络代理')).toBeInTheDocument());
    // 去 tab：页面不再有任何 tab 角色元素
    expect(screen.queryAllByRole('tab')).toHaveLength(0);
    // 三块标题同屏可见（无须切换）：网络代理（代理段标题）、在线更新、关于·版本
    expect(screen.getByText('网络代理')).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: '在线更新' })).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: '关于·版本' })).toBeInTheDocument();
    // 代理表单与在线更新「检查与应用更新」同屏（不靠切 tab）
    expect(screen.getByDisplayValue('http://proxy.internal:8080')).toBeInTheDocument();
    expect(screen.getByText('检查与应用更新')).toBeInTheDocument();
  });

  it('FR-103：关于·版本区直接展示当前版本与生效说明（无须切换）', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    renderPage();

    await waitFor(() => expect(screen.getByText('网络代理')).toBeInTheDocument());
    // 关于区展示当前版本与「无须重启」生效说明（单页同屏，无须点击）
    expect(screen.getByText(/运行时即时生效、无须重启/)).toBeInTheDocument();
    expect(screen.getByText('0.3.0')).toBeInTheDocument();
  });

  it('FR-103：在线更新默认仅显「更新通道」+ 检查更新，高级项收进折叠区', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    renderPage();

    await waitFor(() => expect(screen.getByText('网络代理')).toBeInTheDocument());
    // 默认可见：启用开关、更新通道、检查更新按钮
    expect(screen.getByLabelText('启用在线更新（出站开关）')).toBeVisible();
    expect(screen.getByDisplayValue('stable（仅稳定版）')).toBeVisible();
    expect(screen.getByRole('button', { name: '检查更新' })).toBeVisible();
    // 高级设置切换按钮在场
    expect(screen.getByRole('button', { name: /高级设置/ })).toBeInTheDocument();
    // 高级项默认折叠不可见：仓库源 / API 基址 / 重启模式 / 访问令牌
    expect(screen.queryByDisplayValue('wcpe/JianArtifact')).not.toBeVisible();
    expect(screen.queryByLabelText('访问令牌（私有仓库可选）')).not.toBeVisible();
  });

  it('FR-103：点「高级设置」展开后，仓库源 / API 基址 / 重启模式 / 访问令牌可见', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    renderPage();

    await waitFor(() => expect(screen.getByText('网络代理')).toBeInTheDocument());
    fireEvent.click(screen.getByRole('button', { name: /高级设置/ }));

    await waitFor(() => expect(screen.getByDisplayValue('wcpe/JianArtifact')).toBeVisible());
    expect(screen.getByDisplayValue('https://api.github.com')).toBeVisible();
    expect(screen.getByDisplayValue('self（自拉起新进程）')).toBeVisible();
    expect(screen.getByLabelText('访问令牌（私有仓库可选）')).toBeVisible();
  });

  it('保存动作条为 sticky 底部固定条，保存按钮在其内', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    renderPage();

    await waitFor(() => expect(screen.getByText('网络代理')).toBeInTheDocument());
    // 保存按钮所在的固定条容器带 sticky 定位、贴底（bottom:0），不随内容滚动漂移
    const saveBar = screen.getByTestId('settings-save-bar');
    expect(saveBar).toHaveStyle({ position: 'sticky', bottom: '0' });
    // 保存按钮落在该固定条内
    expect(saveBar).toContainElement(screen.getByText('保存').closest('button'));
  });

  it('FR-100：三代理（HTTP / HTTPS / SOCKS5）各渲染 URL / 用户名 / 密码三字段', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    renderPage();

    await waitFor(() => expect(screen.getByText('网络代理')).toBeInTheDocument());
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

    await waitFor(() => expect(screen.getByText('网络代理')).toBeInTheDocument());
    // HTTP 代理 has_password=true → 徽标在场
    expect(screen.getByText('密码已配置')).toBeInTheDocument();
  });

  it('FR-100：密码框留空保存时 PATCH 载荷省略各代理 password', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    const update = vi.spyOn(api, 'updateSettings').mockResolvedValue(启用样例);
    renderPage();

    await waitFor(() => expect(screen.getByText('网络代理')).toBeInTheDocument());
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

    await waitFor(() => expect(screen.getByText('网络代理')).toBeInTheDocument());
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

    await waitFor(() => expect(screen.getByText('网络代理')).toBeInTheDocument());
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

    await waitFor(() => expect(screen.getByText('网络代理')).toBeInTheDocument());
    // HTTP 代理 has_password=true → 有「清除密码」按钮
    fireEvent.click(screen.getByText('清除密码'));
    fireEvent.click(screen.getByText('保存'));

    await waitFor(() => expect(update).toHaveBeenCalledTimes(1));
    expect(update.mock.calls[0][0].network_proxy.http.password).toBe('');
  });

  it('加载失败时展示错误提示', async () => {
    vi.spyOn(api, 'getSettings').mockRejectedValue(
      new ApiError(403, 'forbidden', '无权执行该操作'),
    );
    renderPage();
    await waitFor(() => expect(screen.getByText('无权执行该操作')).toBeInTheDocument());
  });
});
