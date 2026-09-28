// 操作聚合界面测试：按类型分区、类型筛选、层级顺序（仓库 → GAV → 版本 → 文件）、
// 计数与汇总、非制品归入「其他」、空态、文件叶跳转回调。
import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import type { AuditEvent } from "../src/api/types";
import { OperationAggregation } from "../src/components/audit/OperationAggregation";
import { renderWithProviders } from "./harness";

interface ArtifactOverrides {
  eventId: string;
  /** 仓库内相对路径（不含仓库名）。 */
  path: string;
  action?: string;
  repo?: string;
  occurredAt?: string;
  result?: string;
  actorEmail?: string;
  operationId?: string;
}

/** 造一条制品事件；默认是 maven-releases 上的 asset.put。 */
function artifactEvent(overrides: ArtifactOverrides): AuditEvent {
  const repo = overrides.repo ?? "maven-releases";
  return {
    eventId: overrides.eventId,
    occurredAt: overrides.occurredAt ?? "2026-09-24T10:00:00.000Z",
    category: "asset_change",
    severity: "info",
    result: overrides.result ?? "success",
    action: overrides.action ?? "asset.put",
    target: { kind: "artifact", label: `${repo}/${overrides.path}`, repository: repo },
    actor: { email: overrides.actorEmail ?? "ci@example.com", displayName: "ci" },
    ...(overrides.operationId ? { operationId: overrides.operationId } : {}),
  } as unknown as AuditEvent;
}

/** 造一条非制品的管理类事件（归入「其他」，不参与坐标聚合）。 */
function managementEvent(overrides: Partial<AuditEvent> = {}): AuditEvent {
  return {
    eventId: "audit-setting-1",
    occurredAt: "2026-09-24T09:00:00.000Z",
    category: "management_change",
    severity: "info",
    result: "success",
    action: "settings.update",
    target: { kind: "setting", label: "系统设置" },
    actor: { email: "admin@example.com", displayName: "admin" },
    ...overrides,
  } as unknown as AuditEvent;
}

function renderAggregation(
  events: readonly AuditEvent[],
  options: { loading?: boolean; onJumpToEvent?: (keyword: string) => void } = {},
) {
  return renderWithProviders(
    <OperationAggregation
      events={events}
      loading={options.loading ?? false}
      onJumpToEvent={options.onJumpToEvent ?? (() => {})}
    />,
  );
}

describe("OperationAggregation", () => {
  it("按操作类型分区，汇总与分区计数正确", () => {
    renderAggregation([
      artifactEvent({ eventId: "u1", path: "com/example/demo/1.0.0/demo-1.0.0.jar" }),
      artifactEvent({ eventId: "u2", path: "com/example/demo/1.0.0/demo-1.0.0.pom" }),
      artifactEvent({
        eventId: "d1",
        action: "asset.delete",
        repo: "raw-hosted",
        path: "release/report.pdf",
      }),
      managementEvent({ eventId: "o1" }),
    ]);

    expect(screen.getByTestId("audit-op-aggregation")).toBeTruthy();
    // 三个类型分区都在；没有移动操作，故「移动」分区不渲染。
    expect(screen.getByTestId("audit-op-section-upload")).toBeTruthy();
    expect(screen.getByTestId("audit-op-section-delete")).toBeTruthy();
    expect(screen.getByTestId("audit-op-section-other")).toBeTruthy();
    expect(screen.queryByTestId("audit-op-section-move")).toBeNull();

    // 顶部汇总：3 次操作 · 4 个文件。
    expect(screen.getByTestId("audit-op-summary").textContent).toContain("3 次操作 · 4 个文件");

    // 上传分区：1 次操作 / 2 个文件。
    const upload = screen.getByTestId("audit-op-section-upload");
    expect(within(upload).getByText("上传")).toBeTruthy();
    expect(within(upload).getByText("1 次操作 / 2 个文件")).toBeTruthy();
    expect(within(upload).getByText("maven-releases")).toBeTruthy();

    // 删除分区：1 次操作 / 1 个文件，制品路径以 raw 目录层级展示。
    const del = screen.getByTestId("audit-op-section-delete");
    expect(within(del).getByText("删除")).toBeTruthy();
    expect(within(del).getByText("1 次操作 / 1 个文件")).toBeTruthy();
    expect(within(del).getByText("raw-hosted")).toBeTruthy();
    expect(within(del).getByText("release")).toBeTruthy();
    expect(within(del).getByRole("button", { name: "report.pdf" })).toBeTruthy();

    // 其他分区：非制品动作以动作名展示。
    const other = screen.getByTestId("audit-op-section-other");
    expect(within(other).getByText("其他")).toBeTruthy();
    expect(within(other).getByText("更新设置")).toBeTruthy();
  });

  it("类型筛选只保留对应类型的分区，并同步汇总", async () => {
    const user = userEvent.setup();
    renderAggregation([
      artifactEvent({ eventId: "u1", path: "com/example/demo/1.0.0/demo-1.0.0.jar" }),
      artifactEvent({
        eventId: "d1",
        action: "asset.delete",
        repo: "raw-hosted",
        path: "release/report.pdf",
      }),
      managementEvent({ eventId: "o1" }),
    ]);

    await user.click(screen.getByRole("radio", { name: "删除" }));

    expect(screen.getByTestId("audit-op-section-delete")).toBeTruthy();
    expect(screen.queryByTestId("audit-op-section-upload")).toBeNull();
    expect(screen.queryByTestId("audit-op-section-other")).toBeNull();
    expect(screen.getByTestId("audit-op-summary").textContent).toContain("1 次操作 · 1 个文件");

    // 切回「全部」后所有分区恢复。
    await user.click(screen.getByRole("radio", { name: "全部" }));
    expect(screen.getByTestId("audit-op-section-upload")).toBeTruthy();
    expect(screen.getByTestId("audit-op-section-delete")).toBeTruthy();
    expect(screen.getByTestId("audit-op-section-other")).toBeTruthy();
  });

  it("分区内层级为 仓库 → groupId → artifactId → 版本 → 文件，操作叶带时间/操作者/结果/文件数", () => {
    renderAggregation([
      artifactEvent({
        eventId: "u1",
        path: "com/example/demo/1.0.0/demo-1.0.0.jar",
        occurredAt: "2026-09-24T10:00:00.000Z",
        actorEmail: "ci@example.com",
      }),
      artifactEvent({
        eventId: "u2",
        path: "com/example/demo/1.0.0/demo-1.0.0.pom",
        occurredAt: "2026-09-24T10:01:00.000Z",
      }),
    ]);

    const section = screen.getByTestId("audit-op-section-upload");
    const repo = within(section).getByText("maven-releases");
    const group = within(section).getByText("com.example");
    const artifact = within(section).getByText("demo");
    const version = within(section).getByText("1.0.0");
    const jar = within(section).getByRole("button", { name: "demo-1.0.0.jar" });
    const pom = within(section).getByRole("button", { name: "demo-1.0.0.pom" });

    // 层级先后顺序（DOM 顺序）：仓库在前，文件在后。
    const ordered = [repo, group, artifact, version, jar, pom];
    for (let i = 0; i < ordered.length - 1; i += 1) {
      expect(
        ordered[i]!.compareDocumentPosition(ordered[i + 1]!) & Node.DOCUMENT_POSITION_FOLLOWING,
      ).toBeTruthy();
    }

    // 操作叶展示：结果徽章（成功）、操作者邮箱、文件数。
    expect(within(section).getByText("成功")).toBeTruthy();
    expect(within(section).getByText("ci@example.com")).toBeTruthy();
    expect(within(section).getAllByText("2 个文件").length).toBeGreaterThan(0);
  });

  it("非制品操作归入「其他」，以动作名展示且不编造坐标", () => {
    renderAggregation([managementEvent({ eventId: "o1" })]);

    const other = screen.getByTestId("audit-op-section-other");
    expect(within(other).getByText("更新设置")).toBeTruthy();
    // 未编造仓库 / 坐标层级。
    expect(within(other).queryByText("maven-releases")).toBeNull();
    expect(within(other).queryByText("com.example")).toBeNull();
    expect(screen.queryByTestId("audit-op-section-upload")).toBeNull();
    expect(screen.getByTestId("audit-op-summary").textContent).toContain("1 次操作 · 1 个文件");
  });

  it("空结果集：加载中显示加载文案，加载完成显示空态文案", () => {
    const { unmount } = renderAggregation([], { loading: true });
    expect(screen.getByTestId("audit-op-empty").textContent).toContain("正在加载当前节点审计");
    unmount();

    renderAggregation([], { loading: false });
    expect(screen.getByTestId("audit-op-empty").textContent).toContain(
      "当前结果集内没有可聚合的操作",
    );
  });

  it("点击文件叶触发 onJumpToEvent，关键字为 repo/path", async () => {
    const user = userEvent.setup();
    const onJumpToEvent = vi.fn();
    renderAggregation(
      [artifactEvent({ eventId: "u1", repo: "raw-hosted", path: "release/report.pdf" })],
      { onJumpToEvent },
    );

    await user.click(screen.getByRole("button", { name: "report.pdf" }));
    expect(onJumpToEvent).toHaveBeenCalledWith("raw-hosted/release/report.pdf");
  });
});
