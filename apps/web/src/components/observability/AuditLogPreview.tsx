// 当前节点审计开发预览：统一记录流和聚合视图只在开发态加载。
import { Stack } from "@mantine/core";

import { density } from "../../theme/density";
import { AuditPreview } from "./AuditPreview";
import { DevelopmentPreviewState, useDevelopmentPreviewScenario } from "./DevelopmentPreviewState";
import { PreviewNotice } from "./PreviewNotice";

export function AuditLogPreview() {
  const { scenario, recover } = useDevelopmentPreviewScenario();
  return (
    <DevelopmentPreviewState subject="当前节点审计" scenario={scenario} onRetry={recover}>
      <Stack gap={density.gridSpacing}>
        <PreviewNotice />
        <AuditPreview />
      </Stack>
    </DevelopmentPreviewState>
  );
}
