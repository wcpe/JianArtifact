import { afterAll, afterEach, beforeAll, describe, expect, it } from "vitest";

import { server } from "../src/node";
import { resetDevMockScenario } from "../src/scenario";
import { MOCK_TOKEN, resetStore } from "../src/store";

const admin = { Authorization: `Bearer ${MOCK_TOKEN}` };

beforeAll(() => server.listen({ onUnhandledRequest: "error" }));
afterEach(() => {
  server.resetHandlers();
  resetDevMockScenario();
  resetStore();
});
afterAll(() => server.close());

describe("业务与当前主机观测 Mock", () => {
  it("仅管理员读取真实页面所需的业务 KPI 与当前主机样本形状", async () => {
    for (const path of ["dashboard", "host"]) {
      const denied = await fetch(`http://localhost/api/v1/observability/${path}`);
      expect(denied.status).toBe(401);
      const response = await fetch(`http://localhost/api/v1/observability/${path}`, {
        headers: admin,
      });
      expect(response.status).toBe(200);
      const serialized = JSON.stringify(await response.json());
      expect(serialized).not.toContain("token");
      expect(serialized).not.toContain("peerUrl");
    }
  });

  it("主机网络 mock 提供总发送/总接收与网卡列表，并支持按网卡过滤", async () => {
    const response = await fetch("http://localhost/api/v1/observability/host", { headers: admin });
    expect(response.status).toBe(200);
    const body = (await response.json()) as {
      latest: { networkReceiveBytesTotal: number; networkTransmitBytesTotal: number };
      samples: {
        networkReceiveBytesPerSecond: number;
        networkTransmitBytesPerSecond: number;
        networkReceiveBytesTotal: number;
        networkTransmitBytesTotal: number;
      }[];
      networkInterfaces: { name: string; receiveBytesTotal: number; transmitBytesTotal: number }[];
    };
    // 网卡列表至少含两个非回环网卡，供选择器使用。
    expect(body.networkInterfaces.length).toBeGreaterThanOrEqual(2);
    expect(body.networkInterfaces.map((item) => item.name)).toContain("eth0");
    // 总量 = 自网卡启动以来的累计计数，随采样单调不减；速率仍可有抖动。
    for (let index = 1; index < body.samples.length; index += 1) {
      expect(body.samples[index]!.networkReceiveBytesTotal).toBeGreaterThanOrEqual(
        body.samples[index - 1]!.networkReceiveBytesTotal,
      );
      expect(body.samples[index]!.networkTransmitBytesTotal).toBeGreaterThanOrEqual(
        body.samples[index - 1]!.networkTransmitBytesTotal,
      );
    }
    // 全部网卡聚合 = 各网卡累计总量之和（与后端 host_metric_minute 聚合口径一致）。
    const latestSample = body.samples.at(-1)!;
    expect(latestSample.networkReceiveBytesTotal).toBe(
      body.networkInterfaces.reduce((sum, item) => sum + item.receiveBytesTotal, 0),
    );
    expect(body.latest.networkTransmitBytesTotal).toBe(
      body.networkInterfaces.reduce((sum, item) => sum + item.transmitBytesTotal, 0),
    );

    // 指定网卡：速率与总量都按该网卡，且严格小于聚合值（另一个网卡仍有贡献）。
    const selected = await fetch("http://localhost/api/v1/observability/host?interface=eth0", {
      headers: admin,
    });
    const selectedBody = (await selected.json()) as typeof body;
    expect(selectedBody.networkInterfaces).toHaveLength(body.networkInterfaces.length);
    const eth0 = body.networkInterfaces.find((item) => item.name === "eth0")!;
    const selectedLatest = selectedBody.samples.at(-1)!;
    expect(selectedLatest.networkReceiveBytesTotal).toBe(eth0.receiveBytesTotal);
    expect(selectedLatest.networkReceiveBytesTotal).toBeLessThan(
      latestSample.networkReceiveBytesTotal,
    );
    expect(selectedBody.latest.networkReceiveBytesTotal).toBe(eth0.receiveBytesTotal);

    // 未知网卡：不伪造 0，速率与总量整组置 null。
    const unknown = await fetch("http://localhost/api/v1/observability/host?interface=ghost0", {
      headers: admin,
    });
    const unknownBody = (await unknown.json()) as typeof body;
    expect(unknownBody.samples.at(-1)!.networkReceiveBytesTotal).toBeNull();
    expect(unknownBody.samples.at(-1)!.networkReceiveBytesPerSecond).toBeNull();
  });

  it("分组下载趋势注册成功，时间桶、分组总计与权限口径一致", async () => {
    const denied = await fetch("http://localhost/api/v1/observability/downloads/trend");
    expect(denied.status).toBe(401);
    const forbidden = await fetch("http://localhost/api/v1/observability/downloads/trend", {
      headers: { Authorization: "Bearer mock.jwt.token:user" },
    });
    expect(forbidden.status).toBe(403);

    const response = await fetch("http://localhost/api/v1/observability/downloads/trend", {
      headers: admin,
    });
    expect(response.status).toBe(200);
    const body = (await response.json()) as {
      from: string;
      to: string;
      effectiveBucket: string;
      groupBy: string;
      points: { from: string; to: string; group: string; count: number }[];
      totals: { group: string; count: number }[];
    };
    expect(body.effectiveBucket).toBe("minute");
    expect(body.groupBy).toBe("family");
    expect(Date.parse(body.to) - Date.parse(body.from)).toBe(24 * 60 * 60_000);
    const firstSourceMinute = Math.ceil(Date.parse(body.from) / 60_000) * 60_000;
    const endSourceMinute = Math.ceil(Date.parse(body.to) / 60_000) * 60_000;
    expect(new Set(body.points.map((point) => point.from)).size).toBe(
      (endSourceMinute - firstSourceMinute) / 60_000,
    );
    expect(body.totals.length).toBeGreaterThanOrEqual(3);
    expect(body.totals.length).toBeLessThanOrEqual(5);
    for (const total of body.totals) {
      expect(total.count).toBe(
        body.points
          .filter((point) => point.group === total.group)
          .reduce((sum, point) => sum + point.count, 0),
      );
    }
    expect(body.totals.map((item) => item.count)).toEqual(
      [...body.totals.map((item) => item.count)].sort((left, right) => right - left),
    );

    const from = "2026-09-22T10:00:00.000Z";
    const to = "2026-09-22T12:00:00.000Z";
    const ipResponse = await fetch(
      `http://localhost/api/v1/observability/downloads/trend?from=${encodeURIComponent(from)}&to=${encodeURIComponent(to)}&groupBy=ip&repo=maven-releases`,
      { headers: admin },
    );
    const ipBody = (await ipResponse.json()) as typeof body;
    expect(ipBody).toMatchObject({ from, to, groupBy: "ip", effectiveBucket: "minute" });
    expect(new Set(ipBody.points.map((point) => point.from)).size).toBe(120);
    expect(ipBody.totals.length).toBeGreaterThanOrEqual(4);
    expect(ipBody.totals.length).toBeLessThanOrEqual(6);
  });

  it("仓库下载趋势复用下载点类型、补齐连续小时桶并沿用仓库读权限", async () => {
    const anonymousPrivate = await fetch(
      "http://localhost/api/v1/repositories/maven-releases/download-trend",
    );
    expect(anonymousPrivate.status).toBe(401);

    const publicResponse = await fetch(
      "http://localhost/api/v1/repositories/npm-proxy/download-trend",
    );
    expect(publicResponse.status).toBe(200);
    const body = (await publicResponse.json()) as {
      from: string;
      to: string;
      effectiveBucket: string;
      totalDownloadCount: number;
      trend: { from: string; to: string; downloadCount: number }[];
    };
    expect(body.effectiveBucket).toBe("minute");
    expect(body.totalDownloadCount).toBeGreaterThanOrEqual(120_000);
    expect(body.totalDownloadCount).toBeLessThan(160_000);
    const firstSourceMinute = Math.ceil(Date.parse(body.from) / 60_000) * 60_000;
    const endSourceMinute = Math.ceil(Date.parse(body.to) / 60_000) * 60_000;
    expect(body.trend).toHaveLength((endSourceMinute - firstSourceMinute) / 60_000);
    expect(Date.parse(body.trend[0]?.from ?? "")).toBe(firstSourceMinute);
    expect(Date.parse(body.trend.at(-1)?.to ?? "")).toBe(endSourceMinute);
    expect(
      body.trend.every(
        (point, index, points) =>
          point.downloadCount >= 0 && (index === 0 || points[index - 1]?.to === point.from),
      ),
    ).toBe(true);
    expect(body.trend.some((point) => point.downloadCount === 0)).toBe(true);

    const from = "2026-09-22T10:00:00.000Z";
    const to = "2026-09-22T12:00:00.000Z";
    const custom = await fetch(
      `http://localhost/api/v1/repositories/npm-proxy/download-trend?from=${encodeURIComponent(from)}&to=${encodeURIComponent(to)}`,
    );
    const customBody = (await custom.json()) as typeof body;
    expect(customBody.from).toBe(from);
    expect(customBody.to).toBe(to);
    expect(customBody.effectiveBucket).toBe("minute");
    expect(customBody.trend).toHaveLength(120);
  });

  it("非整分窗口按分钟源桶对齐，to 整分边界不多生成终点桶", async () => {
    const from = "2026-09-22T10:00:30.000Z";
    const to = "2026-09-22T10:02:00.000Z";
    const groupedResponse = await fetch(
      `http://localhost/api/v1/observability/downloads/trend?from=${encodeURIComponent(from)}&to=${encodeURIComponent(to)}&groupBy=ip`,
      { headers: admin },
    );
    const grouped = (await groupedResponse.json()) as {
      effectiveBucket: string;
      points: { from: string; to: string }[];
    };
    expect(grouped.effectiveBucket).toBe("minute");
    expect(new Set(grouped.points.map((point) => point.from))).toEqual(
      new Set(["2026-09-22T10:01:00.000Z"]),
    );
    expect(grouped.points.every((point) => point.to === to)).toBe(true);

    const repoResponse = await fetch(
      `http://localhost/api/v1/repositories/npm-proxy/download-trend?from=${encodeURIComponent(from)}&to=${encodeURIComponent(to)}`,
    );
    const repoTrend = (await repoResponse.json()) as {
      trend: { from: string; to: string; downloadCount: number }[];
    };
    expect(repoTrend.trend).toHaveLength(1);
    expect(repoTrend.trend[0]).toMatchObject({ from: "2026-09-22T10:01:00.000Z", to });
    expect(repoTrend.trend[0]?.downloadCount).toBeGreaterThanOrEqual(0);
  });
});
