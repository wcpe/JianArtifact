// i18next 初始化：当前内置中文（默认），架构预留多语言扩展位。
// 通过 initReactI18next 接入 React；组件用 useTranslation() 读取文案。
import i18n from "i18next";
import { initReactI18next } from "react-i18next";

import { zh } from "./zh";

void i18n.use(initReactI18next).init({
  resources: {
    zh: { translation: zh },
  },
  lng: "zh",
  fallbackLng: "zh",
  interpolation: { escapeValue: false },
});

export default i18n;
