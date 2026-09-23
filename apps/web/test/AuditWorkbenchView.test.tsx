// 审计工作台（单列表 + 顶部 KPI）：滚动限制、KPI 卡片、动作翻译、筛选与批次抽屉的回归覆盖。
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { HttpResponse, http } from "msw";
import { describe, expect, it } from "vitest";

import { server } from "@jianartifact/devmock/node";

import { AuditWorkbenchView } from "../src/components/audit/AuditWorkbenchView";
import type { AuditEvent } from "../src/api/types";
import { renderWithProviders } from "./harness";

function renderWorkbench(route = "/audit-logs") {
  return renderWithProviders(<AuditWorkbenchView />, { route, authenticated: true });
}

/** 最小审计事件骨架：只覆盖详情展示相关字段，供事件页覆盖场景复用。 */
function customEvent(overrides: Partial<AuditEvent> = {}): AuditEvent {
  return {
    eventId: "audit-custom-1",
    occurredAt: "2026-08-30T10:00:00.000Z",
    category: "management_change",
    severity: "normal",
    result: "success",
    action: "settings.update",
    target: { kind: "setting", label: "系统设置" },
    actor: {
      displayName: "admin",
      subjectType: "user",
      authSource: "jwt",
      email: "admin@example.com",
    },
    summary: "保存系统设置",
    durationMs: 120,
    http: { method: "PATCH", path: "/api/v1/settings", statusCode: 200 },
    ...overrides,
  };
}

/** 覆盖事件列表端点：用于控制 httpHeaders / durationServerMs 的缺省与异常取值。 */
function overrideEventList(items: AuditEvent[]): void {
  server.use(
    http.get("*/api/v1/observability/audit/events", () =>
      HttpResponse.json({
        items,
        totalCount: items.length,
        snapshot: "test-snapshot",
        snapshotAt: "2026-08-30T10:00:00.000Z",
      }),
    ),
  );
}

describe("审计工作台（单列表 + 顶部 KPI）", () => {
  it("锁定视口高度：外层不滚动，列表在剩余高度内滚动", async () => {
    renderWorkbench();

    const workbench = await screen.findByTestId("audit-workbench");
    expect(workbench).toBeTruthy();
    const outerStyle = workbench.style;
    expect(outerStyle.height).toContain("calc(100dvh");
    expect(outerStyle.overflow).toBe("hidden");

    const recordsScroll = screen.getByTestId("audit-records-scroll");
    expect(recordsScroll.style.overflowY).toBe("auto");
    // 滚动条隐藏类：常驻滚动条会遮挡内容（用户反馈），该类隐藏滚动条并保留滚动能力。
    expect(recordsScroll.className).toContain("ja-hide-scrollbar");
    // 右栏已移除：待处理风险改为筛选维度。
    expect(screen.queryByTestId("audit-risk-sidebar")).toBeNull();
  });

  it("顶部展示 KPI 卡片指标", async () => {
    renderWorkbench();

    // KPI 卡片 label（与列表结果列文案重名，用 All 变体断言存在）。
    expect(await screen.findByText("审计事件")).toBeTruthy();
    expect(screen.getAllByText("成功").length).toBeGreaterThan(0);
    expect(screen.getAllByText("失败").length).toBeGreaterThan(0);
    expect(screen.getAllByText("高危").length).toBeGreaterThan(0);
    expect(screen.getAllByText("待确认批次").length).toBeGreaterThan(0);
  });

  it("记录区为 OpsSection 分区卡：时间范围在筛选条内，标题已移除", async () => {
    renderWorkbench();

    // 页面标题只出现在页眉面包屑，页面内不重复渲染大标题。
    expect(screen.queryByRole("heading", { name: "审计中心" })).toBeNull();
    await screen.findByTestId("audit-records-scroll");
    // 时间范围 SegmentedControl（渲染为 radio）：档位为 24h/3d/7d/30d，默认 24h。
    expect((screen.getByRole("radio", { name: "24h" }) as HTMLInputElement).checked).toBe(true);
    expect(screen.getByRole("radio", { name: "3d" })).toBeTruthy();
    expect(screen.getByRole("radio", { name: "7d" })).toBeTruthy();
    expect(screen.getByRole("radio", { name: "30d" })).toBeTruthy();
    // 防回归：1h 档已废弃移除，不得再出现。
    expect(screen.queryByRole("radio", { name: "1h" })).toBeNull();
  });

  it("筛选条保留含同步记录开关，自定义时间为组件库日期区间选择器", async () => {
    renderWorkbench();

    await screen.findByTestId("audit-records-scroll");
    // 该开关曾随「原生日期输入 → 组件库」替换被整段误删，此断言防回归。
    const toggle = screen.getByRole("checkbox", { name: "含同步记录" }) as HTMLInputElement;
    expect(toggle.checked).toBe(false);
    // 自定义时间段必须是组件库（DatePickerInput type=range），不得回退到原生日期输入。
    expect(document.querySelectorAll('input[type="date"]').length).toBe(0);
    expect(document.querySelector(".mantine-DatePickerInput-input")).toBeTruthy();
  });

  it("动作列展示 i18n 中文标签而非契约值", async () => {
    renderWorkbench();

    // 契约值 auth.login_rejected → 中文标签
    expect(await screen.findByText("管理登录被拒绝")).toBeTruthy();
    expect(screen.queryByText("auth.login_rejected")).toBeNull();
  });

  it("点击行内展开脱敏详情", async () => {
    const user = userEvent.setup();
    renderWorkbench();

    const rows = await screen.findAllByRole("button", { name: /审计事件：/ });
    expect(rows.length).toBeGreaterThan(0);
    await user.click(rows[0]!);
    expect(await screen.findByText("请求 ID")).toBeTruthy();
    expect(screen.getByText("User-Agent")).toBeTruthy();
    expect(screen.getByText("请求体（已脱敏）")).toBeTruthy();
  });
});

describe("审计工作台（筛选与搜索）", () => {
  it("关键字搜索过滤列表，清除筛选可恢复", async () => {
    const user = userEvent.setup();
    renderWorkbench();

    const box = await screen.findByPlaceholderText("路径 / 动作 / 操作者邮箱");
    await user.type(box, "不存在的对象");
    await user.click(screen.getByRole("button", { name: "筛选" }));

    expect(await screen.findByText(/共 \d+ 条/)).toBeTruthy();
    await user.click(screen.getByRole("button", { name: "清除筛选" }));
    expect((await screen.findAllByRole("button", { name: /审计事件：/ })).length).toBeGreaterThan(
      0,
    );
  });

  it("风险状态筛选：待处理 / 已确认", async () => {
    const user = userEvent.setup();
    renderWorkbench();

    await screen.findByTestId("audit-records-scroll");
    expect(screen.getByRole("radio", { name: "待处理" })).toBeTruthy();
    await user.click(screen.getByRole("radio", { name: "已确认" }));
    // 已确认口径下不再出现未确认批次的「待处理」标记行。
    expect(await screen.findByText(/共 \d+ 条/)).toBeTruthy();
  });
});

describe("审计工作台（风险批次抽屉）", () => {
  it("行内展开后经「查看风险批次」打开抽屉，确认动作需二次确认", async () => {
    const user = userEvent.setup();
    renderWorkbench();

    const rows = await screen.findAllByRole("button", { name: /审计事件：/ });
    await user.click(rows[0]!);
    await user.click(await screen.findByRole("button", { name: "查看批次" }));

    const dialog = await screen.findByRole("dialog", { name: "风险批次详情" });
    expect(dialog).toBeTruthy();

    // 防误触：第一次点击只出现确认气泡，需再次点击「确认」才生效。
    await user.click(screen.getByRole("button", { name: "确认已处理" }));
    expect(
      await screen.findByText("确认该批次全部风险记录已处理？确认后写入确认人与时间。"),
    ).toBeTruthy();
    await user.click(screen.getByRole("button", { name: "确认", exact: true }));
    await waitFor(() => {
      expect(screen.queryByRole("button", { name: "确认已处理" })).toBeNull();
    });
  });
});

describe("审计工作台（聚合范围过大）", () => {
  it("后端 409 时说明原因并给出一键缩小范围，而不是笼统的「暂时不可用」", async () => {
    // 线上 bug 的用户可见形态：24h 窗口事件量超过后端聚合上限 → 409
    // audit_query_too_large → 前端原来一律显示「当前节点审计暂时不可用」，
    // 用户既不知道原因也无从下手（重试必然同一结果）。
    server.use(
      http.get("*/api/v1/observability/audit/summary", () =>
        HttpResponse.json(
          {
            error: {
              code: "audit_query_too_large",
              message: "审计聚合范围过大，请缩小时间范围或筛选条件",
            },
          },
          { status: 409 },
        ),
      ),
    );
    const user = userEvent.setup();
    renderWorkbench();

    expect(await screen.findByText("当前时间范围内的审计事件过多")).toBeTruthy();
    expect(screen.queryByText("当前节点审计暂时不可用")).toBeNull();
    expect(screen.getByText("审计聚合范围过大，请缩小时间范围或筛选条件")).toBeTruthy();

    // 一键切到近 24 小时（当前最小窗口）：范围控件随之切到 24h。
    await user.click(screen.getByRole("button", { name: "改用近 24 小时" }));
    await waitFor(() => {
      expect((screen.getByRole("radio", { name: "24h" }) as HTMLInputElement).checked).toBe(true);
    });
  });
});

describe("审计工作台（详情：服务端耗时与请求头）", () => {
  it("详情并列展示总耗时与服务端耗时，请求头区块渲染白名单键值", async () => {
    const user = userEvent.setup();
    renderWorkbench();

    const rows = await screen.findAllByRole("button", { name: /审计事件：/ });
    expect(rows.length).toBeGreaterThan(0);
    await user.click(rows[0]!);

    // 服务端耗时与总耗时在同一「性能」组内并列可对照（夹具 durationServerMs=71 / durationMs=96）。
    expect(await screen.findByText("服务端耗时")).toBeTruthy();
    expect(screen.getAllByText("71 ms").length).toBeGreaterThan(0);
    expect(screen.getAllByText("96 ms").length).toBeGreaterThan(0);
    // 请求头区块：区块标题 + 键名等宽原样展示（英文头名不翻译）+ 值渲染出来。
    expect(screen.getByText("请求头（已过滤）")).toBeTruthy();
    expect(screen.getByText("Accept")).toBeTruthy();
    expect(screen.getByText("Content-Type")).toBeTruthy();
    // 头值与键名同一行（"Accept: application/json"），按包含匹配即可证明值已渲染。
    expect(screen.getAllByText(/application\/json/).length).toBeGreaterThan(0);
  });

  it("请求头 JSON 解析失败时回退展示原始文本，不静默丢内容", async () => {
    overrideEventList([
      customEvent({
        http: {
          method: "GET",
          path: "/api/v1/settings",
          statusCode: 200,
          httpHeaders: "{not-valid-json",
        },
      }),
    ]);
    const user = userEvent.setup();
    renderWorkbench();

    const rows = await screen.findAllByRole("button", { name: /审计事件：/ });
    expect(rows).toHaveLength(1);
    await user.click(rows[0]!);

    expect(await screen.findByText("请求头（已过滤）")).toBeTruthy();
    // 解析失败兜底：原始文本原样可见。
    expect(screen.getByText("{not-valid-json")).toBeTruthy();
  });

  it("服务端耗时缺省或为 0、请求头缺省时不渲染对应项（不显示假 0）", async () => {
    overrideEventList([
      customEvent({ durationServerMs: 0 }),
      customEvent({ eventId: "audit-custom-2" }),
    ]);
    const user = userEvent.setup();
    renderWorkbench();

    const rows = await screen.findAllByRole("button", { name: /审计事件：/ });
    expect(rows).toHaveLength(2);

    // 第一条：durationServerMs=0（未进入契约路由）→ 不渲染服务端耗时；请求头缺省 → 整块不渲染。
    await user.click(rows[0]!);
    expect(await screen.findByText("请求 ID")).toBeTruthy();
    expect(screen.queryByText("服务端耗时")).toBeNull();
    expect(screen.queryByText("请求头（已过滤）")).toBeNull();

    // 第二条：durationServerMs 与 httpHeaders 均缺省 → 同样不渲染。
    await user.click(rows[1]!);
    expect(await screen.findByText("请求 ID")).toBeTruthy();
    expect(screen.queryByText("服务端耗时")).toBeNull();
    expect(screen.queryByText("请求头（已过滤）")).toBeNull();
  });
});
