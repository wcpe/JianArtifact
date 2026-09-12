// 异步边界：统一把 useAsync 的加载 / 错误 / 越权状态渲染为共享状态态组件，
// 数据就绪后交由 children 渲染。避免各列表页重复分支样板。
// FR-69: 刷新（reload / 翻页 / 排序）时保留旧数据并叠加局部 LoadingOverlay，
// 仅首载（尚无数据）才显示整块骨架，消灭"整页重刷"。
// v0.8.0：后台刷新失败（refreshError）时在数据上方渲染非阻断黄色警告条，
// 不再静默吞掉——"页面卡住不刷新"的反馈缺失由此修复。
import { Alert, Box, Button, LoadingOverlay, Stack } from "@mantine/core";
import { ErrorState, ForbiddenState, LoadingState } from "@jianartifact/ui";
import { IconAlertTriangle } from "@tabler/icons-react";
import type { CSSProperties, ReactNode } from "react";
import { useTranslation } from "react-i18next";

import type { AsyncState } from "../hooks/useAsync";

interface AsyncBoundaryProps<T> {
  state: AsyncState<T>;
  children: (data: T) => ReactNode;
  style?: CSSProperties;
}

/** 依据 async 状态渲染占位或数据视图。 */
export function AsyncBoundary<T>({ state, children, style }: AsyncBoundaryProps<T>) {
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
  return (
    <Stack gap="xs">
      {/* 后台刷新失败：旧数据仍在，非阻断警告，可一键重试 */}
      {state.refreshError ? (
        <Alert
          color="yellow"
          variant="light"
          icon={<IconAlertTriangle size={16} />}
          title={t("common.refreshFailedTitle", { defaultValue: "刷新失败" })}
        >
          <Stack gap="xs">
            <Box component="span" size="sm">
              {t("common.refreshFailedDescription", {
                defaultValue: "仍展示上次数据，可重试刷新。",
              })}
              {state.refreshError.message ? ` ${state.refreshError.message}` : ""}
            </Box>
            <Button size="xs" variant="light" color="yellow" w="fit-content" onClick={state.reload}>
              {t("common.retry")}
            </Button>
          </Stack>
        </Alert>
      ) : null}
      <Box pos="relative" style={style}>
        <LoadingOverlay
          visible={state.loading || state.refreshing}
          zIndex={10}
          overlayProps={{ radius: "sm", blur: 1 }}
          loaderProps={{ size: "sm" }}
          transitionProps={{ duration: 150 }}
        />
        {children(state.data)}
      </Box>
    </Stack>
  );
}
