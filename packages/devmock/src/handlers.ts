// Mock 处理器：产出符合契约的响应体。类型绑定到 openapi-typescript 生成的
// schema.gen.ts（编译期），运行期再由 contract 测试用 ajv 对同一契约做校验。
import type { components } from "./schema.gen";

export type HealthStatus = components["schemas"]["HealthStatus"];
export type ApiError = components["schemas"]["Error"];

/** GET /healthz 的契约响应。 */
export function mockHealthz(version = "0.1.0-mock"): HealthStatus {
  return { status: "ok", version };
}

/** GET /readyz 就绪时的契约响应。 */
export function mockReadyz(version = "0.1.0-mock"): HealthStatus {
  return { status: "ok", version };
}

/** GET /readyz 未就绪（503）时的契约错误响应。 */
export function mockUnavailable(): ApiError {
  return { code: "dependency_unavailable", message: "依赖未就绪" };
}
