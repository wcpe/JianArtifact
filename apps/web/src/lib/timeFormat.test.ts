// timeFormat 单测：时区无关断言策略（期望值一律用 Date 本地 getter 拼装），
// 保证任意宿主时区下 CI 稳定；另附宿主为 UTC+8 时的显式北京语义断言。
import { describe, expect, it } from "vitest";

import { formatUtcToLocal, formatUtcToLocalDate, parseUtc } from "./timeFormat";

const pad = (input: number) => String(input).padStart(2, "0");

/** 用浏览器本地时区把给定 UTC 时刻拼成 `YYYY-MM-DD HH:mm:ss`（与实现口径一致）。 */
function expectedLocal(utc: string): string {
  const date = new Date(utc);
  return (
    `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())} ` +
    `${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}`
  );
}

describe("parseUtc", () => {
  it("naive 字符串（无时区）按 UTC 解析：与带 Z 的同一时刻相等", () => {
    expect(parseUtc("2026-09-12 22:57:28")?.getTime()).toBe(
      new Date("2026-09-12T22:57:28Z").getTime(),
    );
  });

  it("naive 字符串允许 T 分隔与毫秒", () => {
    expect(parseUtc("2026-09-12T22:57:28.123")?.getTime()).toBe(
      new Date("2026-09-12T22:57:28.123Z").getTime(),
    );
  });

  it("已带时区的 RFC3339 按原样解析", () => {
    expect(parseUtc("2026-08-26T14:10:00Z")?.getTime()).toBe(
      new Date("2026-08-26T14:10:00Z").getTime(),
    );
    expect(parseUtc("2026-08-26T14:10:00+08:00")?.getTime()).toBe(
      new Date("2026-08-26T14:10:00+08:00").getTime(),
    );
  });

  it("空值与非法输入返回 null", () => {
    expect(parseUtc(null)).toBeNull();
    expect(parseUtc(undefined)).toBeNull();
    expect(parseUtc("")).toBeNull();
    expect(parseUtc("   ")).toBeNull();
    expect(parseUtc("not-a-date")).toBeNull();
  });
});

describe("formatUtcToLocal", () => {
  const UTC_INPUT = "2026-09-12 22:57:28";

  it("按浏览器本地时区渲染（期望值用本地 getter 拼装）", () => {
    expect(formatUtcToLocal(UTC_INPUT)).toBe(expectedLocal("2026-09-12T22:57:28Z"));
  });

  it("naive 输入与等价带 Z 输入渲染一致（证明按 UTC 解析而非本地）", () => {
    expect(formatUtcToLocal(UTC_INPUT)).toBe(formatUtcToLocal("2026-09-12T22:57:28Z"));
  });

  it("宿主为 UTC+8 时显示北京时间 2026-09-13 06:57:28（跨日）", () => {
    const offsetMinutes = -new Date("2026-09-12T22:57:28Z").getTimezoneOffset();
    if (offsetMinutes === 480) {
      expect(formatUtcToLocal(UTC_INPUT)).toBe("2026-09-13 06:57:28");
    } else {
      expect(formatUtcToLocal(UTC_INPUT)).toBe(expectedLocal("2026-09-12T22:57:28Z"));
    }
  });

  it("空值返回空串，非法输入原样返回", () => {
    expect(formatUtcToLocal(null)).toBe("");
    expect(formatUtcToLocal(undefined)).toBe("");
    expect(formatUtcToLocal("")).toBe("");
    expect(formatUtcToLocal("not-a-date")).toBe("not-a-date");
  });
});

describe("formatUtcToLocalDate", () => {
  it("按浏览器本地时区渲染日期（期望值用本地 getter 拼装）", () => {
    const date = new Date("2026-09-12T22:57:28Z");
    const expected = `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}`;
    expect(formatUtcToLocalDate("2026-09-12 22:57:28")).toBe(expected);
  });

  it("空值返回空串，非法输入原样返回", () => {
    expect(formatUtcToLocalDate("")).toBe("");
    expect(formatUtcToLocalDate(undefined)).toBe("");
    expect(formatUtcToLocalDate("bad")).toBe("bad");
  });
});
