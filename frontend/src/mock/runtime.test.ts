// 运行时 Mock 模式开关逻辑测试（FR-119，ADR-0035）。
//
// 浏览器内真实 service worker 拦截无法在 jsdom 全测（真机待验）；
// 此处覆盖可下沉单测的开关判定：env / localStorage / URL 参数三来源与持久化。
// startMockRuntime 的 worker 启动经动态 import 浏览器 worker，jsdom 无 service worker，
// 故只断言「未启用时不启动」这一可测分支。

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { applyUrlOverride, isMockEnabled, setMockEnabled, startMockRuntime } from './runtime';

const FLAG_KEY = 'jianartifact.mock';

/** 原始 window.location 描述符，用于每个用例后还原（避免 reload 桩泄漏到后续用例）。 */
const originalLocationDescriptor = Object.getOwnPropertyDescriptor(window, 'location');

/** 把地址栏重置为指定 search（jsdom 支持 history.replaceState）。 */
function setSearch(search: string): void {
  window.history.replaceState(null, '', `/${search}`);
}

/** 用带 reload 桩的对象替换 window.location（保留 search 等真实字段）。 */
function stubLocationReload(): ReturnType<typeof vi.fn> {
  const reload = vi.fn();
  Object.defineProperty(window, 'location', {
    value: { ...window.location, reload },
    writable: true,
    configurable: true,
  });
  return reload;
}

describe('运行时 Mock 模式开关（FR-119）', () => {
  beforeEach(() => {
    localStorage.clear();
    vi.unstubAllEnvs();
    setSearch('');
  });
  afterEach(() => {
    vi.restoreAllMocks();
    vi.unstubAllEnvs();
    // 还原真实 window.location，防止某个用例的 reload 桩污染后续用例的 search 读取
    if (originalLocationDescriptor) {
      Object.defineProperty(window, 'location', originalLocationDescriptor);
    }
  });

  it('默认（无 env / 无 localStorage / 无 URL）关闭', () => {
    expect(isMockEnabled()).toBe(false);
  });

  it('构建期 env VITE_MOCK=true 时启用', () => {
    vi.stubEnv('VITE_MOCK', 'true');
    expect(isMockEnabled()).toBe(true);
  });

  it('localStorage 开关为 on 时启用', () => {
    localStorage.setItem(FLAG_KEY, 'on');
    expect(isMockEnabled()).toBe(true);
  });

  it('setMockEnabled(true) 落盘开关并触发刷新', () => {
    const reload = stubLocationReload();
    setMockEnabled(true);
    expect(localStorage.getItem(FLAG_KEY)).toBe('on');
    expect(reload).toHaveBeenCalled();
  });

  it('setMockEnabled(false) 清除开关并触发刷新', () => {
    localStorage.setItem(FLAG_KEY, 'on');
    const reload = stubLocationReload();
    setMockEnabled(false);
    expect(localStorage.getItem(FLAG_KEY)).toBeNull();
    expect(reload).toHaveBeenCalled();
  });

  it('URL ?mock=1 启用并落盘、移除该参数后地址栏干净', () => {
    setSearch('?mock=1&q=keep');
    applyUrlOverride();
    expect(localStorage.getItem(FLAG_KEY)).toBe('on');
    expect(window.location.search).not.toContain('mock=1');
    // 其它参数保留
    expect(window.location.search).toContain('q=keep');
  });

  it('URL ?mock=0 关闭并清盘', () => {
    localStorage.setItem(FLAG_KEY, 'on');
    setSearch('?mock=0');
    applyUrlOverride();
    expect(localStorage.getItem(FLAG_KEY)).toBeNull();
    expect(isMockEnabled()).toBe(false);
  });

  it('未启用时 startMockRuntime 立即返回、不启动 worker', async () => {
    // 无 env / 无 flag / 无 URL → 直接返回，不抛错（jsdom 无 service worker 也安全）
    await expect(startMockRuntime()).resolves.toBeUndefined();
    expect(isMockEnabled()).toBe(false);
  });
});
