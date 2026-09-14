// 轻量 typed API 客户端：统一 baseURL、Bearer 注入、JSON 解析与错误归一化。
// 开发态由 MSW worker 拦截（见 src/mocks），生产态直连同源后端 /api/v1。
import type { components } from "@jianartifact/devmock/schema";

type ErrorResponse = components["schemas"]["Error"];

/** 归一化的接口错误：承载后端 error.code / message 与 HTTP 状态。 */
export class ApiError extends Error {
  readonly code: string;
  readonly status: number;
  readonly operationId?: string;

  constructor(code: string, message: string, status: number, operationId?: string) {
    super(message);
    this.name = "ApiError";
    this.code = code;
    this.status = status;
    this.operationId = operationId;
  }
}

/**
 * 全局 401 事件名：当任何请求收到 HTTP 401 时触发，
 * AuthContext 监听此事件并自动清除本地会话（退出登录）。
 */
export const AUTH_EXPIRED_EVENT = "jianartifact:auth-expired";

const USER_KEY_FOR_CHECK = "jianartifact.user";

/**
 * 触发全局 401 事件。当存在 token 或 user 快照时触发（表明本地仍认为已登录），
 * 避免纯匿名端点的 401 重复触发。
 */
function emitAuthExpired() {
  const hasToken = Boolean(getToken());
  const hasUser = Boolean(localStorage.getItem(USER_KEY_FOR_CHECK));
  if (hasToken || hasUser) {
    window.dispatchEvent(new CustomEvent(AUTH_EXPIRED_EVENT));
  }
}

const TOKEN_KEY = "jianartifact.token";
const DEV_MOCK_SCENARIO_HEADER = "X-Jian-DevMock-Scenario";
const DEV_MOCK_ROUTE_HEADER = "X-Jian-DevMock-Route";
const DEV_MOCK_SCENARIOS = new Set(["normal", "empty", "loading", "error", "standby_read_only"]);

// —— 全局网络活动计数（FR-71）——
// 页眉刷新按钮据此判断"数据是否已返回"：请求开始 +1、落定 -1，归零即空闲。
let activeRequestCount = 0;
const activityListeners = new Set<(count: number) => void>();

function trackRequestStart() {
  activeRequestCount += 1;
  for (const listener of activityListeners) listener(activeRequestCount);
}

function trackRequestEnd() {
  activeRequestCount -= 1;
  for (const listener of activityListeners) listener(activeRequestCount);
}

/** 当前进行中的请求数（含协议层上传）。 */
export function getNetworkActivityCount(): number {
  return activeRequestCount;
}

/** 订阅网络活动计数变化；返回取消订阅函数。 */
export function subscribeNetworkActivity(listener: (count: number) => void): () => void {
  activityListeners.add(listener);
  return () => {
    activityListeners.delete(listener);
  };
}

/** 读取持久化的会话令牌（localStorage）。 */
export function getToken(): string | null {
  try {
    return localStorage.getItem(TOKEN_KEY);
  } catch {
    return null;
  }
}

/** 写入或清除会话令牌。 */
export function setToken(token: string | null): void {
  try {
    if (token) {
      localStorage.setItem(TOKEN_KEY, token);
    } else {
      localStorage.removeItem(TOKEN_KEY);
    }
  } catch {
    /* 隐私模式等场景下忽略存储失败 */
  }
}

interface RequestOptions {
  method?: string;
  body?: unknown;
  /** 查询参数（值为 undefined 时跳过）。 */
  query?: Record<string, string | number | undefined>;
  /** 可选中止信号（超时 / 用户取消）。 */
  signal?: AbortSignal;
  /** 超时毫秒数；默认 15s，防止请求挂起导致页面“无感卡死”。 */
  timeoutMs?: number;
}

/** 默认请求超时：15 秒。 */
const DEFAULT_TIMEOUT_MS = 15_000;

function buildUrl(path: string, query?: RequestOptions["query"]): string {
  const url = `/api/v1${path}`;
  if (!query) {
    return url;
  }
  const params = new URLSearchParams();
  for (const [key, value] of Object.entries(query)) {
    if (value !== undefined) {
      params.set(key, String(value));
    }
  }
  const qs = params.toString();
  return qs ? `${url}?${qs}` : url;
}

async function parseError(response: Response): Promise<ApiError> {
  try {
    const data = (await response.json()) as ErrorResponse;
    const code = data.error?.code ?? "unknown";
    const message = data.error?.message ?? response.statusText;
    return new ApiError(code, message, response.status, data.operationId);
  } catch {
    return new ApiError("unknown", response.statusText || "请求失败", response.status);
  }
}

function isJsonResponse(response: Response) {
  const contentType = response.headers.get("content-type") ?? "";
  return contentType.includes("application/json") || contentType.includes("+json");
}

async function parseSuccessJson<T>(response: Response): Promise<T> {
  try {
    return (await response.json()) as T;
  } catch {
    throw new ApiError("unexpected_response", "接口返回了无效 JSON 响应", response.status);
  }
}

function isMockManagedTarget(target: string): boolean {
  if (typeof window === "undefined") {
    return false;
  }
  const url = new URL(target, window.location.origin);
  return (
    url.origin === window.location.origin &&
    (url.pathname.startsWith("/api/") || url.pathname.startsWith("/repository/"))
  );
}

function devMockHeaders(target: string): Record<string, string> {
  if (!import.meta.env.DEV || typeof window === "undefined") {
    return {};
  }
  if (!isMockManagedTarget(target)) {
    return {};
  }
  const scenario = new URLSearchParams(window.location.search).get("__mock");
  if (!scenario || !DEV_MOCK_SCENARIOS.has(scenario)) {
    return {};
  }
  return {
    [DEV_MOCK_SCENARIO_HEADER]: scenario,
    [DEV_MOCK_ROUTE_HEADER]: window.location.pathname,
  };
}

/** 发起请求；2xx 返回解析后的 JSON（204 返回 undefined），否则抛出 ApiError。 */
export async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const { method = "GET", body, query, signal, timeoutMs = DEFAULT_TIMEOUT_MS } = options;
  const requestUrl = buildUrl(path, query);
  const headers: Record<string, string> = devMockHeaders(requestUrl);
  const token = getToken();
  if (token) {
    headers.Authorization = `Bearer ${token}`;
  }
  let payload: string | undefined;
  if (body !== undefined) {
    headers["Content-Type"] = "application/json";
    payload = JSON.stringify(body);
  }

  // 超时兜底：单一 AbortController，外部信号经事件转发（不用 AbortSignal.any，
  // 避免组合信号在部分 fetch 实现/MSW 拦截器下被误判为已中止）。
  const controller = new AbortController();
  let timedOut = false;
  const timer = window.setTimeout(() => {
    timedOut = true;
    controller.abort();
  }, timeoutMs);
  const onExternalAbort = () => controller.abort();
  if (signal) {
    if (signal.aborted) {
      controller.abort();
    } else {
      signal.addEventListener("abort", onExternalAbort, { once: true });
    }
  }

  trackRequestStart();
  try {
    let response: Response;
    try {
      response = await fetch(requestUrl, {
        method,
        headers,
        body: payload,
      });
    } catch (e) {
      if (signal?.aborted || (e instanceof DOMException && e.name === "AbortError")) {
        throw new ApiError(
          timedOut ? "timeout" : "aborted",
          timedOut ? "请求超时" : "请求已取消或超时",
          0,
        );
      }
      throw new ApiError("network", e instanceof Error ? e.message : "网络错误", 0);
    }
    if (!response.ok) {
      if (response.status === 401) {
        emitAuthExpired();
      }
      throw await parseError(response);
    }
    if (response.status === 204) {
      return undefined as T;
    }
    if (!isJsonResponse(response)) {
      throw new ApiError("unexpected_response", "接口返回了非 JSON 响应", response.status);
    }
    return parseSuccessJson<T>(response);
  } finally {
    window.clearTimeout(timer);
    signal?.removeEventListener("abort", onExternalAbort);
    trackRequestEnd();
  }
}

/**
 * 原始字节请求（仅 PUT/POST）：分片上传等二进制体通道用。
 * 基础 request() 总是把 body 序列化为 JSON 并固定 Content-Type: application/json，
 * 无法满足分片上传「请求体为原始字节（application/octet-stream）」的契约，
 * 因此这里单独走一条最小路径：直接传 Blob/File/ArrayBuffer，设 octet-stream，
 * 复用同一套 Bearer 注入、超时兜底与错误归一化（parseError 为本模块内部函数）。
 */
export async function requestBinary<T>(
  path: string,
  options: {
    method?: "PUT" | "POST";
    body: Blob | File | ArrayBuffer;
    contentType?: string;
    signal?: AbortSignal;
    timeoutMs?: number;
  },
): Promise<T> {
  const { method = "PUT", body, contentType, signal, timeoutMs = DEFAULT_TIMEOUT_MS } = options;
  const requestUrl = buildUrl(path);
  const headers: Record<string, string> = devMockHeaders(requestUrl);
  const token = getToken();
  if (token) {
    headers.Authorization = `Bearer ${token}`;
  }
  headers["Content-Type"] = contentType ?? "application/octet-stream";

  // 超时兜底：与 request() 一致的单一 AbortController + 外部信号转发。
  const controller = new AbortController();
  let timedOut = false;
  const timer = window.setTimeout(() => {
    timedOut = true;
    controller.abort();
  }, timeoutMs);
  const onExternalAbort = () => controller.abort();
  if (signal) {
    if (signal.aborted) {
      controller.abort();
    } else {
      signal.addEventListener("abort", onExternalAbort, { once: true });
    }
  }

  trackRequestStart();
  try {
    let response: Response;
    try {
      response = await fetch(requestUrl, { method, headers, body });
    } catch (e) {
      if (signal?.aborted || (e instanceof DOMException && e.name === "AbortError")) {
        throw new ApiError(
          timedOut ? "timeout" : "aborted",
          timedOut ? "请求超时" : "请求已取消或超时",
          0,
        );
      }
      throw new ApiError("network", e instanceof Error ? e.message : "网络错误", 0);
    }
    if (!response.ok) {
      if (response.status === 401) {
        emitAuthExpired();
      }
      throw await parseError(response);
    }
    if (response.status === 204) {
      return undefined as T;
    }
    if (!isJsonResponse(response)) {
      throw new ApiError("unexpected_response", "接口返回了非 JSON 响应", response.status);
    }
    return parseSuccessJson<T>(response);
  } finally {
    window.clearTimeout(timer);
    signal?.removeEventListener("abort", onExternalAbort);
    trackRequestEnd();
  }
}

/** 协议层 PUT 上传（Raw hosted）：Bearer + 原始 body，非 /api/v1 JSON。 */
export async function putProtocolAsset(
  url: string,
  body: Blob | ArrayBuffer | File,
  contentType?: string,
): Promise<{ repository: string; path: string; hash: string; size: number; contentType: string }> {
  const headers: Record<string, string> = devMockHeaders(url);
  const token = getToken();
  if (token) {
    headers.Authorization = `Bearer ${token}`;
  }
  if (contentType) {
    headers["Content-Type"] = contentType;
  }
  trackRequestStart();
  try {
    let response: Response;
    try {
      response = await fetch(url, { method: "PUT", headers, body });
    } catch (e) {
      throw new ApiError("network", e instanceof Error ? e.message : "网络错误", 0);
    }
    if (!response.ok) {
      throw await parseError(response);
    }
    if (!isJsonResponse(response)) {
      throw new ApiError("unexpected_response", "接口返回了非 JSON 响应", response.status);
    }
    return parseSuccessJson<{
      repository: string;
      path: string;
      hash: string;
      size: number;
      contentType: string;
    }>(response);
  } finally {
    trackRequestEnd();
  }
}

/** 协议层 multipart POST（FR-73 Maven 网页上传）：Bearer + FormData，非契约 JSON 请求。 */
export async function postProtocolForm<T>(url: string, form: FormData): Promise<T> {
  const headers: Record<string, string> = devMockHeaders(url);
  const token = getToken();
  if (token) {
    headers.Authorization = `Bearer ${token}`;
  }
  trackRequestStart();
  try {
    let response: Response;
    try {
      response = await fetch(url, { method: "POST", headers, body: form });
    } catch (e) {
      throw new ApiError("network", e instanceof Error ? e.message : "网络错误", 0);
    }
    if (!response.ok) {
      throw await parseError(response);
    }
    if (!isJsonResponse(response)) {
      throw new ApiError("unexpected_response", "接口返回了非 JSON 响应", response.status);
    }
    return parseSuccessJson<T>(response);
  } finally {
    trackRequestEnd();
  }
}

/** 协议层 DELETE（制品删除）：Bearer + 无 body，非 /api/v1 JSON 请求；204 视为成功。 */
export async function deleteProtocolAsset(url: string): Promise<void> {
  const headers: Record<string, string> = devMockHeaders(url);
  const token = getToken();
  if (token) {
    headers.Authorization = `Bearer ${token}`;
  }
  trackRequestStart();
  try {
    let response: Response;
    try {
      response = await fetch(url, { method: "DELETE", headers });
    } catch (e) {
      throw new ApiError("network", e instanceof Error ? e.message : "网络错误", 0);
    }
    if (!response.ok) {
      throw await parseError(response);
    }
  } finally {
    trackRequestEnd();
  }
}
