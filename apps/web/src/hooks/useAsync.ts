// 异步数据钩子：封装“加载 / 数据 / 错误 + 重新拉取”的通用状态机。
// 列表/详情页据此渲染 LoadingState / ErrorState / EmptyState，避免重复样板。
// 可选 cacheKey（SWR）：命中缓存立即回放旧数据并后台静默刷新，页面来回切换不闪加载、
// 不因一次刷新失败整页死胡同；error 仅在没有任何数据可展示时置位，失败但旧数据在
// 场时改置 refreshError，由页面自行渲染非阻断警告。
//
// 缓存治理（v0.8.0 修复“页面卡住不刷新”）：
// - 缓存带 TTL（默认 60s）：过期条目视为未命中并立即淘汰，页面不会无限展示陈旧数据；
// - 数据在场时的后台刷新保持 refreshing=true，UI 能给出局部加载反馈，请求挂起不再“无感卡死”；
// - invalidateAsyncCache(prefix) 供变更操作按前缀失效缓存，避免回放过期内容；
// - 请求超时由 api/client 统一兜底（AbortController），不再无限等待。
import { useCallback, useEffect, useRef, useState } from "react";

import { ApiError } from "../api/client";

/** 全局刷新事件名：页眉刷新按钮派发，所有 useAsync 实例（及自管数据的组件）重新拉取。 */
export const REFRESH_EVENT = "jianartifact:refresh";

export interface AsyncState<T> {
  data: T | null;
  /** 首载进行中（尚无数据可展示）。 */
  loading: boolean;
  /** 数据在场时的后台刷新进行中（含缓存回放后重拉）。 */
  refreshing: boolean;
  error: ApiError | null;
  /** 权限不足（HTTP 403）——供页面渲染 ForbiddenState。 */
  forbidden: boolean;
  /** 后台刷新失败（旧数据仍在展示）；仅在提供 cacheKey 且存在旧数据时非空。 */
  refreshError: ApiError | null;
  reload: () => void;
}

export interface UseAsyncOptions {
  /** 稳定缓存键（含取数口径，如筛选/分页）；同键切换回放缓存，跨键不串数据。 */
  cacheKey?: string;
  /** 缓存有效期（毫秒）；默认 60s。过期条目视为未命中并淘汰。 */
  staleMs?: number;
}

interface AsyncCacheEntry {
  value: unknown;
  storedAt: number;
}

const asyncCache = new Map<string, AsyncCacheEntry>();
const ASYNC_CACHE_LIMIT = 64;
/** 默认缓存有效期：60 秒。 */
const DEFAULT_STALE_MS = 60_000;
/** 可观测页面静默刷新间隔：仅页面可见时执行。 */
export const OBSERVABILITY_REFRESH_MS = 60_000;

/**
 * 读取页面数据缓存（自管数据的组件在加载前也可用它回放）。
 * 过期条目会被淘汰并视为未命中；未过期时触碰即前移，近似 LRU。
 */
export function readAsyncCache<T>(key: string, staleMs: number = DEFAULT_STALE_MS): T | null {
  const entry = asyncCache.get(key);
  if (!entry) return null;
  if (Date.now() - entry.storedAt > staleMs) {
    asyncCache.delete(key);
    return null;
  }
  // 触碰即前移，近似 LRU。
  asyncCache.delete(key);
  asyncCache.set(key, entry);
  return entry.value as T;
}

/** 写入页面数据缓存；超限时淘汰最旧条目。 */
export function writeAsyncCache(key: string, value: unknown): void {
  asyncCache.delete(key);
  asyncCache.set(key, { value, storedAt: Date.now() });
  if (asyncCache.size > ASYNC_CACHE_LIMIT) {
    const oldest = asyncCache.keys().next().value;
    if (oldest !== undefined) asyncCache.delete(oldest);
  }
}

/**
 * 同键在途请求去重：同一 cacheKey 正在飞行时复用同一个 Promise。
 *
 * 观测页（仪表盘 / 主机监控）会在一次进入时并发拉多个不同端点，而这些端点又会被多个
 * 组件按同一口径重复订阅；全局刷新按钮与 60s 静默轮询还会叠加上来。若不去重，慢接口下
 * 会出现"同一 URL 同时挂起 N 份"，请求数随挂起时长线性增长——这正是"接口一卡，前端跟着
 * 卡死"的放大器。去重后同键始终只有一份在途请求。
 *
 * 无 cacheKey 的调用不参与去重（口径无法判定），保持原样直发。
 *
 * 约束：**同键必须同语义**。若两个组件用同一 cacheKey 但 fetcher 含义不同
 * （典型反例：一个返回 `null` 占位、一个真去取数），去重会把前者的 Promise
 * 复用给后者，现象是"该数据永远加载不出来"。语义不同就必须用不同键，
 * 或干脆不传 cacheKey。
 */
const inflightRequests = new Map<string, Promise<unknown>>();

function fetchOnce<T>(key: string | undefined, fetcher: () => Promise<T>): Promise<T> {
  if (key === undefined) {
    return fetcher();
  }
  const existing = inflightRequests.get(key);
  if (existing) {
    return existing as Promise<T>;
  }
  const tracked: Promise<T> = fetcher().finally(() => {
    if (inflightRequests.get(key) === tracked) {
      inflightRequests.delete(key);
    }
  });
  inflightRequests.set(key, tracked);
  return tracked;
}

/**
 * 失效页面数据缓存：不带参数清空全部；带前缀只移除匹配的键。
 * 变更操作（创建/删除/可见性切换等）成功后调用，防止下一帧回放过期数据。
 */
export function invalidateAsyncCache(prefix?: string): void {
  if (prefix === undefined) {
    asyncCache.clear();
    return;
  }
  for (const key of asyncCache.keys()) {
    if (key.startsWith(prefix)) {
      asyncCache.delete(key);
    }
  }
}

/** 清空缓存（测试隔离用）。同时在途去重表一并清空，避免测试间串用同一挂起请求。 */
export function clearAsyncCache(): void {
  asyncCache.clear();
  inflightRequests.clear();
}

/**
 * 仅在页面可见时按固定间隔刷新，并在标签页重新获得可见性时立即补拉一次。
 * 观测页使用该钩子，避免后台标签页无意义轮询。
 *
 * reload 经 ref 持有：调用方常传内联函数（每次渲染新引用），若直接进 effect 依赖，
 * 定时器会被反复重建——页面只要有一帧重渲染，60s 定时刷新就永远等不到触发，
 * 同时 visibilitychange 监听器反复增删。这里只在 intervalMs 变化时重建。
 */
export function useVisibleRefresh(
  reload: () => void,
  intervalMs: number = OBSERVABILITY_REFRESH_MS,
): void {
  const reloadRef = useRef(reload);
  reloadRef.current = reload;

  useEffect(() => {
    const refreshIfVisible = () => {
      if (document.visibilityState === "visible") reloadRef.current();
    };
    const timer = window.setInterval(refreshIfVisible, intervalMs);
    document.addEventListener("visibilitychange", refreshIfVisible);
    return () => {
      window.clearInterval(timer);
      document.removeEventListener("visibilitychange", refreshIfVisible);
    };
  }, [intervalMs]);
}

/** 组件挂载即执行 fetcher；deps 变化重新拉取；返回状态与手动 reload。 */
export function useAsync<T>(
  fetcher: () => Promise<T>,
  deps: unknown[] = [],
  options: UseAsyncOptions = {},
): AsyncState<T> {
  const cacheKey = options.cacheKey;
  const staleMs = options.staleMs ?? DEFAULT_STALE_MS;
  const [data, setData] = useState<T | null>(null);
  const [loading, setLoading] = useState(
    () => cacheKey === undefined || readAsyncCache(cacheKey, staleMs) === null,
  );
  const [refreshing, setRefreshing] = useState(false);
  const [error, setError] = useState<ApiError | null>(null);
  const [refreshError, setRefreshError] = useState<ApiError | null>(null);
  const [nonce, setNonce] = useState(0);
  // 当前 data 归属的缓存键与是否在场：跨键（如筛选变化）且无缓存时清空旧数据回到加载态；
  // ref 而非 state，供异步回调判定“旧数据是否在场”而不受闭包过期值影响。
  const dataKeyRef = useRef<string | undefined>(undefined);
  const hasDataRef = useRef(false);

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
    const cached = cacheKey !== undefined ? readAsyncCache<T>(cacheKey, staleMs) : null;
    if (cached !== null) {
      // 缓存回放：立即展示旧数据，同时保持刷新反馈（refreshing=true），
      // 请求挂起或缓慢时用户能看到局部加载态，而不是“无感卡死”。
      dataKeyRef.current = cacheKey;
      hasDataRef.current = true;
      setData(cached);
      setLoading(false);
      setRefreshing(true);
    } else if (dataKeyRef.current !== cacheKey) {
      dataKeyRef.current = cacheKey;
      hasDataRef.current = false;
      setData(null);
      setError(null);
      setRefreshError(null);
      setLoading(true);
    }
    fetchOnce(cacheKey, fetcher)
      .then((result) => {
        if (!active) return;
        // null 占位（如“快照就绪后再拉事件”的等待分支）不算数据在场，也不写缓存。
        if (cacheKey !== undefined && result !== null && result !== undefined) {
          writeAsyncCache(cacheKey, result);
        }
        dataKeyRef.current = cacheKey;
        hasDataRef.current = result !== null && result !== undefined;
        setData(result);
        setError(null);
        setRefreshError(null);
        setLoading(false);
        setRefreshing(false);
      })
      .catch((err: unknown) => {
        if (!active) return;
        const apiError = err instanceof ApiError ? err : new ApiError("unknown", String(err), 0);
        if (hasDataRef.current && dataKeyRef.current === cacheKey) {
          // 旧数据在场：保留展示，仅标记刷新失败。
          setRefreshError(apiError);
        } else {
          dataKeyRef.current = cacheKey;
          hasDataRef.current = false;
          setData(null);
          setError(apiError);
          setLoading(false);
        }
        setRefreshing(false);
      })
      .finally(() => {
        if (active) setLoading(false);
      });
    return () => {
      active = false;
    };
  }, [nonce, cacheKey, ...deps]);

  return {
    data,
    loading,
    refreshing,
    error,
    forbidden: error?.status === 403,
    refreshError,
    reload,
  };
}
