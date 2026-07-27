// FR-72 开源协议页：搜索框 + Go/npm 两段依赖协议表格，匿名可访问。
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import licenses from "../src/generated/licenses.json";
import { LicensesPage } from "../src/pages/LicensesPage";
import { renderWithProviders } from "./harness";

describe("开源协议页（FR-72）", () => {
  it("匿名渲染两段依赖表格，含真实依赖行", () => {
    renderWithProviders(<LicensesPage />, { route: "/licenses" });
    // 两个分段标题（含条数）
    expect(screen.getByText(`后端依赖 · Go (${licenses.go.length})`)).toBeTruthy();
    expect(screen.getByText(`前端依赖 · npm (${licenses.npm.length})`)).toBeTruthy();
    // 出现真实依赖行
    expect(screen.getByText("github.com/gin-gonic/gin")).toBeTruthy();
    expect(screen.getByText("@mantine/core")).toBeTruthy();
  });

  it("匿名隐藏版本列，登录后展示", () => {
    const { unmount } = renderWithProviders(<LicensesPage />, { route: "/licenses" });
    // 匿名：无版本列表头，也不渲染具体版本号
    expect(screen.queryByText("版本")).toBeNull();
    expect(screen.queryByText(licenses.go[0].version)).toBeNull();
    unmount();

    renderWithProviders(<LicensesPage />, { route: "/licenses", authenticated: true });
    expect(screen.getAllByText("版本").length).toBeGreaterThan(0);
  });

  it("搜索过滤后不匹配行消失且分段计数更新", async () => {
    const user = userEvent.setup();
    renderWithProviders(<LicensesPage />, { route: "/licenses" });
    await user.type(screen.getByPlaceholderText("搜索依赖包 / 协议 / 作者..."), "gin-gonic");
    await waitFor(() => {
      expect(screen.queryByText("@mantine/core")).toBeNull();
    });
    expect(screen.getByText("github.com/gin-gonic/gin")).toBeTruthy();
    // Go 段计数收敛，npm 段为 0
    expect(screen.getByText(/后端依赖 · Go \(\d+\)/)).toBeTruthy();
    expect(screen.getByText("前端依赖 · npm (0)")).toBeTruthy();
  });
});
