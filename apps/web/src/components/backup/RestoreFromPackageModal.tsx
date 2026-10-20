// 「用这个包恢复」的确认弹窗（FR-40 追加六）。
//
// 为什么需要这个入口：另外三条恢复通道（URL 拉取 / 上传本机文件 / CLI）都要求包来自**外部**，
// 于是页面上明明列着可用的包，却没有任何按钮能用它恢复——用户会直接得出
// "只能备份、不能恢复"的结论。本弹窗补的就是这个缺口。
//
// 为什么必须二次确认而不是点一下就跑：这是**会替换本机全部数据**的动作（用户、仓库、制品、
// 设置），且它与"演练"不同——演练只读、随便点；恢复一旦重启就真的替换了。因此这里
// 把"会发生什么"（替换、需重启、留有回滚备份）在按钮上方讲清，再让用户按键。
//
// 非空目标需显式勾选覆盖：与其它三条通道共用同一道护栏（target_not_empty），
// 勾选框**前置**在提交按钮旁，而不是等失败了再让用户猜哪里不对。
import { Alert, Button, Checkbox, Code, Group, Modal, Stack, Text, Accordion } from "@mantine/core";
import { IconAlertTriangle, IconRestore, IconSnowflake } from "@tabler/icons-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";

import { restoreFromLocalPackage } from "../../api/endpoints";
import { BackupDiffView } from "./BackupDiffView";
import type { BackupPackage } from "../../api/types";
import { notifyError, notifySuccess } from "../../lib/feedback";

export interface RestoreFromPackageModalProps {
  /** 待恢复的包；null 表示关闭。 */
  pkg: BackupPackage | null;
  onClose: () => void;
  /** 受理成功后把导入记录交回页面：由它打开恢复进度弹窗并刷新列表。 */
  onAccepted?: (importId: string) => void;
  /** 建议先冻结写入的入口；不传则不显示该按钮。 */
  onFreeze?: () => void;
  /** 当前是否已冻结（显示"已冻结"而不是再劝一次）。 */
  frozen?: boolean;
  /**
   * 陈旧行处置：404（包已被保留策略淘汰）交回页面统一处理。
   *
   * 为什么必须接这一条：本弹窗是**唯一**从列表行直接发起的恢复入口，而自动备份每 30 秒
   * 生成并淘汰旧包——用户看到列表、点开弹窗、读完说明再按「开始恢复」，这中间包完全可能
   * 已被淘汰（实测正是如此：弹窗开着时点下去，后端返 404）。不接就会退化成裸报
   * 「备份包不存在」，与用户先前反馈的「复制链接会失败」是同一个观感问题。
   */
  onStale?: (err: unknown) => boolean;
}

export function RestoreFromPackageModal({
  pkg,
  onClose,
  onAccepted,
  onFreeze,
  frozen,
  onStale,
}: RestoreFromPackageModalProps) {
  const { t } = useTranslation();
  const [overwrite, setOverwrite] = useState(false);
  const [starting, setStarting] = useState(false);

  const close = () => {
    setOverwrite(false);
    onClose();
  };

  const onStart = async () => {
    if (!pkg) return;
    setStarting(true);
    try {
      const accepted = await restoreFromLocalPackage(pkg.packageId, { overwrite });
      notifySuccess(t("backups.restoreAccepted"));
      // 传回 importId 而不是让页面"刷新一下就好"：受理只是开始，校验与暂存还在跑，
      // 页面要据此打开进度弹窗（见 RestoreProgressModal 文件头）。
      onAccepted?.(accepted.importId);
      close();
    } catch (err) {
      // 包已被淘汰（404）：交回页面刷新列表并给出可操作提示，而不是裸报"备份包不存在"。
      if (onStale?.(err)) {
        close();
        return;
      }
      notifyError(err);
    } finally {
      setStarting(false);
    }
  };

  return (
    <Modal opened={pkg !== null} onClose={close} title={t("backups.restoreConfirmTitle")} centered>
      <Stack gap="sm">
        <Alert color="orange" variant="light" icon={<IconAlertTriangle size={16} />}>
          <Text size="sm">{t("backups.restoreConfirmBody", { id: pkg?.packageId ?? "" })}</Text>
        </Alert>

        {/* 覆盖开关前置：本机非空时没有它会直接 409，而那时用户已经等了一轮校验，
            再去猜"哪里不对"代价更高。勾选框 + 说明把这件事提前讲明白。 */}
        <Checkbox
          checked={overwrite}
          onChange={(e) => setOverwrite(e.currentTarget.checked)}
          label={t("backups.restoreOverwriteLabel")}
          description={t("backups.restoreOverwriteHint")}
          data-testid="restore-overwrite"
        />

        {/* 冻结建议（问题 3）：**不强制**，只提示 + 给入口。
            为什么不做成强制前置：恢复本身是"暂存 + 重启时原子替换"，一致性已由机制保证；
            而强制冻结的代价是"忘记解冻"直接把实例变成只读——那比它要防的问题更常见也更痛。
            已冻结时不再劝一次，改为提醒"重启后记得解冻"。 */}
        {frozen ? (
          <Text size="xs" c="dimmed">
            {t("backups.restoreFrozenNote")}
          </Text>
        ) : (
          <Alert color="blue" variant="light" icon={<IconSnowflake size={16} />}>
            <Stack gap={6}>
              <Text size="xs">{t("backups.restoreFreezeAdvice")}</Text>
              {onFreeze ? (
                <Button
                  size="xs"
                  variant="light"
                  color="blue"
                  w="fit-content"
                  leftSection={<IconSnowflake size={14} />}
                  onClick={onFreeze}
                  data-testid="restore-freeze"
                >
                  {t("backups.freezeFreezeButton")}
                </Button>
              ) : null}
            </Stack>
          </Alert>
        )}

        {/* 包内容与差异（FR-40 追加十四）。
            **放在这里**而不是藏进另一个菜单项：用户点「用这个包恢复」的这一刻，
            才是最需要知道"包里有什么、和我现在的有什么不同"的时候。
            用 Accordion 收起来，是因为摘要行情愿常看，明细只在想看时展开——
            恢复确认弹窗本身要短，不能把用户淹在表格里。 */}
        {pkg ? (
          <Accordion variant="separated" radius="sm" chevronPosition="left">
            <Accordion.Item value="diff">
              <Accordion.Control>
                <Text size="sm" fw={600}>
                  {t("backups.restoreDiffTitle")}
                </Text>
              </Accordion.Control>
              <Accordion.Panel>
                <BackupDiffView packageId={pkg.packageId} />
              </Accordion.Panel>
            </Accordion.Item>
          </Accordion>
        ) : null}

        {pkg ? (
          <Text size="xs" c="dimmed">
            <Code>{pkg.packageId}</Code>
          </Text>
        ) : null}

        <Group justify="flex-end" gap="xs">
          <Button variant="default" onClick={close}>
            {t("common.cancel")}
          </Button>
          <Button
            color="orange"
            leftSection={<IconRestore size={16} />}
            loading={starting}
            onClick={() => void onStart()}
            data-testid="restore-start"
          >
            {t("backups.restoreStart")}
          </Button>
        </Group>
      </Stack>
    </Modal>
  );
}
