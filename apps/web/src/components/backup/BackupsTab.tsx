// 备份与搬迁 Tab —— 任务台信息架构（FR-40 追加四聚合 + 追加五动作收编）。
//
// 页面已收敛为**两段**，操作入口不再占据页面顶部，而是挂在事件表区块的标题行上：
//   ① 内联导入区  —— 两条导入通道就地展开（收起时 display:none，仍常驻挂载）
//   ② 关键事实    —— 最近成功备份、下次自动备份、最近校验、占用、保留策略
//   ③ 统一事件表  —— 本机备份包与导入记录按事件时间混排，独占剩余高度；
//                    动作条（生成/冻结/导入/上传/设置/说明）挂在这一区块的标题行里
//
// 为什么操作入口放标题行（追加五）：用户复核时提出「顶上两块太突兀了 需要收纳成按钮放到
// 备份与导入事件」——那两张常驻动作卡合计约 173px，而它们承载的只是几个按钮加三行说明。
// 收成一行后省下的高度全部给事件表，且操作与它影响的那张表在同一个视觉块里。
//
// 为什么包与导入记录合成一张表（追加四）：
//   - 它们回答的是同一个问题——"这台机器上的备份与搬迁，先后发生过什么"。拆成"页面里的
//     一张表 + 弹窗里的一个标签页"只会逼用户来回对照；混排后一次搬迁的时间线
//     （打一份包 → 传到那边 → 那边导入 → 待重启生效）能在一屏里顺着读下来。
//   - 两类对象**不相交**，混排不会出现"同一件事两行"：本机包登记在 backup_package 表，
//     导入记录的 packageId 是包内携带的**源机器**标识，不会进入本机备份包列表。
//   - 弹窗与待处理条一并删除：记录已经常驻可见，"要重启才生效"这类信息不再需要靠一条
//     汇总提示把用户引开，直接在对应行的状态单元格里给结论。
//
// 布局仍是项目约定的视口锁定式：① 不参与伸缩（flexShrink:0），只有 ③ 吃剩余高度、
// 内部滚动。因此展开导入表单不会挤压列表——它属于 ①，展开时列表整体下移、页根出滚动条。
import { Box, Button, Code, CopyButton, Group, Modal, Stack, Text } from "@mantine/core";
import { EmptyState } from "@jianartifact/ui";
import { IconCopy, IconDownload } from "@tabler/icons-react";
import { useCallback, useEffect, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";

import {
  createBackupLink,
  deleteBackup,
  getBackupRestoreStatus,
  getSettings,
  getWriteFreeze,
  listBackupImports,
  listBackups,
  listBackupVerifications,
  unfreezeWrites,
} from "../../api/endpoints";
import type { BackupImportList, BackupPackage, BackupVerificationReport } from "../../api/types";
import { AsyncBoundary, ContentSkeleton } from "../AsyncBoundary";
import { OpsSection } from "../ops/OpsKit";
import { confirmDanger, notifyError, notifySuccess } from "../../lib/feedback";
import { formatCount, formatStamp } from "../../lib/format";
import { ApiError } from "../../api/client";
import { useAsync, REFRESH_EVENT, importIdFromRefreshEvent } from "../../hooks/useAsync";
import { ActionBar } from "./ActionBar";
import { AutoBackupConfigModal } from "./AutoBackupConfigModal";
import { BackupCreateModal } from "./BackupCreateModal";
import { BackupEventTable } from "./BackupEventTable";
import { DrillReportModal } from "./DrillReportModal";
import { FactsStrip } from "./FactsStrip";
import { ImportInline, type ImportChannel } from "./ImportInline";
import { RestartControl } from "./RestartControl";
import { BackupPreviewModal } from "./BackupPreviewModal";
import { RestoreFromPackageModal } from "./RestoreFromPackageModal";
import { RestoreProgressModal } from "./RestoreProgressModal";
import { VerifyModal, type VerifyRequest } from "./VerifyModal";
import { WriteFreezeModal } from "./WriteFreezeModal";
import { backupInProgress, importInProgress, useAutoBackupSummary } from "./status";

const POLL_INTERVAL_MS = 1500;

/**
 * 自动备份在跑时的列表刷新间隔。
 *
 * 比生成间隔从容（默认 30s，但可配），又足以让被淘汰的陈旧行在用户动手前消失。
 * 取 10s 是权衡：更密会白打接口（列表变化很慢），更疏则用户可能先点到已消失的行。
 */
const AUTO_LIST_REFRESH_MS = 10_000;

// 待重启说明弹窗里的「可复制命令」兜底值。
//
// **不能硬编码一条命令**（实测踩到）：命令随部署形态而变，而后端会告诉我们哪条对应本机。
// 本机实测的现场：仓库自带单元含 `NoNewPrivileges=true`，`sudo systemctl restart` 直接失败
// （sudo 被该 flag 拦住），该形态下应给 `systemctl --user restart`。
// 硬编码 `sudo …` 会让界面同时显示两条命令，其中一条照做必失败——比不给更糟。
//
// （服务端的自动重启走的是进程自替换，不执行任何命令；这个常量只用于"手工重启"文案。）
//
// 故真实取值来自 `RestartControl` 的只读探测（`getRestartCapability`，不触发重启）；
// 这个常量只在探测尚未返回（或探测失败）时短暂占位，且写成**通用形态**而非某台机器的答案。
const RESTART_COMMAND_FALLBACK = "sudo systemctl restart <unit>";

export function BackupsTab() {
  const { t } = useTranslation();
  const [createOpened, setCreateOpened] = useState(false);
  const [freezeOpened, setFreezeOpened] = useState(false);
  const [autoCfgOpened, setAutoCfgOpened] = useState(false);
  /** 校验弹窗目标（packageId + 深度）；null 表示关闭。 */
  const [verifyReq, setVerifyReq] = useState<VerifyRequest | null>(null);
  /** 后端探测到的本机可用重启命令；null 表示尚未返回（显示通用兜底文案）。 */
  const [restartCommand, setRestartCommand] = useState<string | null>(null);
  /** 恢复进度弹窗观察的导入记录；四条恢复通道共用。null 表示关闭。 */
  const [progressId, setProgressId] = useState<string | null>(null);
  const [linkValue, setLinkValue] = useState<string | null>(null);
  /** 演练报告弹窗当前作用的包；null 表示关闭。 */
  const [drillPackageId, setDrillPackageId] = useState<string | null>(null);
  /** 待恢复的包（「用这个包恢复」的确认弹窗）；null 表示关闭。 */
  const [restorePkg, setRestorePkg] = useState<BackupPackage | null>(null);
  // 预览的包标识（FR-40 追加十一）：null = 弹窗关闭。
  const [previewPkg, setPreviewPkg] = useState<string | null>(null);
  /** 内联导入区当前展开的通道；null 表示收起。 */
  const [channel, setChannel] = useState<ImportChannel | null>(null);
  /**
   * 上传进行中。必须由页级持有并同时管住**两条收起路径**：
   *   - 内联区自己的「收起」按钮；
   *   - 动作条上那两个通道按钮（点已展开的那个会收起）。
   * 只在 ImportInline 内部记 busy 是不够的：那样动作条仍能把面板收起来，
   * 而进度条与取消按钮都在面板里——收起来等于把唯一的进度反馈藏掉。
   */
  const [uploadBusy, setUploadBusy] = useState(false);

  /**
   * 切换/收起导入通道。
   *
   * 这里的 `uploadBusy` 早退是**第二层**保险：UI 层两条路径的按钮都已在忙碌时 disabled
   * （那才是实际生效、且被用例守住的那层）。保留它是为了让"上传中不可收起"这条不变量
   * 由状态持有者本人强制，而不是只依赖下游组件记得置灰——将来若有人去掉某个 disabled，
   * 这里仍然拦得住。注意：正因如此它无法被用例单独覆盖（移除它不会让任何用例转红），
   * 这是有意的防御性冗余，不是遗漏的测试。
   */
  const onChannelChange = (next: ImportChannel | null) => {
    if (uploadBusy) return;
    setChannel(next);
  };

  const state = useAsync(() => listBackups({ page: 1, page_size: 100 }), [], {
    cacheKey: "backups:list",
  });
  const reload = state.reload;

  const items = state.data?.items ?? [];
  const hasRunning = useMemo(() => items.some((item) => backupInProgress(item.status)), [items]);

  // 冻结状态：动作条要用它，故在页级取一次并透传（避免两处各取一份出现错位）。
  const freezeState = useAsync(() => getWriteFreeze(), [], { cacheKey: "backups:freeze" });
  const frozen = freezeState.data?.frozen ?? false;

  // 自动备份间隔：只读 settings，用于推算"下次自动备份"。
  // 注意不用 maintenance job 的 intervalSeconds——那是心跳粒度而非用户配置的备份间隔。
  const settingsState = useAsync(() => getSettings(), [], { cacheKey: "backup:auto-config" });
  const autoInterval = settingsState.data ? settingsState.data.backupAutoInterval : null;
  // 保留策略摘要复用 status.ts 的同一实现（0 的语义在那里统一处理），不在这里重写一遍。
  const autoSummary = useAutoBackupSummary(settingsState.data);

  // 待重启生效的恢复：为真时任何导入都会被 409 拒绝，必须让用户看得出原因。
  // 它的生命周期由重启驱动（重启前一直是 true），故随导入记录一起刷新即可。
  const restoreState = useAsync(() => getBackupRestoreStatus(), [], {
    cacheKey: "backups:restore-status",
  });
  const pendingRestart = restoreState.data?.pending ?? false;
  // 说明弹窗的开关。刻意用弹窗而不是常驻区块：这个状态是**临时**的（重启即消失），
  // 常驻一块会给绝大多数时候（无待重启）白占高度——用户先前正是因此删掉了待处理提示条。
  const [pendingInfoOpened, setPendingInfoOpened] = useState(false);

  // 导入记录：与统一事件表用同一份数据（同样的 cacheKey 在途去重，不会打两份）。
  const importsState = useAsync<BackupImportList>(
    () => listBackupImports({ page: 1, page_size: 100 }),
    [],
    { cacheKey: "backups:imports" },
  );
  const imports = importsState.data?.items ?? [];
  const importsRunning = useMemo(
    () => imports.some((it) => importInProgress(it.status)),
    [imports],
  );

  // 最近校验：对最近一个已完成包读它的落库演练报告。
  // 只取一个包、只读一页——没有全局校验端点，扫全部包会是 N+1 请求；
  // 而"最近校验"这个字段本就是要看最新的那次，取最近包足够且诚实。
  const lastDoneId = useMemo(() => {
    const best = items
      .filter((it) => it.status === "done")
      .reduce<BackupPackage | null>(
        (acc, it) => (!acc || it.createdAt > acc.createdAt ? it : acc),
        null,
      );
    return best?.packageId ?? null;
  }, [items]);
  const [lastVerification, setLastVerification] = useState<BackupVerificationReport | null>(null);
  const [verificationLoading, setVerificationLoading] = useState(false);

  useEffect(() => {
    if (!lastDoneId) {
      setLastVerification(null);
      return;
    }
    let active = true;
    setVerificationLoading(true);
    listBackupVerifications(lastDoneId, { page: 1, page_size: 1 })
      .then((res) => {
        if (!active) return;
        setLastVerification(res.items[0] ?? null);
      })
      .catch(() => {
        // 读不到报告不该让整页失败：如实退化为"尚未校验"。
        if (active) setLastVerification(null);
      })
      .finally(() => {
        if (active) setVerificationLoading(false);
      });
    return () => {
      active = false;
    };
  }, [lastDoneId]);

  // 生成中的包由服务端异步推进，这里按固定间隔刷新列表直到全部进入终态。
  // 自动备份在跑时也按较慢节奏刷新（见下方 autoBackupActive 的说明）。
  useEffect(() => {
    if (!hasRunning) {
      return;
    }
    const timer = window.setInterval(reload, POLL_INTERVAL_MS);
    return () => window.clearInterval(timer);
  }, [hasRunning, reload]);

  /**
   * 自动备份在跑时，列表也要按**较慢**节奏刷新。
   *
   * 为什么不能只靠上面的 hasRunning：自动备份每 30 秒就生成一个包、同时淘汰旧的，
   * 但**新增与淘汰都是终态**（done / 直接消失），不会让 hasRunning 变真。于是页面开着
   * 不动时列表永远不更新——旧行一直指着已被淘汰的包，用户点「获取链接 / 校验 / 演练」
   * 就会拿到 404，看着像"功能坏了"（实测收到的反馈正是如此）。
   *
   * 节奏取 10 秒：比生成间隔（默认可配）从容，又足以让陈旧行在用户动手前就消失。
   * 只在**确有自动备份**时开轮询（enabledAuto 由间隔配置决定），不手动打接口。
   */
  const autoBackupActive = (autoInterval ?? 0) > 0;
  useEffect(() => {
    if (!autoBackupActive || hasRunning) {
      return; // hasRunning 时已由上面的快轮询覆盖，不叠加两个定时器
    }
    const timer = window.setInterval(reload, AUTO_LIST_REFRESH_MS);
    return () => window.clearInterval(timer);
  }, [autoBackupActive, hasRunning, reload]);

  // 导入记录同理：存在非终态记录（queued/fetching/staging）时持续轮询，全部终态后停止。
  // 合并成一张表之后，这一处的轮询同时驱动了事件表的进度列与状态徽章。
  const reloadImports = importsState.reload;
  useEffect(() => {
    if (!importsRunning) {
      return;
    }
    const timer = window.setInterval(reloadImports, POLL_INTERVAL_MS);
    return () => window.clearInterval(timer);
  }, [importsRunning, reloadImports]);

  /**
   * 受理导入后自动打开恢复进度弹窗。
   *
   * 为什么在页面级监听而不是让各通道自己回调：四条恢复通道（URL / 上传 / 本机包 / CLI）
   * 都要有进度反馈，而**CLI 通道根本没有前端调用点**——它由运维在服务器上执行，
   * 页面唯一能察觉的方式就是"导入记录列表里多了一条"。监听全局刷新事件（并取其中的
   * importId）能让前三条通道即时弹窗，CLI 那条则退化为列表里可见的一行——
   * 这已经是不改后端所能做到的最好覆盖。
   */
  useEffect(() => {
    const handler = (event: Event) => {
      const importId = importIdFromRefreshEvent(event);
      if (importId) setProgressId(importId);
    };
    window.addEventListener(REFRESH_EVENT, handler);
    return () => window.removeEventListener(REFRESH_EVENT, handler);
  }, []);

  /**
   * 陈旧行的统一处置：列表里那一行指向的包已经不存在了。
   *
   * 为什么会陈旧：自动备份每 30 秒生成新包并淘汰旧包，而**这些变化不会让任何记录进入
   * 非终态**，所以列表此前不会自动刷新；用户点到的可能是已消失的包，后端返 404。
   * 裸报"备份包不存在"让人无从下手——用户看不出那是"页面数据旧了"而不是"功能坏了"
   * （实测反馈「没有复制链接 现在会失败了」正是这个）。
   *
   * 处置顺序：先刷新列表（让陈旧行消失），再提示原因。只提示不刷新的话，
   * 用户很容易再点一次同一行、再失败一次。
   */
  const handleStaleRow = useCallback(
    (err: unknown, action: string): boolean => {
      if (!(err instanceof ApiError) || err.status !== 404) {
        return false;
      }
      notifyError(new Error(t("backups.staleRow", { action })));
      reload();
      // 包没了，与它相关的弹窗已无意义：关掉，避免继续对着不存在的包操作。
      setDrillPackageId(null);
      setRestorePkg(null);
      setLinkValue(null);
      setVerifyReq(null);
      return true;
    },
    [reload, t],
  );

  const onGetLink = async (packageId: string) => {
    try {
      const link = await createBackupLink(packageId);
      setLinkValue(link.url);
    } catch (err) {
      if (handleStaleRow(err, t("backups.actionGetLink"))) return;
      notifyError(err);
    }
  };

  /**
   * 校验 / 深度校验：打开弹窗，由它自己发请求与展示结论。
   *
   * 此前是"await 一下 + toast"——用户看不到在查什么、查到哪、结论落在哪，
   * 深校验动辄几分钟更是全程无反馈。弹窗负责全过程反馈（见 VerifyModal 文件头），
   * 这里只保留行内 loading 与 404 处置。
   */
  const onVerify = (packageId: string, deep: boolean) => {
    setVerifyReq({ packageId, deep });
  };

  const onDelete = (item: BackupPackage) => {
    confirmDanger({
      title: t("backups.deleteTitle"),
      message: t("backups.deleteConfirm", { id: item.packageId }),
      confirmLabel: t("backups.delete"),
      cancelLabel: t("common.cancel"),
      onConfirm: () => {
        void deleteBackup(item.packageId)
          .then(() => {
            notifySuccess(t("backups.delete"));
            reload();
          })
          .catch((err) => {
            // 删一个已经不在的包同样是"这一行过期了"，不是删除失败本身。
            if (handleStaleRow(err, t("backups.actionDelete"))) return;
            notifyError(err);
          });
      },
    });
  };

  const onUnfreeze = useCallback(() => {
    confirmDanger({
      title: t("backups.freezeUnfreezeTitle"),
      message: t("backups.freezeUnfreezeMessage"),
      confirmLabel: t("backups.freezeUnfreezeLabel"),
      cancelLabel: t("common.cancel"),
      onConfirm: () => {
        void unfreezeWrites()
          .then(() => {
            notifySuccess(t("backups.freezeUnfrozen"));
            freezeState.reload();
          })
          .catch(notifyError);
      },
    });
    // freezeState.reload 是 useCallback 稳定引用，但 freezeState 对象每渲染都是新的；
    // 依赖整个对象会让这个回调每次重建，故只依赖真正用到的 reload。
  }, [freezeState.reload, t]);

  const packing = items.some((it) => backupInProgress(it.status));

  return (
    // 外层可滚动：①② 都是 flexShrink:0 的固定内容，矮视口下它们会把事件表挤到只剩几像素，
    // 那时"列表不可用"比"整页可滚"更糟，故允许整页滚动兜底，并给列表一个 minHeight
    // 让它在任何视口下都至少露出一行半。展开导入表单同样靠它兜底（列表整体下移）。
    <Stack
      gap={12}
      data-testid="backups-tab"
      style={{ height: "100%", minHeight: 0, overflowY: "auto", overflowX: "hidden" }}
    >
      {/* 操作入口不再是页面顶部的一块：动作条收成标题行右侧的一排按钮，
          挂在下面 ③ 的 actions 里（见 ActionBar 文件头——原先两张常驻卡合计约 173px，
          而它们承载的只是几个按钮加三行说明）。 */}

      {/* ① 内联导入表单。常驻挂载、按需显示——见 ImportInline 文件头（续传探测前提）。 */}
      <Box style={{ flexShrink: 0 }}>
        <ImportInline
          channel={channel}
          onChannelChange={onChannelChange}
          onBusyChange={setUploadBusy}
        />
      </Box>

      {/* ② 关键事实 */}
      <Box style={{ flexShrink: 0 }}>
        <FactsStrip
          items={items}
          autoIntervalSeconds={autoInterval}
          autoSummary={autoSummary}
          lastVerification={lastVerification}
          verificationLoading={verificationLoading}
        />
      </Box>

      {/* ③ 统一事件表 + 动作条：备份包与导入记录按事件时间混排，独占剩余高度 */}
      <OpsSection
        title={t("backups.eventsSection")}
        meta={
          // 总数是两类之和：表里混排的就是它们。任一侧尚未到达时不显示——
          // 显示一个"只算了一部分"的数字比不显示更容易被当成真实总数。
          state.data && importsState.data
            ? formatCount(state.data.total + importsState.data.total)
            : undefined
        }
        testId="backup-list-section"
        style={{
          flex: 1,
          // 地板值：矮视口下不许被压成零高度（实测 880×622 下曾塌到 2px，表格完全不可用）。
          // 表头 46px + 一行 62px ≈ 108px 才至少露出一行；取 150px 略留余量，
          // 不足时由外层 Stack 的 overflowY:auto 兜底出滚动条。
          minHeight: 150,
          display: "flex",
          flexDirection: "column",
          overflow: "hidden",
        }}
        bodyStyle={{ flex: 1, minHeight: 0, overflow: "auto" }}
        bodyPadding={0}
        actions={
          // 动作条整体挂在这里：标题行右侧原本只有一个「生成备份包」，现在五个按钮
          // 同排——页面上不再有独立的动作卡（省下约 173px 常驻高度，全部让给事件表）。
          <ActionBar
            frozen={frozen}
            freezeUntil={freezeState.data?.until ?? null}
            onCreate={() => setCreateOpened(true)}
            onFreeze={() => setFreezeOpened(true)}
            onUnfreeze={onUnfreeze}
            onOpenAutoConfig={() => setAutoCfgOpened(true)}
            onToggleChannel={onChannelChange}
            activeChannel={channel}
            uploadBusy={uploadBusy}
            packing={packing}
            pendingRestart={pendingRestart}
            onExplainPendingRestart={() => setPendingInfoOpened(true)}
          />
        }
      >
        {/* 两张表的数据可分别到达；任一到场即可渲染已有行，不必互等（否则慢的那张
            会把整块表按在骨架屏上）。两边的首载骨架由 AsyncBoundary 各自表达。 */}
        <AsyncBoundary state={state}>
          {(data) => {
            const rows = data.items.length + imports.length;
            // 两边都到齐且都是空，才是真的没有事件。仅一侧为空时不能报空——
            // 那会把"另一边还没加载出来"说成"什么都没有"。
            if (rows === 0) {
              if (importsState.loading) return <ContentSkeleton rows={3} />;
              return (
                <Box p="xl">
                  <EmptyState
                    message={t("backups.eventsEmpty")}
                    description={t("backups.eventsEmptyHint")}
                  />
                </Box>
              );
            }
            return (
              <BackupEventTable
                items={data.items}
                imports={imports}
                verifyingId={verifyReq?.packageId ?? null}
                onGetLink={(id) => void onGetLink(id)}
                onVerify={(id, deep) => void onVerify(id, deep)}
                onDrill={setDrillPackageId}
                onPreview={setPreviewPkg}
                onRestore={setRestorePkg}
                onDelete={onDelete}
              />
            );
          }}
        </AsyncBoundary>
      </OpsSection>

      <BackupCreateModal
        opened={createOpened}
        onClose={() => setCreateOpened(false)}
        onCreated={reload}
      />

      {/* 演练只读路径会产出报告；404（包已删）场景下弹窗的「刷新」用它回读列表。 */}
      <DrillReportModal
        packageId={drillPackageId}
        onClose={() => setDrillPackageId(null)}
        onRefresh={reload}
      />

      {/* 冻结写入：动作条与事件表都要用同一份冻结状态，故在页级取一次并透传给弹窗。 */}
      <WriteFreezeModal
        opened={freezeOpened}
        onClose={() => setFreezeOpened(false)}
        state={freezeState}
      />

      {/* 包内容预览（FR-40 追加十一）：恢复前先看清包里有什么。
          放在恢复弹窗之前，与菜单里的顺序一致（先看内容，再决定是否恢复）。 */}
      <BackupPreviewModal packageId={previewPkg} onClose={() => setPreviewPkg(null)} />

      {/* 用本机包恢复：受理后把 importId 交给进度弹窗，并刷新列表
          （事件表里会出现一条 origin=local 的导入记录）。 */}
      <RestoreFromPackageModal
        pkg={restorePkg}
        onClose={() => setRestorePkg(null)}
        onAccepted={(importId) => {
          setProgressId(importId);
          reload();
        }}
        onFreeze={() => setFreezeOpened(true)}
        frozen={frozen}
        onStale={(err) => handleStaleRow(err, t("backups.actionRestore"))}
      />

      {/* 恢复进度（问题 1）：四条通道共用一个弹窗——它们都是同一种导入记录，差别只在 origin。
          「立即重启」作为收尾动作内嵌在弹窗里：到达 pending_restart 后，重启才是真正的最后一步，
          让用户去别处找入口等于把流程断在最后一米。 */}
      <RestoreProgressModal
        importId={progressId}
        onClose={() => setProgressId(null)}
        onSettled={reloadImports}
        onFreeze={() => setFreezeOpened(true)}
        frozen={frozen}
        restartSlot={
          <RestartControl
            onRestarted={() => {
              // 服务回来后**顺手关掉进度弹窗**：这次恢复已经落地，弹窗再无话可说。
              //
              // 不关会留下一个自相矛盾的界面（实测踩到）：恢复生效意味着整个数据库被替换，
              // 那条导入记录随之消失，弹窗随后的轮询会 404——于是它一边显示"待重启"、
              // 一边报读不到记录，而此刻数据其实已经替换成功。收尾就该收得干净。
              setProgressId(null);
              reload();
            }}
          />
        }
      />

      {/* 校验 / 深度校验（问题 4）：同步接口没有中间进度，故列检查项 + 就地给结论，
          不画假百分比（见 VerifyModal 文件头）。 */}
      <VerifyModal
        request={verifyReq}
        onClose={() => setVerifyReq(null)}
        onStale={(err) => handleStaleRow(err, t("backups.actionVerify"))}
        onSettled={() => {
          // 结论会改变「最近校验」，顺手重拉（沿用原先 verify 成功后的口径）。
          if (lastDoneId) {
            listBackupVerifications(lastDoneId, { page: 1, page_size: 1 })
              .then((res) => setLastVerification(res.items[0] ?? null))
              .catch(() => undefined);
          }
        }}
      />

      {/* 自动备份与保留策略：同样透传页级取到的 settings，避免弹窗内再取一份出现摘要错位。 */}
      <AutoBackupConfigModal
        opened={autoCfgOpened}
        onClose={() => setAutoCfgOpened(false)}
        state={settingsState}
      />

      {/* 待重启生效的说明：导入按钮在待重启期间转橙色并指向这里。
          为什么是弹窗而不是常驻条：该状态是临时的（重启即消失），常驻会白占高度——
          用户先前正是以"这个汇总提示不要"为由删掉了待处理提示条。 */}
      <Modal
        opened={pendingInfoOpened}
        onClose={() => setPendingInfoOpened(false)}
        title={t("backups.pendingRestartTitle")}
        centered
      >
        <Stack gap="sm">
          <Text size="sm">{t("backups.pendingRestartBody")}</Text>
          {restoreState.data?.packageId ? (
            <Text size="xs" c="dimmed">
              {t("backups.pendingRestartPackage")}：<Code>{restoreState.data.packageId}</Code>
            </Text>
          ) : null}
          {restoreState.data?.stagedAt ? (
            <Text size="xs" c="dimmed">
              {t("backups.pendingRestartStagedAt")}：{formatStamp(restoreState.data.stagedAt)}
            </Text>
          ) : null}
          {/* 一键重启（若部署形态支持）排在最前：它比复制命令少一步，且做完就会探活刷新。
              这个入口必须在这里也有一份——进度弹窗只在"刚受理完"那一刻开着，而待重启状态
              会跨页面刷新一直存在（`restore.pending` 不清就一直挡住后续恢复）。
              用户重新打开页面、从橙色导入按钮进到这里时，若只看到一条要自己复制的命令，
              等于这个功能在最需要它的场景下消失了。 */}
          <RestartControl onRestarted={reload} onCommandResolved={setRestartCommand} />
          <Text size="xs" fw={600}>
            {t("backups.pendingRestartCommandLabel")}
          </Text>
          {/* 命令来自后端的只读探测（见 RESTART_COMMAND_FALLBACK 的说明），
              因此它就是**本机真正可执行**的那一条，而不是某台机器上的猜测。 */}
          <Group gap="xs" wrap="nowrap">
            <Code style={{ flex: 1 }}>{restartCommand ?? RESTART_COMMAND_FALLBACK}</Code>
            <CopyButton value={restartCommand ?? RESTART_COMMAND_FALLBACK}>
              {({ copied, copy }) => (
                <Button
                  size="xs"
                  variant="default"
                  leftSection={<IconCopy size={14} />}
                  onClick={() => {
                    copy();
                    notifySuccess(t("backups.linkCopied"));
                  }}
                >
                  {copied ? t("backups.linkCopied") : t("backups.copyCommand")}
                </Button>
              )}
            </CopyButton>
          </Group>
          <Text size="xs" c="dimmed">
            {t("backups.pendingRestartAfterRestart")}
          </Text>
        </Stack>
      </Modal>

      <Modal
        opened={linkValue !== null}
        onClose={() => setLinkValue(null)}
        title={t("backups.linkTitle")}
        centered
      >
        <Stack gap="sm">
          <Text size="xs" c="dimmed">
            {t("backups.linkHint")}
          </Text>
          {linkValue ? <Code block>{linkValue}</Code> : null}
          <Group justify="flex-end" gap="xs">
            {linkValue ? (
              <>
                <CopyButton value={linkValue}>
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
                <Button component="a" href={linkValue} leftSection={<IconDownload size={16} />}>
                  {t("backups.download")}
                </Button>
              </>
            ) : null}
          </Group>
        </Stack>
      </Modal>
    </Stack>
  );
}
