// FR-72 开源协议页：数据经 admin 端点运行时拉取（不打进前端 bundle），
// 渲染 Go/npm 两段依赖协议表格（含版本列）。
import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { LicensesPage } from "../src/pages/LicensesPage";
import { renderWithProviders } from "./harness";

describe("开源协议页（FR-72）", () => {
  it("登录后拉取清单渲染两段依赖表格（含版本列）", async () => {
    renderWithProviders(<LicensesPage />, { route: "/licenses", authenticated: true });
    // 分段标题（含 devmock 条数）
    expect(await screen.findByText("后端依赖 · Go (2)")).toBeTruthy();
    expect(screen.getByText("前端依赖 · npm (2)")).toBeTruthy();
    // 依赖行与版本列
    expect(screen.getByText("github.com/gin-gonic/gin")).toBeTruthy();
    expect(screen.getByText("@mantine/core")).toBeTruthy();
    expect(screen.getAllByText("版本").length).toBe(2);
    expect(screen.getByText("v1.10.0")).toBeTruthy();
  });

  it("搜索过滤后不匹配行消失且分段计数更新", async () => {
    const user = userEvent.setup();
    renderWithProviders(<LicensesPage />, { route: "/licenses", authenticated: true });
    await screen.findByText("后端依赖 · Go (2)");
    await user.type(screen.getByPlaceholderText("搜索依赖包 / 协议 / 作者..."), "gin-gonic");
    expect(await screen.findByText("后端依赖 · Go (1)")).toBeTruthy();
    expect(screen.queryByText("@mantine/core")).toBeNull();
  });
});
