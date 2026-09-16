// 分片上传卡（FR-137 第三通道）：浏览器内传 GB 级备份包。
// 流程：选文件 → init（取服务端 chunkSize）→ 按 File.slice(chunkSize) 逐片 PUT → 进度条 → complete 触发导入。
// 支持取消（abort，二次确认）与续传（uploadId 持久化到 localStorage；重开页面时 GET 拉回 uploadedChunks，只补缺失片）。
// 与从 URL 导入卡平级：本卡只负责上传进度，complete 后派发全局刷新让导入记录列表呈现 pending_restart。
import { Alert, Box, Button, Group, Progress, Stack, Text } from "@mantine/core";
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
import { REFRESH_EVENT } from "../../hooks/useAsync";
import { OpsSection } from "../ops/OpsKit";

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

type Phase = "idle" | "active" | "resuming" | "finished";

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

export function BackupUploadCard() {
  const { t } = useTranslation();
  const [phase, setPhase] = useState<Phase>("idle");
  const [session, setSession] = useState<BackupUploadSession | null>(null);
  const [uploadedChunks, setUploadedChunks] = useState<number[]>([]);
  const [errorMsg, setErrorMsg] = useState<string | null>(null);
  // 重开页面时发现的未完成会话（含已落盘分片），用于续传提示。
  const [resumable, setResumable] = useState<PersistedUpload | null>(null);

  const fileInputRef = useRef<HTMLInputElement>(null);
  const abortRef = useRef<AbortController | null>(null);
  // 取消后 in-flight 的分片请求会因 signal 中止而 reject，标记后忽略该错误（取消已统一处理）。
  const cancelledRef = useRef(false);

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
        await completeBackupUpload(uploadId);
        clearPersisted();
        setResumable(null);
        setPhase("finished");
        notifySuccess(t("backups.uploadComplete"));
        // 导入记录由平级的从 URL 导入卡渲染，派发全局刷新让其立即轮询到 pending_restart。
        window.dispatchEvent(new CustomEvent(REFRESH_EVENT));
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

  return (
    <OpsSection
      title={t("backups.uploadCardTitle")}
      style={{
        flex: 1,
        minHeight: 0,
        display: "flex",
        flexDirection: "column",
        overflow: "hidden",
      }}
      bodyStyle={{ flex: 1, minHeight: 0, overflow: "auto" }}
      bodyPadding={0}
      actions={
        <Button
          size="xs"
          leftSection={<IconUpload size={14} />}
          onClick={() => fileInputRef.current?.click()}
          disabled={uploading || phase === "finished"}
        >
          {t("backups.uploadButton")}
        </Button>
      }
    >
      <Box p="md">
        <input
          ref={fileInputRef}
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
                  onClick={() => fileInputRef.current?.click()}
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
          <Alert
            color="red"
            title={t("backups.uploadMissingChunkTitle")}
            data-testid="upload-error"
          >
            <Text size="sm">{errorMsg}</Text>
          </Alert>
        ) : null}

        {phase === "idle" && !resumable && !errorMsg ? (
          <Text size="sm" c="dimmed">
            {t("backups.uploadStarted")}
          </Text>
        ) : null}
      </Box>
    </OpsSection>
  );
}
