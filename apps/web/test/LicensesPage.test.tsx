// FR-72 开源协议页：数据经 admin 端点运行时拉取（不打进前端 bundle），
// 渲染 Go/npm 两段依赖协议表格（含版本列）。
import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import {
  releaseDevMockPendingRequests,
  waitForDevMockPendingRequest,
} from "@jianartifact/devmock/scenario";
import { LicensesPage } from "../src/pages/LicensesPage";
import { renderWithProviders } from "./harness";

describe("开源协议页（FR-72）", () => {
  it("登录后拉取清单渲染两段依赖表格（含版本列）", async () => {
    renderWithProviders(<LicensesPage />, { route: "/licenses", authenticated: true });
    // 分段标题带条数：条数随真实依赖清单变化，故只断言"存在计数"而不写死数值。
    expect(await screen.findByText(/后端依赖 · Go \(\d+\)/)).toBeTruthy();
    expect(screen.getByText(/前端依赖 · npm \(\d+\)/)).toBeTruthy();
    // 依赖行与版本列
    expect(screen.getByText("github.com/gin-gonic/gin")).toBeTruthy();
    expect(screen.getByText("@mantine/core")).toBeTruthy();
    expect(screen.getAllByText("版本").length).toBe(2);
    // 版本列确有取值（形如 v1.12.0 / 7.17.8）
    expect(screen.getAllByText(/^v?\d+\.\d+\.\d+/).length).toBeGreaterThan(0);
  });

  it("empty 场景显示两个依赖分组的空态", async () => {
    const route = "/licenses?__mock=empty";
    window.history.replaceState({}, "", route);
    renderWithProviders(<LicensesPage />, { route, authenticated: true });

    expect((await screen.findAllByText("没有匹配的依赖")).length).toBe(2);
  });

  it("loading 场景由测试显式释放后渲染真实清单夹具", async () => {
    const route = "/licenses?__mock=loading";
    window.history.replaceState({}, "", route);
    renderWithProviders(<LicensesPage />, { route, authenticated: true });

    expect(await screen.findByTestId("state-loading")).toBeTruthy();
    await waitForDevMockPendingRequest();
    expect(releaseDevMockPendingRequests()).toBeGreaterThan(0);
    expect(await screen.findByText("react")).toBeTruthy();
  });

  it("error 场景显示可重试的读取失败反馈", async () => {
    const route = "/licenses?__mock=error";
    window.history.replaceState({}, "", route);
    renderWithProviders(<LicensesPage />, { route, authenticated: true });

    expect(await screen.findByTestId("state-error")).toBeTruthy();
    expect(screen.getByRole("button", { name: "重试" })).toBeTruthy();
  });

  it("备用只读场景仍可只读浏览协议清单", async () => {
    const route = "/licenses?__mock=standby_read_only";
    window.history.replaceState({}, "", route);
    renderWithProviders(<LicensesPage />, { route, authenticated: true });

    expect(await screen.findByText("react")).toBeTruthy();
  });

  it("搜索过滤后不匹配行消失且分段计数更新", async () => {
    const user = userEvent.setup();
    renderWithProviders(<LicensesPage />, { route: "/licenses", authenticated: true });
    await screen.findByText(/后端依赖 · Go \(\d+\)/);
    await user.type(screen.getByPlaceholderText("搜索依赖包 / 协议 / 作者..."), "gin-gonic");
    expect(await screen.findByText("后端依赖 · Go (1)")).toBeTruthy();
    expect(screen.queryByText("@mantine/core")).toBeNull();
  });
});
