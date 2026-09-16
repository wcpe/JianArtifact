// 写入冻结窗口卡（FR-134）：展示当前冻结状态，提供有界冻结与解冻。
// 冻结档位仅给 30 分钟 / 2 小时 / 4 小时 / 24 小时——契约与后端都不接受无限期冻结，
// UI 也只暴露有界选项。解冻走 confirmDanger 二次确认。
import { Button, Group, Modal, Radio, Stack, Text, TextInput } from "@mantine/core";
import { IconLock, IconLockOpen, IconSnowflake } from "@tabler/icons-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";

import { freezeWrites, getWriteFreeze, unfreezeWrites } from "../../api/endpoints";
import type { WriteFreezeState } from "../../api/types";
import { confirmDanger, notifyError, notifySuccess } from "../../lib/feedback";
import { formatStamp } from "../../lib/format";
import { useAsync } from "../../hooks/useAsync";
import { OpsSection, StatusPill } from "../ops/OpsKit";

// 仅暴露有界档位（秒）。契约层窗口上限 24 小时，且不允许无限期冻结。
const TTL_OPTIONS = [
  { labelKey: "backups.freezeTtl30m", value: 1800 },
  { labelKey: "backups.freezeTtl2h", value: 7200 },
  { labelKey: "backups.freezeTtl4h", value: 14400 },
  { labelKey: "backups.freezeTtl24h", value: 86400 },
] as const;

export function WriteFreezeCard() {
  const { t } = useTranslation();
  const [opened, setOpened] = useState(false);
  const [ttl, setTtl] = useState<number>(7200);
  const [reason, setReason] = useState("");

  const state = useAsync<WriteFreezeState>(() => getWriteFreeze(), [], {
    cacheKey: "backups:freeze",
  });
  const freeze = state.data;

  const onFreeze = async () => {
    try {
      await freezeWrites({ ttlSeconds: ttl, reason: reason.trim() || undefined });
      notifySuccess(t("backups.freezeStarted"));
      setOpened(false);
      setReason("");
      state.reload();
    } catch (err) {
      notifyError(err);
    }
  };

  const onUnfreeze = () => {
    confirmDanger({
      title: t("backups.freezeUnfreezeTitle"),
      message: t("backups.freezeUnfreezeMessage"),
      confirmLabel: t("backups.freezeUnfreezeLabel"),
      cancelLabel: t("common.cancel"),
      onConfirm: () => {
        void unfreezeWrites()
          .then(() => {
            notifySuccess(t("backups.freezeUnfrozen"));
            state.reload();
          })
          .catch(notifyError);
      },
    });
  };

  const frozen = freeze?.frozen ?? false;

  return (
    <OpsSection
      title={t("backups.freezeCardTitle")}
      style={{ flexShrink: 0 }}
      actions={
        <Group gap="xs">
          <Button
            size="xs"
            leftSection={<IconLock size={14} />}
            disabled={frozen}
            onClick={() => setOpened(true)}
          >
            {t("backups.freezeFreezeButton")}
          </Button>
          <Button
            size="xs"
            variant="default"
            color="orange"
            leftSection={<IconLockOpen size={14} />}
            disabled={!frozen}
            onClick={onUnfreeze}
          >
            {t("backups.freezeUnfreezeButton")}
          </Button>
        </Group>
      }
    >
      <Group gap="sm" wrap="nowrap">
        <StatusPill
          tone={frozen ? "orange" : "green"}
          text={frozen ? t("backups.freezeStatusFrozen") : t("backups.freezeStatusWritable")}
        />
        {frozen && freeze ? (
          <Stack gap={0}>
            {freeze.until ? (
              <Text size="sm">
                {t("backups.freezeUntilLabel")}：{formatStamp(freeze.until)}
              </Text>
            ) : null}
            {freeze.reason ? (
              <Text size="xs" c="dimmed">
                {freeze.reason}
              </Text>
            ) : null}
          </Stack>
        ) : null}
      </Group>

      <Modal
        opened={opened}
        onClose={() => setOpened(false)}
        title={t("backups.freezeModalTitle")}
        centered
      >
        <Stack gap="sm">
          <Text size="xs" c="dimmed">
            {t("backups.freezeModalHint")}
          </Text>
          <div>
            <Text size="xs" fw={500} mb={4}>
              {t("backups.freezeTtlLabel")}
            </Text>
            <Radio.Group value={String(ttl)} onChange={(v) => setTtl(Number(v))}>
              <Stack gap={4}>
                {TTL_OPTIONS.map((o) => (
                  <Radio key={o.value} value={String(o.value)} label={t(o.labelKey)} />
                ))}
              </Stack>
            </Radio.Group>
          </div>
          <TextInput
            label={t("backups.freezeReasonLabel")}
            placeholder={t("backups.freezeReasonPlaceholder")}
            value={reason}
            onChange={(e) => setReason(e.currentTarget.value)}
          />
          <Group justify="flex-end" gap="xs">
            <Button variant="default" onClick={() => setOpened(false)}>
              {t("common.cancel")}
            </Button>
            <Button onClick={() => void onFreeze()} leftSection={<IconSnowflake size={14} />}>
              {t("backups.freezeConfirm")}
            </Button>
          </Group>
        </Stack>
      </Modal>
    </OpsSection>
  );
}
