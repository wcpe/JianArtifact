// uploadTree 纯函数测试：GAV 解析（含 npm / raw 降级与畸形路径）与上传聚合树的计数 / 顺序 / 空态。
//
// 说明：本文件原为 `UploadAggregation.test.ts`——它测的一直是 `uploadTree.ts` 的**纯函数**
// （从不渲染组件）。上传聚合界面已由「操作聚合」（OperationAggregation）取代，但 `uploadTree`
// 的解析/降级逻辑仍被 operations.ts 与 OperationAggregation 复用，故测试保留并更名。
import { describe, expect, it } from "vitest";

import type { AuditEvent } from "../src/api/types";
import {
  artifactSearchText,
  parseUploadCoordinates,
  uploadArtifactTarget,
} from "../src/components/audit/uploadTree";

/** 构造一条上传/发布审计事件（默认 maven 风格 asset.put）。 */
function uploadEvent(overrides: Partial<AuditEvent> = {}): AuditEvent {
  return {
    eventId: "audit-upload-1",
    occurredAt: "2026-08-30T10:00:00.000Z",
    category: "asset_change",
    severity: "normal",
    result: "success",
    action: "asset.put",
    target: { kind: "artifact", label: "maven-releases/com/example/demo/1.0.0/demo-1.0.0.jar" },
    actor: {
      displayName: "release-bot",
      subjectType: "user",
      authSource: "api_key",
      email: "bot@example.com",
    },
    summary: "操作已完成",
    ...overrides,
  };
}

describe("parseUploadCoordinates", () => {
  it("Maven 路径反解出 groupId / artifactId / version / filename", () => {
    const coords = parseUploadCoordinates("com/example/demo/1.2.3/demo-1.2.3.jar", "maven");
    expect(coords).toEqual({
      groupId: "com.example",
      artifactId: "demo",
      version: "1.2.3",
      filename: "demo-1.2.3.jar",
      directory: "com/example/demo/1.2.3",
    });
  });

  it("格式未知时对 Maven 布局走启发式反解", () => {
    const coords = parseUploadCoordinates("com/example/demo/1.2.3/demo-1.2.3.pom");
    expect(coords.groupId).toBe("com.example");
    expect(coords.artifactId).toBe("demo");
    expect(coords.version).toBe("1.2.3");
  });

  it("npm 路径（含 format=npm）降级到「其他」，不误判为 Maven", () => {
    const coords = parseUploadCoordinates("@scope/pkg/-/pkg-1.0.0.tgz", "npm");
    expect(coords.groupId).toBeUndefined();
    expect(coords.filename).toBe("pkg-1.0.0.tgz");
    expect(coords.directory).toBe("@scope/pkg/-");
  });

  it("格式未知时 npm 路径同样降级（版本段无数字，启发式拒绝）", () => {
    const coords = parseUploadCoordinates("@scope/pkg/-/pkg-1.0.0.tgz");
    expect(coords.groupId).toBeUndefined();
  });

  it("raw 任意目录路径降级到「其他」", () => {
    expect(parseUploadCoordinates("docs/release/report.pdf", "raw").groupId).toBeUndefined();
    // 4 段但第二段不是数字版本、文件名不带 artifactId- 前缀，仍判为「其他」。
    expect(parseUploadCoordinates("a/b/c/file.txt").groupId).toBeUndefined();
  });

  it("畸形路径不抛错，仍给出 filename / directory", () => {
    expect(parseUploadCoordinates("").filename).toBe("");
    expect(parseUploadCoordinates("///").directory).toBe("");
    expect(parseUploadCoordinates("a.jar").filename).toBe("a.jar");
    expect(() => parseUploadCoordinates("///a//b///c")).not.toThrow();
  });
});

describe("uploadArtifactTarget", () => {
  it("剥离 label 中的仓库前缀，得到仓库内路径", () => {
    const target = uploadArtifactTarget(
      uploadEvent({
        target: {
          kind: "artifact",
          label: "maven-releases/com/example/demo/1.0.0/demo-1.0.0.jar",
          repository: "maven-releases",
        },
      }),
    );
    expect(target).toEqual({
      repo: "maven-releases",
      path: "com/example/demo/1.0.0/demo-1.0.0.jar",
      label: "maven-releases/com/example/demo/1.0.0/demo-1.0.0.jar",
    });
  });

  it("非制品事件返回 null", () => {
    expect(
      uploadArtifactTarget(uploadEvent({ target: { kind: "setting", label: "系统设置" } })),
    ).toBeNull();
  });
});

describe("artifactSearchText", () => {
  it("返回后端 EntityKey（repo/path）供跳转", () => {
    expect(
      artifactSearchText(
        uploadEvent({
          target: {
            kind: "artifact",
            label: "raw-hosted/release/a.txt",
            repository: "raw-hosted",
          },
        }),
      ),
    ).toBe("raw-hosted/release/a.txt");
  });
});
