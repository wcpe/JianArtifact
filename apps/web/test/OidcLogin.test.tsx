// FR-34：OIDC 登录前端段——入口可见性与回调片段处理。
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";

import { server } from "@jianartifact/devmock/node";

import { useAuth } from "../src/auth/AuthContext";
import { useLoginModal } from "../src/auth/LoginModal";
import { renderWithProviders } from "./harness";

// LoginHarness 提供打开登录框的入口，并暴露当前会话状态供断言。
function LoginHarness() {
  const { openLogin } = useLoginModal();
  const { isAuthenticated, user } = useAuth();
  return (
    <div>
      <button onClick={() => openLogin()}>open-login</button>
      <span data-testid="auth-state">{isAuthenticated ? user?.username : "anonymous"}</span>
    </div>
  );
}

const statusFixture = {
  version: "test",
  ready: true,
  initialized: true,
  migrationVersion: "0001_init",
  userCount: 1,
  bootstrapAllowed: false,
  oidcEnabled: false,
};

describe("OIDC 登录前端段", () => {
  it("未启用 OIDC 时不展示登录入口", async () => {
    const user = userEvent.setup();
    renderWithProviders(<LoginHarness />);
    await user.click(screen.getByText("open-login"));
    await screen.findByLabelText(/用户名|Username/);
    expect(screen.queryByRole("link", { name: /OIDC/ })).toBeNull();
  });

  it("启用 OIDC 时展示入口并指向起跳端点", async () => {
    server.use(
      http.get("*/api/v1/status", () => HttpResponse.json({ ...statusFixture, oidcEnabled: true })),
    );
    const user = userEvent.setup();
    renderWithProviders(<LoginHarness />);
    await user.click(screen.getByText("open-login"));

    const link = await screen.findByRole("link", { name: /OIDC/ });
    // 入口必须是顶层跳转（浏览器离开本页），而不是 fetch 调用。
    expect(link.getAttribute("href")).toBe("/api/v1/auth/oidc/start");
  });

  it("回调令牌片段可建立会话且片段被清除", async () => {
    // 生产环境里回调是真实浏览器导航，片段在 window.location 上；测试用 MemoryRouter，
    // 故需在 jsdom 层面直接设置地址（而非 renderWithProviders 的 route）。
    window.history.replaceState({}, "", "/#token=mock.jwt.token:admin");
    renderWithProviders(<LoginHarness />);

    // 片段只带令牌：身份快照经 /auth/me 取回（devmock 的 /auth/me 返回 mock 用户）。
    await waitFor(() => {
      expect(screen.getByTestId("auth-state").textContent).not.toBe("anonymous");
    });
    expect(window.location.hash).toBe("");
  });
});
