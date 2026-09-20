// 应用级 Mantine Provider：统一注入共享主题。
// 各应用在此之外自行挂载 Notifications 等按需增强，保持本包依赖精简。
import { MantineProvider } from "@mantine/core";
import type { ReactNode } from "react";

import { theme } from "./theme";

export interface AppProviderProps {
  children: ReactNode;
}

/**
 * Mantine 8 起引入 `env` 机制：默认环境下浮层（Popover/Select/Dropdown 等）依赖真实浏览器
 * 的异步定位与过渡，在 jsdom 里会停在 `display:none`（表现为"下拉打不开"、选项查不到）。
 * 声明 `env="test"` 后 Mantine 走同步路径，测试环境才可用。
 * 仅测试环境设置，生产保持默认行为。
 */
function mantineEnv(): "test" | undefined {
  // 用类型断言读取：本包不引入 vite/client 类型。运行时若 import.meta.env 不存在
  // （非 Vite 环境）即返回 undefined，与生产默认行为一致。
  const mode = (import.meta as { env?: { MODE?: string } }).env?.MODE;
  return mode === "test" ? "test" : undefined;
}

/** 包裹应用根，提供共享 Mantine 主题上下文。 */
export function AppProvider({ children }: AppProviderProps) {
  return (
    <MantineProvider theme={theme} defaultColorScheme="auto" env={mantineEnv()}>
      {children}
    </MantineProvider>
  );
}
