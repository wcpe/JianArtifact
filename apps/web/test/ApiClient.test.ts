// 统一资产操作失败时，前端必须保留后端 operationId 供审计追踪。
import { http, HttpResponse } from "msw";
import { afterEach, describe, expect, it, vi } from "vitest";

import { server } from "@jianartifact/devmock/node";
import {
  ApiError,
  deleteProtocolAsset,
  postProtocolForm,
  putProtocolAsset,
  request,
} from "../src/api/client";

afterEach(() => {
  window.history.replaceState({}, "", "/");
  vi.unstubAllEnvs();
});

describe("API 客户端错误响应", () => {
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
});
