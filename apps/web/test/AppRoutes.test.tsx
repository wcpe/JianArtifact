// FR-70 路由级代码分割：懒加载路由经 Suspense 解析后正常渲染（守护测试）。
import { screen, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { AppRoutes, publicRepositoryRedirectPath } from "../src/app/router";
import { renderWithProviders } from "./harness";

describe("路由懒加载（FR-70）", () => {
  it.each([
    ["审计中心", "/audit-logs"],
    ["业务仪表盘", "/dashboard"],
    ["主机监控", "/host-monitoring"],
  ])("普通用户直链%s保持管理权限 403", async (_name, route) => {
    renderWithProviders(<AppRoutes />, {
      route,
      authenticated: true,
      user: {
        id: 2,
        username: "member",
        role: "user",
        status: "active",
        createdAt: "2026-01-01T00:00:00Z",
      },
    });

    expect(await screen.findByTestId("state-forbidden")).toBeTruthy();
  });

  it("公开别名重定向保留开发态场景查询参数（FR-122）", () => {
    expect(publicRepositoryRedirectPath("raw-hosted", "?__mock=empty")).toBe(
      "/repositories/raw-hosted?__mock=empty",
    );
  });

  it("迁移详情 empty 场景经完整路由仍停留在详情页", async () => {
    const route = "/migrations/1?__mock=empty";
    window.history.replaceState({}, "", route);
    renderWithProviders(<AppRoutes />, { route, authenticated: true });

    expect(await screen.findByText("当前没有可迁移仓库，也尚无执行报告。")).toBeTruthy();
  });

  it.each([
    ["normal", "/migrations/1?__mock=normal", "详情 #1"],
    ["error", "/migrations/1?__mock=error", "state-error"],
  ])("迁移详情 %s 场景经完整路由可达", async (_scenario, route, expected) => {
    window.history.replaceState({}, "", route);
    renderWithProviders(<AppRoutes />, { route, authenticated: true });

    if (expected === "state-error") {
      expect(await screen.findByTestId(expected)).toBeTruthy();
      return;
    }
    expect(await screen.findByText(expected)).toBeTruthy();
  });

  it("登录访问 /host-monitoring 渲染当前主机监控页", async () => {
    renderWithProviders(<AppRoutes />, { route: "/host-monitoring", authenticated: true });

    // 页面大标题已移除；位置由全局页眉面包屑表达（运维 / 主机监控）。
    const breadcrumbs = await screen.findByTestId("app-breadcrumbs");
    expect(within(breadcrumbs).getByText("主机监控")).toBeTruthy();
    expect(await screen.findByText("采样正常，主机指标可用", {}, { timeout: 5_000 })).toBeTruthy();
  });

  it("登录访问 /licenses 经懒加载渲染协议页", async () => {
    renderWithProviders(<AppRoutes />, { route: "/licenses", authenticated: true });
    // lazy chunk 解析完成后出现分段标题（数据来自 devmock /api/v1/licenses）
    expect(await screen.findByText(/后端依赖 · Go \(\d+\)/)).toBeTruthy();
  });

  it("匿名访问 /licenses 被鉴权守卫拦截，不渲染协议清单", async () => {
    renderWithProviders(<AppRoutes />, { route: "/licenses" });
    // RequireAuth 弹登录模态框；协议清单内容不出现
    expect(await screen.findAllByText(/登录/)).toBeTruthy();
    expect(screen.queryByText(/后端依赖/)).toBeNull();
  });

  it("匿名访问 /repositories 经懒加载渲染仓库列表", async () => {
    renderWithProviders(<AppRoutes />, { route: "/repositories" });
    // 匿名可见 public 仓库
    expect(await screen.findByText("npm-proxy")).toBeTruthy();
  });
});
