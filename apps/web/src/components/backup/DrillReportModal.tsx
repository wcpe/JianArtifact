// 恢复演练报告查看（FR-40）。
//
// 两段式：上半是"发起演练"入口（浅 / 深两级），下半是该包的历史报告列表。
// 报告无论成败都会落库——失败的报告恰恰是最该被看到的，因此这里不隐藏失败项，
// 而是把每条失败检查项连同原因一起摊开，避免"演练失败但说不清哪里坏了"。
//
// 轮询：演练在服务端异步执行（深校验耗时与包体积同阶），受理后本弹窗按固定间隔
// 重拉报告列表，直到出现一条属于本次演练的新报告为止。
import {
  Badge,
  Box,
  Button,
  Card,
  Group,
  Loader,
  Modal,
  ScrollArea,
  Stack,
  Table,
  Text,
} from "@mantine/core";
import { IconPlayerPlay, IconRefresh, IconRun } from "@tabler/icons-react";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useTranslation } from "react-i18next";

import { drillBackup, listBackupVerifications } from "../../api/endpoints";
import type { BackupVerificationLevel, BackupVerificationReport } from "../../api/types";
import { notifyError, notifySuccess } from "../../lib/feedback";
import { formatCount, formatStamp } from "../../lib/format";
import { AsyncBoundary } from "../AsyncBoundary";
import { StatusPill } from "../ops/OpsKit";
import { useAsync } from "../../hooks/useAsync";
import { drillLevelTone, drillVerdictTone } from "./status";

/** 轮询间隔：演练是分钟级任务，1.5s 的列表刷新频率对本弹窗足够了。 */
const POLL_INTERVAL_MS = 1500;

export interface DrillReportModalProps {
  /** 打开的包标识；null 表示关闭。 */
  packageId: string | null;
  onClose: () => void;
}

export function DrillReportModal({ packageId, onClose }: DrillReportModalProps) {
  const { t } = useTranslation();
  const [level, setLevel] = useState<BackupVerificationLevel>("shallow");
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

  // 本次演练是否已出报告：报告结束时间不早于受理时刻即视为本次产物。
  const settled = useMemo(() => {
    if (!acceptedAt) return false;
    const since = Date.parse(acceptedAt);
    return items.some((item) => {
      const finished = Date.parse(item.finishedAt);
      return Number.isNaN(finished) ? false : finished >= since;
    });
  }, [acceptedAt, items]);

  // 受理后按固定间隔刷新，直到新报告出现。停轮询的条件是"报告真的到了"，
  // 而不是固定次数——深校验跑多久取决于包多大，猜测轮询次数只会漏掉大包。
  const settledRef = useRef(settled);
  settledRef.current = settled;
  useEffect(() => {
    if (!acceptedAt || settled) return;
    const timer = window.setInterval(reload, POLL_INTERVAL_MS);
    return () => window.clearInterval(timer);
  }, [acceptedAt, settled, reload]);

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

        <Group gap="xs" wrap="wrap">
          <Button.Group>
            <Button
              size="xs"
              variant={level === "shallow" ? "filled" : "default"}
              leftSection={<IconRun size={14} />}
              onClick={() => setLevel("shallow")}
            >
              {t("backups.drillLevelShallow")}
            </Button>
            <Button
              size="xs"
              variant={level === "deep" ? "filled" : "default"}
              color={drillLevelTone("deep")}
              leftSection={<IconRun size={14} />}
              onClick={() => setLevel("deep")}
            >
              {t("backups.drillLevelDeep")}
            </Button>
          </Button.Group>
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
          {acceptedAt && !settled ? (
            <Group gap={6} wrap="nowrap">
              <Loader size="xs" />
              <Text size="xs" c="dimmed">
                {t("backups.drillRunning")}
              </Text>
            </Group>
          ) : null}
        </Group>

        <Box>
          <Text size="sm" fw={500} mb={6}>
            {t("backups.drillHistory")}
            {state.data ? (
              <Text span size="xs" c="dimmed" ml={6}>
                {formatCount(state.data.total)}
              </Text>
            ) : null}
          </Text>
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
      <Group gap="xs" wrap="wrap" mb={6}>
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
        <Text size="xs" c="red" mb={6}>
          {report.errorSummary}
        </Text>
      ) : null}

      <Table fz="xs" verticalSpacing={2} horizontalSpacing="xs">
        <Table.Thead>
          <Table.Tr>
            <Table.Th w={90}>{t("backups.drillColCheck")}</Table.Th>
            <Table.Th w={64}>{t("backups.drillColResult")}</Table.Th>
            <Table.Th>{t("backups.drillColDetail")}</Table.Th>
            <Table.Th w={70}>{t("backups.drillColDuration")}</Table.Th>
          </Table.Tr>
        </Table.Thead>
        <Table.Tbody>
          {report.checks.map((check) => (
            <Table.Tr key={check.name}>
              <Table.Td>
                <Text size="xs">{t(`backups.drillCheck_${check.name}`, check.name)}</Text>
              </Table.Td>
              <Table.Td>
                <Text size="xs" c={check.ok ? "green" : "red"} fw={500}>
                  {check.ok ? t("backups.drillCheckOk") : t("backups.drillCheckFail")}
                </Text>
              </Table.Td>
              <Table.Td>
                <Text size="xs" c="dimmed">
                  {check.detail}
                </Text>
              </Table.Td>
              <Table.Td>
                <Text size="xs" c="dimmed">
                  {check.durationMs ? `${check.durationMs} ms` : "—"}
                </Text>
              </Table.Td>
            </Table.Tr>
          ))}
        </Table.Tbody>
      </Table>
    </Card>
  );
}
