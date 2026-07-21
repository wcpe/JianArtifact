// 仓库管理集成测试：渲染种子仓库；未鉴权时列表进入错误态。
import { screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { RepositoriesPage } from "../src/pages/RepositoriesPage";
import { renderWithProviders } from "./harness";

describe("仓库管理", () => {
  it("渲染种子仓库", async () => {
    renderWithProviders(<RepositoriesPage />, { route: "/repositories", authenticated: true });
    expect(await screen.findByText("maven-releases")).toBeTruthy();
    expect(await screen.findByText("npm-proxy")).toBeTruthy();
    expect(await screen.findByText("raw-hosted")).toBeTruthy();
  });

  it("未鉴权进入错误态", async () => {
    renderWithProviders(<RepositoriesPage />, { route: "/repositories", authenticated: false });
    expect(await screen.findByTestId("state-error")).toBeTruthy();
  });
});
