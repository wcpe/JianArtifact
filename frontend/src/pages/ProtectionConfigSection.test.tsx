// 防护配置节组件测试（FR-110 原 FR-80 防护配置页迁入；FR-129 改为受控展示组件）：
// 本组件不再自带 GET/PATCH 与独立保存按钮——state、加载、保存全部由 SettingsPage 托管。
// 这里只验证受控渲染：按 props 展示各维度与当前值、loading 显加载态、无 config 显错误、编辑经 onPatch 回传。

import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { MantineProvider } from '@mantine/core';
import { ProtectionConfigSection } from './ProtectionConfigSection';
import type { ProtectionConfig } from '../api/types';

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

/** 在 Mantine Provider 下渲染受控防护配置节（默认提供完整 props）。 */
function renderSection(props: Partial<React.ComponentProps<typeof ProtectionConfigSection>> = {}) {
  const merged = {
    config: 样例配置,
    allowText: '',
    denyText: '',
    loading: false,
    error: null,
    onAllowTextChange: vi.fn(),
    onDenyTextChange: vi.fn(),
    onPatch: vi.fn(),
    ...props,
  };
  render(
    <MantineProvider>
      <ProtectionConfigSection {...merged} />
    </MantineProvider>,
  );
  return merged;
}

describe('ProtectionConfigSection（受控展示）', () => {
  afterEach(() => vi.restoreAllMocks());

  it('按 props 展示各防护维度分区与当前值', () => {
    renderSection();

    expect(screen.getByText('速率限制')).toBeInTheDocument();
    expect(screen.getByText('IP 黑 / 白名单')).toBeInTheDocument();
    expect(screen.getByText('CC 挑战（PoW）')).toBeInTheDocument();
    expect(screen.getByText('WAF 规则引擎')).toBeInTheDocument();
    // 当前值回显：CC 难度默认 20
    expect(screen.getByDisplayValue('20')).toBeInTheDocument();
  });

  it('不再有独立保存按钮（并入设置页单一保存，FR-129）', () => {
    renderSection();
    expect(screen.queryByRole('button', { name: '保存并即时生效' })).not.toBeInTheDocument();
  });

  it('loading 时显示加载态、不渲染表单', () => {
    renderSection({ config: null, loading: true });
    expect(screen.queryByText('速率限制')).not.toBeInTheDocument();
  });

  it('无 config 且有 error 时展示错误提示', () => {
    renderSection({ config: null, loading: false, error: '无权执行该操作' });
    expect(screen.getByText('无权执行该操作')).toBeInTheDocument();
  });

  it('编辑某维度经 onPatch 回传新值', () => {
    const { onPatch } = renderSection();
    // 切换「启用速率限制」开关 → onPatch('rate_limit', { ...enabled: true })
    fireEvent.click(screen.getByLabelText('启用速率限制'));
    expect(onPatch).toHaveBeenCalledWith('rate_limit', expect.objectContaining({ enabled: true }));
  });
});
