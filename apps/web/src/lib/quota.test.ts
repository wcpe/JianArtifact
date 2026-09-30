// FR-41 配额口径单测：状态阈值（≥90% 接近上限、≥100% 已超限）与「带单位输入」换算。
// 这两件事被列表、详情页配置区、页头徽章三处共用，口径漂移会让同一仓库在不同位置显示
// 不同状态，故单独锁住。
import { describe, expect, it } from "vitest";

import {
  QUOTA_NEAR_RATIO,
  hasQuotaLimit,
  parseNonNegativeInt,
  parseQuotaBytes,
  quotaState,
  repoQuotaState,
  splitQuotaBytes,
} from "./quota";

describe("配额状态判定", () => {
  it("未设上限（0 / 缺省 / null）恒为不限", () => {
    expect(quotaState(123, 0)).toBe("unlimited");
    expect(quotaState(123, undefined)).toBe("unlimited");
    expect(quotaState(123, null)).toBe("unlimited");
  });

  it("阈值：<90% 正常，≥90% 接近上限，≥100% 已超限", () => {
    expect(QUOTA_NEAR_RATIO).toBe(0.9);
    expect(quotaState(89, 100)).toBe("ok");
    expect(quotaState(90, 100)).toBe("near");
    expect(quotaState(99, 100)).toBe("near");
    expect(quotaState(100, 100)).toBe("over");
    expect(quotaState(101, 100)).toBe("over");
  });

  it("用量缺失按 0 处理（缺统计不误报超限）", () => {
    expect(quotaState(undefined, 100)).toBe("ok");
    expect(quotaState(null, 100)).toBe("ok");
  });

  it("仓库级状态取两个维度里更严重的一个", () => {
    expect(
      repoQuotaState({ artifactCount: 1, totalSize: 1, quotaAssets: 100, quotaBytes: 100 }),
    ).toBe("ok");
    expect(repoQuotaState({ artifactCount: 95, totalSize: 1, quotaAssets: 100 })).toBe("near");
    expect(
      repoQuotaState({ artifactCount: 95, totalSize: 100, quotaAssets: 100, quotaBytes: 100 }),
    ).toBe("over");
    // 两个维度都不限 → 不限（不会因为「用量 > 0」被误判成正常/超限）。
    expect(repoQuotaState({ artifactCount: 10, totalSize: 10 })).toBe("unlimited");
  });
});

describe("配额输入的带单位换算", () => {
  it("字节 → 输入值 + 单位：能紧凑表达就用 GB / MB，否则退回字节", () => {
    expect(splitQuotaBytes(0)).toEqual({ value: "0", unit: "GB" });
    expect(splitQuotaBytes(10 * 1024 ** 3)).toEqual({ value: "10", unit: "GB" });
    expect(splitQuotaBytes(8.5 * 1024 ** 3)).toEqual({ value: "8.5", unit: "GB" });
    expect(splitQuotaBytes(512 * 1024 ** 2)).toEqual({ value: "512", unit: "MB" });
    // 8 GiB + 1 B：GB/MB 都表达不了 → 原样按字节回显，避免回显取整后悄悄改掉上限。
    expect(splitQuotaBytes(8 * 1024 ** 3 + 1)).toEqual({
      value: String(8 * 1024 ** 3 + 1),
      unit: "B",
    });
  });

  it("输入值 + 单位 → 字节；空串按不限（0）处理", () => {
    expect(parseQuotaBytes("2", "GB")).toBe(2147483648);
    expect(parseQuotaBytes("1.5", "GB")).toBe(1610612736);
    expect(parseQuotaBytes("512", "MB")).toBe(536870912);
    expect(parseQuotaBytes("1024", "B")).toBe(1024);
    expect(parseQuotaBytes("", "GB")).toBe(0);
    expect(parseQuotaBytes("   ", "GB")).toBe(0);
    expect(parseQuotaBytes("0", "GB")).toBe(0);
  });

  it("非法输入返回 null（负数 / 非数字），由调用方拦截保存", () => {
    expect(parseQuotaBytes("-1", "GB")).toBeNull();
    expect(parseQuotaBytes("abc", "MB")).toBeNull();
    expect(parseNonNegativeInt("-5")).toBeNull();
    expect(parseNonNegativeInt("abc")).toBeNull();
  });

  it("非负整数输入（制品数上限 / 保留天数共用）：空 = 0，小数取整", () => {
    expect(parseNonNegativeInt("")).toBe(0);
    expect(parseNonNegativeInt("2000")).toBe(2000);
    expect(parseNonNegativeInt("2.4")).toBe(2);
    expect(parseNonNegativeInt("30")).toBe(30);
    expect(parseNonNegativeInt("0")).toBe(0);
  });

  it("回显与保存互为逆运算（读取既有上限后原样保存不改变数值）", () => {
    for (const bytes of [10737418240, 2147483648, 9126805504, 8589934593, 1048576, 1]) {
      const { value, unit } = splitQuotaBytes(bytes);
      expect(parseQuotaBytes(value, unit)).toBe(bytes);
    }
  });

  it("hasQuotaLimit：0 / 缺省 / null 都不算设了上限", () => {
    expect(hasQuotaLimit(0)).toBe(false);
    expect(hasQuotaLimit(undefined)).toBe(false);
    expect(hasQuotaLimit(null)).toBe(false);
    expect(hasQuotaLimit(1)).toBe(true);
  });
});
