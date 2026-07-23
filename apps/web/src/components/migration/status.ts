// 迁移状态 / 来源的展示色与辅助计算。
import type { MigrationSourceType, MigrationTaskStatus } from "../../api/types";

/** 任务状态对应 Mantine 颜色。 */
export function statusColor(status: MigrationTaskStatus): string {
  switch (status) {
    case "completed":
      return "green";
    case "running":
      return "blue";
    case "failed":
      return "red";
    case "cancelled":
      return "gray";
    case "planned":
    default:
      return "yellow";
  }
}

/** 来源类型对应颜色。 */
export function sourceColor(source: MigrationSourceType): string {
  switch (source) {
    case "online_rest":
      return "cyan";
    case "offline_dir":
      return "violet";
    case "offline_bundle":
      return "indigo";
    default:
      return "gray";
  }
}

/** 格式 badge 色。 */
export function formatColor(format: string): string {
  switch (format) {
    case "maven":
      return "orange";
    case "npm":
      return "red";
    case "raw":
      return "gray";
    default:
      return "blue";
  }
}

export interface Totals {
  copied: number;
  skipped: number;
  failed: number;
}

export function parseTotals(raw: Record<string, unknown> | undefined | null): Totals {
  const num = (k: string) => {
    const v = raw?.[k];
    return typeof v === "number" ? v : Number(v) || 0;
  };
  return {
    copied: num("copied"),
    skipped: num("skipped"),
    failed: num("failed"),
  };
}

/** 进度百分比：有估算则按 sum/est，否则 running 给不确定感。 */
export function progressPercent(
  totals: Totals,
  estimatedAssets: number,
  status: MigrationTaskStatus,
): number {
  const sum = totals.copied + totals.skipped + totals.failed;
  if (status === "completed") {
    return 100;
  }
  if (estimatedAssets > 0) {
    return Math.min(status === "running" ? 99 : 100, Math.round((sum / estimatedAssets) * 100));
  }
  if (status === "running") {
    return sum > 0 ? Math.min(90, 10 + Math.min(sum, 80)) : 15;
  }
  return sum > 0 ? 100 : 0;
}

export function planEstimatedAssets(
  repos: { estimatedAssets?: number | null }[] | undefined,
): number {
  if (!repos?.length) {
    return 0;
  }
  return repos.reduce((acc, r) => acc + (r.estimatedAssets ?? 0), 0);
}

/** 从 sourceConfig 提取可读字段（不展示整段 JSON）。 */
export function sourceConfigSummary(cfg: Record<string, unknown> | undefined | null): {
  url?: string;
  path?: string;
  includeRepositories?: string[];
} {
  if (!cfg) {
    return {};
  }
  const url = typeof cfg.url === "string" ? cfg.url : undefined;
  const path = typeof cfg.path === "string" ? cfg.path : undefined;
  let includeRepositories: string[] | undefined;
  const inc = cfg.includeRepositories;
  if (Array.isArray(inc)) {
    includeRepositories = inc.filter((x): x is string => typeof x === "string" && x !== "");
  }
  return { url, path, includeRepositories };
}
