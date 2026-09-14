// 生成备份包向导：选择方式 → 填写备注 → 生成并给出去向链接。
// 生成耗时与包体积同阶，因此第三步在弹窗内轮询进度，完成后直接奉上带签名的下载链接。
import {
  Alert,
  Button,
  Code,
  CopyButton,
  Group,
  Modal,
  Radio,
  Stack,
  Stepper,
  Text,
  TextInput,
} from "@mantine/core";
import { IconAlertTriangle, IconCheck, IconCopy, IconDownload } from "@tabler/icons-react";
import { useCallback, useEffect, useState } from "react";
import { useTranslation } from "react-i18next";

import { createBackup, createBackupLink, getBackup } from "../../api/endpoints";
import type { BackupPackage, BackupPackageMode } from "../../api/types";
import { notifyError, notifySuccess } from "../../lib/feedback";

const POLL_INTERVAL_MS = 800;
const POLL_TIMEOUT_MS = 30 * 60 * 1000;

export interface BackupCreateModalProps {
  opened: boolean;
  onClose: () => void;
  /** 生成完成后回调，供列表立即刷新。 */
  onCreated: () => void;
}

/** 轮询直到包进入终态；超时视为失败，避免弹窗永久转圈。 */
async function waitForBackup(packageId: string): Promise<BackupPackage> {
  const deadline = Date.now() + POLL_TIMEOUT_MS;
  for (;;) {
    const rec = await getBackup(packageId);
    if (rec.status === "done" || rec.status === "failed") {
      return rec;
    }
    if (Date.now() > deadline) {
      throw new Error("等待生成完成超时");
    }
    await new Promise((resolve) => setTimeout(resolve, POLL_INTERVAL_MS));
  }
}

export function BackupCreateModal({ opened, onClose, onCreated }: BackupCreateModalProps) {
  const { t } = useTranslation();
  const [active, setActive] = useState(0);
  const [mode, setMode] = useState<BackupPackageMode>("hot");
  const [label, setLabel] = useState("");
  const [busy, setBusy] = useState(false);
  const [result, setResult] = useState<BackupPackage | null>(null);
  const [link, setLink] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);

  const reset = useCallback(() => {
    setActive(0);
    setMode("hot");
    setLabel("");
    setBusy(false);
    setResult(null);
    setLink(null);
    setError(null);
  }, []);

  // 每次关闭都把向导复位，避免下次打开残留上一次的结果。
  useEffect(() => {
    if (!opened) {
      reset();
    }
  }, [opened, reset]);

  const start = async () => {
    setBusy(true);
    setError(null);
    setActive(2);
    try {
      const created = await createBackup({
        mode,
        label: label.trim() ? label.trim() : undefined,
      });
      onCreated();
      const finished = await waitForBackup(created.packageId);
      setResult(finished);
      onCreated();
      if (finished.status === "failed") {
        setError(finished.errorSummary ?? t("backups.generateFailed"));
        return;
      }
      const sign = await createBackupLink(created.packageId);
      setLink(sign.url);
    } catch (err) {
      notifyError(err);
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Modal
      opened={opened}
      onClose={onClose}
      title={t("backups.createTitle")}
      size="lg"
      centered
      closeOnClickOutside={!busy}
      closeOnEscape={!busy}
    >
      <Stack gap="md">
        <Stepper active={active} size="sm" allowNextStepsSelect={false}>
          <Stepper.Step label={t("backups.stepMode")} description={t("backups.stepModeDesc")}>
            <Radio.Group
              value={mode}
              onChange={(value) => setMode(value as BackupPackageMode)}
              mt="md"
            >
              <Stack gap="xs">
                <Radio.Card value="hot" radius="md" p="md" withBorder>
                  <Group wrap="nowrap" align="flex-start">
                    <Radio.Indicator />
                    <Stack gap={2}>
                      <Text fw={500}>{t("backups.modeHotTitle")}</Text>
                      <Text size="xs" c="dimmed">
                        {t("backups.modeHotDesc")}
                      </Text>
                    </Stack>
                  </Group>
                </Radio.Card>
                <Radio.Card value="frozen" radius="md" p="md" withBorder>
                  <Group wrap="nowrap" align="flex-start">
                    <Radio.Indicator />
                    <Stack gap={2}>
                      <Text fw={500}>{t("backups.modeFrozenTitle")}</Text>
                      <Text size="xs" c="dimmed">
                        {t("backups.modeFrozenDesc")}
                      </Text>
                    </Stack>
                  </Group>
                </Radio.Card>
              </Stack>
            </Radio.Group>
          </Stepper.Step>

          <Stepper.Step label={t("backups.stepOptions")} description={t("backups.stepOptionsDesc")}>
            <TextInput
              mt="md"
              label={t("backups.labelField")}
              placeholder={t("backups.labelPlaceholder")}
              value={label}
              maxLength={200}
              onChange={(event) => setLabel(event.currentTarget.value)}
            />
            <Alert mt="md" color="gray" variant="light">
              {t("backups.includedNote")}
            </Alert>
          </Stepper.Step>

          <Stepper.Step label={t("backups.stepRun")} description={t("backups.stepRunDesc")}>
            <Stack gap="sm" mt="md">
              {busy ? <Text size="sm">{t("backups.generating")}</Text> : null}
              {error ? (
                <Alert color="red" icon={<IconAlertTriangle size={16} />}>
                  {error}
                </Alert>
              ) : null}
              {result && result.status === "done" ? (
                <>
                  <Alert color="green" icon={<IconCheck size={16} />}>
                    {t("backups.generateDone")} · {result.packageId}
                  </Alert>
                  {link ? (
                    <Stack gap={4}>
                      <Text size="xs" c="dimmed">
                        {t("backups.linkHint")}
                      </Text>
                      <Code block>{link}</Code>
                    </Stack>
                  ) : null}
                </>
              ) : null}
            </Stack>
          </Stepper.Step>
        </Stepper>

        <Group justify="space-between">
          {/* 第 2 步一旦开始生成就不可回退：快照与打包不可中断。 */}
          <Button
            variant="default"
            style={{ visibility: active === 1 ? "visible" : "hidden" }}
            disabled={active !== 1 || busy}
            onClick={() => setActive(0)}
          >
            {t("backups.back")}
          </Button>
          <Group gap="xs">
            {active === 0 ? (
              <Button onClick={() => setActive(1)}>{t("backups.next")}</Button>
            ) : null}
            {active === 1 ? (
              <Button loading={busy} onClick={() => void start()}>
                {t("backups.start")}
              </Button>
            ) : null}
            {active === 2 ? (
              <>
                {link ? (
                  <>
                    <CopyButton value={link}>
                      {({ copied, copy }) => (
                        <Button
                          variant="default"
                          leftSection={<IconCopy size={16} />}
                          onClick={() => {
                            copy();
                            notifySuccess(t("backups.linkCopied"));
                          }}
                        >
                          {copied ? t("backups.linkCopied") : t("backups.getLink")}
                        </Button>
                      )}
                    </CopyButton>
                    <Button component="a" href={link} leftSection={<IconDownload size={16} />}>
                      {t("backups.download")}
                    </Button>
                  </>
                ) : null}
                <Button variant="default" disabled={busy} onClick={onClose}>
                  {t("backups.close")}
                </Button>
              </>
            ) : null}
          </Group>
        </Group>
      </Stack>
    </Modal>
  );
}
