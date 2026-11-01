// 设置页只保留可在管理端保存的服务配置。
// 布局（v0.8.x）：顶部概览带（当前生效值 + 已保存/未保存 + 保存）→ 双栏分区卡
// （左「服务设置」，右「安全防护」+「允许访问的域名」）→ 全宽的「运行期策略」分区
// （13 项全部铺开、逐项独立保存）→ 窄屏自动回落单栏。
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
  Badge,
  Button,
  Group,
  NumberInput,
  Popover,
  Select,
  SimpleGrid,
  Stack,
  Switch,
  Table,
  TagsInput,
  Text,
  TextInput,
} from "@mantine/core";
import { useState } from "react";
import { useTranslation } from "react-i18next";

import {
  getSettingHistory,
  getSettings,
  putSettings,
  type SettingHistoryEntry,
  type SettingsConfig,
} from "../api/endpoints";
import { AsyncBoundary } from "../components/AsyncBoundary";
import {
  OpsHelpButton,
  OpsKpiBand,
  OpsSection,
  StatusPill,
  type OpsTone,
} from "../components/ops/OpsKit";
import { useAsync } from "../hooks/useAsync";
import {
  BYTE_UNITS,
  DURATION_UNITS,
  bytesToBytes,
  durationToSeconds,
  splitBytes,
  splitSeconds,
  type ByteUnit,
  type DurationUnit,
} from "../lib/units";
import { formatBytes, formatStamp } from "../lib/format";
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

/**
 * 数值量纲：决定界面用哪种单位控件输入。
 *
 * - seconds：以秒为提交单位，界面可切到分钟/小时/天（避免面对 86400 这种数字）
 * - hours / days：单位固定，只显示后缀（这些项换单位没有意义，1.5 天不如 36 小时直观）
 * - bytes：以字节为提交单位，界面可切到 MB/GB/TB
 */
type RuntimePolicyDimension = "seconds" | "hours" | "days" | "bytes";

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
  /** 量纲：决定界面单位控件与后缀。 */
  dimension: RuntimePolicyDimension;
  /** 对应的 setting 表键名，用于查历史（与 API JSON 字段名不同）。 */
  settingKey: string;
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
      {
        key: "blobGcInterval",
        min: 0,
        max: MAX_JOB_INTERVAL_SECS,
        timing: "jobInterval",
        dimension: "seconds",
        settingKey: "blob_gc_interval",
      },
      {
        key: "storageCleanupInterval",
        min: 0,
        max: MAX_JOB_INTERVAL_SECS,
        timing: "jobInterval",
        dimension: "seconds",
        settingKey: "storage_cleanup_interval",
      },
      {
        key: "backupOrphanCleanupInterval",
        min: 0,
        max: MAX_JOB_INTERVAL_SECS,
        timing: "jobInterval",
        dimension: "seconds",
        settingKey: "backup_orphan_cleanup_interval",
      },
      {
        key: "trashGcInterval",
        min: 0,
        max: MAX_JOB_INTERVAL_SECS,
        timing: "jobInterval",
        dimension: "seconds",
        settingKey: "trash_gc_interval",
      },
      {
        key: "upstreamProbeInterval",
        min: 0,
        max: MAX_JOB_INTERVAL_SECS,
        timing: "jobInterval",
        dimension: "seconds",
        settingKey: "upstream_probe_interval",
      },
    ],
  },
  {
    id: "retention",
    titleKey: "settings.runtimePolicyGroupRetention",
    fields: [
      // 宽限期是唯一**不允许 0** 的一项：零宽限会让「刚拷进 backups/、还没在界面导入」
      // 的包在下一轮就被判为孤儿删掉，而这正是宽限期存在的唯一目的。
      {
        key: "backupOrphanGraceHours",
        min: 1,
        max: MAX_CONFIG_HOURS,
        timing: "nextJobRun",
        dimension: "hours",
        settingKey: "backup_orphan_grace_hours",
      },
      {
        key: "storageMetadataRetentionDays",
        min: 0,
        max: MAX_CONFIG_DAYS,
        timing: "nextJobRun",
        dimension: "days",
        settingKey: "storage_metadata_retention_days",
      },
      {
        key: "storageTempMaxAgeHours",
        min: 0,
        max: MAX_CONFIG_HOURS,
        timing: "nextJobRun",
        dimension: "hours",
        settingKey: "storage_temp_max_age_hours",
      },
      // 与上面三项不同：回收站在下一次 cleanup / trash-gc 的前置判断时才读，
      // 故单独标 nextCleanup。同组的 4 项不同时机，正说明时机必须逐项标而不能按组标。
      {
        key: "trashRetentionDays",
        min: 0,
        max: MAX_CONFIG_DAYS,
        timing: "nextCleanup",
        dimension: "days",
        settingKey: "trash_retention_days",
      },
    ],
  },
  {
    id: "thresholds",
    titleKey: "settings.runtimePolicyGroupThresholds",
    fields: [
      // 上限以字节提交：超上限时清理请求会被拒，换算上的取整误差会让上限和实际可用空间对不上。
      {
        key: "trashMaxBytes",
        min: 0,
        max: MAX_TRASH_BYTES,
        timing: "nextCleanup",
        dimension: "bytes",
        settingKey: "trash_max_bytes",
      },
      {
        key: "capacityCacheTtlSeconds",
        min: 0,
        max: MAX_SECS,
        timing: "nextRequest",
        dimension: "seconds",
        settingKey: "capacity_cache_ttl_seconds",
      },
    ],
  },
  {
    id: "upstream",
    titleKey: "settings.runtimePolicyGroupUpstream",
    fields: [
      // 断路器退避与搜索预算都**没有禁用语义**（0 窗口等于失败不阻止、0 预算等于放开无界查询），
      // 非正数由后端回落默认，因此下限是 1。
      {
        key: "autoBlockBaseSeconds",
        min: 1,
        max: MAX_SECS,
        timing: "nextRequest",
        dimension: "seconds",
        settingKey: "auto_block_base_seconds",
      },
      {
        key: "searchTimeoutSeconds",
        min: 1,
        max: MAX_SECS,
        timing: "nextRequest",
        dimension: "seconds",
        settingKey: "search_timeout_seconds",
      },
    ],
  },
];

/** 展平后的 13 项：校验、脏值签名与提交体都按这一份固定顺序遍历。 */
const RUNTIME_POLICY_FIELDS: RuntimePolicyField[] = RUNTIME_POLICY_GROUPS.flatMap(
  (group) => group.fields,
);

/** 输入框当前值 + 所选单位（仅 seconds / bytes 量纲会用到单位）。 */
interface UnitDraft {
  /**
   * 输入框里的数值。null 表示「当前是空输入」。
   *
   * 必须区分 null 与 0：对 10 项运行期策略来说 0 是**合法且有语义**的取值
   * （禁用作业 / 不限制 / 关闭缓存）。若把空输入折成 0，
   * 「清空准备重输」这一中间态就会被存成「显式关闭」，两者意图完全不同。
   */
  value: number | null;
  unit: string;
}

/**
 * 推断某字段初次渲染时应显示的单位与数值。
 *
 * 时长按「能整除的最大单位」拆（86400 秒 → 1 天）；小时/天量纲本就是原值；
 * 字节按 MB/GB/TB 拆（2 GiB → 2 GB）。
 */
function inferUnitDraft(field: RuntimePolicyField, base: number): UnitDraft {
  if (field.dimension === "bytes") {
    const { value, unit } = splitBytes(base);
    return { value, unit };
  }
  if (field.dimension === "seconds") {
    const { value, unit } = splitSeconds(base);
    return { value, unit };
  }
  return { value: base, unit: field.dimension };
}

/** 把「输入框里的数值 + 单位」换算回提交用的基础单位（秒 / 字节）。 */
function unitToBase(field: RuntimePolicyField, value: number, unit: string): number {
  if (field.dimension === "bytes") return bytesToBytes(value, unit as ByteUnit);
  if (field.dimension === "seconds") return durationToSeconds(value, unit as DurationUnit);
  return value;
}

/** 换算成另一单位下应显示的数值（换单位不改已生效的基础值）。 */
function replotInUnit(field: RuntimePolicyField, base: number, unit: string): number {
  if (field.dimension === "bytes") {
    const found = BYTE_UNITS.find((it) => it.value === unit);
    return found ? base / found.bytes : base;
  }
  if (field.dimension === "seconds") {
    const found = DURATION_UNITS.find((it) => it.value === unit);
    return found ? base / found.seconds : base;
  }
  return base;
}

/**
 * 单位文案的 i18n 键。三个展示函数共用，避免各处再写一遍中文
 * （英文界面里出现「小时」「天」正是这么来的）。
 */
const UNIT_LABEL_KEYS: Record<string, string> = {
  seconds: "settings.unitSeconds",
  minutes: "settings.unitMinutes",
  hours: "settings.unitHours",
  days: "settings.unitDays",
};

/** 把单位标识翻成当前语言的单位名。 */
function unitLabel(t: (key: string) => string, unit: string): string {
  const key = UNIT_LABEL_KEYS[unit];
  return key ? t(key) : unit;
}

/** 固定单位字段的后缀文案；可切换单位的项由下拉承载，不再重复显示后缀。 */
function unitSuffix(
  t: (key: string) => string,
  field: RuntimePolicyField,
  draft: UnitDraft,
): string | undefined {
  if (field.dimension === "hours") return unitLabel(t, "hours");
  if (field.dimension === "days") return unitLabel(t, "days");
  // 秒/字节量纲：单位已在右侧下拉里显示，输入框内不再叠一个后缀。
  void draft;
  return undefined;
}

/** 把秒数写成人类可读形式（用于「将提交 N 秒」这类说明）。 */
function humanizeSeconds(t: (key: string) => string, total: number): string {
  const { value, unit } = splitSeconds(total);
  return `${value} ${unitLabel(t, unit)}`;
}

/**
 * 历史里的值按字段量纲做可读化：库里存的是秒/字节，直接摆出来运维还得自己换算。
 * 空串表示「该键此前从未落库」（首次设置）。
 */
function formatHistoryValue(
  t: (key: string) => string,
  field: RuntimePolicyField,
  raw: string,
): string {
  if (raw === "") return t("settings.runtimePolicyHistoryUnset");
  const n = Number(raw);
  if (!Number.isFinite(n)) return raw;
  if (field.dimension === "bytes") return formatBytes(n);
  if (field.dimension === "seconds") return humanizeSeconds(t, n);
  if (field.dimension === "hours") return `${n} ${unitLabel(t, "hours")}`;
  if (field.dimension === "days") return `${n} ${unitLabel(t, "days")}`;
  return raw;
}

/**
 * 单项设置的历史变更**模态框**。
 *
 * 为什么是 Modal 而不是内联展开：设置页内容区是定高可滚动容器，
 * 就地展开一份历史会把同一行的相邻字段挤到下一行、整页跳动。
 * 「看历史」是随手查一下的动作，不应改变页面上任何既有元素的位置。
 *
 * 空态明确写「暂无记录」而不是留白：留白会让人以为功能坏了，
 * 而实际上「从没改过」是完全正常的状态。
 */
function SettingHistoryBody({
  entries,
  loading,
  error,
  formatValue,
}: {
  entries: SettingHistoryEntry[];
  loading: boolean;
  error?: string;
  formatValue: (raw: string) => string;
}) {
  const { t } = useTranslation();
  return (
    <>
      {loading ? (
        <Text size="sm" c="dimmed">
          {t("settings.runtimePolicyHistoryLoading")}
        </Text>
      ) : error ? (
        <Text size="sm" c="red">
          {t("settings.runtimePolicyHistoryError")}
        </Text>
      ) : entries.length === 0 ? (
        <Text size="sm" c="dimmed">
          {t("settings.runtimePolicyHistoryEmpty")}
        </Text>
      ) : (
        <Table striped highlightOnHover withTableBorder>
          <Table.Thead>
            <Table.Tr>
              <Table.Th>{t("settings.runtimePolicyHistoryColTime")}</Table.Th>
              <Table.Th>{t("settings.runtimePolicyHistoryColActor")}</Table.Th>
              <Table.Th>{t("settings.runtimePolicyHistoryColChange")}</Table.Th>
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {entries.map((entry, idx) => (
              <Table.Tr key={`${entry.ts}-${idx}`}>
                {/* 走项目既有 formatStamp：后端时间可能是 SQLite naive UTC 或 RFC3339，
                    直接回显原始串在英文界面里会看到一串机器格式。 */}
                <Table.Td style={{ whiteSpace: "nowrap" }}>{formatStamp(entry.ts)}</Table.Td>
                <Table.Td style={{ whiteSpace: "nowrap" }}>{entry.actor || "—"}</Table.Td>
                <Table.Td>
                  <Text size="sm" ff="monospace">
                    {formatValue(entry.oldValue)} → {formatValue(entry.newValue)}
                  </Text>
                </Table.Td>
              </Table.Tr>
            ))}
          </Table.Tbody>
        </Table>
      )}
    </>
  );
}

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
  // 已保存值按**字段**记录，而不是维护一个整体签名。
  //
  // 为什么不能用一个全局签名：逐项保存时只有该字段落库，其余字段可能还带着未保存的改动。
  // 若拿「当前整表」重算签名写回，那些未提交的改动会被一并算进「已保存」，
  // 顶部随即显示「已保存」——用户据此离开页面，改动就静默丢了。
  const [savedValues, setSavedValues] = useState<SettingsConfig>(() => normalizeForm(initial));
  const [saving, setSaving] = useState(false);
  // 逐项保存中的字段：只禁用该行，其余字段仍可继续编辑。
  const [savingField, setSavingField] = useState<RuntimePolicyKey | null>(null);
  // 逐项校验结果与「输入已完成」标记。校验只在失焦/回车后给出，
  // 不在每次按键时弹错——边打字边报错会让「删掉旧值准备重输」的中间态一直显示错误。
  const [touchedFields, setTouchedFields] = useState<Partial<Record<RuntimePolicyKey, boolean>>>(
    {},
  );
  // 每项当前所选单位（键是设置字段名）。缺失表示「按当前值自动推断」。
  const [unitDrafts, setUnitDrafts] = useState<Partial<Record<RuntimePolicyKey, UnitDraft>>>({});
  // 历史用**模态框**展示，绝不在页面上就地展开。
  //
  // 为什么必须用 Modal 而不是内联展开：设置页内容区是定高可滚动容器，
  // 内联展开一份历史会把同一行的其它字段挤到下一行、整页跳动；
  // 而「看历史」是随手查一下的动作，不该改变页面上任何既有元素的位置。
  // 只保留「当前查看的是哪个键」这一个状态，数据不带多份缓存。
  const [historyKey, setHistoryKey] = useState<string | null>(null);
  const [historyEntries, setHistoryEntries] = useState<SettingHistoryEntry[]>([]);
  const [historyLoading, setHistoryLoading] = useState(false);
  const [historyError, setHistoryError] = useState<string | undefined>(undefined);

  /** 打开某项的历史模态框并拉取数据。 */
  const openHistory = (settingKey: string) => {
    setHistoryKey(settingKey);
    setHistoryEntries([]);
    setHistoryError(undefined);
    setHistoryLoading(true);
    getSettingHistory(settingKey)
      .then((res) => setHistoryEntries(res.entries))
      .catch((err: unknown) => {
        setHistoryError(err instanceof Error ? err.message : String(err));
      })
      .finally(() => setHistoryLoading(false));
  };

  const dirty = formSignature(form) !== formSignature(savedValues);
  // 概览带口径：展示「已保存 / 生效」值（来自 normalizeForm(initial)），不随未保存的
  // 表单改动跳动——改配置时能一眼对照当前线上口径。
  const effective = normalizeForm(initial);

  /**
   * 单项运行期策略的校验结果。
   *
   * 返回 null 表示可保存；返回文案表示不可保存。边界与后端同源（field.min / field.max），
   * 前端只做「保存前拦明显笔误」，真正的夹取仍在后端。
   */
  const validateRuntimeField = (field: RuntimePolicyField, value: number): string | null => {
    const label = t(runtimePolicyTextKeys(field.key).label);
    if (!Number.isFinite(value)) {
      return t("settings.runtimePolicyInvalidNumber", { label });
    }
    if (value < field.min) {
      return field.min > 0
        ? t("settings.runtimePolicyPositiveRequired", { label })
        : t("settings.runtimePolicyNonNegative", { label });
    }
    if (value > field.max) {
      return t("settings.runtimePolicyTooLarge", { label, max: field.max });
    }
    return null;
  };

  /** 只提交被改动的这一项；后端是部分更新，未传的键保持原值。 */
  const saveRuntimeField = (field: RuntimePolicyField) => {
    const error = validateRuntimeField(field, form[field.key]);
    if (error) {
      notifyError(error);
      return;
    }
    const before = effective[field.key];
    const after = form[field.key];
    if (before === after) {
      notifySuccess(t("settings.runtimePolicyNoChange"));
      return;
    }
    setSavingField(field.key);
    putSettings({ [field.key]: after } as Partial<SettingsConfig>)
      .then((res) => {
        const next = normalizeForm(res);
        const applied = next[field.key];
        // 函数式更新：只覆盖这一项，避免把在途期间用户对其它行的编辑回滚成旧值。
        setForm((prev) => ({ ...prev, [field.key]: applied }));
        setSavedValues((prev) => ({ ...prev, [field.key]: applied }));
        // 单位草稿必须跟着回填值走（R4）：否则后端夹取后输入框仍显示旧数字，
        // 与「将提交 …」那行互相矛盾，且 fieldDirty 变 false 导致无法再修正。
        setUnitDrafts((prev) => ({
          ...prev,
          [field.key]: inferUnitDraft(field, applied),
        }));
        // 后端可能夹取（例如超过上界），回填后与提交值不同就必须如实告知，
        // 否则用户以为「我填的 99999 被采纳了」。
        if (applied !== after) {
          notifySuccess(
            t("settings.runtimePolicyClamped", {
              label: t(runtimePolicyTextKeys(field.key).label),
              value: applied,
            }),
          );
        } else {
          notifySuccess(t("common.saved"));
        }
        onSaved();
      })
      .catch(notifyError)
      .finally(() => setSavingField(null));
  };

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
        setSavedValues(next);
        notifySuccess(t("common.saved"));
        onSaved();
      })
      .catch(notifyError)
      .finally(() => setSaving(false));
  };

  return (
    // 不再自造 maw：宽度口径统一由 AppLayout 内容容器承担（现为铺满可用宽度、不居中）。
    //
    // 关于子项被压扁：本容器是定高可滚动 flex 列（flex-basis: 0），而 flex 子项默认
    // flex-shrink: 1，内容超高时会被**按比例压缩**；卡片自身又是 overflow: hidden，
    // 于是内部字段被裁掉。
    //
    // 修复方式刻意是「各卡片自己加 flexShrink: 0」而不是在容器上写
    // `styles={{ root: { "& > *": ... } }}`：后者会让 Mantine 生成一个 <style> 元素，
    // 而它作为 flex 子项会**被渲染成可见文本**（页面上出现一长串 `--sg-spacing-x:`），
    // 反而制造出一个更显眼的渲染缺陷。
    <Stack gap="md" style={{ flex: 1, minHeight: 0, overflowY: "auto", paddingRight: 4 }}>
      {/* 顶部概览带：把「当前生效值」前置，改表单时能一眼对照已保存的口径。
          数据口径来自 normalizeForm(initial)（已保存/生效值），不随未保存的改动跳动。 */}
      <div style={{ flexShrink: 0 }}>
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
      </div>

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

      {/* 运行期策略：13 项全部铺开，每项独立保存。
          不再做成可折叠分区——折叠让「改哪一项」变成先找再展开的两步操作，
          而这里的字段本就是运维最常反复调整的对象。

          flexShrink: 0 不能省：外层是可滚动 flex 列（flex-basis: 0），卡片默认允许收缩，
          展开后会按剩余空间被压扁成一行多高，而卡片自身 overflow: hidden 把下面的
          字段整块裁掉——表现为「只看到第一行，其余输入框既不可见也点不到」。 */}
      <OpsSection
        title={t("settings.runtimePolicyTitle")}
        meta={t("settings.runtimePolicyHint")}
        style={{ flexShrink: 0 }}
        testId="settings-runtime-policy"
      >
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
                  // 单位草稿：用户选了什么单位就记住，切换字段时不被重新推断覆盖。
                  const draft = unitDrafts[field.key] ?? inferUnitDraft(field, form[field.key]);
                  // 空输入是独立状态，必须单独拦：此时 form 里仍是上一次的值，
                  // 若只按 form 校验会判定「合法可保存」，而用户看到的输入框是空的。
                  const isEmptyInput = draft.value === null;
                  const fieldError = isEmptyInput
                    ? t("settings.runtimePolicyRequired", { label })
                    : validateRuntimeField(field, form[field.key]);
                  // 只在「这一项被改过」时才允许单独保存，避免把未改动的项也提交一遍。
                  const fieldDirty = form[field.key] !== savedValues[field.key];
                  const fieldSaving = savingField === field.key;
                  // 校验文案只在用户完成输入后（失焦/回车）才显示，不在打字途中弹。
                  const showError = touchedFields[field.key] && fieldError !== null;
                  const isDuration = field.dimension === "seconds";
                  const isBytes = field.dimension === "bytes";
                  const unitOptions = isBytes ? BYTE_UNITS : DURATION_UNITS;
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
                      <Group gap="xs" wrap="nowrap" align="flex-start">
                        <NumberInput
                          style={{ flex: 1 }}
                          aria-label={label}
                          description={t(keys.help)}
                          allowDecimal={false}
                          // clampBehavior="none"：默认行为会在键入时就把值夹到区间内，
                          // 于是「填了 99999」会静默变成 3600 且永不报越界——用户以为
                          // 自己填的值被接受，实际被悄悄改写。关掉内部夹取，交给下面的
                          // 校验显式报错，也才谈得上「输入完成即验证是否可行」。
                          clampBehavior="none"
                          // 这里显示的是「所选单位下的数值」，提交前统一换算回基础单位。
                          value={draft.value === null ? "" : draft.value}
                          // 固定单位（小时/天）只显示后缀，不给切换控件。
                          suffix={unitSuffix(t, field, draft)}
                          disabled={fieldSaving}
                          // 错误文案只由底部状态行渲染；这里传 true 仅为无障碍标记
                          // （aria-invalid），不重复显示同一句话。
                          error={showError ? true : undefined}
                          // 绿 / 红描边走 CSS 类而不是内联 styles：
                          // styles 每次渲染新建对象（13 字段 × 每次输入），实测拖慢约 30%
                          // 导致并行门禁超时；且 Mantine 的 error 选择器优先级高于内联样式，
                          // 绿框会被红框覆盖。类名 + !important 才能稳定压过。
                          // 未校验过（没碰过）不加类，保持默认外观。
                          className={
                            touchedFields[field.key]
                              ? fieldError
                                ? "ja-field-invalid"
                                : "ja-field-valid"
                              : undefined
                          }
                          onChange={(value) => {
                            // 空串是 Mantine 在清空时的取值。保持 null 而不是折成 0：
                            // 0 在这些项上等于「禁用作业 / 不限制」，与「还没填」语义相反。
                            const isEmpty = value === "" || value === undefined || value === null;
                            const raw = isEmpty ? null : Number(value);
                            // 输入即校验：一开始打字就把该项标为「已校验」，
                            // 错误文案随每次输入实时更新，不必等失焦才出现。
                            // 保存按钮的禁用条件本就是 fieldError !== null，
                            // 因此「校验不过 → 存不下去」在输入过程中就已生效。
                            setTouchedFields((prev) => ({ ...prev, [field.key]: true }));
                            setUnitDrafts((prev) => ({
                              ...prev,
                              [field.key]: { value: raw, unit: draft.unit },
                            }));
                            // 空态时表单值保持不变（不落 0），保存按钮由校验拦住。
                            if (raw === null || !Number.isFinite(raw)) return;
                            setForm((prev) => ({
                              ...prev,
                              [field.key]: unitToBase(field, raw, draft.unit),
                            }));
                          }}
                          onBlur={() =>
                            setTouchedFields((prev) => ({ ...prev, [field.key]: true }))
                          }
                          onKeyDown={(event) => {
                            // 回车＝确认这一项：先标记已校验，再在可保存时单独提交。
                            if (event.key === "Enter") {
                              setTouchedFields((prev) => ({ ...prev, [field.key]: true }));
                              if (validateRuntimeField(field, form[field.key]) === null) {
                                saveRuntimeField(field);
                              }
                            }
                          }}
                        />
                        {/* 可换单位的项给一个紧凑下拉：86400 秒 vs 1 天，
                            后者才是运维脑子里的那个数。 */}
                        {isDuration || isBytes ? (
                          <Select
                            aria-label={t("settings.runtimePolicyUnit", { label })}
                            data={unitOptions.map((u) => ({
                              value: u.value,
                              label: t(u.labelKey),
                            }))}
                            value={draft.unit}
                            allowDeselect={false}
                            comboboxProps={{ withinPortal: true }}
                            w={92}
                            size="sm"
                            disabled={fieldSaving}
                            onChange={(next) => {
                              if (!next) return;
                              // 换单位只改「怎么显示」，不动已生效的秒/字节值：
                              // 先把当前数值按旧单位折回基础值，再按新单位拆出来显示。
                              // 空态时不做折算（保持空输入，仅换掉单位标注）。
                              if (draft.value === null) {
                                setUnitDrafts((prev) => ({
                                  ...prev,
                                  [field.key]: { value: null, unit: next as never },
                                }));
                                return;
                              }
                              const base = unitToBase(field, draft.value, draft.unit);
                              const replot = replotInUnit(field, base, next);
                              setUnitDrafts((prev) => ({
                                ...prev,
                                [field.key]: { value: replot, unit: next as never },
                              }));
                              setForm({ ...form, [field.key]: base });
                            }}
                          />
                        ) : null}
                        <Button
                          size="compact-sm"
                          variant={fieldDirty ? "light" : "subtle"}
                          loading={fieldSaving}
                          disabled={!fieldDirty || fieldError !== null || savingField !== null}
                          onClick={() => saveRuntimeField(field)}
                          aria-label={t("settings.runtimePolicySaveOne", { label })}
                        >
                          {t("settings.runtimePolicySaveOneShort")}
                        </Button>
                        {/* 历史走 Popover 浮层：锚在按钮上、覆盖在内容之上，
                            不占页面空间也不改变任何既有元素的位置。
                            不用 Modal——看历史通常只是「改之前确认上次是谁改的」，
                            模态框会把整页压暗、逼用户先关掉才能继续编辑。 */}
                        <Popover
                          opened={historyKey === field.settingKey}
                          onChange={(next) => {
                            if (!next) setHistoryKey(null);
                          }}
                          position="bottom-end"
                          width={440}
                          shadow="md"
                          withinPortal
                          trapFocus={false}
                          closeOnClickOutside
                        >
                          <Popover.Target>
                            <Button
                              size="compact-sm"
                              variant="subtle"
                              aria-label={t("settings.runtimePolicyHistoryFor", { label })}
                              onClick={() => openHistory(field.settingKey)}
                            >
                              {t("settings.runtimePolicyHistory")}
                            </Button>
                          </Popover.Target>
                          <Popover.Dropdown>
                            <Stack gap="xs">
                              <Text size="sm" fw={600}>
                                {t("settings.runtimePolicyHistoryTitle", { label })}
                              </Text>
                              <SettingHistoryBody
                                entries={historyEntries}
                                loading={historyLoading}
                                error={historyError}
                                formatValue={(raw) => formatHistoryValue(t, field, raw)}
                              />
                            </Stack>
                          </Popover.Dropdown>
                        </Popover>
                      </Group>
                      {/* 底部状态行：校验不通过时给**具体原因**（红），
                          通过时给「将提交多少」并标为可保存（绿）。
                          两者位置固定，不随状态变化而增删，避免行高跳动。 */}
                      {showError ? (
                        <Text size="xs" c="red" fw={500}>
                          {fieldError}
                        </Text>
                      ) : touchedFields[field.key] ? (
                        <Text size="xs" c="teal" fw={500}>
                          ✓{" "}
                          {isBytes
                            ? t("settings.runtimePolicyValidBytes", {
                                human: formatBytes(form[field.key]),
                              })
                            : isDuration
                              ? t("settings.runtimePolicyValidSeconds", {
                                  seconds: String(form[field.key]),
                                  human: humanizeSeconds(t, form[field.key]),
                                })
                              : t("settings.runtimePolicyValidPlain")}
                        </Text>
                      ) : (
                        // 还没动过这一项：只报静态口径（单位 / 允许范围），不做评价。
                        <Text size="xs" c="dimmed">
                          {isBytes
                            ? t("settings.runtimePolicyCommittedBytes", {
                                bytes: String(form[field.key]),
                                human: formatBytes(form[field.key]),
                              })
                            : isDuration
                              ? t("settings.runtimePolicyCommittedSeconds", {
                                  seconds: String(form[field.key]),
                                  human: humanizeSeconds(t, form[field.key]),
                                })
                              : t("settings.runtimePolicyRangeHint", {
                                  min: field.min,
                                  max: field.max,
                                })}
                        </Text>
                      )}
                    </Stack>
                  );
                })}
              </SimpleGrid>
            </Stack>
          ))}
        </Stack>
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
