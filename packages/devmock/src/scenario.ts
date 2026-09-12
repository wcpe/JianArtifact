import { HttpResponse } from "msw";

/** 开发态场景请求头：请求级声明优先于路由级测试设置。 */
export const DEV_MOCK_SCENARIO_HEADER = "X-Jian-DevMock-Scenario";
/** 开发态页面路由请求头：用于将测试场景限制在单一页面。 */
export const DEV_MOCK_ROUTE_HEADER = "X-Jian-DevMock-Route";

const scenarioValues = ["normal", "empty", "loading", "error", "standby_read_only"] as const;

export type DevMockScenario = (typeof scenarioValues)[number];

interface PendingRequest {
  release: () => void;
}

const routeScenarios = new Map<string, DevMockScenario>();
const pendingRequests = new Set<PendingRequest>();
const pendingWaiters = new Set<() => void>();

function isScenario(value: string | null): value is DevMockScenario {
  return value !== null && scenarioValues.some((scenario) => scenario === value);
}

/**
 * 解析本次请求生效的场景：请求头显式声明优先，其次按路由级测试设置，缺省 `normal`。
 * 浏览器正常访问不会携带场景头（只有 URL 带 `?__mock=` 时才发），因此"未声明"必须是 `normal`
 * ——直接读 header 判定会把正常访问误判成未知场景，导致夹具型页面（如迁移）永远走空态。
 */
export function currentDevMockScenario(request: Request): DevMockScenario {
  const explicit = request.headers.get(DEV_MOCK_SCENARIO_HEADER);
  if (isScenario(explicit)) {
    return explicit;
  }
  const route = request.headers.get(DEV_MOCK_ROUTE_HEADER);
  return route ? (routeScenarios.get(route) ?? "normal") : "normal";
}

function standbyRequestAllowed(request: Request): boolean {
  switch (request.method) {
    case "GET":
    case "HEAD":
    case "OPTIONS":
      return true;
    case "POST": {
      const pathname = new URL(request.url).pathname;
      return pathname === "/api/v1/auth/login";
    }
    default:
      return false;
  }
}

function pendingAbortError(signal: AbortSignal): DOMException {
  if (signal.reason instanceof DOMException) {
    return signal.reason;
  }
  return new DOMException("开发态 Mock 加载请求已取消", "AbortError");
}

function waitForRelease(signal: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    const pending: PendingRequest = { release };
    const cleanup = () => {
      pendingRequests.delete(pending);
      signal.removeEventListener("abort", abort);
    };
    function release() {
      cleanup();
      resolve();
    }
    function abort() {
      cleanup();
      reject(pendingAbortError(signal));
    }

    if (signal.aborted) {
      abort();
      return;
    }

    pendingRequests.add(pending);
    signal.addEventListener("abort", abort, { once: true });
    releasePendingWaiters();
  });
}

function releasePendingWaiters() {
  for (const resolve of pendingWaiters) {
    resolve();
  }
  pendingWaiters.clear();
}

/** 为单一路由设置测试场景；下一次 reset 前不会影响其他路由。 */
export function setDevMockScenario(route: string, scenario: DevMockScenario) {
  routeScenarios.set(route, scenario);
}

/** 返回当前尚未释放的 loading 请求数，供测试断言。 */
export function pendingDevMockRequests(): number {
  return pendingRequests.size;
}

/** 等待任意 loading 请求进入显式挂起状态，不使用延时轮询。 */
export function waitForDevMockPendingRequest(): Promise<void> {
  if (pendingRequests.size > 0) {
    return Promise.resolve();
  }
  return new Promise((resolve) => pendingWaiters.add(resolve));
}

/** 显式继续全部 loading 请求，并返回本次释放数量。 */
export function releaseDevMockPendingRequests(): number {
  const current = [...pendingRequests];
  for (const pending of current) {
    pending.release();
  }
  return current.length;
}

/** 清除路由场景并释放遗留请求，避免测试间互相阻塞。 */
export function resetDevMockScenario() {
  routeScenarios.clear();
  releaseDevMockPendingRequests();
  releasePendingWaiters();
}

/** MSW 统一前置守卫：返回 undefined 时继续由路由专属 handler 处理。 */
export async function interceptDevMockScenario(request: Request): Promise<Response | undefined> {
  switch (currentDevMockScenario(request)) {
    case "error":
      return HttpResponse.json(
        { error: { code: "devmock_scenario_error", message: "开发态 Mock 场景模拟服务异常" } },
        { status: 500 },
      );
    case "standby_read_only":
      if (!standbyRequestAllowed(request)) {
        return HttpResponse.json(
          { error: { code: "standby_read_only", message: "备用节点为只读，当前请求已拒绝" } },
          { status: 503 },
        );
      }
      return undefined;
    case "loading":
      await waitForRelease(request.signal);
      return undefined;
    default:
      return undefined;
  }
}
