// 异步边界：统一把 useAsync 的加载 / 错误 / 越权状态渲染为共享状态态组件，
// 数据就绪后交由 children 渲染。避免各列表页重复分支样板。
// FR-69: 刷新（reload / 翻页 / 排序）时保留旧数据并叠加局部 LoadingOverlay，
// 仅首载（尚无数据）才显示整块骨架，消灭"整页重刷"。
// v0.8.0：后台刷新失败（refreshError）时在数据上方渲染非阻断黄色警告条，
// 不再静默吞掉——"页面卡住不刷新"的反馈缺失由此修复。
import { Alert, Box, Button, LoadingOverlay, Skeleton, Stack, VisuallyHidden } from "@mantine/core";
import { ErrorState, ForbiddenState } from "@jianartifact/ui";
import { IconAlertTriangle, IconRefresh } from "@tabler/icons-react";
import type { CSSProperties, ReactNode } from "react";
import { useTranslation } from "react-i18next";

import type { AsyncState } from "../hooks/useAsync";

interface AsyncBoundaryProps<T> {
  state: AsyncState<T>;
  children: (data: T) => ReactNode;
  style?: CSSProperties;
  /**
   * 首载骨架：与真实内容同构的占位。不传则用通用的等高行骨架。
   * 原则是**不要退回居中转圈**——慢接口下只有转圈会让用户以为页面没切过来。
   */
  skeleton?: ReactNode;
}

/**
 * 通用首载骨架：若干等高行，贴合列表 / 表格页的常见形态。
 *
 * 刻意保留 `data-testid="state-loading"` 与一段读屏文案：骨架在视觉上替代了居中转圈，
 * 表达的仍是「内容在加载」这一状态——测试以此为加载态锚点；读屏用户也需要真实文案，
 * 否则纯骨架对他们就是一片空白。
 */
export function ContentSkeleton({ rows = 6 }: { rows?: number }) {
  const { t } = useTranslation();
  return (
    <Stack gap="xs" data-testid="state-loading" aria-busy="true">
      <VisuallyHidden>{t("common.loading")}</VisuallyHidden>
      {Array.from({ length: rows }).map((_, index) => (
        <Skeleton key={index} height={40} radius="sm" />
      ))}
    </Stack>
  );
}

/** 依据 async 状态渲染占位或数据视图。 */
export function AsyncBoundary<T>({ state, children, style, skeleton }: AsyncBoundaryProps<T>) {
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
  // 首载：还没有任何数据可展示。渲染**结构化骨架**而不是居中转圈——慢接口下
  // 用户能立刻看到「页面已经切过来了、数据在加载」，而不是只剩一块空白加一个小圈。
  if (state.data === null) {
    return <>{skeleton ?? <ContentSkeleton />}</>;
  }
  return (
    // flex: 1 / minHeight: 0：让内容区撑满页面剩余高度（页面外壳是 flex column），
    // 否则内部列表的 `flex: 1` 在这一层断掉，表格只占内容高度、下方留一大片空白。
    <Stack gap="xs" style={{ flex: 1, minHeight: 0 }}>
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
            <Button
              size="xs"
              variant="light"
              color="yellow"
              w="fit-content"
              leftSection={<IconRefresh size={14} />}
              onClick={state.reload}
            >
              {t("common.retry")}
            </Button>
          </Stack>
        </Alert>
      ) : null}
      {/* 这一层必须同时是 flex 容器：`flex: 1` 只在 flex 父容器里生效，若这里仍是块级布局，
          子内容自己写的 `flex: 1` 会失效，列表就撑不满剩余高度、下方留一大片空白。 */}
      <Box
        pos="relative"
        style={{ display: "flex", flexDirection: "column", flex: 1, minHeight: 0, ...style }}
      >
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
