// 仓库管理集成测试：渲染种子仓库；匿名（FR-68）仅见 public 仓库且无管理操作。
import { server } from "@jianartifact/devmock/node";
import {
  releaseDevMockPendingRequests,
  waitForDevMockPendingRequest,
} from "@jianartifact/devmock/scenario";
import { http, HttpResponse } from "msw";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it } from "vitest";

import { RepositoriesPage } from "../src/pages/RepositoriesPage";
import { renderWithProviders } from "./harness";

const MEMBER = {
  id: 2,
  username: "developer",
  role: "user" as const,
  status: "active" as const,
  createdAt: "2026-01-02T00:00:00Z",
};

describe("仓库管理", () => {
  afterEach(() => {
    window.history.replaceState({}, "", "/");
  });

  it("渲染种子仓库", async () => {
    renderWithProviders(<RepositoriesPage />, { route: "/repositories", authenticated: true });
    expect(await screen.findByText("maven-releases")).toBeTruthy();
    expect(await screen.findByText("npm-proxy")).toBeTruthy();
    expect(await screen.findByText("raw-hosted")).toBeTruthy();
  });

  it("匿名仅见 public 仓库且无管理操作", async () => {
    renderWithProviders(<RepositoriesPage />, { route: "/repositories", authenticated: false });
    expect(await screen.findByText("npm-proxy")).toBeTruthy();
    // private 仓库不出现在匿名列表。
    expect(screen.queryByText("maven-releases")).toBeNull();
    // 管理操作（新建/删除/清理）对匿名隐藏。
    expect(screen.queryByRole("button", { name: "新建仓库" })).toBeNull();
    expect(screen.queryByLabelText("删除")).toBeNull();
  });

  it("empty URL 场景经真实 MSW 返回仓库空态", async () => {
    window.history.replaceState({}, "", "/repositories?__mock=empty");
    renderWithProviders(<RepositoriesPage />, {
      route: "/repositories?__mock=empty",
      authenticated: true,
    });

    expect(await screen.findByText("暂无仓库")).toBeTruthy();
    expect(screen.queryByText("maven-releases")).toBeNull();
  });

  it("loading 场景由测试显式释放后恢复仓库列表", async () => {
    const route = "/repositories?__mock=loading";
    window.history.replaceState({}, "", route);
    renderWithProviders(<RepositoriesPage />, { route, authenticated: true });

    expect(await screen.findByTestId("state-loading")).toBeTruthy();
    await waitForDevMockPendingRequest();
    expect(releaseDevMockPendingRequests()).toBeGreaterThan(0);
    expect(await screen.findByText("maven-releases")).toBeTruthy();
  });

  it("error 场景显示可重试仓库读取错误", async () => {
    const route = "/repositories?__mock=error";
    window.history.replaceState({}, "", route);
    renderWithProviders(<RepositoriesPage />, { route, authenticated: true });

    expect(await screen.findByTestId("state-error")).toBeTruthy();
    expect(screen.getByRole("button", { name: "重试" })).toBeTruthy();
  });

  it("普通登录用户不显示全局仓库管理写操作", async () => {
    renderWithProviders(<RepositoriesPage />, {
      route: "/repositories",
      authenticated: true,
      user: MEMBER,
      token: "mock.jwt.token:user",
    });

    expect(await screen.findByText("maven-releases")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "新建仓库" })).toBeNull();
    expect(screen.queryByLabelText("删除")).toBeNull();
    // 种子含多个 public 仓库，徽章非交互元素（无 pointer cursor）。
    expect(screen.getAllByText("公开").length).toBeGreaterThan(0);
    screen.getAllByText("公开").forEach((badge) => expect(badge.style.cursor).toBe(""));
  });

  it("选择 proxy 类型后填上游地址并新建", async () => {
    const user = userEvent.setup();
    renderWithProviders(<RepositoriesPage />, { route: "/repositories", authenticated: true });
    await screen.findByText("raw-hosted");

    await user.click(screen.getByRole("button", { name: "新建仓库" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.change(within(dialog).getByLabelText(/名称/), { target: { value: "my-proxy" } });

    // 选择类型为 proxy，触发上游地址输入渲染。
    await user.click(within(dialog).getByLabelText(/类型/));
    await user.click(await screen.findByRole("option", { name: "proxy" }));
    fireEvent.change(within(dialog).getByLabelText(/上游地址/), {
      target: { value: "https://repo.example.com/raw" },
    });

    await user.click(within(dialog).getByRole("button", { name: "新建" }));
    await waitFor(() => expect(screen.getByText("my-proxy")).toBeTruthy());
  });

  it("备用只读拒绝新建仓库时保留已填写内容", async () => {
    const user = userEvent.setup();
    const route = "/repositories?__mock=standby_read_only";
    window.history.replaceState({}, "", route);
    renderWithProviders(<RepositoriesPage />, { route, authenticated: true });
    await screen.findByText("raw-hosted");

    await user.click(screen.getByRole("button", { name: "新建仓库" }));
    const dialog = await screen.findByRole("dialog");
    const name = within(dialog).getByLabelText(/名称/) as HTMLInputElement;
    await user.type(name, "standby-rejected");
    await user.click(within(dialog).getByRole("button", { name: "新建" }));

    expect(await screen.findByText("备用节点为只读，当前请求已拒绝")).toBeTruthy();
    expect(name.value).toBe("standby-rejected");
    expect(screen.queryByText("standby-rejected")).toBeNull();
  });

  it("删除仓库确认后刷新列表，删除失败时保留仓库", async () => {
    const user = userEvent.setup();
    renderWithProviders(<RepositoriesPage />, { route: "/repositories", authenticated: true });
    const rawRow = (await screen.findByText("raw-hosted")).closest("tr")!;

    await user.click(within(rawRow).getByLabelText("删除"));
    await user.click(
      within(await screen.findByRole("dialog")).getByRole("button", { name: "删除" }),
    );

    expect(await screen.findByText("删除成功")).toBeTruthy();
    await waitFor(() => expect(screen.queryByText("raw-hosted")).toBeNull());
  });

  it("删除仓库失败时展示错误并保留原仓库", async () => {
    server.use(
      http.delete("*/api/v1/repositories/:name", () =>
        HttpResponse.json(
          { error: { code: "conflict", message: "仓库删除失败" } },
          { status: 409 },
        ),
      ),
    );
    const user = userEvent.setup();
    renderWithProviders(<RepositoriesPage />, { route: "/repositories", authenticated: true });
    const rawRow = (await screen.findByText("raw-hosted")).closest("tr")!;

    await user.click(within(rawRow).getByLabelText("删除"));
    await user.click(
      within(await screen.findByRole("dialog")).getByRole("button", { name: "删除" }),
    );

    expect(await screen.findByText("仓库删除失败")).toBeTruthy();
    expect(screen.getByText("raw-hosted")).toBeTruthy();
  });

  it("清理确认后展示结果，清理失败时保留仓库", async () => {
    const user = userEvent.setup();
    renderWithProviders(<RepositoriesPage />, { route: "/repositories", authenticated: true });
    const mavenRow = (await screen.findByText("maven-releases")).closest("tr")!;

    await user.click(within(mavenRow).getByLabelText("清理无 Jar 制品"));
    await user.click(
      within(await screen.findByRole("dialog")).getByRole("button", { name: "执行清理" }),
    );

    expect(await screen.findByText("已清理 0 个空制品目录")).toBeTruthy();
    expect(screen.getByText("maven-releases")).toBeTruthy();
  });

  it("清理失败时展示错误并保留仓库", async () => {
    server.use(
      http.post("*/api/v1/repositories/:name/cleanup", () =>
        HttpResponse.json({ error: { code: "conflict", message: "清理失败" } }, { status: 409 }),
      ),
    );
    const user = userEvent.setup();
    renderWithProviders(<RepositoriesPage />, { route: "/repositories", authenticated: true });
    const mavenRow = (await screen.findByText("maven-releases")).closest("tr")!;

    await user.click(within(mavenRow).getByLabelText("清理无 Jar 制品"));
    await user.click(
      within(await screen.findByRole("dialog")).getByRole("button", { name: "执行清理" }),
    );

    expect(await screen.findByText("清理失败")).toBeTruthy();
    expect(screen.getByText("maven-releases")).toBeTruthy();
  });

  it("proxy 仓库行的连接状态收敛为状态图标，文字经无障碍标签可达（FR-114）", async () => {
    renderWithProviders(<RepositoriesPage />, { route: "/repositories", authenticated: true });
    // devmock 种子：npm-proxy / gomod-proxy 上游可达 → 「可用」（多个）。
    // 列表 8 列在 1152 视口只有 ~870px，文字徽章会把类型/可见性列压成「P...」「公.」，
    // 所以状态在列表里只用图标承载，文字走 aria-label（Tooltip 同源文案）。
    expect(await screen.findByText("npm-proxy")).toBeTruthy();
    expect(screen.getAllByLabelText("连接状态：可用").length).toBeGreaterThan(0);
    // hosted 仓库不返回 connectionStatus → 占位符「—」。
    expect(screen.getAllByText("—").length).toBeGreaterThan(0);
  });

  it("置顶仓库排到列表首位，可取消置顶", async () => {
    const user = userEvent.setup();
    // 清理本地置顶偏好，保证种子顺序可预期。
    localStorage.removeItem("jianartifact.pinnedRepos");
    renderWithProviders(<RepositoriesPage />, { route: "/repositories", authenticated: true });
    await screen.findByText("maven-releases");

    // 种子顺序首位是 maven-releases；置顶 npm-proxy 后应排到第一行。
    const npmRow = screen.getByText("npm-proxy").closest("tr")!;
    await user.click(within(npmRow).getByLabelText("置顶"));

    const rows = screen.getAllByRole("row");
    expect(within(rows[1]).getByText("npm-proxy")).toBeTruthy();
    expect(within(rows[1]).getByLabelText("取消置顶")).toBeTruthy();
    // 本地偏好已持久化（按浏览器记忆）。
    expect(JSON.parse(localStorage.getItem("jianartifact.pinnedRepos") ?? "[]")).toContain(
      "npm-proxy",
    );

    // 取消置顶后恢复服务端排序（name asc → docker-hub 首位）。
    await user.click(within(rows[1]).getByLabelText("取消置顶"));
    await waitFor(() =>
      expect(within(screen.getAllByRole("row")[1]).queryByText("npm-proxy")).toBeNull(),
    );
    expect(localStorage.getItem("jianartifact.pinnedRepos")).toBe("[]");
  });

  it("表头点击排序可切换升降序并持久化", async () => {
    const user = userEvent.setup();
    localStorage.removeItem("jianartifact.repoView");
    renderWithProviders(<RepositoriesPage />, { route: "/repositories", authenticated: true });
    await screen.findByText("npm-proxy");

    const headerButton = () =>
      within(screen.getAllByRole("row")[0]).getByRole("button", { name: "名称" });
    // 默认 sortBy 已是 name asc → 首次点击翻转为降序：raw-hosted 首位
    await user.click(headerButton());
    await waitFor(() =>
      expect(within(screen.getAllByRole("row")[1]).getByText("raw-hosted")).toBeTruthy(),
    );

    // 同列再点 → 升序：docker-hub 首位
    await user.click(headerButton());
    await waitFor(() =>
      expect(within(screen.getAllByRole("row")[1]).getByText("docker-hub")).toBeTruthy(),
    );

    // 视图偏好已持久化
    const view = JSON.parse(localStorage.getItem("jianartifact.repoView") ?? "{}");
    expect(view.sortBy).toBe("name");
    expect(view.sortOrder).toBe("asc");
  });

  it("名称筛选只显示匹配仓库并持久化筛选词", async () => {
    const user = userEvent.setup();
    localStorage.removeItem("jianartifact.repoView");
    renderWithProviders(<RepositoriesPage />, { route: "/repositories", authenticated: true });
    await screen.findByText("npm-proxy");

    const filterInput = screen.getByPlaceholderText("按名称筛选");
    await user.type(filterInput, "npm");

    // 列表只剩 npm-proxy，其余被过滤。
    await waitFor(() => expect(screen.queryByText("maven-releases")).toBeNull());
    expect(screen.getByText("npm-proxy")).toBeTruthy();
    expect(JSON.parse(localStorage.getItem("jianartifact.repoView") ?? "{}").nameFilter).toBe(
      "npm",
    );
  });

  it("新建仓库只展示当前进程启用的格式", async () => {
    server.use(http.get("*/api/v1/formats/enabled", () => HttpResponse.json({ formats: ["raw"] })));
    const user = userEvent.setup();
    renderWithProviders(<RepositoriesPage />, { route: "/repositories", authenticated: true });
    await screen.findByText("raw-hosted");

    await user.click(screen.getByRole("button", { name: "新建仓库" }));
    const dialog = await screen.findByRole("dialog");
    await waitFor(() =>
      expect((within(dialog).getByLabelText("格式") as HTMLInputElement).value).toBe("raw"),
    );
    await user.click(within(dialog).getByLabelText("格式"));
    expect(await screen.findByRole("option", { name: "raw" })).toBeTruthy();
    expect(screen.queryByRole("option", { name: "maven" })).toBeNull();
    expect(screen.queryByRole("option", { name: "npm" })).toBeNull();
  });
});
