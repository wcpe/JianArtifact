// Mantine 7 主题：主色沿用旧项目原生蓝（blue），供管理端与验收站共用。
// 组件样式表 `@mantine/core/styles.css` 由各应用入口（main.tsx）导入，不在此耦合。
import { createTheme } from "@mantine/core";

import { tokens } from "./tokens";

/** 全局共享主题：主色 blue（对齐旧项目原生蓝），圆角对齐设计令牌。 */
export const theme = createTheme({
  primaryColor: "blue",
  defaultRadius: "md",
  fontFamily:
    'system-ui, -apple-system, "Segoe UI", Roboto, "PingFang SC", "Microsoft YaHei", sans-serif',
  headings: { fontWeight: "600" },
  other: {
    brandColor: tokens.brandColor,
  },
});
