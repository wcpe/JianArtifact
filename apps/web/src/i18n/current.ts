// 非 React 上下文（纯格式化函数）读取当前语言与 locale 标记的唯一入口。
//
// 格式化函数（formatCount / formatStamp / 审计时间等）不是组件、拿不到 hook，直接读
// i18next 实例的当前语言即可——这样切语言后它们自动跟随，也就不会再出现
// 「模块级 Intl.DateTimeFormat 常量在切语言后仍返回旧格式」这类隐蔽分叉。
import i18n from "./index";
import { toLocaleTag, type Language } from "./language";

/** 当前界面语言（i18next 实例的解析结果）。 */
export function currentLanguage(): Language {
  return i18n.resolvedLanguage === "en" ? "en" : "zh";
}

/** 当前语言的 BCP-47 标记。 */
export function currentLocaleTag(): string {
  return toLocaleTag(currentLanguage());
}

/**
 * 按当前语言取格式化器（同语言 + 同选项复用同一个实例）。
 * Intl 构造器开销不小，而格式化调用在列表行里按行触发，故按 `语言|选项` 缓存。
 */
export function localizedFormatter(
  create: (locale: string) => Intl.DateTimeFormat | Intl.NumberFormat,
  pattern: string,
): Intl.DateTimeFormat | Intl.NumberFormat {
  const cacheKey = `${currentLanguage()}|${pattern}`;
  let formatter = formatterCache.get(cacheKey);
  if (!formatter) {
    formatter = create(currentLocaleTag());
    formatterCache.set(cacheKey, formatter);
  }
  return formatter;
}

const formatterCache = new Map<string, Intl.DateTimeFormat | Intl.NumberFormat>();
