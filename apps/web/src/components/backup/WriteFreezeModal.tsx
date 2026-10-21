// 写入冻结弹窗（FR-134）：只负责**发起冻结**这一个写操作。
// 冻结档位仅给 30 分钟 / 2 小时 / 4 小时 / 24 小时——契约与后端都不接受无限期冻结，
// UI 也只暴露有界选项。
//
// 状态展示与**解冻**在页面的动作条（ActionBar）上，其中冻结态由「解冻写入」按钮
// 自身的橙色变体与文案体现（原先是卡内一只常驻 StatusPill，动作区收成按钮行后
// 不再单列——该状态与按钮本就一一对应）：
// 解冻不放在这里，是因为冻结按钮在已冻结时必须禁用，若解冻也只在弹窗里，就会形成
// 「已冻结 → 按钮禁用 → 打不开弹窗 → 永远解不了冻」的死锁。本组件因此只承担冻结，
// 不渲染状态徽章，也不再自己取数——state 由页面透传，保证「按钮显示的状态」
// 与「这里的按钮可用性」永远同源。
import { Button, Group, Modal, Radio, Stack, Text, TextInput } from "@mantine/core";
import { IconSnowflake } from "@tabler/icons-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";

import { freezeWrites } from "../../api/endpoints";
import type { WriteFreezeState } from "../../api/types";
import type { AsyncState } from "../../hooks/useAsync";
import { notifyError, notifySuccess } from "../../lib/feedback";

// 仅暴露有界档位（秒）。契约层窗口上限 24 小时，且不允许无限期冻结。
const TTL_OPTIONS = [
  { labelKey: "backups.freezeTtl30m", value: 1800 },
  { labelKey: "backups.freezeTtl2h", value: 7200 },
  { labelKey: "backups.freezeTtl4h", value: 14400 },
  { labelKey: "backups.freezeTtl24h", value: 86400 },
] as const;

export function WriteFreezeModal({
  opened,
  onClose,
  state,
}: {
  opened: boolean;
  onClose: () => void;
  state: AsyncState<WriteFreezeState>;
}) {
  const { t } = useTranslation();
  const [ttl, setTtl] = useState<number>(7200);
  const [reason, setReason] = useState("");

  const onFreeze = async () => {
    try {
      await freezeWrites({ ttlSeconds: ttl, reason: reason.trim() || undefined });
      notifySuccess(t("backups.freezeStarted"));
      onClose();
      setReason("");
      state.reload();
    } catch (err) {
      notifyError(err);
    }
  };

  return (
    <Modal opened={opened} onClose={onClose} title={t("backups.freezeModalTitle")} centered>
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
          <Button variant="default" onClick={onClose}>
            {t("common.cancel")}
          </Button>
          <Button onClick={() => void onFreeze()} leftSection={<IconSnowflake size={14} />}>
            {t("backups.freezeConfirm")}
          </Button>
        </Group>
      </Stack>
    </Modal>
  );
}
