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
import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { useLocation } from "react-router-dom";

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

/** 集群配置表单（FR-D 多对端）：读 getClusterStatus 初始化，保存走 setClusterConfig（令牌留空不变）。 */
function ClusterSettingsForm({
  initial,
  onSaved,
}: {
  initial: ClusterStatus;
  onSaved: () => void;
}) {
  const { t } = useTranslation();
  // 多对端列表：优先 initial.peers（不含令牌明文），回退旧单值 peerUrl。
  const [peers, setPeers] = useState<{ url: string; token: string }[]>(
    initial.peers && initial.peers.length > 0
      ? initial.peers.map((p) => ({ url: p.url, token: "" }))
      : initial.peerUrl
        ? [{ url: initial.peerUrl, token: "" }]
        : [{ url: "", token: "" }],
  );
  const [enabled, setEnabled] = useState(initial.enabled);
  const [saving, setSaving] = useState(false);

  const updatePeer = (idx: number, patch: Partial<{ url: string; token: string }>) => {
    setPeers((prev) => prev.map((p, i) => (i === idx ? { ...p, ...patch } : p)));
  };

  const addPeer = () => setPeers((prev) => [...prev, { url: "", token: "" }]);
  const removePeer = (idx: number) => setPeers((prev) => prev.filter((_, i) => i !== idx));

  const save = () => {
    setSaving(true);
    // 过滤空 URL 行；token 留空 = 不变（后端对空 token 保留原值）。
    const valid = peers.filter((p) => p.url.trim() !== "");
    setClusterConfig({
      peers: valid.map((p) => ({ url: p.url.trim(), token: p.token || undefined })),
      enabled,
    })
      .then(() => {
        notifySuccess(t("common.saved"));
        onSaved();
      })
      .catch(notifyError)
      .finally(() => setSaving(false));
  };

  return (
    <Card withBorder radius="md" padding={density.cardPadding} maw={760}>
      <Stack gap="sm">
        <Title order={5}>{t("settings.clusterTitle")}</Title>
        <Text size="xs" c="dimmed">
          {t("settings.clusterPeersHint")}
        </Text>
        {peers.map((p, idx) => (
          <Group key={idx} gap="sm" align="flex-end" wrap="nowrap">
            <TextInput
              label={idx === 0 ? t("cluster.peerUrlLabel") : undefined}
              placeholder="https://repo1.wcpe.top"
              value={p.url}
              disabled={saving}
              style={{ flex: 1 }}
              onChange={(e) => updatePeer(idx, { url: e.currentTarget.value })}
            />
            <PasswordInput
              label={idx === 0 ? t("cluster.peerTokenLabel") : undefined}
              placeholder={t("settings.peerTokenPlaceholder")}
              value={p.token}
              disabled={saving}
              style={{ flex: 1 }}
              onChange={(e) => updatePeer(idx, { token: e.currentTarget.value })}
            />
            <Button
              size="xs"
              variant="subtle"
              color="red"
              disabled={saving || peers.length <= 1}
              onClick={() => removePeer(idx)}
            >
              {t("common.delete")}
            </Button>
          </Group>
        ))}
        <Group>
          <Button size="xs" variant="default" disabled={saving} onClick={addPeer}>
            {t("settings.addPeer")}
          </Button>
        </Group>
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
  const location = useLocation();
  const basicState = useAsync(getSettings, []);
  const clusterState = useAsync(getClusterStatus, []);
  // tab 接 URL 锚点（#basic / #cluster）：刷新/回退/分享可定位到对应 tab。
  const [tab, setTab] = useState(() => location.hash.replace("#", "") || "basic");
  useEffect(() => {
    const onHash = () => setTab(window.location.hash.replace("#", "") || "basic");
    window.addEventListener("hashchange", onHash);
    return () => window.removeEventListener("hashchange", onHash);
  }, []);

  return (
    <>
      <PageHeader title={t("settings.title")} description={t("settings.description")} />
      {/* 内容区宽度由全局 contentMaxWidth 控制，页面内不再二次限宽（FR-90 布局修复）。 */}
      <Tabs
        value={tab}
        onChange={(v) => {
          const next = v ?? "basic";
          setTab(next);
          window.location.hash = next;
        }}
      >
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
