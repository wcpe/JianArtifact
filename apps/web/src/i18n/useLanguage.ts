// 当前界面语言与切换入口。语言策略（优先级、缓存、locale 标记）在 src/i18n/language.ts，
// 这里只负责把它接到 React：跟随路由重算默认语言、切换时同步 i18next 与 <html lang>。
import { useCallback, useEffect, useState } from "react";
import { useLocation } from "react-router-dom";

import i18n from "./index";
import {
  readStoredLanguage,
  resolveLanguage,
  toLocaleTag,
  writeStoredLanguage,
  type Language,
} from "./language";

/** 把语言同步到 i18next 与 <html lang>（读屏与浏览器翻译机制依赖后者）。 */
function applyLanguage(language: Language): void {
  if (i18n.language !== language) {
    void i18n.changeLanguage(language);
  }
  if (typeof document !== "undefined") {
    document.documentElement.lang = language === "en" ? "en" : "zh-CN";
  }
}

export interface LanguageControl {
  /** 当前界面语言。 */
  language: Language;
  /** 用户是否显式选择过语言（显式选择后不再跟随路由默认）。 */
  explicit: boolean;
  /** BCP-47 标记，供 toLocaleString / Intl.* / Mantine 日期组件使用。 */
  localeTag: string;
  /** 用户在页眉显式切换语言（写缓存、即时生效）。 */
  setLanguage: (language: Language) => void;
}

/**
 * 语言控制 hook。
 * 初始值由语言策略解析（用户偏好 > 公开页的浏览器语言 > 管理页默认中文）；
 * 路由变化时，只要用户没有显式选择过，就按新路径重算默认语言。
 */
export function useLanguage(): LanguageControl {
  const { pathname } = useLocation();
  const [explicit, setExplicit] = useState(() => readStoredLanguage() !== null);
  const [language, setLanguageState] = useState<Language>(() => resolveLanguage({ pathname }));

  // 用户未显式选择时，随路由重算（例如从公开的 /repositories 走到管理页 /dashboard）。
  useEffect(() => {
    if (explicit) return;
    setLanguageState(resolveLanguage({ pathname }));
  }, [pathname, explicit]);

  // 任何来源的语言变化都同步到 i18next 与 <html lang>。
  useEffect(() => {
    applyLanguage(language);
  }, [language]);

  const setLanguage = useCallback((next: Language) => {
    writeStoredLanguage(next);
    // 同步切换 i18next，避免「状态已变、格式还停在旧 locale」的中间帧。
    applyLanguage(next);
    setExplicit(true);
    setLanguageState(next);
  }, []);

  return { language, explicit, localeTag: toLocaleTag(language), setLanguage };
}
