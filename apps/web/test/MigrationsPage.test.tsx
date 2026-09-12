import {
  releaseDevMockPendingRequests,
  waitForDevMockPendingRequest,
} from "@jianartifact/devmock/scenario";
import { screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { MigrationsPage } from "../src/pages/MigrationsPage";
import { renderWithProviders } from "./harness";

describe("迁移列表开发态场景", () => {
  it("normal 场景展示可进入详情的生命周期任务", async () => {
    window.history.replaceState({}, "", "/migrations?__mock=normal");
    renderWithProviders(<MigrationsPage />, {
      route: "/migrations?__mock=normal",
      authenticated: true,
    });

    expect(await screen.findByText("#5")).toBeTruthy();
  });

  it("empty 场景仍展示空态", async () => {
    window.history.replaceState({}, "", "/migrations?__mock=empty");
    renderWithProviders(<MigrationsPage />, {
      route: "/migrations?__mock=empty",
      authenticated: true,
    });

    expect(await screen.findByTestId("state-empty")).toBeTruthy();
  });

  it("loading 场景仅在显式释放后继续加载", async () => {
    const route = "/migrations?__mock=loading";
    window.history.replaceState({}, "", route);
    renderWithProviders(<MigrationsPage />, { route, authenticated: true });

    expect(await screen.findByTestId("state-loading")).toBeTruthy();
    await waitForDevMockPendingRequest();
    expect(releaseDevMockPendingRequests()).toBeGreaterThan(0);
    expect(await screen.findByText("#5")).toBeTruthy();
  });

  it("error 场景展示可重试错误态", async () => {
    const route = "/migrations?__mock=error";
    window.history.replaceState({}, "", route);
    renderWithProviders(<MigrationsPage />, { route, authenticated: true });

    expect(await screen.findByTestId("state-error")).toBeTruthy();
    expect(screen.getByRole("button", { name: "重试" })).toBeTruthy();
  });

  it("备用只读场景允许读取，但不会伪造活动任务", async () => {
    const route = "/migrations?__mock=standby_read_only";
    window.history.replaceState({}, "", route);
    renderWithProviders(<MigrationsPage />, { route, authenticated: true });

    expect(await screen.findByTestId("state-empty")).toBeTruthy();
    expect(screen.queryByText("迁移服务正在检查是否已有进行中的任务…")).toBeNull();
  });
});
