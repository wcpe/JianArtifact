// 分片上传面板（FR-137 第三通道）：浏览器内传 GB 级备份包。
// 流程：选文件 → init（取服务端 chunkSize）→ 按 File.slice(chunkSize) 逐片 PUT → 进度条 → complete 触发导入。
// 支持取消（abort，二次确认）与续传（uploadId 持久化到 localStorage；重开页面时 GET 拉回 uploadedChunks，只补缺失片）。
//
// 本组件是 body-only：展开容器由 ImportInline 承担，它只负责上传本身。
// 覆盖 / 深校验两个开关也由它自己持有——它们是在 complete 这一刻提交的，摆在上传按钮旁边
// 才不会被误以为属于别的通道。
import { Alert, Box, Button, Checkbox, Group, Progress, Stack, Text } from "@mantine/core";
import { IconArrowRight, IconPlayerStop, IconUpload } from "@tabler/icons-react";
import { useCallback, useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";

import {
  abortBackupUpload,
  completeBackupUpload,
  createBackupUpload,
  getBackupUpload,
  uploadBackupChunk,
} from "../../api/endpoints";
import type { BackupUploadSession } from "../../api/types";
import { ApiError } from "../../api/client";
import { confirmDanger, notifyError, notifySuccess } from "../../lib/feedback";
import { formatBytes } from "../../lib/format";
import { dispatchRefresh } from "../../hooks/useAsync";

// 续传持久化的资源语义键：只存会话元数据（uploadId/文件名/体积/chunkSize），不存文件内容（整包太大）。
const UPLOAD_STORAGE_KEY = "jianartifact.backupUpload";

interface PersistedUpload {
  uploadId: string;
  fileName: string;
  totalBytes: number;
  chunkSize: number;
}

function loadPersisted(): PersistedUpload | null {
  try {
    const raw = localStorage.getItem(UPLOAD_STORAGE_KEY);
    return raw ? (JSON.parse(raw) as PersistedUpload) : null;
  } catch {
    return null;
  }
}

function savePersisted(p: PersistedUpload): void {
  try {
    localStorage.setItem(UPLOAD_STORAGE_KEY, JSON.stringify(p));
  } catch {
    /* 隐私模式等场景下忽略存储失败 */
  }
}

function clearPersisted(): void {
  try {
    localStorage.removeItem(UPLOAD_STORAGE_KEY);
  } catch {
    /* 忽略 */
  }
}

/** 上传阶段：idle 空闲 / active 新会话上传中 / resuming 续传中 / finished 已完成待关闭回执。 */
export type UploadPhase = "idle" | "active" | "resuming" | "finished";

/** 已落盘分片对应的字节数（末片可能不满 chunkSize）。 */
function uploadedBytes(uploadedChunks: number[], chunkSize: number, totalBytes: number): number {
  const totalChunks = Math.ceil(totalBytes / chunkSize);
  if (totalChunks === 0) return 0;
  let bytes = 0;
  for (const i of uploadedChunks) {
    bytes += i === totalChunks - 1 ? totalBytes - (totalChunks - 1) * chunkSize : chunkSize;
  }
  return bytes;
}

export function BackupUploadPanel({
  onBusyChange,
  onResumableChange,
}: {
  /** 上传进行中（active / resuming）：调用方需据此禁止关闭弹窗。 */
  onBusyChange?: (busy: boolean) => void;
  /** 存在未完成会话待续传：调用方需据此保证续传入口可见。 */
  onResumableChange?: (resumable: boolean) => void;
}) {
  const { t } = useTranslation();
  // 覆盖 / 深校验由本组件自持：它们在 complete 那一刻提交，是上传自身的选项。
  // （不做成受控：上一版把它提到弹窗层再回传，多一层状态同步只为让两个开关"看起来在上传之外"，
  //  而用户真正需要的是它们紧挨着上传按钮。）
  const [overwrite, setOverwrite] = useState(false);
  const [deep, setDeep] = useState(false);
  const [phase, setPhase] = useState<UploadPhase>("idle");
  const [session, setSession] = useState<BackupUploadSession | null>(null);
  const [uploadedChunks, setUploadedChunks] = useState<number[]>([]);
  const [errorMsg, setErrorMsg] = useState<string | null>(null);
  // 重开页面时发现的未完成会话（含已落盘分片），用于续传提示。
  const [resumable, setResumable] = useState<PersistedUpload | null>(null);

  // 隐藏 input 由本组件自持：选文件按钮（空闲时）与续传按钮都在组件内，
  // 不再需要外部透过 ref 触发——面板已整体进入 Modal，没有"外层按钮"这回事了。
  const pickRef = useRef<HTMLInputElement | null>(null);
  const abortRef = useRef<AbortController | null>(null);
  // 取消后 in-flight 的分片请求会因 signal 中止而 reject，标记后忽略该错误（取消已统一处理）。
  const cancelledRef = useRef(false);
  // 选项经 ref 持有最新值：completeUpload 是 useCallback，若把选项放进依赖会让上传中途
  // 改变闭包引用；而用户恰恰可能在上传进行中才勾上「覆盖已有数据」。
  const overwriteRef = useRef(overwrite);
  overwriteRef.current = overwrite;
  const deepRef = useRef(deep);
  deepRef.current = deep;

  // 重开页面：若本地留存了未完成会话，拉回会话状态与已落盘分片，只补缺失片。
  useEffect(() => {
    const persisted = loadPersisted();
    if (!persisted) return;
    getBackupUpload(persisted.uploadId)
      .then((s) => {
        if (s.status === "initialized" || s.status === "receiving") {
          setResumable(persisted);
          setSession(s);
          setUploadedChunks(s.uploadedChunks);
        } else {
          // 已完成 / 已中止 / 已清理：无可续传。
          clearPersisted();
        }
      })
      .catch(() => clearPersisted());
  }, []);

  const handleError = useCallback(
    (err: unknown) => {
      const msg = err instanceof ApiError ? err.message : String(err);
      setErrorMsg(msg);
      setPhase("idle");
      notifyError(t("backups.uploadError", { message: msg }));
    },
    [t],
  );

  const completeUpload = useCallback(
    async (uploadId: string) => {
      try {
        // 选项必须在 complete 时提交：契约的 CompleteBackupUploadRequest 才带 overwrite / deep，
        // init 阶段不接受它们。omit 掉 false 值，与 URL 导入通道的写法一致。
        const accepted = await completeBackupUpload(uploadId, {
          overwrite: overwriteRef.current || undefined,
          deep: deepRef.current || undefined,
        });
        clearPersisted();
        setResumable(null);
        setPhase("finished");
        notifySuccess(t("backups.uploadComplete"));
        // 派发全局刷新：导入记录表与备份列表都在监听该事件，
        // 让新记录（pending_restart）与随之生成的包立即呈现，无需用户手动刷新。
        // 带上 importId：页面据此打开恢复进度弹窗，把"已受理"之后的暂存过程也显示出来。
        dispatchRefresh(accepted.importId);
      } catch (err) {
        handleError(err);
      }
    },
    [handleError, t],
  );

  const runUpload = useCallback(
    async (file: File, initial: BackupUploadSession) => {
      const { uploadId, chunkSize, totalBytes } = initial;
      const totalChunks = Math.ceil(totalBytes / chunkSize);
      const have = new Set(initial.uploadedChunks);
      abortRef.current = new AbortController();
      for (let i = 0; i < totalChunks; i += 1) {
        if (have.has(i)) continue; // 续传：已落盘分片跳过。
        const start = i * chunkSize;
        const end = Math.min(totalBytes, start + chunkSize);
        const blob = file.slice(start, end);
        const resp = await uploadBackupChunk(uploadId, i, blob, {
          signal: abortRef.current.signal,
        });
        have.add(i);
        setUploadedChunks([...have].sort((a, b) => a - b));
        setSession(resp);
      }
      // 全部分片就位 → 拼装并触发导入。
      await completeUpload(uploadId);
    },
    [completeUpload],
  );

  const onFileChosen = useCallback(
    async (file: File) => {
      setErrorMsg(null);
      cancelledRef.current = false;
      try {
        let active: BackupUploadSession;
        if (resumable) {
          // 续传：校验文件大小一致后复用既有会话，只补缺失片。
          if (file.size !== resumable.totalBytes) {
            notifyError(t("backups.uploadFileMismatch"));
            return;
          }
          active = await getBackupUpload(resumable.uploadId);
          setPhase("resuming");
        } else {
          active = await createBackupUpload({
            fileName: file.name,
            totalBytes: file.size,
          });
          savePersisted({
            uploadId: active.uploadId,
            fileName: file.name,
            totalBytes: file.size,
            chunkSize: active.chunkSize,
          });
          setPhase("active");
        }
        setSession(active);
        setUploadedChunks(active.uploadedChunks);
        await runUpload(file, active);
      } catch (err) {
        // 取消导致的请求中止已统一在 onCancel 处理，这里不再报错。
        if (cancelledRef.current) {
          return;
        }
        handleError(err);
      }
    },
    [handleError, resumable, runUpload, t],
  );

  const onInputChange = (e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0];
    if (file) void onFileChosen(file);
    // 允许再次选择同一文件。
    e.target.value = "";
  };

  const onCancel = useCallback(() => {
    confirmDanger({
      title: t("backups.uploadCancel"),
      message: t("backups.uploadCancelConfirm"),
      confirmLabel: t("backups.uploadCancel"),
      cancelLabel: t("common.cancel"),
      onConfirm: async () => {
        cancelledRef.current = true;
        abortRef.current?.abort();
        const id = session?.uploadId;
        if (id) {
          try {
            await abortBackupUpload(id);
          } catch {
            /* 即便 abort 失败也按本地已取消处理 */
          }
        }
        clearPersisted();
        setResumable(null);
        setSession(null);
        setUploadedChunks([]);
        setPhase("idle");
        notifySuccess(t("backups.uploadCancelled"));
      },
    });
  }, [session, t]);

  const onDiscard = useCallback(() => {
    confirmDanger({
      title: t("backups.uploadDiscard"),
      message: t("backups.uploadDiscardConfirm"),
      confirmLabel: t("backups.uploadDiscard"),
      cancelLabel: t("common.cancel"),
      onConfirm: async () => {
        const id = resumable?.uploadId;
        if (id) {
          try {
            await abortBackupUpload(id);
          } catch {
            /* 忽略 */
          }
        }
        clearPersisted();
        setResumable(null);
        setSession(null);
        setUploadedChunks([]);
        notifySuccess(t("backups.uploadDiscarded"));
      },
    });
  }, [resumable, t]);

  const onFinishClose = useCallback(() => {
    setSession(null);
    setUploadedChunks([]);
    setErrorMsg(null);
    setPhase("idle");
  }, []);

  const done = session ? uploadedBytes(uploadedChunks, session.chunkSize, session.totalBytes) : 0;
  const percent =
    session && session.totalBytes > 0
      ? Math.min(100, Math.round((done / session.totalBytes) * 100))
      : 0;
  const uploading = phase === "active" || phase === "resuming";

  // 上报回调用 ref 持有最新引用：区块传进来的是内联箭头函数，每次渲染都是新引用，
  // 若把它放进 effect 依赖，就会「上报 → 父 setState → 父重渲染 → 新回调 → effect 重跑」无限循环。
  const onBusyChangeRef = useRef(onBusyChange);
  onBusyChangeRef.current = onBusyChange;
  const onResumableChangeRef = useRef(onResumableChange);
  onResumableChangeRef.current = onResumableChange;

  // 上传进行中：弹窗不能被关掉，否则进度条会被藏住。
  useEffect(() => {
    onBusyChangeRef.current?.(uploading);
  }, [uploading]);

  // 存在未完成会话：入口按钮要给出提示，否则续传提示与「继续上传 / 放弃」都无从发现。
  const resumableFlag = !!resumable;
  useEffect(() => {
    onResumableChangeRef.current?.(resumableFlag);
  }, [resumableFlag]);

  return (
    // 弹窗（Modal.Body）已自带内边距，这里不再叠加，避免双重留白。
    <Box>
      <input
        ref={pickRef}
        type="file"
        data-testid="backup-upload-input"
        style={{ display: "none" }}
        onChange={onInputChange}
      />

      {/* 重开页面发现未完成会话：提示续传（只补缺失片）或放弃。 */}
      {resumable && phase === "idle" ? (
        <Alert
          color="blue"
          title={t("backups.uploadResumableTitle")}
          mb="sm"
          data-testid="upload-resume-callout"
        >
          <Stack gap="xs">
            <Text size="sm">
              {t("backups.uploadResumableHint", { fileName: resumable.fileName })}
            </Text>
            <Group gap="xs">
              <Button
                size="xs"
                onClick={() => pickRef.current?.click()}
                leftSection={<IconArrowRight size={14} />}
              >
                {t("backups.uploadResume")}
              </Button>
              <Button
                size="xs"
                variant="default"
                color="red"
                onClick={onDiscard}
                leftSection={<IconPlayerStop size={14} />}
              >
                {t("backups.uploadDiscard")}
              </Button>
            </Group>
          </Stack>
        </Alert>
      ) : null}

      {uploading ? (
        <Stack gap="xs">
          <Group justify="space-between">
            <Text size="sm" fw={500} data-testid="upload-percent">
              {t("backups.uploadProgress", { percent })}
            </Text>
            <Text size="xs" c="dimmed">
              {t("backups.uploadBytes", {
                done: formatBytes(done),
                total: formatBytes(session?.totalBytes ?? 0),
              })}
            </Text>
          </Group>
          <Progress value={percent} size="md" data-testid="upload-progress" />
          <Group justify="flex-end">
            <Button size="xs" variant="default" color="red" onClick={onCancel}>
              {t("backups.uploadCancel")}
            </Button>
          </Group>
        </Stack>
      ) : null}

      {phase === "finished" && session ? (
        <Alert color="green" title={t("backups.uploadComplete")} data-testid="upload-finished">
          <Group justify="space-between">
            <Text size="sm" c="orange">
              {t("backups.uploadCompleteHint")}
            </Text>
            <Button size="xs" variant="default" onClick={onFinishClose}>
              {t("common.close")}
            </Button>
          </Group>
        </Alert>
      ) : null}

      {errorMsg ? (
        <Alert color="red" title={t("backups.uploadMissingChunkTitle")} data-testid="upload-error">
          <Text size="sm">{errorMsg}</Text>
        </Alert>
      ) : null}

      {/* idle（尚未选文件）时给引导文案**加一个选文件按钮**：面板在弹窗里，
          没有外层按钮可以代它触发文件选择，必须自足。文案本身也不能写成"已开始上传"
          ——那与状态正好相反，会让还没动手的用户以为上传已经在跑了。 */}
      {phase === "idle" && !resumable && !errorMsg ? (
        <Stack gap="sm">
          <Text size="sm" c="dimmed">
            {t("backups.uploadIdleHint")}
          </Text>
          {/* 覆盖 / 深校验就在选文件上方：它们决定 complete 时提交什么选项。
              尤其「覆盖已有数据」——目标实例非空时不开它，导入会在**异步暂存阶段**失败
              （HTTP 已返回 202），用户只会看到"上传成功、随后记录变失败"，无从知道差在哪。 */}
          <Checkbox
            label={t("backups.importOverwrite")}
            description={t("backups.importOverwriteHint")}
            checked={overwrite}
            onChange={(e) => setOverwrite(e.currentTarget.checked)}
            data-testid="upload-overwrite"
          />
          <Checkbox
            label={t("backups.importDeep")}
            description={t("backups.importDeepHint")}
            checked={deep}
            onChange={(e) => setDeep(e.currentTarget.checked)}
            data-testid="upload-deep"
          />
          <Group>
            <Button
              size="xs"
              leftSection={<IconUpload size={14} />}
              onClick={() => pickRef.current?.click()}
            >
              {t("backups.uploadChooseFile")}
            </Button>
          </Group>
        </Stack>
      ) : null}
    </Box>
  );
}
