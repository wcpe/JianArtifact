// 账户与服务页开发态场景：所有状态均通过 URL 的 __mock 参数驱动真实 MSW 请求。
import { emptyStore } from "@jianartifact/devmock";
import { server } from "@jianartifact/devmock/node";
import {
  releaseDevMockPendingRequests,
  waitForDevMockPendingRequest,
} from "@jianartifact/devmock/scenario";
import { cleanup, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { afterEach, describe, expect, it } from "vitest";

import { SettingsPage } from "../src/pages/SettingsPage";
import { SetupPage } from "../src/pages/SetupPage";
import { TokensPage } from "../src/pages/TokensPage";
import { UsersPage } from "../src/pages/UsersPage";
import { renderWithProviders } from "./harness";

function setBrowserRoute(route: string) {
  window.history.replaceState({}, "", route);
}

afterEach(() => {
  cleanup();
  setBrowserRoute("/");
});

describe("账户与服务页开发态场景", () => {
  it("用户、令牌和设置页在正常场景展示可操作数据", async () => {
    renderWithProviders(<UsersPage />, { route: "/users", authenticated: true });
    expect(await screen.findByText("admin")).toBeTruthy();
    cleanup();

    renderWithProviders(<TokensPage />, { route: "/tokens", authenticated: true });
    expect(await screen.findByText("ci")).toBeTruthy();
    cleanup();

    renderWithProviders(<SettingsPage />, { route: "/settings", authenticated: true });
    expect(await screen.findByDisplayValue("https://repo.example.com")).toBeTruthy();
  });

  it("用户页在 empty 场景展示空态", async () => {
    setBrowserRoute("/users?__mock=empty");
    renderWithProviders(<UsersPage />, { route: "/users?__mock=empty", authenticated: true });

    expect(await screen.findByTestId("state-empty")).toBeTruthy();
  });

  it("令牌页在 empty 场景展示空态", async () => {
    setBrowserRoute("/tokens?__mock=empty");
    renderWithProviders(<TokensPage />, { route: "/tokens?__mock=empty", authenticated: true });

    expect(await screen.findByTestId("state-empty")).toBeTruthy();
  });

  it("设置页在 empty 场景明确说明尚未配置对外地址", async () => {
    setBrowserRoute("/settings?__mock=empty");
    renderWithProviders(<SettingsPage />, { route: "/settings?__mock=empty", authenticated: true });

    expect(await screen.findByTestId("settings-empty")).toBeTruthy();
  });

  it("初始化页在 empty 场景展示首次引导", async () => {
    setBrowserRoute("/setup?__mock=empty");
    renderWithProviders(<SetupPage />, { route: "/setup?__mock=empty" });

    expect(await screen.findByText("欢迎使用 JianArtifact")).toBeTruthy();
  });

  it.each([
    ["用户", "/users", <UsersPage />],
    ["令牌", "/tokens", <TokensPage />],
    ["设置", "/settings", <SettingsPage />],
    ["初始化", "/setup", <SetupPage />],
  ])("%s 页在 loading 场景持续展示加载态", async (_name, route, page) => {
    setBrowserRoute(`${route}?__mock=loading`);
    renderWithProviders(page, {
      route: `${route}?__mock=loading`,
      authenticated: route !== "/setup",
    });

    expect(await screen.findByTestId("state-loading")).toBeTruthy();
    await waitForDevMockPendingRequest();
    expect(releaseDevMockPendingRequests()).toBeGreaterThan(0);
  });

  it.each([
    ["用户", "/users", <UsersPage />],
    ["令牌", "/tokens", <TokensPage />],
    ["设置", "/settings", <SettingsPage />],
    ["初始化", "/setup", <SetupPage />],
  ])("%s 页在 error 场景展示可重试失败态", async (_name, route, page) => {
    setBrowserRoute(`${route}?__mock=error`);
    renderWithProviders(page, {
      route: `${route}?__mock=error`,
      authenticated: route !== "/setup",
    });

    expect(await screen.findByTestId("state-error")).toBeTruthy();
  });

  it.each([
    ["用户", "users", <UsersPage />, true],
    ["令牌", "tokens", <TokensPage />, true],
    ["设置", "settings", <SettingsPage />, true],
    ["初始化", "status", <SetupPage />, false],
  ])("%s 页在 403 时展示越权态", async (_name, endpoint, page, authenticated) => {
    server.use(
      http.get(`*/api/v1/${endpoint}`, () =>
        HttpResponse.json({ error: { code: "forbidden", message: "没有权限" } }, { status: 403 }),
      ),
    );
    renderWithProviders(page, { route: `/${endpoint}`, authenticated });

    expect(await screen.findByTestId("state-forbidden")).toBeTruthy();
  });

  it("备用节点拒绝新建用户时保留已输入表单并反馈失败", async () => {
    setBrowserRoute("/users?__mock=standby_read_only");
    const user = userEvent.setup();
    renderWithProviders(<UsersPage />, {
      route: "/users?__mock=standby_read_only",
      authenticated: true,
    });
    await screen.findByText("admin");

    await user.click(screen.getByRole("button", { name: "新建用户" }));
    const dialog = await screen.findByRole("dialog");
    const username = within(dialog).getByLabelText(/^用户名/);
    await user.type(username, "standby-user");
    await user.type(within(dialog).getByLabelText(/^口令/), "password1");
    await user.click(within(dialog).getByRole("button", { name: "新建" }));

    expect(await screen.findByText("备用节点为只读，当前请求已拒绝")).toBeTruthy();
    expect((username as HTMLInputElement).value).toBe("standby-user");
  });

  it("令牌创建仅展示一次明文，关闭后列表不再回显", async () => {
    const user = userEvent.setup();
    renderWithProviders(<TokensPage />, { route: "/tokens", authenticated: true });
    await screen.findByText("ci");

    await user.click(screen.getByRole("button", { name: "新建令牌" }));
    const dialog = await screen.findByRole("dialog");
    await user.type(within(dialog).getByLabelText(/^名称/), "release-bot");
    await user.click(within(dialog).getByRole("button", { name: "新建" }));

    const plaintext = await screen.findByText((text) => text.startsWith("jat_"));
    const token = plaintext.textContent ?? "";
    await user.click(screen.getByRole("button", { name: "关闭" }));

    await waitFor(() => expect(screen.queryByText(token)).toBeNull());
    expect(await screen.findByText("release-bot")).toBeTruthy();
  });

  it("备用节点拒绝创建令牌时保留输入且不展示明文", async () => {
    setBrowserRoute("/tokens?__mock=standby_read_only");
    const user = userEvent.setup();
    renderWithProviders(<TokensPage />, {
      route: "/tokens?__mock=standby_read_only",
      authenticated: true,
    });
    await screen.findByText("ci");

    await user.click(screen.getByRole("button", { name: "新建令牌" }));
    const dialog = await screen.findByRole("dialog");
    const name = within(dialog).getByLabelText(/^名称/);
    await user.type(name, "blocked-token");
    await user.click(within(dialog).getByRole("button", { name: "新建" }));

    expect(await screen.findByText("备用节点为只读，当前请求已拒绝")).toBeTruthy();
    expect((name as HTMLInputElement).value).toBe("blocked-token");
    expect(screen.queryByText((text) => text.startsWith("jat_"))).toBeNull();
  });

  it("设置保存成功后回显新值，备用节点拒绝时保留编辑值", async () => {
    const user = userEvent.setup();
    renderWithProviders(<SettingsPage />, { route: "/settings", authenticated: true });
    const publicUrl = (await screen.findByLabelText("对外基础 URL")) as HTMLInputElement;
    await user.clear(publicUrl);
    await user.type(publicUrl, "https://settings.example.com");
    await user.click(screen.getByRole("button", { name: "保存设置" }));
    expect(await screen.findByText("保存成功")).toBeTruthy();
    expect(publicUrl.value).toBe("https://settings.example.com");

    cleanup();
    setBrowserRoute("/settings?__mock=standby_read_only");
    renderWithProviders(<SettingsPage />, {
      route: "/settings?__mock=standby_read_only",
      authenticated: true,
    });
    const standbyUrl = (await screen.findByLabelText("对外基础 URL")) as HTMLInputElement;
    await user.clear(standbyUrl);
    await user.type(standbyUrl, "https://keep.example.com");
    await user.click(screen.getByRole("button", { name: "保存设置" }));
    expect(await screen.findByText("备用节点为只读，当前请求已拒绝")).toBeTruthy();
    expect(standbyUrl.value).toBe("https://keep.example.com");
  });

  it("初始化失败不显示后端错误内的口令内容", async () => {
    emptyStore();
    server.use(
      http.get("*/api/v1/status", () =>
        HttpResponse.json({
          version: "",
          ready: true,
          initialized: false,
          migrationVersion: "",
          userCount: 0,
          bootstrapAllowed: true,
        }),
      ),
      http.post("*/api/v1/auth/bootstrap", () =>
        HttpResponse.json(
          { error: { code: "bootstrap_failed", message: "口令 Password1 不应回显" } },
          { status: 500 },
        ),
      ),
    );
    const user = userEvent.setup();
    renderWithProviders(<SetupPage />, { route: "/setup" });
    await user.click(await screen.findByRole("button", { name: "开始初始化" }));
    await user.type(screen.getByLabelText(/^用户名/), "root");
    await user.type(screen.getByLabelText(/^口令/), "Password1");
    await user.type(screen.getByLabelText(/^确认口令/), "Password1");
    await user.click(screen.getByRole("button", { name: "创建并进入" }));

    expect(await screen.findByText("创建管理员失败，请检查后重试。")).toBeTruthy();
    expect(screen.queryByText("口令 Password1 不应回显")).toBeNull();
  });
});
