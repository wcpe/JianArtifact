// 安全治理 devmock（二）：强认证挑战流、安全通知/投递、处置动作、恢复闸与保全清单。
// 与真实后端契约对齐：挑战不直签会话，确认后下发一次性恢复码。
import { http, HttpResponse } from "msw";
import { MOCK_TOKEN, MOCK_SECOND_ADMIN_TOKEN } from "./store";

const root = "*/api/v1/security";
const authRoot = "*/api/v1/auth/mfa";

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

const MOCK_USER = {
  id: 1,
  username: "admin",
  role: "admin" as const,
  status: "active" as const,
  webLoginDisabled: false,
  createdAt: "2026-01-01T00:00:00Z",
};

/** 登记挑战（claim_required/恢复重绑/主动登记共用结构）。 */
export function mfaEnrollChallenge(next?: "claim_required" | "mfa_required", user?: unknown) {
  const challenge = {
    challengeId: "ch-mock-01",
    challengeToken: "mock-challenge-token",
    expiresAt: new Date(Date.now() + 15 * 60_000).toISOString(),
    enrollmentUri:
      "otpauth://totp/JianArtifact:admin?secret=JBSWY3DPEHPK3PXP&issuer=JianArtifact&digits=6&period=30",
  };
  if (!next) return challenge;
  return {
    next,
    token: "",
    challengeId: challenge.challengeId,
    challengeToken: challenge.challengeToken,
    challengeExpiresAt: challenge.expiresAt,
    enrollmentUri: challenge.enrollmentUri,
    user: user ?? MOCK_USER,
  };
}

const recoveryCodes = () => ({
  recoveryCodes: ["rc-1a2b3c4d", "rc-5e6f7a8b", "rc-9c0d1e2f", "rc-3b4a5c6d"],
});

// —— 强认证挑战流（auth 根，无鉴权要求：挑战明文即凭据）——
const mfaHandlers = [
  http.post(`${authRoot}/verify`, async ({ request }) => {
    const body = (await request.json().catch(() => ({}))) as {
      challengeToken?: string;
      otp?: string;
    };
    if (!body.challengeToken || !body.otp) {
      return HttpResponse.json(
        { error: { code: "bad_request", message: "挑战与口令必填" } },
        { status: 400 },
      );
    }
    return HttpResponse.json({ next: "authenticated", token: MOCK_TOKEN, user: MOCK_USER });
  }),

  http.post(`${authRoot}/recover`, async ({ request }) => {
    const body = (await request.json().catch(() => ({}))) as {
      challengeToken?: string;
      recoveryCode?: string;
    };
    if (!body.challengeToken || !body.recoveryCode) {
      return HttpResponse.json(
        { error: { code: "bad_request", message: "挑战与恢复码必填" } },
        { status: 400 },
      );
    }
    return HttpResponse.json(mfaEnrollChallenge());
  }),

  http.post(`${authRoot}/claim`, async ({ request }) => {
    const body = (await request.json().catch(() => ({}))) as {
      username?: string;
      password?: string;
      claimTicket?: string;
    };
    if (!body.username || !body.password || !body.claimTicket) {
      return HttpResponse.json(
        { error: { code: "bad_request", message: "用户名、口令与认领票必填" } },
        { status: 400 },
      );
    }
    if (body.claimTicket === "invalid") {
      return HttpResponse.json(
        { error: { code: "claim_rejected", message: "认领票无效或已用尽" } },
        { status: 401 },
      );
    }
    return HttpResponse.json(mfaEnrollChallenge());
  }),

  http.post(`${authRoot}/enroll`, ({ request }) => {
    const errResp = unauthorized(request);
    if (errResp) return errResp;
    return HttpResponse.json(mfaEnrollChallenge());
  }),

  http.post(`${authRoot}/enroll/confirm`, async ({ request }) => {
    const body = (await request.json().catch(() => ({}))) as {
      challengeToken?: string;
      otp?: string;
    };
    if (!body.challengeToken || !body.otp) {
      return HttpResponse.json(
        { error: { code: "bad_request", message: "挑战与口令必填" } },
        { status: 400 },
      );
    }
    return HttpResponse.json(recoveryCodes());
  }),

  http.get(`${authRoot}/status`, ({ request }) => {
    const errResp = unauthorized(request);
    if (errResp) return errResp;
    return HttpResponse.json({
      enrolled: true,
      enrollmentPending: false,
      recoveryCodesRemaining: 4,
      mfaRequired: true,
    });
  }),

  http.post(`${authRoot}/step-up`, ({ request }) => {
    const errResp = unauthorized(request);
    if (errResp) return errResp;
    return HttpResponse.json({
      challengeId: "ch-stepup-01",
      challengeToken: "mock-stepup-token",
      expiresAt: new Date(Date.now() + 10 * 60_000).toISOString(),
    });
  }),

  http.post(`${authRoot}/step-up/verify`, ({ request }) => {
    const errResp = unauthorized(request);
    if (errResp) return errResp;
    return HttpResponse.json({ mfaVerifiedAt: new Date().toISOString() });
  }),
];

// —— 安全通知与投递 ——
const notifications = [
  {
    seq: 3,
    noticeId: "ntc-03",
    kind: "finding",
    title: "subject_denial_burst 命中 user:2",
    payload: "主体 user:2 于 5 分钟内被拒绝 12 次",
    createdUnix: 1_762_000_100,
    acked: false,
  },
  {
    seq: 2,
    noticeId: "ntc-02",
    kind: "case",
    title: "案件 case-02 已认领",
    payload: "拒绝后新来源已提交",
    createdUnix: 1_761_900_500,
    acked: true,
  },
];

const deliveries = [
  {
    deliveryId: "dlv-01",
    channel: "smtp" as const,
    targetMasked: "a***@example.com",
    state: "accepted" as const,
    attempts: 1,
  },
  {
    deliveryId: "dlv-02",
    channel: "qq" as const,
    targetMasked: "qq:u***1",
    state: "failed" as const,
    attempts: 3,
    lastError: "endpoint_unconfigured",
  },
];

// —— 处置动作 / 恢复闸 / 保全清单 ——
let gateState = "normal";

const opsHandlers = [
  http.get(`${root}/notifications`, ({ request }) => {
    const errResp = unauthorized(request);
    if (errResp) return errResp;
    const url = new URL(request.url);
    const after = Number(url.searchParams.get("after") ?? 0);
    const items = notifications.filter((n) => n.seq > after);
    return HttpResponse.json({ items, nextCursor: 3, hasMore: false, watermark: 3 });
  }),

  http.put(`${root}/notifications/:noticeId/ack`, ({ request, params }) => {
    const errResp = unauthorized(request);
    if (errResp) return errResp;
    const n = notifications.find((x) => x.noticeId === params.noticeId);
    if (!n)
      return HttpResponse.json(
        { error: { code: "not_found", message: "通知不存在" } },
        { status: 404 },
      );
    n.acked = true;
    return new HttpResponse(null, { status: 204 });
  }),

  http.get(`${root}/notification-deliveries`, ({ request }) => {
    const errResp = unauthorized(request);
    if (errResp) return errResp;
    return HttpResponse.json({ items: deliveries });
  }),

  http.post(`${root}/notification-deliveries/:deliveryId/retry`, ({ request, params }) => {
    const errResp = unauthorized(request);
    if (errResp) return errResp;
    const d = deliveries.find((x) => x.deliveryId === params.deliveryId);
    if (!d)
      return HttpResponse.json(
        { error: { code: "not_found", message: "投递不存在" } },
        { status: 404 },
      );
    if (d.state !== "failed") {
      return HttpResponse.json(
        { error: { code: "conflict", message: "仅失败投递可重试" } },
        { status: 409 },
      );
    }
    return HttpResponse.json({ ...d, state: "pending" });
  }),

  http.post(`${root}/actions`, async ({ request }) => {
    const errResp = unauthorized(request);
    if (errResp) return errResp;
    const body = (await request.json().catch(() => ({}))) as { targets?: { target?: string }[] };
    return HttpResponse.json({
      actionId: "act-mock-01",
      actionType: "restrict",
      actionDigest: "sha256:mockdigest01",
      expiresAt: new Date(Date.now() + 10 * 60_000).toISOString(),
      targets: (body.targets ?? []).map((t) => t.target ?? ""),
    });
  }),

  http.post(`${root}/actions/:actionId/execute`, async ({ request, params }) => {
    const errResp = unauthorized(request);
    if (errResp) return errResp;
    const body = (await request.json().catch(() => ({}))) as { actionDigest?: string };
    if (body.actionDigest !== "sha256:mockdigest01") {
      return HttpResponse.json(
        { error: { code: "conflict", message: "动作摘要漂移" } },
        { status: 409 },
      );
    }
    return HttpResponse.json({
      actionId: params.actionId,
      state: "succeeded",
      items: [{ seq: 1, target: "instance", outcome: "applied", detail: "实例写锁已生效" }],
    });
  }),

  http.get(`${root}/recovery`, ({ request }) => {
    const errResp = unauthorized(request);
    if (errResp) return errResp;
    return HttpResponse.json({ state: gateState, securityEpoch: 1, updatedUnix: 1_762_000_000 });
  }),

  http.post(`${root}/recovery/begin`, async ({ request }) => {
    const errResp = unauthorized(request);
    if (errResp) return errResp;
    gateState = "isolated";
    return new HttpResponse(null, { status: 204 });
  }),

  http.post(`${root}/recovery/report`, async ({ request }) => {
    const errResp = unauthorized(request);
    if (errResp) return errResp;
    gateState = gateState === "isolated" ? "ready_for_review" : gateState;
    return new HttpResponse(null, { status: 204 });
  }),

  http.post(`${root}/recovery/confirm`, async ({ request }) => {
    const errResp = unauthorized(request);
    if (errResp) return errResp;
    gateState = "normal";
    return new HttpResponse(null, { status: 204 });
  }),

  http.get(`${root}/holds`, ({ request }) => {
    const errResp = unauthorized(request);
    if (errResp) return errResp;
    return HttpResponse.json({
      items: [
        {
          holdId: "hold-01",
          digest: "sha256:ab12",
          byteState: "metadata_only",
          state: "active",
          createdUnix: 1_762_000_000,
        },
      ],
    });
  }),
];

export const securityOpsHandlers = [...mfaHandlers, ...opsHandlers];
