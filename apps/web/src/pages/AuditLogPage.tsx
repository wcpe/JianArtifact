// 当前节点审计页（FR-118）：方案 A 双栏调查工作台。
// 开发态数据由 DevMock 提供；旧实现（AuditLogView / 方案 R AuditWorkbenchView）已清理。
import { AuditWorkbenchView } from "../components/audit/AuditWorkbenchView";

export function AuditLogPage() {
  return <AuditWorkbenchView />;
}
