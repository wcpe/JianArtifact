// 首次初始化页集成测试：验证空库实例展示初始化引导，并走完「欢迎 → 创建管理员 → 完成」流程写入会话令牌。
import { emptyStore } from "@jianartifact/devmock";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";

import { server } from "@jianartifact/devmock/node";
import { getToken } from "../src/api/client";
import { SetupPage } from "../src/pages/SetupPage";
import { renderWithProviders } from "./harness";

function mockEmptyStatus(bootstrapAllowed: boolean) {
  server.use(
    http.get("*/api/v1/status", () =>
      HttpResponse.json({
        version: "",
        ready: true,
        initialized: false,
        migrationVersion: "",
        userCount: 0,
        bootstrapAllowed,
      }),
    ),
  );
}

describe("首次初始化页", () => {
  it("空库展示初始化引导", async () => {
    emptyStore();
    mockEmptyStatus(true);
    renderWithProviders(<SetupPage />, { route: "/setup" });
    expect(await screen.findByText("初始化实例")).toBeTruthy();
    expect(await screen.findByText("欢迎使用 JianArtifact")).toBeTruthy();
  });

  it("创建管理员后写入会话令牌", async () => {
    emptyStore();
    mockEmptyStatus(true);
    const user = userEvent.setup();
    renderWithProviders(<SetupPage />, { route: "/setup" });

    await user.click(await screen.findByRole("button", { name: "开始初始化" }));

    await user.type(screen.getByLabelText(/用户名/), "root");
    await user.type(screen.getByLabelText(/^口令/), "Password1");
    await user.type(screen.getByLabelText(/确认口令/), "Password1");
    await user.click(screen.getByRole("button", { name: "创建并进入" }));

    await waitFor(() => expect(getToken()).toBe("mock.jwt.token"));
    expect(await screen.findByText("初始化完成")).toBeTruthy();
  });

  it("备用空库直达初始化页时不展示创建入口", async () => {
    mockEmptyStatus(false);
    renderWithProviders(<SetupPage />, { route: "/setup" });

    expect(await screen.findByText("当前实例暂不允许初始化")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "开始初始化" })).toBeNull();
    expect(screen.queryByLabelText("用户名")).toBeNull();
    expect(screen.queryByLabelText("口令")).toBeNull();
  });
});
