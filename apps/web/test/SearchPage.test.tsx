// 搜索页（之后的主从双栏）集成测试：
// - 表达式查询 + 仓库分面钻取条仍然可用；
// - 请求口径变成 `group_by=package&order=relevance`（默认排序从 updated desc 改为相关度）；
// - 排序偏好仍由 SEARCH_SORT_KEY 记忆，且切换排序**不再发请求**（前端重排）；
// - 四态（错误 / 空 / 加载 / 备用只读场景）只替换结果区，搜索框始终可用；
// - 换关键词时旧结果留在原位，不被首载骨架替换（keepPreviousData 语义）。
import { http, HttpResponse } from "msw";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { server } from "@jianartifact/devmock/node";
import {
  releaseDevMockPendingRequests,
  waitForDevMockPendingRequest,
} from "@jianartifact/devmock/scenario";
import { SearchPage, SEARCH_SORT_KEY } from "../src/pages/SearchPage";
import { renderWithProviders } from "./harness";

/**
 * 文件级搜索结果条目（`group_by=package` 的 `items[]`，分页仍按文件维度）。
 * 包级结构另由 `packageResponse` 构造：页面主数据源是 `packages`。
 */
function searchItem(path: string) {
  return {
    repository: "maven-releases",
    path,
    size: 20480,
    hash: "0".repeat(64),
    sha1: "1".repeat(40),
    updatedAt: "2026-01-04T00:00:00Z",
  };
}

/** 单个包节点：坐标取自 path 的末三段（group/artifact/version）。 */
function packageOf(path: string) {
  const segments = path.split("/");
  const name = segments.at(-1) ?? path;
  const version = segments.at(-2) ?? "1.0.0";
  const artifact = segments.at(-3) ?? "app";
  const coordinate = `${segments.slice(0, -3).join(".")}:${artifact}`;
  return {
    coordinate,
    kind: "maven" as const,
    repositories: ["maven-releases"],
    versionCount: 1,
    fileCount: 1,
    bestPrecision: "none" as const,
    latestUpdatedAt: "2026-01-04T00:00:00Z",
    topVersion: version,
    versions: [
      {
        version,
        precision: "none" as const,
        fileCount: 1,
        size: 20480,
        latestUpdatedAt: "2026-01-04T00:00:00Z",
        files: [
          {
            name,
            path,
            repository: "maven-releases",
            size: 20480,
            sha1: "1".repeat(40),
            updatedAt: "2026-01-04T00:00:00Z",
            fileKind: "jar",
          },
        ],
      },
    ],
  };
}

/** 构造 `group_by=package` 的搜索结果响应体。 */
function packageResponse(paths: string[]) {
  const items = paths.map(searchItem);
  const packages = paths.map(packageOf);
  return HttpResponse.json({
    items,
    total: items.length,
    facets: paths.length ? [{ repository: "maven-releases", count: paths.length }] : [],
    packageTotal: packages.length,
    packages,
    aggregation: { scanned: items.length, truncated: false },
    appliedOrder: "relevance",
  });
}

/**
 * 覆盖 /search 端点并返回记录到的请求 URL 数组。
 * 用自建 handler 而非 devmock 种子，便于精确断言请求参数并控制返回内容。
 */
function captureSearch(paths: string[]): URL[] {
  const calls: URL[] = [];
  server.use(
    http.get("*/api/v1/search", ({ request }) => {
      calls.push(new URL(request.url));
      return packageResponse(paths);
    }),
  );
  return calls;
}

/** 按测试 id + 数据属性定位树行（高亮会把文本切成多个元素，不能靠纯文本查询）。 */
function packageRow(coordinate: string): HTMLElement {
  const row = screen
    .getAllByTestId("search-package-row")
    .find((element) => element.getAttribute("data-coordinate") === coordinate);
  if (!row) {
    throw new Error(`未找到包行 ${coordinate}`);
  }
  return row;
}

describe("制品搜索 Mock", () => {
  it("按表达式查询后展示仓库聚合与包级结果树", async () => {
    renderWithProviders(<SearchPage />, { route: "/search?q=app", authenticated: true });

    expect(await screen.findByTestId("search-package-tree")).toBeTruthy();
    expect(packageRow("com.example:app")).toBeTruthy();
    // 分面计数随种子样本数变化，只断言"该仓库出现在聚合里并有计数"。
    expect(screen.getByText(/maven-releases \d+/)).toBeTruthy();
    // 右栏默认展示首个包首个版本的首个文件详情（种子数据里首个包不一定是 app）。
    const panel = await screen.findByTestId("search-detail-panel");
    expect(within(panel).getByRole("heading")).toBeTruthy();
    expect(within(panel).getByText("包坐标")).toBeTruthy();
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
    // 空态只替换结果区：搜索框仍在，用户可以直接换词。
    expect(screen.getByPlaceholderText(/搜索制品/)).toBeTruthy();
  });

  it("loading 场景仅在显式释放后显示搜索结果", async () => {
    const route = "/search?q=app&__mock=loading";
    window.history.replaceState({}, "", route);
    renderWithProviders(<SearchPage />, { route, authenticated: true });

    // 双栏各自一块骨架，故加载态锚点不止一个。
    expect((await screen.findAllByTestId("state-loading")).length).toBeGreaterThan(0);
    await waitForDevMockPendingRequest();
    expect(releaseDevMockPendingRequests()).toBeGreaterThan(0);
    expect(await screen.findByTestId("search-package-tree")).toBeTruthy();
  });

  it("备用只读场景仍可读取并浏览搜索结果", async () => {
    const route = "/search?q=app&__mock=standby_read_only";
    window.history.replaceState({}, "", route);
    renderWithProviders(<SearchPage />, { route, authenticated: true });

    expect(await screen.findByTestId("search-package-tree")).toBeTruthy();
    expect(packageRow("com.example:app")).toBeTruthy();
  });
});

describe("搜索排序（默认 / 记忆 / 前端重排）", () => {
  it("默认按相关度请求包级聚合（group_by=package&order=relevance）", async () => {
    const calls = captureSearch(["com/example/app/1.0.0/app-1.0.0.jar"]);
    renderWithProviders(<SearchPage />, { route: "/search?q=app", authenticated: true });

    expect(await screen.findByTestId("search-package-tree")).toBeTruthy();
    expect(calls).toHaveLength(1);
    expect(calls[0].searchParams.get("group_by")).toBe("package");
    expect(calls[0].searchParams.get("order")).toBe("relevance");
    // 相关度不下推 sort：其它排序键由前端重排。
    expect(calls[0].searchParams.get("sort")).toBeNull();
    // 包级聚合不分页：后端聚合扫描预算固定、total 是 COUNT(*)，分页参数换不出新内容，
    // 只会多打一次全量聚合，因此**不得**带上（页面也没有分页入口）。
    expect(calls[0].searchParams.get("page")).toBeNull();
    expect(calls[0].searchParams.get("page_size")).toBeNull();
  });

  it("切换排序写入 localStorage，但不产生新的搜索请求", async () => {
    const user = userEvent.setup();
    const calls = captureSearch([
      "com/example/app/1.0.0/app-1.0.0.jar",
      "com/example/zeta/2.0.0/zeta-2.0.0.jar",
    ]);
    renderWithProviders(<SearchPage />, { route: "/search?q=app", authenticated: true });

    await screen.findByTestId("search-package-tree");
    expect(calls).toHaveLength(1);

    // 排序控件从「相关度」切到「名称」（Mantine Select 的可访问名落在内部 input 上）。
    await user.click(screen.getAllByLabelText("排序")[0]!);
    await user.click(await screen.findByRole("option", { name: "名称" }));

    await waitFor(() => {
      expect(JSON.parse(localStorage.getItem(SEARCH_SORT_KEY) ?? "null")).toEqual({
        by: "name",
        order: "asc",
      });
    });
    // 前端重排：不重发请求，左栏仍按包分组。
    expect(calls).toHaveLength(1);
    expect(packageRow("com.example:app")).toBeTruthy();
    expect(packageRow("com.example:zeta")).toBeTruthy();
  });

  it("重挂载时从 localStorage 读回已记忆的排序（只影响前端顺序）", async () => {
    localStorage.setItem(SEARCH_SORT_KEY, JSON.stringify({ by: "name", order: "desc" }));
    const calls = captureSearch(["com/example/app/1.0.0/app-1.0.0.jar"]);
    renderWithProviders(<SearchPage />, { route: "/search?q=app", authenticated: true });

    expect(await screen.findByTestId("search-package-tree")).toBeTruthy();
    // 请求口径恒定，记忆值只决定左栏的展示顺序。
    expect(calls[0].searchParams.get("group_by")).toBe("package");
    expect(calls[0].searchParams.get("order")).toBe("relevance");
  });

  it("localStorage 中的脏值回退到默认排序（相关度）", async () => {
    localStorage.setItem(SEARCH_SORT_KEY, JSON.stringify({ by: "bogus", order: "sideways" }));
    renderWithProviders(<SearchPage />, { route: "/search?q=app", authenticated: true });

    expect(await screen.findByTestId("search-package-tree")).toBeTruthy();
    // 脏值不影响渲染：仍按默认相关度出包树。
    expect(packageRow("com.example:app")).toBeTruthy();
  });

  it("换关键词时旧结果保留到新数据到达（不闪首载骨架）", async () => {
    const user = userEvent.setup();
    let release: (() => void) | undefined;
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    let callCount = 0;
    server.use(
      http.get("*/api/v1/search", async () => {
        callCount += 1;
        // 第二次请求（换关键词后）挂在闸门后，以便观察切换过程中的中间态。
        if (callCount > 1) await gate;
        return packageResponse(
          callCount > 1
            ? ["com/example/sorted/9.9.9/sorted-9.9.9.jar"]
            : ["com/example/app/1.0.0/app-1.0.0.jar"],
        );
      }),
    );
    renderWithProviders(<SearchPage />, { route: "/search?q=app", authenticated: true });
    expect(await screen.findByTestId("search-package-tree")).toBeTruthy();

    const input = screen.getByPlaceholderText(/搜索制品/);
    await user.clear(input);
    await user.type(input, "sorted{Enter}");

    // 新请求尚未返回：旧树必须仍在 DOM 中，且不出现首载骨架屏。
    await waitFor(() => expect(callCount).toBeGreaterThan(1));
    expect(packageRow("com.example:app")).toBeTruthy();
    expect(screen.queryByTestId("state-loading")).toBeNull();
    // 此时展示的仍是上一次搜索的结果，必须在结果区顶部点名，避免误读成新关键词的结果。
    expect(screen.getByTestId("search-stale-results")).toBeTruthy();

    release?.();
    // 新数据到达后就地替换，全程没有退化成骨架。
    await waitFor(() => expect(packageRow("com.example:sorted")).toBeTruthy());
    expect(screen.queryByTestId("state-loading")).toBeNull();
    expect(screen.queryAllByTestId("search-package-row")).toHaveLength(1);
    // 结果已属于当前关键词：上一次结果的提示随之消失。
    expect(screen.queryByTestId("search-stale-results")).toBeNull();
  });

  it("换关键词后搜索失败：保留旧结果并给出非阻断告警与重试入口", async () => {
    const user = userEvent.setup();
    let callCount = 0;
    server.use(
      http.get("*/api/v1/search", () => {
        callCount += 1;
        // 首次搜索成功；换关键词后的那次失败（500）。
        return callCount > 1
          ? HttpResponse.json(
              { error: { code: "upstream_error", message: "搜索服务暂不可用" } },
              { status: 500 },
            )
          : packageResponse(["com/example/app/1.0.0/app-1.0.0.jar"]);
      }),
    );
    renderWithProviders(<SearchPage />, { route: "/search?q=app", authenticated: true });
    expect(await screen.findByTestId("search-package-tree")).toBeTruthy();

    const input = screen.getByPlaceholderText(/搜索制品/);
    await user.clear(input);
    await user.type(input, "sorted{Enter}");

    // 刷新失败不替换内容：旧结果仍在场，只多出一条非阻断告警（不是整块错误态）。
    const alert = await screen.findByTestId("search-refresh-error");
    expect(within(alert).getByText("刷新失败")).toBeTruthy();
    expect(within(alert).getByText(/搜索服务暂不可用/)).toBeTruthy();
    expect(within(alert).getByRole("button", { name: "重试" })).toBeTruthy();
    expect(packageRow("com.example:app")).toBeTruthy();
    // 告警已说明「仍展示上次数据」，不必再叠一条上一次结果的提示。
    expect(screen.queryByTestId("search-stale-results")).toBeNull();
  });
});
