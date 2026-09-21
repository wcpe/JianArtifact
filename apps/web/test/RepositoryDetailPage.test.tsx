// 仓库详情集成测试：目录树逐级展开点选文件后渲染详情/使用说明；匿名访问 private 仓库报未认证。
// FR-74：整页固定布局（面板内滚）、未登录仅页眉一个登录入口、客户端发布提示收纳为紧凑小字。
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";
import { Route, Routes } from "react-router-dom";

import { store } from "@jianartifact/devmock";
import { server } from "@jianartifact/devmock/node";
import {
  releaseDevMockPendingRequests,
  waitForDevMockPendingRequest,
} from "@jianartifact/devmock/scenario";
import { AppRoutes } from "../src/app/router";
import { RepositoryDetailPage } from "../src/pages/RepositoryDetailPage";
import { formatUtcToLocalDate } from "../src/lib/timeFormat";
import { renderWithProviders } from "./harness";

function renderDetail(name: string, authenticated: boolean, query = "") {
  return renderWithProviders(
    <Routes>
      <Route path="/repositories/:name" element={<RepositoryDetailPage />} />
    </Routes>,
    { route: `/repositories/${name}${query}`, authenticated },
  );
}

function renderDetailScenario(scenario: "empty" | "loading" | "error" | "standby_read_only") {
  const route = `/repositories/maven-releases?__mock=${scenario}`;
  window.history.replaceState({}, "", route);
  return renderWithProviders(
    <Routes>
      <Route path="/repositories/:name" element={<RepositoryDetailPage />} />
    </Routes>,
    { route, authenticated: true },
  );
}

describe("仓库详情", () => {
  it("目录树点选文件后渲染详情与使用说明", async () => {
    const user = userEvent.setup();
    renderDetail("maven-releases", true);
    // FR-54 懒加载目录树：根层仅有 com，逐级点开 maven 坐标目录
    await user.click(await screen.findByText("com"));
    await user.click(await screen.findByText("example"));
    await user.click(await screen.findByText("app"));
    await user.click(await screen.findByText("1.0.0"));
    await user.click(await screen.findByText("app-1.0.0.jar"));
    // 选中文件后右侧详情展示完整路径、maven 使用说明片段与复制按钮
    expect(await screen.findByText("com/example/app/1.0.0/app-1.0.0.jar")).toBeTruthy();
    expect(await screen.findByText("解析依赖（pom.xml）")).toBeTruthy();
    expect((await screen.findAllByRole("button", { name: "复制" })).length).toBeGreaterThan(0);
  });

  it("empty 场景展示空制品树", async () => {
    renderDetailScenario("empty");
    expect(await screen.findByText("暂无制品")).toBeTruthy();
  });

  it("loading 场景由测试显式释放后恢复正常目录树", async () => {
    renderDetailScenario("loading");
    expect((await screen.findAllByText("加载中…")).length).toBeGreaterThan(0);
    await waitForDevMockPendingRequest();
    expect(releaseDevMockPendingRequests()).toBeGreaterThan(0);
    expect(await screen.findByText("com")).toBeTruthy();
  });

  it("error 场景展示可重试错误态", async () => {
    renderDetailScenario("error");
    expect(await screen.findByTestId("state-error")).toBeTruthy();
    expect(screen.getByRole("button", { name: "重试" })).toBeTruthy();
  });

  it("备用只读场景仍允许浏览现有制品", async () => {
    renderDetailScenario("standby_read_only");
    expect(await screen.findByText("com")).toBeTruthy();
  });

  it("匿名访问 private 仓库展示未认证错误", async () => {
    renderDetail("maven-releases", false);
    // FR-66：匿名仅可读 public 仓库，private 目录树请求 401 → 错误提示
    expect((await screen.findAllByText("未认证")).length).toBeGreaterThan(0);
  });

  it("未登录访问详情页仅页眉一个登录入口（FR-74）", async () => {
    renderWithProviders(<AppRoutes />, { route: "/repositories/npm-proxy" });
    // 等详情页渲染完成（位置由全局页眉面包屑表达：概览 / 仓库 / npm-proxy）
    const breadcrumbs = await screen.findByTestId("app-breadcrumbs");
    expect(within(breadcrumbs).getByText("npm-proxy")).toBeTruthy();
    // 登录按钮只剩页眉一个，内容区不再重复
    expect(screen.getAllByRole("button", { name: "登录" })).toHaveLength(1);
  });

  it("非网页上传仓库的客户端发布提示收纳为紧凑小字（FR-74）", async () => {
    renderDetail("npm-proxy", true);
    // 紧凑提示 + 跳使用说明链接取代大块 Alert
    expect(await screen.findByText("查看使用说明")).toBeTruthy();
    expect(screen.queryByText("请用客户端发布")).toBeNull();
  });

  it("详情页外壳为固定高度布局，面板内滚（FR-74）", async () => {
    renderDetail("maven-releases", true);
    await screen.findByText("com");
    const shell = screen.getByTestId("repo-detail-shell");
    expect(shell.style.overflow).toBe("hidden");
    expect(shell.style.height).toContain("100dvh");
  });

  it("语义 Tab 固定在页头下方，窄屏可横向滚动（FR-122）", async () => {
    renderDetail("maven-releases", true);
    const tablist = await screen.findByRole("tablist");

    expect(tablist.style.position).toBe("sticky");
    expect(tablist.style.top).toBe("0px");
    expect(tablist.style.overflowX).toBe("auto");
    expect(screen.getAllByRole("tab").map((tab) => tab.textContent)).toEqual([
      "浏览",
      "配置",
      "ACL",
    ]);
  });

  it("拖拽分割条调整树宽并持久化到 localStorage", async () => {
    localStorage.setItem("jianartifact.treeWidth", JSON.stringify(420));
    renderDetail("maven-releases", true);

    const splitter = await screen.findByRole("separator", { name: "调整文件树宽度" });
    const treePanel = splitter.previousElementSibling as HTMLElement;
    expect(treePanel.style.width).toBe("420px");

    fireEvent.mouseDown(splitter, { clientX: 500 });
    fireEvent.mouseMove(window, { clientX: 580 });
    fireEvent.mouseUp(window);

    await waitFor(() => expect(treePanel.style.width).toBe("500px"));
    await waitFor(() =>
      expect(JSON.parse(localStorage.getItem("jianartifact.treeWidth") ?? "0")).toBe(500),
    );
  });

  it("group 仓库配置 tab 可编辑成员仓库（members）", async () => {
    // 复现：group 仓库 config tab 应提供成员仓库编辑控件；缺失则本测试失败。
    store.createRepository({
      name: "npm-public",
      format: "npm",
      type: "group",
      visibility: "private",
      members: ["npm-release"],
    });
    const user = userEvent.setup();
    renderDetail("npm-public", true);
    // 等详情页渲染，切到配置 tab
    await user.click(await screen.findByText("配置"));
    // group 仓库应能编辑成员仓库（members 多选/输入）；修复前不存在，修复后渲染（label 可能多处匹配）
    expect((await screen.findAllByLabelText(/成员仓库/)).length).toBeGreaterThan(0);
  });

  it("proxy 仓库配置 tab 显示连接状态并支持手动重测（FR-114）", async () => {
    const user = userEvent.setup();
    renderDetail("npm-proxy", true);
    await user.click(await screen.findByText("配置"));
    // devmock 种子：npm-proxy 上游可达 → 「可用」徽章。
    expect(await screen.findByText("可用")).toBeTruthy();
    // 重测按钮存在且可点击（online proxy）。
    const recheck = await screen.findByRole("button", { name: "重新探测" });
    expect(recheck).toBeTruthy();
    await user.click(recheck);
    // 重测后通知「重测完成」，状态仍为「可用」。
    expect(await screen.findByText("重测完成")).toBeTruthy();
    expect(screen.getByText("可用")).toBeTruthy();
  });

  it("offline 开关切换后连接状态显示离线（MD-2）", async () => {
    const user = userEvent.setup();
    renderDetail("npm-proxy", true);
    await user.click(await screen.findByText("配置"));
    expect(await screen.findByText("可用")).toBeTruthy();

    // 关掉在线开关 → 显示「离线」。
    await user.click(screen.getByRole("switch", { name: /在线/ }));
    expect(await screen.findByText("离线")).toBeTruthy();
    // offline 时重测按钮禁用。
    const recheck = await screen.findByRole("button", { name: "重新探测" });
    expect((recheck as HTMLButtonElement).disabled).toBe(true);

    // 重新打开在线开关 → 恢复内存态（mock 上游可达 → 可用）。
    await user.click(screen.getByRole("switch", { name: /在线/ }));
    expect(await screen.findByText("可用")).toBeTruthy();
    expect((screen.getByRole("button", { name: "重新探测" }) as HTMLButtonElement).disabled).toBe(
      false,
    );
  });

  it("配置保存成功后刷新详情中的可见性", async () => {
    const user = userEvent.setup();
    renderDetail("maven-releases", true);

    await user.click(await screen.findByRole("tab", { name: "配置" }));
    await user.click(screen.getAllByLabelText("可见性")[0]!);
    await user.click(await screen.findByRole("option", { name: "公开" }));
    await user.click(screen.getByRole("button", { name: "保存配置" }));

    expect(await screen.findByText("保存成功")).toBeTruthy();
    await waitFor(() => expect(screen.getAllByText("公开").length).toBeGreaterThan(0));
  });

  it("配置保存失败时保留可见性与描述输入", async () => {
    server.use(
      http.patch("*/api/v1/repositories/:name", () =>
        HttpResponse.json(
          { error: { code: "standby_read_only", message: "备用节点为只读，当前请求已拒绝" } },
          { status: 503 },
        ),
      ),
    );
    const user = userEvent.setup();
    renderDetail("maven-releases", true);

    await user.click(await screen.findByRole("tab", { name: "配置" }));
    const description = screen.getByLabelText("描述") as HTMLTextAreaElement;
    await user.clear(description);
    await user.type(description, "保留的配置说明");
    await user.click(screen.getAllByLabelText("可见性")[0]!);
    await user.click(await screen.findByRole("option", { name: "公开" }));
    await user.click(screen.getByRole("button", { name: "保存配置" }));

    expect(await screen.findByText("备用节点为只读，当前请求已拒绝")).toBeTruthy();
    expect(description.value).toBe("保留的配置说明");
    expect((screen.getAllByLabelText("可见性")[0]! as HTMLInputElement).value).toBe("公开");
  });

  it("页头概览的创建时间按浏览器本地时区展示，不做 UTC 截断", async () => {
    // 概览行用 visibleFrom="md" 承载，而 jsdom 的 matchMedia 默认一律不匹配
    // （test/setup.ts 返回 matches:false）——这里临时按「桌面」渲染，跑完立刻还原。
    const originalMatchMedia = window.matchMedia;
    window.matchMedia = ((query: string) =>
      ({
        matches: query.includes("min-width"),
        media: query,
        onchange: null,
        addListener: () => {},
        removeListener: () => {},
        addEventListener: () => {},
        removeEventListener: () => {},
        dispatchEvent: () => false,
      }) as MediaQueryList) as typeof window.matchMedia;

    try {
      renderDetail("maven-releases", true);
      // 种子值来自 packages/devmock 的仓库列表（maven-releases.createdAt）。
      const seedCreatedAt = "2026-01-01T00:00:00Z";
      expect(await screen.findByText(formatUtcToLocalDate(seedCreatedAt))).toBeTruthy();

      const utcSlice = seedCreatedAt.slice(0, 10);
      const offsetMinutes = -new Date(seedCreatedAt).getTimezoneOffset();
      if (offsetMinutes !== 0 && utcSlice !== formatUtcToLocalDate(seedCreatedAt)) {
        // 非 UTC 宿主下按 UTC 截断会差一天，这条断言专门拦「绕过统一入口」的写法。
        expect(screen.queryByText(utcSlice)).toBeNull();
      }
    } finally {
      window.matchMedia = originalMatchMedia;
    }
  });

  it("仓库内搜索超单页时显示加载进度并支持「加载更多」翻页（搜索截断回归）", async () => {
    const user = userEvent.setup();
    // 以小数据复现分页链路：命中 2 条、单页返回 1 条。
    // 回归点：此前前端传 page_size=200 被后端静默回落 20 条且无翻页入口，
    // 用户永远只能看到前几条；现应可见「已显示 x / 共 y」并提供「加载更多」。
    server.use(
      http.get("*/api/v1/search", ({ request }) => {
        const page = Number(new URL(request.url).searchParams.get("page") ?? "1");
        const item = (path: string) => ({
          repository: "maven-releases",
          path,
          size: 128,
          hash: "0".repeat(64),
          updatedAt: "2026-01-01T00:00:00Z",
        });
        return HttpResponse.json({
          items: page === 1 ? [item("com/first.jar")] : [item("com/second.jar")],
          total: 2,
          facets: {},
        });
      }),
    );
    renderDetail("maven-releases", true);

    const box = await screen.findByPlaceholderText(/搜索制品/);
    await user.type(box, "jar");
    await user.keyboard("{Enter}");

    // 首页：提示已显示 1 / 共 2 条，并提供「加载更多」按钮
    expect(await screen.findByText("已显示 1 / 共 2 条结果")).toBeTruthy();

    await user.click(screen.getByRole("button", { name: "加载更多" }));

    // 翻页后：追加到 2/2、按钮消失、第二页结果进入树
    expect(await screen.findByText("找到 2 条结果")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "加载更多" })).toBeNull();
    expect(screen.getByText("second.jar")).toBeTruthy();
  });

  it("带 highlight 进入时逐级展开并选中命中文件（FR-145）", async () => {
    renderDetail("maven-releases", true, "?highlight=com/example/app/1.0.0/app-1.0.0.jar");
    // 祖先目录逐级懒加载展开后自动选中：右侧详情出现完整路径与使用说明（等同手动点选）
    expect(await screen.findByText("com/example/app/1.0.0/app-1.0.0.jar")).toBeTruthy();
    expect(await screen.findByText("解析依赖（pom.xml）")).toBeTruthy();
  });

  it("highlight 指向不存在路径时静默降级：仓库正常打开、不报错（FR-145）", async () => {
    renderDetail("maven-releases", true, "?highlight=missing/none.jar");
    // 树根层正常渲染、可交互；失效路径不产生错误提示
    expect(await screen.findByText("com")).toBeTruthy();
    expect(screen.queryByText(/none\.jar/)).toBeNull();
  });

  it("匿名浏览公开仓库详情时概览带展示真实统计（回归：此前显示 0 / 0 B）", async () => {
    // 此前匿名走 getRepositoryUsage（只含 format/type/description，无统计），概览带的
    // 「制品数 / 体积」恒为 0；现统一走公开列表 API（带 artifactCount/totalSize）。
    // 种子：npm-proxy artifactCount=5240、totalSize=12884901888（12 GiB）。
    renderDetail("npm-proxy", false);
    expect(await screen.findByText("5,240")).toBeTruthy();
    expect(await screen.findByText(/^12(\.\d+)? GB$/)).toBeTruthy();
  });
});
