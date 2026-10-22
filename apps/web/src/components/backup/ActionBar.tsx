// 动作条：页面最上面一排按钮，直接挂在「备份与导入事件」区块的标题行右侧。
//
// 为什么不再是两张常驻卡（用户复核追加四后提出）：那两张卡合计约 173px 常驻高度，
// 而它们承载的只是四个按钮加三行说明文字——按钮是操作、说明不是。用户的原话是
// 「顶上两块太突兀了 需要收纳成按钮放到 备份与导入事件」，故整块收成一行：
//   ① 按钮全部挂到标题行右侧（`OpsSection` 的 `actions`），不再占用独立的纵向空间；
//   ② 卡里的说明文字迁到「说明」气泡（OpsHelpButton 的既有定位：需要时才展开）；
//   ③ 写入冻结状态本来是卡内常驻的 StatusPill，收成按钮后由「冻结/解冻写入」按钮
//      自身的语义色与文案体现（冻结态用橙色变体 + 「解冻写入」），不再单列一行，
//      因为该状态本就与按钮一一对应，单独再摆一遍是同一事实的第二次表述。
//
// 保留的行为约束（勿在重构中丢掉）：
//   备份按钮在生成中禁用（后端同一时刻只跑一个生成槽位，撞车会被静默跳过，
//   表现为"点了没反应"，不如直接禁用）；上传进行中两个导入按钮都要禁用，
//   否则能把内联面板收掉、连进度条与取消入口一起藏掉（见 ImportInline 文件头）。
import { Button, Group } from "@mantine/core";
import {
  IconArchive,
  IconDatabaseImport,
  IconLock,
  IconLockOpen,
  IconSettings,
  IconUpload,
  IconWorldDownload,
} from "@tabler/icons-react";
import { useTranslation } from "react-i18next";

import { formatStamp } from "../../lib/format";
import { OpsHelpButton } from "../ops/OpsKit";
import type { ImportChannel } from "./ImportInline";

export interface ActionBarProps {
  /** 写入是否已冻结。 */
  frozen: boolean;
  /** 冻结截止时间（已冻结时可能为空，表示不限时）。 */
  freezeUntil?: string | null;
  /** 生成备份包的入口（打开向导）。 */
  onCreate: () => void;
  /** 冻结写入的入口。 */
  onFreeze: () => void;
  /** 解冻写入（仅冻结时显示）。 */
  onUnfreeze: () => void;
  /** 自动备份与保留策略设置的入口。 */
  onOpenAutoConfig: () => void;
  /**
   * 导入通道的展开/收起：点击已展开的那个通道会收起（再点一次 = 撤销）。
   * null 表示整块收起。
   */
  onToggleChannel: (next: ImportChannel | null) => void;
  /** 当前展开的导入通道；null = 收起。用于把按钮切成「激活」样式。 */
  activeChannel: ImportChannel | null;
  /**
   * 上传进行中：两个通道按钮都禁用。
   *
   * 这是"上传中不可收起面板"**实际生效**的那一层——动作条的按钮直接 setChannel，
   * 若不置灰就能把内联面板收掉（进度条与取消入口随之消失），绕开面板自己的收起按钮。
   */
  uploadBusy: boolean;
  /** 是否有备份生成中（生成中不宜再开一个）。 */
  packing: boolean;
  /**
   * 是否存在待重启生效的恢复。
   *
   * 为真时两个导入按钮转橙色并可点开说明：此刻任何导入都会被后端 409 拒绝，
   * 而原因（"有一份恢复等着重启"）此前只藏在可能已滚出视野的事件行徽章里。
   */
  pendingRestart?: boolean;
  /** 点开"待重启"说明的入口（由页面打开说明弹窗）。 */
  onExplainPendingRestart?: () => void;
}

export function ActionBar({
  frozen,
  freezeUntil,
  onCreate,
  onFreeze,
  onUnfreeze,
  onOpenAutoConfig,
  onToggleChannel,
  activeChannel,
  uploadBusy,
  packing,
  pendingRestart,
  onExplainPendingRestart,
}: ActionBarProps) {
  const { t } = useTranslation();
  /** 点已展开的那个通道 = 收起；点另一个 = 切过去。 */
  const toggle = (next: ImportChannel) => onToggleChannel(activeChannel === next ? null : next);
  /** 待重启期间：不展开表单（展开了提交也必被拒），改为解释原因与给出重启命令。 */
  const explainPendingRestart = () => onExplainPendingRestart?.();

  return (
    // data-testid 沿用 backup-actions：测试一直靠它把「同名但不同位置」的按钮
    // （区块头的「生成备份包」与内联区的「从 URL 导入」）区分开，改名只会让守卫无谓地漂。
    <Group gap="xs" wrap="wrap" data-testid="backup-actions">
      {/* 生成中禁用：后端同一时刻只跑一个生成槽位，撞车的请求会被跳过（不算失败），
          但那会表现为"点了没反应"，不如直接禁用并说明。 */}
      <Button
        size="xs"
        leftSection={<IconArchive size={14} />}
        disabled={packing}
        onClick={onCreate}
      >
        {t("backups.create")}
      </Button>

      {/* 冻结 / 解冻：冻结态用橙色变体与「解冻写入」文案表明当前状态。
          解冻入口必须常驻可达——若只在冻结弹窗里才给解冻，冻结期间按钮禁用会形成
          「已冻结 → 打不开弹窗 → 永远解不了冻」的死锁（该坑此前踩过，见 WriteFreezeModal）。 */}
      {frozen ? (
        <Button
          size="xs"
          variant="light"
          color="orange"
          leftSection={<IconLockOpen size={14} />}
          onClick={onUnfreeze}
          title={
            // 用 formatStamp 而非原样输出 ISO：标题气泡是给人读的，裸时间戳没有意义。
            freezeUntil
              ? `${t("backups.freezeUntilLabel")} ${formatStamp(freezeUntil)}`
              : t("backups.freezeStatusFrozen")
          }
        >
          {t("backups.freezeUnfreezeButton")}
        </Button>
      ) : (
        <Button size="xs" variant="default" leftSection={<IconLock size={14} />} onClick={onFreeze}>
          {t("backups.freezeFreezeButton")}
        </Button>
      )}

      {/* 每张卡曾各有**一个**主按钮（备份卡=生成备份包、搬迁卡=从 URL 导入），
          收成一行后这个层级差异靠 variant 保留：主路径 filled，其余 default——
          否则一排同款式按钮又看不出该先按哪个。
          点已展开的通道按钮 = 收起，转 light 表明它正展开着。

          待重启生效（pendingRestart）：两个导入按钮转橙色并带角标。**不禁用**——
          禁用只告诉用户"不能按"，却不说为什么；点了会弹出说明与重启命令，
          那才是他此刻需要的东西。见下方 explainPendingRestart。 */}
      <Button
        size="xs"
        variant={pendingRestart ? "light" : activeChannel === "url" ? "light" : "filled"}
        color={pendingRestart ? "orange" : undefined}
        leftSection={<IconWorldDownload size={14} />}
        disabled={uploadBusy}
        onClick={() => (pendingRestart ? explainPendingRestart() : toggle("url"))}
        data-testid="action-import-url"
        title={pendingRestart ? t("backups.pendingRestartBadge") : undefined}
      >
        {t("backups.importButton")}
      </Button>
      <Button
        size="xs"
        variant={activeChannel === "upload" ? "light" : "default"}
        leftSection={<IconUpload size={14} />}
        disabled={uploadBusy}
        onClick={() => toggle("upload")}
        data-testid="action-import-upload"
      >
        {t("backups.uploadButton")}
      </Button>
      {/* 导入未管理的包：与上面两个通道一样不写 restore.pending，
          因此待重启生效时也允许打开——它只是把包纳入管理，不触碰本机数据。 */}
      <Button
        size="xs"
        variant={activeChannel === "adopt" ? "light" : "default"}
        leftSection={<IconDatabaseImport size={14} />}
        disabled={uploadBusy}
        onClick={() => toggle("adopt")}
        data-testid="action-import-adopt"
      >
        {t("backups.adoptButton")}
      </Button>
      <Button
        size="xs"
        variant="default"
        leftSection={<IconSettings size={14} />}
        onClick={onOpenAutoConfig}
      >
        {t("backups.autoCfgSettingsButton")}
      </Button>

      {/* 说明文字不再常驻：卡里那三行（含"冻结后打的包在时间点上一致"这类理由）
          挂到气泡上，需要时才看。这里刻意复用原来那几张卡的文案键（startTitle /
          startHint / moveTitle / moveHint / freezeStatus*），而不是另写一套新文案——
          否则旧键变成零引用、说明内容等于被悄悄删掉。 */}
      <OpsHelpButton
        title={t("backups.actionBarHelpTitle")}
        items={[
          {
            label: t("backups.startTitle"),
            value: frozen ? t("backups.startHintFrozen") : t("backups.startHint"),
          },
          {
            label: t("backups.moveTitle"),
            value: t("backups.moveHint"),
          },
          {
            // 冻结状态在气泡里也如实可读，不只在按钮颜色上。
            label: t("backups.freezeFreezeButton"),
            value: frozen ? t("backups.freezeStatusFrozen") : t("backups.freezeStatusWritable"),
          },
          // 三个"检查类"动作的分工：它们都在事件表的「⋯」菜单里，只有几个字的标签，
          // 用户看不出彼此的区别与代价。实测反馈"校验和深度校验有什么用、为什么不写提示"，
          // 故在说明气泡里集中讲清：三者是递进关系，代价也逐级上升。
          {
            label: t("backups.verify"),
            value: t("backups.verifyHint"),
          },
          {
            label: t("backups.verifyDeep"),
            value: t("backups.verifyDeepHint"),
          },
          {
            label: t("backups.drillOpen"),
            value: t("backups.drillOpenHint"),
          },
          {
            // 「用这个包恢复」是本次新增的恢复入口，也要在这里出现——
            // 否则用户只知道有另外三条"从外面弄包进来"的通道，仍会以为本机包没法用。
            label: t("backups.restoreFromPackage"),
            value: t("backups.restoreHint"),
          },
        ]}
      />
    </Group>
  );
}
