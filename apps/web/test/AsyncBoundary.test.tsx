// FR-69 异步加载体验：刷新/翻页时 AsyncBoundary 保留旧数据 + 局部 loading，不整页换骨架。
import { AppProvider } from "@jianartifact/ui";
import { act, render, screen } from "@testing-library/react";
import type { ReactNode } from "react";
import { describe, expect, it } from "vitest";

import { AsyncBoundary } from "../src/components/AsyncBoundary";
import { useAsync } from "../src/hooks/useAsync";
import type { AsyncState } from "../src/hooks/useAsync";
import "../src/i18n";

function renderWithMantine(ui: ReactNode) {
  return render(<AppProvider>{ui}</AppProvider>);
}

function makeState<T>(overrides: Partial<AsyncState<T>>): AsyncState<T> {
  return {
    data: null,
    loading: false,
    error: null,
    forbidden: false,
    reload: () => {},
    ...overrides,
  };
}

describe("AsyncBoundary（FR-69 keep-previous-data）", () => {
  it("首载（无数据）时显示骨架", () => {
    renderWithMantine(
      <AsyncBoundary state={makeState<string>({ loading: true })}>
        {(data) => <div>{data}</div>}
      </AsyncBoundary>,
    );
    expect(screen.getByText("加载中…")).toBeTruthy();
  });

  it("刷新中（已有旧数据）保留旧内容并显示局部 loading 覆盖层", () => {
    const { container } = renderWithMantine(
      <AsyncBoundary state={makeState<string>({ data: "旧数据仍可见", loading: true })}>
        {(data) => <div>{data}</div>}
      </AsyncBoundary>,
    );
    // 旧数据不被整页骨架替换
    expect(screen.getByText("旧数据仍可见")).toBeTruthy();
    expect(screen.queryByText("加载中…")).toBeNull();
    // 有局部 loading 覆盖层指示刷新中
    expect(container.querySelector(".mantine-LoadingOverlay-root")).toBeTruthy();
  });

  it("数据就绪且非加载中时无覆盖层", () => {
    const { container } = renderWithMantine(
      <AsyncBoundary state={makeState<string>({ data: "就绪" })}>
        {(data) => <div>{data}</div>}
      </AsyncBoundary>,
    );
    expect(screen.getByText("就绪")).toBeTruthy();
    expect(container.querySelector(".mantine-LoadingOverlay-root")).toBeNull();
  });
});

describe("useAsync + AsyncBoundary（reload 保留旧数据）", () => {
  it("reload 拉取期间旧列表保持可见", async () => {
    // 可控的两阶段 fetcher：首次立即返回，reload 后挂起直到手动 resolve。
    let call = 0;
    let resolveSecond: (v: string) => void = () => {};
    const fetcher = () => {
      call += 1;
      if (call === 1) {
        return Promise.resolve("第一版数据");
      }
      return new Promise<string>((resolve) => {
        resolveSecond = resolve;
      });
    };

    let reloadFn: () => void = () => {};
    function Probe() {
      const state = useAsync(fetcher, []);
      reloadFn = state.reload;
      return <AsyncBoundary state={state}>{(data) => <div>{data}</div>}</AsyncBoundary>;
    }

    renderWithMantine(<Probe />);
    expect(await screen.findByText("第一版数据")).toBeTruthy();

    // 触发 reload：第二次请求挂起中，旧数据必须仍在（不整页重刷）
    act(() => reloadFn());
    expect(screen.getByText("第一版数据")).toBeTruthy();
    expect(screen.queryByText("加载中…")).toBeNull();

    // 第二次请求返回后展示新数据
    await act(async () => {
      resolveSecond("第二版数据");
    });
    expect(await screen.findByText("第二版数据")).toBeTruthy();
  });
});
