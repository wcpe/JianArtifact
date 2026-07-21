// @vitest-environment jsdom
// 共享状态组件渲染测试：验证四种业务状态态在 Mantine Provider 下正确呈现文案与测试锚点。
import { render, screen } from "@testing-library/react";
import type { ReactNode } from "react";
import { describe, expect, it, vi } from "vitest";

import { AppProvider } from "./AppProvider";
import { EmptyState, ErrorState, ForbiddenState, LoadingState } from "./components/StateMessage";

function renderWithProvider(ui: ReactNode) {
  return render(<AppProvider>{ui}</AppProvider>);
}

describe("业务状态态组件", () => {
  it("加载态渲染默认文案", () => {
    renderWithProvider(<LoadingState />);
    expect(screen.getByTestId("state-loading").textContent).toContain("加载中");
  });

  it("空态渲染主副文案", () => {
    renderWithProvider(<EmptyState message="暂无用户" description="点击右上角新建" />);
    const node = screen.getByTestId("state-empty");
    expect(node.textContent).toContain("暂无用户");
    expect(node.textContent).toContain("点击右上角新建");
  });

  it("错误态可触发重试回调", () => {
    const onRetry = vi.fn();
    renderWithProvider(<ErrorState message="加载失败" onRetry={onRetry} />);
    screen.getByText("重试").click();
    expect(onRetry).toHaveBeenCalledTimes(1);
  });

  it("越权态渲染默认提示", () => {
    renderWithProvider(<ForbiddenState />);
    expect(screen.getByTestId("state-forbidden").textContent).toContain("无访问权限");
  });
});
