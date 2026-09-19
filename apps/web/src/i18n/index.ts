// i18next 初始化：中英双语资源，初始语言由 src/i18n/language.ts 的语言策略解析。
// 通过 initReactI18next 接入 React；组件用 useTranslation() 读取文案。
import i18n from "i18next";
import { initReactI18next } from "react-i18next";

import { zh } from "./zh";
import { en } from "./en";
import { resolveLanguage, type Language } from "./language";

/**
 * 初始化语言：尊重用户的显式选择，否则按当前路径的默认语言
 * （公开页按浏览器语言、管理页与 /setup 默认中文）。
 */
export function initialLanguage(pathname = window.location.pathname): Language {
  return resolveLanguage({ pathname });
}

const initial = initialLanguage();

void i18n.use(initReactI18next).init({
  resources: {
    zh: { translation: zh },
    en: { translation: en },
  },
  lng: initial,
  fallbackLng: "zh",
  interpolation: { escapeValue: false },
});

// 让读屏与浏览器翻译机制拿到正确语言（index.html 里静态写的是 zh-CN）。
if (typeof document !== "undefined") {
  document.documentElement.lang = initial === "en" ? "en" : "zh-CN";
}

export default i18n;
