// 关键事实条：页面第三段只回答「我的备份可靠吗」——四个数字一次说完。
//
// 每个取值的来源与被刻意排除的东西（决定这块可不可信的地方）：
//
//   最近成功备份  真的：从备份包列表里取最近一个 done 的 createdAt。
//   下次自动备份  真的，但**不是**从 maintenance job 的 intervalSeconds 推——`backup-auto`
//                 注册的是固定心跳（见 apps/server/cmd/jianartifact/auto_backup_job.go），
//                 每轮重读配置判断是否到点，因此 intervalSeconds 是心跳粒度而非用户的备份间隔，
//                 拿它算"下次"会算出一个错的时刻。这里改用真正生效的两个输入：
//                 用户配置的间隔（settings.backupAutoInterval）+ 最近一次生成的时刻。
//                 配置为 0 时如实显示「已关闭」——这正是 0 的语义，不能显示成"每 0 秒"。
//   最近校验      真的：对最近一个已完成包读它的落库演练报告。
//                 没有报告时如实显示「尚未校验」，而不是借 /verify 的即时结论冒充历史。
//   占用          真的：把当前列表内已完成包的 sizeBytes 求和，并同时给出包数。
//                 刻意**不写成**"磁盘占用"——没有全局用量端点，这只是这一页能看到的那些包。
import { Group, Paper, Stack, Text, ThemeIcon } from "@mantine/core";
import {
  IconArchive,
  IconBolt,
  IconClock,
  IconRefresh,
  IconShieldCheck,
} from "@tabler/icons-react";
import type { ReactNode } from "react";
import { useTranslation } from "react-i18next";

import type { BackupPackage, BackupVerificationReport } from "../../api/types";
import { formatBytes, formatStamp } from "../../lib/format";

export interface FactsStripProps {
  items: BackupPackage[];
  /** 自动备份间隔（秒）；0 = 已关闭。 */
  autoIntervalSeconds: number | null;
  /**
   * 自动备份与保留策略的完整摘要（含三个 0 的语义），来自 useAutoBackupSummary。
   * 读不到配置时为 null。
   */
  autoSummary?: string | null;
  /** 最近一次演练报告（可为 null：还没演练过）。 */
  lastVerification: BackupVerificationReport | null;
  /** 校验报告是否仍在读取中。 */
  verificationLoading?: boolean;
}

interface Fact {
  key: string;
  icon: ReactNode;
  label: string;
  value: string;
  tone?: string;
}

export function FactsStrip({
  items,
  autoIntervalSeconds,
  autoSummary,
  lastVerification,
  verificationLoading,
}: FactsStripProps) {
  const { t } = useTranslation();

  const doneItems = items.filter((it) => it.status === "done");
  // 最近成功备份：done 里 createdAt 最大的一条。列表按创建时间倒序返回，
  // 但这里不依赖该约定——显式比较，避免后端改排序就悄悄取错。
  const lastSuccess = doneItems.reduce<BackupPackage | null>((best, it) => {
    if (!best) return it;
    return it.createdAt > best.createdAt ? it : best;
  }, null);

  const totalBytes = doneItems.reduce((sum, it) => sum + (it.sizeBytes ?? 0), 0);

  // 下次自动备份：用**自动包的最近一次生成时刻** + 用户配置的间隔推算。
  // 必须只看 origin==="auto"——手动打的包不计入自动周期，否则手动备份一次就会把
  // "下次自动备份"错误地推后一个间隔。
  const lastAuto = doneItems
    .filter((it) => it.origin === "auto")
    .reduce<BackupPackage | null>(
      (best, it) => (!best || it.createdAt > best.createdAt ? it : best),
      null,
    );

  let nextAuto = t("backups.factNoData");
  if (autoIntervalSeconds === 0) {
    nextAuto = t("backups.factAutoOff");
  } else if (autoIntervalSeconds && autoIntervalSeconds > 0) {
    const lastMs = lastAuto ? Date.parse(lastAuto.createdAt) : Number.NaN;
    if (Number.isFinite(lastMs)) {
      const next = new Date(lastMs + autoIntervalSeconds * 1000);
      // 已过期说明这一轮还没跑到（心跳粒度下最多晚一个心跳），如实显示"即将执行"。
      // 不能用 factNextPending（"等待首次执行"）——那时实例上已经有自动包了，
      // 说"等待首次执行"是假话，两者必须分开（实测踩到：30s 间隔下几乎总是走这个分支）。
      nextAuto =
        next.getTime() <= Date.now() ? t("backups.factNextDue") : formatStamp(next.toISOString());
    } else {
      // 开着重置但还没有自动包：无从推算具体时刻，如实说"等待首次执行"。
      nextAuto = t("backups.factNextPending");
    }
  }

  let verifyText = t("backups.factVerifyNone");
  if (verificationLoading) {
    verifyText = "…";
  } else if (lastVerification) {
    const when = lastVerification.finishedAt ?? lastVerification.startedAt;
    const verdict = lastVerification.ok
      ? t("backups.factVerifyPassed")
      : t("backups.factVerifyFailed");
    verifyText = `${verdict} · ${formatStamp(when)}`;
  }

  const facts: Fact[] = [
    {
      key: "lastSuccess",
      icon: <IconArchive size={15} />,
      label: t("backups.factLastSuccess"),
      value: lastSuccess ? formatStamp(lastSuccess.createdAt) : t("backups.factNoData"),
    },
    {
      key: "nextAuto",
      icon: <IconBolt size={15} />,
      label: t("backups.factNextAuto"),
      value: nextAuto,
    },
    {
      key: "lastVerify",
      icon: <IconShieldCheck size={15} />,
      label: t("backups.factLastVerify"),
      value: verifyText,
      tone:
        lastVerification && !lastVerification.ok
          ? "var(--mantine-color-red-7)"
          : lastVerification
            ? "var(--mantine-color-green-7)"
            : undefined,
    },
    {
      key: "usage",
      icon: <IconClock size={15} />,
      label: t("backups.factUsage"),
      value: t("backups.factUsageValue", {
        size: formatBytes(totalBytes),
        count: doneItems.length,
      }),
    },
    {
      // 保留策略摘要：从已删除的常驻状态条迁到这里，信息一条没少。
      // 三个 0 的语义由 useAutoBackupSummary 统一处理（0 = 关闭 / 不限制），
      // 这里只负责展示，避免在组件里重写一遍 0 的判断。
      key: "autoPolicy",
      icon: <IconRefresh size={15} />,
      label: t("backups.factAutoPolicy"),
      value: autoSummary ?? t("backups.factNoData"),
    },
  ];

  return (
    <Paper withBorder radius="md" p="sm" data-testid="backup-facts">
      <Group gap={0} wrap="wrap" align="flex-start">
        {facts.map((f) => (
          // flex 基准 150px：窄视口下自动换行成两列/一列，不挤压也不截断。
          <Stack key={f.key} gap={2} px="sm" style={{ flex: "1 1 150px", minWidth: 0 }}>
            <Group gap={5} wrap="nowrap">
              <ThemeIcon variant="transparent" size={16} c="dimmed">
                {f.icon}
              </ThemeIcon>
              <Text size="xs" c="dimmed" fw={600} truncate>
                {f.label}
              </Text>
            </Group>
            <Text size="sm" fw={700} truncate title={f.value} c={f.tone}>
              {f.value}
            </Text>
          </Stack>
        ))}
      </Group>
    </Paper>
  );
}
