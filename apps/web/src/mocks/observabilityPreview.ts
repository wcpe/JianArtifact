export type PreviewRange = "24h" | "7d" | "30d";

export interface PreviewTrendPoint {
  label: string;
  value: number;
}

export interface PreviewMetric {
  label: string;
  value: string;
  hint: string;
  tone?: "default" | "danger";
}

/**
 * 审计事件的 result 枚举值（数据契约的单一真源）。
 *
 * 这些值是**内部判定用**的数据标识，不是展示文案——聚合逻辑（成功数/失败数）依赖它们做
 * 等号比较，因此不能写成 `t("...")` 之类的翻译取值：翻译文案一旦改动，判定会静默失效
 * （例如把「失败」改成「未成功」会让所有失败事件被计成成功）。
 * 展示用的中文标签走 i18n 键（见 zh.ts 的 auditResult.*），与这里的键名对应但值独立。
 */
export const AUDIT_RESULT = {
  success: "成功",
  failure: "失败",
  applied: "已应用",
} as const;

export type AuditResult = (typeof AUDIT_RESULT)[keyof typeof AUDIT_RESULT];

export interface AuditPreviewEvent {
  id: string;
  source: "audit" | "replication";
  occurredAt: string;
  category: AuditPreviewCategory;
  timestamp: string;
  actor: string;
  authSource: string;
  action: string;
  target: string;
  result: AuditResult;
  operationId?: string;
  risk?: boolean;
  summary?: string;
  affectedCount?: number;
}

export type AuditPreviewCategory = "管理变更" | "制品变更" | "安全事件" | "同步复制";

interface DashboardRangePreview {
  requestSummary: string;
  failureSummary: string;
  metrics: PreviewMetric[];
  requests: PreviewTrendPoint[];
  downloads: PreviewTrendPoint[];
  failures: PreviewTrendPoint[];
  capacity: PreviewTrendPoint[];
  capacitySummary: string;
}

interface AuditRangePreview {
  events: PreviewTrendPoint[];
  failures: PreviewTrendPoint[];
}

const dashboardMetrics: PreviewMetric[] = [
  { label: "仓库数", value: "14", hint: "当前可用仓库" },
  { label: "制品数", value: "28,416", hint: "当前逻辑制品" },
  { label: "逻辑制品体积", value: "86.4 GB", hint: "按制品记录汇总" },
  { label: "请求量", value: "124,820", hint: "已完成协议请求" },
  { label: "下载量", value: "81,266", hint: "成功下载 200 / 206" },
  { label: "失败请求", value: "3", hint: "最终服务错误", tone: "danger" },
  { label: "缓存命中率", value: "92.8%", hint: "可缓存代理读取" },
];

const rangePoints: Record<PreviewRange, PreviewTrendPoint[]> = {
  "24h": [
    { label: "00:00", value: 132 },
    { label: "04:00", value: 98 },
    { label: "08:00", value: 420 },
    { label: "12:00", value: 604 },
    { label: "16:00", value: 712 },
    { label: "20:00", value: 486 },
  ],
  "7d": [
    { label: "周一", value: 8560 },
    { label: "周二", value: 9120 },
    { label: "周三", value: 8840 },
    { label: "周四", value: 10220 },
    { label: "周五", value: 11040 },
    { label: "周六", value: 8320 },
    { label: "周日", value: 9080 },
  ],
  "30d": [
    { label: "第 1 周", value: 44200 },
    { label: "第 2 周", value: 47800 },
    { label: "第 3 周", value: 50240 },
    { label: "第 4 周", value: 53680 },
  ],
};

function scalePoints(points: PreviewTrendPoint[], numerator: number, divisor: number) {
  return points.map((point) => ({
    ...point,
    value: Math.round((point.value * numerator) / divisor),
  }));
}

function dashboardRange(
  range: PreviewRange,
  requestSummary: string,
  failureSummary: string,
  capacitySummary: string,
): DashboardRangePreview {
  const requests = rangePoints[range];
  return {
    requestSummary,
    failureSummary,
    metrics: dashboardMetrics,
    requests,
    downloads: scalePoints(requests, 65, 100),
    failures: scalePoints(requests, 1, 120),
    capacity: scalePoints(requests, 9, 100),
    capacitySummary,
  };
}

export const dashboardPreview: Record<PreviewRange, DashboardRangePreview> = {
  "24h": dashboardRange(
    "24h",
    "文本摘要：请求量较上一时段增长 8.4%，失败请求 3 次。",
    "文本摘要：失败请求 3 次，主要集中在上午的上游重试时段。",
    "文本摘要：逻辑制品体积较 24 小时前增加 1.2 GB。",
  ),
  "7d": dashboardRange(
    "7d",
    "文本摘要：请求量较上一周期增长 11.2%，失败请求 17 次。",
    "文本摘要：失败请求 17 次，未出现连续失败高峰。",
    "文本摘要：逻辑制品体积较上周增加 6.8 GB。",
  ),
  "30d": dashboardRange(
    "30d",
    "文本摘要：请求量较上一周期增长 6.1%，失败请求 62 次。",
    "文本摘要：失败请求 62 次，趋势保持平稳。",
    "文本摘要：逻辑制品体积较上月增加 24.6 GB。",
  ),
};

export const auditPreview: Record<PreviewRange, AuditRangePreview> = {
  "24h": {
    events: rangePoints["24h"],
    failures: [
      { label: "00:00", value: 0 },
      { label: "04:00", value: 0 },
      { label: "08:00", value: 1 },
      { label: "12:00", value: 0 },
      { label: "16:00", value: 1 },
      { label: "20:00", value: 0 },
    ],
  },
  "7d": {
    events: rangePoints["7d"],
    failures: scalePoints(rangePoints["7d"], 1, 900),
  },
  "30d": {
    events: rangePoints["30d"],
    failures: scalePoints(rangePoints["30d"], 1, 900),
  },
};

export const auditPreviewEvents: AuditPreviewEvent[] = [
  {
    id: "audit-105",
    source: "audit",
    occurredAt: "2026-08-26T14:10:00Z",
    category: "制品变更",
    timestamp: "今天 14:10",
    actor: "release-bot",
    authSource: "协议账户",
    action: "重建 Maven 元数据",
    target: "maven-releases / com.example:demo",
    result: AUDIT_RESULT.success,
    operationId: "op_01J8X1Q9P7M4Z2A6F5C3D8H0K1",
    summary: "已完成当前仓库的关联元数据更新。",
    affectedCount: 2,
  },
  {
    id: "audit-104",
    source: "audit",
    occurredAt: "2026-08-26T14:11:00Z",
    category: "制品变更",
    timestamp: "今天 14:11",
    actor: "release-bot",
    authSource: "协议账户",
    action: "删除制品版本",
    target: "maven-releases / com.example:demo:1.4.2",
    result: AUDIT_RESULT.success,
    operationId: "op_01J8X1Q9P7M4Z2A6F5C3D8H0K1",
    risk: true,
    summary: "已删除当前 Hosted 仓库中的指定版本。",
    affectedCount: 12,
  },
  {
    id: "audit-103",
    source: "audit",
    occurredAt: "2026-08-26T11:38:00Z",
    category: "安全事件",
    timestamp: "今天 11:38",
    actor: "release-bot",
    authSource: "协议账户",
    action: "校验发布目标",
    target: "npm-private / internal/preview.tgz",
    result: AUDIT_RESULT.success,
    operationId: "op_01J8X1R2K6D3M8V4N9P5Q7S0T1",
    summary: "已完成目标路径和仓库状态校验。",
  },
  {
    id: "audit-102",
    source: "audit",
    occurredAt: "2026-08-26T11:37:00Z",
    category: "安全事件",
    timestamp: "今天 11:37",
    actor: "release-bot",
    authSource: "协议账户",
    action: "拒绝受限路径发布",
    target: "npm-private / internal/preview.tgz",
    result: AUDIT_RESULT.failure,
    operationId: "op_01J8X1R2K6D3M8V4N9P5Q7S0T1",
    summary: "命中发布账号允许路径前缀限制，未写入制品。",
  },
  {
    id: "audit-101",
    source: "audit",
    occurredAt: "2026-08-26T09:24:00Z",
    category: "管理变更",
    timestamp: "今天 09:24",
    actor: "wxys233",
    authSource: "网页会话",
    action: "创建 Hosted 仓库",
    target: "raw-releases",
    result: AUDIT_RESULT.success,
  },
];

export const auditCategories: Array<AuditPreviewCategory | "全部事件"> = [
  "全部事件",
  "管理变更",
  "制品变更",
  "安全事件",
  "同步复制",
];
