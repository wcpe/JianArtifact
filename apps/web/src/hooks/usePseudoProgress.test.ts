// 伪进度曲线与计时钩子（FR-152）：
// - 前段 3.2s 缓出爬到 88%，末段 0.7s 冲刺到 100%，之后保持；
// - 全程单调递增、缓出段不停顿（逐 200ms 至少 1 个百分点），避免「停在接近完成」的假卡死；
// - 阶段随进度阈值切换；已用时长按采样间隔累加；
// - 卸载即清除定时器（不留 act 告警与泄漏）。
import { act, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import {
  CRAWL_MS,
  CRAWL_TARGET,
  SPRINT_MS,
  TICK_MS,
  pseudoProgressValue,
  stageForProgress,
  usePseudoProgress,
} from "./usePseudoProgress";

describe("伪进度曲线 pseudoProgressValue", () => {
  it("起点为 0，3.2s 恰好到缓出终点 88", () => {
    expect(pseudoProgressValue(0)).toBe(0);
    expect(pseudoProgressValue(CRAWL_MS)).toBe(CRAWL_TARGET);
  });

  it("末段 0.7s 内冲到 100，到点前仍未满", () => {
    expect(pseudoProgressValue(CRAWL_MS + SPRINT_MS - TICK_MS)).toBeLessThan(100);
    expect(pseudoProgressValue(CRAWL_MS + SPRINT_MS)).toBe(100);
    // 之后再等也不会超过 100（请求慢时停在满格，不出现回退或溢出）。
    expect(pseudoProgressValue(60_000)).toBe(100);
  });

  it("全区间单调不回退", () => {
    let previous = 0;
    for (let elapsed = 0; elapsed <= CRAWL_MS + SPRINT_MS + 1_000; elapsed += TICK_MS) {
      const value = pseudoProgressValue(elapsed);
      expect(value).toBeGreaterThanOrEqual(previous);
      previous = value;
    }
    expect(previous).toBe(100);
  });

  it("缓出段持续在动：每 200ms 至少推进 1 个百分点", () => {
    for (let elapsed = 0; elapsed + 200 <= CRAWL_MS; elapsed += 200) {
      const delta = pseudoProgressValue(elapsed + 200) - pseudoProgressValue(elapsed);
      expect(delta).toBeGreaterThanOrEqual(1);
    }
  });

  it("非法入参按 0 处理", () => {
    expect(pseudoProgressValue(-1)).toBe(0);
    expect(pseudoProgressValue(Number.NaN)).toBe(0);
    expect(pseudoProgressValue(Number.POSITIVE_INFINITY)).toBe(100);
  });
});

describe("阶段阈值 stageForProgress", () => {
  it("按进度切到检索 / 筛选 / 排序 / 收尾四个阶段", () => {
    expect(stageForProgress(0)).toBe("indexing");
    expect(stageForProgress(44)).toBe("indexing");
    expect(stageForProgress(45)).toBe("filtering");
    expect(stageForProgress(69)).toBe("filtering");
    expect(stageForProgress(70)).toBe("ranking");
    expect(stageForProgress(89)).toBe("ranking");
    expect(stageForProgress(90)).toBe("finalizing");
    expect(stageForProgress(100)).toBe("finalizing");
  });
});

describe("usePseudoProgress 计时", () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it("按时间推进：3.2s 到 88%，再 0.7s 到 100%", () => {
    const { result } = renderHook(() => usePseudoProgress());
    expect(result.current.value).toBe(0);
    expect(result.current.elapsedMs).toBe(0);

    act(() => {
      vi.advanceTimersByTime(CRAWL_MS);
    });
    expect(result.current.value).toBe(CRAWL_TARGET);
    expect(result.current.elapsedMs).toBe(CRAWL_MS);

    act(() => {
      vi.advanceTimersByTime(SPRINT_MS);
    });
    expect(result.current.value).toBe(100);
  });

  it("已用时长实时累加（每 100ms 一档，供 UI 显示 1.8s 这类读数）", () => {
    const { result } = renderHook(() => usePseudoProgress());
    act(() => {
      vi.advanceTimersByTime(1_800);
    });
    expect(result.current.elapsedMs).toBe(1_800);
    expect((result.current.elapsedMs / 1000).toFixed(1)).toBe("1.8");
  });

  it("阶段文案随进度阈值切换", () => {
    const { result } = renderHook(() => usePseudoProgress());
    expect(result.current.stage).toBe("indexing");

    // 1.1s：进度约 46%，进入「筛选制品」。
    act(() => {
      vi.advanceTimersByTime(1_100);
    });
    expect(result.current.stage).toBe("filtering");

    // 2.3s：约 72%，进入「按相关度排序」。
    act(() => {
      vi.advanceTimersByTime(1_200);
    });
    expect(result.current.stage).toBe("ranking");

    // 3.4s：进入末段冲刺，约 90%，切到「即将完成」。
    act(() => {
      vi.advanceTimersByTime(1_100);
    });
    expect(result.current.stage).toBe("finalizing");
  });

  it("卸载时清除定时器，不再产生状态更新", () => {
    const clearIntervalSpy = vi.spyOn(window, "clearInterval");
    const { unmount } = renderHook(() => usePseudoProgress());
    expect(vi.getTimerCount()).toBe(1);

    unmount();

    expect(clearIntervalSpy).toHaveBeenCalledTimes(1);
    expect(vi.getTimerCount()).toBe(0);
    clearIntervalSpy.mockRestore();
  });
});
