// 开发态场景隔离：上一用例留下的 loading 请求和浏览器路由不能影响下一用例。
import {
  pendingDevMockRequests,
  waitForDevMockPendingRequest,
} from "@jianartifact/devmock/scenario";
import { screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { UsersPage } from "../src/pages/UsersPage";
import { renderWithProviders } from "./harness";

describe("开发态测试隔离", () => {
  it("可以留下 loading 请求以验证全局清理", async () => {
    window.history.replaceState({}, "", "/users?__mock=loading");
    renderWithProviders(<UsersPage />, {
      route: "/users?__mock=loading",
      authenticated: true,
    });

    expect(await screen.findByTestId("state-loading")).toBeTruthy();
    await waitForDevMockPendingRequest();
    expect(pendingDevMockRequests()).toBeGreaterThan(0);
  });

  it("下一用例从空闲的正常根路由开始", () => {
    expect(pendingDevMockRequests()).toBe(0);
    expect(window.location.pathname).toBe("/");
    expect(window.location.search).toBe("");
  });
});
