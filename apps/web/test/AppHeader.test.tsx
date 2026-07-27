// FR-71 页眉打磨：刷新按钮旋转/禁用态随网络活动归零恢复；useAsync 响应全局刷新事件。
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { describe, expect, it } from "vitest";

import { AppRoutes } from "../src/app/router";
import { useAsync, REFRESH_EVENT } from "../src/hooks/useAsync";
import { renderWithProviders } from "./harness";

/** 使用 useAsync 的最小组件：暴露 fetcher 调用次数。 */
function AsyncProbe({ onFetch }: { onFetch: () => void }) {
  const [calls] = useState({ n: 0 });
  const { loading } = useAsync(async () => {
    calls.n += 1;
    onFetch();
    return calls.n;
  }, []);
  return <div data-testid="probe">{loading ? "loading" : `calls:${calls.n}`}</div>;
}

describe("页眉打磨（FR-71）", () => {
  it("点击刷新按钮进入刷新态，请求归零后恢复可用", async () => {
    renderWithProviders(<AppRoutes />, { route: "/repositories" });
    // 等首屏数据就绪，避免初始请求干扰计数
    expect(await screen.findByText("npm-proxy")).toBeTruthy();
    const refreshButton = (await screen.findByRole("button", {
      name: "刷新",
    })) as HTMLButtonElement;
    expect(refreshButton.disabled).toBe(false);

    await userEvent.click(refreshButton);
    // 点击后立即进入刷新态（禁用 + 旋转指示）
    expect(refreshButton.disabled).toBe(true);
    // 触发的重新拉取归零后（含最短旋转时长）恢复可用
    await waitFor(() => expect(refreshButton.disabled).toBe(false), { timeout: 3000 });
  });

  it("useAsync 响应全局刷新事件重新拉取", async () => {
    let fetches = 0;
    renderWithProviders(<AsyncProbe onFetch={() => (fetches += 1)} />);
    await waitFor(() => expect(fetches).toBe(1));

    window.dispatchEvent(new CustomEvent(REFRESH_EVENT));
    await waitFor(() => expect(fetches).toBe(2));
  });
});
