// 审计深链拼装：观测图表（下载趋势 / 占比饼）点击后跳转审计工作台，
// 带上对应的时间窗与筛选条件，让「看到异常 → 查审计明细」一步直达。
//
// 参数名以 useAuditQuery 的 readUrlState 实际读取的 key 为准
// （range / from / to / q / clientIp / attention），注意是 attention 而非 attentionId。
// 空值一律不拼，避免产生 ?from=&to= 这类无效参数。

export interface AuditLinkOptions {
  /** 时间档位：24h / 3d / 7d / 30d / custom；跳转桶区间时固定传 "custom"。 */
  range?: string;
  /** 自定义起点（ISO date-time）；range=custom 时生效。 */
  from?: string;
  /** 自定义终点（ISO date-time）；range=custom 时生效。 */
  to?: string;
  /** 客户端 IP（饼图按 IP 分组点击时传组值）。 */
  clientIp?: string;
  /** 关键字（饼图按 UA 族分组点击时传组值，审计侧模糊匹配）。 */
  q?: string;
  /** 风险状态筛选：pending / acknowledged。 */
  attention?: string;
}

/**
 * 拼装审计工作台深链（/audit-logs?...）。
 * 只拼非空参数，顺序固定为 range → from → to → clientIp → q → attention，
 * 便于测试按参数名断言与人工阅读。
 */
export function buildAuditLink(opts: AuditLinkOptions): string {
  const params = new URLSearchParams();
  if (opts.range) params.set("range", opts.range);
  if (opts.from) params.set("from", opts.from);
  if (opts.to) params.set("to", opts.to);
  if (opts.clientIp) params.set("clientIp", opts.clientIp);
  if (opts.q) params.set("q", opts.q);
  if (opts.attention) params.set("attention", opts.attention);
  const suffix = params.toString();
  return suffix ? `/audit-logs?${suffix}` : "/audit-logs";
}
