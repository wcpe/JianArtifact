// 上游健康面板（FR-159）的关键界面约束：
//   - 空态必须是「尚无采样」，绝不能显示 100%（0 样本 ≠ 满连通）；
//   - request / probe 两条序列必须可区分（切换后率值各自独立，不被平均）；
//   - 明细表交代「分钟桶聚合」口径，无成功样本时延迟显示「无成功样本」而不是 0 ms。
// 端点层用 vi.mock 替换，直接喂固定响应，避免测试依赖 MSW 场景状态。
import { MantineProvider } from "@mantine/core";
import { render, screen, waitFor } from "@testing-library/react";
import "@testing-library/jest-dom/vitest";
import { beforeEach, describe, expect, it, vi } from "vitest";

import "../../i18n";
import type {
  UpstreamHealthPoint,
  UpstreamHealthRecent,
  UpstreamHealthSeries,
} from "../../api/types";

const getUpstreamHealth = vi.fn();
const getUpstreamHealthRecent = vi.fn();

vi.mock("../../api/endpoints", () => ({
  getUpstreamHealth: (...args: unknown[]) => getUpstreamHealth(...args),
  getUpstreamHealthRecent: (...args: unknown[]) => getUpstreamHealthRecent(...args),
}));

// 被测组件在 mock 之后引入，确保它拿到的是替换后的端点。
const { UpstreamHealthPanel } = await import("./UpstreamHealthPanel");

function renderPanel() {
  return render(
    <MantineProvider>
      <UpstreamHealthPanel repoName="proxy-a" />
    </MantineProvider>,
  );
}

/** 构造一个采样点。 */
function point(overrides: Partial<UpstreamHealthPoint>): UpstreamHealthPoint {
  return {
    bucketStart: "2026-08-27 10:00:00",
    source: "request",
    samples: 1,
    ok: 1,
    timeout: 0,
    unreachable: 0,
    slow: 0,
    aborted: 0,
    avgLatencyMs: 120,
    maxLatencyMs: 120,
    ...overrides,
  };
}

/** 空序列响应：两路都没有采样。 */
const emptySeries: UpstreamHealthSeries = {
  repository: "proxy-a",
  from: "2026-08-27T00:00:00Z",
  to: "2026-08-27T12:00:00Z",
  points: [],
  summaries: {},
};

const emptyRecent: UpstreamHealthRecent = { repository: "proxy-a", items: [] };

beforeEach(() => {
  getUpstreamHealth.mockReset();
  getUpstreamHealthRecent.mockReset();
  getUpstreamHealthRecent.mockResolvedValue(emptyRecent);
});

describe("UpstreamHealthPanel", () => {
  it("空态显示「尚无采样」且不显示 100%", async () => {
    getUpstreamHealth.mockResolvedValue(emptySeries);
    renderPanel();

    const empty = await screen.findByTestId("repo-upstream-health-empty");
    expect(empty).toHaveTextContent("尚无采样");
    // 关键断言：不能出现任何百分比读数——0 样本被算成 100% 会直接误导运维。
    expect(screen.queryByText(/%/)).not.toBeInTheDocument();
    expect(screen.getByTestId("repo-upstream-health-recent")).toHaveTextContent("尚无采样明细");
  });

  it("request 与 probe 各自成率，切换后互不平均", async () => {
    // request 2/3 成功（66.7%），probe 1/1 成功（100%）。
    getUpstreamHealth.mockResolvedValue({
      repository: "proxy-a",
      from: "2026-08-27T00:00:00Z",
      to: "2026-08-27T12:00:00Z",
      points: [
        point({ source: "request", samples: 3, ok: 2, timeout: 1, avgLatencyMs: 100 }),
        point({
          bucketStart: "2026-08-27 10:01:00",
          source: "probe",
          samples: 1,
          ok: 1,
          avgLatencyMs: 20,
        }),
      ],
      summaries: {
        request: {
          samples: 3,
          ok: 2,
          timeout: 1,
          unreachable: 0,
          slow: 0,
          aborted: 0,
          failed: 1,
          connectivity: 2 / 3,
          avgLatencyMs: 100,
          p95LatencyMs: 100,
        },
        probe: {
          samples: 1,
          ok: 1,
          timeout: 0,
          unreachable: 0,
          slow: 0,
          aborted: 0,
          failed: 0,
          connectivity: 1,
          avgLatencyMs: 20,
          p95LatencyMs: 20,
        },
      },
    });
    renderPanel();

    // 默认展示 request 序列：66.7%（若两条被平均会得到 3/4=75%）。
    await waitFor(() => {
      expect(screen.getByText("66.7%")).toBeInTheDocument();
    });
    expect(screen.queryByText("75.0%")).not.toBeInTheDocument();

    // 失败原因分布里，未响应 1 次。
    expect(screen.getByTestId("repo-upstream-health-failures")).toHaveTextContent("未响应");
  });

  it("明细行如实反映失败与「无成功样本」的延迟", async () => {
    getUpstreamHealth.mockResolvedValue({
      repository: "proxy-a",
      from: "2026-08-27T00:00:00Z",
      to: "2026-08-27T12:00:00Z",
      points: [
        point({
          source: "request",
          samples: 2,
          ok: 0,
          unreachable: 2,
          avgLatencyMs: null,
          maxLatencyMs: null,
        }),
      ],
      summaries: {
        request: {
          samples: 2,
          ok: 0,
          timeout: 0,
          unreachable: 2,
          slow: 0,
          aborted: 0,
          failed: 2,
          connectivity: 0,
          avgLatencyMs: null,
          p95LatencyMs: null,
        },
      },
    });
    getUpstreamHealthRecent.mockResolvedValue({
      repository: "proxy-a",
      items: [
        point({
          source: "request",
          samples: 2,
          ok: 0,
          unreachable: 2,
          avgLatencyMs: null,
          maxLatencyMs: null,
        }),
      ],
    });
    renderPanel();

    // 有样本但全失败 → 连通率 0.0%，不是「尚无采样」。
    await waitFor(() => {
      expect(screen.getByText("0.0%")).toBeInTheDocument();
    });
    // 明细：结果列写「2 次失败」，延迟列写「无成功样本」，绝不写 0 ms。
    const row = await screen.findByTestId("upstream-health-row-2026-08-27 10:00:00-request");
    expect(row).toHaveTextContent("2 次失败");
    expect(row).toHaveTextContent("无成功样本");
  });
});
