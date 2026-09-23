import { HttpResponse, http } from "msw";
import { screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { server } from "@jianartifact/devmock/node";
import { HostMonitoringPage } from "../src/pages/HostMonitoringPage";
import i18n from "../src/i18n";
import { renderWithProviders } from "./harness";

describe("当前主机监控", () => {
  it("开发态读取真实 DevMock 主机接口，而非固定预览", async () => {
    renderWithProviders(<HostMonitoringPage />, { route: "/host-monitoring", authenticated: true });

    await screen.findByText("采样正常，主机指标可用", undefined, { timeout: 5000 });
    expect(screen.getByText("CPU 使用趋势")).toBeTruthy();
    expect(screen.getByText("网络流量趋势")).toBeTruthy();
    expect(screen.queryByText("开发预览数据")).toBeNull();
    expect(screen.queryByText(/同步令牌|JIAN_/)).toBeNull();
  });

  it("内存与磁盘展示总量/已用/可用三值与比率，运行时长出现在进程指标表", async () => {
    renderWithProviders(<HostMonitoringPage />, { route: "/host-monitoring", authenticated: true });

    await screen.findByText("采样正常，主机指标可用", undefined, { timeout: 5000 });
    // 内存三值标签（mock：总量 16.0 GB 固定，已用/可用随采样点变化）；
    // 「可用内存」同时出现在三值格与趋势图图例，故用 getAllByText。
    expect(screen.getByText("内存总量")).toBeTruthy();
    expect(screen.getByText("内存已用")).toBeTruthy();
    expect(screen.getAllByText("可用内存").length).toBeGreaterThanOrEqual(1);
    expect(screen.getByText("内存使用率")).toBeTruthy();
    expect(screen.getAllByText("16.0 GB").length).toBeGreaterThanOrEqual(1);
    // 磁盘三值标签（mock：总量 1000.0 GB 固定）；标签同时出现在三值格与图例，用 getAllByText
    expect(screen.getAllByText("磁盘总量").length).toBeGreaterThanOrEqual(1);
    expect(screen.getAllByText("磁盘已用").length).toBeGreaterThanOrEqual(1);
    expect(screen.getByText("磁盘占用率")).toBeTruthy();
    expect(screen.getAllByText(/GB|TB/).length).toBeGreaterThanOrEqual(3);
    // 进程指标表：表标题、三列与运行时长行（mock 运行时长 > 1 小时 → 形如 01:00:33）
    expect(screen.getByText("进程指标")).toBeTruthy();
    expect(screen.getByText("指标")).toBeTruthy();
    expect(screen.getByText("当前值")).toBeTruthy();
    expect(screen.getByText("说明")).toBeTruthy();
    expect(screen.getByText("运行时长")).toBeTruthy();
    expect(screen.getByText(/挂钟口径，仅统计当前进程/)).toBeTruthy();
    expect(screen.getAllByText(/\d{2}:\d{2}:\d{2}/).length).toBeGreaterThanOrEqual(1);
    // 内存多线图已并入进程 RSS 图例（原独立「进程内存趋势」图移除）
    expect(screen.getByRole("button", { name: "已用内存" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "进程 RSS" })).toBeTruthy();
    expect(screen.queryByText("进程内存趋势")).toBeNull();
  });

  it("历史样本缺新容量字段时降级为 —，不伪造 0", async () => {
    server.use(
      http.get("*/api/v1/observability/host", () => {
        const base = {
          from: "2024-05-01T00:00:00Z",
          to: "2024-05-01T01:00:00Z",
          hostState: { state: "ok" },
          networkState: { state: "ok" },
          processState: { state: "ok" },
          readinessState: { state: "ok" },
          memoryTotalBytes: 8589934592,
          memoryAvailableBytes: 4294967296,
          diskAvailableBytes: 107374182400,
          networkReceiveBytesPerSecond: 1024,
          networkTransmitBytesPerSecond: 512,
          processRssBytes: 67108864,
          processCpuPercent: 2.5,
          goroutineCount: 40,
        };
        return HttpResponse.json({
          hostState: "healthy",
          latestSampleAt: base.to,
          effectiveBucket: "minute",
          latest: base,
          samples: [base],
        });
      }),
    );
    renderWithProviders(<HostMonitoringPage />, { route: "/host-monitoring", authenticated: true });

    await screen.findByText("采样正常，主机指标可用", undefined, { timeout: 5000 });
    // 老数据无 memoryUsedBytes：内存已用按 total − available 兜底（8 − 4 = 4.0 GB）
    // 「4.0 GB」也会出现在趋势图剖析条等处，故用 getAllByText。
    expect(screen.getAllByText("4.0 GB").length).toBeGreaterThanOrEqual(1);
    // 老数据无 diskTotalBytes / diskUsedBytes / processUptimeSeconds：显示占位 "—"
    const dashes = screen.getAllByText("—");
    expect(dashes.length).toBeGreaterThanOrEqual(3);
  });

  it("真实接口无样本时明确呈现空态", async () => {
    server.use(
      http.get("*/api/v1/observability/host", () =>
        HttpResponse.json({ hostState: "unknown", effectiveBucket: "minute", samples: [] }),
      ),
    );
    renderWithProviders(<HostMonitoringPage />, { route: "/host-monitoring", authenticated: true });

    expect(await screen.findByText("尚无主机样本")).toBeTruthy();
  });

  it("真实接口失败时展示可重试错误，而不伪装为预览数据", async () => {
    server.use(
      http.get("*/api/v1/observability/host", () =>
        HttpResponse.json(
          { error: { code: "host_unavailable", message: "采样服务暂不可用" } },
          { status: 503 },
        ),
      ),
    );
    renderWithProviders(<HostMonitoringPage />, { route: "/host-monitoring", authenticated: true });

    expect(await screen.findByText("无法读取主机监控")).toBeTruthy();
    expect(screen.getByRole("button", { name: "重试" })).toBeTruthy();
  });

  it("采样范围档位跟随语言，而非硬编码中文", async () => {
    await i18n.changeLanguage("en");
    renderWithProviders(<HostMonitoringPage />, { route: "/host-monitoring", authenticated: true });

    // 该页全部文案（含状态行）都跟随语言，故等待英文版状态文本即代表已切到英文。
    expect(
      await screen.findByText("Sampling OK, host metrics available", undefined, { timeout: 5000 }),
    ).toBeTruthy();
    // 5 个档位由 hostMonitoring.range* 键提供，切英文后应显示英文且不再残留中文。
    expect(screen.getByRole("button", { name: "Last 24 hours" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Last 7 days" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "近 24 小时" })).toBeNull();
  });
});
