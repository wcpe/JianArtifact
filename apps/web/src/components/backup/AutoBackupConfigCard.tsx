// 自动备份与保留策略配置卡（FR-40）——**默认折叠，展开后编辑，保存即运行时生效**。
//
// 历史：本卡最初是只读的"配置途径说明"卡，因为当时后端未暴露这三项的读写端点，
// 规格 §3.5 因此写明"间隔改动需重启生效"。后来用户提出"需要允许后台配置"，
// 后端把三项纳入既有 settings 通道（`GET/PUT /api/v1/settings`），作业也改为
// "固定心跳 + 每轮重读配置"，于是本卡改为受控表单，**保存即生效、无需重启**。
//
// 为什么默认折叠（FR-40 追加二）：改成表单后本卡涨到 365px 且 flexShrink:0，
// 而这一页可用高只有 768px——冻结卡(114) + 本卡(365) + 间距(48) 吃掉后，
// 三个 flex:1 区块（备份列表 / 从 URL 导入 / 分片上传）各只剩 80px，
// 备份列表作为本页主体只能显示约一行，用户实测反馈"高度压缩的看不清了"。
// 折叠态只占约 60px，列表恢复到 main 同量级。依据也与 OpsSection 的既有约定一致：
// "说明类内容不放这里：它不该常驻占用卡片高度"。
//
// 三个取值语义（0 都是有意义的取值，不是"未设置"）：
//   - 间隔 0 = 关闭自动备份（作业仍在跑心跳，但每轮跳过）；
//   - 份数 0 = 不按份数限制；
//   - 天数 0 = 不限天数。
// 表单因此**不能**用 `||` 兜底，否则 0 会被悄悄替换成默认值。
//
// 环境变量的角色已变为**首次播种的初值**：仅在 setting 表尚无该组配置时写入，
// 之后以界面配置为准。卡片底部保留这一说明，避免运维以为改 env 仍然生效。
import {
  Alert,
  Box,
  Button,
  Card,
  Collapse,
  Group,
  NumberInput,
  Stack,
  Text,
  ThemeIcon,
} from "@mantine/core";
import {
  IconAlertTriangle,
  IconCheck,
  IconChevronDown,
  IconChevronUp,
  IconClock,
  IconDatabase,
} from "@tabler/icons-react";
import { useCallback, useEffect, useState } from "react";
import { useTranslation } from "react-i18next";

import { getSettings, putSettings } from "../../api/endpoints";
import { notifyError, notifySuccess } from "../../lib/feedback";
import { AsyncBoundary } from "../AsyncBoundary";
import { OpsSection } from "../ops/OpsKit";
import { useAsync } from "../../hooks/useAsync";

/** 表单里三项的取值上限，与后端 settings_handlers.go 的校验保持一致。 */
const LIMITS = {
  autoInterval: 30 * 24 * 3600, // 30 天
  keepLast: 1000,
  retentionDays: 3650, // 10 年
} as const;

/** 表单草稿：三项均为字符串以便区分"空输入"与 0（NumberInput 允许清空）。 */
interface ConfigDraft {
  autoInterval: number | string;
  keepLast: number | string;
  retentionDays: number | string;
}

export function AutoBackupConfigCard() {
  const { t } = useTranslation();
  const state = useAsync(() => getSettings(), [], { cacheKey: "backup:auto-config" });
  const [draft, setDraft] = useState<ConfigDraft | null>(null);
  const [saving, setSaving] = useState(false);
  /** 折叠态：默认收起，只留标题 + 当前值摘要 + 展开按钮。 */
  const [expanded, setExpanded] = useState(false);

  // 读到配置后用服务端值初始化草稿；reload 后也要重新对齐，避免显示陈旧值。
  useEffect(() => {
    if (!state.data) return;
    setDraft({
      autoInterval: state.data.backupAutoInterval,
      keepLast: state.data.backupKeepLast,
      retentionDays: state.data.backupRetentionDays,
    });
  }, [state.data]);

  const reload = state.reload;
  const onSave = useCallback(async () => {
    if (!draft) return;
    // 空输入按 0 提交（等于关闭 / 不限制），而不是拒绝保存：
    // 用户清空输入框的意图就是"不要这个限制"，报错反而妨碍操作。
    const toInt = (v: number | string): number => {
      if (v === "" || v === undefined || v === null) return 0;
      const n = typeof v === "number" ? v : Number(v);
      return Number.isFinite(n) ? Math.trunc(n) : 0;
    };
    setSaving(true);
    try {
      await putSettings({
        backupAutoInterval: toInt(draft.autoInterval),
        backupKeepLast: toInt(draft.keepLast),
        backupRetentionDays: toInt(draft.retentionDays),
      });
      notifySuccess(t("backups.autoCfgSaved"));
      reload();
    } catch (err) {
      notifyError(err);
    } finally {
      setSaving(false);
    }
  }, [draft, reload, t]);

  const dirty =
    !!draft &&
    !!state.data &&
    (Number(draft.autoInterval || 0) !== state.data.backupAutoInterval ||
      Number(draft.keepLast || 0) !== state.data.backupKeepLast ||
      Number(draft.retentionDays || 0) !== state.data.backupRetentionDays);

  // 折叠态摘要：必须先说清"自动备份当前是不是在跑"。间隔为 0 时显示"已关闭"，
  // 否则用户看到「间隔 0 秒」会以为它在按 0 秒跑；也不能只显示 0 而省略语义。
  const summary = (() => {
    if (!state.data) return null;
    const { backupAutoInterval, backupKeepLast, backupRetentionDays } = state.data;
    const intervalPart =
      backupAutoInterval > 0
        ? t("backups.autoCfgSummaryInterval", { n: backupAutoInterval })
        : t("backups.autoCfgSummaryOff");
    const keepPart =
      backupKeepLast > 0
        ? t("backups.autoCfgSummaryKeep", { n: backupKeepLast })
        : t("backups.autoCfgSummaryKeepUnlimited");
    const daysPart =
      backupRetentionDays > 0
        ? t("backups.autoCfgSummaryDays", { n: backupRetentionDays })
        : t("backups.autoCfgSummaryDaysUnlimited");
    return `${intervalPart} · ${keepPart} · ${daysPart}`;
  })();

  return (
    <OpsSection
      title={t("backups.autoCfgTitle")}
      style={{ flexShrink: 0 }}
      actions={
        <Group gap={6} wrap="nowrap">
          <ThemeIcon size="sm" variant="light" color="teal">
            <IconCheck size={12} />
          </ThemeIcon>
          <Text size="xs" c="teal" visibleFrom="sm">
            {t("backups.autoCfgLiveNote")}
          </Text>
        </Group>
      }
    >
      <Stack gap="xs">
        {/* 折叠态的常驻行：当前值摘要 + 展开开关。摘要在加载中/失败时留占位而非空白，
            否则用户会以为"没有配置"而不是"还没读到"。 */}
        <Group gap="xs" justify="space-between" wrap="nowrap">
          <Text size="xs" c="dimmed" truncate>
            {summary ?? t("backups.autoCfgSummaryLoading")}
          </Text>
          <Button
            size="compact-xs"
            variant={expanded ? "light" : "default"}
            rightSection={expanded ? <IconChevronUp size={14} /> : <IconChevronDown size={14} />}
            onClick={() => setExpanded((v) => !v)}
            aria-expanded={expanded}
          >
            {expanded ? t("backups.autoCfgCollapse") : t("backups.autoCfgEdit")}
          </Button>
        </Group>

        <Collapse expanded={expanded}>
          <Box pt="xs">
            <AsyncBoundary state={state}>
              {() =>
                draft ? (
                  <Stack gap="sm">
                    <Text size="xs" c="dimmed">
                      {t("backups.autoCfgHint")}
                    </Text>

                    <Group gap="sm" align="flex-start" wrap="wrap">
                      <ConfigField
                        icon={<IconClock size={14} />}
                        label={t("backups.autoCfgInterval")}
                        desc={t("backups.autoCfgIntervalDesc")}
                        value={draft.autoInterval}
                        onChange={(v) => setDraft({ ...draft, autoInterval: v })}
                        max={LIMITS.autoInterval}
                        testId="auto-interval"
                      />
                      <ConfigField
                        icon={<IconDatabase size={14} />}
                        label={t("backups.autoCfgKeepLast")}
                        desc={t("backups.autoCfgKeepLastDesc")}
                        value={draft.keepLast}
                        onChange={(v) => setDraft({ ...draft, keepLast: v })}
                        max={LIMITS.keepLast}
                        testId="keep-last"
                      />
                      <ConfigField
                        icon={<IconDatabase size={14} />}
                        label={t("backups.autoCfgRetentionDays")}
                        desc={t("backups.autoCfgRetentionDaysDesc")}
                        value={draft.retentionDays}
                        onChange={(v) => setDraft({ ...draft, retentionDays: v })}
                        max={LIMITS.retentionDays}
                        testId="retention-days"
                      />
                    </Group>

                    <Group gap="xs" justify="space-between" wrap="wrap">
                      <Text size="xs" c="dimmed">
                        {t("backups.autoCfgJobsHint")}
                      </Text>
                      <Button
                        size="xs"
                        loading={saving}
                        disabled={!dirty}
                        leftSection={<IconCheck size={14} />}
                        onClick={() => void onSave()}
                      >
                        {t("backups.autoCfgSave")}
                      </Button>
                    </Group>

                    <Alert
                      color="gray"
                      variant="light"
                      icon={
                        <ThemeIcon size="sm" variant="light" color="gray">
                          <IconAlertTriangle size={12} />
                        </ThemeIcon>
                      }
                    >
                      <Text size="xs" c="dimmed">
                        {t("backups.autoCfgEnvNote")}
                      </Text>
                    </Alert>
                  </Stack>
                ) : null
              }
            </AsyncBoundary>
          </Box>
        </Collapse>
      </Stack>
    </OpsSection>
  );
}

/** 单项配置输入：图标 + 标签 + 说明 + 数值输入。 */
function ConfigField({
  icon,
  label,
  desc,
  value,
  onChange,
  max,
  testId,
}: {
  icon: React.ReactNode;
  label: string;
  desc: string;
  value: number | string;
  onChange: (v: number | string) => void;
  max: number;
  testId: string;
}) {
  return (
    <Card withBorder radius="md" padding="sm" style={{ flex: "1 1 220px", minWidth: 200 }}>
      <Stack gap={4}>
        <Group gap={6} wrap="nowrap">
          <ThemeIcon size="sm" variant="light" color="gray">
            {icon}
          </ThemeIcon>
          <Text size="sm" fw={500}>
            {label}
          </Text>
        </Group>
        <NumberInput
          size="xs"
          min={0}
          max={max}
          allowDecimal={false}
          allowNegative={false}
          value={value}
          onChange={onChange}
          data-testid={testId}
          clampBehavior="strict"
        />
        <Text size="xs" c="dimmed">
          {desc}
        </Text>
      </Stack>
    </Card>
  );
}
