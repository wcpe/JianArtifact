// 一键重启控件（问题 2）。
//
// 为什么需要它：恢复的最后一步是重启服务，而此前界面只给出**一条要用户自己复制的命令**。
// 用户反馈的就是这一步的别扭——"需要重启"变成了"你自己去查怎么重启"。
//
// 为什么服务端先回响应再重启：重启会杀掉当前进程，若先执行重启，那条响应根本发不出去，
// 浏览器只会看到一个连接中断，与"请求失败"无从区分。所以契约规定服务端延迟约 2 秒执行，
// 先回 202（见 api/openapi.yaml 的 /maintenance/restart 与 domain.RestartService）。
//
// **能力探测与降级是本控件的重点**：服务端重启走的是**进程自替换**（`syscall.Exec` 换新映像），
// 不需要任何额外权限；但它仍可能不可用（非 Unix 平台 / argv[0] 不可执行 / 容器编排形态）。
// 服务端在 `supported:false` 时给出 `reason` 与手工等效命令，界面就老老实实显示命令——
// **绝不假装成功，也不把"不支持"说成"失败"**。
//
// 探活：重启后页面还停在旧连接上，必须自己轮询才知道服务回来了。这里轮询一个便宜且公开的
// 健康端点，成功即刷新页面数据。探活有上限，超时如实告知用户手动刷新，不无限转圈。
import { Alert, Button, Code, CopyButton, Group, Stack, Text } from "@mantine/core";
import { IconAlertTriangle, IconCopy, IconRefresh } from "@tabler/icons-react";
import { useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";

import { getRestartCapability, restartService } from "../../api/endpoints";
import { ApiError } from "../../api/client";
import type { MaintenanceRestartResult } from "../../api/types";
import { confirmDanger, notifyError, notifySuccess } from "../../lib/feedback";

/** 探活间隔与上限：重启通常几秒内回来；给足 1 分钟，超时就如实说超时。 */
const PROBE_INTERVAL_MS = 1500;
const PROBE_TIMEOUT_MS = 60_000;

/** 健康探针。用不依赖鉴权的公开端点，避免会话在重启后失效导致误判"还没回来"。 */
async function probeAlive(): Promise<boolean> {
  try {
    const res = await fetch("/api/v1/status", { method: "GET", cache: "no-store" });
    return res.ok;
  } catch {
    // 服务没起来时 fetch 会直接抛（连接被拒）：这就是"还没回来"，不是错误。
    return false;
  }
}

export function RestartControl({
  /** 重启成功回来后调用：让外层刷新列表/状态。 */
  onRestarted,
  /**
   * 后端给出的**手工重启等效命令**回传。
   *
   * 为什么需要它：界面上另有一处「可复制命令」的保底出口（待重启说明弹窗），
   * 而该命令**随部署形态而变**——自带 systemd 单元含 `NoNewPrivileges=true`，
   * `sudo systemctl restart` 在那台机器上必然失败，该形态下应给 `systemctl --user restart`。
   * 硬编码一条会在部分部署上给出**照做也失败**的指引，那比不给更糟；
   * 故把后端探测到的那条如实回传，由使用方展示。
   *
   * 注意这条命令**不是**服务端自动重启会执行的命令——自动重启用的是进程自替换，
   * 压根不执行命令；它只是"若你想手工重启，敲这条"。
   */
  onCommandResolved,
}: {
  onRestarted?: () => void;
  onCommandResolved?: (command: string) => void;
}) {
  const { t } = useTranslation();
  const [busy, setBusy] = useState(false);
  const [waiting, setWaiting] = useState(false);
  const [fallback, setFallback] = useState<MaintenanceRestartResult | null>(null);
  const [timedOut, setTimedOut] = useState(false);

  // 组件可能在探活期间被卸载（弹窗关闭）：用 ref 让轮询自己停下，避免对已卸载组件 setState。
  const aliveRef = useRef(true);
  useEffect(() => {
    aliveRef.current = true;
    return () => {
      aliveRef.current = false;
    };
  }, []);

  /**
   * 挂载即**只读**探测一次，把后端认可的命令回传给使用方。
   *
   * 必须用 `getRestartCapability`（GET）而不是 `restartService`（POST）：后者在
   * `supported:true` 时会当场排定重启，挂载即调用等于"打开弹窗就重启服务"。
   * GET 只跑同步预检，不排定任何执行，因此可安全地在这里调用。
   */
  const cmdRef = useRef(onCommandResolved);
  cmdRef.current = onCommandResolved;
  useEffect(() => {
    let active = true;
    getRestartCapability()
      .then((res) => {
        if (active && res.command) cmdRef.current?.(res.command);
      })
      .catch(() => {
        // 探测失败（网络/未授权）不影响本控件：使用方保留自己的兜底文案。
      });
    return () => {
      active = false;
    };
  }, []);

  const doRestart = async () => {
    setBusy(true);
    setTimedOut(false);
    try {
      const res = await restartService();
      // 无论受理还是降级，都把后端给出的命令回传：它是**本机对应的那条手工命令**
      // （可能带 sudo、也可能不带，取决于部署形态与运行身份）。
      if (res.command) onCommandResolved?.(res.command);
      if (!res.supported) {
        // 降级是正常业务结果（HTTP 200）：展示命令，不报错。
        setFallback(res);
        return;
      }
      notifySuccess(t("backups.restartAccepted"));
      setWaiting(true);
    } catch (err) {
      // 请求在服务重启的瞬间也可能断掉——但那同样意味着**已经在重启了**，
      // 不能报成失败把用户引向"再点一次"。故只对明确的 4xx 报错。
      if (err instanceof ApiError && err.status >= 400 && err.status < 500) {
        notifyError(err);
        return;
      }
      setWaiting(true);
    } finally {
      setBusy(false);
    }
  };

  const onRestart = () => {
    confirmDanger({
      title: t("backups.restartConfirmTitle"),
      message: t("backups.restartConfirmBody"),
      confirmLabel: t("backups.restartConfirm"),
      cancelLabel: t("common.cancel"),
      onConfirm: () => void doRestart(),
    });
  };

  // 探活：只在"已受理重启"后跑。成功 → 通知外层刷新；超时 → 如实告知。
  useEffect(() => {
    if (!waiting) return;
    const deadline = Date.now() + PROBE_TIMEOUT_MS;
    let stopped = false;
    // timer 必须先声明：tick 内部会重新安排下一次，而它在下面被同一个作用域引用。
    let timer: number | undefined;
    const tick = async () => {
      if (stopped || !aliveRef.current) return;
      if (await probeAlive()) {
        if (stopped || !aliveRef.current) return;
        setWaiting(false);
        notifySuccess(t("backups.restartDone"));
        onRestarted?.();
        return;
      }
      if (Date.now() > deadline) {
        if (!aliveRef.current) return;
        setWaiting(false);
        setTimedOut(true);
        return;
      }
      timer = window.setTimeout(() => void tick(), PROBE_INTERVAL_MS);
    };
    // 首个探针也稍作等待：重启动作有约 2 秒延迟，立刻探必然还是活的（旧进程），
    // 那会让"服务已回来"在换映像之前就误报成功。
    timer = window.setTimeout(() => void tick(), PROBE_INTERVAL_MS);
    return () => {
      stopped = true;
      if (timer !== undefined) window.clearTimeout(timer);
    };
  }, [waiting, onRestarted, t]);

  if (fallback) {
    return (
      <Alert color="orange" variant="light" icon={<IconAlertTriangle size={16} />}>
        <Stack gap={6}>
          <Text size="sm" fw={600}>
            {t("backups.restartUnsupportedTitle")}
          </Text>
          <Text size="xs">
            {fallback.reason
              ? t("backups.restartUnsupportedBody", { reason: fallback.reason })
              : t("backups.restartUnsupportedManual")}
          </Text>
          {/* 命令始终返回：即使探测失败也要让用户有可执行的东西，而不是一个死胡同。 */}
          <Group gap="xs" wrap="nowrap">
            <Code style={{ flex: 1 }}>{fallback.command}</Code>
            <CopyButton value={fallback.command}>
              {({ copied, copy }) => (
                <Button
                  size="xs"
                  variant="default"
                  leftSection={<IconCopy size={14} />}
                  onClick={copy}
                >
                  {copied ? t("backups.linkCopied") : t("backups.copyCommand")}
                </Button>
              )}
            </CopyButton>
          </Group>
        </Stack>
      </Alert>
    );
  }

  return (
    <Stack gap={6}>
      <Group gap="xs" wrap="wrap">
        <Button
          color="orange"
          loading={busy || waiting}
          leftSection={<IconRefresh size={16} />}
          onClick={onRestart}
          data-testid="restart-service"
        >
          {waiting ? t("backups.restartWaiting") : t("backups.restartButton")}
        </Button>
      </Group>
      {waiting ? (
        <Text size="xs" c="dimmed">
          {t("backups.restartWaitingHint")}
        </Text>
      ) : null}
      {timedOut ? (
        <Alert color="yellow" variant="light" icon={<IconAlertTriangle size={16} />} py={6}>
          <Text size="xs">{t("backups.restartTimeout")}</Text>
        </Alert>
      ) : null}
    </Stack>
  );
}
