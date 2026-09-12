// 备份与搬迁 Tab：备份包列表 + 生成 + 取链接 + 校验 + 删除。
// 布局遵循项目约定：外层锁定视口高度，只有列表区滚动，页面不满页下滚；
// 页内不重复渲染大标题（标题由页眉面包屑承担）。
import { Box, Button, Code, CopyButton, Group, Modal, Stack, Text } from "@mantine/core";
import { EmptyState } from "@jianartifact/ui";
import { IconCopy, IconDownload, IconPlus, IconRefresh } from "@tabler/icons-react";
import { useEffect, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";

import { createBackupLink, deleteBackup, listBackups, verifyBackup } from "../../api/endpoints";
import type { BackupPackage } from "../../api/types";
import { AsyncBoundary } from "../AsyncBoundary";
import { OpsSection } from "../ops/OpsKit";
import { confirmDanger, notifyError, notifySuccess } from "../../lib/feedback";
import { formatCount } from "../../lib/format";
import { useAsync } from "../../hooks/useAsync";
import { BackupCreateModal } from "./BackupCreateModal";
import { BackupTable } from "./BackupTable";
import { ImportFromUrlCard } from "./ImportFromUrlCard";
import { WriteFreezeCard } from "./WriteFreezeCard";
import { BackupUploadCard } from "./BackupUploadCard";
import { backupInProgress } from "./status";

const POLL_INTERVAL_MS = 1500;

export function BackupsTab() {
  const { t } = useTranslation();
  const [createOpened, setCreateOpened] = useState(false);
  const [verifyingId, setVerifyingId] = useState<string | null>(null);
  const [linkValue, setLinkValue] = useState<string | null>(null);

  const state = useAsync(() => listBackups({ page: 1, page_size: 100 }), [], {
    cacheKey: "backups:list",
  });
  const reload = state.reload;

  const items = state.data?.items ?? [];
  const hasRunning = useMemo(() => items.some((item) => backupInProgress(item.status)), [items]);

  // 生成中的包由服务端异步推进，这里按固定间隔刷新列表直到全部进入终态。
  useEffect(() => {
    if (!hasRunning) {
      return;
    }
    const timer = window.setInterval(reload, POLL_INTERVAL_MS);
    return () => window.clearInterval(timer);
  }, [hasRunning, reload]);

  const onGetLink = async (packageId: string) => {
    try {
      const link = await createBackupLink(packageId);
      setLinkValue(link.url);
    } catch (err) {
      notifyError(err);
    }
  };

  const onVerify = async (packageId: string, deep: boolean) => {
    setVerifyingId(packageId);
    try {
      await verifyBackup(packageId, deep);
      notifySuccess(t("backups.verifyOk"));
    } catch (err) {
      notifyError(err);
    } finally {
      setVerifyingId(null);
    }
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
          .catch(notifyError);
      },
    });
  };

  return (
    <Box
      data-testid="backups-tab"
      style={{
        height: "calc(100dvh - var(--app-shell-header-height) - var(--app-shell-padding) * 2)",
        minHeight: 480,
        display: "flex",
        flexDirection: "column",
        gap: 12,
        overflow: "hidden",
      }}
    >
      <WriteFreezeCard />

      <OpsSection
        title={t("backups.tabBackup")}
        meta={state.data ? formatCount(state.data.total) : undefined}
        style={{ flex: 1, minHeight: 0, display: "flex", flexDirection: "column", overflow: "hidden" }}
        bodyStyle={{ flex: 1, minHeight: 0, overflow: "auto" }}
        bodyPadding={0}
        actions={
          <Group gap="xs">
            <Button
              variant="default"
              size="xs"
              leftSection={<IconRefresh size={14} />}
              loading={state.loading || state.refreshing}
              onClick={reload}
            >
              {t("backups.refresh")}
            </Button>
            <Button
              size="xs"
              leftSection={<IconPlus size={14} />}
              onClick={() => setCreateOpened(true)}
            >
              {t("backups.create")}
            </Button>
          </Group>
        }
      >
        <AsyncBoundary state={state}>
          {(data) =>
            data.items.length === 0 ? (
              <Box p="xl">
                <EmptyState message={t("backups.empty")} description={t("backups.emptyHint")} />
              </Box>
            ) : (
              <BackupTable
                items={data.items}
                verifyingId={verifyingId}
                onGetLink={(id) => void onGetLink(id)}
                onVerify={(id, deep) => void onVerify(id, deep)}
                onDelete={onDelete}
              />
            )
          }
        </AsyncBoundary>
      </OpsSection>

      <ImportFromUrlCard />

      <BackupUploadCard />

      <BackupCreateModal
        opened={createOpened}
        onClose={() => setCreateOpened(false)}
        onCreated={reload}
      />

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
    </Box>
  );
}
