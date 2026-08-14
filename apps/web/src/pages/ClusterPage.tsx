// FR-86/88: 集群页——对端配置（web 可视化）、自动同步开关、立即同步按钮与同步状态展示（仅管理员）。
// 数据来自非契约端点 GET/PUT /api/v1/cluster、POST /api/v1/cluster/sync-now（复制引擎见 FR-83~85）。
import { Badge, Button, Card, Group, Stack, Switch, Text, TextInput, Title } from "@mantine/core";
import { EmptyState, PageHeader } from "@jianartifact/ui";
import { useReducer, useState } from "react";
import { useTranslation } from "react-i18next";

import { AsyncBoundary } from "../components/AsyncBoundary";
import { getClusterStatus, setClusterConfig, triggerClusterSync } from "../api/endpoints";
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
  const [, force] = useReducer((x) => x + 1, 0);

  // 对端配置表单（令牌不回显，留空保持不变）。
  const [peerUrl, setPeerUrl] = useState("");
  const [peerToken, setPeerToken] = useState("");
  const [saving, setSaving] = useState(false);
  const [syncing, setSyncing] = useState(false);

  return (
    <>
      <PageHeader title={t("cluster.title")} description={t("cluster.description")} />
      <AsyncBoundary state={state}>
        {(st) => (
          <Stack gap="md" maw={640}>
            {/* 对端配置表单（FR-88）：保存仅入库，不自动开始同步。 */}
            <Card withBorder radius="md" padding={density.cardPadding}>
              <Stack gap="sm">
                <Title order={5}>{t("cluster.peerConfigTitle")}</Title>
                <TextInput
                  label={t("cluster.peerUrlLabel")}
                  placeholder="http://192.168.100.108:50020"
                  value={peerUrl || st.peerUrl || ""}
                  onChange={(e) => setPeerUrl(e.currentTarget.value)}
                />
                <TextInput
                  label={t("cluster.peerTokenLabel")}
                  placeholder={t("cluster.peerTokenPlaceholder")}
                  value={peerToken}
                  onChange={(e) => setPeerToken(e.currentTarget.value)}
                />
                <Group justify="space-between" align="center">
                  <Text size="xs" c="dimmed">
                    {t("cluster.peerConfigHint")}
                  </Text>
                  <Button
                    size="xs"
                    variant="default"
                    loading={saving}
                    onClick={async () => {
                      setSaving(true);
                      try {
                        await setClusterConfig({
                          peerUrl: peerUrl || undefined,
                          peerToken: peerToken || undefined,
                        });
                        setPeerToken(""); // 令牌不回显
                        force();
                      } finally {
                        setSaving(false);
                      }
                    }}
                  >
                    {t("cluster.saveConfig")}
                  </Button>
                </Group>
              </Stack>
            </Card>

            {!st.peerUrl ? (
              <Card withBorder radius="md" padding={density.cardPadding}>
                <EmptyState message={t("cluster.notConfigured")} />
              </Card>
            ) : (
              <Card withBorder radius="md" padding={density.cardPadding}>
                <Stack gap="xs">
                  <Title order={5}>{t("cluster.title")}</Title>
                  <StatRow label={t("cluster.nodeId")} value={st.nodeId} />
                  <StatRow label={t("cluster.peerUrl")} value={st.peerUrl} />
                  <StatRow
                    label={t("cluster.token")}
                    value={st.tokenSet ? t("cluster.tokenConfigured") : t("cluster.tokenMissing")}
                  />
                  <StatRow
                    label={t("cluster.watermark")}
                    value={st.hasWatermark ? String(st.watermark) : t("cluster.watermarkNone")}
                  />
                  <StatRow
                    label={t("cluster.lastSync")}
                    value={st.lastSyncAt ?? t("cluster.lastSyncNever")}
                  />
                  {st.lastError && <StatRow label={t("cluster.lastError")} value={st.lastError} />}
                </Stack>
              </Card>
            )}

            {/* 同步控制：自动同步开关 + 立即同步（FR-88）。 */}
            <Card withBorder radius="md" padding={density.cardPadding}>
              <Group justify="space-between" gap="md" wrap="nowrap">
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
                <Group gap="xs" wrap="nowrap">
                  <Button
                    size="xs"
                    variant="default"
                    loading={syncing}
                    disabled={!st.peerUrl}
                    onClick={async () => {
                      setSyncing(true);
                      try {
                        await triggerClusterSync();
                        force();
                      } finally {
                        setSyncing(false);
                      }
                    }}
                  >
                    {t("cluster.syncNow")}
                  </Button>
                  <Switch
                    checked={st.enabled}
                    disabled={saving}
                    aria-label={t("cluster.enabledLabel")}
                    onChange={async (e) => {
                      setSaving(true);
                      try {
                        await setClusterConfig({ enabled: e.currentTarget.checked });
                        force();
                      } finally {
                        setSaving(false);
                      }
                    }}
                  />
                </Group>
              </Group>
            </Card>
          </Stack>
        )}
      </AsyncBoundary>
    </>
  );
}
