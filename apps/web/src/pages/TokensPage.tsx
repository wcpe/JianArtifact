// 访问令牌：列表 + 新建（明文仅回显一次）+ 吊销。
import {
  Alert,
  Box,
  Button,
  Code,
  Group,
  Modal,
  Stack,
  Table,
  Text,
  TextInput,
} from "@mantine/core";
import { useForm } from "@mantine/form";
import { useDisclosure, useMediaQuery } from "@mantine/hooks";
import { IconClock, IconHistory, IconKey, IconPlus, IconTrash } from "@tabler/icons-react";
import { EmptyState } from "@jianartifact/ui";
import { useState } from "react";
import { useTranslation } from "react-i18next";

import { PageShell } from "../app/PageShell";
import { AsyncBoundary } from "../components/AsyncBoundary";
import { CopyTextButton } from "../components/CopyTextButton";
import { OpsHelpButton, OpsKpiBand } from "../components/ops/OpsKit";
import { createToken, deleteToken, listTokens } from "../api/endpoints";
import type { TokenCreated } from "../api/types";
import { useAsync } from "../hooks/useAsync";
import { confirmDanger, notifyError, notifySuccess } from "../lib/feedback";
import { formatUtcToLocal, formatUtcToLocalDate } from "../lib/timeFormat";

export function TokensPage() {
  const { t } = useTranslation();
  const state = useAsync(listTokens, [], { cacheKey: "tokens:list" });
  // 顶部概览带口径：直接由当前列表派生，保证与表格同源；数据未就绪时显示「—」而不是假 0。
  // 概览带只取日期（`formatUtcToLocalDate`）：完整时间戳在窄屏会被 ellipsis 截成「2026-0…」，
  // 表格里仍保留完整时刻。
  const tokens = state.data?.items ?? [];
  const ready = state.data !== null;
  // 窄屏（< 48em）：4 列在 390px 下会把创建时间压成两行，操作按钮也只剩竖排空间。
  const isNarrow = useMediaQuery("(max-width: 48em)") ?? false;
  const latest = tokens.length
    ? formatUtcToLocalDate(
        tokens.reduce((max, tk) => (tk.createdAt > max.createdAt ? tk : max)).createdAt,
      )
    : "—";
  const earliest = tokens.length
    ? formatUtcToLocalDate(
        tokens.reduce((min, tk) => (tk.createdAt < min.createdAt ? tk : min)).createdAt,
      )
    : "—";
  const [createOpened, createModal] = useDisclosure(false);
  const [creating, setCreating] = useState(false);
  const [created, setCreated] = useState<TokenCreated | null>(null);

  const form = useForm({
    initialValues: { name: "" },
    validate: { name: (v) => (v.trim() ? null : t("tokens.name")) },
  });

  const handleCreate = form.onSubmit((values) => {
    setCreating(true);
    createToken(values.name)
      .then((token) => {
        createModal.close();
        form.reset();
        setCreated(token);
        state.reload();
      })
      .catch(notifyError)
      .finally(() => setCreating(false));
  });

  const handleDelete = (id: number) => {
    confirmDanger({
      title: t("common.delete"),
      message: t("tokens.deleteConfirm"),
      confirmLabel: t("common.delete"),
      cancelLabel: t("common.cancel"),
      onConfirm: () => {
        deleteToken(id)
          .then(() => {
            notifySuccess(t("common.deleted"));
            state.reload();
          })
          .catch(notifyError);
      },
    });
  };

  return (
    <>
      {/* 列表页统一范式：页头操作固定，表格在视口内滚动 + 表头吸顶。 */}
      <PageShell>
        <Stack gap="md" style={{ flex: 1, minHeight: 0 }}>
          {/* 顶部概览带：页内大标题移除后，由它承担原来标题的视觉重量——左侧是当前
              用户名下令牌的构成（口径来自当前列表，未就绪时显示「—」而不是假的 0），
              右侧是唯一的主操作。 */}
          <OpsKpiBand
            variant="strip"
            label={t("tokens.summaryLabel")}
            // 窄屏单列：签发日期（10 字符）在 2 列布局里会被 ellipsis 截断，单列才放得下。
            // 窄屏 2 列：原来 base:1 让三个指标竖着堆三行、独占近半屏；
            // 3 列会把 2026-01-22 这类完整日期裁掉（实测 scrollWidth 107 > 90），故用 2 列。
            cols={isNarrow ? { base: 2, xs: 2 } : { base: 1, xs: 2, sm: 3 }}
            items={[
              {
                label: t("tokens.summaryTotal"),
                value: ready ? String(tokens.length) : "—",
                icon: <IconKey size={16} />,
                hint: t("tokens.summaryHint"),
              },
              {
                label: t("tokens.summaryLatest"),
                value: ready && tokens.length ? latest : "—",
                icon: <IconClock size={16} />,
                hint: t("tokens.summaryHint"),
              },
              {
                label: t("tokens.summaryEarliest"),
                value: ready && tokens.length ? earliest : "—",
                icon: <IconHistory size={16} />,
                hint: t("tokens.summaryHint"),
              },
            ]}
            actions={
              <>
                {/* 使用与安全说明搬到工具栏按钮上（气泡里），不再占页面布局。 */}
                <OpsHelpButton
                  title={t("tokens.usageTitle")}
                  items={[
                    {
                      label: t("tokens.usageAuthLabel", { defaultValue: "如何认证" }),
                      value: t("tokens.usageAuth"),
                    },
                    {
                      label: t("tokens.usageOnceLabel", { defaultValue: "明文只显示一次" }),
                      value: t("tokens.usageOnce"),
                    },
                    {
                      label: t("tokens.usageRevokeLabel", { defaultValue: "吊销即失效" }),
                      value: t("tokens.usageRevoke"),
                    },
                    {
                      label: t("tokens.usageScopeLabel", { defaultValue: "权限范围" }),
                      value: t("tokens.usageScope"),
                    },
                  ]}
                />
                <Button leftSection={<IconPlus size={14} />} onClick={createModal.open}>
                  {t("tokens.create")}
                </Button>
              </>
            }
          />

          <Box style={{ flex: 1, minHeight: 0, overflowY: "auto" }}>
            <AsyncBoundary state={state}>
              {(list) =>
                list.items.length === 0 ? (
                  <EmptyState message={t("tokens.empty")} />
                ) : (
                  <Table
                    striped={!isNarrow}
                    withRowBorders
                    highlightOnHover
                    stickyHeader
                    verticalSpacing={isNarrow ? "md" : "xs"}
                  >
                    <Table.Thead>
                      <Table.Tr>
                        {/* 窄屏只留「名称 + 操作」：4 列在 390px 下会把创建时间压成两行；
                            id 与创建时间落到名称下方副文本，操作按钮也一并挪到那里（全宽排布）。 */}
                        {isNarrow ? null : <Table.Th>{t("tokens.id")}</Table.Th>}
                        <Table.Th>{t("tokens.name")}</Table.Th>
                        {isNarrow ? null : <Table.Th>{t("tokens.createdAt")}</Table.Th>}
                        {isNarrow ? null : <Table.Th>{t("common.actions")}</Table.Th>}
                      </Table.Tr>
                    </Table.Thead>
                    <Table.Tbody>
                      {list.items.map((token) => {
                        const revoke = (
                          <Button
                            size={isNarrow ? "xs" : "compact-xs"}
                            variant="subtle"
                            color="red"
                            leftSection={<IconTrash size={14} />}
                            onClick={() => handleDelete(token.id)}
                            aria-label={t("common.delete")}
                          >
                            {t("tokens.revoke", { defaultValue: "吊销" })}
                          </Button>
                        );
                        return (
                          <Table.Tr key={token.id}>
                            {isNarrow ? null : <Table.Td>{token.id}</Table.Td>}
                            <Table.Td>
                              <Stack gap={isNarrow ? 12 : 4}>
                                <Text size="sm">{token.name}</Text>
                                {isNarrow ? (
                                  <Text size="xs" c="dimmed" truncate>
                                    #{token.id} · {formatUtcToLocal(token.createdAt)}
                                  </Text>
                                ) : null}
                                {/* 操作按钮必须包在 Group 里：Stack 默认 align="stretch" 会把按钮
                                    拉满整行、文字居中，看起来像一条居中的空按钮。 */}
                                {isNarrow ? <Group gap={8}>{revoke}</Group> : null}
                              </Stack>
                            </Table.Td>
                            {isNarrow ? null : (
                              <Table.Td>{formatUtcToLocal(token.createdAt)}</Table.Td>
                            )}
                            {isNarrow ? null : <Table.Td>{revoke}</Table.Td>}
                          </Table.Tr>
                        );
                      })}
                    </Table.Tbody>
                  </Table>
                )
              }
            </AsyncBoundary>
          </Box>
        </Stack>
      </PageShell>

      <Modal opened={createOpened} onClose={createModal.close} title={t("tokens.create")}>
        <form onSubmit={handleCreate}>
          <TextInput label={t("tokens.name")} withAsterisk {...form.getInputProps("name")} />
          <Group justify="flex-end" mt="md">
            <Button variant="default" onClick={createModal.close}>
              {t("common.cancel")}
            </Button>
            <Button type="submit" loading={creating}>
              {t("common.create")}
            </Button>
          </Group>
        </form>
      </Modal>

      <Modal
        opened={created !== null}
        onClose={() => setCreated(null)}
        title={t("tokens.plaintextTitle")}
      >
        <Alert color="yellow" variant="light" mb="sm">
          {t("tokens.plaintextHint")}
        </Alert>
        <Group justify="space-between" wrap="nowrap">
          <Code style={{ wordBreak: "break-all" }}>{created?.token}</Code>
          <CopyTextButton value={created?.token ?? ""} />
        </Group>
        <Group justify="flex-end" mt="md">
          <Button onClick={() => setCreated(null)}>{t("common.close")}</Button>
        </Group>
      </Modal>
    </>
  );
}
