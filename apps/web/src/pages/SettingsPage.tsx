// FR-117：设置页只保留可在管理端保存的服务配置。
// 布局（v0.8.x）：顶部概览带（当前生效值 + 已保存/未保存 + 保存）→ 双栏分区卡
// （左「服务设置」，右「安全防护」+「允许访问的域名」）→ 底部「生效与回滚」说明（默认收起）；
// 窄屏自动回落单栏。
import {
  IconClock,
  IconCopy,
  IconDeviceFloppy,
  IconKey,
  IconRefresh,
  IconShieldCheck,
  IconWorld,
} from "@tabler/icons-react";
import {
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
import { OpsHelpButton, OpsKpiBand, OpsSection, StatusPill } from "../components/ops/OpsKit";
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
  // 概览带口径：展示「已保存 / 生效」值（来自 normalizeForm(initial)），不随未保存的
  // 表单改动跳动——改配置时能一眼对照当前线上口径。
  const effective = normalizeForm(initial);

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
    // 不再自造 maw：宽度口径统一由 AppLayout 内容容器（density.contentMaxWidth）承担。
    <Stack gap="md">
      {/* 顶部概览带：把「当前生效值」前置，改表单时能一眼对照已保存的口径。
          数据口径来自 normalizeForm(initial)（已保存/生效值），不随未保存的改动跳动。 */}
      <OpsKpiBand
        variant="strip"
        label={t("settings.summaryLabel")}
        cols={{ base: 2, sm: 4 }}
        items={[
          {
            label: t("settings.summaryAnonymous"),
            value: effective.anonymousAccess
              ? t("settings.summaryAnonymousOn")
              : t("settings.summaryAnonymousOff"),
            icon: <IconWorld size={16} />,
            hint: t("settings.summaryHint"),
          },
          {
            label: t("settings.summaryAllowedHosts"),
            value:
              effective.allowedHosts.length === 0
                ? t("settings.summaryAllowedHostsAll")
                : t("settings.allowedHostsCount", { count: effective.allowedHosts.length }),
            icon: <IconShieldCheck size={16} />,
            hint: t("settings.summaryHint"),
          },
          {
            label: t("settings.summaryOriginToken"),
            value: effective.originTokenEnabled
              ? t("settings.summaryOriginTokenOn")
              : t("settings.summaryOriginTokenOff"),
            icon: <IconKey size={16} />,
            hint: t("settings.summaryHint"),
          },
          {
            label: t("settings.summaryUpstreamTimeout"),
            value: t("settings.summaryTimeoutValue", { secs: effective.upstreamTimeout }),
            icon: <IconClock size={16} />,
            hint: t("settings.summaryHint"),
          },
        ]}
        actions={
          <>
            {/* 生效与回滚说明搬到工具栏按钮上（气泡里），不再占页面布局。 */}
            <OpsHelpButton
              title={t("settings.effectsTitle")}
              items={[
                {
                  label: t("settings.effectsImmediateLabel", { defaultValue: "立即生效" }),
                  value: t("settings.effectsImmediate"),
                },
                {
                  label: t("settings.effectsLocalScopeLabel", { defaultValue: "节点本地" }),
                  value: t("settings.effectsLocalScope"),
                },
                {
                  label: t("settings.effectsLockoutLabel", { defaultValue: "锁死风险" }),
                  value: t("settings.effectsLockout"),
                },
              ]}
            />
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
          </>
        }
      />

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
              />
              {/* 生成 / 复制从输入框右侧图标移出为「图标 + 文字」按钮：
                  图标-only 看不出是生成还是复制，且 rightSection 里也塞不下文字。 */}
              <Group gap="xs" wrap="wrap">
                <Button
                  size="compact-xs"
                  variant="light"
                  leftSection={<IconRefresh size={14} />}
                  aria-label={t("settings.originTokenGenerate")}
                  disabled={!form.originTokenEnabled}
                  onClick={() => setForm({ ...form, originTokenValue: generateOriginToken() })}
                >
                  {t("settings.originTokenGenerate")}
                </Button>
                <Button
                  size="compact-xs"
                  variant="light"
                  leftSection={<IconCopy size={14} />}
                  aria-label={t("settings.originTokenCopy")}
                  disabled={!form.originTokenValue}
                  onClick={() =>
                    navigator.clipboard
                      ?.writeText(form.originTokenValue)
                      .then(() => notifySuccess(t("common.copied")))
                  }
                >
                  {t("settings.originTokenCopy")}
                </Button>
              </Group>
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
