// 从 URL 导入卡（FR-137）：分区卡右上「从 URL 导入」按钮 → Modal 表单；
// 卡体为导入记录表。提交成功后关闭并刷新记录列表，存在非终态记录时按 1500ms 轮询。
import { Button, Group, Modal, Stack, Switch, TextInput } from "@mantine/core";
import { IconWorldDownload } from "@tabler/icons-react";
import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";

import { importBackupFromURL, listBackupImports } from "../../api/endpoints";
import type { BackupImportList } from "../../api/types";
import { notifyError, notifySuccess } from "../../lib/feedback";
import { formatCount } from "../../lib/format";
import { useAsync } from "../../hooks/useAsync";
import { OpsSection } from "../ops/OpsKit";
import { importInProgress } from "./status";
import { ImportRecordsTable } from "./ImportRecordsTable";

const POLL_INTERVAL_MS = 1500;

export function ImportFromUrlCard() {
  const { t } = useTranslation();
  const [opened, setOpened] = useState(false);
  const [url, setUrl] = useState("");
  const [overwrite, setOverwrite] = useState(false);
  const [deep, setDeep] = useState(false);
  const [sha256, setSha256] = useState("");

  const state = useAsync<BackupImportList>(
    () => listBackupImports({ page: 1, page_size: 100 }),
    [],
    { cacheKey: "backups:imports" },
  );
  const items = state.data?.items ?? [];
  const hasRunning = items.some((item) => importInProgress(item.status));

  // 存在非终态导入记录（queued/fetching/staging）时持续轮询，全部终态后停止。
  useEffect(() => {
    if (!hasRunning) {
      return;
    }
    const timer = window.setInterval(() => state.reload(), POLL_INTERVAL_MS);
    return () => window.clearInterval(timer);
  }, [hasRunning, state.reload]);

  const onImport = async () => {
    const trimmed = url.trim();
    if (!/^https?:\/\//i.test(trimmed)) {
      notifyError(t("backups.importFieldUrlRequired"));
      return;
    }
    try {
      await importBackupFromURL({
        sourceUrl: trimmed,
        overwrite: overwrite || undefined,
        deep: deep || undefined,
        expectedSha256: sha256.trim() || undefined,
      });
      notifySuccess(t("backups.importStarted"));
      setOpened(false);
      setUrl("");
      setSha256("");
      state.reload();
    } catch (err) {
      notifyError(err);
    }
  };

  return (
    <OpsSection
      title={t("backups.importCardTitle")}
      meta={state.data ? formatCount(state.data.total) : undefined}
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
          leftSection={<IconWorldDownload size={14} />}
          onClick={() => setOpened(true)}
        >
          {t("backups.importButton")}
        </Button>
      }
    >
      <ImportRecordsTable items={items} loading={state.loading} />

      <Modal
        opened={opened}
        onClose={() => setOpened(false)}
        title={t("backups.importModalTitle")}
        centered
      >
        <Stack gap="sm">
          <TextInput
            label={t("backups.importFieldUrl")}
            placeholder={t("backups.importFieldUrlPlaceholder")}
            value={url}
            onChange={(e) => setUrl(e.currentTarget.value)}
            required
          />
          <Switch
            label={t("backups.importOverwrite")}
            description={t("backups.importOverwriteHint")}
            checked={overwrite}
            onChange={(e) => setOverwrite(e.currentTarget.checked)}
          />
          <Switch
            label={t("backups.importDeep")}
            description={t("backups.importDeepHint")}
            checked={deep}
            onChange={(e) => setDeep(e.currentTarget.checked)}
          />
          <TextInput
            label={t("backups.importSha256")}
            placeholder={t("backups.importSha256Placeholder")}
            value={sha256}
            onChange={(e) => setSha256(e.currentTarget.value)}
          />
          <Group justify="flex-end" gap="xs">
            <Button variant="default" onClick={() => setOpened(false)}>
              {t("common.cancel")}
            </Button>
            <Button onClick={() => void onImport()}>{t("backups.importSubmit")}</Button>
          </Group>
        </Stack>
      </Modal>
    </OpsSection>
  );
}
