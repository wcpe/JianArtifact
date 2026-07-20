// 健康状态与 api/openapi.yaml 的 HealthStatus.status 枚举对齐；
// 契约一致性由 @jianartifact/devmock 的 contract 测试守护，此处仅做展示映射。
export type HealthState = "ok" | "degraded" | "unavailable";

const labels: Record<HealthState, string> = {
  ok: "运行正常",
  degraded: "降级运行",
  unavailable: "不可用",
};

/** 将契约状态枚举映射为中文展示标签。 */
export function describeStatus(status: HealthState): string {
  return labels[status];
}
