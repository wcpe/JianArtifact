// 审计「操作」聚合（纯前端纯函数）：把扁平事件流归并成「一次完整操作」。
//
// 为什么需要它：一次 `mvn deploy` 会为同一个版本写下几十个文件事件（jar / pom / sources /
// javadoc / 各 .sha1 / .md5 / maven-metadata.xml），逐条平铺既淹没信息、也看不出「这是一次操作」。
// 同理一次批量删除会写多条 `asset.delete`。本模块把这类事件归并成一个可折叠的操作。
//
// 归并优先级（**先权威后兜底**）：
// 1. `event.operationId` 非空 → 以它为键权威归并。资产操作（批量删除/移动等）由后端同事务
//    写入同一 operationId，天然对齐；
// 2. 无 operationId（协议上传 / Maven 网页表单上传都不带）→ 以
//    `类型 + 仓库 + 坐标(或目录) + 操作者 + 时间窗` 为键归并；
// 3. 其余单事件（仓库/用户/设置等管理动作）→ 各自成一个操作，不强行编造关联。
//
// 时间窗的作用：一次 deploy 的各文件几乎同时写入，但可能跨分钟边界；窗口只用于容忍这种
// 边界抖动，不用于「把一段时间内的巧合写入并成一个操作」。
//
// 关键约束：**永不丢弃事件**。归并只改变分组，不改变集合；坐标反解不出的（npm / raw / 未知格式）
// 一律降级到「其他」分枝，按目录聚合。

import type { AuditEvent } from "../../api/types";
import { actorEmail, isUploadAction, parseTime } from "./labels";
import { parseUploadCoordinates, uploadArtifactTarget } from "./uploadTree";

/** 操作类型：决定聚合界面的分区与事件流的图标/色彩。 */
export type OperationKind = "upload" | "delete" | "move" | "other";

/** 同一次多文件写入的时间窗（毫秒）：只容忍跨分钟边界，不做大范围猜测。 */
export const OPERATION_WINDOW_MS = 15 * 60 * 1000;

/** 制品坐标：能反解出 Maven GAV 时给出；否则由 directory 承担聚合键。 */
export interface OperationCoordinates {
  groupId?: string;
  artifactId?: string;
  version?: string;
  /** 目录路径（去掉文件名后的剩余部分）。 */
  directory?: string;
}

export interface AuditOperation {
  /** 归并键：有 operationId 时即它本人，否则为派生键。用于 React key 与展开态。 */
  key: string;
  kind: OperationKind;
  /** 主导动作（取首个事件，事件已按时间倒序）。 */
  action: string;
  /** 操作涉及的仓库名（多仓库时取首个；跨仓库不会被并成一个操作）。 */
  repo: string | null;
  /** Maven 坐标（若能反解），供界面展示 groupId:artifactId:version。 */
  coordinates: OperationCoordinates | null;
  /** 该操作包含的事件，按时间倒序。 */
  events: AuditEvent[];
  /** 最新 / 最早事件时间（毫秒；无样本为 0）。 */
  latestAt: number;
  earliestAt: number;
  /** 操作结果（取首个事件的 result；同一操作的各事件 result 一致）。 */
  result: string;
  /** 发起者展示名（取首个事件）。 */
  actor: string;
}

/** 动作 → 操作类型。上传白名单复用 labels（与后端 action 命名一致）。 */
export function operationKindOf(action: unknown): OperationKind {
  const value = typeof action === "string" ? action.trim() : "";
  if (!value) return "other";
  if (isUploadAction(value)) return "upload";
  if (value === "asset.delete") return "delete";
  if (value === "asset.move" || value === "asset.rename") return "move";
  return "other";
}

/** 只有「多文件写入」类操作才值得按坐标 + 时间窗兜底归并。 */
function isBatchableKind(kind: OperationKind): boolean {
  return kind === "upload" || kind === "delete" || kind === "move";
}

/** 事件是否属于「制品操作」（能取到 repo/path）——决定能否参与坐标聚合。 */
function isArtifactEvent(event: AuditEvent): boolean {
  return uploadArtifactTarget(event) !== null;
}

function operationIdOf(event: AuditEvent): string {
  const value = (event as { operationId?: unknown }).operationId;
  return typeof value === "string" ? value.trim() : "";
}

function resultOf(event: AuditEvent): string {
  const value = (event as { result?: unknown }).result;
  return typeof value === "string" ? value : "";
}

/**
 * 为无 operationId 的制品事件派生归并键。
 *
 * 键的构成刻意包含 `actor` 与 `kind`：同一个人在同一时间窗内对同一坐标做的上传与删除
 * 是两回事，不能并成一个操作；不同人对同一坐标的写入也应分开记账。
 * 仓库也在键内，避免跨仓库并账。坐标缺失时退回目录路径（「其他」分枝）。
 */
function derivedKey(event: AuditEvent, kind: OperationKind): string | null {
  const target = uploadArtifactTarget(event);
  if (!target) return null;
  const action = typeof event.action === "string" ? event.action : "";
  const coords = parseUploadCoordinates(target.path);
  const scope =
    coords.groupId && coords.artifactId && coords.version
      ? `${coords.groupId}:${coords.artifactId}:${coords.version}`
      : `dir:${coords.directory}`;
  const actor = actorEmail(event.actor);
  const bucket = Math.floor(parseTime(event.occurredAt) / OPERATION_WINDOW_MS);
  return `${kind}|${action}|${target.repo}|${scope}|${actor}|${bucket}`;
}

/**
 * 把事件流归并成操作列表（按最新时间倒序）。
 *
 * 单事件操作也会被包装成 `AuditOperation`（`events.length === 1`），让调用方用一套模型渲染——
 * 事件流据此对「单事件」保持与改动前一致的展示，对「多事件」折叠成一行。
 */
export function buildOperations(events: readonly AuditEvent[]): AuditOperation[] {
  const byKey = new Map<string, AuditEvent[]>();
  // 保持稳定的插入顺序，便于同时间操作的展示顺序可预期。
  const order: string[] = [];

  for (const event of events) {
    const kind = operationKindOf(event.action);
    const opId = operationIdOf(event);
    // 只有制品类的批量操作才走派生键；管理类（other）单事件各自成 op，
    // 否则同一时间窗里两个不相关的管理动作会被误并成「一次操作」。
    const derived =
      opId || !isBatchableKind(kind) || !isArtifactEvent(event)
        ? ""
        : (derivedKey(event, kind) ?? "");
    const key = opId ? `op:${opId}` : derived ? `derived:${derived}` : `single:${event.eventId}`;
    const bucket = byKey.get(key);
    if (bucket) {
      bucket.push(event);
    } else {
      byKey.set(key, [event]);
      order.push(key);
    }
  }

  const operations: AuditOperation[] = [];
  for (const key of order) {
    const group = byKey.get(key)!;
    // 每个操作内部按时间倒序（最新在前）。
    const sorted = [...group].sort((a, b) => parseTime(b.occurredAt) - parseTime(a.occurredAt));
    const head = sorted[0]!;
    const target = uploadArtifactTarget(head);
    const coords = target ? parseUploadCoordinates(target.path) : null;
    let latestAt = 0;
    let earliestAt = 0;
    for (const event of sorted) {
      const at = parseTime(event.occurredAt);
      if (at > latestAt) latestAt = at;
      if (earliestAt === 0 || (at > 0 && at < earliestAt)) earliestAt = at;
    }
    operations.push({
      key,
      kind: operationKindOf(head.action),
      action: typeof head.action === "string" ? head.action : "",
      repo: target?.repo ?? null,
      coordinates: coords
        ? {
            groupId: coords.groupId,
            artifactId: coords.artifactId,
            version: coords.version,
            directory: coords.directory,
          }
        : null,
      events: sorted,
      latestAt,
      earliestAt,
      result: resultOf(head),
      actor: actorEmail(head.actor),
    });
  }

  operations.sort((a, b) => b.latestAt - a.latestAt);
  return operations;
}

/** 操作内的文件数（= 事件数）；供「N 次操作 / M 个文件」这类汇总复用。 */
export function operationFileCount(operation: AuditOperation): number {
  return operation.events.length;
}
