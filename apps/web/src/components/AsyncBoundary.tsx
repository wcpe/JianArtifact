// 异步边界：统一把 useAsync 的加载 / 错误 / 越权状态渲染为共享状态态组件，
// 数据就绪后交由 children 渲染。避免各列表页重复分支样板。
// FR-69: 刷新（reload / 翻页 / 排序）时保留旧数据并叠加局部 LoadingOverlay，
// 仅首载（尚无数据）才显示整块骨架，消灭"整页重刷"。
import { Box, LoadingOverlay } from "@mantine/core";
import { ErrorState, ForbiddenState, LoadingState } from "@jianartifact/ui";
import type { ReactNode } from "react";
import { useTranslation } from "react-i18next";

import type { AsyncState } from "../hooks/useAsync";

interface AsyncBoundaryProps<T> {
  state: AsyncState<T>;
  children: (data: T) => ReactNode;
}

/** 依据 async 状态渲染占位或数据视图。 */
export function AsyncBoundary<T>({ state, children }: AsyncBoundaryProps<T>) {
  const { t } = useTranslation();

  if (state.forbidden) {
    return <ForbiddenState message={t("common.forbidden")} />;
  }
  if (state.error) {
    return (
      <ErrorState
        message={t("common.error")}
        description={state.error.message}
        onRetry={state.reload}
        retryLabel={t("common.retry")}
      />
    );
  }
  // 首载：还没有任何数据可展示，整块骨架
  if (state.data === null) {
    return <LoadingState message={t("common.loading")} />;
  }
  // 已有数据：刷新期间保留旧内容，仅叠加半透明覆盖层
  return (
    <Box pos="relative">
      <LoadingOverlay
        visible={state.loading}
        zIndex={10}
        overlayProps={{ radius: "sm", blur: 1 }}
        loaderProps={{ size: "sm" }}
        transitionProps={{ duration: 150 }}
      />
      {children(state.data)}
    </Box>
  );
}
