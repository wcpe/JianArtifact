// 备份包状态到 OpsKit 语义色调的映射，与集群 / 审计页共用同一套视觉语言。
import { useMemo } from "react";
import { useTranslation } from "react-i18next";

import type { SettingsConfig } from "../../api/endpoints";
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

/**
 * 备份包来源色调：自动备份用青色与手动区分。
 *
 * 刻意不用绿 / 红：那两色在本页已被"已完成 / 失败"状态占用，
 * 来源与状态是两个正交维度，共色会让人误读成状态。
 */
export function backupOriginTone(origin: string): OpsTone {
  return origin === "auto" ? "cyan" : "gray";
}

/** 生成中的状态：页面需要继续轮询进度。 */
export function backupInProgress(status: string): boolean {
  return status === "queued" || status === "snapshotting" || status === "packing";
}

/** 演练级别色调：深校验读取全量数据，用时更长、强度更高。 */
export function drillLevelTone(level: string): OpsTone {
  return level === "deep" ? "orange" : "blue";
}

/** 演练结论色调：与报告里的 ok 字段一一对应。 */
export function drillVerdictTone(ok: boolean): OpsTone {
  return ok ? "green" : "red";
}

// 从 URL 导入备份包状态映射：queued 灰、fetching/staging 蓝、
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

/**
 * 自动备份配置摘要：状态条与配置弹窗共用同一段语义。
 *
 * 三个 `0` 都是有意义的取值，必须各自成句——**不得退化成 `||` 兜底**：
 * 间隔 0 若显示成「每 0 秒备份一次」，用户会以为它在按 0 秒跑（实际是关闭）；
 * 份数 / 天数 0 若显示成具体数字，则把"不限制"说成了"限制到某个数"。
 *
 * 返回 null 表示配置尚未读到（调用方自行决定占位文案）。
 */
export function useAutoBackupSummary(config: SettingsConfig | null): string | null {
  const { t } = useTranslation();
  return useMemo(() => {
    if (!config) return null;
    const { backupAutoInterval, backupKeepLast, backupRetentionDays } = config;
    const intervalPart =
      backupAutoInterval > 0
        ? t("backups.autoCfgSummaryInterval", { n: backupAutoInterval })
        : t("backups.autoCfgSummaryOff");
    const keepPart =
      backupKeepLast > 0
        ? t("backups.autoCfgSummaryKeep", { n: backupKeepLast })
        : t("backups.autoCfgSummaryKeepUnlimited");
    const daysPart =
      backupRetentionDays > 0
        ? t("backups.autoCfgSummaryDays", { n: backupRetentionDays })
        : t("backups.autoCfgSummaryDaysUnlimited");
    return `${intervalPart} · ${keepPart} · ${daysPart}`;
  }, [config, t]);
}
