// 契约一致性测试（对齐 AC-06 / AC-0.1.0-3）：
// 用 ajv 以 api/openapi.yaml 为唯一真源校验 mock 处理器输出；
// 任何 mock 偏离契约（漂移）都会使断言失败。
import Ajv from "ajv";
import { describe, expect, it } from "vitest";

import {
  mockAclList,
  mockAddUserGroupMemberRequest,
  mockAssetList,
  mockBackupDrillAccepted,
  mockBackupPackage,
  mockBackupPackageList,
  mockBackupVerificationList,
  mockBackupVerificationReport,
  mockBatchDeleteAssets,
  mockConnectionStatus,
  mockCreateUserGroupRequest,
  mockDrillBackupRequest,
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
    // 六档动作全部合法（由三档扩为六档）。
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

  it("契约枚举含 HALF_OPEN，缺它或漂移都能被检出", () => {
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

  it("旧审计预览字段在契约中只作可选的弃用字段保留", () => {
    const context = schemaFor("AuditHttpContext") as {
      required?: string[];
      properties: Record<string, { deprecated?: boolean }>;
    };
    for (const field of ["tokenPreview", "bodyPreview"]) {
      expect(context.required).not.toContain(field);
      expect(context.properties[field]?.deprecated).toBe(true);
    }
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

  // —— 来源列 + 恢复演练 ——
  it("备份包来源仅接受 manual / auto，且是 required（漂移可被检出）", () => {
    expectValid("BackupPackage", mockBackupPackage());
    expectValid("BackupPackageList", mockBackupPackageList());

    // 两种来源取值都合法；覆盖 mock 里真实出现的两种形状。
    const validate = ajv.compile(schemaFor("BackupPackage"));
    for (const origin of ["manual", "auto"]) {
      expect(validate({ ...mockBackupPackage(), origin })).toBe(true);
    }
    // 反向：origin 是 required——缺它必须被拒（否则前端拿到 undefined 会渲染成空白徽章）。
    const withoutOrigin: Record<string, unknown> = { ...mockBackupPackage() };
    delete withoutOrigin.origin;
    expect(validate(withoutOrigin)).toBe(false);
    // 反向：非法来源取值与大小写近似写法都不得被静默放行。
    for (const near of ["Manual", "AUTO", "automatic", "手动", ""]) {
      expect(validate({ ...mockBackupPackage(), origin: near })).toBe(false);
    }
    // 枚举本身必须含这两个取值（直接读契约，避免"mock 恰好没用到"掩盖漂移）。
    const originSchema = schemaFor("BackupPackageOrigin") as { enum?: string[] };
    expect(originSchema.enum).toEqual(["manual", "auto"]);
  });

  it("恢复演练的受理结果与报告满足契约，漂移可被检出", () => {
    expectValid("DrillBackupRequest", mockDrillBackupRequest());
    expectValid("DrillBackupRequest", {});
    expectValid("BackupDrillAccepted", mockBackupDrillAccepted());
    expectValid("BackupDrillCheck", mockBackupVerificationReport().checks[0]!);
    expectValid("BackupVerificationReport", mockBackupVerificationReport());
    expectValid("BackupVerificationList", mockBackupVerificationList());

    // 反向：报告缺 required 字段（id / level / ok / 时间 / durationMs / checks）必须被拒。
    const validateReport = ajv.compile(schemaFor("BackupVerificationReport"));
    const full = mockBackupVerificationReport();
    for (const key of [
      "id",
      "packageId",
      "level",
      "ok",
      "startedAt",
      "finishedAt",
      "durationMs",
      "checks",
    ]) {
      const partial: Record<string, unknown> = { ...full };
      delete partial[key];
      expect(validateReport(partial)).toBe(false);
    }
    // 反向：级别只接受 shallow / deep。
    expect(validateReport({ ...full, level: "full" })).toBe(false);
    // 反向：失败报告不因缺 errorSummary 而非法——它是可选字段（成功时本就没有）。
    const succeeded: Record<string, unknown> = { ...full, ok: true };
    delete succeeded.errorSummary;
    expect(validateReport(succeeded)).toBe(true);
    // 反向：受理结果缺 acceptedAt 必须被拒（轮询要靠它判断哪条报告属于本次演练）。
    const validateAccepted = ajv.compile(schemaFor("BackupDrillAccepted"));
    expect(validateAccepted({ packageId: "bk-a", level: "shallow" })).toBe(false);
    // 反向：请求体级别非法取值必须被拒。
    const validateRequest = ajv.compile(schemaFor("DrillBackupRequest"));
    expect(validateRequest({ level: "ultra" })).toBe(false);

    // 反向：检查项的 detail / name 是 required——报告必须能说清"哪项、为什么"。
    const validateCheck = ajv.compile(schemaFor("BackupDrillCheck"));
    expect(validateCheck({ name: "integrity", ok: true })).toBe(false);
    expect(validateCheck({ ok: true, detail: "缺少检查项名" })).toBe(false);
  });

  it("两个演练端点已登记在契约里且声明了预期状态码", () => {
    const doc = loadOpenApi() as unknown as {
      paths: Record<
        string,
        {
          post?: { operationId?: string; responses?: Record<string, unknown> };
          get?: { operationId?: string; responses?: Record<string, unknown> };
        }
      >;
    };
    const drill = doc.paths["/api/v1/backups/{id}/drill"]?.post;
    expect(drill?.operationId).toBe("drillBackup");
    // 202 是关键约定：深校验耗时与包体积同阶，端点必须异步受理而不是同步等待。
    expect(drill?.responses?.["202"]).toBeTruthy();
    expect(drill?.responses?.["404"]).toBeTruthy();
    expect(drill?.responses?.["409"]).toBeTruthy();

    const verifications = doc.paths["/api/v1/backups/{id}/verifications"]?.get;
    expect(verifications?.operationId).toBe("listBackupVerifications");
    expect(verifications?.responses?.["200"]).toBeTruthy();
    expect(verifications?.responses?.["404"]).toBeTruthy();
  });

  it("安全审计登记四条交互管理员查询及独立失败状态", () => {
    const root = "/api/v1/observability/audit";
    type Response = {
      $ref?: string;
      description?: string;
      content?: { "application/json": { schema: { $ref: string } } };
    };
    const doc = loadOpenApi() as unknown as {
      paths: Record<
        string,
        {
          get?: {
            operationId: string;
            description: string;
            security: unknown;
            responses: Record<string, Response>;
          };
        }
      >;
      components: { responses: Record<string, Response> };
    };
    const errors: Record<string, readonly [string, string?]> = {
      "400": ["InvalidSecurityAuditFilter", "invalid_security_filter"],
      "401": ["Unauthorized"],
      "403": ["Forbidden"],
      "404": ["SecurityAuditEventNotFound", "security_event_not_found"],
      "410": ["SecurityAuditCursorExpired", "security_cursor_expired"],
      "503": ["SecurityAuditUnavailable", "security_audit_unavailable"],
    };
    const routes = [
      ["/security-events", "listSecurityAuditEvents", "SecurityAuditEventPage", [400, 410]],
      ["/security-events/{eventId}", "getSecurityAuditEvent", "SecurityAuditEvent", [400, 404]],
      [
        "/security-events/{eventId}/objects",
        "listSecurityAuditEventObjects",
        "SecurityAuditEventObjectPage",
        [400, 404, 410],
      ],
      [
        "/collection-health",
        "getSecurityAuditCollectionHealth",
        "SecurityAuditCollectionHealth",
        [400],
      ],
    ] as const;
    for (const [path, operationId, schema, extraStatuses] of routes) {
      const operation = doc.paths[root + path]?.get;
      expect(operation?.operationId).toBe(operationId);
      expect(operation?.description).toContain("全局管理员交互会话");
      expect(operation?.security).toEqual([{ bearerAuth: [] }]);
      const responses = operation!.responses;
      expect(Object.keys(responses).sort()).toEqual(
        [200, 401, 403, 503, ...extraStatuses].sort().map(String),
      );
      expect(responses["200"]?.content?.["application/json"].schema.$ref).toBe(
        `#/components/schemas/${schema}`,
      );
      for (const [status, response] of Object.entries(responses)) {
        if (status === "200") continue;
        const [name, code] = errors[status]!;
        expect(response.$ref).toBe(`#/components/responses/${name}`);
        const error = doc.components.responses[name];
        expect(error?.content?.["application/json"].schema.$ref).toBe("#/components/schemas/Error");
        if (code) expect(error?.description).toContain(`\`${code}\``);
      }
    }
    expect(doc.paths[`${root}/events`]?.get?.operationId).toBe("listAuditObservabilityEvents");
    expect(doc.paths[`${root}/events/{eventId}`]?.get?.operationId).toBe(
      "getAuditObservabilityEvent",
    );
  });

  it("安全审计仅登记仓储支持的过滤条件与独立对象分页参数", () => {
    type Param = { name: string; in: string; required?: boolean; schema: Record<string, unknown> };
    const doc = loadOpenApi() as unknown as {
      paths: Record<
        string,
        { get: { parameters: (Param | { $ref: string })[]; description: string } }
      >;
      components: { parameters: Record<string, Param> };
    };
    const path = "/api/v1/observability/audit/security-events";
    const parametersFor = (route: string) =>
      Object.fromEntries(
        doc.paths[route]!.get.parameters.map((param) => {
          const resolved =
            "$ref" in param ? doc.components.parameters[param.$ref.split("/").pop()!]! : param;
          return [resolved.name, resolved];
        }),
      );
    const list = parametersFor(path);
    expect(Object.keys(list).sort()).toEqual(
      [
        "from",
        "to",
        "limit",
        "cursor",
        "ip",
        "cidr",
        "eventType",
        "outcome",
        "actorUserId",
        "repositoryId",
        "credentialId",
        "sessionId",
        "requestId",
        "originRequestId",
        "operationId",
        "taskId",
        "actionId",
        "canonicalPath",
        "objectId",
        "digest",
      ].sort(),
    );
    for (const name of ["from", "to"]) {
      const param = list[name]!;
      expect(param).toMatchObject({
        in: "query",
        required: true,
        schema: { type: "string", format: "date-time" },
      });
      const validate = ajv.compile(param.schema);
      for (const value of [
        "2026-01-01T00:00:00.1234Z",
        "2026-01-01T00:00:00.0000000001Z",
        "2026-01-01T05:30:00.123000000001+05:30",
        "2026-01-01T00:00:00.1200+24:00",
        "2026-01-01T00:00:00.1200-03:60",
        "2026-01-01T00:00:00.123",
      ]) {
        expect(validate(value), `${name} 不接受 ${value}`).toBe(false);
      }
      for (const value of [
        "2026-01-01T00:00:00Z",
        "2026-01-01T00:00:00.1Z",
        "2026-01-01T00:00:00.12Z",
        "2026-01-01T00:00:00.123Z",
        "2026-01-01T00:00:00.1230Z",
        "2026-01-01T05:30:00.123000000000+05:30",
        "2025-12-31T20:30:00.123000000000-03:30",
        "2026-01-01T00:00:00.000000000000Z",
      ]) {
        expect(validate(value), `${name} 接受 ${value}`).toBe(true);
      }
    }
    const detail = parametersFor(`${path}/{eventId}`);
    expect(Object.keys(detail)).toEqual(["eventId"]);
    const objects = parametersFor(`${path}/{eventId}/objects`);
    expect(Object.keys(objects).sort()).toEqual(["cursor", "eventId", "limit"]);
    expect(objects.eventId).toMatchObject({ in: "path", required: true, schema: { minLength: 1 } });
    for (const params of [list, objects]) {
      expect(params.limit?.schema).toMatchObject({
        type: "integer",
        minimum: 1,
        default: 50,
        maximum: 100,
      });
      expect(params.cursor?.required).not.toBe(true);
      expect(params.cursor?.schema).toMatchObject({ type: "string", minLength: 1 });
    }
    expect(doc.paths[path]!.get.description).toContain("UTC 半开区间");
    expect(doc.paths[path]!.get.description).toContain("30 天");
    expect(doc.paths[path]!.get.description).toContain("互斥");
    expect(doc.paths[path]!.get.description).toContain("重复");
  });

  it("安全审计事件和对象只接受脱敏字段，不借用业务回执时间", () => {
    const at = "2026-01-01T00:00:00.000Z";
    const event = {
      eventId: "event-a",
      eventVersion: 1,
      eventType: "auth.attempt",
      occurredAt: at,
      ingestedAt: at,
      actorKind: "unknown",
      outcome: "denied",
      committed: "not_applicable",
      proxyTrust: "unknown",
      errorCode: "authentication_failed",
    };
    expectValid("SecurityAuditEvent", event);
    const validateEvent = ajv.compile(schemaFor("SecurityAuditEvent"));
    for (const field of [
      "committedAt",
      "recordedAt",
      "body",
      "details",
      "tokenPreview",
      "jti",
      "error",
      "rawError",
      "accountEpoch",
      "securityEpoch",
      "retentionGeneration",
      "sourceTrustVersion",
    ]) {
      expect(validateEvent({ ...event, [field]: "不是公开字段" }), field).toBe(false);
    }
    expect(validateEvent({ ...event, outcome: "ok" })).toBe(false);
    const object = {
      ordinal: 1,
      objectKind: "asset",
      repositoryId: 1,
      canonicalPath: "example/file",
      sizeBytes: 0,
    };
    expectValid("SecurityAuditEventObject", object);
    const validateObject = ajv.compile(schemaFor("SecurityAuditEventObject"));
    expect(validateObject({ ...object, ordinal: 0 })).toBe(false);
    expect(validateObject({ ...object, sizeBytes: -1 })).toBe(false);
    expect(validateObject({ ...object, body: "不是对象字段" })).toBe(false);
  });

  it("候选与资格收窄契约仅公开批准的元数据、词表和错误", () => {
    const doc = loadOpenApi() as unknown as {
      paths: Record<
        string,
        Record<string, { operationId: string; responses: Record<string, unknown> }>
      >;
    };
    const update = doc.paths["/api/v1/users/{id}"]?.patch;
    expect(update?.operationId).toBe("updateUser");
    expect(update?.responses["409"]).toEqual({
      $ref: "#/components/responses/CredentialVersionConflict",
    });
    expect(update?.responses["503"]).toEqual({
      $ref: "#/components/responses/SecurityAuditUnavailable",
    });
    const path = doc.paths["/api/v1/security/admission/candidates/{operationId}"];
    expect(Object.keys(path ?? {})).toEqual(["get"]);
    expect(path?.get?.operationId).toBe("getPrivateAdmissionCandidate");
    expect(Object.keys(path!.get!.responses).sort()).toEqual(["200", "401", "403", "404", "503"]);
    const validate = ajv.compile(schemaFor("PrivateAdmissionCandidateStatus"));
    const body = {
      operationId: "f4f6df7b-a5d9-4f8a-9e90-8e92237cb582",
      repositoryId: 1,
      targetVersion: 2,
      state: "staged",
      coverage: "unknown",
      expiresAt: "2026-01-02T00:00:00Z",
      manifestVersion: 1,
      manifestDigest: `sha256:${"a".repeat(64)}`,
    };
    expect(validate(body)).toBe(true);
    expect(validate({ ...body, state: "published" })).toBe(false);
    expect(validate({ ...body, coverage: "complete" })).toBe(false);
    expect(validate({ ...body, operationId: "f4f6df7b-a5d9-7f8a-9e90-8e92237cb582" })).toBe(false);
    for (const key of [
      "source",
      "canonicalPath",
      "downloadUrl",
      "accountEpoch",
      "privateObjectKey",
    ]) {
      expect(validate({ ...body, [key]: "不公开" }), key).toBe(false);
    }
    const validateType = ajv.compile(schemaFor("SecurityAuditEventType"));
    expect(validateType("auth.account_access_changed")).toBe(true);
    expect(validateType("admission.candidate_staged")).toBe(true);
  });

  it("私有候选对账契约区分元数据查询与服务端状态更新", () => {
    const doc = loadOpenApi() as unknown as {
      paths: Record<string, { get?: { description: string; requestBody?: unknown } }>;
    };
    const get = doc.paths["/api/v1/security/admission/candidates/{operationId}"]?.get;
    expect(get?.description).toContain("可触发服务端幂等完整性或到期对账并持久更新状态");
    expect(get?.description).toContain("不接受客户端写入，不提供下载");
    expect(get?.description).toContain("不发放发布权或删除文件");
    expect(get?.description).not.toContain("只读");
    expect(get?.requestBody).toBeUndefined();
  });

  it.each([
    ["/api/v1/users/{id}/publish-policies", "putPublishPolicies"],
    ["/api/v1/users/{id}/publish-policies/{repo}", "putPublishPolicy"],
  ])("发布策略资格失败契约保留 %s 的通用冲突并声明嵌套错误", (path, operationId) => {
    type Response = {
      $ref?: string;
      description?: string;
      content?: { "application/json": { schema: { $ref: string } } };
    };
    const doc = loadOpenApi() as unknown as {
      paths: Record<string, { put?: { operationId: string; responses: Record<string, Response> } }>;
      components: { responses: Record<string, Response> };
    };
    const put = doc.paths[path]?.put;
    expect(put?.operationId).toBe(operationId);
    expect(Object.keys(put!.responses).sort()).toEqual([
      "200",
      "400",
      "401",
      "403",
      "404",
      "409",
      "503",
    ]);
    for (const [status, component, code] of [
      ["409", "PublishPolicyWriteConflict", "credential_version_conflict"],
      ["503", "SecurityAuditUnavailable", "security_audit_unavailable"],
    ] as const) {
      expect(put!.responses[status]?.$ref).toBe(`#/components/responses/${component}`);
      const error = doc.components.responses[component];
      expect(error?.description).toContain(`\`${code}\``);
      expect(error?.content?.["application/json"].schema.$ref).toBe("#/components/schemas/Error");
    }
    expect(doc.components.responses.PublishPolicyWriteConflict?.description).toContain(
      "发布策略冲突",
    );
  });

  it("改密提交事实与用户目标只公开稳定标识", () => {
    const validateType = ajv.compile(schemaFor("SecurityAuditEventType"));
    expect(validateType("auth.password_changed")).toBe(true);
    expect(validateType("auth.password_reset")).toBe(false);
    const at = "2026-01-01T00:00:00.000Z";
    const event = {
      eventId: "event-a",
      eventVersion: 1,
      eventType: "auth.password_changed",
      occurredAt: at,
      ingestedAt: at,
      actorKind: "user",
      actorUserId: 1,
      proxyTrust: "unknown",
      operationId: "operation-a",
      outcome: "succeeded",
      committed: "committed",
    };
    expectValid("SecurityAuditEvent", event);
    const validateEvent = ajv.compile(schemaFor("SecurityAuditEvent"));
    const validateObject = ajv.compile(schemaFor("SecurityAuditEventObject"));
    const target = { ordinal: 1, objectKind: "user", objectId: "2" };
    expect(validateObject(target)).toBe(true);
    expect(validateObject({ ...target, objectId: undefined })).toBe(false);
    for (const objectId of [2, "0", "-1", "02", "display-name"]) {
      expect(validateObject({ ...target, objectId }), String(objectId)).toBe(false);
    }
    expect(validateObject({ ...target, digest: "不属于账号目标" })).toBe(false);
    for (const field of ["password", "passwordHash", "accountEpoch", "securityEpoch"]) {
      expect(validateEvent({ ...event, [field]: "仅用于断言" }), field).toBe(false);
      expect(validateObject({ ...target, [field]: "仅用于断言" }), field).toBe(false);
    }
  });

  it("改密沿用旧请求和无响应体的 204，新增失败沿用嵌套错误", () => {
    type Response = {
      $ref?: string;
      description?: string;
      content?: { "application/json": { schema: { $ref: string } } };
    };
    const doc = loadOpenApi() as unknown as {
      paths: Record<
        string,
        {
          post?: {
            operationId: string;
            requestBody: Response;
            responses: Record<string, Response>;
          };
        }
      >;
      components: { responses: Record<string, Response> };
    };
    const operation = doc.paths["/api/v1/users/{id}/password"]?.post;
    expect(operation?.operationId).toBe("changePassword");
    expect(operation?.requestBody.content?.["application/json"].schema.$ref).toBe(
      "#/components/schemas/PasswordChangeRequest",
    );
    const responses = operation!.responses;
    expect(Object.keys(responses).sort()).toEqual([
      "204",
      "400",
      "401",
      "403",
      "404",
      "409",
      "503",
    ]);
    expect(responses["204"]?.content).toBeUndefined();
    expect(responses["204"]?.description).toContain(
      "同值经事务确认仍返回 204，不推进代次，不追加新的改密成功事件",
    );
    for (const [status, key, code] of [
      ["409", "CredentialVersionConflict", "credential_version_conflict"],
      ["503", "SecurityAuditUnavailable", "security_audit_unavailable"],
    ] as const) {
      expect(responses[status]?.$ref).toBe(`#/components/responses/${key}`);
      const error = doc.components.responses[key];
      expect(error?.description).toContain(`\`${code}\``);
      expect(error?.content?.["application/json"].schema.$ref).toBe("#/components/schemas/Error");
      expectValid("Error", { error: { code, message: "改密未确认为成功" } });
    }
  });

  it("安全审计页区分投影与覆盖且不返回总计数或内部锚点", () => {
    const page = {
      items: [],
      hasMore: false,
      snapshotWatermark: 0,
      projectionWatermark: 0,
      projectionHealth: "healthy",
      coverage: "unknown",
    };
    expectValid("SecurityAuditEventPage", page);
    const validateList = ajv.compile(schemaFor("SecurityAuditEventPage"));
    for (const field of Object.keys(page)) {
      const partial: Record<string, unknown> = { ...page };
      delete partial[field];
      expect(validateList(partial), field).toBe(false);
    }
    expect(validateList({ ...page, count: 0 })).toBe(false);
    expect(validateList({ ...page, projectionHealth: "gapped" })).toBe(false);
    expect(validateList({ ...page, coverage: "healthy" })).toBe(false);
    expectValid("SecurityAuditEventObjectPage", { items: [], hasMore: false });
    const validateObjects = ajv.compile(schemaFor("SecurityAuditEventObjectPage"));
    expect(validateObjects({ items: [], hasMore: true, nextCursor: "" })).toBe(false);
    expect(validateObjects({ items: [], hasMore: false, eventSequence: 1 })).toBe(false);
  });

  it("安全审计健康只公开有界状态和已知事实，通用错误保留嵌套外形", () => {
    const validatePublicHealth = ajv.compile(schemaFor("HealthStatus"));
    for (const status of ["healthy", "degraded", "unknown", "unavailable"]) {
      expect(validatePublicHealth({ ...mockHealthz(), auditCollection: status })).toBe(true);
    }
    expect(
      validatePublicHealth({ ...mockHealthz(), auditCollection: { clientIp: "192.0.2.2" } }),
    ).toBe(false);
    expect(schemaFor("Error").properties).toHaveProperty(
      "requestId",
      expect.objectContaining({ type: "string" }),
    );
    const error = { error: { code: "invalid_security_filter", message: "查询条件无效" } };
    expectValid("Error", error);
    expectValid("Error", { ...error, requestId: "server-request-a" });
    expect(ajv.compile(schemaFor("Error"))({ errorCode: "invalid_security_filter" })).toBe(false);

    const health = { state: "starting", coverage: "unknown" };
    expectValid("SecurityAuditCollectionHealth", health);
    const gap = {
      windowStart: "2026-01-01T00:00:00.000Z",
      windowEnd: "2026-01-01T00:01:00.000Z",
      gapReason: "unclean_restart",
      coverage: "unknown",
    };
    expectValid("SecurityAuditCollectionHealth", { ...health, lastGap: gap });
    const validate = ajv.compile(schemaFor("SecurityAuditCollectionHealth"));
    expect(validate({ ...health, failure: "原始错误不可公开" })).toBe(false);
    expect(validate({ ...health, observedCount: -1 })).toBe(false);
    for (const field of ["clientIp", "actorUserId", "canonicalPath", "workerId"]) {
      expect(validate({ ...health, [field]: "不是健康字段" }), field).toBe(false);
    }
    expect(validate({ ...health, lastGap: { ...gap, source: "不是缺口字段" } })).toBe(false);
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
