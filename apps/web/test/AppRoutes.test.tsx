// FR-70 路由级代码分割：懒加载路由经 Suspense 解析后正常渲染（守护测试）。
import { screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { AppRoutes } from "../src/app/router";
import { renderWithProviders } from "./harness";

describe("路由懒加载（FR-70）", () => {
  it("登录访问 /licenses 经懒加载渲染协议页", async () => {
    renderWithProviders(<AppRoutes />, { route: "/licenses", authenticated: true });
    // lazy chunk 解析完成后出现分段标题（数据来自 devmock /api/v1/licenses）
    expect(await screen.findByText("后端依赖 · Go (2)")).toBeTruthy();
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
