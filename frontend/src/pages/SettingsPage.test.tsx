// 设置页组件测试（FR-87 只读 + FR-88 可编辑热替换 + FR-103 锚点单页重做 + FR-106 动态配置）：
// 加载填充脱敏配置到表单、单个全局保存一次性提交 settings + dynamic 两次 PATCH、
// 检查更新展示版本对比 + release 说明、有更新 / 预发布 / 回滚各态、enabled=false 禁用升级、
// 立即更新确认后显应用进度条、各错误码（400/409/502/422）友好提示；
// 并校验左侧 sticky 锚点子导航（六项，点击平滑滚动）+ 单页分节（各节标题可见、非 tab 隐藏）+
// 在线更新「应用更新」卡片各态 + 高级项默认折叠可展开 + **只有一个保存按钮**。

import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, waitFor, fireEvent, within } from '@testing-library/react';
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

/** 默认对 getDynamicConfig 打桩（多数用例不关心动态配置，但页面会加载它）。 */
function 桩动态配置(config: DynamicConfig = 动态配置样例) {
  return vi.spyOn(api, 'getDynamicConfig').mockResolvedValue(config);
}

describe('SettingsPage', () => {
  afterEach(() => vi.restoreAllMocks());

  it('加载后将脱敏后的网络代理与在线更新配置填入可编辑表单', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    桩动态配置();
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

  // ===== FR-103：锚点单页骨架 =====

  it('FR-103：左侧锚点导航有六项（网络代理 / 在线更新 / 限制与配额 / 可观测性 / 漏洞库 / 安全·会话）', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    桩动态配置();
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    // 导航在 <nav> 内（与正文同名标题区分）
    const nav = screen.getByRole('navigation', { name: '设置分节导航' });
    expect(within(nav).getByText('网络代理')).toBeInTheDocument();
    expect(within(nav).getByText('在线更新')).toBeInTheDocument();
    expect(within(nav).getByText('限制与配额')).toBeInTheDocument();
    expect(within(nav).getByText('可观测性')).toBeInTheDocument();
    expect(within(nav).getByText('漏洞库')).toBeInTheDocument();
    expect(within(nav).getByText('安全 / 会话')).toBeInTheDocument();
  });

  it('FR-103：点击锚点导航项平滑滚动到对应节（调 scrollIntoView）', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    桩动态配置();
    // scrollIntoView 已在 setup 全局桩，这里 spy 以断言被调用且传 behavior:'smooth'
    const scrollSpy = vi.spyOn(Element.prototype, 'scrollIntoView');
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    const nav = screen.getByRole('navigation', { name: '设置分节导航' });
    fireEvent.click(within(nav).getByText('漏洞库'));

    expect(scrollSpy).toHaveBeenCalled();
    const arg = scrollSpy.mock.calls[scrollSpy.mock.calls.length - 1][0];
    expect(arg).toMatchObject({ behavior: 'smooth' });
  });

  it('FR-103：单页分节——各节标题默认即可见（非 tab 隐藏），无需切换', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    桩动态配置();
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    // 网络代理 + 限制与配额 + 漏洞库 + 安全/会话 节标题（heading）默认可见，无须点 tab
    expect(screen.getByRole('heading', { name: '网络代理' })).toBeVisible();
    expect(screen.getByRole('heading', { name: '限制与配额' })).toBeVisible();
    expect(screen.getByRole('heading', { name: '漏洞库' })).toBeVisible();
    expect(screen.getByRole('heading', { name: '安全 / 会话' })).toBeVisible();
    // 应用更新卡片标题也在场可见（在线更新节）
    expect(screen.getByRole('heading', { name: '应用更新' })).toBeVisible();
  });

  it('FR-103：只有一个保存按钮——保存条内有「保存」、且不存在「保存系统配置」按钮', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    桩动态配置();
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    // 保存条在场且 sticky 贴底，内含唯一「保存」按钮
    const saveBar = screen.getByTestId('settings-save-bar');
    expect(saveBar).toHaveStyle({ position: 'sticky', bottom: '0' });
    expect(within(saveBar).getByRole('button', { name: '保存' })).toBeInTheDocument();
    // 旧的「保存系统配置」按钮已去除：整页不存在
    expect(screen.queryByRole('button', { name: '保存系统配置' })).not.toBeInTheDocument();
    // 整页文案恰为「保存」的按钮只此一个（不含检查更新 / 立即更新 / 回滚等其它按钮）
    const allButtons = screen.getAllByRole('button');
    const saveButtons = allButtons.filter((b) => b.textContent?.trim() === '保存');
    expect(saveButtons).toHaveLength(1);
  });

  it('FR-103：在线更新卡片显启用开关、通道切换、检查更新；默认 stable 无预发布徽标', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    桩动态配置();
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    expect(screen.getByLabelText('启用在线更新（出站开关）')).toBeVisible();
    expect(screen.getByText('正式版')).toBeVisible();
    expect(screen.getByText('测试版')).toBeVisible();
    expect(screen.getByRole('button', { name: '检查更新' })).toBeVisible();
    // 默认 stable：无预发布徽标
    expect(screen.queryByText('预发布')).not.toBeInTheDocument();
  });

  it('FR-103：通道切「测试版」显「预发布」徽标与预发布提示框', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    桩动态配置();
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    fireEvent.click(screen.getByText('测试版'));
    await waitFor(() => expect(screen.getByText('预发布')).toBeInTheDocument());
    expect(screen.getByText(/滚动开发预览/)).toBeInTheDocument();
  });

  it('FR-103：在线更新高级项默认折叠不可见，点「高级设置」展开后可见', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    桩动态配置();
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
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

  // ===== FR-103：单个全局保存（合并 settings + dynamic 两次 PATCH）=====

  it('FR-103：点保存一次性提交 updateSettings + updateDynamicConfig，成功显已保存', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(未启用样例);
    桩动态配置();
    const update = vi.spyOn(api, 'updateSettings').mockResolvedValue(未启用样例);
    const updateDyn = vi
      .spyOn(api, 'updateDynamicConfig')
      .mockImplementation((c) => Promise.resolve(c));
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    await waitFor(() => expect(screen.getByLabelText('会话有效期（秒）')).toBeVisible());
    fireEvent.click(screen.getByRole('button', { name: '保存' }));

    await waitFor(() => expect(update).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(updateDyn).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(screen.getByText(/已保存/)).toBeInTheDocument());
  });

  it('FR-103：保存时改过的动态配置（会话有效期）随 updateDynamicConfig 一并提交', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    桩动态配置();
    vi.spyOn(api, 'updateSettings').mockResolvedValue(启用样例);
    const updateDyn = vi
      .spyOn(api, 'updateDynamicConfig')
      .mockImplementation((c) => Promise.resolve(c));
    renderPage();

    await waitFor(() => expect(screen.getByLabelText('会话有效期（秒）')).toBeVisible());
    fireEvent.change(screen.getByLabelText('会话有效期（秒）'), { target: { value: '7200' } });
    fireEvent.click(screen.getByRole('button', { name: '保存' }));

    await waitFor(() => expect(updateDyn).toHaveBeenCalledTimes(1));
    expect(updateDyn.mock.calls[0][0].auth.session_ttl_secs).toBe(7200);
  });

  it('FR-103：动态配置未加载（getDynamicConfig 失败）时保存只发 updateSettings，不报错', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    vi.spyOn(api, 'getDynamicConfig').mockRejectedValue(
      new ApiError(403, 'forbidden', '无权执行该操作'),
    );
    const update = vi.spyOn(api, 'updateSettings').mockResolvedValue(启用样例);
    const updateDyn = vi.spyOn(api, 'updateDynamicConfig');
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    fireEvent.click(screen.getByRole('button', { name: '保存' }));

    await waitFor(() => expect(update).toHaveBeenCalledTimes(1));
    expect(updateDyn).not.toHaveBeenCalled();
  });

  it('FR-103：保存（settings PATCH）返回 400 时展示友好提示', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    桩动态配置();
    vi.spyOn(api, 'updateSettings').mockRejectedValue(
      new ApiError(400, 'bad_request', '网络代理配置非法：出站 HTTP 代理配置无效'),
    );
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    fireEvent.click(screen.getByRole('button', { name: '保存' }));
    await waitFor(() =>
      expect(screen.getByText('网络代理配置非法：出站 HTTP 代理配置无效')).toBeInTheDocument(),
    );
  });

  it('FR-103：动态配置保存返回 400（非法值）时展示友好提示', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    桩动态配置();
    vi.spyOn(api, 'updateSettings').mockResolvedValue(启用样例);
    vi.spyOn(api, 'updateDynamicConfig').mockRejectedValue(
      new ApiError(
        400,
        'bad_request',
        '动态配置非法：会话有效期（auth.session_ttl_secs）必须大于 0',
      ),
    );
    renderPage();

    await waitFor(() => expect(screen.getByLabelText('会话有效期（秒）')).toBeVisible());
    fireEvent.click(screen.getByRole('button', { name: '保存' }));

    await waitFor(() =>
      expect(
        screen.getByText('动态配置非法：会话有效期（auth.session_ttl_secs）必须大于 0'),
      ).toBeInTheDocument(),
    );
  });

  // ===== FR-103：应用更新进度条 =====

  it('FR-103：有更新时点立即更新确认后显示应用进度条，apply 成功进入正在重启态', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    桩动态配置();
    vi.spyOn(api, 'checkUpdate').mockResolvedValue({
      current_version: '0.3.0',
      latest_version: '0.4.0',
      update_available: true,
      asset_name: 'jianartifact-x86_64.tar.gz',
      notes: '',
    });
    // apply 延迟 resolve，给进度条出现留窗口
    let resolveApply!: () => void;
    vi.spyOn(api, 'applyUpdate').mockReturnValue(
      new Promise((res) => {
        resolveApply = () => res({ status: '已更新，正在重启', new_version: '0.4.0' });
      }),
    );
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    fireEvent.click(screen.getByText('检查更新'));
    await waitFor(() =>
      expect(screen.getByRole('button', { name: /立即更新并重启/ })).toBeInTheDocument(),
    );
    fireEvent.click(screen.getByRole('button', { name: /立即更新并重启/ }));
    await waitFor(() => expect(screen.getByText('确认升级到新版本')).toBeInTheDocument());
    fireEvent.click(screen.getByText('确认升级'));

    // 应用进度条出现（apply 在途）
    await waitFor(() => expect(screen.getByTestId('apply-progress')).toBeInTheDocument());
    // 放行 apply，进入已触发升级态
    resolveApply();
    await waitFor(() => expect(screen.getByText('已触发升级')).toBeInTheDocument());
  });

  it('点检查更新展示版本对比；有更新时出现立即更新按钮', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    桩动态配置();
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
    fireEvent.click(screen.getByText('检查更新'));

    await waitFor(() => expect(screen.getByText('有可用更新')).toBeInTheDocument());
    expect(screen.getByText('0.4.0')).toBeInTheDocument();
    expect(screen.getByText('修复若干问题')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /立即更新并重启/ })).toBeInTheDocument();
  });

  it('enabled=false 时升级相关按钮禁用 / 不可用', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(未启用样例);
    桩动态配置();
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    await waitFor(() => expect(screen.getByText('在线更新未启用')).toBeInTheDocument());
    // 检查更新按钮禁用
    const btn = screen.getByText('检查更新').closest('button');
    expect(btn).toBeDisabled();
  });

  it('检查更新返回 409（未启用）时展示友好提示', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    桩动态配置();
    vi.spyOn(api, 'checkUpdate').mockRejectedValue(new ApiError(409, 'conflict', '在线更新未启用'));
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    fireEvent.click(screen.getByText('检查更新'));
    await waitFor(() => expect(screen.getByText('在线更新未启用')).toBeInTheDocument());
  });

  it('检查更新返回 502（上游不可达）时展示友好提示', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    桩动态配置();
    vi.spyOn(api, 'checkUpdate').mockRejectedValue(
      new ApiError(502, 'bad_gateway', '上游拉取失败'),
    );
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    fireEvent.click(screen.getByText('检查更新'));
    await waitFor(() => expect(screen.getByText('上游拉取失败')).toBeInTheDocument());
  });

  it('应用更新返回 422（校验失败）时展示友好提示且不进入重启态、进度条撤下', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    桩动态配置();
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
    // 失败后进度条撤下
    expect(screen.queryByTestId('apply-progress')).not.toBeInTheDocument();
  });

  // ===== FR-100：网络代理三字段 / 三态（合并到全局保存，契约不回归）=====

  it('编辑代理 URL 后点保存，PATCH 载荷带新 URL、省略 password、默认通道 stable', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(未启用样例);
    桩动态配置();
    const update = vi.spyOn(api, 'updateSettings').mockResolvedValue(未启用样例);
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    const urlInputs = screen.getAllByLabelText('URL');
    fireEvent.change(urlInputs[0], { target: { value: 'http://new-proxy.internal:3128' } });
    fireEvent.click(screen.getByLabelText('启用在线更新（出站开关）'));
    fireEvent.click(screen.getByRole('button', { name: '保存' }));

    await waitFor(() => expect(update).toHaveBeenCalledTimes(1));
    const payload = update.mock.calls[0][0];
    expect(payload.network_proxy.http.url).toBe('http://new-proxy.internal:3128');
    expect(payload.network_proxy.http.password).toBeUndefined();
    expect(payload.update.enabled).toBe(true);
    expect(payload.update.token).toBeUndefined();
    expect(payload.update.channel).toBe('stable');
  });

  it('FR-100：填入新令牌保存时 PATCH 载荷带 token', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(未启用样例);
    桩动态配置();
    const update = vi.spyOn(api, 'updateSettings').mockResolvedValue(未启用样例);
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    // 令牌在「高级设置」折叠区内，先展开
    fireEvent.click(screen.getByRole('button', { name: /高级设置/ }));
    fireEvent.change(screen.getByLabelText('访问令牌（私有仓库可选）'), {
      target: { value: 'ghp_newtoken' },
    });
    fireEvent.click(screen.getByRole('button', { name: '保存' }));

    await waitFor(() => expect(update).toHaveBeenCalledTimes(1));
    expect(update.mock.calls[0][0].update.token).toBe('ghp_newtoken');
  });

  it('FR-89：切「测试版」通道后保存，PATCH 载荷带 channel=prerelease', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(未启用样例);
    桩动态配置();
    const update = vi.spyOn(api, 'updateSettings').mockResolvedValue(未启用样例);
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    fireEvent.click(screen.getByText('测试版'));
    fireEvent.click(screen.getByRole('button', { name: '保存' }));

    await waitFor(() => expect(update).toHaveBeenCalledTimes(1));
    expect(update.mock.calls[0][0].update.channel).toBe('prerelease');
  });

  it('FR-89：加载后将后端返回的 channel 填入通道切换（测试版选中）', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue({
      ...启用样例,
      update: { ...启用样例.update, channel: 'prerelease' },
    });
    桩动态配置();
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    // prerelease 通道：预发布徽标在场（说明 channel 已回显为 prerelease）
    expect(screen.getByText('预发布')).toBeInTheDocument();
  });

  it('FR-100：三代理（HTTP / HTTPS / SOCKS5）各渲染 URL / 用户名 / 密码三字段', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    桩动态配置();
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    expect(screen.getByText('HTTP 代理')).toBeInTheDocument();
    expect(screen.getByText('HTTPS 代理')).toBeInTheDocument();
    expect(screen.getByText(/SOCKS5 代理/)).toBeInTheDocument();
    expect(screen.getAllByLabelText('URL')).toHaveLength(3);
    expect(screen.getAllByLabelText('用户名')).toHaveLength(3);
    expect(screen.getAllByLabelText('密码')).toHaveLength(3);
  });

  it('FR-100：has_password 为真时展示「密码已配置」徽标', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    桩动态配置();
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    expect(screen.getByText('密码已配置')).toBeInTheDocument();
  });

  it('FR-100：密码框留空保存时 PATCH 载荷省略各代理 password', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    桩动态配置();
    const update = vi.spyOn(api, 'updateSettings').mockResolvedValue(启用样例);
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    fireEvent.click(screen.getByRole('button', { name: '保存' }));

    await waitFor(() => expect(update).toHaveBeenCalledTimes(1));
    const np = update.mock.calls[0][0].network_proxy;
    expect(np.http.password).toBeUndefined();
    expect(np.https.password).toBeUndefined();
    expect(np.all.password).toBeUndefined();
    expect(np.http.username).toBe('alice');
  });

  it('FR-100：填入密码保存时对应代理 PATCH 载荷带 password', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    桩动态配置();
    const update = vi.spyOn(api, 'updateSettings').mockResolvedValue(启用样例);
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    const passInputs = screen.getAllByLabelText('密码');
    fireEvent.change(passInputs[0], { target: { value: 's3cret' } });
    fireEvent.click(screen.getByRole('button', { name: '保存' }));

    await waitFor(() => expect(update).toHaveBeenCalledTimes(1));
    const np = update.mock.calls[0][0].network_proxy;
    expect(np.http.password).toBe('s3cret');
    expect(np.https.password).toBeUndefined();
    expect(np.all.password).toBeUndefined();
  });

  it('FR-100：SOCKS5 的 URL 能填入并组装到 all 槽', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(未启用样例);
    桩动态配置();
    const update = vi.spyOn(api, 'updateSettings').mockResolvedValue(未启用样例);
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    const urlInputs = screen.getAllByLabelText('URL');
    fireEvent.change(urlInputs[2], { target: { value: 'socks5://socks.internal:1080' } });
    fireEvent.click(screen.getByRole('button', { name: '保存' }));

    await waitFor(() => expect(update).toHaveBeenCalledTimes(1));
    expect(update.mock.calls[0][0].network_proxy.all.url).toBe('socks5://socks.internal:1080');
  });

  it('FR-100：点「清除密码」后保存，对应代理 PATCH 载荷带 password 空串', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    桩动态配置();
    const update = vi.spyOn(api, 'updateSettings').mockResolvedValue(启用样例);
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    fireEvent.click(screen.getByText('清除密码'));
    fireEvent.click(screen.getByRole('button', { name: '保存' }));

    await waitFor(() => expect(update).toHaveBeenCalledTimes(1));
    expect(update.mock.calls[0][0].network_proxy.http.password).toBe('');
  });

  // ===== FR-104：回滚 =====

  it('FR-104：有回滚备份时回滚按钮可用，点回滚走二次确认并调 rollback，成功后进入正在重启态', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    桩动态配置();
    const rollback = vi
      .spyOn(api, 'rollbackUpdate')
      .mockResolvedValue({ status: '已回滚，正在重启' });
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
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
    桩动态配置();
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    const btn = screen.getByText('回滚到上一版').closest('button');
    expect(btn).toBeDisabled();
    expect(screen.getByText(/暂无可回滚的备份版本/)).toBeInTheDocument();
  });

  it('FR-104：回滚返回 409（无备份）时展示友好提示且不进入重启态', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    桩动态配置();
    vi.spyOn(api, 'rollbackUpdate').mockRejectedValue(
      new ApiError(409, 'conflict', '无可回滚的备份版本'),
    );
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
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
    桩动态配置();
    renderPage();
    await waitFor(() => expect(screen.getByText('无权执行该操作')).toBeInTheDocument());
  });

  // ===== FR-106：动态配置各节并入锚点节（表单回显 + 字段绑定不回归）=====

  it('FR-106：限制配额 / 可观测性 / 安全会话各节表单回显默认值', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    桩动态配置();
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    // 各节默认值：会话有效期 3600、审计保留 90、采样间隔 60（均默认可见，无须切 tab）
    await waitFor(() => expect(screen.getByDisplayValue('3600')).toBeVisible());
    expect(screen.getByDisplayValue('90')).toBeVisible();
    expect(screen.getByDisplayValue('60')).toBeVisible();
    // 重启生效标注（区别于代理 / 更新即时生效）：每个动态配置节各一枚徽标，取首枚断言可见
    const restartBadges = screen.getAllByText('保存后重启生效');
    expect(restartBadges.length).toBeGreaterThan(0);
    expect(restartBadges[0]).toBeVisible();
  });
});
