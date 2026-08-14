// FR-86: 集群页——展示节点间复制同步状态与调度启停控制（仅管理员）。
// 数据来自非契约端点 GET/PUT /api/v1/cluster（复制引擎见 FR-83~85）。
import { Badge, Card, Group, Stack, Switch, Text, Title } from "@mantine/core";
import { EmptyState, PageHeader } from "@jianartifact/ui";
import { useReducer, useState } from "react";
import { useTranslation } from "react-i18next";

import { AsyncBoundary } from "../components/AsyncBoundary";
import { getClusterStatus, setClusterEnabled } from "../api/endpoints";
import type { ClusterStatus } from "../api/endpoints";
import { useAsync } from "../hooks/useAsync";
import { density } from "../theme/density";

function StatRow({ label, value }: { label: string; value: string }) {
  return (
    <Group justify="space-between" gap="md" wrap="nowrap">
      <Text size="sm" c="dimmed">
        {label}
      </Text>
      <Text size="sm" fw={500} ta="right">
        {value}
      </Text>
    </Group>
  );
}

export function ClusterPage() {
  const { t } = useTranslation();
  const state = useAsync(getClusterStatus, []);

  return (
    <>
      <PageHeader title={t("cluster.title")} description={t("cluster.description")} />
      <AsyncBoundary state={state}>
        {(st) => (
          <Stack gap="md" maw={640}>
            {!st.peerUrl ? (
              <Card withBorder radius="md" padding={density.cardPadding}>
                <EmptyState message={t("cluster.notConfigured")} />
              </Card>
            ) : (
              <>
                <Card withBorder radius="md" padding={density.cardPadding}>
                  <Stack gap="xs">
                    <Title order={5}>{t("cluster.title")}</Title>
                    <StatRow label={t("cluster.nodeId")} value={st.nodeId} />
                    <StatRow label={t("cluster.peerUrl")} value={st.peerUrl} />
                    <StatRow
                      label={t("cluster.token")}
                      value={
                        st.tokenSet ? t("cluster.tokenConfigured") : t("cluster.tokenMissing")
                      }
                    />
                    <StatRow
                      label={t("cluster.watermark")}
                      value={
                        st.hasWatermark ? String(st.watermark) : t("cluster.watermarkNone")
                      }
                    />
                    <StatRow
                      label={t("cluster.lastSync")}
                      value={st.lastSyncAt ?? t("cluster.lastSyncNever")}
                    />
                    {st.lastError && (
                      <StatRow label={t("cluster.lastError")} value={st.lastError} />
                    )}
                  </Stack>
                </Card>

                <Card withBorder radius="md" padding={density.cardPadding}>
                  <Group justify="space-between" gap="md">
                    <Stack gap={0}>
                      <Text size="sm" fw={500}>
                        {t("cluster.enabledLabel")}
                      </Text>
                      <Text size="xs" c="dimmed">
                        {t("cluster.enabledHint")}
                      </Text>
                    </Stack>
                    <Badge color={st.enabled ? "green" : "gray"} variant="light">
                      {st.enabled ? t("common.yes") : t("common.no")}
                    </Badge>
                    <ClusterToggle st={st} />
                  </Group>
                </Card>
              </>
            )}
          </Stack>
        )}
      </AsyncBoundary>
    </>
  );
}

/** 启停开关：调 PUT /cluster 后刷新本地状态。 */
function ClusterToggle({ st }: { st: ClusterStatus }) {
  const { t } = useTranslation();
  const [busy, setBusy] = useState(false);
  const [, force] = useReducer((x) => x + 1, 0);

  return (
    <Switch
      checked={st.enabled}
      disabled={busy}
      aria-label={t("cluster.enabledLabel")}
      onChange={async (e) => {
        setBusy(true);
        try {
          await setClusterEnabled(e.currentTarget.checked);
          force();
        } finally {
          setBusy(false);
        }
      }}
    />
  );
}
