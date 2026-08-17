// 审计日志页（FR-38）：展示全部管理写操作（谁在何时做了什么），支持按 操作者/操作类型/仓库/时间 筛选。
import { useCallback, useState } from "react";
import { useTranslation } from "react-i18next";
import {
  Badge,
  Button,
  Card,
  Group,
  Input,
  Pagination,
  Select,
  Stack,
  Table,
  Text,
} from "@mantine/core";
import { EmptyState, PageHeader } from "@jianartifact/ui";
import { getAuditLogs, type AuditLogEntry } from "../api/endpoints";
import { useAsync, REFRESH_EVENT } from "../hooks/useAsync";
import { AsyncBoundary } from "../components/AsyncBoundary";
import { density } from "../theme/density";

const PAGE_SIZE = 50;

/** 操作类型可读标签（fallback：原值）。 */
function actionLabel(action: string): string {
  const map: Record<string, string> = {
    "asset.put": "上传制品",
    "asset.delete": "删除制品",
    "repo.create": "创建仓库",
    "repo.delete": "删除仓库",
    "acl.set": "设置 ACL",
    "user.create": "创建用户",
    "user.update": "更新用户",
    "user.delete": "删除用户",
    "user.password": "修改口令",
    "token.create": "签发令牌",
    "token.revoke": "吊销令牌",
    "setting.set": "更新设置",
  };
  return map[action] ?? action;
}

/** 结果徽章：ok / error。 */
function resultBadge(result: string) {
  const color = result === "error" ? "red" : "green";
  return <Badge color={color} variant="light" size="xs">{result}</Badge>;
}

export function AuditLogPage() {
  const { t } = useTranslation();
  const [page, setPage] = useState(1);
  const [actor, setActor] = useState("");
  const [action, setAction] = useState("");
  const [repo, setRepo] = useState("");
  const [appliedFilter, setAppliedFilter] = useState<{ actor: string; action: string; repo: string }>({
    actor: "",
    action: "",
    repo: "",
  });

  const state = useAsync(
    () =>
      getAuditLogs({
        actor: appliedFilter.actor || undefined,
        action: appliedFilter.action || undefined,
        repo: appliedFilter.repo || undefined,
        limit: PAGE_SIZE,
        offset: (page - 1) * PAGE_SIZE,
      }),
    [page, appliedFilter],
  );

  const applyFilter = useCallback(() => {
    setPage(1);
    setAppliedFilter({ actor: actor.trim(), action, repo: repo.trim() });
  }, [actor, action, repo]);

  const resetFilter = useCallback(() => {
    setActor("");
    setAction("");
    setRepo("");
    setPage(1);
    setAppliedFilter({ actor: "", action: "", repo: "" });
  }, []);

  return (
    <>
      <PageHeader title={t("audit.title", { defaultValue: "审计日志" })} description={t("audit.description", { defaultValue: "全部管理操作记录：谁在何时上传/删除/修改了什么" })} />
      <Card withBorder radius="md" padding={density.cardPadding}>
        <Stack gap="sm">
          {/* FR-38：筛选栏（操作者 / 操作类型 / 仓库） */}
          <Group gap="sm" align="flex-end" wrap="wrap">
            <Input.Wrapper label={t("audit.filterActor", { defaultValue: "操作者" })} w={180}>
              <Input size="xs" value={actor} onChange={(e) => setActor(e.currentTarget.value)} placeholder={t("audit.filterActorPlaceholder", { defaultValue: "用户名" })} />
            </Input.Wrapper>
            <Input.Wrapper label={t("audit.filterAction", { defaultValue: "操作类型" })} w={220}>
              <Select
                size="xs"
                data={[
                  { value: "", label: t("audit.allActions", { defaultValue: "全部操作" }) },
                  { value: "asset.put", label: "上传制品" },
                  { value: "asset.delete", label: "删除制品" },
                  { value: "repo.create", label: "创建仓库" },
                  { value: "repo.delete", label: "删除仓库" },
                  { value: "acl.set", label: "设置 ACL" },
                  { value: "user.create", label: "创建用户" },
                  { value: "user.update", label: "更新用户" },
                  { value: "user.delete", label: "删除用户" },
                  { value: "user.password", label: "修改口令" },
                  { value: "token.create", label: "签发令牌" },
                  { value: "token.revoke", label: "吊销令牌" },
                  { value: "setting.set", label: "更新设置" },
                ]}
                value={action}
                onChange={(v) => setAction(v ?? "")}
              />
            </Input.Wrapper>
            <Input.Wrapper label={t("audit.filterRepo", { defaultValue: "仓库" })} w={200}>
              <Input size="xs" value={repo} onChange={(e) => setRepo(e.currentTarget.value)} placeholder={t("audit.filterRepoPlaceholder", { defaultValue: "仓库名" })} />
            </Input.Wrapper>
            <Button size="xs" onClick={applyFilter}>{t("audit.applyFilter", { defaultValue: "筛选" })}</Button>
            <Button size="xs" variant="default" onClick={resetFilter}>{t("audit.resetFilter", { defaultValue: "重置" })}</Button>
          </Group>

          <AsyncBoundary state={state}>
            {(list) =>
              (list.items ?? []).length === 0 ? (
                <EmptyState message={t("audit.empty", { defaultValue: "暂无审计记录" })} />
              ) : (
                <>
                  <Table striped highlightOnHover withTableBorder>
                    <Table.Thead>
                      <Table.Tr>
                        <Table.Th>{t("audit.ts", { defaultValue: "时间" })}</Table.Th>
                        <Table.Th>{t("audit.actor", { defaultValue: "操作者" })}</Table.Th>
                        <Table.Th>{t("audit.action", { defaultValue: "操作" })}</Table.Th>
                        <Table.Th>{t("audit.entityKey", { defaultValue: "对象" })}</Table.Th>
                        <Table.Th>{t("audit.repo", { defaultValue: "仓库" })}</Table.Th>
                        <Table.Th>{t("audit.result", { defaultValue: "结果" })}</Table.Th>
                        <Table.Th>{t("audit.ip", { defaultValue: "来源" })}</Table.Th>
                      </Table.Tr>
                    </Table.Thead>
                    <Table.Tbody>
                      {list.items.map((e: AuditLogEntry) => (
                        <Table.Tr key={e.id}>
                          <Table.Td>{new Date(e.ts).toLocaleString()}</Table.Td>
                          <Table.Td>{e.actor || "-"}</Table.Td>
                          <Table.Td>{actionLabel(e.action)}</Table.Td>
                          <Table.Td>
                            <Text size="xs" style={{ wordBreak: "break-all" }}>
                              {e.entityKey || "-"}
                            </Text>
                          </Table.Td>
                          <Table.Td>{e.repo || "-"}</Table.Td>
                          <Table.Td>{resultBadge(e.result)}</Table.Td>
                          <Table.Td>{e.ip || "-"}</Table.Td>
                        </Table.Tr>
                      ))}
                    </Table.Tbody>
                  </Table>
                  {list.total > PAGE_SIZE && (
                    <Pagination
                      total={Math.ceil(list.total / PAGE_SIZE)}
                      value={page}
                      onChange={setPage}
                      size="sm"
                    />
                  )}
                </>
              )
            }
          </AsyncBoundary>
        </Stack>
      </Card>
    </>
  );
}
