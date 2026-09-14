// 在线 Nexus 迁移向导：直填地址与临时认证信息的请求和存储边界。
import { cleanup, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({
  discoverMigrations: vi.fn(),
}));

vi.mock("../src/api/endpoints", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../src/api/endpoints")>()),
  discoverMigrations: api.discoverMigrations,
}));

import { MigrationWizardPage } from "../src/pages/MigrationWizardPage";
import { server } from "@jianartifact/devmock/node";
import { renderWithProviders } from "./harness";

const discoverResponse = {
  taskId: 41,
  plan: {
    repositories: [{ name: "maven-releases", format: "maven", type: "hosted" }],
    warnings: [],
    stats: { repositoryCount: 1 },
    estimated: true,
  },
};

async function openOnlineConfig(user: ReturnType<typeof userEvent.setup>) {
  sessionStorage.clear();
  renderWithProviders(<MigrationWizardPage />, { route: "/migrations/new", authenticated: true });
  await user.click(await screen.findByRole("button", { name: "确定" }));
  return screen.findByLabelText("Nexus 地址");
}

function sourceAuthSelect(): HTMLInputElement {
  return screen.getByRole("textbox", { name: "认证方式" });
}

function discoverInput(): Record<string, unknown> {
  return api.discoverMigrations.mock.calls[0]?.[0] as Record<string, unknown>;
}

beforeEach(() => {
  api.discoverMigrations.mockReset().mockResolvedValue(discoverResponse);
  // 向导页首屏会做"初始迁移检查"：一旦存在 running 任务会提示并重定向到该任务详情
  // （不允许并发迁移，属正确产品行为）。本文件验证的是"可新建"路径，
  // 故显式声明前置为无进行中任务，不依赖 devmock 种子夹具的偶然状态。
  server.use(http.get("*/api/v1/migrations", () => HttpResponse.json({ items: [], total: 0 })));
});

afterEach(() => {
  cleanup();
  sessionStorage.clear();
});

describe("在线 Nexus 迁移向导", () => {
  it("普通用户的初始迁移检查显示越权态且不渲染向导", async () => {
    server.use(
      http.get("*/api/v1/migrations", () =>
        HttpResponse.json(
          { error: { code: "forbidden", message: "需要管理员权限" } },
          { status: 403 },
        ),
      ),
    );
    renderWithProviders(<MigrationWizardPage />, {
      route: "/migrations/new",
      authenticated: true,
      user: {
        id: 2,
        username: "developer",
        role: "user",
        status: "active",
        createdAt: "2026-01-02T00:00:00Z",
      },
    });

    expect(await screen.findByTestId("state-forbidden")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "确定" })).toBeNull();
  });

  it("初始迁移检查失败显示可重试错误态", async () => {
    server.use(
      http.get("*/api/v1/migrations", () =>
        HttpResponse.json(
          { error: { code: "migration_unavailable", message: "迁移服务暂不可用" } },
          { status: 500 },
        ),
      ),
    );
    renderWithProviders(<MigrationWizardPage />, { route: "/migrations/new", authenticated: true });

    expect((await screen.findByTestId("state-error")).textContent).toContain("迁移服务暂不可用");
    expect(screen.getByRole("button", { name: "重试" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "确定" })).toBeNull();
  });

  it("匿名方式发送直填 Nexus 地址", async () => {
    const user = userEvent.setup();

    await openOnlineConfig(user);
    await user.type(screen.getByLabelText("Nexus 地址"), "https://bak.maven.example.com");
    expect((screen.getByLabelText("Nexus 地址") as HTMLInputElement).value).toBe(
      "https://bak.maven.example.com",
    );
    const discoverButton = screen.getByRole("button", { name: "发现并落库" });
    expect((discoverButton as HTMLButtonElement).disabled).toBe(false);
    await user.click(discoverButton);

    await waitFor(() => expect(api.discoverMigrations).toHaveBeenCalledTimes(1));
    expect(discoverInput()).toMatchObject({
      sourceType: "online_rest",
      sourceConfig: { url: "https://bak.maven.example.com" },
      sourceAuth: { type: "anonymous" },
      conflictPolicy: "skip",
    });
  });

  it("发现失败后保留可安全重试的来源地址与当前步骤", async () => {
    const user = userEvent.setup();
    api.discoverMigrations.mockRejectedValueOnce(new Error("网络暂不可用"));

    const address = await openOnlineConfig(user);
    await user.type(address, "https://example.invalid");
    await user.click(screen.getByRole("button", { name: "发现并落库" }));

    expect(await screen.findByText("发现失败，请检查 Nexus 地址、认证方式和网络连接")).toBeTruthy();
    expect((screen.getByLabelText("Nexus 地址") as HTMLInputElement).value).toBe(
      "https://example.invalid",
    );
    expect(screen.getByRole("button", { name: "发现并落库" })).toBeTruthy();
  });

  it("Basic 认证发送用户名和密码，但不写入向导草稿", async () => {
    const user = userEvent.setup();
    const password = "user-token-password";

    await openOnlineConfig(user);
    await user.type(screen.getByLabelText("Nexus 地址"), "https://bak.maven.example.com");
    await user.click(sourceAuthSelect());
    await user.click(
      await screen.findByRole("option", { name: "Basic（用户名/密码或 User Token）" }),
    );
    await screen.findByLabelText("用户名");
    await screen.findByLabelText("密码");
    await user.type(screen.getByLabelText("用户名"), "user-token-name");
    await user.type(screen.getByLabelText("密码"), password);

    await waitFor(() => {
      const draft = sessionStorage.getItem("jianartifact.migration.wizard") ?? "";
      expect(draft).not.toContain("user-token-name");
      expect(draft).not.toContain(password);
    });

    await user.click(screen.getByRole("button", { name: "发现并落库" }));
    await waitFor(() => expect(api.discoverMigrations).toHaveBeenCalledTimes(1));
    expect(discoverInput()).toMatchObject({
      sourceAuth: { type: "basic", username: "user-token-name", password },
    });
  }, 10_000);

  it("切换为 Bearer 后仅发送令牌认证", async () => {
    const user = userEvent.setup();

    await openOnlineConfig(user);
    await user.type(screen.getByLabelText("Nexus 地址"), "https://bak.maven.example.com");
    await user.click(sourceAuthSelect());
    await user.click(
      await screen.findByRole("option", { name: "Basic（用户名/密码或 User Token）" }),
    );
    await screen.findByLabelText("用户名");
    await screen.findByLabelText("密码");
    await user.type(screen.getByLabelText("用户名"), "should-be-cleared");
    await user.type(screen.getByLabelText("密码"), "should-be-cleared");
    await user.click(sourceAuthSelect());
    await user.click(await screen.findByRole("option", { name: "Bearer Token" }));
    await screen.findByLabelText("令牌");
    await user.type(screen.getByLabelText("令牌"), "bearer-token-value");

    await user.click(screen.getByRole("button", { name: "发现并落库" }));
    await waitFor(() => expect(api.discoverMigrations).toHaveBeenCalledTimes(1));
    expect(discoverInput()).toMatchObject({
      sourceAuth: { type: "bearer", token: "bearer-token-value" },
    });
    expect(JSON.stringify(discoverInput())).not.toContain("should-be-cleared");
  });

  it("缺少与认证方式对应的凭据时不发起发现", async () => {
    const user = userEvent.setup();

    await openOnlineConfig(user);
    await user.type(screen.getByLabelText("Nexus 地址"), "https://bak.maven.example.com");
    await user.click(sourceAuthSelect());
    await user.click(await screen.findByRole("option", { name: "Bearer Token" }));
    await screen.findByLabelText("令牌");
    await user.click(screen.getByRole("button", { name: "发现并落库" }));

    expect(api.discoverMigrations).not.toHaveBeenCalled();
    expect(await screen.findByText("请填写 Bearer Token")).toBeTruthy();
  });
});
