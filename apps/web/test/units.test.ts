import { describe, expect, it } from "vitest";

import { bytesToBytes, durationToSeconds, splitBytes, splitSeconds } from "../src/lib/units";

// 拆分的核心要求是「换回去必须还是原值」：界面把 86400 显示成 1 天，
// 用户不改动就保存时，提交值必须仍是 86400，不能浮成 86400.0000001 或跌成 86399。
describe("时长单位换算与拆分", () => {
  it("换算回秒", () => {
    expect(durationToSeconds(1, "seconds")).toBe(1);
    expect(durationToSeconds(5, "minutes")).toBe(300);
    expect(durationToSeconds(2, "hours")).toBe(7200);
    expect(durationToSeconds(1, "days")).toBe(86400);
  });

  it("非法输入按 0 处理，不产生 NaN 提交", () => {
    expect(durationToSeconds("", "seconds")).toBe(0);
    expect(durationToSeconds(-5, "seconds")).toBe(0);
    expect(durationToSeconds(Number.NaN, "days")).toBe(0);
  });

  it("拆成最自然的单位", () => {
    expect(splitSeconds(86400)).toEqual({ value: 1, unit: "days" });
    expect(splitSeconds(3600)).toEqual({ value: 1, unit: "hours" });
    expect(splitSeconds(1800)).toEqual({ value: 30, unit: "minutes" });
    expect(splitSeconds(90)).toEqual({ value: 90, unit: "seconds" });
    expect(splitSeconds(0)).toEqual({ value: 0, unit: "seconds" });
  });

  it("拆分后换回秒等于原值（四舍五入不得丢精度）", () => {
    for (const total of [1, 59, 60, 90, 3600, 7200, 86400, 604800, 2592000]) {
      const { value, unit } = splitSeconds(total);
      expect(durationToSeconds(value, unit)).toBe(total);
    }
  });

  // 不是整单位的秒数不能被"凑整"成整单位，否则 90 秒会显示成 1.5 分钟、
  // 再保存时若用户没动就变成 90（尚可）或 60（丢值）。
  it("非整单位不强行进位", () => {
    expect(splitSeconds(90).unit).toBe("seconds");
    expect(splitSeconds(3661).unit).toBe("seconds");
  });
});

describe("容量单位换算与拆分", () => {
  it("换算回字节", () => {
    expect(bytesToBytes(1, "mb")).toBe(1024 ** 2);
    expect(bytesToBytes(2, "gb")).toBe(2 * 1024 ** 3);
    expect(bytesToBytes(1, "tb")).toBe(1024 ** 4);
  });

  it("回收站默认 2 GiB 显示为 2 GB", () => {
    expect(splitBytes(2 * 1024 ** 3)).toEqual({ value: 2, unit: "gb" });
  });

  it("1 TiB 显示为 1 TB", () => {
    expect(splitBytes(1024 ** 4)).toEqual({ value: 1, unit: "tb" });
  });

  it("拆分后换回字节等于原值", () => {
    for (const total of [1, 1024 ** 2, 2 * 1024 ** 3, 1024 ** 4, 5 * 1024 ** 3]) {
      const { value, unit } = splitBytes(total);
      expect(bytesToBytes(value, unit)).toBe(total);
    }
  });
});
