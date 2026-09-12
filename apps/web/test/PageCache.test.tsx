// 页面数据缓存（SWR）与审计日志软失败行为（原消息中心用例迁移到审计页）：
// 1) 重挂载（页面来回切换）立即回放上次成功数据，不闪加载态、不重新从零拉取；
// 2) 后台刷新失败时保留旧数据并给警告横幅，不整页死胡同；
// 3) 无任何缓存时失败仍是显式错误态，场景恢复后可重试成功。
// devmock 场景经页面 URL 的 __mock 参数驱动（client 按其注入场景请求头）。
import { act, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it } from "vitest";

import { AuditLogPage } from "../src/pages/AuditLogPage";
import { REFRESH_EVENT, clearAsyncCache } from "../src/hooks/useAsync";
import { renderWithProviders } from "./harness";

describe("页面缓存与软失败（审计日志）", () => {
  beforeEach(() => {
    clearAsyncCache();
    window.history.replaceState({}, "", "/audit-logs");
  });

  it("重挂载立即回放缓存数据，不闪加载态", async () => {
    const first = renderWithProviders(<AuditLogPage />, {
      route: "/audit-logs",
      authenticated: true,
    });
    expect(await first.findByText("审计记录")).toBeTruthy();
    first.unmount();

    const second = renderWithProviders(<AuditLogPage />, {
      route: "/audit-logs",
      authenticated: true,
    });
    // 同步断言：缓存命中时首帧就渲染数据（此时后台刷新尚未完成）。
    expect(second.getByText("审计记录")).toBeTruthy();
  });

  it("后台刷新失败保留旧数据并给警告，不整页报错", async () => {
    const view = renderWithProviders(<AuditLogPage />, {
      route: "/audit-logs",
      authenticated: true,
    });
    expect(await view.findByText("审计记录")).toBeTruthy();

    window.history.replaceState({}, "", "/audit-logs?__mock=error");
    act(() => {
      window.dispatchEvent(new Event(REFRESH_EVENT));
    });
    expect(view.getByText("审计记录")).toBeTruthy();
    expect(await view.findByText(/最近一次刷新失败/)).toBeTruthy();
    expect(view.queryByText(/当前节点审计暂时不可用/)).toBeNull();
  });

  it("无缓存时失败仍显式报错，场景恢复后可重试成功", async () => {
    window.history.replaceState({}, "", "/audit-logs?__mock=error");
    const view = renderWithProviders(<AuditLogPage />, {
      route: "/audit-logs",
      authenticated: true,
    });
    expect(await view.findByText(/当前节点审计暂时不可用/)).toBeTruthy();

    window.history.replaceState({}, "", "/audit-logs");
    await userEvent.setup().click(screen.getByRole("button", { name: "重试" }));
    expect(await view.findByText("审计记录")).toBeTruthy();
  });
});
