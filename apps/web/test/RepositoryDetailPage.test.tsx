// 仓库详情集成测试：目录树逐级展开点选文件后渲染详情/使用说明；匿名访问 private 仓库报未认证。
// FR-74：整页固定布局（面板内滚）、未登录仅页眉一个登录入口、客户端发布提示收纳为紧凑小字。
import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";
import { Route, Routes } from "react-router-dom";

import { AppRoutes } from "../src/app/router";
import { RepositoryDetailPage } from "../src/pages/RepositoryDetailPage";
import { renderWithProviders } from "./harness";

function renderDetail(name: string, authenticated: boolean) {
  return renderWithProviders(
    <Routes>
      <Route path="/repositories/:name" element={<RepositoryDetailPage />} />
    </Routes>,
    { route: `/repositories/${name}`, authenticated },
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

  it("匿名访问 private 仓库展示未认证错误", async () => {
    renderDetail("maven-releases", false);
    // FR-66：匿名仅可读 public 仓库，private 目录树请求 401 → 错误提示
    expect((await screen.findAllByText("未认证")).length).toBeGreaterThan(0);
  });

  it("未登录访问详情页仅页眉一个登录入口（FR-74）", async () => {
    renderWithProviders(<AppRoutes />, { route: "/repositories/npm-proxy" });
    // 等详情页渲染完成（页头标题含仓库名）
    expect(await screen.findByText("仓库详情 · npm-proxy")).toBeTruthy();
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
    expect(shell.style.height).toContain("100vh");
  });
});
