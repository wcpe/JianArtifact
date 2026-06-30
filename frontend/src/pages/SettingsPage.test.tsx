// 设置页组件测试（FR-87 只读 + FR-88 可编辑热替换 + FR-106 动态配置 + FR-129 顶部 Tab 分页）：
// 加载填充脱敏配置到表单、单个全局保存一次性提交 settings + dynamic + protection 三次 PATCH、
// 网络代理三字段 / 密码三态契约（FR-100）、动态配置各节回显与字段绑定、各错误码（400）友好提示；
// 并校验顶部 Tab 分页（六项，点 Tab 切换显示对应节内容、非默认节切换前不可见）+ **只有一个保存按钮**。
// FR-129：原锚点长滚动 + scroll-spy 改为 Tab 分页（切换不滚动、根因消除高亮 / hover 错位）；
// 防护节并入单一保存（移除其独立「保存并即时生效」按钮）。
// 注：在线更新已迁至「系统」页（FR-109，SystemPage），本页不再含相关 UI 与测试。

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, fireEvent, within } from '@testing-library/react';
import { MantineProvider } from '@mantine/core';
import { SettingsPage } from './SettingsPage';
import * as api from '../api/endpoints';
import { ApiError } from '../api/client';
import type { SettingsView, DynamicConfig, ProtectionConfig } from '../api/types';

/** 在 Mantine Provider 下渲染设置页。 */
function renderPage() {
  return render(
    <MantineProvider>
      <SettingsPage />
    </MantineProvider>,
  );
}

/** 点击顶部 Tab 切到指定分节（Tab 标题与各节标题同文案）。 */
function 切到Tab(name: string) {
  const tablist = screen.getByRole('tablist');
  fireEvent.click(within(tablist).getByRole('tab', { name }));
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

/** 一份默认（各防护关闭）的防护配置样例（FR-110 防护节嵌入设置页后，页面会加载它）。
 *  各数值刻意避开动态配置样例的 60 / 90 / 3600 等，免得 getByDisplayValue 跨节误命中多个元素。 */
const 防护配置样例: ProtectionConfig = {
  rate_limit: {
    enabled: false,
    window_secs: 61,
    ip_max_requests: 1200,
    identity_max_requests: 2400,
    repo_max_requests: 0,
    ip_max_concurrent: 0,
    user_max_concurrent: 0,
    repo_max_concurrent: 0,
  },
  ip_list: { allow: [], deny: [] },
  ban: { enabled: false, window_secs: 62, threshold: 100, duration_secs: 901 },
  slowloris: {
    enabled: false,
    body_read_timeout_secs: 31,
    header_timeout_secs: 32,
    max_body_bytes: 0,
  },
  cc_challenge: { enabled: false, difficulty: 21, ttl_secs: 301, exempt_authenticated: true },
  waf: { enabled: false, rules: [] },
  alerts: {
    enabled: false,
    window_secs: 301,
    rate_limit_warn_threshold: 1001,
    ban_warn_threshold: 51,
    cc_challenge_fail_warn_threshold: 1002,
    waf_block_warn_threshold: 501,
    slowloris_warn_threshold: 201,
    max_rows: 100001,
  },
};

/** 默认对 getDynamicConfig 打桩（多数用例不关心动态配置，但页面会加载它）。 */
function 桩动态配置(config: DynamicConfig = 动态配置样例) {
  return vi.spyOn(api, 'getDynamicConfig').mockResolvedValue(config);
}

/** 默认对 getProtectionConfig 打桩（FR-110 防护节会在挂载时加载防护配置）。 */
function 桩防护配置(config: ProtectionConfig = 防护配置样例) {
  return vi.spyOn(api, 'getProtectionConfig').mockResolvedValue(config);
}

describe('SettingsPage', () => {
  // FR-110：防护节挂载即拉防护配置；各用例默认打桩，避免真实 fetch。需要定制的用例可再行覆盖。
  beforeEach(() => {
    桩防护配置();
  });
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

  // ===== FR-129：顶部 Tab 分页骨架 =====

  it('FR-129：顶部 Tab 列表有六项（网络代理 / 限制与配额 / 可观测性 / 漏洞库 / 安全·会话 / 防护配置）', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    桩动态配置();
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    const tablist = screen.getByRole('tablist');
    expect(within(tablist).getByRole('tab', { name: '网络代理' })).toBeInTheDocument();
    expect(within(tablist).getByRole('tab', { name: '限制与配额' })).toBeInTheDocument();
    expect(within(tablist).getByRole('tab', { name: '可观测性' })).toBeInTheDocument();
    expect(within(tablist).getByRole('tab', { name: '漏洞库' })).toBeInTheDocument();
    expect(within(tablist).getByRole('tab', { name: '安全 / 会话' })).toBeInTheDocument();
    expect(within(tablist).getByRole('tab', { name: '防护配置' })).toBeInTheDocument();
  });

  it('FR-129：默认显示网络代理节，其余节内容切 Tab 前不可见', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    桩动态配置();
    renderPage();

    // 默认 Tab 为网络代理：HTTP 代理可见
    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeVisible());
    // 安全 / 会话节字段（会话有效期）虽随面板挂载在 DOM，但未激活面板不可见
    expect(screen.getByLabelText('会话有效期（秒）')).not.toBeVisible();
  });

  it('FR-129：点击 Tab 切换显示对应节内容', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    桩动态配置();
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeVisible());
    // 切到「安全 / 会话」Tab → 会话有效期字段可见
    切到Tab('安全 / 会话');
    await waitFor(() => expect(screen.getByLabelText('会话有效期（秒）')).toBeVisible());
    // 切走后网络代理节内容重新可见、安全会话节内容隐藏
    切到Tab('网络代理');
    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeVisible());
    expect(screen.getByLabelText('会话有效期（秒）')).not.toBeVisible();
  });

  it('FR-129：只有一个保存按钮——保存条内有「保存」、且无防护节「保存并即时生效」按钮', async () => {
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
    // 防护节独立保存按钮已并入全局保存：切到防护 Tab 也不应再出现
    切到Tab('防护配置');
    await waitFor(() => expect(screen.getByText('速率限制')).toBeVisible());
    expect(screen.queryByRole('button', { name: '保存并即时生效' })).not.toBeInTheDocument();
    // 整页文案恰为「保存」的按钮只此一个
    const allButtons = screen.getAllByRole('button');
    const saveButtons = allButtons.filter((b) => b.textContent?.trim() === '保存');
    expect(saveButtons).toHaveLength(1);
  });

  // ===== FR-103/129：单个全局保存（合并 settings + dynamic + protection 三次 PATCH）=====

  it('FR-129：点保存一次性提交 updateSettings + updateDynamicConfig + updateProtectionConfig，成功显已保存', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(未启用样例);
    桩动态配置();
    const update = vi.spyOn(api, 'updateSettings').mockResolvedValue(未启用样例);
    const updateDyn = vi
      .spyOn(api, 'updateDynamicConfig')
      .mockImplementation((c) => Promise.resolve(c));
    const updateProtection = vi
      .spyOn(api, 'updateProtectionConfig')
      .mockImplementation((c) => Promise.resolve(c));
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    fireEvent.click(screen.getByRole('button', { name: '保存' }));

    await waitFor(() => expect(update).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(updateDyn).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(updateProtection).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(screen.getByText(/已保存/)).toBeInTheDocument());
  });

  it('FR-129：保存时改过的动态配置（会话有效期）随 updateDynamicConfig 一并提交', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    桩动态配置();
    vi.spyOn(api, 'updateSettings').mockResolvedValue(启用样例);
    vi.spyOn(api, 'updateProtectionConfig').mockImplementation((c) => Promise.resolve(c));
    const updateDyn = vi
      .spyOn(api, 'updateDynamicConfig')
      .mockImplementation((c) => Promise.resolve(c));
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    切到Tab('安全 / 会话');
    await waitFor(() => expect(screen.getByLabelText('会话有效期（秒）')).toBeVisible());
    fireEvent.change(screen.getByLabelText('会话有效期（秒）'), { target: { value: '7200' } });
    fireEvent.click(screen.getByRole('button', { name: '保存' }));

    await waitFor(() => expect(updateDyn).toHaveBeenCalledTimes(1));
    expect(updateDyn.mock.calls[0][0].auth.session_ttl_secs).toBe(7200);
  });

  it('FR-129：保存时改过的防护配置随 updateProtectionConfig 一并提交（防护并入单一保存）', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    桩动态配置();
    vi.spyOn(api, 'updateSettings').mockResolvedValue(启用样例);
    vi.spyOn(api, 'updateDynamicConfig').mockImplementation((c) => Promise.resolve(c));
    const updateProtection = vi
      .spyOn(api, 'updateProtectionConfig')
      .mockImplementation((c) => Promise.resolve(c));
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    // 切到防护 Tab，开启「速率限制」开关
    切到Tab('防护配置');
    await waitFor(() => expect(screen.getByText('速率限制')).toBeVisible());
    fireEvent.click(screen.getByLabelText('启用速率限制'));
    fireEvent.click(screen.getByRole('button', { name: '保存' }));

    await waitFor(() => expect(updateProtection).toHaveBeenCalledTimes(1));
    expect(updateProtection.mock.calls[0][0].rate_limit.enabled).toBe(true);
  });

  it('FR-129：动态配置未加载（getDynamicConfig 失败）时保存只发 settings + protection，不报错', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    vi.spyOn(api, 'getDynamicConfig').mockRejectedValue(
      new ApiError(403, 'forbidden', '无权执行该操作'),
    );
    const update = vi.spyOn(api, 'updateSettings').mockResolvedValue(启用样例);
    const updateDyn = vi.spyOn(api, 'updateDynamicConfig');
    const updateProtection = vi
      .spyOn(api, 'updateProtectionConfig')
      .mockImplementation((c) => Promise.resolve(c));
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    fireEvent.click(screen.getByRole('button', { name: '保存' }));

    await waitFor(() => expect(update).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(updateProtection).toHaveBeenCalledTimes(1));
    expect(updateDyn).not.toHaveBeenCalled();
  });

  it('FR-129：防护配置未加载（getProtectionConfig 失败）时保存只发 settings + dynamic，不报错', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    桩动态配置();
    vi.spyOn(api, 'getProtectionConfig').mockRejectedValue(
      new ApiError(403, 'forbidden', '无权执行该操作'),
    );
    const update = vi.spyOn(api, 'updateSettings').mockResolvedValue(启用样例);
    const updateDyn = vi
      .spyOn(api, 'updateDynamicConfig')
      .mockImplementation((c) => Promise.resolve(c));
    const updateProtection = vi.spyOn(api, 'updateProtectionConfig');
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    fireEvent.click(screen.getByRole('button', { name: '保存' }));

    await waitFor(() => expect(update).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(updateDyn).toHaveBeenCalledTimes(1));
    expect(updateProtection).not.toHaveBeenCalled();
  });

  it('FR-129：保存（settings PATCH）返回 400 时展示友好提示', async () => {
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

  it('FR-129：动态配置保存返回 400（非法值）时展示友好提示', async () => {
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

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    fireEvent.click(screen.getByRole('button', { name: '保存' }));

    await waitFor(() =>
      expect(
        screen.getByText('动态配置非法：会话有效期（auth.session_ttl_secs）必须大于 0'),
      ).toBeInTheDocument(),
    );
  });

  it('FR-129：防护配置保存返回 400（非法值）时展示友好提示', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    桩动态配置();
    vi.spyOn(api, 'updateSettings').mockResolvedValue(启用样例);
    vi.spyOn(api, 'updateDynamicConfig').mockImplementation((c) => Promise.resolve(c));
    vi.spyOn(api, 'updateProtectionConfig').mockRejectedValue(
      new ApiError(400, 'bad_request', '防护配置非法：限流窗口必须大于 0'),
    );
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    fireEvent.click(screen.getByRole('button', { name: '保存' }));

    await waitFor(() =>
      expect(screen.getByText('防护配置非法：限流窗口必须大于 0')).toBeInTheDocument(),
    );
  });

  // ===== FR-100：网络代理三字段 / 三态（合并到全局保存，契约不回归）=====

  it('编辑代理 URL 后点保存，PATCH 载荷带新 URL、省略 password', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(未启用样例);
    桩动态配置();
    vi.spyOn(api, 'updateProtectionConfig').mockImplementation((c) => Promise.resolve(c));
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
    vi.spyOn(api, 'updateProtectionConfig').mockImplementation((c) => Promise.resolve(c));
    vi.spyOn(api, 'updateDynamicConfig').mockImplementation((c) => Promise.resolve(c));
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
    vi.spyOn(api, 'updateProtectionConfig').mockImplementation((c) => Promise.resolve(c));
    vi.spyOn(api, 'updateDynamicConfig').mockImplementation((c) => Promise.resolve(c));
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
    vi.spyOn(api, 'updateProtectionConfig').mockImplementation((c) => Promise.resolve(c));
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
    vi.spyOn(api, 'updateProtectionConfig').mockImplementation((c) => Promise.resolve(c));
    vi.spyOn(api, 'updateDynamicConfig').mockImplementation((c) => Promise.resolve(c));
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

  // ===== FR-106：动态配置各节并入 Tab 分页（表单回显 + 字段绑定不回归）=====

  it('FR-106：切到各 Tab 后限制配额 / 可观测性 / 安全会话节表单回显默认值', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    桩动态配置();
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeVisible());
    // 可观测性 Tab：审计保留 90、采样间隔 60
    切到Tab('可观测性');
    await waitFor(() => expect(screen.getByDisplayValue('90')).toBeVisible());
    expect(screen.getByDisplayValue('60')).toBeVisible();
    // 安全 / 会话 Tab：会话有效期 3600 + 重启生效徽标（区别于代理即时生效）
    切到Tab('安全 / 会话');
    await waitFor(() => expect(screen.getByDisplayValue('3600')).toBeVisible());
    // 安全会话节内的重启徽标随该 Tab 激活而可见（面板保持挂载，故各节均有徽标，断言激活节的可见）
    const authPanel = screen.getByLabelText('会话有效期（秒）').closest('[role="tabpanel"]');
    expect(authPanel).not.toBeNull();
    expect(within(authPanel as HTMLElement).getByText('保存后重启生效')).toBeVisible();
  });

  // ===== FR-110/129：防护配置并入设置页（Tab 分节 + 并入单一保存）=====

  it('FR-129：顶部 Tab 含「防护配置」一项', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    桩动态配置();
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    const tablist = screen.getByRole('tablist');
    expect(within(tablist).getByRole('tab', { name: '防护配置' })).toBeInTheDocument();
  });

  it('FR-129：切到防护 Tab——防护各维度标题可见、无独立保存按钮', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    桩动态配置();
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    切到Tab('防护配置');
    // 防护各维度分区可见
    await waitFor(() => expect(screen.getByText('速率限制')).toBeVisible());
    expect(screen.getByText('WAF 规则引擎')).toBeVisible();
    // 防护节不再有自己的保存按钮（并入单一保存）
    expect(screen.queryByRole('button', { name: '保存并即时生效' })).not.toBeInTheDocument();
  });

  // ===== FR-128：代理连通性测试（每代理各一个按钮 → 共用模态框，回流 UX）=====

  it('FR-128：代理 Tab 三代理各有独立「测试」按钮（http / https / all）', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    桩动态配置();
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    // 三代理各一个测试按钮
    expect(screen.getByTestId('proxy-test-button-http')).toBeInTheDocument();
    expect(screen.getByTestId('proxy-test-button-http')).toHaveTextContent('测试');
    expect(screen.getByTestId('proxy-test-button-https')).toBeInTheDocument();
    expect(screen.getByTestId('proxy-test-button-all')).toBeInTheDocument();
  });

  it('FR-128：点 HTTP 代理「测试」按钮弹出模态框，预填代理 URL', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    桩动态配置();
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    // 点 HTTP 代理测试按钮
    fireEvent.click(screen.getByTestId('proxy-test-button-http'));

    // 模态框打开：URL 输入框与内部测试按钮可见
    await waitFor(() => expect(screen.getByTestId('proxy-test-url-input')).toBeInTheDocument());
    // 预填了当前 HTTP 代理 URL（启用样例 http.url = 'http://proxy.internal:8080'）
    expect((screen.getByTestId('proxy-test-url-input') as HTMLInputElement).value).toBe(
      'http://proxy.internal:8080',
    );
    expect(screen.getByTestId('proxy-test-button')).toBeInTheDocument();
  });

  it('FR-128：模态框内点「测试」调 testProxy 并展示连通成功结果', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    桩动态配置();
    const testProxy = vi.spyOn(api, 'testProxy').mockResolvedValue({
      ok: true,
      status: 200,
      elapsed_ms: 123,
    });
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    // 打开 HTTP 代理的测试模态框
    fireEvent.click(screen.getByTestId('proxy-test-button-http'));
    await waitFor(() => expect(screen.getByTestId('proxy-test-url-input')).toBeInTheDocument());

    // 修改 URL 后点测试
    fireEvent.change(screen.getByTestId('proxy-test-url-input'), {
      target: { value: 'https://example.com' },
    });
    fireEvent.click(screen.getByTestId('proxy-test-button'));

    await waitFor(() => expect(testProxy).toHaveBeenCalledWith('https://example.com'));
    // 展示成功结果（绿色，含状态码与耗时）
    await waitFor(() => expect(screen.getByTestId('proxy-test-result')).toBeInTheDocument());
    expect(screen.getByTestId('proxy-test-result').textContent).toContain('200');
    expect(screen.getByTestId('proxy-test-result').textContent).toContain('123');
  });

  it('FR-128：模态框内点「测试」调 testProxy 并展示连通失败结果', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(启用样例);
    桩动态配置();
    vi.spyOn(api, 'testProxy').mockResolvedValue({
      ok: false,
      elapsed_ms: 500,
      error: '连接失败',
    });
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    fireEvent.click(screen.getByTestId('proxy-test-button-https'));
    await waitFor(() => expect(screen.getByTestId('proxy-test-url-input')).toBeInTheDocument());

    fireEvent.change(screen.getByTestId('proxy-test-url-input'), {
      target: { value: 'http://127.0.0.1:1' },
    });
    fireEvent.click(screen.getByTestId('proxy-test-button'));

    await waitFor(() => expect(screen.getByTestId('proxy-test-result')).toBeInTheDocument());
    expect(screen.getByTestId('proxy-test-result').textContent).toContain('连接失败');
  });

  it('FR-128：模态框内 URL 为空时不调 testProxy，展示提示', async () => {
    vi.spyOn(api, 'getSettings').mockResolvedValue(未启用样例);
    桩动态配置();
    const testProxy = vi.spyOn(api, 'testProxy');
    renderPage();

    await waitFor(() => expect(screen.getByText('HTTP 代理')).toBeInTheDocument());
    // 未启用样例代理 URL 为空，打开模态框后 URL 输入框预填空
    fireEvent.click(screen.getByTestId('proxy-test-button-http'));
    await waitFor(() => expect(screen.getByTestId('proxy-test-url-input')).toBeInTheDocument());

    // 清空 URL 后直接点测试
    fireEvent.change(screen.getByTestId('proxy-test-url-input'), { target: { value: '' } });
    fireEvent.click(screen.getByTestId('proxy-test-button'));

    // 不应调用 testProxy
    expect(testProxy).not.toHaveBeenCalled();
    // 应展示错误提示
    await waitFor(() => expect(screen.getByTestId('proxy-test-error')).toBeInTheDocument());
  });
});
