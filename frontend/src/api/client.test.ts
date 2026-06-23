// API 客户端单元测试：令牌存取、Bearer 头附加、错误结构解析、401 回调。

import { describe, it, expect, beforeEach, vi, afterEach } from 'vitest';
import {
  request,
  ApiError,
  getToken,
  setToken,
  clearToken,
  setUnauthorizedHandler,
} from './client';

/** 构造一个 fetch 响应桩。 */
function mockResponse(status: number, body: unknown, ok = status >= 200 && status < 300): Response {
  const text = body === undefined ? '' : JSON.stringify(body);
  return {
    ok,
    status,
    text: () => Promise.resolve(text),
    json: () => Promise.resolve(body),
  } as Response;
}

describe('令牌存取', () => {
  beforeEach(() => localStorage.clear());

  it('setToken 后 getToken 返回同值', () => {
    expect(getToken()).toBeNull();
    setToken('abc');
    expect(getToken()).toBe('abc');
  });

  it('clearToken 清除令牌', () => {
    setToken('abc');
    clearToken();
    expect(getToken()).toBeNull();
  });
});

describe('request 行为', () => {
  beforeEach(() => {
    localStorage.clear();
    setUnauthorizedHandler(() => {});
  });
  afterEach(() => vi.restoreAllMocks());

  it('已登录时附加 Bearer 头并走相对 /api/v1 路径', async () => {
    setToken('tok-123');
    const fetchSpy = vi
      .spyOn(globalThis, 'fetch')
      .mockResolvedValue(mockResponse(200, { ok: true }));

    await request('/me');

    expect(fetchSpy).toHaveBeenCalledTimes(1);
    const [url, init] = fetchSpy.mock.calls[0];
    expect(url).toBe('/api/v1/me');
    expect((init?.headers as Record<string, string>)['Authorization']).toBe('Bearer tok-123');
  });

  it('POST 体序列化为 JSON 并设置 Content-Type', async () => {
    const fetchSpy = vi
      .spyOn(globalThis, 'fetch')
      .mockResolvedValue(mockResponse(200, { ok: true }));

    await request('/auth/login', { method: 'POST', body: { username: 'a', password: 'b' } });

    const [, init] = fetchSpy.mock.calls[0];
    expect(init?.method).toBe('POST');
    expect((init?.headers as Record<string, string>)['Content-Type']).toBe('application/json');
    expect(init?.body).toBe(JSON.stringify({ username: 'a', password: 'b' }));
  });

  it('查询参数拼接并跳过 undefined', async () => {
    const fetchSpy = vi
      .spyOn(globalThis, 'fetch')
      .mockResolvedValue(mockResponse(200, { items: [] }));

    await request('/search', { query: { q: 'foo', format: undefined, limit: 20 } });

    const [url] = fetchSpy.mock.calls[0];
    expect(url).toBe('/api/v1/search?q=foo&limit=20');
  });

  it('解析后端错误结构并抛出 ApiError', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      mockResponse(409, { error: { code: 'conflict', message: '仓库名已存在' } }),
    );

    await expect(request('/repositories', { method: 'POST', body: {} })).rejects.toMatchObject({
      status: 409,
      code: 'conflict',
      message: '仓库名已存在',
    });
  });

  it('401 触发未认证回调', async () => {
    const handler = vi.fn();
    setUnauthorizedHandler(handler);
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      mockResponse(401, { error: { code: 'unauthorized', message: '未认证' } }),
    );

    await expect(request('/me')).rejects.toBeInstanceOf(ApiError);
    expect(handler).toHaveBeenCalledTimes(1);
  });

  it('204 返回 undefined', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(mockResponse(204, undefined));
    const result = await request('/auth/logout', { method: 'POST' });
    expect(result).toBeUndefined();
  });
});
