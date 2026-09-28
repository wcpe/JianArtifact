// 仓库详情集成测试：目录树逐级展开点选文件后渲染详情/使用说明；匿名访问 private 仓库报未认证。
// FR-74：整页固定布局（面板内滚）、未登录仅页眉一个登录入口、客户端发布提示收纳为紧凑小字。
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { describe, expect, it, vi } from "vitest";
import { Route, Routes, useLocation, useParams } from "react-router-dom";

import { store } from "@jianartifact/devmock";
import { server } from "@jianartifact/devmock/node";
import {
  releaseDevMockPendingRequests,
  waitForDevMockPendingRequest,
} from "@jianartifact/devmock/scenario";
import { AppRoutes } from "../src/app/router";
import { RepositoryDetailPage } from "../src/pages/RepositoryDetailPage";
import { formatCount } from "../src/lib/format";
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

/** 在详情页旁挂一个路由参数探针：重命名成功后用于断言已导航到新名。 */
function DetailWithRouteName() {
  const { name } = useParams();
  return (
    <>
      <div data-testid="route-name">{name}</div>
      <RepositoryDetailPage />
    </>
  );
}

function DetailLocationProbe() {
  const location = useLocation();
  return <div data-testid="detail-location">{location.search}</div>;
}

/** 非管理员会话快照（role=user）：配置页签与重命名入口都应对其隐藏。 */
const MEMBER_USER = {
  id: 2,
  username: "developer",
  role: "user" as const,
  status: "active" as const,
  createdAt: "2026-01-02T00:00:00Z",
};

/**
 * 把 window.matchMedia 临时桩成窄屏：`max-width: 48em` 查询命中（RepoBrowser 的 isNarrow）。
 * 测试替身默认所有媒体查询都不命中（等价桌面），故既有桌面用例不受影响；
 * 返回的 spy 必须在用例结束前还原（try/finally），否则会泄漏到同文件后续用例。
 */
function mockNarrowViewport() {
  return vi.spyOn(window, "matchMedia").mockImplementation(
    (query: string) =>
      ({
        matches: query.includes("max-width: 48em"),
        media: query,
        onchange: null,
        addListener: () => {},
        removeListener: () => {},
        addEventListener: () => {},
        removeEventListener: () => {},
        dispatchEvent: () => false,
      }) as MediaQueryList,
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

  it("受控页签写入 tab 查询参数并保留 highlight 等其他参数", async () => {
    const user = userEvent.setup();
    renderWithProviders(
      <Routes>
        <Route
          path="/repositories/:name"
          element={
            <>
              <DetailLocationProbe />
              <RepositoryDetailPage />
            </>
          }
        />
      </Routes>,
      {
        route: "/repositories/maven-releases?highlight=com/example&from=search",
        authenticated: true,
      },
    );

    expect((await screen.findByRole("tab", { name: "浏览" })).getAttribute("aria-selected")).toBe(
      "true",
    );
    await user.click(await screen.findByRole("tab", { name: "配置" }));
    expect(screen.getByTestId("detail-location").textContent).toBe(
      "?highlight=com%2Fexample&from=search&tab=config",
    );

    await user.click(await screen.findByRole("tab", { name: "ACL" }));
    expect(screen.getByTestId("detail-location").textContent).toBe(
      "?highlight=com%2Fexample&from=search&tab=acl",
    );
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

  it("通过仓库别名进入详情时仍展示页头属性并保留管理能力", async () => {
    store.updateRepository("maven-releases", { aliases: ["maven-legacy"] });
    const user = userEvent.setup();
    renderDetail("maven-legacy", true);

    // 别名只用于定位仓库，接口请求仍沿用 URL 中的名称；页头属性不能因定位失败而缺失。
    expect(await screen.findByText("maven")).toBeTruthy();
    expect(await screen.findByText("hosted")).toBeTruthy();

    await user.click(await screen.findByRole("tab", { name: "配置" }));
    expect(await screen.findByRole("button", { name: "重命名" })).toBeTruthy();
  });

  it("总下载次数留在页头概览，下载趋势移入右侧详情卡片顶部（阶段 D-1）", async () => {
    // 端点 B（非契约 /repositories/:name/download-trend）未在 devmock 注册，
    // 按既有模式在测试内 server.use 覆盖最小合法响应（全时段累计 + 补零趋势点）。
    server.use(
      http.get("*/api/v1/repositories/:name/download-trend", () =>
        HttpResponse.json({
          from: "2026-09-01T00:00:00.000Z",
          to: "2026-09-02T00:00:00.000Z",
          effectiveBucket: "hour",
          totalDownloadCount: 12345,
          trend: [
            {
              from: "2026-09-01T00:00:00.000Z",
              to: "2026-09-01T01:00:00.000Z",
              downloadCount: 3,
            },
            {
              from: "2026-09-01T01:00:00.000Z",
              to: "2026-09-01T02:00:00.000Z",
              downloadCount: 5,
            },
          ],
        }),
      ),
    );
    renderDetail("maven-releases", true);

    // 页头统计：label「总下载次数」与 formatCount(totalDownloadCount) 的数值同组出现（保留不动）。
    const statLabel = await screen.findByText("总下载次数");
    const statGroup = statLabel.parentElement;
    expect(statGroup).not.toBeNull();
    expect(within(statGroup as HTMLElement).getByText(formatCount(12345))).toBeTruthy();

    // 趋势区落在右侧详情卡片内部（与文件树同级的两栏右栏），而不是页头与页签之间的页面级整行。
    const detailPanel = await screen.findByTestId("repo-detail-panel");
    const trend = await screen.findByTestId("repo-detail-trend");
    expect(detailPanel.contains(trend)).toBe(true);
    // 整行区块已移除：下载趋势只存在于浏览 Tab 面板内（页面顶层的 Tabs 直系子节点里不再有它）。
    expect(trend.closest('[role="tabpanel"]')).not.toBeNull();
    expect(screen.getAllByTestId("repo-detail-trend")).toHaveLength(1);

    // 默认展开：不再有折叠控件，无需任何点击即直接渲染图表，且图表就在右栏卡片内。
    expect(screen.queryByTestId("repo-detail-trend-toggle")).toBeNull();
    const chart = await screen.findByRole("img", { name: "下载趋势（近 24 小时）：下载" });
    expect(trend.contains(chart)).toBe(true);

    // 趋势与下方内容同处一个滚动区（不再钉在固定列首）：往下滚时它会向上滚出视野，
    // 因此必须位于 ScrollArea 的 viewport 内，而不是它的兄弟节点。
    expect(trend.closest(".mantine-ScrollArea-viewport")).not.toBeNull();
  });

  it("下载趋势默认直接渲染图表（不再折叠）且仍在右栏卡片内", async () => {
    server.use(
      http.get("*/api/v1/repositories/:name/download-trend", () =>
        HttpResponse.json({
          from: "2026-09-01T00:00:00.000Z",
          to: "2026-09-02T00:00:00.000Z",
          effectiveBucket: "hour",
          totalDownloadCount: 8888,
          trend: [
            {
              from: "2026-09-01T00:00:00.000Z",
              to: "2026-09-01T01:00:00.000Z",
              downloadCount: 2,
            },
          ],
        }),
      ),
    );
    renderDetail("maven-releases", true);

    // 无折叠控件：默认即渲染图表（不经任何点击），且仍落在右栏详情卡片内（没有跑到页面顶层）。
    expect(await screen.findByTestId("repo-detail-trend")).toBeTruthy();
    expect(screen.queryByTestId("repo-detail-trend-toggle")).toBeNull();
    const img = await screen.findByRole("img", { name: "下载趋势（近 24 小时）：下载" });
    expect(screen.getByTestId("repo-detail-panel").contains(img)).toBe(true);
    expect(screen.getByTestId("repo-detail-trend").closest('[role="tabpanel"]')).not.toBeNull();
  });

  it("下载趋势属于仓库级信息：未选中文件时也常驻右栏顶部（阶段 D-1）", async () => {
    // 未选中文件时右栏渲染的是 UsagePanel；下载趋势不随选中态出现/消失。
    server.use(
      http.get("*/api/v1/repositories/:name/download-trend", () =>
        HttpResponse.json({
          from: "2026-09-01T00:00:00.000Z",
          to: "2026-09-02T00:00:00.000Z",
          effectiveBucket: "hour",
          totalDownloadCount: 7,
          trend: [
            {
              from: "2026-09-01T00:00:00.000Z",
              to: "2026-09-01T01:00:00.000Z",
              downloadCount: 7,
            },
          ],
        }),
      ),
    );
    renderDetail("maven-releases", true);

    // 未选中任何文件：右栏是使用说明，趋势图依旧在右栏卡片内。
    expect(await screen.findByText("使用说明")).toBeTruthy();
    const detailPanel = await screen.findByTestId("repo-detail-panel");
    const trend = await screen.findByTestId("repo-detail-trend");
    expect(detailPanel.contains(trend)).toBe(true);
    expect(await screen.findByText("下载趋势（近 24 小时）")).toBeTruthy();
  });

  it("配置页签展示别名并支持增删，保存时随 updateRepository 一并提交（阶段 D-2）", async () => {
    // 种子仓库主名 maven-releases，预置别名 maven-legacy 以验证「展示 + 移除」。
    store.updateRepository("maven-releases", { aliases: ["maven-legacy"] });
    let patchBody: { aliases?: string[] } | null = null;
    server.use(
      http.patch("*/api/v1/repositories/:name", async ({ request }) => {
        patchBody = (await request.json()) as { aliases?: string[] };
        return HttpResponse.json({
          id: 1,
          name: "maven-releases",
          format: "maven",
          type: "hosted",
          visibility: "private",
          createdAt: "2026-01-01T00:00:00Z",
          aliases: patchBody.aliases ?? [],
        });
      }),
    );
    const user = userEvent.setup();
    renderDetail("maven-releases", true);

    await user.click(await screen.findByRole("tab", { name: "配置" }));

    // 已有别名以标签形式展示。
    const legacyLabel = await screen.findByText("maven-legacy");
    expect(legacyLabel).toBeTruthy();

    // 新增一个别名（输入后回车）。
    const aliasInput = screen.getByPlaceholderText("添加别名");
    await user.type(aliasInput, "maven-old{Enter}");
    expect(await screen.findByText("maven-old")).toBeTruthy();

    // 移除原别名：点掉标签上的删除按钮。
    const pillRoot = legacyLabel.parentElement as HTMLElement;
    await user.click(pillRoot.querySelector("button") as HTMLButtonElement);
    await waitFor(() => expect(screen.queryByText("maven-legacy")).toBeNull());

    await user.click(screen.getByRole("button", { name: "保存配置" }));
    expect(await screen.findByText("保存成功")).toBeTruthy();
    await waitFor(() => expect(patchBody?.aliases).toEqual(["maven-old"]));
  });

  it("别名等于仓库主名时前端拦截并给出可读提示（阶段 D-2）", async () => {
    const user = userEvent.setup();
    renderDetail("maven-releases", true);
    await user.click(await screen.findByRole("tab", { name: "配置" }));

    const aliasInput = await screen.findByPlaceholderText("添加别名");
    await user.type(aliasInput, "maven-releases{Enter}");

    // 内联错误提示出现（不与主名相同）。
    expect((await screen.findAllByText("别名不能与仓库主名相同")).length).toBeGreaterThan(0);
  });

  it("重命名成功后提示并导航到新名仓库详情（阶段 D-2）", async () => {
    const user = userEvent.setup();
    renderWithProviders(
      <Routes>
        <Route path="/repositories/:name" element={<DetailWithRouteName />} />
      </Routes>,
      { route: "/repositories/maven-releases", authenticated: true },
    );

    await user.click(await screen.findByRole("tab", { name: "配置" }));
    await user.click(await screen.findByRole("button", { name: "重命名" }));

    const dialog = await screen.findByRole("dialog");
    const input = within(dialog).getByLabelText("新名称") as HTMLInputElement;
    await user.clear(input);
    await user.type(input, "maven-renamed");
    await user.click(within(dialog).getByRole("button", { name: "确认重命名" }));

    // 成功提示 + 路由参数已切到新名（旧名失效，不导航会 404）。
    expect(await screen.findByText("已重命名")).toBeTruthy();
    await waitFor(() => expect(screen.getByTestId("route-name").textContent).toBe("maven-renamed"));
  });

  it("重命名冲突（409）展示名称被占用且不跳转（阶段 D-2）", async () => {
    server.use(
      http.post("*/api/v1/repositories/:name/rename", () =>
        HttpResponse.json(
          { error: { code: "conflict", message: "名称已被占用" } },
          { status: 409 },
        ),
      ),
    );
    const user = userEvent.setup();
    renderWithProviders(
      <Routes>
        <Route path="/repositories/:name" element={<DetailWithRouteName />} />
      </Routes>,
      { route: "/repositories/maven-releases", authenticated: true },
    );

    await user.click(await screen.findByRole("tab", { name: "配置" }));
    await user.click(await screen.findByRole("button", { name: "重命名" }));

    const dialog = await screen.findByRole("dialog");
    const input = within(dialog).getByLabelText("新名称") as HTMLInputElement;
    await user.clear(input);
    await user.type(input, "npm-proxy");
    await user.click(within(dialog).getByRole("button", { name: "确认重命名" }));

    expect(await screen.findByText("该名称已被占用")).toBeTruthy();
    // 未成功 → 仍停留在原仓库。
    expect(screen.getByTestId("route-name").textContent).toBe("maven-releases");
  });

  it("使用说明三组全展开、顶部只有一个工具切换器（阶段 3.2）", async () => {
    renderDetail("maven-releases", true);

    // 未选中文件 → 右栏是使用说明；标题保留在面板顶部。
    expect(await screen.findByText("使用说明")).toBeTruthy();

    // 三个分组按固定顺序一次看全（认证 → 解析依赖 → 发布制品）；other 为空集 → 不出现。
    // 不再折叠：它们本就是配置一个仓库时要一起做的事。
    const groupIds = (await screen.findAllByTestId(/^repo-usage-group-/)).map((node) =>
      node.getAttribute("data-testid"),
    );
    expect(groupIds).toEqual([
      "repo-usage-group-auth",
      "repo-usage-group-resolve",
      "repo-usage-group-publish",
    ]);
    expect(await screen.findByText(/<server>/)).toBeTruthy();
    expect(screen.getByText("发布制品（pom.xml + mvn deploy）")).toBeTruthy();

    // 工具切换器只有一个（不再每组一个），默认选中第一个工具 Maven；选项只显示工具名。
    const selects = screen.getAllByTestId("repo-usage-tool-select");
    expect(selects.length).toBe(1);
    expect((selects[0] as HTMLInputElement).value).toBe("Maven");
  });

  it("顶部切换工具：三组同时按该工具筛选，无片段的组整组隐藏（阶段 3.2）", async () => {
    const user = userEvent.setup();
    renderDetail("maven-releases", true);

    // 选项是跨分组的工具并集，只显示工具名（不带片段数）。
    await user.click(await screen.findByTestId("repo-usage-tool-select"));
    const optionNames = (await screen.findAllByRole("option")).map((node) => node.textContent);
    expect(optionNames).toEqual([
      "Maven",
      "Gradle（Groovy）",
      "Gradle（Kotlin DSL）",
      "sbt",
      "Ivy",
      "Ant",
    ]);

    // 切到 Gradle（Groovy DSL）：一次切换，三组同时换成 Gradle 写法。
    await user.click(await screen.findByRole("option", { name: "Gradle（Groovy）" }));
    expect(await screen.findByText("认证（~/.gradle/gradle.properties）")).toBeTruthy();
    expect(screen.getByText("解析依赖（Gradle）")).toBeTruthy();
    expect(screen.getByText("发布制品（Gradle）")).toBeTruthy();
    expect(screen.queryByText(/<server>/)).toBeNull();

    // 切到 sbt：只有「解析依赖」有该工具，另两组整组隐藏（而不是留空标题）。
    await user.click(screen.getByTestId("repo-usage-tool-select"));
    await user.click(await screen.findByRole("option", { name: "sbt" }));
    expect(await screen.findByText("解析依赖（sbt）")).toBeTruthy();
    expect(screen.queryByTestId("repo-usage-group-auth")).toBeNull();
    expect(screen.queryByTestId("repo-usage-group-publish")).toBeNull();

    // 代码块右上角的复制按钮仍在；点击走复制链路且不抛错。
    const copies = screen.getAllByRole("button", { name: "复制" });
    expect(copies.length).toBeGreaterThan(0);
    await user.click(copies[0]!);
  });

  it("raw 仓库：切到 wget 后只剩「解析依赖」（发布制品只有 curl）（阶段 3.2）", async () => {
    const user = userEvent.setup();
    renderDetail("raw-hosted", true);

    // 默认 curl：解析与发布两组都在。
    expect(await screen.findByTestId("repo-usage-group-resolve")).toBeTruthy();
    expect(screen.getByTestId("repo-usage-group-publish")).toBeTruthy();

    await user.click(await screen.findByTestId("repo-usage-tool-select"));
    await user.click(await screen.findByRole("option", { name: "wget" }));

    expect(await screen.findByText("下载制品（wget）")).toBeTruthy();
    expect(screen.queryByTestId("repo-usage-group-publish")).toBeNull();
  });

  it("使用说明防御：非法 group 归入「其他」、缺失 tool 仍显示片段（阶段 3.2）", async () => {
    server.use(
      http.get("*/api/v1/repositories/:name/usage", () =>
        HttpResponse.json({
          format: "raw",
          type: "hosted",
          snippets: [
            { title: "无分组片段", code: "echo no-group" },
            { title: "非法分组片段", code: "echo bad-group", group: "legacy" },
          ],
        }),
      ),
    );
    renderDetail("maven-releases", true);

    expect(await screen.findByText("使用说明")).toBeTruthy();
    // auth / resolve / publish 均为空 → 只剩「其他」一组，且已展开（无需点击）。
    expect(await screen.findByTestId("repo-usage-group-other")).toBeTruthy();

    // 两段都缺失合法 tool → 同属「未知工具」一档；只有一种工具 → 不渲染切换器，片段仍全部显示。
    expect(await screen.findByText("无分组片段")).toBeTruthy();
    expect(screen.getByText("非法分组片段")).toBeTruthy();
    expect(screen.queryByTestId("repo-usage-tool-select")).toBeNull();
  });

  it("非管理员看不到配置页签与重命名入口（阶段 D-2）", async () => {
    renderWithProviders(
      <Routes>
        <Route path="/repositories/:name" element={<RepositoryDetailPage />} />
      </Routes>,
      {
        route: "/repositories/maven-releases",
        authenticated: true,
        user: MEMBER_USER,
        token: "mock.jwt.token:user",
      },
    );

    await screen.findByText("com");
    expect(screen.queryByRole("tab", { name: "配置" })).toBeNull();
    expect(screen.queryByRole("button", { name: "重命名" })).toBeNull();
  });

  describe("窄屏浏览（FR-74 后续：树占满高度、详情改底部抽屉）", () => {
    it("窄屏点选文件后弹出抽屉，抽屉内可见制品路径与下载入口，桌面详情卡不再渲染", async () => {
      const user = userEvent.setup();
      const spy = mockNarrowViewport();
      try {
        renderDetail("maven-releases", true);
        await user.click(await screen.findByText("com"));
        await user.click(await screen.findByText("example"));
        await user.click(await screen.findByText("app"));
        await user.click(await screen.findByText("1.0.0"));
        await user.click(await screen.findByText("app-1.0.0.jar"));

        const drawer = await screen.findByRole("dialog");
        expect(within(drawer).getByText("com/example/app/1.0.0/app-1.0.0.jar")).toBeTruthy();
        expect(within(drawer).getByRole("link", { name: "下载" })).toBeTruthy();
        // 窄屏不再渲染桌面详情卡：详情空间全部让给抽屉，卡片不占布局高度。
        expect(screen.queryByTestId("repo-detail-panel")).toBeNull();
      } finally {
        spy.mockRestore();
      }
    });

    it("窄屏关闭抽屉后回到文件树：抽屉消失、树保持展开状态", async () => {
      const user = userEvent.setup();
      const spy = mockNarrowViewport();
      try {
        renderDetail("maven-releases", true);
        await user.click(await screen.findByText("com"));
        await user.click(await screen.findByText("example"));
        await user.click(await screen.findByText("app"));
        await user.click(await screen.findByText("1.0.0"));
        await user.click(await screen.findByText("app-1.0.0.jar"));

        const drawer = await screen.findByRole("dialog");
        await user.click(within(drawer).getByRole("button", { name: "关闭" }));

        await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
        // 树仍在且保持原有展开状态（祖先目录与命中文件不需要重新展开）。
        expect(screen.getByRole("tree")).toBeTruthy();
        expect(screen.getByText("app-1.0.0.jar")).toBeTruthy();
      } finally {
        spy.mockRestore();
      }
    });

    it("窄屏文件树占满可用高度：树卡不再有 45vh 上限", async () => {
      const spy = mockNarrowViewport();
      try {
        renderDetail("maven-releases", true);
        const tree = await screen.findByRole("tree");
        // 树卡即文件树最近的 Card 根节点（ScrollArea 不是 Card）。
        const treeCard = tree.closest(".mantine-Card-root") as HTMLElement | null;
        expect(treeCard).not.toBeNull();
        expect((treeCard as HTMLElement).style.maxHeight).toBe("");
        // 改为参与拉伸（flex: 1 → 1 1 0%），不再按内容自适应。
        expect((treeCard as HTMLElement).style.flex).toBe("1 1 0%");
      } finally {
        spy.mockRestore();
      }
    });

    it("窄屏未选中文件时「使用说明」入口可见，点击在同一抽屉内渲染使用说明", async () => {
      const user = userEvent.setup();
      const spy = mockNarrowViewport();
      try {
        renderDetail("maven-releases", true);
        const entry = await screen.findByTestId("repo-usage-entry");
        expect(entry.textContent).toBe("使用说明");

        await user.click(entry);
        const drawer = await screen.findByRole("dialog");
        expect(within(drawer).getByTestId("repo-usage-panel")).toBeTruthy();
        expect(within(drawer).getByText("解析依赖（pom.xml）")).toBeTruthy();
      } finally {
        spy.mockRestore();
      }
    });
  });
});
