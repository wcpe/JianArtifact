// 安全治理 devmock：案件列表/详情/四栏/转移/既読 + 规则命中种子数据。
import { http, HttpResponse } from "msw";
import { MOCK_TOKEN, MOCK_SECOND_ADMIN_TOKEN } from "./store";

const root = "*/api/v1/security";

function unauthorized(request: Request) {
  const header = request.headers.get("Authorization") ?? "";
  const token = /^bearer /i.test(header) ? header.slice(7).trim() : "";
  if (token !== MOCK_TOKEN && token !== MOCK_SECOND_ADMIN_TOKEN) {
    return HttpResponse.json(
      { error: { code: "forbidden", message: "forbidden" } },
      { status: 403 },
    );
  }
  return null;
}

interface MockCase {
  caseId: string;
  title: string;
  state: "open" | "claimed" | "closed";
  severity: string;
  resolution?: string;
  targetVersion: number;
  createdUnix: number;
  updatedUnix: number;
  ownerUserId?: number;
  reason?: string;
}

const cases: MockCase[] = [
  {
    caseId: "case-01",
    title: "同主体拒绝激增",
    state: "open",
    severity: "high",
    targetVersion: 3,
    createdUnix: 1_762_000_000,
    updatedUnix: 1_762_000_100,
    reason: "rule-engine 自动建案",
  },
  {
    caseId: "case-02",
    title: "拒绝后新来源已提交",
    state: "claimed",
    severity: "medium",
    targetVersion: 5,
    createdUnix: 1_761_900_000,
    updatedUnix: 1_761_900_400,
    ownerUserId: 1,
  },
];

const findings = [
  {
    findingId: "fnd-case-01",
    ruleId: "subject_denial_burst",
    ruleVersion: 1,
    severity: "high",
    confidence: "high",
    subjectKey: "user:2",
    windowFrom: 1_761_999_400,
    windowTo: 1_762_000_000,
    summary: "主体 user:2 于 5 分钟内被拒绝 12 次",
    caseId: "case-01",
    createdUnix: 1_762_000_000,
  },
  {
    findingId: "fnd-case-02",
    ruleId: "denied_then_committed",
    ruleVersion: 1,
    severity: "medium",
    confidence: "medium",
    subjectKey: "ip:203.0.113.7",
    windowFrom: 1_761_899_400,
    windowTo: 1_761_900_000,
    summary: "拒绝后 10 分钟内出现其他来源的成功提交",
    caseId: "case-02",
    createdUnix: 1_761_900_000,
  },
];

const caseLinks = [
  {
    caseId: "case-01",
    eventId: "audit-security-login-1",
    linkKind: "fact",
    linkedBy: "rule-engine",
    linkedUnix: 1_762_000_000,
  },
  {
    caseId: "case-01",
    eventId: "audit-security-login-2",
    linkKind: "candidate",
    linkedBy: "admin-1",
    linkedUnix: 1_762_000_050,
  },
  {
    caseId: "case-02",
    eventId: "audit-security-login-1",
    linkKind: "fact",
    linkedBy: "rule-engine",
    linkedUnix: 1_761_900_000,
  },
];

const timelineItems = [
  {
    sequence: 11,
    eventId: "audit-security-login-1",
    eventType: "protocol.write_denied",
    occurredAt: 1_761_999_600,
    outcome: "denied",
    errorCode: "forbidden",
    actorUserId: 2,
    clientIp: "203.0.113.7",
    linkKind: "fact",
  },
  {
    sequence: 12,
    eventId: "audit-security-login-2",
    eventType: "asset.change_committed",
    occurredAt: 1_761_999_800,
    outcome: "succeeded",
    errorCode: "",
    actorUserId: 2,
    clientIp: "203.0.113.9",
    linkKind: "candidate",
  },
];
const uploadItems = [
  {
    eventId: "audit-security-login-2",
    eventType: "asset.change_committed",
    canonicalPath: "libs/com/example/demo-1.0.jar",
    objectKind: "asset",
    objectId: "",
    digest: "sha256:ab12",
    sizeBytes: 2048,
    manifestDigest: "",
    targetVersion: "1.0",
    repository: "raw-demo",
    linkKind: "fact",
  },
];
const identityItems = [
  {
    actorUserId: 2,
    actorName: "dev-user",
    credentialId: "550e8400-e29b-41d4-a716-446655440000",
    sessionId: "550e8400-e29b-41d4-a716-446655440001",
    authMethod: "bearer_token",
    authStrength: "machine",
    clientIp: "203.0.113.7",
    userAgent: "curl/8.0",
    events: 12,
  },
];
const holdItems = [
  {
    holdId: "hold-01",
    digest: "sha256:ab12",
    byteState: "metadata_only",
    state: "active",
    createdAt: 1_762_000_050,
  },
];

const bindings = new Map<number, string>();

export const securityCaseHandlers = [
  http.get(`${root}/cases`, ({ request }) => {
    const denied = unauthorized(request);
    if (denied) return denied;
    const url = new URL(request.url);
    const state = url.searchParams.get("state");
    return HttpResponse.json(state ? cases.filter((c) => c.state === state) : cases);
  }),
  http.get(`${root}/findings`, ({ request }) => {
    const denied = unauthorized(request);
    if (denied) return denied;
    return HttpResponse.json(findings);
  }),
  http.get(`${root}/cases/:caseId/links`, ({ request, params }) => {
    const denied = unauthorized(request);
    if (denied) return denied;
    const c = cases.find((x) => x.caseId === params.caseId);
    if (!c)
      return HttpResponse.json(
        { error: { code: "security_case_not_found", message: "not found" } },
        { status: 404 },
      );
    return HttpResponse.json(caseLinks.filter((l) => l.caseId === c.caseId));
  }),
  http.get(`${root}/cases/:caseId`, ({ request, params }) => {
    const denied = unauthorized(request);
    if (denied) return denied;
    const c = cases.find((x) => x.caseId === params.caseId);
    if (!c)
      return HttpResponse.json(
        { error: { code: "security_case_not_found", message: "not found" } },
        { status: 404 },
      );
    return HttpResponse.json({
      case: c,
      findings: findings.filter((f) => f.caseId === c.caseId),
      links: caseLinks.filter((l) => l.caseId === c.caseId),
      readSeenVersion: 2,
      readUnix: 1_762_000_080,
    });
  }),
  http.post(`${root}/cases/:caseId/transitions`, async ({ request, params }) => {
    const denied = unauthorized(request);
    if (denied) return denied;
    const c = cases.find((x) => x.caseId === params.caseId);
    if (!c)
      return HttpResponse.json(
        { error: { code: "security_case_not_found", message: "not found" } },
        { status: 404 },
      );
    const body = (await request.json()) as { action: string; expectVersion?: number };
    if (body.expectVersion !== undefined && body.expectVersion !== c.targetVersion) {
      return HttpResponse.json(
        { error: { code: "security_confirmation_stale", message: "stale" } },
        { status: 409 },
      );
    }
    if (body.action === "claim" && c.state === "open") {
      c.state = "claimed";
      c.ownerUserId = 1;
      c.targetVersion += 1;
    } else if (body.action === "close" && c.state === "claimed") {
      c.state = "closed";
      c.resolution = body.action ? "done" : "done";
      c.targetVersion += 1;
    } else {
      return HttpResponse.json(
        { error: { code: "security_transition_invalid", message: "invalid" } },
        { status: 409 },
      );
    }
    c.updatedUnix = 1_762_000_200;
    return HttpResponse.json(c);
  }),
  http.put(`${root}/cases/:caseId/read`, async ({ request, params }) => {
    const denied = unauthorized(request);
    if (denied) return denied;
    const c = cases.find((x) => x.caseId === params.caseId);
    if (!c)
      return HttpResponse.json(
        { error: { code: "security_case_not_found", message: "not found" } },
        { status: 404 },
      );
    return HttpResponse.json({ seenVersion: c.targetVersion });
  }),
  http.get(`${root}/cases/:caseId/timeline`, ({ request }) => {
    const denied = unauthorized(request);
    if (denied) return denied;
    return HttpResponse.json({ items: timelineItems });
  }),
  http.get(`${root}/cases/:caseId/uploads`, ({ request }) => {
    const denied = unauthorized(request);
    if (denied) return denied;
    return HttpResponse.json({ items: uploadItems });
  }),
  http.get(`${root}/cases/:caseId/identity`, ({ request }) => {
    const denied = unauthorized(request);
    if (denied) return denied;
    return HttpResponse.json({ items: identityItems });
  }),
  http.get(`${root}/cases/:caseId/actions-evidence`, ({ request }) => {
    const denied = unauthorized(request);
    if (denied) return denied;
    return HttpResponse.json({
      findings: findings.filter((f) => f.caseId === "case-01"),
      holds: holdItems,
    });
  }),
  http.put(`${root}/qq-bindings/:userId`, async ({ request, params }) => {
    const denied = unauthorized(request);
    if (denied) return denied;
    const body = (await request.json()) as { openid?: string };
    if (!body.openid || body.openid.length < 3) {
      return HttpResponse.json(
        { error: { code: "bad_request", message: "openid" } },
        { status: 400 },
      );
    }
    bindings.set(Number(params.userId), body.openid);
    return new HttpResponse(null, { status: 204 });
  }),
  http.delete(`${root}/qq-bindings/:userId`, ({ request, params }) => {
    const denied = unauthorized(request);
    if (denied) return denied;
    if (!bindings.delete(Number(params.userId))) {
      return HttpResponse.json(
        { error: { code: "not_found", message: "not found" } },
        { status: 404 },
      );
    }
    return new HttpResponse(null, { status: 204 });
  }),
];
