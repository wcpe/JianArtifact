// 契约一致性测试（对齐 AC-06 / AC-0.1.0-3）：
// 用 ajv 以 api/openapi.yaml 为唯一真源校验 mock 处理器输出；
// 任何 mock 偏离契约（漂移）都会使断言失败。
import Ajv from "ajv";
import { describe, expect, it } from "vitest";

import {
  mockAclList,
  mockAssetList,
  mockBatchDeleteAssets,
  mockConnectionStatus,
  mockHealthz,
  mockLoginResponse,
  mockReadyz,
  mockRepository,
  mockRepositoryList,
  mockReplicationApplyLogList,
  mockStatus,
  mockTokenCreated,
  mockTokenList,
  mockUnavailable,
  mockUsageInfo,
  mockUser,
  mockUserList,
} from "../src/handlers";
import { observabilityStore, resetObservabilityStore } from "../src/observability";
import { allSchemas, loadOpenApi, schemaFor } from "../src/openapi";

const ajv = new Ajv({ strict: false, allErrors: true });

// 预注册全部组件 schema，令交叉 $ref（如 UserList → User）可被 ajv 解析。
for (const [id, schema] of Object.entries(allSchemas())) {
  if (!ajv.getSchema(id)) {
    ajv.addSchema(schema, id);
  }
}

// 用契约中的组件 schema 校验一个 mock 值；漂移即失败。
function expectValid(schemaName: string, value: unknown): void {
  const validate = ajv.compile(schemaFor(schemaName));
  const ok = validate(value);
  if (!ok) {
    throw new Error(`${schemaName} 校验失败：${ajv.errorsText(validate.errors)}`);
  }
  expect(ok).toBe(true);
}

describe("devmock ↔ OpenAPI 契约一致性", () => {
  it("健康 / 状态类响应满足契约", () => {
    expectValid("HealthStatus", mockHealthz());
    expectValid("HealthStatus", mockReadyz());
    expectValid("Error", mockUnavailable());
    expectValid("StatusInfo", mockStatus());
  });

  it("认证 / 用户类响应满足契约", () => {
    expectValid("LoginResponse", mockLoginResponse());
    expectValid("User", mockUser());
    expectValid("UserList", mockUserList());
  });

  it("令牌类响应满足契约", () => {
    expectValid("TokenList", mockTokenList());
    expectValid("TokenCreated", mockTokenCreated());
  });

  it("仓库 / ACL 类响应满足契约", () => {
    expectValid("Repository", mockRepository());
    expectValid("RepositoryList", mockRepositoryList());
    expectValid("AclList", mockAclList());
  });

  it("连接状态响应满足契约并接受真实枚举", () => {
    expectValid("ConnectionStatus", mockConnectionStatus());
    expectValid("ConnectionStatus", { status: "AVAILABLE", description: "上游可用" });
    expectValid("ConnectionStatus", { status: "OFFLINE", description: "已手动离线" });
    const validate = ajv.compile(schemaFor("ConnectionStatus"));
    // 非法枚举应被契约拒绝（漂移可检出）。
    expect(validate({ status: "UNKNOWN_STATE" })).toBe(false);
    // 缺 required 的 status 应被契约拒绝。
    expect(validate({ description: "缺少状态" })).toBe(false);
  });

  it("制品浏览 / 使用片段响应满足契约", () => {
    expectValid("AssetList", mockAssetList());
    expectValid("UsageInfo", mockUsageInfo());
  });

  it("批量删除响应满足契约", () => {
    expectValid("BatchDeleteAssetsResponse", mockBatchDeleteAssets());
    expectValid("BatchDeleteAssetsRequest", {
      paths: ["a/1.txt", "a/2.txt"],
      overrideReason: "旧接口兼容测试",
    });
  });

  it("统一制品操作的全部失败响应都要求 operationId 并声明 500", () => {
    const operationError = schemaFor("AssetOperationError");
    expect(operationError.required).toContain("operationId");
    expectValid("AssetOperationError", {
      operationId: "018f0000-0000-7000-8000-000000000001",
      error: { code: "internal_error", message: "内部错误" },
    });
    const validateOperationError = ajv.compile(operationError);
    expect(validateOperationError({ error: { code: "internal_error", message: "内部错误" } })).toBe(
      false,
    );

    const doc = loadOpenApi() as unknown as {
      paths: Record<string, { post?: { responses?: Record<string, { $ref?: string }> } }>;
    };
    for (const endpoint of [
      "/api/v1/repositories/{name}/assets/batch-delete",
      "/api/v1/repositories/{name}/assets/operations",
    ]) {
      const responses = doc.paths[endpoint]?.post?.responses;
      for (const status of ["400", "401", "403", "404", "409", "500"]) {
        expect(responses?.[status]?.$ref).toContain("AssetOperation");
      }
    }
  });

  it("复制接收审计响应满足契约并接受真实状态枚举", () => {
    expectValid("ReplicationApplyLogList", mockReplicationApplyLogList());
    const validate = ajv.compile(schemaFor("ReplicationApplyLog"));
    expect(
      validate({ ...mockReplicationApplyLogList().items[0], result: "not-a-real-result" }),
    ).toBe(false);
  });

  it("统一审计概览、事件、风险批次确认和通知均满足 OpenAPI 契约", () => {
    resetObservabilityStore();
    const query = { categories: [], results: [] };
    const summary = observabilityStore.summary(query);
    expectValid("AuditObservabilitySummary", summary);

    const events = observabilityStore.events(query, summary.snapshot, 0, 50);
    expect(events).not.toBe("stale");
    expectValid("AuditEventPage", events);
    const firstEvent = (events as { items: { eventId: string }[] }).items[0]!;
    expectValid("AuditEventDetail", observabilityStore.eventDetail(firstEvent.eventId));

    const notifications = observabilityStore.notifications();
    expectValid("AuditAttentionNotificationList", notifications);
    const attentionId = notifications.items[0]!.attentionId;
    expectValid("AuditAttentionDetail", observabilityStore.attention(attentionId, 0, 50));
    expectValid(
      "AcknowledgeAuditAttentionResponse",
      observabilityStore.acknowledge(attentionId, {
        displayName: "admin",
        subjectType: "user",
        userId: 1,
        authSource: "web",
      }),
    );
  });

  it("在线迁移认证只允许匿名、Basic 或 Bearer，且秘密字段仅写入", () => {
    expectValid("MigrationSourceAuth", { type: "anonymous" });
    expectValid("MigrationSourceAuth", {
      type: "basic",
      username: "nexus-user",
      password: "private-password",
    });
    expectValid("MigrationSourceAuth", { type: "bearer", token: "private-token" });

    const validate = ajv.compile(schemaFor("MigrationSourceAuth"));
    expect(validate({ type: "basic", username: "nexus-user" })).toBe(false);
    expect(validate({ type: "bearer", token: "private-token", username: "nexus-user" })).toBe(
      false,
    );

    const schemas = loadOpenApi() as unknown as {
      components: {
        schemas: Record<string, { properties?: Record<string, { writeOnly?: boolean }> }>;
      };
    };
    expect(
      schemas.components.schemas.MigrationSourceAuthBasic?.properties?.password?.writeOnly,
    ).toBe(true);
    expect(schemas.components.schemas.MigrationSourceAuthBearer?.properties?.token?.writeOnly).toBe(
      true,
    );
  });

  it("远程 Nexus 仓库索引接受在线来源配置和仅写入认证", () => {
    expectValid("RemoteNexusRepositoryRequest", {
      sourceConfig: { url: "https://nexus.example" },
      sourceAuth: { type: "bearer", token: "private-token" },
    });

    const validate = ajv.compile(schemaFor("RemoteNexusRepositoryRequest"));
    expect(validate({ sourceRef: "NEXUS_TEST" })).toBe(false);
    expect(validate({ sourceConfig: { path: "/data/nexus" } })).toBe(false);
  });

  it("迁移任务响应只包含安全来源地址和认证类型", () => {
    expectValid("MigrationSourceConfig", { url: "https://nexus.example" });
    const validateSourceConfig = ajv.compile(schemaFor("MigrationSourceConfig"));
    expect(
      validateSourceConfig({ url: "https://nexus.example", password: "private-password" }),
    ).toBe(false);

    expectValid("MigrationTask", {
      id: 1,
      status: "planned",
      sourceType: "online_rest",
      sourceConfig: { url: "https://nexus.example" },
      sourceAuthType: "basic",
      conflictPolicy: "skip",
      createdAt: "2026-08-24T00:00:00Z",
      updatedAt: "2026-08-24T00:00:00Z",
    });

    const validate = ajv.compile(schemaFor("MigrationTask"));
    expect(
      validate({
        id: 1,
        status: "planned",
        sourceType: "online_rest",
        sourceConfig: { url: "https://nexus.example" },
        sourceAuth: { type: "bearer", token: "private-token" },
        conflictPolicy: "skip",
        createdAt: "2026-08-24T00:00:00Z",
        updatedAt: "2026-08-24T00:00:00Z",
      }),
    ).toBe(false);
  });

  it("契约漂移可被检出：缺 required 字段或非法枚举的响应校验失败", () => {
    const validate = ajv.compile(schemaFor("HealthStatus"));
    // 故意构造漂移响应：缺 version（required）且 status 非契约枚举
    expect(validate({ status: "healthy" })).toBe(false);

    // 错误信封漂移：扁平 {code,message} 不再满足嵌套 Error 契约
    const validateErr = ajv.compile(schemaFor("Error"));
    expect(validateErr({ code: "x", message: "y" })).toBe(false);
  });
});
