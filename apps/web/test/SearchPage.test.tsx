import { http, HttpResponse } from "msw";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { server } from "@jianartifact/devmock/node";
import {
  releaseDevMockPendingRequests,
  waitForDevMockPendingRequest,
} from "@jianartifact/devmock/scenario";
import { SearchPage, SEARCH_SORT_KEY } from "../src/pages/SearchPage";
import { renderWithProviders } from "./harness";

/** 搜索结果条目工厂：只需 path 有区分度即可，其余字段填合法占位。 */
function searchItem(path: string) {
  return {
    repository: "maven-releases",
    path,
    size: 20480,
    hash: "0".repeat(64),
    updatedAt: "2026-01-04T00:00:00Z",
  };
}

/** 构造搜索结果响应体。 */
function searchResponse(paths: string[]) {
  return HttpResponse.json({
    items: paths.map(searchItem),
    total: paths.length,
    facets: paths.length ? [{ repository: "maven-releases", count: paths.length }] : [],
  });
}

/**
 * 覆盖 /search 端点并返回记录到的请求 URL 数组。
 * 用自建 handler 而非 devmock 种子，便于精确断言排序参数并控制返回内容。
 */
function captureSearch(paths: string[]): URL[] {
  const calls: URL[] = [];
  server.use(
    http.get("*/api/v1/search", ({ request }) => {
      calls.push(new URL(request.url));
      return searchResponse(paths);
    }),
  );
  return calls;
}

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

describe("搜索排序（默认 / 记忆 / 布局稳定）", () => {
  it("默认按修改时间倒序（新的在前）请求", async () => {
    const calls = captureSearch(["com/example/app/1.0.0/app-1.0.0.jar"]);
    renderWithProviders(<SearchPage />, { route: "/search?q=app", authenticated: true });

    expect(await screen.findByText("app-1.0.0.jar")).toBeTruthy();
    expect(calls).toHaveLength(1);
    expect(calls[0].searchParams.get("sort")).toBe("updated");
    expect(calls[0].searchParams.get("order")).toBe("desc");
  });

  it("点击表头切换排序后写入 localStorage", async () => {
    const user = userEvent.setup();
    captureSearch(["com/example/app/1.0.0/app-1.0.0.jar"]);
    renderWithProviders(<SearchPage />, { route: "/search?q=app", authenticated: true });

    await screen.findByText("app-1.0.0.jar");
    // 点「名称」表头：换列 → 重置为升序
    await user.click(screen.getByRole("columnheader", { name: "名称" }));

    await waitFor(() => {
      expect(JSON.parse(localStorage.getItem(SEARCH_SORT_KEY) ?? "null")).toEqual({
        by: "name",
        order: "asc",
      });
    });
  });

  it("重挂载时从 localStorage 读回已记忆的排序", async () => {
    localStorage.setItem(SEARCH_SORT_KEY, JSON.stringify({ by: "name", order: "asc" }));
    const calls = captureSearch(["com/example/app/1.0.0/app-1.0.0.jar"]);
    renderWithProviders(<SearchPage />, { route: "/search?q=app", authenticated: true });

    expect(await screen.findByText("app-1.0.0.jar")).toBeTruthy();
    expect(calls[0].searchParams.get("sort")).toBe("name");
    expect(calls[0].searchParams.get("order")).toBe("asc");
  });

  it("localStorage 中的脏值回退到默认排序", async () => {
    localStorage.setItem(SEARCH_SORT_KEY, JSON.stringify({ by: "bogus", order: "sideways" }));
    const calls = captureSearch(["com/example/app/1.0.0/app-1.0.0.jar"]);
    renderWithProviders(<SearchPage />, { route: "/search?q=app", authenticated: true });

    expect(await screen.findByText("app-1.0.0.jar")).toBeTruthy();
    expect(calls[0].searchParams.get("sort")).toBe("updated");
    expect(calls[0].searchParams.get("order")).toBe("desc");
  });

  it("切换排序时旧结果不被首载骨架替换（布局不跳动）", async () => {
    const user = userEvent.setup();
    let release: (() => void) | undefined;
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    let callCount = 0;
    server.use(
      http.get("*/api/v1/search", async () => {
        callCount += 1;
        // 第二次请求（排序后）挂在闸门后，以便观察切换过程中的中间态。
        if (callCount > 1) await gate;
        return searchResponse(
          callCount > 1
            ? ["com/example/sorted/9.9.9/sorted-9.9.9.jar"]
            : ["com/example/app/1.0.0/app-1.0.0.jar"],
        );
      }),
    );
    renderWithProviders(<SearchPage />, { route: "/search?q=app", authenticated: true });
    expect(await screen.findByText("app-1.0.0.jar")).toBeTruthy();

    await user.click(screen.getByRole("columnheader", { name: "名称" }));

    // 排序请求尚未返回：旧行必须仍在 DOM 中，且不出现首载骨架屏 / 加载态。
    expect(screen.getByText("app-1.0.0.jar")).toBeTruthy();
    expect(screen.queryByTestId("state-loading")).toBeNull();

    release?.();
    // 新数据到达后就地替换，布局始终未退化为骨架。
    expect(await screen.findByText("sorted-9.9.9.jar")).toBeTruthy();
    expect(screen.queryByTestId("state-loading")).toBeNull();
  });
});
