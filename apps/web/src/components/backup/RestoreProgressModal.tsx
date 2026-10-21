// 恢复进度弹窗（FR-40 追加七）。
//
// 为什么需要它：恢复受理后**只弹了一个 toast**（"已受理，正在校验与暂存；完成后需重启服务生效"），
// 此后再无任何反馈——用户不知道它在跑、跑到哪、还要多久，也不知道失败没有。
// 实测反馈「只会提示已受理…没有进度么？」正是指此。
//
// 数据是现成的：`BackupImport` 记录本就带 status / fetchedBytes / totalBytes / blobCount，
// 且状态机会推进（queued → fetching → staging → pending_restart），四条恢复通道
//（URL / 上传 / 本机包 / CLI）共用同一种记录。此前只是**没有把它显示出来**。
//
// 本弹窗同时承担收尾引导：到达 pending_restart 后给出「下一步该做什么」——
// 建议冻结（避免重启期间新写入丢失）与重启入口。这是恢复流程真正的最后一步，
// 不能只留用户对着一条"待重启"的记录发愣。
import {
  Alert,
  Badge,
  Button,
  Code,
  Group,
  Loader,
  Modal,
  Progress,
  Stack,
  Text,
} from "@mantine/core";
import { IconAlertTriangle, IconCheck, IconRefresh, IconSnowflake } from "@tabler/icons-react";
import { useEffect, useMemo } from "react";
import { useTranslation } from "react-i18next";

import { getBackupImport } from "../../api/endpoints";
import type { BackupImport } from "../../api/types";
import { AsyncBoundary } from "../AsyncBoundary";
import { StatusPill } from "../ops/OpsKit";
import { useAsync } from "../../hooks/useAsync";
import { importInProgress, importStatusTone } from "./status";

/** 轮询间隔：暂存是分钟级动作，1.5s 足以让阶段推进看起来连续。 */
const POLL_INTERVAL_MS = 1500;

export interface RestoreProgressModalProps {
  /** 待观察的导入记录标识；null 表示关闭。 */
  importId: string | null;
  onClose: () => void;
  /** 到达终态时通知外层刷新列表（事件表里那条记录的状态已变）。 */
  onSettled?: () => void;
  /** 「立即重启服务」入口；不传则不显示该按钮（例如能力探测不可用）。 */
  restartSlot?: React.ReactNode;
  /** 打开冻结弹窗的入口（恢复完成后建议解冻，这里也提示）。 */
  onFreeze?: () => void;
  /** 当前是否已冻结。 */
  frozen?: boolean;
}

export function RestoreProgressModal({
  importId,
  onClose,
  onSettled,
  restartSlot,
  onFreeze,
  frozen,
}: RestoreProgressModalProps) {
  const { t } = useTranslation();

  const state = useAsync(
    () => (importId ? getBackupImport(importId) : Promise.resolve(null)),
    [importId],
    { cacheKey: `backups:import:${importId ?? "none"}` },
  );
  const reload = state.reload;
  const record = state.data ?? null;
  const running = record ? importInProgress(record.status) : false;

  /**
   * 记录读不到了（404）。
   *
   * 这里**不是**普通的"这一行陈旧"：恢复生效会把整个数据库替换成包里的快照，
   * 而这条导入记录写在**被替换掉的那个**库里——于是重启完成后它自然消失。
   * 所以此刻 404 通常意味着**恢复已经成功落地**（用户手动重启也会走到这里）。
   *
   * 界面必须说清这一点，而不能报"读不到记录"：那个报错与真实情况正好相反。
   */
  const recordGone = state.error?.status === 404;

  // 只在仍在推进时轮询；到终态就停——继续拉只会白打接口。
  // 注意终态里 pending_restart 也算（它不会再变，除非重启），故一并停。
  useEffect(() => {
    if (!importId || !running) return;
    const timer = window.setInterval(reload, POLL_INTERVAL_MS);
    return () => window.clearInterval(timer);
  }, [importId, running, reload]);

  // 首次到达终态时通知外层刷新一次（列表里的状态徽章要跟着变）。
  const settledId = useMemo(
    () => (record && !importInProgress(record.status) ? record.importId : null),
    [record],
  );
  useEffect(() => {
    if (settledId) onSettled?.();
  }, [settledId, onSettled]);

  return (
    <Modal
      opened={importId !== null}
      onClose={onClose}
      title={t("backups.restoreProgressTitle")}
      centered
      size="lg"
    >
      {/*
        读不到记录（404）：见 recordGone 的说明——这在恢复流程里通常意味着**已经生效**。
        必须放在 AsyncBoundary **之外**：boundary 看到 error 就渲染通用错误态，会把
        这个"其实是成功"的收尾态挡掉（那正是最误导的一种呈现）。
      */}
      {recordGone ? (
        <Stack gap="sm" data-testid="restore-applied">
          <Alert color="green" variant="light" icon={<IconCheck size={16} />}>
            <Stack gap={6}>
              <Text size="sm" fw={600}>
                {t("backups.restoreAppliedTitle")}
              </Text>
              <Text size="sm">{t("backups.restoreAppliedBody")}</Text>
            </Stack>
          </Alert>
          <Group justify="flex-end">
            <Button variant="default" onClick={onClose}>
              {t("backups.restoreAppliedClose")}
            </Button>
          </Group>
        </Stack>
      ) : (
        <AsyncBoundary state={state}>
          {(data) =>
            data === null ? null : (
              <Stack gap="sm">
                <Group gap="xs" wrap="wrap">
                  <StatusPill
                    tone={importStatusTone(data.status)}
                    text={t(`backups.import_status_${data.status}`)}
                  />
                  <Badge variant="light" tt="none" color="violet">
                    {t(`backups.import_origin_${data.origin}`)}
                  </Badge>
                  <Code>{data.importId}</Code>
                </Group>

                {/* 阶段说明：让"正在做什么"可见。逐阶段写清，因为四步里只有一步能给出
                  真实百分比（拉取有字节数），其余三步是**不确定进度**——
                  画一个编出来的百分比比不画更糟，故那些阶段只给转圈 + 文字。 */}
                <Text size="sm">{t(`backups.restoreStage_${data.status}`)}</Text>

                <ImportProgress data={data} />

                {data.error || data.errorCode ? (
                  <Alert color="red" variant="light" icon={<IconAlertTriangle size={16} />}>
                    <Stack gap="xs">
                      <Text size="sm">
                        {data.errorCode
                          ? t(`backups.backupImportError.${data.errorCode}`, {
                              defaultValue: data.error ?? "",
                            })
                          : data.error}
                      </Text>
                      <Button
                        size="xs"
                        variant="light"
                        color="red"
                        w="fit-content"
                        leftSection={<IconRefresh size={14} />}
                        onClick={reload}
                      >
                        {t("backups.refresh")}
                      </Button>
                    </Stack>
                  </Alert>
                ) : null}

                {/* 待重启：这是恢复流程真正的最后一步，必须给出下一步动作。
                  冻结建议放在这里而不是流程开头——用户此刻已经等完校验与暂存，
                  正要重启，这才是"现在要不要先冻结"这个决策的真实时点。 */}
                {data.status === "pending_restart" ? (
                  <Alert color="orange" variant="light" icon={<IconAlertTriangle size={16} />}>
                    <Stack gap="xs">
                      <Text size="sm" fw={600}>
                        <IconCheck size={14} style={{ verticalAlign: "-2px" }} />{" "}
                        {t("backups.restoreReadyTitle")}
                      </Text>
                      <Text size="sm">{t("backups.restoreReadyBody")}</Text>
                      {frozen ? (
                        <Text size="xs" c="dimmed">
                          {t("backups.restoreFrozenNote")}
                        </Text>
                      ) : (
                        <Group gap="xs" wrap="wrap">
                          <Text size="xs" c="dimmed" style={{ flex: "1 1 240px" }}>
                            {t("backups.restoreFreezeAdvice")}
                          </Text>
                          {onFreeze ? (
                            <Button
                              size="xs"
                              variant="light"
                              color="orange"
                              leftSection={<IconSnowflake size={14} />}
                              onClick={onFreeze}
                            >
                              {t("backups.freezeFreezeButton")}
                            </Button>
                          ) : null}
                        </Group>
                      )}
                      {restartSlot}
                    </Stack>
                  </Alert>
                ) : null}
              </Stack>
            )
          }
        </AsyncBoundary>
      )}
    </Modal>
  );
}

/**
 * 进度条：只有"拉取"阶段有真实字节数可算，其余阶段显示不确定进度。
 *
 * 刻意不给后者编百分比——那会让人以为"快好了"，实际深校验的耗时与包体积同阶，
 * 编出来的数字比不显示更误导。
 */
function ImportProgress({ data }: { data: BackupImport }) {
  const { t } = useTranslation();
  const total = data.totalBytes ?? 0;
  const fetched = data.fetchedBytes ?? 0;

  if (data.status === "fetching" && total > 0) {
    const pct = Math.min(100, Math.round((fetched / total) * 100));
    return (
      <Stack gap={4}>
        <Progress value={pct} size="sm" striped animated />
        <Text size="xs" c="dimmed">
          {t("backups.restoreProgressBytes", {
            fetched: formatKiB(fetched),
            total: formatKiB(total),
            percent: pct,
          })}
        </Text>
      </Stack>
    );
  }

  if (importInProgress(data.status)) {
    return (
      <Group gap={6} wrap="nowrap">
        <Loader size="xs" />
        <Text size="xs" c="dimmed">
          {t("backups.restoreProgressIndeterminate", { blobs: data.blobCount ?? 0 })}
        </Text>
      </Group>
    );
  }
  return null;
}

/** 字节量按 KiB/MiB 归一，与列表里的口径一致，避免两处显示不同单位。 */
function formatKiB(bytes: number): string {
  if (bytes >= 1024 * 1024) return `${(bytes / 1024 / 1024).toFixed(1)} MiB`;
  return `${(bytes / 1024).toFixed(0)} KiB`;
}
