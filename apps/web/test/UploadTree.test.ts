// uploadTree 纯函数测试：GAV 解析（含 npm / raw 降级与畸形路径）与上传聚合树的计数 / 顺序 / 空态。
//
// 说明：本文件原为 `UploadAggregation.test.ts`——它测的一直是 `uploadTree.ts` 的**纯函数**
// （从不渲染组件）。上传聚合界面已由「操作聚合」（OperationAggregation）取代，但 `uploadTree`
// 的解析/降级逻辑仍被 operations.ts 与 OperationAggregation 复用，故测试保留并更名。
import { describe, expect, it } from "vitest";

import type { AuditEvent } from "../src/api/types";
import {
  artifactSearchText,
  buildUploadTree,
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

describe("buildUploadTree", () => {
  const othersLabel = "其他";

  it("空结果集返回空树", () => {
    expect(buildUploadTree([], { othersLabel })).toEqual([]);
  });

  it("非上传动作与非制品事件不进入聚合", () => {
    const tree = buildUploadTree(
      [
        uploadEvent({ eventId: "e1", action: "asset.delete" }),
        uploadEvent({ eventId: "e2", target: { kind: "setting", label: "系统设置" } }),
      ],
      { othersLabel },
    );
    expect(tree).toEqual([]);
  });

  it("按 仓库 → groupId → artifactId → 版本 → 文件 聚合且计数正确", () => {
    const tree = buildUploadTree(
      [
        uploadEvent({
          eventId: "e1",
          occurredAt: "2026-08-30T10:00:00.000Z",
          target: {
            kind: "artifact",
            label: "maven-releases/com/example/demo/1.0.0/demo-1.0.0.jar",
            repository: "maven-releases",
          },
        }),
        uploadEvent({
          eventId: "e2",
          occurredAt: "2026-08-30T11:30:00.000Z",
          target: {
            kind: "artifact",
            label: "maven-releases/com/example/demo/1.0.0/demo-1.0.0.pom",
            repository: "maven-releases",
          },
        }),
        uploadEvent({
          eventId: "e3",
          occurredAt: "2026-08-30T12:00:00.000Z",
          target: {
            kind: "artifact",
            label: "maven-releases/com/example/demo/1.1.0/demo-1.1.0.jar",
            repository: "maven-releases",
          },
        }),
      ],
      { othersLabel },
    );

    expect(tree).toHaveLength(1);
    const repo = tree[0]!;
    expect(repo.kind).toBe("repository");
    expect(repo.label).toBe("maven-releases");
    expect(repo.count).toBe(3);
    expect(new Date(repo.latestAt).toISOString()).toBe("2026-08-30T12:00:00.000Z");

    const group = repo.children[0]!;
    expect([group.kind, group.label, group.count]).toEqual(["group", "com.example", 3]);

    const artifact = group.children[0]!;
    expect([artifact.kind, artifact.label, artifact.count]).toEqual(["artifact", "demo", 3]);

    // 版本按最近上传时间倒序：1.1.0（12:00）在 1.0.0（11:30）之前。
    expect(artifact.children.map((node) => node.label)).toEqual(["1.1.0", "1.0.0"]);
    const version110 = artifact.children[0]!;
    expect(version110.kind).toBe("version");
    expect(version110.count).toBe(1);
    expect(version110.children[0]).toMatchObject({ kind: "file", label: "demo-1.1.0.jar" });

    const version100 = artifact.children[1]!;
    expect(version100.count).toBe(2);
    // 同版本下两个文件按名称升序。
    expect(version100.children.map((node) => node.label)).toEqual([
      "demo-1.0.0.jar",
      "demo-1.0.0.pom",
    ]);
    // 文件叶子保留对应事件，可用于跳转。
    expect(version100.children[0]!.event?.eventId).toBe("e1");
  });

  it("非 Maven 路径归入「其他」分支，按目录聚合，且排在最后", () => {
    const tree = buildUploadTree(
      [
        uploadEvent({
          eventId: "npm-1",
          action: "npm.publish",
          target: {
            kind: "artifact",
            label: "npm-hosted/@scope/pkg/-/pkg-1.0.0.tgz",
            repository: "npm-hosted",
          },
        }),
        uploadEvent({
          eventId: "raw-1",
          target: {
            kind: "artifact",
            label: "raw-hosted/release/report.pdf",
            repository: "raw-hosted",
          },
        }),
        uploadEvent({
          eventId: "mvn-1",
          target: {
            kind: "artifact",
            label: "maven-releases/com/example/demo/1.0.0/demo-1.0.0.jar",
            repository: "maven-releases",
          },
        }),
      ],
      { othersLabel },
    );

    // 仓库按名称升序。
    expect(tree.map((node) => node.label)).toEqual(["maven-releases", "npm-hosted", "raw-hosted"]);

    // npm-hosted 下只有「其他」分支，其下按目录聚合并落到文件叶子。
    const npmRepo = tree.find((node) => node.label === "npm-hosted")!;
    const npmOthers = npmRepo.children[0]!;
    expect([npmOthers.kind, npmOthers.label]).toEqual(["others", othersLabel]);
    expect(npmOthers.children[0]!.kind).toBe("directory");
    expect(npmOthers.children[0]!.label).toBe("@scope/pkg/-");
    expect(npmOthers.children[0]!.children[0]).toMatchObject({
      kind: "file",
      label: "pkg-1.0.0.tgz",
    });

    // raw-hosted 下同样是「其他」分支。
    const rawRepo = tree.find((node) => node.label === "raw-hosted")!;
    expect(rawRepo.children[0]!.kind).toBe("others");
    expect(rawRepo.children[0]!.children[0]!.label).toBe("release");
    expect(rawRepo.children[0]!.children[0]!.children[0]!.label).toBe("report.pdf");
  });

  it("同一目录下多个文件合并到同一目录节点并累计计数", () => {
    const tree = buildUploadTree(
      [
        uploadEvent({
          eventId: "r1",
          target: {
            kind: "artifact",
            label: "raw-hosted/release/a.txt",
            repository: "raw-hosted",
          },
        }),
        uploadEvent({
          eventId: "r2",
          target: {
            kind: "artifact",
            label: "raw-hosted/release/b.txt",
            repository: "raw-hosted",
          },
        }),
      ],
      { othersLabel },
    );

    const others = tree[0]!.children[0]!;
    expect(others.count).toBe(2);
    const dir = others.children[0]!;
    expect([dir.kind, dir.label, dir.count]).toEqual(["directory", "release", 2]);
    expect(dir.children.map((node) => node.label)).toEqual(["a.txt", "b.txt"]);
  });

  it("artifactSearchText 返回后端 EntityKey（repo/path）供跳转", () => {
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
