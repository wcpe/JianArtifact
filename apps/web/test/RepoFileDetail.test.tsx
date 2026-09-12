// FR-105：文件详情不提供删除入口，危险操作仅从资产树右键菜单发起。
import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { RepoBrowser } from "../src/components/repo/RepoBrowser";
import type { User } from "../src/api/types";
import { renderWithProviders } from "./harness";

const NON_ADMIN_USER: User = {
  id: 2,
  username: "developer",
  role: "user",
  status: "active",
  createdAt: "2026-01-02T00:00:00Z",
};

const JAR_PATH = "com/example/app/1.0.0/app-1.0.0.jar";

/** 逐级展开 maven-releases 目录树并点选 jar 文件，等右侧详情渲染。 */
async function openJarDetail(user: ReturnType<typeof userEvent.setup>) {
  await user.click(await screen.findByText("com"));
  await user.click(await screen.findByText("example"));
  await user.click(await screen.findByText("app"));
  await user.click(await screen.findByText("1.0.0"));
  await user.click(await screen.findByText("app-1.0.0.jar"));
  await screen.findByText(JAR_PATH);
}

function renderBrowser(authenticated: boolean, user?: User) {
  return renderWithProviders(<RepoBrowser repoName="maven-releases" allowUpload />, {
    route: "/",
    authenticated,
    user,
  });
}

describe("FR-105 文件详情", () => {
  it("管理员选中文件后不显示删除按钮", async () => {
    const user = userEvent.setup();
    renderBrowser(true);
    await openJarDetail(user);
    expect(screen.queryByRole("button", { name: "删除" })).toBeNull();
  });

  it("非管理员选中文件后不显示删除按钮", async () => {
    const user = userEvent.setup();
    renderBrowser(true, NON_ADMIN_USER);
    await openJarDetail(user);
    expect(screen.queryByRole("button", { name: "删除" })).toBeNull();
  });
});
