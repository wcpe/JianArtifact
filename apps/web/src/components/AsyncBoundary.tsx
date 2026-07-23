// 异步边界：统一把 useAsync 的加载 / 错误 / 越权状态渲染为共享状态态组件，
// 数据就绪后交由 children 渲染。避免各列表页重复分支样板。
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
  if (state.loading || state.data === null) {
    return <LoadingState message={t("common.loading")} />;
  }
  return <>{children(state.data)}</>;
}
