// 防护配置节组件测试（FR-110，原 FR-80 防护配置页迁入）：加载后展示各维度表单、
// 保存调 PATCH、失败展示错误文案；节自带 GET/PATCH /protection/config 与独立保存按钮。

import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/react';
import { MantineProvider } from '@mantine/core';
import { ProtectionConfigSection } from './ProtectionConfigSection';
import * as api from '../api/endpoints';
import { ApiError } from '../api/client';
import type { ProtectionConfig } from '../api/types';

/** 在 Mantine Provider 下渲染防护配置节。 */
function renderSection() {
  return render(
    <MantineProvider>
      <ProtectionConfigSection />
    </MantineProvider>,
  );
}

/** 一份默认（各防护关闭）的防护配置样例，对齐后端默认值。 */
const 样例配置: ProtectionConfig = {
  rate_limit: {
    enabled: false,
    window_secs: 60,
    ip_max_requests: 1200,
    identity_max_requests: 2400,
    repo_max_requests: 0,
    ip_max_concurrent: 0,
    user_max_concurrent: 0,
    repo_max_concurrent: 0,
  },
  ip_list: { allow: [], deny: [] },
  ban: { enabled: false, window_secs: 60, threshold: 100, duration_secs: 900 },
  slowloris: {
    enabled: false,
    body_read_timeout_secs: 30,
    header_timeout_secs: 30,
    max_body_bytes: 0,
  },
  cc_challenge: { enabled: false, difficulty: 20, ttl_secs: 300, exempt_authenticated: true },
  waf: { enabled: false, rules: [] },
  alerts: {
    enabled: false,
    window_secs: 300,
    rate_limit_warn_threshold: 1000,
    ban_warn_threshold: 50,
    cc_challenge_fail_warn_threshold: 1000,
    waf_block_warn_threshold: 500,
    slowloris_warn_threshold: 200,
    max_rows: 100000,
  },
};

describe('ProtectionConfigSection', () => {
  afterEach(() => vi.restoreAllMocks());

  it('加载后展示各防护维度分区与当前值', async () => {
    vi.spyOn(api, 'getProtectionConfig').mockResolvedValue(样例配置);
    renderSection();

    // 各维度分区标题
    await waitFor(() => expect(screen.getByText('速率限制')).toBeInTheDocument());
    expect(screen.getByText('IP 黑 / 白名单')).toBeInTheDocument();
    expect(screen.getByText('CC 挑战（PoW）')).toBeInTheDocument();
    expect(screen.getByText('WAF 规则引擎')).toBeInTheDocument();
    // 当前值回显：CC 难度默认 20
    expect(screen.getByDisplayValue('20')).toBeInTheDocument();
  });

  it('保存时把当前配置整体 PATCH 回后端并提示已生效', async () => {
    vi.spyOn(api, 'getProtectionConfig').mockResolvedValue(样例配置);
    const update = vi
      .spyOn(api, 'updateProtectionConfig')
      .mockImplementation((cfg) => Promise.resolve(cfg));
    renderSection();

    await waitFor(() => expect(screen.getByText('速率限制')).toBeInTheDocument());
    fireEvent.click(screen.getByText('保存并即时生效'));

    await waitFor(() => expect(update).toHaveBeenCalledTimes(1));
    // 提交载荷应是完整七维配置
    const payload = update.mock.calls[0][0];
    expect(payload.rate_limit.window_secs).toBe(60);
    expect(payload.cc_challenge.difficulty).toBe(20);
    // 成功后提示已生效
    await waitFor(() => expect(screen.getByText('已保存，配置已即时生效。')).toBeInTheDocument());
  });

  it('保存失败（如后端 400 校验）时展示错误文案', async () => {
    vi.spyOn(api, 'getProtectionConfig').mockResolvedValue(样例配置);
    vi.spyOn(api, 'updateProtectionConfig').mockRejectedValue(
      new ApiError(
        400,
        'bad_request',
        '防护配置非法：限流时间窗（rate_limit.window_secs）必须大于 0',
      ),
    );
    renderSection();

    await waitFor(() => expect(screen.getByText('速率限制')).toBeInTheDocument());
    fireEvent.click(screen.getByText('保存并即时生效'));

    await waitFor(() =>
      expect(
        screen.getByText('防护配置非法：限流时间窗（rate_limit.window_secs）必须大于 0'),
      ).toBeInTheDocument(),
    );
  });

  it('加载失败时展示错误提示', async () => {
    vi.spyOn(api, 'getProtectionConfig').mockRejectedValue(
      new ApiError(403, 'forbidden', '无权执行该操作'),
    );
    renderSection();

    await waitFor(() => expect(screen.getByText('无权执行该操作')).toBeInTheDocument());
  });
});
