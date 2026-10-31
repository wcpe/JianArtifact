// 数值 + 单位的换算与拆分，供设置页的时长 / 容量字段复用。
//
// 设计约束（重要）：
//   - 提交给后端的**永远是基础单位**（秒 / 字节），不是「数值 + 单位」的组合。
//     后端的设置契约就是秒与字节，界面上换算是纯展示层的便利，
//     在这里引入第二套真源会让「界面显示 1 天、库里存 86400」迟早对不上。
//   - 因此拆分只为回填输入框，换算是可逆的；不参与任何持久化格式。

/** 时长单位：秒数换算系数与 i18n 键。 */
export const DURATION_UNITS = [
  { value: "seconds", seconds: 1, labelKey: "settings.unitSeconds" },
  { value: "minutes", seconds: 60, labelKey: "settings.unitMinutes" },
  { value: "hours", seconds: 3600, labelKey: "settings.unitHours" },
  { value: "days", seconds: 86400, labelKey: "settings.unitDays" },
] as const;

/** 容量单位：字节换算系数与 i18n 键。 */
export const BYTE_UNITS = [
  // 字节作为兜底单位存在：不是整 MB 的值只能用字节精确表示，
  // 少了它就只能显示带小数的 MB，而那一项是要落库的容量上限，不能丢精度。
  { value: "bytes", bytes: 1, labelKey: "settings.unitBytes" },
  { value: "mb", bytes: 1024 ** 2, labelKey: "settings.unitMegabytes" },
  { value: "gb", bytes: 1024 ** 3, labelKey: "settings.unitGigabytes" },
  { value: "tb", bytes: 1024 ** 4, labelKey: "settings.unitTerabytes" },
] as const;

export type DurationUnit = (typeof DURATION_UNITS)[number]["value"];
export type ByteUnit = (typeof BYTE_UNITS)[number]["value"];

/** 把「数值 + 时长单位」换算为秒。非法输入返回 0。 */
export function durationToSeconds(value: number | string, unit: DurationUnit): number {
  const n = typeof value === "number" ? value : Number(value);
  if (!Number.isFinite(n) || n < 0) return 0;
  const found = DURATION_UNITS.find((it) => it.value === unit);
  return Math.trunc(n) * (found ? found.seconds : 1);
}

/** 把「数值 + 容量单位」换算为字节。非法输入返回 0。 */
export function bytesToBytes(value: number | string, unit: ByteUnit): number {
  const n = typeof value === "number" ? value : Number(value);
  if (!Number.isFinite(n) || n < 0) return 0;
  const found = BYTE_UNITS.find((it) => it.value === unit);
  return Math.trunc(n) * (found ? found.bytes : 1);
}

/**
 * 为回填挑选「读起来最自然」的单位：取能整除该值的最大单位，且商不小于 1。
 * 86400 秒 → 1 天；3600 秒 → 1 小时；90 秒 → 90 秒。
 *
 * 为什么不固定用秒：作业间隔默认 86400 秒，让运维面对「86400」再自己心算，
 * 正是这类界面最主要的不好用之处。也不能一律用最大单位——30 秒会变成 0.0003 天。
 */
export function splitSeconds(total: number): { value: number; unit: DurationUnit } {
  const safe = Number.isFinite(total) && total > 0 ? Math.trunc(total) : 0;
  if (safe === 0) return { value: 0, unit: "seconds" };
  // 从大到小找第一个能整除且商 ≥ 1 的单位（跳过「秒」，它是兜底）。
  for (const candidate of [...DURATION_UNITS].reverse()) {
    if (candidate.value === "seconds") continue;
    if (safe % candidate.seconds === 0 && safe / candidate.seconds >= 1) {
      return { value: safe / candidate.seconds, unit: candidate.value };
    }
  }
  return { value: safe, unit: "seconds" };
}

/** 字节数的自然单位拆分，规则同 splitSeconds。 */
export function splitBytes(total: number): { value: number; unit: ByteUnit } {
  const safe = Number.isFinite(total) && total > 0 ? Math.trunc(total) : 0;
  if (safe === 0) return { value: 0, unit: "mb" };
  for (const candidate of [...BYTE_UNITS].reverse()) {
    // 字节是兜底单位，不参与「取最大整单位」的挑选。
    if (candidate.value === "bytes") continue;
    if (safe % candidate.bytes === 0 && safe / candidate.bytes >= 1) {
      return { value: safe / candidate.bytes, unit: candidate.value };
    }
  }
  // 不足 1 MB 或不是整 MB：带小数的 MB 会丢精度（换回去不再是原字节），
  // 而这一项是要落库的容量上限，宁可显示成原始字节数也不能悄悄改掉用户设的值。
  return { value: safe, unit: "bytes" };
}
