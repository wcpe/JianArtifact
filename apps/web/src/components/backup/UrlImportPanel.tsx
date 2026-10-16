// 从 URL 导入（FR-137）：只负责表单本体，容器（内联展开区）由 ImportInline 承担。
//
// body-only 的理由与 BackupUploadPanel 一致：它曾经自带一张卡（ImportFromUrlCard），
// 展开后就与备份列表抢页面高度；后来进弹窗，现在直接展开在页面里，由 ImportInline 控制显隐。
//
// 提交成功后派发全局 REFRESH_EVENT（useAsync 监听），与上传通道**同一个刷新通道**——
// 不再接受 onImported 回调：同一件事有两个刷新通道，容易出现一个刷了另一个没刷的错位，
// 而新记录必须立刻出现在下方统一事件表里（事件表只在其内出现非终态记录时才开始轮询，
// 不主动刷新就会卡在"提交成功但列表里看不到"）。
import { Button, Group, Stack, Switch, TextInput } from "@mantine/core";
import { IconPlayerPlay } from "@tabler/icons-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";

import { importBackupFromURL } from "../../api/endpoints";
import { notifyError, notifySuccess } from "../../lib/feedback";
import { dispatchRefresh } from "../../hooks/useAsync";

export function UrlImportPanel() {
  const { t } = useTranslation();
  const [url, setUrl] = useState("");
  const [overwrite, setOverwrite] = useState(false);
  const [deep, setDeep] = useState(false);
  const [sha256, setSha256] = useState("");

  const onImport = async () => {
    const trimmed = url.trim();
    if (!/^https?:\/\//i.test(trimmed)) {
      notifyError(t("backups.importFieldUrlRequired"));
      return;
    }
    try {
      const accepted = await importBackupFromURL({
        sourceUrl: trimmed,
        // omit 掉 false 值：契约里这两个字段是"显式置真才有意义"，传 false 与不传等价，
        // 但传 undefined 能让请求体保持最小。
        overwrite: overwrite || undefined,
        deep: deep || undefined,
        expectedSha256: sha256.trim() || undefined,
      });
      notifySuccess(t("backups.importStarted"));
      setUrl("");
      setSha256("");
      // 带上 importId：页面据此打开恢复进度弹窗。此前只派发一个"去刷新"的信号，
      // 用户随后除了列表里多一行之外没有任何进度反馈（见 RestoreProgressModal 文件头）。
      dispatchRefresh(accepted.importId);
    } catch (err) {
      notifyError(err);
    }
  };

  return (
    <Stack gap="sm">
      <TextInput
        label={t("backups.importFieldUrl")}
        placeholder={t("backups.importFieldUrlPlaceholder")}
        value={url}
        onChange={(e) => setUrl(e.currentTarget.value)}
        required
      />
      {/* 「目标实例非空」是这一页最容易踩的失败：它由后端在**异步暂存阶段**判定，
          HTTP 早已返回 202，用户只会看到"已受理、随后记录变失败"。所以开关与说明必须
          一起摆在提交按钮上方，而不是等失败后再让用户猜。 */}
      <Switch
        label={t("backups.importOverwrite")}
        description={t("backups.importOverwriteHint")}
        checked={overwrite}
        onChange={(e) => setOverwrite(e.currentTarget.checked)}
        data-testid="url-import-overwrite"
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
      <Group justify="flex-end">
        <Button onClick={() => void onImport()} leftSection={<IconPlayerPlay size={16} />}>
          {t("backups.importSubmit")}
        </Button>
      </Group>
    </Stack>
  );
}
