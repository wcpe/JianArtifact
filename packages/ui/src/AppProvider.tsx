// 应用级 Mantine Provider：统一注入共享主题。
// 各应用在此之外自行挂载 Notifications 等按需增强，保持本包依赖精简。
import { MantineProvider } from "@mantine/core";
import type { ReactNode } from "react";

import { theme } from "./theme";

export interface AppProviderProps {
  children: ReactNode;
}

/** 包裹应用根，提供共享 Mantine 主题上下文。 */
export function AppProvider({ children }: AppProviderProps) {
  return (
    <MantineProvider theme={theme} defaultColorScheme="auto">
      {children}
    </MantineProvider>
  );
}
