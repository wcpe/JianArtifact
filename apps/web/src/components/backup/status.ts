// 备份包状态到 OpsKit 语义色调的映射，与集群 / 审计页共用同一套视觉语言。
import type { BackupPackageStatus } from "../../api/types";
import type { OpsTone } from "../ops/OpsKit";

const STATUS_TONE: Record<BackupPackageStatus, OpsTone> = {
  queued: "gray",
  snapshotting: "blue",
  packing: "blue",
  done: "green",
  failed: "red",
};

export function backupStatusTone(status: string): OpsTone {
  return STATUS_TONE[status as BackupPackageStatus] ?? "gray";
}

export function backupModeTone(mode: string): OpsTone {
  return mode === "frozen" ? "orange" : "blue";
}

/** 生成中的状态：页面需要继续轮询进度。 */
export function backupInProgress(status: string): boolean {
  return status === "queued" || status === "snapshotting" || status === "packing";
}

// 从 URL 导入备份包状态映射（FR-137）：queued 灰、fetching/staging 蓝、
// pending_restart 橙（需重启服务）、done 绿、failed 红。
const IMPORT_STATUS_TONE: Record<string, OpsTone> = {
  queued: "gray",
  fetching: "blue",
  staging: "blue",
  pending_restart: "orange",
  done: "green",
  failed: "red",
};

export function importStatusTone(status: string): OpsTone {
  return IMPORT_STATUS_TONE[status] ?? "gray";
}

/** 非终态：拉取/暂存中，页面需继续轮询进度。 */
export function importInProgress(status: string): boolean {
  return status === "queued" || status === "fetching" || status === "staging";
}
