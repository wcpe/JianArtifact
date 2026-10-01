// FR-41 存储治理：仓库配额的状态判定与「带单位输入」换算（仓库列表与仓库详情共用）。
//
// 为什么单独抽一层：配额有两处展示（列表的用量列、详情页的配置区与页头徽章）与一处编辑，
// 阈值、配色、单位换算若各写一套必然漂移——口径一旦不一致，界面上「接近上限」的仓库在另一处
// 会显示成「正常」，管理员就无法据此判断该不该扩容。
//
// 状态口径（与服务端 429 拒绝点一致：`used >= limit` 即拒绝写入）：
// - unlimited：未设上限（0 或字段缺省），永远不拒绝；
// - ok：已用 < 90% 上限；
// - near：已用 ≥ 90% 上限（仍可写，但很快就会撞限，界面需要提前预警）；
// - over：已用 ≥ 上限（服务端开始拒绝写入：HTTP 429 / quota_exceeded）。
export const QUOTA_NEAR_RATIO = 0.9;

export type QuotaState = "unlimited" | "ok" | "near" | "over";

/** 状态严重度：用于把「字节」与「制品数」两个维度归并成一个仓库级状态（取更严重者）。 */
const STATE_SEVERITY: Record<QuotaState, number> = {
  unlimited: 0,
  ok: 1,
  near: 2,
  over: 3,
};

/** 单个维度的配额状态；`limit` 为 0 / 缺省（不限）时恒为 unlimited。 */
export function quotaState(
  used: number | null | undefined,
  limit: number | null | undefined,
): QuotaState {
  if (!limit || limit <= 0) return "unlimited";
  // 用量缺失（老接口 / 尚未统计）按 0 处理：不能因为缺统计就误报超限。
  const actual = used ?? 0;
  if (actual >= limit) return "over";
  if (actual >= limit * QUOTA_NEAR_RATIO) return "near";
  return "ok";
}

/** 仓库级配额状态：字节与制品数两个维度里更严重的一个。 */
export function repoQuotaState(repo: {
  artifactCount?: number;
  totalSize?: number;
  quotaBytes?: number;
  quotaAssets?: number;
}): QuotaState {
  const bytes = quotaState(repo.totalSize, repo.quotaBytes);
  const assets = quotaState(repo.artifactCount, repo.quotaAssets);
  return STATE_SEVERITY[bytes] >= STATE_SEVERITY[assets] ? bytes : assets;
}

/** 状态 → i18n 键（`quota.*` 命名空间）。 */
export const QUOTA_STATE_LABEL_KEY: Record<QuotaState, string> = {
  unlimited: "quota.unlimited",
  ok: "quota.stateOk",
  near: "quota.stateNear",
  over: "quota.stateOver",
};

/** 状态 → Mantine 语义色；unlimited 由调用方自行决定是否着色（多数场景直接不显示徽章）。 */
export const QUOTA_STATE_COLOR: Record<QuotaState, string> = {
  unlimited: "gray",
  ok: "green",
  near: "orange",
  over: "red",
};

/**
 * 配额输入单位。
 * 管理员按 GB/MB 填写即可，不需要心算字节；`B` 只作为「不能整除」时的兜底单位，
 * 保证既有数值回显后原样保存不会丢精度。
 */
export type QuotaUnit = "B" | "MB" | "GB";

/** 输入框单位选择器的候选项顺序：常用的 MB / GB 在前，字节兜底放最后。 */
export const QUOTA_UNITS: QuotaUnit[] = ["MB", "GB", "B"];

const QUOTA_UNIT_FACTOR: Record<QuotaUnit, number> = {
  B: 1,
  MB: 1024 ** 2,
  GB: 1024 ** 3,
};

/** 数值在该单位下能否被「紧凑且精确」地表达；能则返回展示文本，否则 null。 */
function compactValue(total: number, factor: number): string | null {
  const scaled = total / factor;
  // 小于 1 的写法（0.5 GB）反而难读，交给更小一级的单位。
  if (scaled < 1) return null;
  const rounded = Number(scaled.toFixed(2));
  // 用「回显后原样保存是否等价」判定精度，而不是固定 epsilon：大数值下 epsilon 太松，
  // 会把 8589934593 显示成 8 GB（差 1 字节），保存时就悄悄改掉了上限。
  return Math.round(rounded * factor) === total ? String(rounded) : null;
}

/**
 * 字节 → 输入框取值与单位。
 * 优先用能紧凑表达的最大单位（10 GB / 8.5 GB / 512 MB）；「0.5 GB」这种小于 1 的写法
 * 反而难读，会继续退到更小一级的单位。两级单位都表达不了时退回字节，
 * 这样「读取既有配置 → 原样保存」不会因换算取整而悄悄改变上限值。
 */
export function splitQuotaBytes(bytes: number | null | undefined): {
  value: string;
  unit: QuotaUnit;
} {
  const total = bytes ?? 0;
  if (total <= 0) {
    // 0 = 不限：默认按 GB 展示，符合管理员填写配额的习惯。
    return { value: "0", unit: "GB" };
  }
  for (const unit of ["GB", "MB"] as const) {
    const value = compactValue(total, QUOTA_UNIT_FACTOR[unit]);
    if (value) return { value, unit };
  }
  return { value: String(total), unit: "B" };
}

/**
 * 输入框取值 + 单位 → 字节。
 * 空串按 0（不限）处理；非数字或负数返回 null，由调用方提示并阻止保存。
 */
export function parseQuotaBytes(raw: string, unit: QuotaUnit): number | null {
  const trimmed = raw.trim();
  if (!trimmed) return 0;
  const value = Number(trimmed);
  if (!Number.isFinite(value) || value < 0) return null;
  return Math.round(value * QUOTA_UNIT_FACTOR[unit]);
}

/**
 * 非负整数输入 → 整数（制品数上限 / 代理缓存保留天数共用）。
 * 空串按 0 处理（各自的「0」语义不同：配额是「不限」、保留天数是「关闭」）；
 * 非数字或负数返回 null，由调用方提示并阻止保存。
 */
export function parseNonNegativeInt(raw: string): number | null {
  const trimmed = raw.trim();
  if (!trimmed) return 0;
  const value = Number(trimmed);
  if (!Number.isFinite(value) || value < 0) return null;
  return Math.round(value);
}

/** 上限是否生效（0 / 缺省 = 不限）。 */
export function hasQuotaLimit(limit: number | null | undefined): limit is number {
  return typeof limit === "number" && limit > 0;
}
