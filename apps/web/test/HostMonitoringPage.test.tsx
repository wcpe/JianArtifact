import { HttpResponse, http } from "msw";
import { screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { server } from "@jianartifact/devmock/node";
import { HostMonitoringPage } from "../src/pages/HostMonitoringPage";
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
});
