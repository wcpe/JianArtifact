import { describe, expect, it, vi } from "vitest";

const { releasePending, start } = vi.hoisted(() => ({
  releasePending: vi.fn(() => 1),
  start: vi.fn(),
}));

vi.mock("@jianartifact/devmock/browser", () => ({ worker: { start } }));
vi.mock("@jianartifact/devmock/store", () => ({
  store: {},
  emptyStore: vi.fn(),
  resetStore: vi.fn(),
}));
vi.mock("@jianartifact/devmock/scenario", () => ({
  releaseDevMockPendingRequests: releasePending,
}));

import {
  enableMocking,
  handleUnhandledMockRequest,
  isMockManagedRequest,
} from "../src/mocks/enableMocking";

describe("开发态 Mock 未处理请求守卫", () => {
  it("仅将同源 API 与仓库协议请求报告为可诊断错误", () => {
    expect(isMockManagedRequest("http://localhost/api/v1/settings", "http://localhost")).toBe(true);
    expect(
      isMockManagedRequest("http://localhost/repository/raw-hosted/demo.txt", "http://localhost"),
    ).toBe(true);
    expect(isMockManagedRequest("http://localhost/assets/app.js", "http://localhost")).toBe(false);
    expect(
      isMockManagedRequest("https://upstream.example/api/v1/settings", "http://localhost"),
    ).toBe(false);
  });

  it("将受管的未处理请求输出 MSW 错误，静态资源继续旁路", () => {
    const error = vi.fn();
    const print = { error };

    handleUnhandledMockRequest(
      new Request("http://localhost/api/v1/search"),
      print,
      "http://localhost",
    );
    handleUnhandledMockRequest(
      new Request("http://localhost/assets/app.js"),
      print,
      "http://localhost",
    );

    expect(error).toHaveBeenCalledTimes(1);
  });

  it("仅开发态暴露显式释放 loading 请求的窄入口", async () => {
    await enableMocking();
    const mockWindow = window as unknown as {
      __devmock?: { releasePending?: () => number };
    };

    expect(mockWindow.__devmock?.releasePending).toBe(releasePending);
    expect(mockWindow.__devmock?.releasePending?.()).toBe(1);
    expect(start).toHaveBeenCalledOnce();
  });

  it("浏览器 worker 对受管未处理请求使用错误诊断，对其他请求保持旁路", async () => {
    await enableMocking();
    const options = start.mock.calls.at(-1)?.[0];
    const error = vi.fn();
    const print = { error };
    const origin = window.location.origin;

    options?.onUnhandledRequest?.(new Request(`${origin}/api/v1/missing`), print);
    options?.onUnhandledRequest?.(new Request(`${origin}/repository/raw-hosted/missing`), print);
    options?.onUnhandledRequest?.(new Request(`${origin}/assets/app.js`), print);
    options?.onUnhandledRequest?.(new Request("https://upstream.example/api/v1/missing"), print);

    expect(error).toHaveBeenCalledTimes(2);
  });
});
