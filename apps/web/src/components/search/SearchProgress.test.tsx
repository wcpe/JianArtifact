// 进度条组件：阶段文案、已用时长、进度推进与取消按钮。
// 时间用假定时器推进，不真等 3 秒。
import { MantineProvider } from "@mantine/core";
import { act, render, screen } from "@testing-library/react";
import "@testing-library/jest-dom/vitest";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import "../../i18n";
import { SearchProgress } from "./SearchProgress";

function renderProgress(onCancel: () => void) {
  return render(
    <MantineProvider>
      <SearchProgress onCancel={onCancel} />
    </MantineProvider>,
  );
}

/** 读取进度条当前百分比（Mantine 的 Progress 自带 role=progressbar + aria-valuenow）。 */
function progressValue(): number {
  return Number(screen.getByRole("progressbar").getAttribute("aria-valuenow"));
}

describe("SearchProgress", () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it("展示阶段文案、已用时长、取消按钮，并按时序推进", () => {
    renderProgress(vi.fn());

    expect(screen.getByTestId("search-progress")).toBeInTheDocument();
    expect(screen.getByTestId("search-progress")).toHaveTextContent("正在检索索引…");
    expect(screen.getByTestId("search-progress")).toHaveTextContent("· 0.0s");
    expect(screen.getByRole("button", { name: "取消搜索" })).toBeInTheDocument();
    expect(progressValue()).toBe(0);

    // 1.1s：进度过半前，阶段切到「筛选制品」，时长读数跟着走。
    act(() => {
      vi.advanceTimersByTime(1_100);
    });
    expect(progressValue()).toBeGreaterThan(0);
    expect(screen.getByTestId("search-progress")).toHaveTextContent("正在筛选制品…");
    expect(screen.getByTestId("search-progress")).toHaveTextContent("· 1.1s");

    // 3.2s：缓出段走完，停在 88%。
    act(() => {
      vi.advanceTimersByTime(2_100);
    });
    expect(progressValue()).toBe(88);

    // 再 0.7s：冲刺到 100%，阶段为「即将完成」。
    act(() => {
      vi.advanceTimersByTime(700);
    });
    expect(progressValue()).toBe(100);
    expect(screen.getByTestId("search-progress")).toHaveTextContent("即将完成…");
  });

  it("点取消按钮触发回调（由父组件 abort 请求）", () => {
    const onCancel = vi.fn();
    renderProgress(onCancel);

    screen.getByRole("button", { name: "取消搜索" }).click();

    expect(onCancel).toHaveBeenCalledTimes(1);
  });

  it("卸载后进度条与定时器一并消失", () => {
    const { unmount } = renderProgress(vi.fn());
    expect(screen.getByTestId("search-progress")).toBeInTheDocument();

    unmount();

    expect(screen.queryByTestId("search-progress")).toBeNull();
    expect(vi.getTimerCount()).toBe(0);
  });
});
