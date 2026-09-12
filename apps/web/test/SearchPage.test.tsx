import { http, HttpResponse } from "msw";
import { screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { server } from "@jianartifact/devmock/node";
import {
  releaseDevMockPendingRequests,
  waitForDevMockPendingRequest,
} from "@jianartifact/devmock/scenario";
import { SearchPage } from "../src/pages/SearchPage";
import { renderWithProviders } from "./harness";

describe("制品搜索 Mock", () => {
  it("按表达式查询后展示仓库聚合与制品结果", async () => {
    renderWithProviders(<SearchPage />, { route: "/search?q=app", authenticated: true });

    expect(await screen.findByText("app-1.0.0.jar")).toBeTruthy();
    // 分面计数随种子样本数变化，只断言"该仓库出现在聚合里并有计数"。
    expect(screen.getByText(/maven-releases \d+/)).toBeTruthy();
  });

  it("搜索服务失败时提供可重试的错误反馈", async () => {
    server.use(
      http.get("*/api/v1/search", () =>
        HttpResponse.json(
          { error: { code: "upstream_error", message: "搜索服务暂不可用" } },
          { status: 500 },
        ),
      ),
    );
    renderWithProviders(<SearchPage />, { route: "/search?q=app", authenticated: true });

    expect(await screen.findByText("搜索服务暂不可用")).toBeTruthy();
    expect(screen.getByRole("button", { name: "重试" })).toBeTruthy();
  });

  it("empty 场景明确展示无结果", async () => {
    const route = "/search?q=app&__mock=empty";
    window.history.replaceState({}, "", route);
    renderWithProviders(<SearchPage />, { route, authenticated: true });

    expect(await screen.findByText("未找到匹配的制品")).toBeTruthy();
  });

  it("loading 场景仅在显式释放后显示搜索结果", async () => {
    const route = "/search?q=app&__mock=loading";
    window.history.replaceState({}, "", route);
    renderWithProviders(<SearchPage />, { route, authenticated: true });

    expect(await screen.findByTestId("state-loading")).toBeTruthy();
    await waitForDevMockPendingRequest();
    expect(releaseDevMockPendingRequests()).toBeGreaterThan(0);
    expect(await screen.findByText("app-1.0.0.jar")).toBeTruthy();
  });

  it("备用只读场景仍可读取并筛选搜索结果", async () => {
    const route = "/search?q=app&__mock=standby_read_only";
    window.history.replaceState({}, "", route);
    renderWithProviders(<SearchPage />, { route, authenticated: true });

    expect(await screen.findByText("app-1.0.0.jar")).toBeTruthy();
  });
});
