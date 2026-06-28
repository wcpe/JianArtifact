// 设置页组件测试（FR-87 只读 + FR-88 可编辑热替换 + FR-103 锚点单页重做 + FR-106 动态配置）：
// 加载填充脱敏配置到表单、单个全局保存一次性提交 settings + dynamic 两次 PATCH、
// 网络代理三字段 / 密码三态契约（FR-100）、动态配置各节回显与字段绑定、各错误码（400）友好提示；
// 并校验左侧 sticky 锚点子导航（五项，点击平滑滚动）+ 单页分节（各节标题可见、非 tab 隐藏）+
// **只有一个保存按钮**。
// 注：在线更新已迁至「系统」页（FR-109，SystemPage），本页不再含相关 UI 与测试。

import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, waitFor, fireEvent, within } from '@testing-library/react';
import { MantineProvider } from '@mantine/core';
import { SettingsPage } from './SettingsPage';
import * as api from '../api/endpoints';
import { ApiError } from '../api/client';
import type { SettingsView, DynamicConfig } from '../api/types';

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

  it('加载后将脱敏后的网络代理配置填入可编辑表单', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    桩动态配置();
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    // 代理 URL 脱敏后填入输入框（不含凭据）
    expect(screen.getByDisplayValue('http://proxy.internal:8080')).toBeInTheDocument();
    expect(screen.getByDisplayValue('https://proxy.internal:8443')).toBeInTheDocument();
    expect(screen.getByDisplayValue('localhost,127.0.0.1')).toBeInTheDocument();
  });

  // ===== FR-103：锚点单页骨架 =====

  it('FR-103：左侧锚点导航有五项（网络代理 / 限制与配额 / 可观测性 / 漏洞库 / 安全·会话）', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    桩动态配置();
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    // 导航在 <nav> 内（与正文同名标题区分）
    const nav = screen.getByRole('navigation', { name: '设置分节导航' });
    expect(within(nav).getByText('网络代理')).toBeInTheDocument();
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
    // 整页文案恰为「保存」的按钮只此一个
    const allButtons = screen.getAllByRole('button');
    const saveButtons = allButtons.filter((b) => b.textContent?.trim() === '保存');
    expect(saveButtons).toHaveLength(1);
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

  // ===== FR-100：网络代理三字段 / 三态（合并到全局保存，契约不回归）=====

  it('编辑代理 URL 后点保存，PATCH 载荷带新 URL、省略 password', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(未启用样例);
    桩动态配置();
    const update = vi.spyOn(api, 'updateSettings').mockResolvedValue(未启用样例);
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    const urlInputs = screen.getAllByLabelText('URL');
    fireEvent.change(urlInputs[0], { target: { value: 'http://new-proxy.internal:3128' } });
    fireEvent.click(screen.getByRole('button', { name: '保存' }));

    await waitFor(() => expect(update).toHaveBeenCalledTimes(1));
    const payload = update.mock.calls[0][0];
    expect(payload.network_proxy).toBeDefined();
    expect(payload.network_proxy!.http.url).toBe('http://new-proxy.internal:3128');
    expect(payload.network_proxy!.http.password).toBeUndefined();
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
    const np = update.mock.calls[0][0].network_proxy!;
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
    const np = update.mock.calls[0][0].network_proxy!;
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
    const payload = update.mock.calls[0][0];
    expect(payload.network_proxy).toBeDefined();
    expect(payload.network_proxy!.all.url).toBe('socks5://socks.internal:1080');
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
    const payload = update.mock.calls[0][0];
    expect(payload.network_proxy).toBeDefined();
    expect(payload.network_proxy!.http.password).toBe('');
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
