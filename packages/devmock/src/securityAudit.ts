import { http, HttpResponse } from "msw";
import type { components } from "./schema.gen";
import { MOCK_SECOND_ADMIN_TOKEN, MOCK_TOKEN, store } from "./store";

type ErrorBody = components["schemas"]["Error"];
const root = "*/api/v1/observability/audit";
const listKeys = new Set([
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
]);
const refKeys = ["requestId", "originRequestId", "operationId", "taskId", "actionId", "objectId"];
const eventTypes = new Set([
  "auth.attempt",
  "auth.session_created",
  "protocol.write_denied",
  "asset.change_committed",
  "asset.response_observed",
]);
const outcomes = new Set(["succeeded", "denied", "failed", "pending", "unknown"]);

function error(code: string, status: number) {
  const body: ErrorBody = { error: { code, message: code } };
  return HttpResponse.json(body, { status });
}
function authorize(request: Request) {
  const header = request.headers.get("Authorization") ?? "";
  const token = /^bearer /i.test(header) ? header.slice(7).trim() : "";
  const id =
    token === MOCK_TOKEN
      ? 1
      : token === MOCK_SECOND_ADMIN_TOKEN
        ? 3
        : token === "mock.jwt.token:user"
          ? 2
          : undefined;
  if (id !== undefined) {
    const user = store.findUser(id);
    if (!user || user.status !== "active") return error("unauthenticated", 401);
    return user.role === "admin" && !user.webLoginDisabled ? null : error("forbidden", 403);
  }
  let owner = store.machineTokenOwner(token);
  if (!owner && /^basic /i.test(header)) {
    try {
      const credentials = atob(header.slice(6).trim());
      const separator = credentials.indexOf(":");
      if (separator >= 0) {
        const user = credentials.slice(0, separator);
        const pass = credentials.slice(separator + 1);
        if (pass.startsWith("jat_")) owner = store.machineTokenOwner(pass);
        else if (!pass && user.startsWith("jat_")) owner = store.machineTokenOwner(user);
      }
    } catch {
      // 无效编码不是已认证主体。
    }
  }
  return owner?.status === "active" ? error("forbidden", 403) : error("unauthenticated", 401);
}
function paramsValid(url: string, allowed: Set<string>) {
  const marker = url.indexOf("?");
  const raw = marker < 0 ? "" : url.slice(marker + 1);
  if (new TextEncoder().encode(raw).length > 16384 || (marker >= 0 && !raw)) return false;
  if (raw && (raw.startsWith("&") || raw.endsWith("&") || raw.includes("&&") || raw.includes(";")))
    return false;
  try {
    for (const pair of raw ? raw.split("&") : []) {
      const [key = "", ...rest] = pair.split("=");
      const value = decodeURIComponent(rest.join("=").replace(/\+/g, " "));
      const decodedKey = decodeURIComponent(key.replace(/\+/g, " "));
      if (!allowed.has(decodedKey) || !value.trim() || /\p{Cc}/u.test(value)) return false;
    }
  } catch {
    return false;
  }
  const params = new URLSearchParams(raw);
  for (const [key] of params) {
    if (params.getAll(key).length !== 1) return false;
  }
  const limit = params.get("limit");
  if (limit !== null && (!/^\d+$/.test(limit) || Number(limit) < 1 || Number(limit) > 100))
    return false;
  // 空模拟库从不签发游标，因此任何传入的游标都不能被当成真实过期游标。
  return !params.has("cursor");
}
function validIP(ip: string): boolean {
  if (ip.includes(":")) {
    if (!/^[\da-fA-F:.]+$/.test(ip) || ip.includes("%")) return false;
    try {
      new URL(`http://[${ip}]/`);
      return true;
    } catch {
      return false;
    }
  }
  const parts = ip.split(".");
  return parts.length === 4 && parts.every((p) => /^(0|[1-9]\d{0,2})$/.test(p) && Number(p) <= 255);
}
function isIPv4Mapped(ip: string): boolean {
  const canonical = new URL(`http://[${ip}]/`).hostname.slice(1, -1);
  const [left = "", right = ""] = canonical.split("::");
  const before = left ? left.split(":") : [];
  const after = right ? right.split(":") : [];
  const groups = canonical.includes("::")
    ? [...before, ...Array<string>(8 - before.length - after.length).fill("0"), ...after]
    : canonical.split(":");
  return (
    groups.length === 8 &&
    groups.slice(0, 5).every((part) => Number.parseInt(part, 16) === 0) &&
    Number.parseInt(groups[5] ?? "", 16) === 0xffff
  );
}
function validDate(value: string): number {
  const match =
    /^(\d{4})-(\d\d)-(\d\d)T(\d\d):(\d\d):(\d\d)(?:\.(\d+))?(Z|([+-])(\d\d):(\d\d))$/.exec(value);
  if (!match) return NaN;
  const [
    ,
    year,
    month,
    day,
    hour,
    minute,
    second,
    fraction = "",
    zone,
    sign,
    zoneHour,
    zoneMinute,
  ] = match;
  if (
    fraction.slice(3).replace(/0/g, "") ||
    Number(year) < 1 ||
    Number(month) < 1 ||
    Number(month) > 12 ||
    Number(day) < 1 ||
    Number(hour) > 23 ||
    Number(minute) > 59 ||
    Number(second) > 59 ||
    (zone !== "Z" && (Number(zoneHour) > 23 || Number(zoneMinute) > 59))
  )
    return NaN;
  const date = new Date(0);
  date.setUTCFullYear(Number(year), Number(month) - 1, Number(day));
  date.setUTCHours(
    Number(hour),
    Number(minute),
    Number(second),
    Number(fraction.slice(0, 3).padEnd(3, "0")),
  );
  if (
    date.getUTCFullYear() !== Number(year) ||
    date.getUTCMonth() !== Number(month) - 1 ||
    date.getUTCDate() !== Number(day)
  )
    return NaN;
  const offset =
    zone === "Z" ? 0 : (Number(zoneHour) * 60 + Number(zoneMinute)) * (sign === "+" ? 1 : -1);
  const ms = date.getTime() - offset * 60000;
  const utcYear = new Date(ms).getUTCFullYear();
  return utcYear >= 1 && utcYear <= 9999 ? ms : NaN;
}
function validList(url: string): boolean {
  if (!paramsValid(url, listKeys)) return false;
  const params = new URL(url).searchParams;
  const from = validDate(params.get("from") ?? ""),
    to = validDate(params.get("to") ?? "");
  if (!Number.isFinite(from) || !Number.isFinite(to) || to <= from || to - from > 30 * 86400000)
    return false;
  const ip = params.get("ip"),
    cidr = params.get("cidr");
  if (ip && cidr) return false;
  if (ip && !validIP(ip)) return false;
  if (cidr) {
    const [address, bits, extra] = cidr.split("/");
    if (
      !address ||
      !bits ||
      extra !== undefined ||
      !validIP(address) ||
      !/^(0|[1-9]\d*)$/.test(bits) ||
      Number(bits) > (address.includes(":") ? 128 : 32) ||
      (address.includes(":") && isIPv4Mapped(address) && Number(bits) < 96)
    )
      return false;
  }
  const type = params.get("eventType"),
    outcome = params.get("outcome");
  if ((type && !eventTypes.has(type)) || (outcome && !outcomes.has(outcome))) return false;
  for (const key of ["actorUserId", "repositoryId"]) {
    const value = params.get(key);
    if (
      value &&
      (!/^\d+$/.test(value) || BigInt(value) < 1n || BigInt(value) > 9223372036854775807n)
    )
      return false;
  }
  for (const key of refKeys) {
    const value = params.get(key);
    if (value && (!/^[a-zA-Z0-9_-]{1,128}$/.test(value) || /^jat_/i.test(value))) return false;
  }
  for (const key of ["credentialId", "sessionId"]) {
    const value = params.get(key);
    if (
      value &&
      !/^(?:[A-Z2-7]{26}|[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12})$/.test(
        value,
      )
    )
      return false;
  }
  const digest = params.get("digest");
  if (digest && !/^(?:sha256:[0-9a-f]{64}|sha512:[0-9a-f]{128})$/.test(digest)) return false;
  const path = params.get("canonicalPath");
  if (
    path &&
    (new TextEncoder().encode(path).length > 1024 ||
      /[?#\\\p{Cc}\p{Cf}\p{Zl}\p{Zp}]/u.test(path) ||
      path.split("/").some((p) => !p || p === "." || p === ".."))
  )
    return false;
  return true;
}
const invalid = () => error("invalid_security_filter", 400);
const notFound = () => error("security_event_not_found", 404);
const eventIdValid = (id: string) => /^[a-zA-Z0-9_-]{1,128}$/.test(id) && !/^jat_/i.test(id);
export const securityAuditHandlers = [
  http.get(`${root}/security-events`, ({ request }) => {
    const denied = authorize(request);
    if (denied) return denied;
    if (!validList(request.url)) return invalid();
    const body: components["schemas"]["SecurityAuditEventPage"] = {
      items: [],
      hasMore: false,
      snapshotWatermark: 0,
      projectionWatermark: 0,
      projectionHealth: "healthy",
      coverage: "unknown",
    };
    return HttpResponse.json(body);
  }),
  http.get(`${root}/security-events/:eventId/objects`, ({ request, params }) => {
    const denied = authorize(request);
    if (denied) return denied;
    if (
      !eventIdValid(String(params.eventId)) ||
      !paramsValid(request.url, new Set(["limit", "cursor"]))
    )
      return invalid();
    return notFound();
  }),
  http.get(`${root}/security-events/:eventId`, ({ request, params }) => {
    const denied = authorize(request);
    if (denied) return denied;
    if (!eventIdValid(String(params.eventId)) || !paramsValid(request.url, new Set()))
      return invalid();
    return notFound();
  }),
  http.get(`${root}/collection-health`, ({ request }) => {
    const denied = authorize(request);
    if (denied) return denied;
    if (!paramsValid(request.url, new Set())) return invalid();
    const body: components["schemas"]["SecurityAuditCollectionHealth"] = {
      state: "starting",
      coverage: "unknown",
    };
    return HttpResponse.json(body);
  }),
];
