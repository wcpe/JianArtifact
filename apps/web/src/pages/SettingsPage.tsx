// FR-117：设置页只保留可在管理端保存的服务配置。
// 布局（v0.8.x）：双栏分区卡——左「服务设置」，右「安全防护」+「允许访问的域名」，
// 顶部一条操作行承载保存与「有未保存的改动」提示；窄屏自动回落单栏。
import { IconCopy, IconDeviceFloppy, IconRefresh } from "@tabler/icons-react";
import {
  ActionIcon,
  Alert,
  Button,
  Group,
  NumberInput,
  SimpleGrid,
  Stack,
  Switch,
  TagsInput,
  Text,
  TextInput,
} from "@mantine/core";
import { useState } from "react";
import { useTranslation } from "react-i18next";

import { getSettings, putSettings, type SettingsConfig } from "../api/endpoints";
import { AsyncBoundary } from "../components/AsyncBoundary";
import { OpsSection, StatusPill } from "../components/ops/OpsKit";
import { useAsync } from "../hooks/useAsync";
import { notifyError, notifySuccess } from "../lib/feedback";

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

/**
 * 归一化单条域名白名单输入：容忍粘贴完整 URL（https://repo.example.com/path），
 * 只保留主机名（域名或 IP，小写）。与后端 normalizeHostEntry 同口径：
 * 去协议 → 去 user@ → 去路径/查询 → 去端口 → 去 IPv6 方括号。
 */
export function normalizeHostInput(raw: string): string {
  let host = raw.trim();
  const schemeAt = host.indexOf("://");
  if (schemeAt >= 0) {
    host = host.slice(schemeAt + 3);
    const at = host.lastIndexOf("@");
    if (at >= 0) host = host.slice(at + 1);
  }
  const cut = host.search(/[/?#]/);
  if (cut >= 0) host = host.slice(0, cut);
  const bracketed = host.startsWith("[") && host.includes("]");
  if (bracketed) {
    // IPv6 字面量：[::1] 或 [::1]:8080。
    host = host.slice(1, host.indexOf("]"));
  } else if ((host.match(/:/g) ?? []).length === 1) {
    // 单冒号视为 host:port（IPv6 字面量本身含多个冒号，不在此列）。
    host = host.replace(/:\d+$/, "");
  }
  return host.trim().toLowerCase();
}

/** 表单「脏值」签名：用于「有未保存的改动」提示，避免对象键顺序影响比较。 */
function formSignature(form: SettingsConfig): string {
  return JSON.stringify([
    form.anonymousAccess,
    form.publicUrl,
    form.upstreamTimeout,
    form.allowedHosts,
    form.originTokenEnabled,
    form.originTokenHeader,
    form.originTokenValue,
  ]);
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
  const [savedSignature, setSavedSignature] = useState(() => formSignature(normalizeForm(initial)));
  const [saving, setSaving] = useState(false);
  const dirty = formSignature(form) !== savedSignature;

  const save = () => {
    if (form.upstreamTimeout < MIN_SECS || form.upstreamTimeout > MAX_SECS) {
      notifyError(t("settings.validationRange"));
      return;
    }
    setSaving(true);
    putSettings({
      anonymousAccess: form.anonymousAccess,
      publicUrl: form.publicUrl.trim(),
      upstreamTimeout: form.upstreamTimeout,
      allowedHosts: form.allowedHosts,
      originTokenEnabled: form.originTokenEnabled,
      originTokenHeader: form.originTokenHeader.trim(),
      originTokenValue: form.originTokenValue.trim(),
    })
      .then((res) => {
        const next = normalizeForm(res);
        setForm(next);
        setSavedSignature(formSignature(next));
        notifySuccess(t("common.saved"));
        onSaved();
      })
      .catch(notifyError)
      .finally(() => setSaving(false));
  };

  return (
    <Stack gap="md" maw={1080}>
      {/* 操作行：状态提示 + 保存（表单较长时不必滚到底部找按钮） */}
      <Group justify="space-between" align="center" wrap="wrap" gap="sm">
        <StatusPill
          tone={dirty ? "yellow" : "gray"}
          text={dirty ? t("settings.unsaved") : t("settings.saved")}
        />
        <Button
          size="sm"
          leftSection={<IconDeviceFloppy size={16} />}
          loading={saving}
          onClick={save}
        >
          {t("settings.save")}
        </Button>
      </Group>

      <SimpleGrid cols={{ base: 1, lg: 2 }} spacing="md" verticalSpacing="md">
        {/* 左栏：服务设置 */}
        <OpsSection
          title={t("settings.serviceTitle")}
          meta={t("settings.serviceHint")}
          style={{ height: "100%" }}
        >
          <Stack gap="md">
            {!initial.publicUrl ? (
              <Alert color="blue" variant="light" data-testid="settings-empty" p="xs">
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
                onChange={(event) =>
                  setForm({ ...form, anonymousAccess: event.currentTarget.checked })
                }
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
          </Stack>
        </OpsSection>

        {/* 右栏：安全防护 + 域名白名单 */}
        <Stack gap="md">
          <OpsSection
            title={t("settings.securityTitle")}
            meta={
              <StatusPill
                tone={form.originTokenEnabled ? "green" : "gray"}
                text={
                  form.originTokenEnabled ? t("settings.tokenEnabled") : t("settings.tokenDisabled")
                }
              />
            }
          >
            <Stack gap="md">
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
                onChange={(event) =>
                  setForm({ ...form, originTokenHeader: event.currentTarget.value })
                }
              />
              <TextInput
                label={t("settings.originTokenValueLabel")}
                description={t("settings.originTokenValueHint")}
                value={form.originTokenValue}
                disabled={saving || !form.originTokenEnabled}
                onChange={(event) =>
                  setForm({ ...form, originTokenValue: event.currentTarget.value })
                }
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
            </Stack>
          </OpsSection>

          <OpsSection
            title={t("settings.allowedHostsLabel")}
            meta={
              form.allowedHosts.length > 0
                ? t("settings.allowedHostsCount", { count: form.allowedHosts.length })
                : t("settings.allowedHostsUnlimited")
            }
          >
            <TagsInput
              aria-label={t("settings.allowedHostsLabel")}
              description={t("settings.allowedHostsHint")}
              placeholder="repo.example.com"
              value={form.allowedHosts}
              disabled={saving}
              clearable
              onChange={(value) =>
                setForm({
                  ...form,
                  allowedHosts: Array.from(new Set(value.map(normalizeHostInput).filter(Boolean))),
                })
              }
            />
          </OpsSection>
        </Stack>
      </SimpleGrid>
    </Stack>
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
