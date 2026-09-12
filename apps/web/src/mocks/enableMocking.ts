// 开发态 Mock 接入：仅在 dev 构建启动 MSW 浏览器 worker，拦截 /api/v1 请求。
// 生产构建（import.meta.env.PROD）直接跳过，请求直连后端；devmock 不进生产包。
type UnhandledRequestPrinter = { error: () => void };

export function isMockManagedRequest(requestUrl: string, origin: string): boolean {
  const url = new URL(requestUrl, origin);
  return (
    url.origin === origin &&
    (url.pathname.startsWith("/api/") || url.pathname.startsWith("/repository/"))
  );
}

export function handleUnhandledMockRequest(
  request: Request,
  print: UnhandledRequestPrinter,
  origin: string,
): void {
  if (isMockManagedRequest(request.url, origin)) {
    print.error();
  }
}

export async function enableMocking(): Promise<void> {
  if (!import.meta.env.DEV) {
    return;
  }
  const devmock = await import("@jianartifact/devmock/browser");
  const { store, emptyStore, resetStore } =
    await import("@jianartifact/devmock/store");
  const { releaseDevMockPendingRequests } = await import("@jianartifact/devmock/scenario");
  // 开发/联调用：暴露 store 复位钩子，便于手动验收「空库初始化」与「已初始化登录」两条路径。
  // 例如控制台执行 `__devmock.emptyStore()` 后刷新，即可进入 /setup 首次初始化引导。
  (window as unknown as { __devmock?: unknown }).__devmock = {
    store,
    emptyStore,
    resetStore,
    releasePending: releaseDevMockPendingRequests,
  };
  await devmock.worker.start({
    onUnhandledRequest(request, print) {
      handleUnhandledMockRequest(request, print, window.location.origin);
    },
  });
}
