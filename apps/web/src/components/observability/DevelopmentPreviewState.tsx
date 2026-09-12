// 开发态观测预览的状态边界：状态只从当前路由查询参数读取，不发起模拟接口请求。
import { EmptyState, ErrorState, LoadingState } from "@jianartifact/ui";
import { useEffect, useMemo, useState } from "react";
import { useLocation } from "react-router-dom";
import type { ReactNode } from "react";

type DevelopmentPreviewScenario = "normal" | "empty" | "loading" | "error" | "standby_read_only";

const previewScenarios = new Set<DevelopmentPreviewScenario>([
  "normal",
  "empty",
  "loading",
  "error",
  "standby_read_only",
]);

function readScenario(search: string): DevelopmentPreviewScenario {
  const scenario = new URLSearchParams(search).get("__mock");
  return scenario && previewScenarios.has(scenario as DevelopmentPreviewScenario)
    ? (scenario as DevelopmentPreviewScenario)
    : "normal";
}

/** 读取开发预览场景；重试只恢复本次视图，不修改 URL 或任何 Mock 数据。 */
export function useDevelopmentPreviewScenario() {
  const { search } = useLocation();
  const requestedScenario = useMemo(() => readScenario(search), [search]);
  const [recovered, setRecovered] = useState(false);

  useEffect(() => setRecovered(false), [requestedScenario]);

  return {
    scenario: recovered ? "normal" : requestedScenario,
    recover: () => setRecovered(true),
  } as const;
}

interface DevelopmentPreviewStateProps {
  subject: string;
  scenario: DevelopmentPreviewScenario;
  onRetry: () => void;
  children: ReactNode;
}

/** 统一呈现开发预览的读取状态；只读失败仍保留当前路由，重试回到本地正常样例。 */
export function DevelopmentPreviewState({
  subject,
  scenario,
  onRetry,
  children,
}: DevelopmentPreviewStateProps) {
  if (scenario === "empty") {
    return (
      <EmptyState
        message={`暂无${subject}预览数据`}
        description="当前筛选条件下没有可展示的本地样例。"
      />
    );
  }
  if (scenario === "loading") {
    return <LoadingState message={`正在加载${subject}预览数据…`} />;
  }
  if (scenario === "error") {
    return (
      <ErrorState
        message={`${subject}预览数据读取失败`}
        description="这是开发态的固定失败场景；不会访问节点或写入任何数据。"
        onRetry={onRetry}
        retryLabel="重新加载预览"
      />
    );
  }
  return <>{children}</>;
}
