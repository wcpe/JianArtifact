// i18n 资源守卫：`en` 与 `zh` 必须同命名空间、同键集、同插值占位符，且英文侧不留中文。
//
// 编译期已由 `en.ts` 的 `satisfies Resources` 拦住「缺键 / 多键」，本用例再复核值层面的
// 形态——占位符集合必须逐个键对应（否则英文文案会吃掉变量、渲染出残缺句子），空值与中文
// 残留也一并拦下。此前的守卫只比 `migrations` 一个命名空间，其余 23 个命名空间漏网。
import { describe, expect, it } from "vitest";

import { en } from "./en";
import { zh } from "./zh";

/** 递归展开为 `命名空间.键` → 值（嵌套对象按点号继续展开）。 */
function flatten(resource: Record<string, unknown>, prefix = ""): Record<string, string> {
  const out: Record<string, string> = {};
  for (const [key, value] of Object.entries(resource)) {
    const path = prefix ? `${prefix}.${key}` : key;
    if (value && typeof value === "object") {
      Object.assign(out, flatten(value as Record<string, unknown>, path));
    } else {
      out[path] = String(value);
    }
  }
  return out;
}

/** 提取 `{{占位符}}` 集合并排序，便于直接比较。 */
function placeholders(value: string): string[] {
  return (value.match(/\{\{[^}]+\}\}/g) ?? []).sort();
}

const zhFlat = flatten(zh);
const enFlat = flatten(en);
const zhKeys = Object.keys(zhFlat).sort();
const enKeys = Object.keys(enFlat).sort();

describe("i18n 资源完整性（zh / en）", () => {
  it("两种语言的命名空间键集完全一致", () => {
    expect(Object.keys(en).sort()).toEqual(Object.keys(zh).sort());
  });

  it("两种语言的全部叶子键完全一致（缺键与多余键都会失败）", () => {
    expect(enKeys).toEqual(zhKeys);
  });

  it("每个键的插值占位符集合一一对应", () => {
    const mismatched = zhKeys
      .filter((key) => placeholders(zhFlat[key]!).join() !== placeholders(enFlat[key]!).join())
      .map((key) => `${key}: zh[${placeholders(zhFlat[key]!).join()}] en[${placeholders(enFlat[key]!).join()}]`);
    expect(mismatched).toEqual([]);
  });

  it("英文资源不含中文，且两种语言都无空值", () => {
    const cjk = /[\u4e00-\u9fff]/;
    expect(Object.entries(enFlat).filter(([, value]) => cjk.test(value)).map(([key]) => key)).toEqual([]);
    expect(Object.entries(zhFlat).filter(([, value]) => value.trim() === "").map(([key]) => key)).toEqual([]);
    expect(Object.entries(enFlat).filter(([, value]) => value.trim() === "").map(([key]) => key)).toEqual([]);
  });

  it("键集规模符合预期（防止命名空间被整体误删）", () => {
    // 24 个命名空间、根级键数下限；数值随功能增补而上调，删键时这条会先失败。
    expect(Object.keys(zh).length).toBeGreaterThanOrEqual(24);
    expect(zhKeys.length).toBeGreaterThanOrEqual(1000);
  });
});
