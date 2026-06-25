// 防护监控展示辅助（FR-78）：维度 / 严重度的中文标签与徽章配色。
// 集中维护展示映射，避免在监控页内散落魔法字符串与重复的 switch 分支。

/** 防护维度 → 中文标签（未知维度回退原值，兼容后端新增维度）。 */
const DIMENSION_LABELS: Record<string, string> = {
  rate_limit: '限流',
  ban: '自动封禁',
  cc_challenge: 'CC 挑战',
  waf: 'WAF 阻断',
  slowloris: '慢速攻击',
};

/** 取防护维度的中文标签。 */
export function dimensionLabel(dimension: string): string {
  return DIMENSION_LABELS[dimension] ?? dimension;
}

/** 严重度 → 中文标签。 */
const SEVERITY_LABELS: Record<string, string> = {
  warn: '警告',
  error: '错误',
};

/** 取严重度的中文标签。 */
export function severityLabel(severity: string): string {
  return SEVERITY_LABELS[severity] ?? severity;
}

/** 严重度 → 徽章配色（error 比 warn 更醒目）。 */
const SEVERITY_COLORS: Record<string, string> = {
  warn: 'yellow',
  error: 'red',
};

/** 取严重度的徽章配色。 */
export function severityColor(severity: string): string {
  return SEVERITY_COLORS[severity] ?? 'gray';
}
