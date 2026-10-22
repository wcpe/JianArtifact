// 搜索页的进度 / 取消 / 超时 / 乱序回归：
// - 搜索进行中在结果区顶部显示伪进度、阶段文案、已用时长与取消按钮，完成后立即收敛；
// - 取消经 AbortSignal 真实中止请求，界面静默回到空闲态（不算失败），并可立即重搜；
// - 后端 504 search_timeout 给「超时」专属文案，其它失败仍是通用文案；
// - 快速连续搜索：旧请求被中止，旧响应晚到也不得覆盖新结果；
// - 同口径后台刷新沿用 keepPreviousData：旧结果留在原位 + 局部进度反馈，不回到骨架，
//   且不重置用户已展开 / 已选中的节点；包级聚合没有分页入口。
//
// 伪进度本身（曲线 / 阶段 / 时长 / 定时器清理）在 hooks/usePseudoProgress.test.ts 与
// components/search/SearchProgress.test.tsx 里用假定时器精确断言，本文件只管页面集成。
import { MantineProvider } from "@mantine/core";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import "@testing-library/jest-dom/vitest";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { MemoryRouter } from "react-router-dom";
import { describe, expect, it } from "vitest";

import { server } from "@jianartifact/devmock/node";

import type { SearchPackage, SearchResult } from "../api/endpoints";
import { AuthProvider } from "../auth/AuthContext";
import { REFRESH_EVENT } from "../hooks/useAsync";
import "../i18n";
import { SearchPage } from "./SearchPage";

function file(name: string, path: string, size = 1024) {
  return {
    name,
    path,
    repository: "maven-releases",
    size,
    sha1: `sha1-${name}`,
    updatedAt: "2026-10-18 09:12:33",
    fileKind: "jar",
  };
}

function pkg(coordinate: string): SearchPackage {
  const name = coordinate.split(":")[1]!;
  return {
    coordinate,
    kind: "maven",
    repositories: ["maven-releases"],
    versionCount: 1,
    fileCount: 1,
    bestPrecision: "exact",
    latestUpdatedAt: "2026-10-18 09:12:33",
    topVersion: "1.0.0",
    versions: [
      {
        version: "1.0.0",
        precision: "exact",
        fileCount: 1,
        size: 1024,
        latestUpdatedAt: "2026-10-18 09:12:33",
        files: [file(`${name}-1.0.0.jar`, `com/example/${name}/1.0.0/${name}-1.0.0.jar`)],
      },
    ],
  };
}

const OLD_PKG = pkg("com.example:old");
const NEW_PKG = pkg("com.example:new");

function searchResponse(packages: SearchPackage[], total = packages.length): SearchResult {
  return {
    items: [],
    total,
    facets: [{ repository: "maven-releases", count: total }],
    packageTotal: packages.length,
    packages,
    aggregation: { scanned: total, truncated: false },
    appliedOrder: "relevance",
  };
}

function renderPage(q = "keyword") {
  return render(
    <MantineProvider>
      {/* 搜索页要读会话身份（取数依赖与缓存键都带 user.id），必须包在 AuthProvider 里。 */}
      <AuthProvider>
        <MemoryRouter initialEntries={[`/search?q=${encodeURIComponent(q)}`]}>
          <SearchPage />
        </MemoryRouter>
      </AuthProvider>
    </MantineProvider>,
  );
}

/** 覆写搜索端点：body 可以是即时响应，也可以是「被调用后由用例手动落定」的挂起响应。 */
function mockSearch(body: SearchResult | ((request: Request) => Promise<Response> | Response)) {
  const calls: string[] = [];
  server.use(
    http.get("*/api/v1/search", ({ request }) => {
      calls.push(request.url);
      return typeof body === "function" ? body(request) : HttpResponse.json(body as never);
    }),
  );
  return calls;
}

/** 构造一个由用例控制落定时机的挂起响应（配合 request.signal 观察是否被中止）。 */
function hangingResponse(signalSpy?: { aborted: boolean }) {
  let release: (() => void) | undefined;
  const response = async (request: Request) => {
    if (signalSpy) {
      request.signal.addEventListener("abort", () => {
        signalSpy.aborted = true;
      });
    }
    await new Promise<void>((resolve) => {
      release = resolve;
      // 兜底：即便请求没有被中止（例如运行时不转发 signal），5s 后也自行落定，
      // 避免用例结束时留下永久挂起的 handler。
      setTimeout(resolve, 5_000);
    });
    return HttpResponse.json(searchResponse([NEW_PKG]) as never);
  };
  return { response, release: () => release?.() };
}

const progressBar = () => screen.queryByTestId("search-progress");
const tree = () => screen.getByTestId("search-package-tree");

describe("SearchPage 搜索进度与取消", () => {
  it("搜索进行中显示进度条、阶段文案、已用时长与取消按钮，完成后立即消失", async () => {
    const hanging = hangingResponse();
    mockSearch(hanging.response);
    renderPage();

    const progress = await screen.findByTestId("search-progress");
    // 阶段文案 + 已用时长（同一行文本，如「正在检索索引… · 0.0s」）。
    expect(progress).toHaveTextContent("正在检索索引…");
    expect(progress).toHaveTextContent(/· \d+\.\ds/);
    expect(within(progress).getByRole("progressbar")).toBeInTheDocument();
    expect(within(progress).getByRole("button", { name: "取消搜索" })).toBeInTheDocument();
    // 首载骨架与进度条共存：骨架仍然显示在结果区（不整体卸载搜索框与 facet 条）。
    // 双栏各一块骨架，所以按集合断言存在即可，不锁死具体块数。
    expect(screen.getAllByTestId("state-loading").length).toBeGreaterThan(0);
    expect(screen.getByPlaceholderText(/搜索制品/)).toBeInTheDocument();

    hanging.release();

    // 数据到达后进度条立刻收敛，不残留。
    await screen.findByTestId("search-package-tree");
    await waitFor(() => expect(progressBar()).toBeNull());
  });

  it("点取消后请求被中止，界面静默回到空闲态且不报错，可立即重新搜索", async () => {
    const signalSpy = { aborted: false };
    const hanging = hangingResponse(signalSpy);
    let calls = 0;
    server.use(
      http.get("*/api/v1/search", ({ request }) => {
        calls += 1;
        if (calls === 1) return hanging.response(request);
        return HttpResponse.json(searchResponse([NEW_PKG]) as never);
      }),
    );
    renderPage();

    await screen.findByTestId("search-progress");
    await userEvent.click(screen.getByRole("button", { name: "取消搜索" }));

    // 请求确实被 AbortSignal 中止，而不是只把响应丢掉。
    await waitFor(() => expect(signalSpy.aborted).toBe(true));
    // 取消不是失败：回到空闲态，无错误态、无进度条。
    expect(await screen.findByTestId("search-cancelled")).toBeInTheDocument();
    expect(screen.queryByTestId("state-error")).toBeNull();
    expect(progressBar()).toBeNull();

    // 取消后可立即重搜（同口径提交会显式重发，而不是因为 URL 没变而毫无反应）。
    await userEvent.click(screen.getByRole("button", { name: "搜索" }));
    await waitFor(() => expect(tree()).toBeInTheDocument());
    expect(screen.queryByTestId("search-cancelled")).toBeNull();
  });

  it("后端 504 search_timeout 显示超时专属文案", async () => {
    mockSearch(
      () =>
        HttpResponse.json(
          { error: { code: "search_timeout", message: "搜索超时，请缩小检索范围后重试" } },
          { status: 504 },
        ) as never,
    );
    renderPage();

    expect(await screen.findByText("搜索超时")).toBeInTheDocument();
    expect(screen.getByText(/检索耗时超出限制/)).toBeInTheDocument();
    // 失败同样立即收敛，不残留半截进度条。
    expect(progressBar()).toBeNull();
  });

  it("其它失败仍用通用错误文案，不冒充超时", async () => {
    mockSearch(
      () =>
        HttpResponse.json(
          { error: { code: "internal", message: "搜索服务不可用" } },
          { status: 500 },
        ) as never,
    );
    renderPage();

    expect(await screen.findByTestId("state-error")).toBeInTheDocument();
    expect(screen.getByText("加载失败")).toBeInTheDocument();
    expect(screen.getByText("搜索服务不可用")).toBeInTheDocument();
    expect(screen.queryByText("搜索超时")).toBeNull();
  });

  it("快速连续搜索：旧请求被中止，旧响应晚到不覆盖新结果", async () => {
    const oldSignal = { aborted: false };
    let oldRelease: (() => void) | undefined;
    const pendingNew: Array<() => void> = [];
    server.use(
      http.get("*/api/v1/search", async ({ request }) => {
        const q = new URL(request.url).searchParams.get("q");
        if (q === "旧") {
          request.signal.addEventListener("abort", () => {
            oldSignal.aborted = true;
          });
          oldRelease = () => {};
          await new Promise<void>((resolve) => {
            oldRelease = resolve;
            setTimeout(resolve, 5_000);
          });
          return HttpResponse.json(searchResponse([OLD_PKG]) as never);
        }
        // 新查询：同样由用例手动落定，确保「旧响应更晚返回」的时序。
        await new Promise<void>((resolve) => {
          pendingNew.push(resolve);
        });
        return HttpResponse.json(searchResponse([NEW_PKG]) as never);
      }),
    );

    renderPage("旧");
    await screen.findByTestId("search-progress");

    const input = screen.getByPlaceholderText(/搜索制品/);
    await userEvent.clear(input);
    await userEvent.type(input, "新{Enter}");

    // 新请求先落定 → 列表切到新结果。
    await waitFor(() => expect(pendingNew).toHaveLength(1));
    pendingNew[0]!();
    await waitFor(() => expect(within(tree()).getByText("com.example:new")).toBeInTheDocument());

    // 旧请求在切换那一刻已被中止；即便响应迟到也不得覆盖新结果。
    expect(oldSignal.aborted).toBe(true);
    oldRelease?.();
    await waitFor(() => expect(within(tree()).queryByText("com.example:old")).toBeNull());
    expect(within(tree()).getByText("com.example:new")).toBeInTheDocument();
  });

  it("同口径后台刷新保留旧结果与用户的展开 / 选中，不回到骨架也不重置视图", async () => {
    let releaseSecond: (() => void) | undefined;
    let calls = 0;
    server.use(
      http.get("*/api/v1/search", async () => {
        calls += 1;
        if (calls === 1) {
          return HttpResponse.json(searchResponse([OLD_PKG, NEW_PKG]) as never);
        }
        await new Promise<void>((resolve) => {
          releaseSecond = resolve;
        });
        return HttpResponse.json(searchResponse([OLD_PKG, NEW_PKG]) as never);
      }),
    );

    renderPage();
    await screen.findByTestId("search-package-tree");

    // 把展开与选中都切到第二个包（默认是首个包）：刷新后二者都必须原样保留。
    await userEvent.click(
      screen
        .getAllByTestId("search-package-row")
        .find((row) => row.getAttribute("data-coordinate") === "com.example:new")!,
    );
    const heading = () =>
      within(screen.getByTestId("search-detail-panel")).getByRole("heading").textContent;
    await waitFor(() => expect(heading()).toBe("new-1.0.0.jar"));

    // 页眉刷新派发的是全局刷新事件：同一搜索口径的重新取数。
    act(() => {
      window.dispatchEvent(new CustomEvent(REFRESH_EVENT));
    });

    // 刷新期间：进度条出现，旧结果仍在原位、没有退回骨架。
    await waitFor(() => expect(progressBar()).toBeInTheDocument());
    expect(tree()).toBeInTheDocument();
    expect(screen.queryByTestId("state-loading")).toBeNull();

    releaseSecond?.();
    await waitFor(() => expect(progressBar()).toBeNull());

    // 刷新落定后：选中项仍是原来那个文件（不跳回首个包的首个版本）。
    await waitFor(() => expect(heading()).toBe("new-1.0.0.jar"));
    expect(calls).toBeGreaterThanOrEqual(2);
    // 包级聚合没有分页入口：`total` 再大也不该出现页码按钮。
    expect(screen.queryByRole("button", { name: "2" })).toBeNull();
  });
});
