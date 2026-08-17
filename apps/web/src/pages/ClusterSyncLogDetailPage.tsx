// 集群同步历史详情页（FR-98）：展示某次同步（repl_sync_log:id）拉取/应用的具体变更。
// 复用 SyncLogChangesView（与集群页详情模态框同一实现）；本页作为独立路由/深链入口。
import { useTranslation } from "react-i18next";
import { useParams } from "react-router-dom";
import { Button, Group } from "@mantine/core";
import { useNavigate } from "react-router-dom";
import { PageHeader } from "@jianartifact/ui";
import { SyncLogChangesView } from "../components/repo/SyncLogChangesView";

export function ClusterSyncLogDetailPage() {
  const { t } = useTranslation();
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const logId = Number(id);

  return (
    <>
      <PageHeader
        title={t("cluster.syncLogDetailTitle", { defaultValue: "同步历史详情" })}
        description={`#${logId}`}
      />
      <div style={{ height: "calc(100vh - 180px)", display: "flex", flexDirection: "column" }}>
        <SyncLogChangesView logId={logId} />
        <Group justify="flex-start" mt="md">
          <Button size="xs" variant="default" onClick={() => navigate("/cluster")}>
            {t("cluster.backToCluster", { defaultValue: "返回集群页" })}
          </Button>
        </Group>
      </div>
    </>
  );
}
