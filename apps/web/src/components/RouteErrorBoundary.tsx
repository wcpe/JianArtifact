// 路由级错误边界：兜住渲染期异常，尤其是 React.lazy 的动态 import 失败。
//
// 没有它时，一次 chunk 加载失败会让整棵组件树崩掉 → **白屏**，用户只能干瞪眼：
// - 生产发版后，停留在旧页面的用户点导航请求已被删除的旧 chunk（404）；
// - 网络抖动导致脚本下载中断；
// - dev server 重新预构建依赖期间的 `504 Outdated Optimize Dep`。
//
// 这里给出明确提示和一个「重新加载」按钮。挂载时由调用方用 `key={pathname}` 强制重建，
// 否则一次失败会把错误态带到后续所有页面。
import { Component } from "react";
import type { ErrorInfo, ReactNode } from "react";
import { Button, Center, Stack, Text, Title } from "@mantine/core";
import { IconRefresh } from "@tabler/icons-react";
import { t as translate } from "i18next";

interface RouteErrorBoundaryProps {
  children: ReactNode;
}

interface RouteErrorBoundaryState {
  message: string | null;
}

export class RouteErrorBoundary extends Component<
  RouteErrorBoundaryProps,
  RouteErrorBoundaryState
> {
  state: RouteErrorBoundaryState = { message: null };

  static getDerivedStateFromError(error: unknown): RouteErrorBoundaryState {
    return { message: error instanceof Error ? error.message : String(error) };
  }

  componentDidCatch(error: Error, info: ErrorInfo): void {
    // 保留根因，便于在控制台定位是哪个懒加载 chunk 挂了。
    console.error("路由渲染失败", error, info.componentStack);
  }

  render(): ReactNode {
    if (this.state.message === null) {
      return this.props.children;
    }
    return (
      <Center mih={280} p="md">
        <Stack gap="xs" align="center">
          {/* class 组件拿不到 useTranslation hook，直接调 i18next 的 t 取值。 */}
          <Title order={4}>{translate("common.loadFailedTitle")}</Title>
          <Text size="sm" c="dimmed" ta="center" maw={420}>
            {translate("common.loadFailedHint")}
          </Text>
          <Button
            size="xs"
            variant="light"
            onClick={() => window.location.reload()}
            leftSection={<IconRefresh size={14} />}
          >
            {translate("common.reload")}
          </Button>
        </Stack>
      </Center>
    );
  }
}
