// 统一资产操作失败时，前端必须保留后端 operationId 供审计追踪。
// 另含取消 / 超时对**响应体下载阶段**的覆盖：响应头立即返回、响应体仍在传输时，
// 取消与 15s 兜底都必须继续有效（这是「取消只覆盖到响应头」的经典漏窗）。
import { http, HttpResponse } from "msw";
import { afterEach, describe, expect, it, vi } from "vitest";

import { server } from "@jianartifact/devmock/node";
import {
  ApiError,
  deleteProtocolAsset,
  getNetworkActivityCount,
  postProtocolForm,
  putProtocolAsset,
  request,
  requestBinary,
} from "../src/api/client";

afterEach(() => {
  window.history.replaceState({}, "", "/");
  vi.unstubAllEnvs();
  vi.unstubAllGlobals();
});

/**
 * 构造「响应头立即返回、响应体稍后才到」的响应，并把 fetch 换成直接返回它的桩。
 *
 * 为什么不用 MSW 只延迟整个 handler：那样延迟发生在响应头之前，`fetch()` 一直挂着，
 * 覆盖不到「响应头已到、body 还在读」这个窗口——而恰恰是这段窗口里取消与超时曾经失效
 * （`return parseSuccessJson(...)` 不 await，finally 提前清掉了计时器与取消监听）。
 */
function stubDelayedBodyFetch() {
  let bodyController!: ReadableStreamDefaultController<Uint8Array>;
  const stream = new ReadableStream<Uint8Array>({
    start(controller) {
      bodyController = controller;
    },
  });
  const response = new Response(stream, {
    status: 200,
    headers: { "Content-Type": "application/json" },
  });
  const fetchStub = vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => {
    const signal = init?.signal;
    // 真实浏览器里中止会让响应体流失败；桩里等价地把流打成 AbortError。
    const failBody = () => bodyController.error(new DOMException("读取被中止", "AbortError"));
    if (signal?.aborted) {
      failBody();
    } else {
      signal?.addEventListener("abort", failBody, { once: true });
    }
    return response;
  });
  vi.stubGlobal("fetch", fetchStub);
  return {
    response,
    /** 把响应体写完整（正常结束）。 */
    finish(body: string) {
      bodyController.enqueue(new TextEncoder().encode(body));
      bodyController.close();
    },
  };
}

describe("API 客户端错误响应", () => {
  it("响应头已返回、响应体仍在传输时取消：仍归一化为取消，且在读取结束前不提前清理", async () => {
    const body = stubDelayedBodyFetch();
    const baseline = getNetworkActivityCount();
    const controller = new AbortController();

    const pending = request("/settings", {
      signal: controller.signal,
      // 取一个足够长的超时，确保本用例观察的是「取消」而不是「超时」。
      timeoutMs: 60_000,
    });

    // 响应体读取已经开始（bodyUsed 为真），而流还没结束——正是出问题的那段窗口。
    await vi.waitFor(() => expect(body.response.bodyUsed).toBe(true));
    // 活动计数必须还是「进行中」：说明 finally 没有提前执行。
    expect(getNetworkActivityCount()).toBe(baseline + 1);

    controller.abort();

    await expect(pending).rejects.toMatchObject({
      name: "ApiError",
      code: "aborted",
      status: 0,
    });
    // 读取真正结束后才收尾。
    expect(getNetworkActivityCount()).toBe(baseline);
  });

  it("响应体仍在传输时超时兜底仍然生效（不被提前清理）", async () => {
    const body = stubDelayedBodyFetch();
    const baseline = getNetworkActivityCount();

    await expect(request("/settings", { timeoutMs: 40 })).rejects.toMatchObject({
      name: "ApiError",
      code: "timeout",
      status: 0,
    });
    // 兜底触发即中止了读取，流随之失败，计数归零。
    expect(getNetworkActivityCount()).toBe(baseline);
    expect(body.response.bodyUsed).toBe(true);
  });

  it("响应体正常读完时仍正确返回解析结果", async () => {
    const body = stubDelayedBodyFetch();
    const baseline = getNetworkActivityCount();

    const pending = request<{ ok: boolean }>("/settings");
    await vi.waitFor(() => expect(body.response.bodyUsed).toBe(true));
    body.finish(JSON.stringify({ ok: true }));

    await expect(pending).resolves.toEqual({ ok: true });
    expect(getNetworkActivityCount()).toBe(baseline);
  });

  it("原始字节请求同样把取消覆盖到响应体读取阶段", async () => {
    const body = stubDelayedBodyFetch();
    const baseline = getNetworkActivityCount();
    const controller = new AbortController();

    const pending = requestBinary("/repositories/raw/repo/assets", {
      method: "PUT",
      body: new Blob(["demo"]),
      signal: controller.signal,
      timeoutMs: 60_000,
    });

    await vi.waitFor(() => expect(body.response.bodyUsed).toBe(true));
    expect(getNetworkActivityCount()).toBe(baseline + 1);

    controller.abort();

    await expect(pending).rejects.toMatchObject({
      name: "ApiError",
      code: "aborted",
      status: 0,
    });
    expect(getNetworkActivityCount()).toBe(baseline);
  });

  it("成功状态却返回 HTML 时归一化为可读 API 错误", async () => {
    server.use(
      http.get(
        "*/api/v1/settings",
        () =>
          new HttpResponse("<!doctype html><title>SPA fallback</title>", {
            status: 200,
            headers: { "Content-Type": "text/html; charset=utf-8" },
          }),
      ),
    );

    await expect(request("/settings")).rejects.toMatchObject({
      name: "ApiError",
      code: "unexpected_response",
      message: "接口返回了非 JSON 响应",
      status: 200,
    });
  });

  it("开发态从受限 URL 场景向 API 和协议请求附加场景与页面路径", async () => {
    window.history.replaceState({}, "", "/repositories/raw-hosted?__mock=empty");
    let apiHeaders: Headers | undefined;
    let protocolHeaders: Headers | undefined;
    let formHeaders: Headers | undefined;
    let deleteHeaders: Headers | undefined;
    server.use(
      http.get("*/api/v1/settings", ({ request }) => {
        apiHeaders = request.headers;
        return HttpResponse.json({});
      }),
      http.put("*/repository/raw-hosted/demo.txt", ({ request }) => {
        protocolHeaders = request.headers;
        return HttpResponse.json({});
      }),
      http.post("*/api/v1/repositories/maven-releases/maven-upload", ({ request }) => {
        formHeaders = request.headers;
        return HttpResponse.json({});
      }),
      http.delete("*/repository/raw-hosted/demo.txt", ({ request }) => {
        deleteHeaders = request.headers;
        return new HttpResponse(null, { status: 204 });
      }),
    );

    await request("/settings");
    await putProtocolAsset("/repository/raw-hosted/demo.txt", new Blob(["demo"]));
    await postProtocolForm("/api/v1/repositories/maven-releases/maven-upload", new FormData());
    await expect(deleteProtocolAsset("/repository/raw-hosted/demo.txt")).resolves.toBeUndefined();

    for (const headers of [apiHeaders, protocolHeaders, formHeaders, deleteHeaders]) {
      expect(headers?.get("X-Jian-DevMock-Scenario")).toBe("empty");
      expect(headers?.get("X-Jian-DevMock-Route")).toBe("/repositories/raw-hosted");
    }
  });

  it("生产态与未知场景均不发送开发态 Mock 请求头", async () => {
    window.history.replaceState({}, "", "/settings?__mock=unknown");
    let headers: Headers | undefined;
    server.use(
      http.get("*/api/v1/settings", ({ request }) => {
        headers = request.headers;
        return HttpResponse.json({});
      }),
    );

    await request("/settings");
    expect(headers?.has("X-Jian-DevMock-Scenario")).toBe(false);
    expect(headers?.has("X-Jian-DevMock-Route")).toBe(false);

    window.history.replaceState({}, "", "/settings?__mock=empty");
    vi.stubEnv("DEV", false);
    await request("/settings");
    expect(headers?.has("X-Jian-DevMock-Scenario")).toBe(false);
    expect(headers?.has("X-Jian-DevMock-Route")).toBe(false);
  });

  it("不向跨域或非受管目标泄露开发态 Mock 场景请求头", async () => {
    window.history.replaceState({}, "", "/repositories/raw-hosted?__mock=empty");
    let crossOriginHeaders: Headers | undefined;
    let unmanagedHeaders: Headers | undefined;
    server.use(
      http.put("https://upstream.example/repository/raw-hosted/demo.txt", ({ request }) => {
        crossOriginHeaders = request.headers;
        return HttpResponse.json({});
      }),
      http.put("*/assets/demo.txt", ({ request }) => {
        unmanagedHeaders = request.headers;
        return HttpResponse.json({});
      }),
    );

    await putProtocolAsset(
      "https://upstream.example/repository/raw-hosted/demo.txt",
      new Blob(["demo"]),
    );
    await putProtocolAsset("/assets/demo.txt", new Blob(["demo"]));

    for (const headers of [crossOriginHeaders, unmanagedHeaders]) {
      expect(headers?.has("X-Jian-DevMock-Scenario")).toBe(false);
      expect(headers?.has("X-Jian-DevMock-Route")).toBe(false);
    }
  });

  it("协议上传的 2xx 非 JSON 响应也归一化为 API 错误", async () => {
    server.use(
      http.put(
        "*/repository/raw-hosted/demo.txt",
        () =>
          new HttpResponse("上传网关回退", {
            status: 201,
            headers: { "Content-Type": "text/plain" },
          }),
      ),
      http.post(
        "*/api/v1/repositories/maven-releases/maven-upload",
        () =>
          new HttpResponse("上传网关回退", {
            status: 202,
            headers: { "Content-Type": "text/plain" },
          }),
      ),
    );

    await expect(
      putProtocolAsset("/repository/raw-hosted/demo.txt", new Blob(["demo"])),
    ).rejects.toMatchObject({
      name: "ApiError",
      code: "unexpected_response",
      status: 201,
    });
    await expect(
      postProtocolForm("/api/v1/repositories/maven-releases/maven-upload", new FormData()),
    ).rejects.toMatchObject({
      name: "ApiError",
      code: "unexpected_response",
      status: 202,
    });
  });

  it("JSON Content-Type 的畸形成功响应也统一归一化为 API 错误", async () => {
    server.use(
      http.get(
        "*/api/v1/settings",
        () =>
          new HttpResponse("{", { status: 200, headers: { "Content-Type": "application/json" } }),
      ),
      http.put(
        "*/repository/raw-hosted/demo.txt",
        () =>
          new HttpResponse("{", { status: 201, headers: { "Content-Type": "application/json" } }),
      ),
      http.post(
        "*/api/v1/repositories/maven-releases/maven-upload",
        () =>
          new HttpResponse("{", { status: 202, headers: { "Content-Type": "application/json" } }),
      ),
    );

    await expect(request("/settings")).rejects.toMatchObject({
      name: "ApiError",
      code: "unexpected_response",
    });
    await expect(
      putProtocolAsset("/repository/raw-hosted/demo.txt", new Blob(["demo"])),
    ).rejects.toMatchObject({
      name: "ApiError",
      code: "unexpected_response",
    });
    await expect(
      postProtocolForm("/api/v1/repositories/maven-releases/maven-upload", new FormData()),
    ).rejects.toMatchObject({
      name: "ApiError",
      code: "unexpected_response",
    });
  });

  it("保留统一资产操作失败的 operationId", async () => {
    server.use(
      http.post("*/api/v1/repositories/raw/assets/operations", () =>
        HttpResponse.json(
          {
            error: { code: "internal_error", message: "内部错误" },
            operationId: "op-failed-asset-operation",
          },
          { status: 500 },
        ),
      ),
    );

    try {
      await request("/repositories/raw/assets/operations", { method: "POST", body: {} });
      throw new Error("请求应抛出 ApiError");
    } catch (error) {
      expect(error).toBeInstanceOf(ApiError);
      expect((error as ApiError).operationId).toBe("op-failed-asset-operation");
    }
  });

  // 取消必须是「真实中止网络请求」，而不是只在前端丢弃响应。
  it("外部 AbortSignal 中止时真的中断在途请求，并归一化为 aborted", async () => {
    let handlerAborted = false;
    let notifyStarted!: () => void;
    const started = new Promise<void>((resolve) => {
      notifyStarted = resolve;
    });
    server.use(
      http.get("*/api/v1/settings", async ({ request }) => {
        notifyStarted();
        await new Promise<void>((resolve) => {
          request.signal.addEventListener("abort", () => {
            handlerAborted = true;
            resolve();
          });
        });
        return HttpResponse.json({});
      }),
    );

    const controller = new AbortController();
    const pending = request("/settings", { signal: controller.signal });
    await started;
    controller.abort();

    await expect(pending).rejects.toMatchObject({
      name: "ApiError",
      code: "aborted",
      status: 0,
    });
    expect(handlerAborted).toBe(true);
  });

  it("超时兜底同样中断请求，并归一化为 timeout（供页面区分超时文案）", async () => {
    let handlerAborted = false;
    server.use(
      http.get("*/api/v1/settings", async ({ request }) => {
        await new Promise<void>((resolve) => {
          request.signal.addEventListener("abort", () => {
            handlerAborted = true;
            resolve();
          });
        });
        return HttpResponse.json({});
      }),
    );

    await expect(request("/settings", { timeoutMs: 20 })).rejects.toMatchObject({
      name: "ApiError",
      code: "timeout",
      status: 0,
    });
    expect(handlerAborted).toBe(true);
  });
});
