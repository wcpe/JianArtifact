// 全局进度条上下文（FR-127）：统一管理「在途请求数」与「路由切换」的进度状态，
// 供全局进度条组件消费；自研，不引第三方依赖。
//
// 设计：
// - 用进程内原子计数（useRef + useState）跟踪在途请求数，>0 则 loading=true。
// - 路由切换另设独立的 routeLoading，由 RouteProgressTrigger 在 location 变化时
//   短暂置 true → 下一帧 false（视觉上一闪而过的伪进度，给「瞬切」页面也有反馈感）。
// - 两路取 OR：只要有一路为 true，整体 loading 即为 true。

import { createContext, useCallback, useContext, useRef, useState } from 'react';

/** 全局进度上下文值。 */
interface GlobalProgressContextValue {
  /** 综合加载状态：在途请求 OR 路由切换任一为真时为 true。 */
  loading: boolean;
  /** 在途请求数加 1（发请求时调用）。 */
  inc: () => void;
  /** 在途请求数减 1（请求完成/失败时调用）。 */
  dec: () => void;
  /** 路由切换进度：置 true → 触发进度条，组件自行在短暂 tick 后置 false。 */
  setRouteLoading: (v: boolean) => void;
}

// eslint-disable-next-line react-refresh/only-export-components -- context 与 hook 同文件导出是常见 React 模式，此处刻意不拆分
export const GlobalProgressContext = createContext<GlobalProgressContextValue>({
  loading: false,
  inc: () => {},
  dec: () => {},
  setRouteLoading: () => {},
});

/** 全局进度提供者：包裹在路由与应用根节点之外。 */
export function GlobalProgressProvider({ children }: { children: React.ReactNode }) {
  // inflight：在途请求计数（用 ref 保证并发更新不丢失）。
  const inflightRef = useRef(0);
  const [reqLoading, setReqLoading] = useState(false);
  const [routeLoading, setRouteLoading] = useState(false);

  const inc = useCallback(() => {
    inflightRef.current += 1;
    setReqLoading(true);
  }, []);

  const dec = useCallback(() => {
    inflightRef.current = Math.max(0, inflightRef.current - 1);
    if (inflightRef.current === 0) {
      setReqLoading(false);
    }
  }, []);

  const loading = reqLoading || routeLoading;

  return (
    <GlobalProgressContext.Provider value={{ loading, inc, dec, setRouteLoading }}>
      {children}
    </GlobalProgressContext.Provider>
  );
}

// eslint-disable-next-line react-refresh/only-export-components -- hook 与 context/provider 同文件是常见 React 模式
export function useGlobalProgress() {
  return useContext(GlobalProgressContext);
}
