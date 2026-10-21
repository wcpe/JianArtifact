// 校验 / 深度校验弹窗。
//
// 为什么需要它：原先「校验」只是 `await verifyBackup(...)` 再弹一个 toast。用户看不到
// 在查什么、查到哪一步、结论是什么；深校验动辄几分钟，期间界面毫无反馈，与"卡住了"无从区分。
//
// **诚实性约束（本文件的核心设计）**：后端 verify 是**同步**接口——一次返回最终结论
//（`ok` / `blobs` / `assets` / `error`），没有中间进度可查（对比 drill 是 202 + 轮询）。
// 因此这里**不画百分比**：画一个编出来的进度条比不画更糟，它会让人以为"快好了"。
// 取而代之的是两件真实可给的东西：
//   1. 开跑**前**列出真实会执行的检查项（顺序与 archive.Reader.Verify 的实现步骤一一对应）；
//   2. 跑完**后**就地展示结论（通过 / 未通过与原因、规模、耗时），而不是让结论随 toast 消失。
//
// 浅校验必须显式写明"不核对内容"：否则"校验通过"会被读成"这份包没问题"，而它其实
// 对"结构完好但内容被篡改"的包照样判通过——那是深校验才管的事。
import {
  Alert,
  Box,
  Button,
  Code,
  Group,
  Loader,
  Modal,
  Stack,
  Text,
  ThemeIcon,
} from "@mantine/core";
import { IconCheck, IconInfoCircle, IconReload, IconX } from "@tabler/icons-react";
import { useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";

import { verifyBackup } from "../../api/endpoints";
import type { BackupVerification } from "../../api/types";
import { ApiError } from "../../api/client";

/** 一次校验请求的目标。null 表示弹窗关闭。 */
export interface VerifyRequest {
  packageId: string;
  deep: boolean;
}

interface VerifyOutcome {
  ok: boolean;
  /** 失败原因（后端原文或网络错误）。 */
  reason?: string;
  blobs?: number;
  assets?: number;
  /** 耗时毫秒（前端实测，用于给"慢"一个具体量级）。 */
  elapsedMs: number;
}

export function VerifyModal({
  request,
  onClose,
  onStale,
  onSettled,
}: {
  request: VerifyRequest | null;
  onClose: () => void;
  /** 404（包已被淘汰）交由外层统一处置；弹窗自身不重复报错。 */
  onStale?: (err: unknown) => boolean;
  /** 校验完成、结论已展示（外层可据此刷新"最近校验"等派生数据）。 */
  onSettled?: () => void;
}) {
  const { t } = useTranslation();
  const [outcome, setOutcome] = useState<VerifyOutcome | null>(null);
  const [running, setRunning] = useState(false);

  // 目标变化即重置结论：否则上一个包的结论会短暂显示在新包名下，
  // 那是最糟的一类错——**张冠李戴的结论**比没有结论更容易被当真。
  const pkgId = request?.packageId ?? null;
  // 档位可在弹窗内升级：入口只有一个「校验」，
  // 浅校验先跑，用户看到结论后若还想逐字节核对，再点「深度校验」。
  // 用 state 而不是直接用 request.deep，正是为了支持这种就地升级。
  const [deep, setDeep] = useState(request?.deep ?? false);
  useEffect(() => {
    setDeep(request?.deep ?? false);
  }, [request?.packageId, request?.deep]);
  useEffect(() => {
    setOutcome(null);
  }, [pkgId, deep]);

  // 同一目标只发一次请求。这里用 ref 记录"已发过的目标键"，
  // 避免 React 严格模式下 effect 跑两次真的打两次深校验（那是分钟级代价）。
  const firedRef = useRef<string | null>(null);
  useEffect(() => {
    if (!pkgId || firedRef.current === `${pkgId}:${deep}`) return;
    firedRef.current = `${pkgId}:${deep}`;
    let active = true;
    const startedAt = Date.now();
    setRunning(true);
    setOutcome(null);
    verifyBackup(pkgId, deep)
      .then((res: BackupVerification) => {
        if (!active) return;
        setOutcome({
          ok: res.ok,
          reason: res.error,
          blobs: res.blobs,
          assets: res.assets,
          elapsedMs: Date.now() - startedAt,
        });
      })
      .catch((err: unknown) => {
        if (!active) return;
        // 包没了（404）是"这一行过期了"，不是校验失败：交回外层刷新列表并提示，
        // 弹窗不留一个误导性的红结论。
        if (onStale?.(err)) {
          onClose();
          return;
        }
        const reason =
          err instanceof ApiError ? err.message : err instanceof Error ? err.message : undefined;
        setOutcome({ ok: false, reason, elapsedMs: Date.now() - startedAt });
      })
      .finally(() => {
        if (active) setRunning(false);
      });
    return () => {
      active = false;
    };
  }, [pkgId, deep, onStale, onClose]);

  // 结论落地再通知外层：调用方通常要重拉"最近校验"，此时后端已写入报告。
  const settled = outcome !== null;
  useEffect(() => {
    if (settled) onSettled?.();
  }, [settled, onSettled]);

  return (
    <Modal
      opened={request !== null}
      onClose={onClose}
      title={deep ? t("backups.verifyModalTitleDeep") : t("backups.verifyModalTitle")}
      centered
    >
      <Stack gap="sm">
        {pkgId ? (
          <Text size="xs" c="dimmed">
            {t("backups.verifyTarget")}：<Code>{pkgId}</Code>
          </Text>
        ) : null}

        {/* 检查项清单：无论跑没跑完都列着——跑之前它是"将要做什么"，
            跑完之后它是"做了什么"的对照物，结论就贴在它下面。 */}
        <Box>
          <Text size="sm" fw={600} mb={6}>
            {t("backups.verifyChecksTitle")}
          </Text>
          <Stack gap={6}>
            {[
              "verifyCheckManifest",
              "verifyCheckDb",
              "verifyCheckIndex",
              "verifyCheckComplete",
              ...(deep ? ["verifyCheckBlobsDeep"] : []),
            ].map((key) => (
              <Group key={key} gap="xs" wrap="nowrap" align="flex-start">
                {/* 未完成时统一给一个中性圆点，不给"逐项打勾"的假进度——
                    同步接口拿不到逐项完成情况，勾到哪一项都是编的。 */}
                <ThemeIcon size={16} radius="xl" variant="light" color={settled ? "green" : "gray"}>
                  {settled && outcome?.ok ? <IconCheck size={11} /> : <Box w={5} h={5} />}
                </ThemeIcon>
                <Text size="xs">{t(`backups.${key}`)}</Text>
              </Group>
            ))}
          </Stack>
        </Box>

        {running ? (
          <Group gap={8} wrap="nowrap">
            <Loader size="xs" />
            <Text size="xs" c="dimmed">
              {deep ? t("backups.verifyRunningDeep") : t("backups.verifyRunning")}
            </Text>
          </Group>
        ) : null}

        {/* 浅校验的运行中/结论期都要挂这句：它约束的是"通过"这个结论的含义，
            而不是某一次结果。 */}
        {!deep ? (
          <Alert color="blue" variant="light" icon={<IconInfoCircle size={16} />} py={8}>
            <Text size="xs">{t("backups.verifyShallowNotContent")}</Text>
          </Alert>
        ) : null}

        {outcome ? (
          <Alert
            color={outcome.ok ? "green" : "red"}
            variant="light"
            icon={outcome.ok ? <IconCheck size={16} /> : <IconX size={16} />}
          >
            <Stack gap={4}>
              <Text size="sm" fw={600}>
                {outcome.ok ? t("backups.verifyResultOk") : t("backups.verifyResultFailed")}
              </Text>
              {outcome.ok ? (
                <Text size="xs">
                  {t("backups.verifyResultSummary", {
                    blobs: outcome.blobs ?? 0,
                    assets: outcome.assets ?? 0,
                  })}
                </Text>
              ) : outcome.reason ? (
                <Text size="xs">{outcome.reason}</Text>
              ) : null}
              <Text size="xs" c="dimmed">
                {t("backups.verifyElapsed", { seconds: (outcome.elapsedMs / 1000).toFixed(1) })}
              </Text>
            </Stack>
          </Alert>
        ) : null}

        <Group justify="space-between" align="center" wrap="wrap">
          {/* 浅校验通过后才给「深度校验」入口：它耗时与包体积同阶（20GB 是分钟级），
              不该在用户还没看到浅结论时就摆在最显眼处诱导点击。
              代价直接写在按钮文字边上，避免"点下去才发现很慢"。 */}
          {!deep && outcome?.ok ? (
            <Stack gap={2}>
              <Button
                size="xs"
                variant="light"
                leftSection={<IconReload size={14} />}
                onClick={() => setDeep(true)}
                data-testid="verify-deepen"
              >
                {t("backups.verifyDeepen")}
              </Button>
              <Text size="xs" c="dimmed">
                {t("backups.verifyDeepenHint")}
              </Text>
            </Stack>
          ) : (
            <Box />
          )}
          <Group gap="xs">
            {/* 深校验可能要几分钟，允许提前关掉弹窗（请求仍在飞、结论会由后端落库）；
                这里只是把"等不了"的出口给出来，不假装能取消服务端动作。 */}
            <Button variant="default" onClick={onClose}>
              {t("common.close")}
            </Button>
          </Group>
        </Group>
      </Stack>
    </Modal>
  );
}
