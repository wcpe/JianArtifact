// 设置接口的归一化守卫：13 项运行期策略里 10 项的 0 是**合法且有意义的取值**
// （禁用作业 / 不自动清理 / 不限制 / 禁用缓存）。
//
// 为什么单开一个文件：设置页用例把 ../src/api/endpoints 整体 mock 掉了，
// 走不到 endpoints.ts 里的 normalizeSettings。而「0 被打回默认值」恰恰是这类
// 归一化最典型的缺陷——写成 `raw.blobGcInterval || 86400` 时界面会显示
// 「每天一次」，与真实的「已禁用」相反，运维据此判断就会得出错误结论。
import { http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";

import { server } from "@jianartifact/devmock/node";
import { getSettings, putSettings } from "../src/api/endpoints";

/** 只回运行期策略 13 项的最小响应体（其余字段缺失，正好顺带验证兜底）。 */
function runtimePolicyResponse(overrides: Record<string, number> = {}) {
  return {
    blobGcInterval: 0,
    storageCleanupInterval: 0,
    backupOrphanCleanupInterval: 0,
    trashGcInterval: 0,
    upstreamProbeInterval: 0,
    backupOrphanGraceHours: 0,
    storageMetadataRetentionDays: 0,
    storageTempMaxAgeHours: 0,
    trashRetentionDays: 0,
    trashMaxBytes: 0,
    autoBlockBaseSeconds: 0,
    searchTimeoutSeconds: 0,
    capacityCacheTtlSeconds: 0,
    ...overrides,
  };
}

describe("设置接口的运行期策略归一化", () => {
  it("0 原样保留，不被当成空值打回默认（?? 而非 ||）", async () => {
    server.use(http.get("*/api/v1/settings", () => HttpResponse.json(runtimePolicyResponse())));

    const settings = await getSettings();

    expect(settings.blobGcInterval).toBe(0);
    expect(settings.storageCleanupInterval).toBe(0);
    expect(settings.trashRetentionDays).toBe(0);
    expect(settings.trashMaxBytes).toBe(0);
    expect(settings.capacityCacheTtlSeconds).toBe(0);
  });

  it("字段缺失时回落到与后端一致的默认值", async () => {
    server.use(http.get("*/api/v1/settings", () => HttpResponse.json({})));

    const settings = await getSettings();

    expect(settings.blobGcInterval).toBe(86400);
    expect(settings.storageCleanupInterval).toBe(86400);
    expect(settings.upstreamProbeInterval).toBe(60);
    expect(settings.backupOrphanGraceHours).toBe(1);
    expect(settings.trashRetentionDays).toBe(7);
    // 2 GiB：必须用幂运算，`2 << 30` 按 32 位算会得到负数。
    expect(settings.trashMaxBytes).toBe(2 * 1024 ** 3);
    expect(settings.autoBlockBaseSeconds).toBe(40);
    expect(settings.searchTimeoutSeconds).toBe(10);
    expect(settings.capacityCacheTtlSeconds).toBe(30);
  });

  it("PUT 的 13 项原样上行（0 不会被请求体丢掉）", async () => {
    let received: Record<string, number> = {};
    server.use(
      http.put("*/api/v1/settings", async ({ request }) => {
        received = (await request.json()) as Record<string, number>;
        return HttpResponse.json(runtimePolicyResponse());
      }),
    );

    await putSettings({
      blobGcInterval: 0,
      trashRetentionDays: 0,
      trashMaxBytes: 0,
      capacityCacheTtlSeconds: 0,
    });

    // 后端是「传哪个改哪个」：0 若被前端过滤掉，就等于「保持原值」，
    // 用户以为关掉了作业，实际仍在跑。
    expect(received.blobGcInterval).toBe(0);
    expect(received.trashRetentionDays).toBe(0);
    expect(received.trashMaxBytes).toBe(0);
    expect(received.capacityCacheTtlSeconds).toBe(0);
  });
});
