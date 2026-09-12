// FR-117：设置页只保留可在管理端保存的服务配置。
import { IconCopy, IconRefresh } from "@tabler/icons-react";
import {
  ActionIcon,
  Alert,
  Button,
  Card,
  Divider,
  Group,
  NumberInput,
  Stack,
  Switch,
  Text,
  TextInput,
  Title,
} from "@mantine/core";
import { useState } from "react";
import { useTranslation } from "react-i18next";

import { getSettings, putSettings, type SettingsConfig } from "../api/endpoints";
import { AsyncBoundary } from "../components/AsyncBoundary";
import { useAsync } from "../hooks/useAsync";
import { notifyError, notifySuccess } from "../lib/feedback";
import { density } from "../theme/density";

const MIN_SECS = 1;
const MAX_SECS = 3600;

/**
 * 归一化设置对象：旧后端 / 旧 Mock 场景可能缺少 allowedHosts 与
 * originToken* 字段（FR-129/130 后新增），在表单状态初始化时补齐默认值，
 * 避免对 undefined 调用 join 等方法崩溃。
 */
function normalizeForm(initial: SettingsConfig): SettingsConfig {
  return {
    anonymousAccess: initial.anonymousAccess ?? false,
    publicUrl: initial.publicUrl ?? "",
    upstreamTimeout: initial.upstreamTimeout ?? 30,
    syncInterval: initial.syncInterval ?? 5,
    allowedHosts: Array.isArray(initial.allowedHosts) ? initial.allowedHosts : [],
    originTokenEnabled: initial.originTokenEnabled ?? false,
    originTokenHeader: initial.originTokenHeader ?? "",
    originTokenValue: initial.originTokenValue ?? "",
  };
}

// generateOriginToken 生成 32 字节随机 Token 的 hex 表示（64 字符），用于 CDN 回源请求头。
function generateOriginToken(): string {
  const bytes = new Uint8Array(32);
  crypto.getRandomValues(bytes);
  return Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("");
}

function ServiceSettingsForm({
  initial,
  onSaved,
}: {
  initial: SettingsConfig;
  onSaved: () => void;
}) {
  const { t } = useTranslation();
  const [form, setForm] = useState<SettingsConfig>(() => normalizeForm(initial));
  const [saving, setSaving] = useState(false);

  const save = () => {
    if (form.upstreamTimeout < MIN_SECS || form.upstreamTimeout > MAX_SECS) {
      notifyError(t("settings.validationRange"));
      return;
    }
    setSaving(true);
    putSettings({
      anonymousAccess: form.anonymousAccess,
      publicUrl: form.publicUrl,
      upstreamTimeout: form.upstreamTimeout,
      allowedHosts: form.allowedHosts,
      originTokenEnabled: form.originTokenEnabled,
      originTokenHeader: form.originTokenHeader,
      originTokenValue: form.originTokenValue,
    })
      .then((res) => {
        setForm(normalizeForm(res));
        notifySuccess(t("common.saved"));
        onSaved();
      })
      .catch(notifyError)
      .finally(() => setSaving(false));
  };

  return (
    <Card withBorder radius="md" padding={density.cardPadding} maw={680}>
      <Stack gap="md">
        <div>
          <Title order={5}>{t("settings.serviceTitle")}</Title>
          <Text size="sm" c="dimmed" mt={2}>
            {t("settings.serviceHint")}
          </Text>
        </div>
        {!initial.publicUrl ? (
          <Alert color="blue" variant="light" data-testid="settings-empty">
            {t("settings.publicUrlEmpty")}
          </Alert>
        ) : null}
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
            onChange={(event) => setForm({ ...form, anonymousAccess: event.currentTarget.checked })}
          />
        </Group>
        <TextInput
          label={t("settings.publicUrlLabel")}
          description={t("settings.publicUrlHint")}
          placeholder="https://repo.example.com"
          value={form.publicUrl}
          disabled={saving}
          onChange={(event) => setForm({ ...form, publicUrl: event.currentTarget.value })}
        />
        <NumberInput
          label={t("settings.upstreamTimeoutLabel")}
          min={MIN_SECS}
          max={MAX_SECS}
          value={form.upstreamTimeout}
          disabled={saving}
          onChange={(value) => setForm({ ...form, upstreamTimeout: Number(value) || 0 })}
        />
        <TextInput
          label={t("settings.allowedHostsLabel")}
          description={t("settings.allowedHostsHint")}
          placeholder="maven.example.com, repo.example.com"
          value={form.allowedHosts.join(", ")}
          disabled={saving}
          onChange={(event) =>
            setForm({
              ...form,
              allowedHosts: event.currentTarget.value
                .split(",")
                .map((item) => item.trim())
                .filter(Boolean),
            })
          }
        />
        <Divider my="xs" label={t("settings.securityTitle")} labelPosition="left" />
        <Group justify="space-between" gap="md" wrap="nowrap">
          <Stack gap={0}>
            <Text size="sm" fw={500}>
              {t("settings.originTokenLabel")}
            </Text>
            <Text size="xs" c="dimmed">
              {t("settings.originTokenHint")}
            </Text>
          </Stack>
          <Switch
            checked={form.originTokenEnabled}
            disabled={saving}
            aria-label={t("settings.originTokenLabel")}
            onChange={(event) =>
              setForm({ ...form, originTokenEnabled: event.currentTarget.checked })
            }
          />
        </Group>
        <TextInput
          label={t("settings.originTokenHeaderLabel")}
          placeholder="X-Jian-Origin-Token"
          value={form.originTokenHeader}
          disabled={saving || !form.originTokenEnabled}
          onChange={(event) => setForm({ ...form, originTokenHeader: event.currentTarget.value })}
        />
        <TextInput
          label={t("settings.originTokenValueLabel")}
          description={t("settings.originTokenValueHint")}
          value={form.originTokenValue}
          disabled={saving || !form.originTokenEnabled}
          onChange={(event) => setForm({ ...form, originTokenValue: event.currentTarget.value })}
          rightSection={
            <Group gap={4} wrap="nowrap">
              <ActionIcon
                variant="light"
                size="sm"
                aria-label={t("settings.originTokenGenerate")}
                disabled={!form.originTokenEnabled}
                onClick={() => setForm({ ...form, originTokenValue: generateOriginToken() })}
              >
                <IconRefresh size={16} />
              </ActionIcon>
              <ActionIcon
                variant="light"
                size="sm"
                aria-label={t("settings.originTokenCopy")}
                disabled={!form.originTokenValue}
                onClick={() =>
                  navigator.clipboard
                    ?.writeText(form.originTokenValue)
                    .then(() => notifySuccess(t("common.copied")))
                }
              >
                <IconCopy size={16} />
              </ActionIcon>
            </Group>
          }
        />
        <Group justify="flex-end">
          <Button size="xs" loading={saving} onClick={save}>
            {t("settings.save")}
          </Button>
        </Group>
      </Stack>
    </Card>
  );
}

export function SettingsPage() {
  const settingsState = useAsync(getSettings, [], { cacheKey: "settings:service" });

  return (
    <>
      <AsyncBoundary state={settingsState}>
        {(settings) => <ServiceSettingsForm initial={settings} onSaved={settingsState.reload} />}
      </AsyncBoundary>
    </>
  );
}
