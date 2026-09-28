// 操作聚合纯函数测试：归并优先级、时间窗、非 Maven 降级、不丢事件。
import { describe, expect, it } from "vitest";

import type { AuditEvent } from "../src/api/types";
import {
  buildOperations,
  operationKindOf,
  OPERATION_WINDOW_MS,
} from "../src/components/audit/operations";

/** 造一个制品事件；默认是 maven-releases 上的 asset.put。 */
function artifactEvent(overrides: {
  eventId: string;
  path: string;
  action?: string;
  occurredAt?: string;
  operationId?: string;
  repo?: string;
  actorEmail?: string;
  result?: string;
}): AuditEvent {
  return {
    eventId: overrides.eventId,
    occurredAt: overrides.occurredAt ?? "2026-09-24T10:00:00.000Z",
    category: "artifact",
    severity: "info",
    result: overrides.result ?? "ok",
    action: overrides.action ?? "asset.put",
    target: {
      kind: "artifact",
      label: `${overrides.repo ?? "maven-releases"}/${overrides.path}`,
      repository: overrides.repo ?? "maven-releases",
    },
    actor: { email: overrides.actorEmail ?? "ci@example.com", displayName: "ci" },
    summary: "操作已完成",
    ...(overrides.operationId ? { operationId: overrides.operationId } : {}),
  } as unknown as AuditEvent;
}

describe("operationKindOf", () => {
  it("按 action 归类上传 / 删除 / 移动 / 其他", () => {
    expect(operationKindOf("asset.put")).toBe("upload");
    expect(operationKindOf("npm.publish")).toBe("upload");
    expect(operationKindOf("oci.blob.put")).toBe("upload");
    expect(operationKindOf("asset.delete")).toBe("delete");
    expect(operationKindOf("asset.move")).toBe("move");
    expect(operationKindOf("asset.rename")).toBe("move");
    expect(operationKindOf("repo.create")).toBe("other");
    expect(operationKindOf("")).toBe("other");
  });
});

describe("buildOperations 归并", () => {
  it("同版本多文件（jar/pom/xml/sha1/md5）并成一个操作", () => {
    const base = "com/example/demo/1.0.0";
    const events = [
      artifactEvent({ eventId: "e1", path: `${base}/demo-1.0.0.jar` }),
      artifactEvent({ eventId: "e2", path: `${base}/demo-1.0.0.pom` }),
      artifactEvent({ eventId: "e3", path: `${base}/demo-1.0.0.jar.sha1` }),
      artifactEvent({ eventId: "e4", path: `${base}/demo-1.0.0.jar.md5` }),
      artifactEvent({ eventId: "e5", path: `${base}/maven-metadata.xml` }),
    ];
    const ops = buildOperations(events);
    expect(ops).toHaveLength(1);
    expect(ops[0]!.kind).toBe("upload");
    expect(ops[0]!.events).toHaveLength(5);
    expect(ops[0]!.coordinates).toMatchObject({
      groupId: "com.example",
      artifactId: "demo",
      version: "1.0.0",
    });
  });

  it("不同版本 / 不同制品 / 不同仓库不并账", () => {
    const events = [
      artifactEvent({ eventId: "e1", path: "com/example/demo/1.0.0/demo-1.0.0.jar" }),
      artifactEvent({ eventId: "e2", path: "com/example/demo/1.0.1/demo-1.0.1.jar" }),
      artifactEvent({ eventId: "e3", path: "com/example/other/1.0.0/other-1.0.0.jar" }),
      artifactEvent({
        eventId: "e4",
        path: "com/example/demo/1.0.0/demo-1.0.0.jar",
        repo: "maven-snapshots",
      }),
    ];
    expect(buildOperations(events)).toHaveLength(4);
  });

  it("不同操作者不并账（同一坐标同一时间窗）", () => {
    const events = [
      artifactEvent({ eventId: "e1", path: "g/a/1.0/a-1.0.jar", actorEmail: "a@x.com" }),
      artifactEvent({ eventId: "e2", path: "g/a/1.0/a-1.0.pom", actorEmail: "b@x.com" }),
    ];
    expect(buildOperations(events)).toHaveLength(2);
  });

  it("同坐标但操作类型不同不并账（上传与删除是两回事）", () => {
    const events = [
      artifactEvent({ eventId: "e1", path: "g/a/1.0/a-1.0.jar" }),
      artifactEvent({ eventId: "e2", path: "g/a/1.0/a-1.0.jar", action: "asset.delete" }),
    ];
    const ops = buildOperations(events);
    expect(ops).toHaveLength(2);
    expect(ops.map((o) => o.kind).sort()).toEqual(["delete", "upload"]);
  });

  it("operationId 权威归并：即使坐标不同也并成一个操作", () => {
    const events = [
      artifactEvent({ eventId: "e1", path: "g/a/1.0/a-1.0.jar", operationId: "op-1" }),
      artifactEvent({ eventId: "e2", path: "g/a/1.0/a-1.0.pom", operationId: "op-1" }),
      // 同一 operationId 但换成另一个制品——仍属同一次操作
      artifactEvent({ eventId: "e3", path: "g/b/1.0/b-1.0.jar", operationId: "op-1" }),
    ];
    const ops = buildOperations(events);
    expect(ops).toHaveLength(1);
    expect(ops[0]!.key).toBe("op:op-1");
    expect(ops[0]!.events).toHaveLength(3);
  });

  it("operationId 优先于派生键（带 opId 的事件不会与无 opId 的按坐标并账）", () => {
    const events = [
      artifactEvent({ eventId: "e1", path: "g/a/1.0/a-1.0.jar", operationId: "op-1" }),
      artifactEvent({ eventId: "e2", path: "g/a/1.0/a-1.0.pom" }),
    ];
    expect(buildOperations(events)).toHaveLength(2);
  });

  it("跨 15 分钟边界的同一次操作仍并成一个（窗口按锚点而非翻转桶）", () => {
    // 两个事件只隔 1 秒，但恰好落在 15 分钟桶的两侧。
    const events = [
      artifactEvent({
        eventId: "e1",
        path: "g/a/1.0/a-1.0.jar",
        occurredAt: "2026-09-24T10:14:59.000Z",
      }),
      artifactEvent({
        eventId: "e2",
        path: "g/a/1.0/a-1.0.pom",
        occurredAt: "2026-09-24T10:15:00.000Z",
      }),
    ];
    const ops = buildOperations(events);
    expect(ops).toHaveLength(1);
    expect(ops[0]!.events).toHaveLength(2);
  });

  it("超出时间窗的同坐标事件不并账", () => {
    const first = new Date("2026-09-24T10:00:00.000Z").getTime();
    const later = new Date(first + OPERATION_WINDOW_MS).toISOString();
    const events = [
      artifactEvent({ eventId: "e1", path: "g/a/1.0/a-1.0.jar" }),
      artifactEvent({ eventId: "e2", path: "g/a/1.0/a-1.0.pom", occurredAt: later }),
    ];
    expect(buildOperations(events)).toHaveLength(2);
  });

  it("非 Maven（npm tarball）降级到目录聚合并归为上传", () => {
    const events = [
      artifactEvent({ eventId: "e1", path: "@scope/pkg/-/pkg-1.0.0.tgz", action: "npm.publish" }),
      artifactEvent({
        eventId: "e2",
        path: "@scope/pkg/-/pkg-1.0.0.tgz.sha1",
        action: "npm.publish",
      }),
    ];
    const ops = buildOperations(events);
    expect(ops).toHaveLength(1);
    expect(ops[0]!.coordinates?.groupId).toBeUndefined();
    expect(ops[0]!.coordinates?.directory).toBe("@scope/pkg/-");
  });

  it("非制品事件各自成一个操作，不参与坐标聚合", () => {
    const events = [
      artifactEvent({ eventId: "n1", path: "", action: "repo.create" }),
      artifactEvent({ eventId: "n2", path: "", action: "user.update" }),
    ];
    const ops = buildOperations(events);
    expect(ops).toHaveLength(2);
    expect(ops.every((o) => o.kind === "other")).toBe(true);
  });

  it("空集合返回空数组", () => {
    expect(buildOperations([])).toEqual([]);
  });

  it("操作按最新时间倒序，且内部事件也倒序", () => {
    const events = [
      artifactEvent({
        eventId: "old",
        path: "g/a/1.0/a-1.0.jar",
        occurredAt: "2026-09-24T09:00:00.000Z",
      }),
      artifactEvent({
        eventId: "new",
        path: "g/b/1.0/b-1.0.jar",
        occurredAt: "2026-09-24T11:00:00.000Z",
      }),
      artifactEvent({
        eventId: "mid",
        path: "g/a/1.0/a-1.0.pom",
        occurredAt: "2026-09-24T09:05:00.000Z",
      }),
    ];
    const ops = buildOperations(events);
    expect(ops[0]!.repo).toBe("maven-releases");
    expect(ops[0]!.events[0]!.eventId).toBe("new");
    // 第二个操作内部：mid（09:05）在 old（09:00）之前
    expect(ops[1]!.events.map((e) => e.eventId)).toEqual(["mid", "old"]);
  });

  it("不丢事件：所有输入事件都出现在某个操作里", () => {
    const events = [
      artifactEvent({ eventId: "e1", path: "g/a/1.0/a-1.0.jar", operationId: "op-1" }),
      artifactEvent({ eventId: "e2", path: "g/a/1.0/a-1.0.pom" }),
      artifactEvent({ eventId: "e3", path: "", action: "setting.update" }),
      artifactEvent({ eventId: "e4", path: "g/a/2.0/a-2.0.jar", action: "asset.delete" }),
    ];
    const ops = buildOperations(events);
    const seen = ops.flatMap((op) => op.events.map((e) => e.eventId)).sort();
    expect(seen).toEqual(["e1", "e2", "e3", "e4"]);
  });
});
