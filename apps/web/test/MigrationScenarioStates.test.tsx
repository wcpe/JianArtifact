import {
  releaseDevMockPendingRequests,
  waitForDevMockPendingRequest,
} from "@jianartifact/devmock/scenario";
import { screen, waitFor } from "@testing-library/react";
import { Route, Routes } from "react-router-dom";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { store } from "@jianartifact/devmock";
import { MigrationDetailPage } from "../src/pages/MigrationDetailPage";
import { MigrationWizardPage } from "../src/pages/MigrationWizardPage";
import { renderWithProviders } from "./harness";

function renderDetail(route: string) {
  window.history.replaceState({}, "", route);
  return renderWithProviders(
    <Routes>
      <Route path="/migrations/:id" element={<MigrationDetailPage />} />
    </Routes>,
    { route, authenticated: true },
  );
}

function renderWizard(scenario: "empty" | "loading" | "error" | "standby_read_only") {
  const route = `/migrations/new?__mock=${scenario}`;
  window.history.replaceState({}, "", route);
  return renderWithProviders(<MigrationWizardPage />, { route, authenticated: true });
}

describe("迁移详情开发态场景", () => {
  it("开始迁移前要求确认，未确认时任务仍保持 planned", async () => {
    const user = userEvent.setup();

    renderDetail("/migrations/1?__mock=normal");
    expect(await screen.findByText("详情 #1")).toBeTruthy();
    await user.click(screen.getByRole("button", { name: "开始迁移" }));

    expect(await screen.findByRole("dialog", { name: "确认开始迁移" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "开始迁移" })).toBeTruthy();
    expect(screen.getAllByText("已计划").length).toBeGreaterThan(0);
  });

  it("续传前要求确认，未确认时任务仍保持失败", async () => {
    const user = userEvent.setup();

    renderDetail("/migrations/3?__mock=normal");
    expect(await screen.findByText("详情 #3")).toBeTruthy();
    await user.click(screen.getByRole("button", { name: "续传" }));

    expect(await screen.findByRole("dialog", { name: "确认续传" })).toBeTruthy();
    expect(screen.getAllByText("失败").length).toBeGreaterThan(0);
  });

  it("任务不存在时展示可重试错误态，运行中的详情卸载后停止轮询", async () => {
    renderDetail("/migrations/404?__mock=normal");
    expect(await screen.findByTestId("state-error")).toBeTruthy();
    expect(screen.getByRole("button", { name: "重试" })).toBeTruthy();

    const clearInterval = vi.spyOn(window, "clearInterval");
    const { unmount } = renderDetail("/migrations/2?__mock=normal");
    expect(await screen.findByText("详情 #2")).toBeTruthy();
    unmount();
    await waitFor(() => expect(clearInterval).toHaveBeenCalled());
    clearInterval.mockRestore();
  });

  it("loading 显式释放后展示同一生命周期夹具", async () => {
    renderDetail("/migrations/1?__mock=loading");

    expect(await screen.findByTestId("state-loading")).toBeTruthy();
    await waitForDevMockPendingRequest();
    expect(releaseDevMockPendingRequests()).toBeGreaterThan(0);
    expect(await screen.findByText("详情 #1")).toBeTruthy();
  });

  it("empty 详情展示无候选和无报告提示", async () => {
    renderDetail("/migrations/1?__mock=empty");

    expect(await screen.findByText("当前没有可迁移仓库，也尚无执行报告。")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "开始迁移" })).toBeNull();
    expect(screen.queryByRole("button", { name: "取消迁移" })).toBeNull();
  });

  it("向导 normal 场景不预置活动任务，管理员仍可开始新发现", async () => {
    const route = "/migrations/new?__mock=normal";
    window.history.replaceState({}, "", route);
    renderWithProviders(<MigrationWizardPage />, { route, authenticated: true });

    expect(await screen.findByRole("link", { name: "返回列表" })).toBeTruthy();
    expect(screen.queryByText("迁移服务正在检查是否已有进行中的任务…")).toBeNull();
  });

  it("向导 empty 场景仍可开始新的迁移发现", async () => {
    renderWizard("empty");
    expect(await screen.findByRole("link", { name: "返回列表" })).toBeTruthy();
  });

  it("向导 loading 场景由测试显式释放后可继续配置", async () => {
    renderWizard("loading");
    expect(await screen.findByText("正在检查是否有未完成的迁移任务…")).toBeTruthy();
    await waitForDevMockPendingRequest();
    expect(releaseDevMockPendingRequests()).toBeGreaterThan(0);
    expect(await screen.findByRole("link", { name: "返回列表" })).toBeTruthy();
  });

  it("向导 error 场景展示初始检查的重试反馈", async () => {
    renderWizard("error");
    expect(await screen.findByTestId("state-error")).toBeTruthy();
    expect(screen.getByRole("button", { name: "重试" })).toBeTruthy();
  });

  it("备用只读向导允许查看配置，但不伪造运行中任务", async () => {
    renderWizard("standby_read_only");
    expect(await screen.findByRole("link", { name: "返回列表" })).toBeTruthy();
    expect(screen.queryByText("迁移服务正在检查是否已有进行中的任务…")).toBeNull();
  });

  it("备用只读下开始迁移仍先要求确认且保持 planned 状态", async () => {
    store.seedMigrationLifecycleFixtures();
    const user = userEvent.setup();
    const route = "/migrations/1?__mock=standby_read_only";
    renderDetail(route);

    expect(await screen.findByText("详情 #1")).toBeTruthy();
    const start = screen.getByRole("button", { name: "开始迁移" });
    await user.click(start);

    expect(await screen.findByRole("dialog", { name: "确认开始迁移" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "开始迁移" })).toBeTruthy();
  });
});
