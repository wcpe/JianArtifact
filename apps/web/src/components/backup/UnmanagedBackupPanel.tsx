// 未管理备份包面板（FR-40 追加八）。
//
// # 解决什么问题
//
// `${JIAN_DATA_DIR}/backups/` 里的包体与数据库登记可能失步：文件在、登记没了。
// 这类包在常规列表里完全不可见（列表读登记表），于是磁盘被占着却既不能用也不能删。
// 本面板把它们列出来，让用户勾选后导入管理列表。
//
// # 为什么支持单选与多选
//
// 用户的实际场景不同：偶尔只捞回一个包，也可能一次清理一堆历史残留。
// 全选/清空是"多选"的便捷入口，单个勾选是"单选"的自然形态，两者共用同一份选中集合，
// 不需要两组控件。
//
// # 为什么不自动导入
//
// 自动导入会把"用户想保留的"和"用户想删掉的"混为一谈。导入是一次明确的决策，
// 因此本面板只做选择与提交，扫描结果本身不改变服务器状态。
import {
  Alert,
  Badge,
  Box,
  Button,
  Checkbox,
  Group,
  Loader,
  ScrollArea,
  Stack,
  Table,
  Text,
} from "@mantine/core";
import { IconRefresh, IconDownload } from "@tabler/icons-react";
import { useCallback, useEffect, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";

import { adoptBackups, listUnmanagedBackups } from "../../api/endpoints";
import type { AdoptBackupsResult, UnmanagedBackup } from "../../api/types";
import { notifyError, notifySuccess } from "../../lib/feedback";
import { formatBytes } from "../../lib/format";
import { dispatchRefresh } from "../../hooks/useAsync";

export function UnmanagedBackupPanel({ onDone }: { onDone?: () => void }) {
  const { t } = useTranslation();
  const [items, setItems] = useState<UnmanagedBackup[] | null>(null);
  const [loading, setLoading] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // 选中集合用 Set：勾选/取消是高频操作，数组的 indexOf 在多选几十项时会明显拖慢。
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [result, setResult] = useState<AdoptBackupsResult | null>(null);

  const scan = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const list = await listUnmanagedBackups();
      setItems(list.items);
      // 扫描后清理已不存在的选中项：否则提交时会带上服务端已无的标识。
      const present = new Set(list.items.map((it) => it.fileId));
      setSelected((prev) => new Set([...prev].filter((id) => present.has(id))));
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
      setItems([]);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void scan();
  }, [scan]);

  // 可导入的条目：valid=true。非法包展示出来但不可勾选（用户需要知道它存在）。
  const importable = useMemo(() => (items ?? []).filter((it) => it.valid), [items]);
  const allSelected = importable.length > 0 && importable.every((it) => selected.has(it.fileId));

  const toggle = useCallback((fileId: string) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(fileId)) {
        next.delete(fileId);
      } else {
        next.add(fileId);
      }
      return next;
    });
  }, []);

  const toggleAll = useCallback(() => {
    setSelected((prev) => {
      if (importable.length > 0 && importable.every((it) => prev.has(it.fileId))) {
        return new Set();
      }
      return new Set(importable.map((it) => it.fileId));
    });
  }, [importable]);

  const submit = useCallback(async () => {
    const ids = [...selected];
    if (ids.length === 0) {
      return;
    }
    setSubmitting(true);
    setError(null);
    try {
      const res = await adoptBackups(ids);
      setResult(res);
      if (res.adopted > 0) {
        notifySuccess(t("backups.adoptResultSummary", res));
        // 备份列表在监听该事件：让刚导入的包立即出现在下面的表格里。
        dispatchRefresh();
        onDone?.();
      }
      if (res.failed > 0) {
        notifyError(new Error(t("backups.adoptResultSummary", res)));
      }
      // 重新扫描：导入成功的条目应从"未管理"里消失，失败的仍在。
      await scan();
      setSelected(new Set());
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setSubmitting(false);
    }
  }, [onDone, scan, selected, t]);

  return (
    <Stack gap="sm" data-testid="unmanaged-panel">
      <Text size="sm" c="dimmed">
        {t("backups.adoptHint")}
      </Text>

      {error ? (
        <Alert color="red" variant="light">
          {error}
        </Alert>
      ) : null}

      {loading && items === null ? (
        <Group gap="xs">
          <Loader size="xs" />
          <Text size="sm" c="dimmed">
            {t("backups.adoptLoading")}
          </Text>
        </Group>
      ) : null}

      {items !== null && items.length === 0 ? (
        <Text size="sm" c="dimmed" data-testid="unmanaged-empty">
          {t("backups.adoptEmpty")}
        </Text>
      ) : null}

      {items !== null && items.length > 0 ? (
        <>
          <Group justify="space-between" wrap="nowrap">
            <Group gap="xs">
              <Checkbox
                label={t("backups.adoptSelectAll")}
                checked={allSelected}
                indeterminate={selected.size > 0 && !allSelected}
                onChange={toggleAll}
                disabled={importable.length === 0}
                data-testid="unmanaged-select-all"
              />
              {selected.size > 0 ? (
                <Badge variant="light">
                  {t("backups.adoptSelected", { count: selected.size })}
                </Badge>
              ) : null}
            </Group>
            <Group gap="xs">
              <Button
                size="xs"
                variant="default"
                leftSection={<IconRefresh size={14} />}
                onClick={() => void scan()}
                loading={loading}
              >
                {t("backups.adoptRescan")}
              </Button>
              <Button
                size="xs"
                leftSection={<IconDownload size={14} />}
                onClick={() => void submit()}
                disabled={selected.size === 0}
                loading={submitting}
                data-testid="unmanaged-submit"
              >
                {t("backups.adoptSubmit")}
              </Button>
            </Group>
          </Group>

          <ScrollArea.Autosize mah={320}>
            <Table striped highlightOnHover verticalSpacing="xs" data-testid="unmanaged-table">
              <Table.Thead>
                <Table.Tr>
                  <Table.Th style={{ width: 36 }} />
                  <Table.Th>{t("backups.colIdentity")}</Table.Th>
                  <Table.Th>{t("backups.adoptSize")}</Table.Th>
                  <Table.Th>{t("backups.adoptModified")}</Table.Th>
                  <Table.Th />
                </Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {items.map((it) => (
                  <Table.Tr key={it.fileId} data-testid={`unmanaged-row-${it.fileId}`}>
                    <Table.Td>
                      <Checkbox
                        checked={selected.has(it.fileId)}
                        onChange={() => toggle(it.fileId)}
                        disabled={!it.valid}
                        aria-label={it.fileId}
                      />
                    </Table.Td>
                    <Table.Td>
                      <Box>
                        <Text size="xs" fw={500} ff="monospace">
                          {it.fileId}
                        </Text>
                        {it.renamed ? (
                          <Text size="xs" c="orange">
                            {t("backups.adoptRenamed")}
                          </Text>
                        ) : null}
                      </Box>
                    </Table.Td>
                    <Table.Td>
                      <Text size="xs">{formatBytes(it.sizeBytes)}</Text>
                    </Table.Td>
                    <Table.Td>
                      <Text size="xs" c="dimmed">
                        {it.modifiedAt ? new Date(it.modifiedAt).toLocaleString() : "—"}
                      </Text>
                    </Table.Td>
                    <Table.Td>
                      {it.valid ? (
                        <Badge size="xs" variant="light" color={it.hasSidecar ? "teal" : "gray"}>
                          {it.hasSidecar
                            ? t("backups.adoptHasSidecar")
                            : t("backups.adoptNoSidecar")}
                        </Badge>
                      ) : (
                        <Badge size="xs" variant="light" color="red" title={it.reason ?? ""}>
                          {t("backups.adoptInvalid")}
                        </Badge>
                      )}
                    </Table.Td>
                  </Table.Tr>
                ))}
              </Table.Tbody>
            </Table>
          </ScrollArea.Autosize>
        </>
      ) : null}

      {result ? (
        <Alert
          color={result.failed > 0 ? "orange" : "green"}
          variant="light"
          data-testid="unmanaged-result"
        >
          <Text size="sm" fw={500}>
            {t("backups.adoptResultTitle")}
          </Text>
          <Text size="sm">{t("backups.adoptResultSummary", result)}</Text>
        </Alert>
      ) : null}
    </Stack>
  );
}
