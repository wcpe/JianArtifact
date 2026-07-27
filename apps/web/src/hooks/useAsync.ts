// 异步数据钩子：封装“加载 / 数据 / 错误 + 重新拉取”的通用状态机。
// 列表/详情页据此渲染 LoadingState / ErrorState / EmptyState，避免重复样板。
import { useCallback, useEffect, useState } from "react";

import { ApiError } from "../api/client";

/** 全局刷新事件名：页眉刷新按钮派发，所有 useAsync 实例（及自管数据的组件）重新拉取。 */
export const REFRESH_EVENT = "jianartifact:refresh";

export interface AsyncState<T> {
  data: T | null;
  loading: boolean;
  error: ApiError | null;
  /** 权限不足（HTTP 403）——供页面渲染 ForbiddenState。 */
  forbidden: boolean;
  reload: () => void;
}

/** 组件挂载即执行 fetcher；deps 变化重新拉取；返回状态与手动 reload。 */
export function useAsync<T>(fetcher: () => Promise<T>, deps: unknown[] = []): AsyncState<T> {
  const [data, setData] = useState<T | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<ApiError | null>(null);
  const [nonce, setNonce] = useState(0);

  const reload = useCallback(() => {
    setNonce((n) => n + 1);
  }, []);

  // FR-71：响应页眉刷新按钮的全局刷新事件。
  useEffect(() => {
    const handler = () => setNonce((n) => n + 1);
    window.addEventListener(REFRESH_EVENT, handler);
    return () => window.removeEventListener(REFRESH_EVENT, handler);
  }, []);

  useEffect(() => {
    let active = true;
    setLoading(true);
    setError(null);
    fetcher()
      .then((result) => {
        if (active) {
          setData(result);
        }
      })
      .catch((err: unknown) => {
        if (active) {
          setError(err instanceof ApiError ? err : new ApiError("unknown", String(err), 0));
        }
      })
      .finally(() => {
        if (active) {
          setLoading(false);
        }
      });
    return () => {
      active = false;
    };
  }, [nonce, ...deps]);

  return {
    data,
    loading,
    error,
    forbidden: error?.status === 403,
    reload,
  };
}
