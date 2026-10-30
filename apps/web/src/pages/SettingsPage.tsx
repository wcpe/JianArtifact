// 设置页只保留可在管理端保存的服务配置。
// 布局（v0.8.x）：顶部概览带（当前生效值 + 已保存/未保存 + 保存）→ 双栏分区卡
// （左「服务设置」，右「安全防护」+「允许访问的域名」）→ 全宽的「运行期策略」分区
// （13 项，默认折叠概览）→ 窄屏自动回落单栏。
import {
  IconChevronDown,
  IconChevronUp,
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
  Badge,
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
import {
  OpsHelpButton,
  OpsKpiBand,
  OpsSection,
  StatusPill,
  type OpsTone,
} from "../components/ops/OpsKit";
import { useAsync } from "../hooks/useAsync";
import { formatBytes } from "../lib/format";
import { notifyError, notifySuccess } from "../lib/feedback";
import { PageShell } from "../app/PageShell";

const MIN_SECS = 1;
const MAX_SECS = 3600;
// 运行期策略的取值上界，逐项对齐后端 settings_handlers.go 的常量：
// maxBackupAutoIntervalSecs（作业间隔 30 天）、maxConfigHours / maxConfigDays（一年）、
// maxTrashMaxBytes（1 TiB）、maxConfigSecs（秒级请求预算 3600）。
//
// 为什么不像后端那样分成 4 个量纲各定一个常量就够了：前端的区间只用来在保存前
// 拦「明显笔误」，真正的夹取与留痕在后端做（环境变量入口与设置页入口必须同口径，
// 界面自己再夹一套就成了第二份真源）。所以这里的上界照抄后端，不另立标准。
const MAX_JOB_INTERVAL_SECS = 30 * 24 * 3600;
const MAX_CONFIG_HOURS = 8760;
const MAX_CONFIG_DAYS = 365;
// 1 TiB。必须写成幂运算而不是 `1 << 40`：JS 位运算按 32 位有符号整数执行，
// `1 << 40` 实际得到 256，上界会静默变成一个荒谬的小值。
const MAX_TRASH_BYTES = 1024 ** 4;

/** 运行期策略分区的字段键（顺序与规格 §2.1 的迁移清单一致）。 */
type RuntimePolicyKey =
  | "blobGcInterval"
  | "storageCleanupInterval"
  | "backupOrphanCleanupInterval"
  | "trashGcInterval"
  | "upstreamProbeInterval"
  | "backupOrphanGraceHours"
  | "storageMetadataRetentionDays"
  | "storageTempMaxAgeHours"
  | "trashRetentionDays"
  | "trashMaxBytes"
  | "autoBlockBaseSeconds"
  | "searchTimeoutSeconds"
  | "capacityCacheTtlSeconds";

/**
 * 生效时机（规格 §2.2）。**必须逐项如实标注**。
 *
 * 这是本批最容易做错的地方：13 项里没有一项真正「保存即影响当前状态」——
 * 作业间隔要等调度器下一轮、保留策略要等作业下一轮现读、请求预算要等下一次请求。
 * 一律写「立即生效」会让运维改完立刻去验证，然后得出「配置没生效」的错误结论；
 * 写错比不写更糟。
 */
type RuntimePolicyTiming = "jobInterval" | "nextJobRun" | "nextCleanup" | "nextRequest";

/** 单项运行期策略的声明式描述。 */
interface RuntimePolicyField {
  key: RuntimePolicyKey;
  /**
   * 允许的最小值。0 = 该项的「禁用 / 不清理 / 不限制 / 禁用缓存」是合法取值；
   * 1 = 该项没有关闭语义（0 会让配置静默失效），与后端 zeroAllowed=false 一一对应。
   */
  min: number;
  max: number;
  timing: RuntimePolicyTiming;
}

interface RuntimePolicyGroup {
  id: string;
  /** 分组标题的 i18n 键。 */
  titleKey: string;
  fields: RuntimePolicyField[];
}

/**
 * 四组：作业间隔 / 保留策略 / 容量与阈值 / 回源与搜索。
 *
 * 为什么用声明式数组而不是逐项手写 13 段 JSX：13 项除「键 / 区间 / 生效时机」外完全同构，
 * 手写会让「某一项漏写 0 的语义」这类缺陷藏在 13 份雷同代码里；集中声明后，
 * 区间与生效时机都是必填字段，漏一项的后果是编译期就少一个元素、界面直接显示不出来。
 */
const RUNTIME_POLICY_GROUPS: RuntimePolicyGroup[] = [
  {
    id: "jobIntervals",
    titleKey: "settings.runtimePolicyGroupJobIntervals",
    fields: [
      { key: "blobGcInterval", min: 0, max: MAX_JOB_INTERVAL_SECS, timing: "jobInterval" },
      { key: "storageCleanupInterval", min: 0, max: MAX_JOB_INTERVAL_SECS, timing: "jobInterval" },
      {
        key: "backupOrphanCleanupInterval",
        min: 0,
        max: MAX_JOB_INTERVAL_SECS,
        timing: "jobInterval",
      },
      { key: "trashGcInterval", min: 0, max: MAX_JOB_INTERVAL_SECS, timing: "jobInterval" },
      { key: "upstreamProbeInterval", min: 0, max: MAX_JOB_INTERVAL_SECS, timing: "jobInterval" },
    ],
  },
  {
    id: "retention",
    titleKey: "settings.runtimePolicyGroupRetention",
    fields: [
      // 宽限期是唯一**不允许 0** 的一项：零宽限会让「刚拷进 backups/、还没在界面导入」
      // 的包在下一轮就被判为孤儿删掉，而这正是宽限期存在的唯一目的。
      { key: "backupOrphanGraceHours", min: 1, max: MAX_CONFIG_HOURS, timing: "nextJobRun" },
      { key: "storageMetadataRetentionDays", min: 0, max: MAX_CONFIG_DAYS, timing: "nextJobRun" },
      { key: "storageTempMaxAgeHours", min: 0, max: MAX_CONFIG_HOURS, timing: "nextJobRun" },
      // 与上面三项不同：回收站在下一次 cleanup / trash-gc 的前置判断时才读，
      // 故单独标 nextCleanup。同组的 4 项不同时机，正说明时机必须逐项标而不能按组标。
      { key: "trashRetentionDays", min: 0, max: MAX_CONFIG_DAYS, timing: "nextCleanup" },
    ],
  },
  {
    id: "thresholds",
    titleKey: "settings.runtimePolicyGroupThresholds",
    fields: [
      // 上限以字节提交：超上限时清理请求会被拒，换算上的取整误差会让上限和实际可用空间对不上。
      { key: "trashMaxBytes", min: 0, max: MAX_TRASH_BYTES, timing: "nextCleanup" },
      { key: "capacityCacheTtlSeconds", min: 0, max: MAX_SECS, timing: "nextRequest" },
    ],
  },
  {
    id: "upstream",
    titleKey: "settings.runtimePolicyGroupUpstream",
    fields: [
      // 断路器退避与搜索预算都**没有禁用语义**（0 窗口等于失败不阻止、0 预算等于放开无界查询），
      // 非正数由后端回落默认，因此下限是 1。
      { key: "autoBlockBaseSeconds", min: 1, max: MAX_SECS, timing: "nextRequest" },
      { key: "searchTimeoutSeconds", min: 1, max: MAX_SECS, timing: "nextRequest" },
    ],
  },
];

/** 展平后的 13 项：校验、脏值签名与提交体都按这一份固定顺序遍历。 */
const RUNTIME_POLICY_FIELDS: RuntimePolicyField[] = RUNTIME_POLICY_GROUPS.flatMap(
  (group) => group.fields,
);

/**
 * 生效时机的展示口径：文案键 + 徽章色。
 *
 * 用不同色区分而不是全用同一色：这四类的验证方式完全不同（看调度器状态 / 等下一轮作业 /
 * 等下一次清理 / 直接发一个请求），颜色是唯一能在不改布局的前提下做出区分的信号。
 */
const RUNTIME_POLICY_TIMING: Record<RuntimePolicyTiming, { textKey: string; tone: OpsTone }> = {
  jobInterval: { textKey: "settings.runtimePolicyTimingJobIntervals", tone: "blue" },
  nextJobRun: { textKey: "settings.runtimePolicyTimingNextJobRun", tone: "indigo" },
  nextCleanup: { textKey: "settings.runtimePolicyTimingNextCleanup", tone: "cyan" },
  nextRequest: { textKey: "settings.runtimePolicyTimingNextRequest", tone: "teal" },
};

/**
 * 单项的标签 / 帮助文案键：由字段名推导（runtimePolicyBlobGcIntervalLabel / ...Help）。
 *
 * 13 × 2 个键手写两遍，正是「中英不同步」与「某一项文案接错字段」的典型产生方式；
 * 推导后键名与字段名的对应关系只有一处。
 */
function runtimePolicyTextKeys(key: RuntimePolicyKey): { label: string; help: string } {
  const suffix = key.charAt(0).toUpperCase() + key.slice(1);
  return {
    label: `settings.runtimePolicy${suffix}Label`,
    help: `settings.runtimePolicy${suffix}Help`,
  };
}

/**
 * 归一化设置对象：旧后端 / 旧 Mock 场景可能缺少 allowedHosts 与
 * originToken* 字段（/130 后新增），在表单状态初始化时补齐默认值，
 * 避免对 undefined 调用 join 等方法崩溃。
 */
function normalizeForm(initial: SettingsConfig): SettingsConfig {
  return {
    anonymousAccess: initial.anonymousAccess ?? false,
    publicUrl: initial.publicUrl ?? "",
    upstreamTimeout: initial.upstreamTimeout ?? 30,
    allowedHosts: Array.isArray(initial.allowedHosts) ? initial.allowedHosts : [],
    originTokenEnabled: initial.originTokenEnabled ?? false,
    originTokenHeader: initial.originTokenHeader ?? "",
    originTokenValue: initial.originTokenValue ?? "",
    // 三项备份配置由「迁移与备份」页的专用卡片编辑；本页只是携带它们透传，
    // 以免整对象提交时把这三项丢成 undefined（后端按"传哪个改哪个"处理，
    // 丢掉即等于本页会静默复位别人的设置）。
    backupAutoInterval: initial.backupAutoInterval ?? 0,
    backupKeepLast: initial.backupKeepLast ?? 7,
    backupRetentionDays: initial.backupRetentionDays ?? 30,
    // 的派生字段同样要透传：本页不编辑它们，但整对象提交时
    // 缺失会让后端拿到空值。回落到"按秒表达"是安全的，语义与旧行为一致。
    backupAutoIntervalExpr: initial.backupAutoIntervalExpr ?? "",
    backupAutoIntervalValue: initial.backupAutoIntervalValue ?? initial.backupAutoInterval ?? 0,
    backupAutoIntervalUnit: initial.backupAutoIntervalUnit ?? "seconds",
    backupLatestPackageBytes: initial.backupLatestPackageBytes ?? 0,
    // 两项 proxy/group 调优（本页可编辑）。缺省值与后端一致；负缓存 TTL 的 0
    // 是「关闭」的合法取值，必须用 ?? 而不是 ||，否则关闭态会被打回 60。
    groupMemberTimeout: initial.groupMemberTimeout ?? 3,
    negativeCacheTTL: initial.negativeCacheTTL ?? 60,
    // 13 项运行期策略（本页「运行期策略」分区可编辑）。缺省值与后端
    // settings_handlers.go 的同名常量一致，兜「旧后端 / 未播种」两种情形。
    //
    // 同样只能用 ??：其中 10 项的 0 是合法取值（禁用作业 / 不自动清理 /
    // 不限制 / 禁用缓存），写成 `|| 86400` 会把「已禁用」显示成「每天一次」，
    // 界面上的值与真实行为相反。
    blobGcInterval: initial.blobGcInterval ?? 86400,
    storageCleanupInterval: initial.storageCleanupInterval ?? 86400,
    backupOrphanCleanupInterval: initial.backupOrphanCleanupInterval ?? 86400,
    trashGcInterval: initial.trashGcInterval ?? 86400,
    upstreamProbeInterval: initial.upstreamProbeInterval ?? 60,
    backupOrphanGraceHours: initial.backupOrphanGraceHours ?? 1,
    storageMetadataRetentionDays: initial.storageMetadataRetentionDays ?? 7,
    storageTempMaxAgeHours: initial.storageTempMaxAgeHours ?? 24,
    trashRetentionDays: initial.trashRetentionDays ?? 7,
    // 2 GiB 已超 32 位 int，必须用幂运算：`2 << 30` 按 32 位算会得到负数。
    trashMaxBytes: initial.trashMaxBytes ?? 2 * 1024 ** 3,
    autoBlockBaseSeconds: initial.autoBlockBaseSeconds ?? 40,
    searchTimeoutSeconds: initial.searchTimeoutSeconds ?? 10,
    capacityCacheTtlSeconds: initial.capacityCacheTtlSeconds ?? 30,
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
    form.groupMemberTimeout,
    form.negativeCacheTTL,
    // 13 项运行期策略逐项计入：漏掉它们会让「改了运行期策略但状态仍显示已保存」，
    // 甚至因签名相同被判为未改动。顺序固定（与声明顺序一致），故按列表展开即可。
    ...RUNTIME_POLICY_FIELDS.map((field) => form[field.key]),
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
  // 「运行期策略」分区默认折叠：13 个数字输入框常驻会把页面撑到需要反复滚动，
  // 而进设置页的常见目的只是改一两个字段。
  const [policyExpanded, setPolicyExpanded] = useState(false);
  const dirty = formSignature(form) !== savedSignature;
  // 概览带口径：展示「已保存 / 生效」值（来自 normalizeForm(initial)），不随未保存的
  // 表单改动跳动——改配置时能一眼对照当前线上口径。
  const effective = normalizeForm(initial);

  const save = () => {
    if (form.upstreamTimeout < MIN_SECS || form.upstreamTimeout > MAX_SECS) {
      notifyError(t("settings.validationRange"));
      return;
    }
    // 成员确认预算无「关闭」语义：0 秒等于每个成员都立刻超时，因此下限是 1。
    if (form.groupMemberTimeout < MIN_SECS || form.groupMemberTimeout > MAX_SECS) {
      notifyError(t("settings.groupMemberTimeoutRange"));
      return;
    }
    // 负缓存 TTL 的下限是 0——0 恰恰是「关闭负缓存」的显式取值，不能被前端拦掉。
    if (form.negativeCacheTTL < 0 || form.negativeCacheTTL > MAX_SECS) {
      notifyError(t("settings.negativeCacheTTLRange"));
      return;
    }
    // 13 项运行期策略：逐项按声明区间检查，报错时带上字段名，
    // 免得用户在一屏 13 个输入框里自己找是哪一项不合规。
    // 这里只拦「明显笔误」；真正的夹取与中文警告由后端完成（两个入口必须同口径）。
    for (const field of RUNTIME_POLICY_FIELDS) {
      const label = t(runtimePolicyTextKeys(field.key).label);
      const value = form[field.key];
      if (value < field.min) {
        notifyError(
          field.min > 0
            ? t("settings.runtimePolicyPositiveRequired", { label })
            : t("settings.runtimePolicyNonNegative", { label }),
        );
        return;
      }
      if (value > field.max) {
        notifyError(t("settings.runtimePolicyTooLarge", { label, max: field.max }));
        return;
      }
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
      groupMemberTimeout: form.groupMemberTimeout,
      negativeCacheTTL: form.negativeCacheTTL,
      // 13 项运行期策略必须逐项提交：后端是「传哪个改哪个」，
      // 漏传即等于保持原值，而用户会以为改动已保存。
      blobGcInterval: form.blobGcInterval,
      storageCleanupInterval: form.storageCleanupInterval,
      backupOrphanCleanupInterval: form.backupOrphanCleanupInterval,
      trashGcInterval: form.trashGcInterval,
      upstreamProbeInterval: form.upstreamProbeInterval,
      backupOrphanGraceHours: form.backupOrphanGraceHours,
      storageMetadataRetentionDays: form.storageMetadataRetentionDays,
      storageTempMaxAgeHours: form.storageTempMaxAgeHours,
      trashRetentionDays: form.trashRetentionDays,
      // 提交的始终是字节数，界面上的「≈ 2.0 GB」只是显示换算。
      trashMaxBytes: form.trashMaxBytes,
      autoBlockBaseSeconds: form.autoBlockBaseSeconds,
      searchTimeoutSeconds: form.searchTimeoutSeconds,
      capacityCacheTtlSeconds: form.capacityCacheTtlSeconds,
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
    // 不再自造 maw：宽度口径统一由 AppLayout 内容容器承担（现为铺满可用宽度、不居中）。
    <Stack gap="md" style={{ flex: 1, minHeight: 0, overflowY: "auto", paddingRight: 4 }}>
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
            {/* 两项 proxy/group 调优。放在「服务设置」而非安全区：它们调的是读路径的
                失败速度与记忆时长，不涉及访问控制。帮助文案必须写明「改动即时生效、
                无需重启」与「0 = 关闭负缓存」——后者是界面上唯一能表达关闭语义的地方。 */}
            <NumberInput
              label={t("settings.groupMemberTimeoutLabel")}
              description={t("settings.groupMemberTimeoutHint")}
              min={MIN_SECS}
              max={MAX_SECS}
              value={form.groupMemberTimeout}
              disabled={saving}
              onChange={(value) => setForm({ ...form, groupMemberTimeout: Number(value) || 0 })}
            />
            <NumberInput
              label={t("settings.negativeCacheTTLLabel")}
              description={t("settings.negativeCacheTTLHint")}
              min={0}
              max={MAX_SECS}
              value={form.negativeCacheTTL}
              disabled={saving}
              onChange={(value) => setForm({ ...form, negativeCacheTTL: Number(value) || 0 })}
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

      {/* 运行期策略：13 项塞不进上面两栏（纵向会拉到两三屏高），单独成区并默认折叠成概览。
          折叠态只报「四组各几项」，刻意不在折叠态写任何生效时机——
          四组内部的时机并不统一（如「保留策略」里回收站保留天数是下一次清理生效，
          其余三项是下一次作业生效），按组写一句时机就是错的。 */}
      <OpsSection
        title={t("settings.runtimePolicyTitle")}
        meta={t("settings.runtimePolicyHint")}
        actions={
          <Button
            size="compact-sm"
            variant="subtle"
            aria-expanded={policyExpanded}
            leftSection={
              policyExpanded ? <IconChevronUp size={16} /> : <IconChevronDown size={16} />
            }
            onClick={() => setPolicyExpanded(!policyExpanded)}
          >
            {policyExpanded
              ? t("settings.runtimePolicyCollapse")
              : t("settings.runtimePolicyExpand")}
          </Button>
        }
        testId="settings-runtime-policy"
      >
        {policyExpanded ? (
          <Stack gap="lg">
            {RUNTIME_POLICY_GROUPS.map((group) => (
              <Stack gap="sm" key={group.id}>
                <Text size="xs" fw={700} c="dimmed">
                  {t(group.titleKey)}
                </Text>
                <SimpleGrid cols={{ base: 1, sm: 2, lg: 3 }} spacing="md" verticalSpacing="md">
                  {group.fields.map((field) => {
                    const keys = runtimePolicyTextKeys(field.key);
                    const timing = RUNTIME_POLICY_TIMING[field.timing];
                    const label = t(keys.label);
                    return (
                      <Stack gap={4} key={field.key}>
                        <Group justify="space-between" gap="xs" wrap="nowrap" align="center">
                          <Text size="sm" fw={500}>
                            {label}
                          </Text>
                          {/* 生效时机逐项标注。位置固定在标签行右侧：它是每项的必读信息，
                              塞进 description 尾部会被长帮助文案淹掉。 */}
                          <Badge size="xs" variant="light" color={timing.tone}>
                            {t(timing.textKey)}
                          </Badge>
                        </Group>
                        <NumberInput
                          aria-label={label}
                          description={t(keys.help)}
                          min={field.min}
                          max={field.max}
                          allowDecimal={false}
                          value={form[field.key]}
                          disabled={saving}
                          onChange={(value) =>
                            setForm({ ...form, [field.key]: Number(value) || 0 })
                          }
                        />
                        {field.key === "trashMaxBytes" ? (
                          // 字节数直接输入没人算得清，旁边给出可读换算；
                          // 但提交值仍是字节——换算只用于显示，取整误差会让上限与实际可用空间对不上。
                          <Text size="xs" c="dimmed">
                            {t("settings.runtimePolicyBytesPreview", {
                              human: formatBytes(form.trashMaxBytes),
                            })}
                          </Text>
                        ) : null}
                      </Stack>
                    );
                  })}
                </SimpleGrid>
              </Stack>
            ))}
          </Stack>
        ) : (
          <Stack gap="xs">
            <SimpleGrid cols={{ base: 1, sm: 2, lg: 4 }} spacing="sm" verticalSpacing="sm">
              {RUNTIME_POLICY_GROUPS.map((group) => (
                <Stack gap={0} key={group.id}>
                  <Text size="sm" fw={500}>
                    {t(group.titleKey)}
                  </Text>
                  <Text size="xs" c="dimmed">
                    {t("settings.runtimePolicyGroupCount", { count: group.fields.length })}
                  </Text>
                </Stack>
              ))}
            </SimpleGrid>
            <Text size="xs" c="dimmed">
              {t("settings.runtimePolicyOverviewHint")}
            </Text>
          </Stack>
        )}
      </OpsSection>
    </Stack>
  );
}

export function SettingsPage() {
  const settingsState = useAsync(getSettings, [], { cacheKey: "settings:service" });

  return (
    <PageShell testId="settings-page">
      <AsyncBoundary state={settingsState}>
        {(settings) => <ServiceSettingsForm initial={settings} onSaved={settingsState.reload} />}
      </AsyncBoundary>
    </PageShell>
  );
}
