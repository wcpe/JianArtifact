// 契约一致性测试（对齐 AC-06 / AC-0.1.0-3）：
// 用 ajv 以 api/openapi.yaml 为唯一真源校验 mock 处理器输出；
// 任何 mock 偏离契约（漂移）都会使断言失败。
import Ajv from "ajv";
import { describe, expect, it } from "vitest";

import {
  mockAclList,
  mockAddUserGroupMemberRequest,
  mockAssetList,
  mockBatchDeleteAssets,
  mockConnectionStatus,
  mockCreateUserGroupRequest,
  mockHalfOpenConnectionStatus,
  mockHealthz,
  mockLoginResponse,
  mockMaintenanceJobList,
  mockMaintenanceJobRunResult,
  mockPinnedRepositories,
  mockPublicRepositoryList,
  mockPublishPoliciesBatchRequest,
  mockPublishPoliciesBatchResponse,
  mockPutPinnedRepositories,
  mockReadyz,
  mockRepository,
  mockRepositoryList,
  mockStatus,
  mockTokenCreated,
  mockTokenList,
  mockUnavailable,
  mockUpdateUserGroupRequest,
  mockUsageInfo,
  mockUser,
  mockUserGroup,
  mockUserGroupList,
  mockUserGroupMember,
  mockUserGroupMemberList,
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

  it("用户组与成员响应满足契约，主体类型漂移可被检出", () => {
    expectValid("UserGroup", mockUserGroup());
    expectValid("UserGroupList", mockUserGroupList());
    expectValid("UserGroupMember", mockUserGroupMember());
    expectValid("UserGroupMemberList", mockUserGroupMemberList());
    expectValid("CreateUserGroupRequest", mockCreateUserGroupRequest());
    expectValid("UpdateUserGroupRequest", mockUpdateUserGroupRequest());
    expectValid("AddUserGroupMemberRequest", mockAddUserGroupMemberRequest());

    // 漂移可检出：组缺 required 的 name / createdAt 必须被拒绝；成员缺 userId 同理。
    const validateGroup = ajv.compile(schemaFor("UserGroup"));
    expect(validateGroup({ id: 1, description: "缺组名" })).toBe(false);
    const validateMember = ajv.compile(schemaFor("UserGroupMember"));
    expect(validateMember({ username: "publisher", createdAt: "2026-01-01T00:00:00Z" })).toBe(
      false,
    );
    // 建组请求必须带 name（required）。
    const validateCreate = ajv.compile(schemaFor("CreateUserGroupRequest"));
    expect(validateCreate({ description: "只有说明" })).toBe(false);
    // 加入成员必须带 userId（required）。
    const validateAdd = ajv.compile(schemaFor("AddUserGroupMemberRequest"));
    expect(validateAdd({})).toBe(false);
  });

  it("ACL 条目接受六档动作与用户组主体，非法取值可被检出", () => {
    // 用户主体：缺省 subjectType 即 user，subjectId 有值。
    expectValid("AclEntry", { subjectId: 1, action: "read" });
    // 组主体：subjectType=group + subjectGroupId。
    expectValid("AclEntry", { subjectType: "group", subjectGroupId: 7, action: "publish" });
    // 六档动作全部合法（FR-36 由三档扩为六档）。
    for (const action of ["read", "write", "publish", "delete", "acl_manage", "admin"]) {
      expectValid("AclEntry", { subjectId: 1, action });
    }

    const validate = ajv.compile(schemaFor("AclEntry"));
    // 漂移可检出：越界的动作枚举（既有的三档之外不得静默放行任意串）。
    expect(validate({ subjectId: 1, action: "superuser" })).toBe(false);
    // 缺 action（required）应被拒绝——主体字段已不再是 required。
    expect(validate({ subjectId: 1 })).toBe(false);
    // 主体类型只接受 user / group。
    expect(validate({ subjectType: "team", subjectGroupId: 7, action: "read" })).toBe(false);
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

  it("置顶仓库与公开列表响应满足契约", () => {
    expectValid("PinnedRepositoriesResponse", mockPinnedRepositories());
    expectValid("PutPinnedRepositoriesRequest", mockPutPinnedRepositories());
    expectValid("PublicRepositoryList", mockPublicRepositoryList());

    // 漂移可检出：置顶响应缺 names、公开列表缺 pinnedNames 都必须被拒绝。
    const validatePinned = ajv.compile(schemaFor("PinnedRepositoriesResponse"));
    expect(validatePinned({ repositoryIds: [1] })).toBe(false);
    const validatePublic = ajv.compile(schemaFor("PublicRepositoryList"));
    expect(validatePublic({ items: [], total: 0 })).toBe(false);
  });

  it("连接状态响应满足契约并接受真实枚举", () => {
    expectValid("ConnectionStatus", mockConnectionStatus());
    expectValid("ConnectionStatus", mockHalfOpenConnectionStatus());
    expectValid("ConnectionStatus", { status: "AVAILABLE", description: "上游可用" });
    expectValid("ConnectionStatus", { status: "OFFLINE", description: "已手动离线" });
    const validate = ajv.compile(schemaFor("ConnectionStatus"));
    // 非法枚举应被契约拒绝（漂移可检出）。
    expect(validate({ status: "UNKNOWN_STATE" })).toBe(false);
    // 缺 required 的 status 应被契约拒绝。
    expect(validate({ description: "缺少状态" })).toBe(false);
  });

  it("契约枚举含 HALF_OPEN（FR-43），缺它或漂移都能被检出", () => {
    // 正向：阻止态的六个取值全部合法，HALF_OPEN 不被当作未知状态拒绝。
    for (const status of [
      "READY",
      "AVAILABLE",
      "UNAVAILABLE",
      "AUTO_BLOCKED",
      "HALF_OPEN",
      "OFFLINE",
    ]) {
      expectValid("ConnectionStatus", { status });
    }

    // 反向：契约里的 HALF_OPEN 若被删回旧枚举，上面的正向断言与这里都会失败——
    // 直接读枚举确认两侧产物同源，避免「mock 恰好没用到该取值」掩盖漂移。
    const schema = schemaFor("ConnectionStatus") as {
      properties?: { status?: { enum?: string[] } };
    };
    expect(schema.properties?.status?.enum).toContain("HALF_OPEN");

    // 枚举区分大小写与写法：半开的近似写法不得被静默放行
    // （`half_open` 是内部 domain 常量的风格，`HalfOpen` 是 Go 类型风格，都不是契约取值）。
    const validate = ajv.compile(schemaFor("ConnectionStatus"));
    for (const near of ["half_open", "HalfOpen", "HALFOPEN", "半开"]) {
      expect(validate({ status: near })).toBe(false);
    }
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

  it("批量发布策略请求与逐仓库结果满足契约", () => {
    expectValid("PublishPoliciesBatchRequest", mockPublishPoliciesBatchRequest());
    expectValid("PublishPoliciesBatchResponse", mockPublishPoliciesBatchResponse());

    // 漂移可检出：缺 repositories（required）的请求、缺 results（required）的响应都必须被拒绝。
    const validateRequest = ajv.compile(schemaFor("PublishPoliciesBatchRequest"));
    expect(validateRequest({ allowedPrefixes: [] })).toBe(false);
    expect(validateRequest({ repositories: [] })).toBe(false);

    const validateResponse = ajv.compile(schemaFor("PublishPoliciesBatchResponse"));
    expect(validateResponse({})).toBe(false);
    // 逐仓库结果必须携带 repository 与 ok。
    expect(validateResponse({ results: [{ repository: "a" }] })).toBe(false);
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

  it("存储治理运维作业面与仓库配额字段满足契约，漂移可被检出", () => {
    expectValid("MaintenanceJobList", mockMaintenanceJobList());
    expectValid("MaintenanceJobRunResult", mockMaintenanceJobRunResult());

    // 漂移可检出：作业项缺 required（name 等）的清单必须被拒绝。
    const validateJob = ajv.compile(schemaFor("MaintenanceJob"));
    expect(validateJob({ intervalSeconds: 86400, running: false, runs: 0, failures: 0 })).toBe(
      false,
    );
    const validateJobList = ajv.compile(schemaFor("MaintenanceJobList"));
    expect(validateJobList({})).toBe(false);

    // 仓库配额：缺省即"不限"，因此省略 quotaBytes/quotaAssets 仍然合法；负数被 minimum 拒绝。
    const validateRepository = ajv.compile(schemaFor("Repository"));
    const withoutQuota: Record<string, unknown> = { ...mockRepository() };
    delete withoutQuota.quotaBytes;
    delete withoutQuota.quotaAssets;
    expect(validateRepository(withoutQuota)).toBe(true);
    expect(validateRepository({ ...mockRepository(), quotaBytes: -1 })).toBe(false);

    // 代理缓存保留天数：与配额同口径——缺省即"关闭"，仅 proxy 有语义；负数被 minimum 拒绝。
    // mockRepository() 是 hosted 仓库，按域层规则本就不该带该字段，故用 proxy 形状的对象校验。
    const validateRequest = ajv.compile(schemaFor("CreateRepositoryRequest"));
    expect(validateRequest({ name: "npm-proxy", format: "npm", type: "proxy" })).toBe(true);
    expect(
      validateRequest({
        name: "npm-proxy",
        format: "npm",
        type: "proxy",
        cacheRetentionDays: 30,
      }),
    ).toBe(true);
    expect(
      validateRequest({
        name: "npm-proxy",
        format: "npm",
        type: "proxy",
        cacheRetentionDays: -1,
      }),
    ).toBe(false);

    const validateUpdateRequest = ajv.compile(schemaFor("UpdateRepositoryRequest"));
    expect(validateUpdateRequest({ cacheRetentionDays: 0 })).toBe(true);
    expect(validateUpdateRequest({ cacheRetentionDays: -1 })).toBe(false);

    const validateProxyRepository = ajv.compile(schemaFor("Repository"));
    const proxyRepo = {
      ...mockRepository(),
      type: "proxy",
      remoteUrl: "https://repo1.maven.org/maven2",
      cacheRetentionDays: 30,
    };
    expect(validateProxyRepository(proxyRepo)).toBe(true);
    expect(validateProxyRepository({ ...proxyRepo, cacheRetentionDays: -1 })).toBe(false);
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
