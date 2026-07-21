// 展台渲染测试：核验四类状态态与关键交互（表单校验 + 通知）在验收站可用。
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";
import { AppProvider } from "@jianartifact/ui";
import { Notifications } from "@mantine/notifications";
import type { ReactElement } from "react";

import { StatesSection } from "../src/sections/StatesSection";
import { InteractionsSection } from "../src/sections/InteractionsSection";

function renderWithProviders(ui: ReactElement) {
  return render(
    <AppProvider>
      <Notifications />
      {ui}
    </AppProvider>,
  );
}

describe("StatesSection 状态态展台", () => {
  it("并列渲染加载 / 空 / 错误 / 越权四类状态态", () => {
    renderWithProviders(<StatesSection />);
    expect(screen.getByTestId("state-loading")).toBeInTheDocument();
    expect(screen.getByTestId("state-empty")).toBeInTheDocument();
    expect(screen.getByTestId("state-error")).toBeInTheDocument();
    expect(screen.getByTestId("state-forbidden")).toBeInTheDocument();
  });
});

describe("InteractionsSection 关键交互展台", () => {
  it("空名称提交时展示校验错误，不触发通知", async () => {
    const user = userEvent.setup();
    renderWithProviders(<InteractionsSection />);
    await user.click(screen.getByRole("button", { name: "提交" }));
    expect(await screen.findByText("名称不能为空")).toBeInTheDocument();
  });

  it("填入名称提交后弹出成功通知", async () => {
    const user = userEvent.setup();
    renderWithProviders(<InteractionsSection />);
    await user.type(screen.getByLabelText("仓库名称"), "maven-releases");
    await user.click(screen.getByRole("button", { name: "提交" }));
    expect(await screen.findByText("创建成功")).toBeInTheDocument();
  });
});
