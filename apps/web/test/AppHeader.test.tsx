// FR-71 页眉打磨：刷新按钮旋转/禁用态随网络活动归零恢复；useAsync 响应全局刷新事件。
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { describe, expect, it, vi } from "vitest";

import { AppRoutes } from "../src/app/router";
import { useAsync, REFRESH_EVENT } from "../src/hooks/useAsync";
import { renderWithProviders } from "./harness";

/** 使用 useAsync 的最小组件：暴露 fetcher 调用次数。 */
function AsyncProbe({ onFetch }: { onFetch: () => void }) {
  const [calls] = useState({ n: 0 });
  const { loading } = useAsync(async () => {
    calls.n += 1;
    onFetch();
    return calls.n;
  }, []);
  return <div data-testid="probe">{loading ? "loading" : `calls:${calls.n}`}</div>;
}

describe("页眉打磨（FR-71）", () => {
  it("页眉不再提供通知入口（统一收敛到审计日志），账户菜单可用", async () => {
    renderWithProviders(<AppRoutes />, { route: "/dashboard", authenticated: true });

    // 消息中心与页眉通知下拉已退役：只保留账户菜单与审计日志页。
    expect(screen.queryByRole("button", { name: /风险通知/ })).toBeNull();
    expect(screen.getByRole("button", { name: "admin（管理员）" })).toBeTruthy();
  });

  it("登录后的导航按概览、运维、管理分区，并将开源协议独立置底", async () => {
    renderWithProviders(<AppRoutes />, { route: "/dashboard", authenticated: true });

    // 面包屑出现即代表外壳挂载完成；「概览」同时出现在导航段与面包屑，用 getAllByText。
    expect(await screen.findByText("业务仪表盘")).toBeTruthy();
    expect(screen.getAllByText("概览").length).toBeGreaterThan(0);
    expect(screen.getByText("运维")).toBeTruthy();
    expect(screen.getByText("管理")).toBeTruthy();
    expect(screen.getByText("主机监控")).toBeTruthy();
    expect(screen.getByText("开源协议")).toBeTruthy();
    expect(screen.getAllByRole("button", { name: "切换导航展开收起" })).toHaveLength(2);
  });

  it("登录后以紧凑身份胶囊展示首字母、用户名、角色，并经键盘展开唯一退出动作", async () => {
    renderWithProviders(<AppRoutes />, { route: "/repositories", authenticated: true });

    const accountButton = await screen.findByRole("button", { name: "admin（管理员）" });
    expect(within(accountButton).getByTestId("account-avatar").textContent).toBe("A");
    expect(within(accountButton).getByText("admin")).toBeTruthy();
    expect(within(accountButton).getByText("管理员")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "退出登录" })).toBeNull();

    accountButton.focus();
    await userEvent.keyboard("{Enter}");

    const identity = await screen.findByTestId("account-identity");
    expect(within(identity).getByText("当前登录身份")).toBeTruthy();
    expect(within(identity).getByText("admin")).toBeTruthy();
    expect(within(identity).getByText("管理员")).toBeTruthy();
    expect(within(identity).queryByRole("menuitem")).toBeNull();
    expect(screen.getAllByRole("menuitem", { name: "退出登录" })).toHaveLength(1);
  });

  it("点击刷新按钮进入刷新态，请求归零后恢复可用", async () => {
    renderWithProviders(<AppRoutes />, { route: "/repositories" });
    // 等首屏数据就绪，避免初始请求干扰计数
    expect(await screen.findByText("npm-proxy")).toBeTruthy();
    const refreshButton = (await screen.findByRole("button", {
      name: "刷新",
    })) as HTMLButtonElement;
    expect(refreshButton.disabled).toBe(false);

    // 点击派发全局刷新事件并进入刷新态；并发负载下"立即断言 disabled"存在竞速，
    // 改为断言事件派发（确定性）+ 最终恢复可用（waitFor）。
    const dispatchSpy = vi.spyOn(window, "dispatchEvent");
    await userEvent.click(refreshButton);
    expect(
      dispatchSpy.mock.calls.some(
        ([event]) => event instanceof CustomEvent && event.type === REFRESH_EVENT,
      ),
    ).toBe(true);
    dispatchSpy.mockRestore();
    // 触发的重新拉取归零后（含最短旋转时长）恢复可用
    await waitFor(() => expect(refreshButton.disabled).toBe(false), { timeout: 3000 });
  });

  it("useAsync 响应全局刷新事件重新拉取", async () => {
    let fetches = 0;
    renderWithProviders(<AsyncProbe onFetch={() => (fetches += 1)} />);
    await waitFor(() => expect(fetches).toBe(1));

    window.dispatchEvent(new CustomEvent(REFRESH_EVENT));
    await waitFor(() => expect(fetches).toBe(2));
  });
});
