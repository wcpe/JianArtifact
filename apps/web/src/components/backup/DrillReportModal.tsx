// 恢复演练报告查看。
//
// 两段式：上半是"发起演练"入口（浅 / 深两级），下半是该包的历史报告列表。
// 报告无论成败都会落库——失败的报告恰恰是最该被看到的，因此这里不隐藏失败项，
// 而是把每条失败检查项连同原因一起摊开，避免"演练失败但说不清哪里坏了"。
//
// 轮询：演练在服务端异步执行（深校验耗时与包体积同阶），受理后本弹窗按固定间隔
// 重拉报告列表，直到出现一条属于本次演练的新报告为止。
//
// 陈旧包：保留策略（KEEP_LAST 等）会把超限的包淘汰掉，此时被打开的列表行已经过期，
// 报告接口返回 404。这种情况给出"刷新列表"的可操作提示，而不是通用错误态——详见
// 下方 packageMissing 处的说明。
import {
  Alert,
  Badge,
  Box,
  Button,
  Card,
  Group,
  Loader,
  Modal,
  Radio,
  ScrollArea,
  Stack,
  Text,
  ThemeIcon,
} from "@mantine/core";
import {
  IconAlertTriangle,
  IconCheck,
  IconPlayerPlay,
  IconRefresh,
  IconX,
} from "@tabler/icons-react";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useTranslation } from "react-i18next";

import { drillBackup, listBackupVerifications } from "../../api/endpoints";
import type {
  BackupVerificationLevel,
  BackupVerificationReport,
  BackupDrillCheck,
} from "../../api/types";
import { notifyError, notifySuccess } from "../../lib/feedback";
import { formatCount, formatStamp } from "../../lib/format";
import { AsyncBoundary } from "../AsyncBoundary";
import { StatusPill } from "../ops/OpsKit";
import { useAsync } from "../../hooks/useAsync";
import { drillLevelTone, drillVerdictTone } from "./status";

/** 轮询间隔：演练是分钟级任务，1.5s 的列表刷新频率对本弹窗足够了。 */
const POLL_INTERVAL_MS = 1500;

/**
 * 轮询超时上限：到点必须停，且必须**说出来**。
 *
 * 为什么需要兜底：停轮询的常规条件是"报告到了"，但还有一类情况报告永远不会到——
 * 包在演练途中被保留策略淘汰（后端 404）、后端崩溃、任务被吞掉。那时若只靠
 * "报告到了"这一个条件，弹窗会**永久**显示"演练进行中"，用户既不知道出了什么事，
 * 也不知道还能做什么（实测收到的反馈正是如此）。
 *
 * 上限取 10 分钟：深校验耗时与包体积同阶，几百 MB 的包也可能跑上分钟级，
 * 故必须给足余量；而它只用于兜住"永远不会到"的情况，不参与正常路径。
 */
export const DRILL_POLL_TIMEOUT_MS = 10 * 60 * 1000;

export interface DrillReportModalProps {
  /** 打开的包标识；null 表示关闭。 */
  packageId: string | null;
  onClose: () => void;
  /** 刷新外层备份列表（404 场景下该行已陈旧，刷新后它会消失）。 */
  onRefresh?: () => void;
}

export function DrillReportModal({ packageId, onClose, onRefresh }: DrillReportModalProps) {
  const { t } = useTranslation();
  // 默认深校验：浅校验对「内容被静默篡改但结构完好」的包会判为通过（见下方选项说明），
  // 而演练的目的恰恰是回答"这份包真的能恢复吗"——漏判一次就等于白演练。
  // 代价是耗时与包体积同阶，但演练本就是低频动作，取严格是更安全的默认。
  const [level, setLevel] = useState<BackupVerificationLevel>("deep");
  const [starting, setStarting] = useState(false);
  /** 本次演练的受理时刻：用来判断"哪条报告才算这次的结果"。 */
  const [acceptedAt, setAcceptedAt] = useState<string | null>(null);

  const state = useAsync(
    () =>
      packageId
        ? listBackupVerifications(packageId, { page: 1, page_size: 20 })
        : Promise.resolve({ items: [], total: 0 }),
    [packageId],
    { cacheKey: `backups:verifications:${packageId ?? "none"}` },
  );
  const reload = state.reload;
  const items = useMemo(() => state.data?.items ?? [], [state.data]);

  // 该包已被保留策略淘汰（后端 404）。单独识别它，是因为这个 404 不是"接口坏了"，
  // 而是"这一行已经陈旧"——重试永远不会成功，唯一出路是刷新外层列表让该行消失。
  //
  // **两个 error 都要看**（实测漏报）：`useAsync` 在"已有旧数据在场"时把刷新失败放进
  // `refreshError` 而非 `error`。而演练的典型场景恰恰是——打开弹窗时列表已加载（旧数据在场），
  // 轮询期间包才被淘汰，于是 404 落在 `refreshError` 上。只看 `state.error` 的话，
  // 这个提示根本不会出现，用户只看到永久的"演练进行中"。
  const packageMissing = state.error?.status === 404 || state.refreshError?.status === 404;

  /**
   * 陈旧行的处置：先刷新外层备份列表，再关闭弹窗。
   *
   * 先刷新后关闭，是因为用户真正需要看的是刷新后的列表（那条被淘汰的包会随之消失），
   * 而不是继续停在一个对已不存在的包做演练的弹窗里。
   */
  const handleRefreshAndClose = useCallback(() => {
    onRefresh?.();
    onClose();
  }, [onRefresh, onClose]);

  // 本次演练是否已出报告：报告结束时间不早于受理时刻即视为本次产物。
  const settled = useMemo(() => {
    if (!acceptedAt) return false;
    const since = Date.parse(acceptedAt);
    return items.some((item) => {
      const finished = Date.parse(item.finishedAt);
      return Number.isNaN(finished) ? false : finished >= since;
    });
  }, [acceptedAt, items]);

  /**
   * 轮询超时：到点必须停，且必须**说出来**。理由见文件头的 DRILL_POLL_TIMEOUT_MS。
   */
  const [pollTimedOut, setPollTimedOut] = useState(false);

  useEffect(() => {
    if (!acceptedAt || settled) {
      setPollTimedOut(false);
      return;
    }
    const timer = window.setTimeout(() => setPollTimedOut(true), DRILL_POLL_TIMEOUT_MS);
    return () => window.clearTimeout(timer);
  }, [acceptedAt, settled]);

  // 受理后按固定间隔刷新，直到新报告出现（或超时 / 包已消失）。
  // 停轮询的条件是"报告真的到了"，而不是固定次数——深校验跑多久取决于包多大，
  // 猜测轮询次数只会漏掉大包。
  const settledRef = useRef(settled);
  settledRef.current = settled;
  useEffect(() => {
    // 包已不在（404）时继续轮询毫无意义：重试永远不会成功。
    if (!acceptedAt || settled || packageMissing || pollTimedOut) return;
    const timer = window.setInterval(reload, POLL_INTERVAL_MS);
    return () => window.clearInterval(timer);
  }, [acceptedAt, settled, packageMissing, pollTimedOut, reload]);

  const onStart = useCallback(async () => {
    if (!packageId) return;
    setStarting(true);
    try {
      const accepted = await drillBackup(packageId, level);
      setAcceptedAt(accepted.acceptedAt);
      notifySuccess(t("backups.drillStarted"));
      reload();
    } catch (err) {
      notifyError(err);
    } finally {
      setStarting(false);
    }
  }, [level, packageId, reload, t]);

  return (
    <Modal
      opened={packageId !== null}
      onClose={onClose}
      title={t("backups.drillTitle")}
      size="xl"
      centered
    >
      <Stack gap="md">
        <Text size="xs" c="dimmed">
          {t("backups.drillHint")}
        </Text>

        {/* 两级校验的差异必须写清楚：正常包跑两级结果一模一样（内容没问题时，
            "没验内容"和"验过内容"的结论当然相同），只有 blob 被静默篡改、
            而 tar/gzip 结构仍完好时，浅校验才会漏判。不写明白，用户只会
            看到两次相同的报告，以为深校验是多余的。 */}
        <Radio.Group value={level} onChange={(value) => setLevel(value as BackupVerificationLevel)}>
          <Stack gap="xs">
            <Radio.Card value="shallow" radius="md" p="sm" withBorder>
              <Group wrap="nowrap" align="flex-start">
                <Radio.Indicator mt={2} />
                <Stack gap={2}>
                  <Text size="sm" fw={500}>
                    {t("backups.drillShallowTitle")}
                  </Text>
                  <Text size="xs" c="dimmed">
                    {t("backups.drillShallowDesc")}
                  </Text>
                </Stack>
              </Group>
            </Radio.Card>
            <Radio.Card value="deep" radius="md" p="sm" withBorder>
              <Group wrap="nowrap" align="flex-start">
                <Radio.Indicator mt={2} />
                <Stack gap={2}>
                  <Group gap={6} wrap="nowrap">
                    <Text size="sm" fw={500}>
                      {t("backups.drillDeepTitle")}
                    </Text>
                    <Badge color={drillLevelTone("deep")} variant="light" tt="none" size="sm">
                      {t("backups.drillRecommended")}
                    </Badge>
                  </Group>
                  <Text size="xs" c="dimmed">
                    {t("backups.drillDeepDesc")}
                  </Text>
                </Stack>
              </Group>
            </Radio.Card>
          </Stack>
        </Radio.Group>

        <Group gap="xs" wrap="wrap">
          <Button
            size="xs"
            leftSection={<IconPlayerPlay size={14} />}
            loading={starting}
            disabled={packageId === null}
            onClick={() => void onStart()}
          >
            {t("backups.drillStart")}
          </Button>
          <Button
            size="xs"
            variant="subtle"
            leftSection={<IconRefresh size={14} />}
            onClick={reload}
          >
            {t("backups.refresh")}
          </Button>
          {/* 演练进行中的三种收尾都必须可见，不能只留一个转圈的 Loader：
              · 正常出报告 → settled，什么都不显示（下面已渲染报告卡片）
              · 包在途中被淘汰 → packageMissing，转圈必须消失（否则就是"永久卡住"）
              · 超时未出报告 → pollTimedOut，如实说明并让用户能刷新 */}
          {acceptedAt && !settled && !packageMissing && !pollTimedOut ? (
            <Group gap={6} wrap="nowrap">
              <Loader size="xs" />
              <Text size="xs" c="dimmed">
                {t("backups.drillRunning")}
              </Text>
            </Group>
          ) : null}
          {pollTimedOut && !settled && !packageMissing ? (
            <Group gap={6} wrap="nowrap">
              <ThemeIcon size={16} radius="xl" variant="light" color="yellow">
                <IconAlertTriangle size={11} />
              </ThemeIcon>
              <Text size="xs" c="dimmed">
                {t("backups.drillTimedOut")}
              </Text>
            </Group>
          ) : null}
        </Group>

        <Box>
          <Text size="sm" fw={500} mb={6}>
            {t("backups.drillHistory")}
            {/* 报告数只在**包还在**时显示。
                实机踩到：包被保留策略淘汰后，`useAsync` 会保留上一次的数据（它刻意
                "刷新失败不丢旧数据"），于是这里渲染出旧报告数，而下方的 packageMissing
                同时说"包已被删除"——同屏一句"历史报告 1"一句"包没了"，自相矛盾，
                用户无从判断哪个是真的。包缺失时它必然是过时数据，故一并隐去。 */}
            {state.data && !packageMissing ? (
              <Text span size="xs" c="dimmed" ml={6}>
                {formatCount(state.data.total)}
              </Text>
            ) : null}
          </Text>
          {/* 404 单独渲染可操作提示，不走 AsyncBoundary 的通用 ErrorState。
              通用 ErrorState 的出路是"重试"，但包已被保留策略删除时重试永远失败，
              用户会陷在"点重试 → 还是同一句报错"的死循环里，且无从知道该刷新列表。
              这里给的是唯一有效的动作：刷新外层列表（顺带关掉这个已无意义的弹窗）。
              其它错误（500 / 网络失败等）仍是可恢复的，继续交给 AsyncBoundary，
              保留它自带的重试按钮。 */}
          {packageMissing ? (
            // 刻意不给 Alert 加 title：弹窗标题已经是「恢复演练」，再用同一个键当标题
            // 会在同屏重复同一句话；而另起一句新标题需要新增 i18n 键，超出本次改动范围。
            // 正文本身已把原因和出路说清，标题只会是噪音。
            <Alert color="yellow" variant="light" icon={<IconAlertTriangle size={16} />}>
              <Stack gap="xs">
                <Text size="sm">{t("backups.drillPackageMissing")}</Text>
                {/* 若本次演练正跑到一半（受理了但还没出报告），必须点明它已被中断。
                    否则用户刚点完"发起演练"就看到这句，会以为是自己点错了——
                    实际是包在演练途中被保留策略淘汰了（短间隔自动备份下很容易发生）。 */}
                {acceptedAt && !settled ? (
                  <Text size="sm">{t("backups.drillInterrupted")}</Text>
                ) : null}
                <Button
                  size="xs"
                  variant="light"
                  color="yellow"
                  w="fit-content"
                  leftSection={<IconRefresh size={14} />}
                  onClick={handleRefreshAndClose}
                >
                  {t("common.refresh")}
                </Button>
              </Stack>
            </Alert>
          ) : (
            <AsyncBoundary state={state}>
              {(data) =>
                data.items.length === 0 ? (
                  <Text size="xs" c="dimmed">
                    {t("backups.drillNoReports")}
                  </Text>
                ) : (
                  <ScrollArea.Autosize mah={360}>
                    <Stack gap="sm">
                      {data.items.map((report) => (
                        <DrillReportCard key={report.id} report={report} />
                      ))}
                    </Stack>
                  </ScrollArea.Autosize>
                )
              }
            </AsyncBoundary>
          )}
        </Box>
      </Stack>
    </Modal>
  );
}

/** 单份报告的卡片：总判定 + 耗时 + 逐项结论（失败项一并展示，不粉饰）。 */
function DrillReportCard({ report }: { report: BackupVerificationReport }) {
  const { t } = useTranslation();
  return (
    <Card withBorder radius="md" padding="sm">
      <Group gap="xs" wrap="wrap" mb={report.checks.length > 0 ? "xs" : 0}>
        <StatusPill
          tone={drillVerdictTone(report.ok)}
          text={report.ok ? t("backups.drillPassed") : t("backups.drillFailed")}
        />
        <Badge color={drillLevelTone(report.level)} variant="light" tt="none">
          {t(`backups.drillLevel_${report.level}`)}
        </Badge>
        <Text size="xs" c="dimmed">
          {formatStamp(report.finishedAt)} · {report.durationMs} ms
        </Text>
      </Group>

      {report.errorSummary ? (
        <Text size="xs" c="red" mb={report.checks.length > 0 ? "xs" : 0}>
          {report.errorSummary}
        </Text>
      ) : null}

      <Stack gap={0}>
        {report.checks.map((check, index) => (
          <CheckRow key={check.name} check={check} first={index === 0} />
        ))}
      </Stack>
    </Card>
  );
}

/**
 * 单条检查结论：一行"状态 + 检查项 + 耗时"，说明作为次行缩进展示。
 *
 * 不用表格：四条检查项的宽度差异极大（"解包"两字 vs "SQLite 一致性"），表格会把
 * 列宽固定成最宽项的需求，挤窄说明列后长说明折成多行、行高在 21–38px 之间跳动。
 * 明细清单让每项自己占一段，说明再长也只是自己换行，不影响邻项。
 */
function CheckRow({ check, first }: { check: BackupDrillCheck; first: boolean }) {
  const { t } = useTranslation();
  const icon = check.ok ? (
    <ThemeIcon size={16} radius="xl" variant="light" color="green">
      <IconCheck size={11} />
    </ThemeIcon>
  ) : (
    <ThemeIcon size={16} radius="xl" variant="light" color="red">
      <IconX size={11} />
    </ThemeIcon>
  );
  return (
    <Box pt={first ? 0 : "xs"} mt={first ? 0 : "xs"} style={first ? undefined : rowDividerStyle}>
      <Group gap="xs" wrap="nowrap" justify="space-between">
        <Group gap={6} wrap="nowrap">
          {icon}
          <Text size="xs" fw={500}>
            {t(`backups.drillCheck_${check.name}`, check.name)}
          </Text>
        </Group>
        <Text size="xs" c="dimmed">
          {check.durationMs ? `${check.durationMs} ms` : "—"}
        </Text>
      </Group>
      <Text size="xs" c="dimmed" pl={22} mt={2}>
        {check.detail}
      </Text>
    </Box>
  );
}

/** 检查项之间的分隔线：用主题变量，不自绘颜色。 */
const rowDividerStyle = {
  borderTop: "1px solid var(--mantine-color-default-border)",
} as const;
