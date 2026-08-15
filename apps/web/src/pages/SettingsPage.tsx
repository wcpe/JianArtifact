// FR-89/90: 设置页——「基础设置」tab（匿名访问开关/对外 URL/回源超时/同步间隔，
// 经 GET/PUT /api/v1/settings 读写，保存后运行时生效）与「集群」tab
// （对端 URL/令牌/自动同步开关，复用 GET/PUT /api/v1/cluster，令牌不回显）。
// 仅管理员；原分散于用户页（匿名开关）与集群页（对端配置）的配置迁入本页。
import {
  Button,
  Card,
  Group,
  NumberInput,
  PasswordInput,
  Stack,
  Switch,
  Tabs,
  Text,
  TextInput,
  Title,
} from "@mantine/core";
import { PageHeader } from "@jianartifact/ui";
import { useState } from "react";
import { useTranslation } from "react-i18next";

import { AsyncBoundary } from "../components/AsyncBoundary";
import {
  getClusterStatus,
  getSettings,
  putSettings,
  setClusterConfig,
  type ClusterStatus,
  type SettingsConfig,
} from "../api/endpoints";
import { useAsync } from "../hooks/useAsync";
import { notifyError, notifySuccess } from "../lib/feedback";
import { density } from "../theme/density";

/** 秒级配置的允许范围（与后端校验一致）。 */
const MIN_SECS = 1;
const MAX_SECS = 3600;

/** 基础设置表单：读 getSettings 初始化，保存走 putSettings（四项全量提交）。 */
function BasicSettingsForm({ initial, onSaved }: { initial: SettingsConfig; onSaved: () => void }) {
  const { t } = useTranslation();
  const [form, setForm] = useState<SettingsConfig>({ ...initial });
  const [saving, setSaving] = useState(false);

  const save = () => {
    if (
      form.upstreamTimeout < MIN_SECS ||
      form.upstreamTimeout > MAX_SECS ||
      form.syncInterval < MIN_SECS ||
      form.syncInterval > MAX_SECS
    ) {
      notifyError(t("settings.validationRange"));
      return;
    }
    setSaving(true);
    putSettings({
      anonymousAccess: form.anonymousAccess,
      publicUrl: form.publicUrl,
      upstreamTimeout: form.upstreamTimeout,
      syncInterval: form.syncInterval,
    })
      .then((res) => {
        setForm({ ...res });
        notifySuccess(t("common.saved"));
        onSaved();
      })
      .catch(notifyError)
      .finally(() => setSaving(false));
  };

  return (
    <Card withBorder radius="md" padding={density.cardPadding} maw={640}>
      <Stack gap="sm">
        <Title order={5}>{t("settings.basicTitle")}</Title>
        <Group justify="space-between" gap="md" wrap="nowrap">
          <Stack gap={0}>
            <Text size="sm" fw={500}>
              {t("settings.anonymousLabel")}
            </Text>
            <Text size="xs" c="dimmed">
              {t("settings.anonymousHint")}
            </Text>
          </Stack>
          <Switch
            checked={form.anonymousAccess}
            disabled={saving}
            aria-label={t("settings.anonymousLabel")}
            onChange={(e) => setForm({ ...form, anonymousAccess: e.currentTarget.checked })}
          />
        </Group>
        <TextInput
          label={t("settings.publicUrlLabel")}
          description={t("settings.publicUrlHint")}
          placeholder="https://repo.wcpe.top"
          value={form.publicUrl}
          disabled={saving}
          onChange={(e) => setForm({ ...form, publicUrl: e.currentTarget.value })}
        />
        <Group grow>
          <NumberInput
            label={t("settings.upstreamTimeoutLabel")}
            min={MIN_SECS}
            max={MAX_SECS}
            value={form.upstreamTimeout}
            disabled={saving}
            onChange={(v) => setForm({ ...form, upstreamTimeout: Number(v) || 0 })}
          />
          <NumberInput
            label={t("settings.syncIntervalLabel")}
            description={t("settings.syncIntervalHint")}
            min={MIN_SECS}
            max={MAX_SECS}
            value={form.syncInterval}
            disabled={saving}
            onChange={(v) => setForm({ ...form, syncInterval: Number(v) || 0 })}
          />
        </Group>
        <Group justify="flex-end">
          <Button size="xs" loading={saving} onClick={save}>
            {t("settings.saveConfig")}
          </Button>
        </Group>
      </Stack>
    </Card>
  );
}

/** 集群配置表单：读 getClusterStatus 初始化，保存走 setClusterConfig（令牌留空不变）。 */
function ClusterSettingsForm({
  initial,
  onSaved,
}: {
  initial: ClusterStatus;
  onSaved: () => void;
}) {
  const { t } = useTranslation();
  const [peerUrl, setPeerUrl] = useState(initial.peerUrl ?? "");
  const [peerToken, setPeerToken] = useState("");
  const [enabled, setEnabled] = useState(initial.enabled);
  const [saving, setSaving] = useState(false);

  const save = () => {
    setSaving(true);
    setClusterConfig({
      peerUrl: peerUrl || undefined, // 空串 = 不变（与集群页原逻辑一致）
      peerToken: peerToken || undefined,
      enabled,
    })
      .then(() => {
        setPeerToken(""); // 令牌不回显
        notifySuccess(t("common.saved"));
        onSaved();
      })
      .catch(notifyError)
      .finally(() => setSaving(false));
  };

  return (
    <Card withBorder radius="md" padding={density.cardPadding} maw={640}>
      <Stack gap="sm">
        <Title order={5}>{t("settings.clusterTitle")}</Title>
        <TextInput
          label={t("cluster.peerUrlLabel")}
          placeholder="https://repo1.wcpe.top"
          value={peerUrl}
          disabled={saving}
          onChange={(e) => setPeerUrl(e.currentTarget.value)}
        />
        <PasswordInput
          label={t("cluster.peerTokenLabel")}
          placeholder={t("settings.peerTokenPlaceholder")}
          value={peerToken}
          disabled={saving}
          onChange={(e) => setPeerToken(e.currentTarget.value)}
        />
        <Group justify="space-between" gap="md" wrap="nowrap">
          <Stack gap={0}>
            <Text size="sm" fw={500}>
              {t("cluster.enabledLabel")}
            </Text>
            <Text size="xs" c="dimmed">
              {t("cluster.enabledHint")}
            </Text>
          </Stack>
          <Switch
            checked={enabled}
            disabled={saving}
            aria-label={t("cluster.enabledLabel")}
            onChange={(e) => setEnabled(e.currentTarget.checked)}
          />
        </Group>
        <Group justify="flex-end">
          <Button size="xs" loading={saving} onClick={save}>
            {t("settings.saveConfig")}
          </Button>
        </Group>
      </Stack>
    </Card>
  );
}

export function SettingsPage() {
  const { t } = useTranslation();
  const basicState = useAsync(getSettings, []);
  const clusterState = useAsync(getClusterStatus, []);

  return (
    <>
      <PageHeader title={t("settings.title")} description={t("settings.description")} />
      <Tabs defaultValue="basic" maw={900}>
        <Tabs.List>
          <Tabs.Tab value="basic">{t("settings.basicTitle")}</Tabs.Tab>
          <Tabs.Tab value="cluster">{t("settings.clusterTitle")}</Tabs.Tab>
        </Tabs.List>

        {/* 基础设置 tab：匿名开关 / 对外 URL / 回源超时 / 同步间隔（FR-89）。 */}
        <Tabs.Panel value="basic" pt="md">
          <AsyncBoundary state={basicState}>
            {(st) => <BasicSettingsForm initial={st} onSaved={basicState.reload} />}
          </AsyncBoundary>
        </Tabs.Panel>

        {/* 集群 tab：对端 URL / 令牌 / 自动同步开关（FR-88 配置迁入）。 */}
        <Tabs.Panel value="cluster" pt="md">
          <AsyncBoundary state={clusterState}>
            {(st) => <ClusterSettingsForm initial={st} onSaved={clusterState.reload} />}
          </AsyncBoundary>
        </Tabs.Panel>
      </Tabs>
    </>
  );
}
