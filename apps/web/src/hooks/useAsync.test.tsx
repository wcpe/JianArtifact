// useAsync 的 abortable 扩展、乱序保护与身份边界：
// - 开启 abortable 后 fetcher 收到 AbortSignal，cancel() 真的 abort 在途请求；
// - 取消不是失败：静默回到空闲态，不落 error / refreshError；
// - 口径变化（deps 变）会中止上一次请求；
// - 旧响应晚返回不得覆盖新数据（abort 与 active 标志双保险，两种模式各验一遍）；
// - keepPreviousData 只在同一身份内保留旧数据：身份一变立即丢弃、且不回放缓存，
//   新请求失败或被取消时都不得把上一主体的数据放回界面。
import { act, renderHook, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { ApiError } from "../api/client";
import { useAsync } from "./useAsync";

/** 可手工控制落定时机、并可按 abort 自动拒绝的请求桩。 */
function makeDeferred<T>(signal?: AbortSignal) {
  let resolve!: (value: T) => void;
  let reject!: (reason: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  signal?.addEventListener("abort", () => {
    // 与 api/client 的失败口径一致：中止归一为 ApiError(code=aborted)。
    reject(new ApiError("aborted", "请求已取消或超时", 0));
  });
  return { promise, resolve, reject };
}

describe("useAsync · abortable", () => {
  it("把 AbortSignal 透传给 fetcher，cancel() 真正 abort 并静默回到空闲态", async () => {
    const signals: AbortSignal[] = [];
    const fetcher = vi.fn((signal?: AbortSignal) => {
      if (signal) signals.push(signal);
      return makeDeferred<string>(signal).promise;
    });
    const { result } = renderHook(() =>
      useAsync(fetcher, ["q"], { cacheKey: "cancel-case", abortable: true }),
    );

    await waitFor(() => expect(signals).toHaveLength(1));
    expect(signals[0]!.aborted).toBe(false);
    expect(result.current.loading).toBe(true);

    act(() => result.current.cancel());

    // 断言请求信号真的被中止（这是「取消」区别于「丢弃响应」的关键）。
    expect(signals[0]!.aborted).toBe(true);
    await waitFor(() => expect(result.current.cancelled).toBe(true));
    expect(result.current.loading).toBe(false);
    expect(result.current.refreshing).toBe(false);
    // 取消不是失败：不产生任何错误态。
    expect(result.current.error).toBeNull();
    expect(result.current.refreshError).toBeNull();
    expect(result.current.data).toBeNull();
  });

  it("取消后可以立刻重新发起（reload 退出已取消态并重新拉取）", async () => {
    let attempt = 0;
    const fetcher = vi.fn((signal?: AbortSignal) => {
      attempt += 1;
      if (attempt === 1) return makeDeferred<string>(signal).promise;
      return Promise.resolve("第二轮结果");
    });
    const { result } = renderHook(() =>
      useAsync(fetcher, ["q"], { cacheKey: "retry-after-cancel", abortable: true }),
    );

    act(() => result.current.cancel());
    await waitFor(() => expect(result.current.cancelled).toBe(true));

    act(() => result.current.reload());

    await waitFor(() => expect(result.current.data).toBe("第二轮结果"));
    expect(result.current.cancelled).toBe(false);
    expect(result.current.loading).toBe(false);
    expect(result.current.error).toBeNull();
  });

  it("口径变化时中止上一次请求，旧响应晚到不覆盖新数据", async () => {
    const deferreds = new Map<string, ReturnType<typeof makeDeferred<string>>>();
    const { result, rerender } = renderHook(
      ({ q }: { q: string }) =>
        useAsync(
          (signal) => {
            const deferred = makeDeferred<string>(signal);
            deferreds.set(q, deferred);
            return deferred.promise;
          },
          [q],
          { cacheKey: `switch-${q}`, abortable: true },
        ),
      { initialProps: { q: "旧" } },
    );

    await waitFor(() => expect(deferreds.has("旧")).toBe(true));

    rerender({ q: "新" });
    await waitFor(() => expect(deferreds.has("新")).toBe(true));

    // 新请求先落定，旧请求随后才落定（且它在切换那一刻已被 abort）。
    act(() => deferreds.get("新")!.resolve("新结果"));
    await waitFor(() => expect(result.current.data).toBe("新结果"));

    act(() => deferreds.get("旧")!.resolve("旧结果"));
    // 给晚到的旧响应一个落定机会，确认它没把新结果盖掉。
    await act(async () => {
      await Promise.resolve();
    });
    expect(result.current.data).toBe("新结果");
  });
});

describe("useAsync · 默认模式（不可中止）", () => {
  it("fetcher 收到 undefined signal，cancel() 是空操作", async () => {
    const signals: Array<AbortSignal | undefined> = [];
    const fetcher = vi.fn((signal?: AbortSignal) => {
      signals.push(signal);
      return Promise.resolve("ok");
    });
    const { result } = renderHook(() => useAsync(fetcher, [], { cacheKey: "plain-mode" }));

    await waitFor(() => expect(result.current.data).toBe("ok"));
    expect(signals[0]).toBeUndefined();

    act(() => result.current.cancel());

    expect(result.current.cancelled).toBe(false);
    expect(result.current.data).toBe("ok");
  });

  it("旧响应晚返回也不覆盖新数据（active 标志兜底，不依赖 abort）", async () => {
    const deferreds = new Map<string, ReturnType<typeof makeDeferred<string>>>();
    const { result, rerender } = renderHook(
      ({ q }: { q: string }) =>
        useAsync(
          () => {
            const deferred = makeDeferred<string>();
            deferreds.set(q, deferred);
            return deferred.promise;
          },
          [q],
          { cacheKey: `plain-${q}` },
        ),
      { initialProps: { q: "旧" } },
    );

    await waitFor(() => expect(deferreds.has("旧")).toBe(true));
    rerender({ q: "新" });
    await waitFor(() => expect(deferreds.has("新")).toBe(true));

    act(() => deferreds.get("新")!.resolve("新结果"));
    await waitFor(() => expect(result.current.data).toBe("新结果"));

    act(() => deferreds.get("旧")!.resolve("旧结果"));
    await act(async () => {
      await Promise.resolve();
    });
    expect(result.current.data).toBe("新结果");
  });
});

// 身份边界：keepPreviousData 只能在同一取数主体内保留旧数据。
// 组件 state 不受 clearAsyncCache() 管辖，身份一变就必须当场作废，
// 否则上一主体（已登录）的私有结果会在新主体（匿名）下继续展示——
// 新请求失败或被取消时更是永远不会被替换掉。
describe("useAsync · keepPreviousData 的身份边界", () => {
  it("身份不变时跨键切换仍保留旧数据（既有语义不变）", async () => {
    const deferreds = new Map<string, ReturnType<typeof makeDeferred<string>>>();
    const { result, rerender } = renderHook(
      ({ q }: { q: string }) =>
        useAsync(
          (signal) => {
            const deferred = makeDeferred<string>(signal);
            deferreds.set(q, deferred);
            return deferred.promise;
          },
          [q],
          { cacheKey: `same-identity-${q}`, keepPreviousData: true, identityKey: "user:1" },
        ),
      { initialProps: { q: "旧" } },
    );

    await waitFor(() => expect(deferreds.has("旧")).toBe(true));
    act(() => deferreds.get("旧")!.resolve("旧结果"));
    await waitFor(() => expect(result.current.data).toBe("旧结果"));

    rerender({ q: "新" });
    await waitFor(() => expect(deferreds.has("新")).toBe(true));
    // 同一身份的换口径：旧内容留在原位，只置后台刷新态，不退回首载骨架。
    expect(result.current.data).toBe("旧结果");
    expect(result.current.refreshing).toBe(true);
    expect(result.current.loading).toBe(false);

    act(() => deferreds.get("新")!.resolve("新结果"));
    await waitFor(() => expect(result.current.data).toBe("新结果"));
  });

  it("身份变化时立即丢弃上一主体的数据，且新请求失败也不回填", async () => {
    const deferreds = new Map<string, ReturnType<typeof makeDeferred<string>>>();
    const { result, rerender } = renderHook(
      ({ who }: { who: string }) =>
        useAsync(
          (signal) => {
            const deferred = makeDeferred<string>(signal);
            deferreds.set(who, deferred);
            return deferred.promise;
          },
          [who],
          {
            cacheKey: `per-identity-${who}`,
            keepPreviousData: true,
            abortable: true,
            identityKey: who,
          },
        ),
      { initialProps: { who: "user:1" } },
    );

    await waitFor(() => expect(deferreds.has("user:1")).toBe(true));
    act(() => deferreds.get("user:1")!.resolve("私有仓库结果"));
    await waitFor(() => expect(result.current.data).toBe("私有仓库结果"));

    // 会话失效：user:1 → anon。旧主体的数据必须当场消失，不能等匿名请求落定。
    rerender({ who: "anon" });
    expect(result.current.data).toBeNull();
    expect(result.current.loading).toBe(true);
    expect(result.current.refreshing).toBe(false);

    // 匿名请求失败：不得把上一主体的结果又放回界面。
    act(() => deferreds.get("anon")!.reject(new ApiError("internal_error", "搜索不可用", 500)));
    await waitFor(() => expect(result.current.error?.code).toBe("internal_error"));
    expect(result.current.data).toBeNull();
    expect(result.current.refreshError).toBeNull();
  });

  it("身份变化后取消新请求：旧主体数据同样不得残留", async () => {
    const deferreds = new Map<string, ReturnType<typeof makeDeferred<string>>>();
    const { result, rerender } = renderHook(
      ({ who }: { who: string }) =>
        useAsync(
          (signal) => {
            const deferred = makeDeferred<string>(signal);
            deferreds.set(who, deferred);
            return deferred.promise;
          },
          [who],
          {
            cacheKey: `cancel-per-identity-${who}`,
            keepPreviousData: true,
            abortable: true,
            identityKey: who,
          },
        ),
      { initialProps: { who: "user:1" } },
    );

    await waitFor(() => expect(deferreds.has("user:1")).toBe(true));
    act(() => deferreds.get("user:1")!.resolve("私有仓库结果"));
    await waitFor(() => expect(result.current.data).toBe("私有仓库结果"));

    rerender({ who: "anon" });
    expect(result.current.data).toBeNull();

    act(() => result.current.cancel());
    await waitFor(() => expect(result.current.cancelled).toBe(true));
    // 取消是静默空闲态：既没有错误，也没有把旧主体的结果放回来。
    expect(result.current.error).toBeNull();
    expect(result.current.refreshError).toBeNull();
    expect(result.current.data).toBeNull();
  });

  it("身份变化时不回放缓存（缓存键没编入身份也挡得住）", async () => {
    const deferreds = new Map<string, ReturnType<typeof makeDeferred<string>>>();
    const { result, rerender } = renderHook(
      ({ who }: { who: string }) =>
        useAsync(
          (signal) => {
            const deferred = makeDeferred<string>(signal);
            deferreds.set(who, deferred);
            return deferred.promise;
          },
          [],
          {
            // 故意让两个身份共用同一条缓存：真实调用点应把身份编进键里，
            // 但钩子自己也得挡住跨身份回放，不能只依赖调用方自律。
            cacheKey: "shared-cache-key",
            keepPreviousData: true,
            abortable: true,
            identityKey: who,
          },
        ),
      { initialProps: { who: "user:1" } },
    );

    await waitFor(() => expect(deferreds.has("user:1")).toBe(true));
    act(() => deferreds.get("user:1")!.resolve("私有仓库结果"));
    await waitFor(() => expect(result.current.data).toBe("私有仓库结果"));

    rerender({ who: "anon" });
    // 换身份后即便同键有缓存，也不得回放给新主体；新请求仍在飞，界面应停在首载态。
    expect(result.current.data).toBeNull();
    expect(result.current.loading).toBe(true);
  });
});
