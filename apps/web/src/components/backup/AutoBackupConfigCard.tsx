// 自动备份与保留策略配置卡（FR-40）——**只读展示 + 修改途径说明**。
//
// 为什么不给可编辑表单：本批后端没有暴露这四项配置的读写端点。
// 已核实的读取途径与结论：
//   - `GET /api/v1/settings`（SettingsResponse）只有匿名开关 / 对外 URL / 回源超时 /
//     同步间隔 / 域名白名单 / 回源 Token，**不含** backup 三项；
//   - `GET /api/v1/maintenance/jobs` 只暴露已注册作业的 intervalSeconds——间隔为 0
//     时作业根本不注册、清单里就没有这一项，因此它既读不到保留份数 / 天数，
//     也无法可靠表达"自动备份当前是关闭的"；
//   - 这三项来自环境变量，只在启动时读一次（config.go）。
//
// 于是本卡按规格 §3.5 的取舍呈现：如实说明"在哪配、怎么生效"，而不是造一个
// 前端假表单或假装支持热改。**间隔改动需重启生效**这一条必须写在界面上——
// 让人以为保存即生效，比不提供表单更糟。
//
// 一旦后端把配置纳入契约（例如经 settings 通道），把本卡换成受控表单即可，
// 文案里的环境变量名届时仍可作为"等价的运维途径"保留。
import { Code, Group, SimpleGrid, Stack, Text, ThemeIcon } from "@mantine/core";
import { IconAlertTriangle, IconClock, IconDatabase, IconReload } from "@tabler/icons-react";
import { useTranslation } from "react-i18next";

import { OpsSection } from "../ops/OpsKit";

/** 展示用条目：环境变量名 + 默认值。默认值取自 config.go 的常量（默认关闭 / 7 份 / 30 天）。 */
const AUTO_BACKUP_ENV_VARS = [
  { env: "JIAN_BACKUP_AUTO_INTERVAL", defaultKey: "backups.autoCfgDefaultDisabled" },
  { env: "JIAN_BACKUP_KEEP_LAST", defaultKey: "backups.autoCfgDefault7" },
  { env: "JIAN_BACKUP_RETENTION_DAYS", defaultKey: "backups.autoCfgDefault30" },
] as const;

export function AutoBackupConfigCard() {
  const { t } = useTranslation();

  return (
    <OpsSection
      title={t("backups.autoCfgTitle")}
      style={{ flexShrink: 0 }}
      actions={
        <Group gap={6} wrap="nowrap">
          <ThemeIcon size="sm" variant="light" color="orange">
            <IconAlertTriangle size={12} />
          </ThemeIcon>
          <Text size="xs" c="orange">
            {t("backups.autoCfgRestartNote")}
          </Text>
        </Group>
      }
    >
      <Stack gap="sm">
        <Text size="xs" c="dimmed">
          {t("backups.autoCfgHint")}
        </Text>

        <SimpleGrid cols={{ base: 1, sm: 3 }} spacing="sm">
          <ConfigItem
            icon={<IconClock size={14} />}
            env="JIAN_BACKUP_AUTO_INTERVAL"
            label={t("backups.autoCfgInterval")}
            desc={t("backups.autoCfgIntervalDesc")}
          />
          <ConfigItem
            icon={<IconDatabase size={14} />}
            env="JIAN_BACKUP_KEEP_LAST"
            label={t("backups.autoCfgKeepLast")}
            desc={t("backups.autoCfgKeepLastDesc")}
          />
          <ConfigItem
            icon={<IconDatabase size={14} />}
            env="JIAN_BACKUP_RETENTION_DAYS"
            label={t("backups.autoCfgRetentionDays")}
            desc={t("backups.autoCfgRetentionDaysDesc")}
          />
        </SimpleGrid>

        <Group gap={6} wrap="nowrap" align="flex-start">
          <ThemeIcon size="sm" variant="light" color="gray">
            <IconReload size={12} />
          </ThemeIcon>
          <Text size="xs" c="dimmed">
            {t("backups.autoCfgHowTo")}
          </Text>
        </Group>

        <Text size="xs" c="dimmed">
          {t("backups.autoCfgJobsHint")}
        </Text>
      </Stack>
    </OpsSection>
  );
}

/** 单项配置：标签 + 环境变量名 + 语义说明。值本身不可在此读取，故展示变量名而非当前值。 */
function ConfigItem({
  icon,
  env,
  label,
  desc,
}: {
  icon: React.ReactNode;
  env: string;
  label: string;
  desc: string;
}) {
  const { t } = useTranslation();
  const entry = AUTO_BACKUP_ENV_VARS.find((item) => item.env === env);
  return (
    <Stack
      gap={4}
      p="sm"
      style={{
        border: "1px solid var(--mantine-color-default-border)",
        borderRadius: "var(--mantine-radius-md)",
      }}
    >
      <Group gap={6} wrap="nowrap">
        <ThemeIcon size="sm" variant="light" color="gray">
          {icon}
        </ThemeIcon>
        <Text size="sm" fw={500}>
          {label}
        </Text>
      </Group>
      <Code fz="xs">{env}</Code>
      <Text size="xs" c="dimmed">
        {desc}
      </Text>
      {entry ? (
        <Text size="xs" c="dimmed">
          {t("backups.autoCfgDefaultLabel")}
          {t(entry.defaultKey)}
        </Text>
      ) : null}
    </Stack>
  );
}
