// 仓库详情集成测试：渲染种子制品列表与使用说明片段；未鉴权进入错误态。
import { screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { Route, Routes } from "react-router-dom";

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
  it("渲染种子制品与使用说明", async () => {
    renderDetail("maven-releases", true);
    expect(await screen.findByText("com/example/app/1.0.0/app-1.0.0.jar")).toBeTruthy();
    expect(await screen.findByText("com/example/app/1.0.0/app-1.0.0.pom")).toBeTruthy();
    // maven hosted：解析依赖片段 + 复制按钮存在。
    expect(await screen.findByText("解析依赖（pom.xml）")).toBeTruthy();
    expect((await screen.findAllByRole("button", { name: "复制" })).length).toBeGreaterThan(0);
  });

  it("未鉴权进入错误态", async () => {
    renderDetail("maven-releases", false);
    expect((await screen.findAllByTestId("state-error")).length).toBeGreaterThan(0);
  });
});
