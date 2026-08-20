// 审计日志页：管理操作与复制应用记录。
import { useCallback, useState } from "react";
import { useTranslation } from "react-i18next";
import {
  Badge,
  Box,
  Button,
  Card,
  Group,
  Input,
  Pagination,
  Select,
  Stack,
  Table,
  Tabs,
  Text,
} from "@mantine/core";
import { EmptyState, PageHeader } from "@jianartifact/ui";
import { getAuditLogs, getReplicationApplyLogs, type AuditLogEntry } from "../api/endpoints";
import type { ReplicationApplyLog } from "../api/types";
import { useAsync } from "../hooks/useAsync";
import { AsyncBoundary } from "../components/AsyncBoundary";
import { density } from "../theme/density";

const PAGE_SIZE = 50;
const REPLICATION_FAILED_RESULTS = ["failed", "blob_failed", "skipped_permanent"];
const REPLICATION_PENDING_RESULTS = ["pending_parent", "metadata_applied_pending_blob"];

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

function resultBadge(result: string) {
  return (
    <Badge color={result === "error" ? "red" : "green"} variant="light" size="xs">
      {result}
    </Badge>
  );
}

function replicationResultBadge(result: string) {
  const color = REPLICATION_FAILED_RESULTS.includes(result)
    ? "red"
    : REPLICATION_PENDING_RESULTS.includes(result)
      ? "yellow"
      : "green";
  return (
    <Badge color={color} variant="light" size="xs">
      {result}
    </Badge>
  );
}

function ReplicationApplyLogPanel() {
  const { t } = useTranslation();
  const [page, setPage] = useState(1);
  const [result, setResult] = useState("");
  const [entityType, setEntityType] = useState("");
  const [sourceNode, setSourceNode] = useState("");
  const [appliedFilter, setAppliedFilter] = useState({
    result: "",
    entityType: "",
    sourceNode: "",
  });
  const state = useAsync(
    () =>
      getReplicationApplyLogs({
        result: appliedFilter.result || undefined,
        entityType: appliedFilter.entityType || undefined,
        sourceNode: appliedFilter.sourceNode || undefined,
        limit: PAGE_SIZE,
        offset: (page - 1) * PAGE_SIZE,
      }),
    [page, appliedFilter],
  );

  const applyFilter = useCallback(() => {
    setPage(1);
    setAppliedFilter({ result, entityType, sourceNode: sourceNode.trim() });
  }, [entityType, result, sourceNode]);

  const resetFilter = useCallback(() => {
    setResult("");
    setEntityType("");
    setSourceNode("");
    setPage(1);
    setAppliedFilter({ result: "", entityType: "", sourceNode: "" });
  }, []);

  return (
    <Stack gap="sm" style={{ flex: 1, minHeight: 0, display: "flex", flexDirection: "column" }}>
      <Group gap="sm" align="flex-end" wrap="wrap">
        <Input.Wrapper label={t("audit.replicationFilterResult")} w={220}>
          <Select
            aria-label={t("audit.replicationFilterResult")}
            size="xs"
            data={[
              { value: "", label: t("audit.replicationAllResults") },
              { value: "applied", label: t("audit.replicationResultApplied") },
              { value: "lww_skipped", label: t("audit.replicationResultLwwSkipped") },
              {
                value: "metadata_applied_pending_blob",
                label: t("audit.replicationResultPendingBlob"),
              },
              { value: "pending_parent", label: t("audit.replicationResultPendingParent") },
              { value: "blob_failed", label: t("audit.replicationResultBlobFailed") },
              { value: "skipped_permanent", label: t("audit.replicationResultSkippedPermanent") },
              { value: "failed", label: t("audit.replicationResultFailed") },
            ]}
            value={result}
            onChange={(value) => setResult(value ?? "")}
          />
        </Input.Wrapper>
        <Input.Wrapper label={t("audit.replicationFilterEntity")} w={180}>
          <Input
            size="xs"
            value={entityType}
            onChange={(e) => setEntityType(e.currentTarget.value)}
            placeholder={t("audit.replicationEntityPlaceholder")}
          />
        </Input.Wrapper>
        <Input.Wrapper label={t("audit.replicationFilterSourceNode")} w={220}>
          <Input
            size="xs"
            value={sourceNode}
            onChange={(e) => setSourceNode(e.currentTarget.value)}
            placeholder={t("audit.replicationSourceNodePlaceholder")}
          />
        </Input.Wrapper>
        <Button size="xs" onClick={applyFilter}>
          {t("audit.applyFilter")}
        </Button>
        <Button size="xs" variant="default" onClick={resetFilter}>
          {t("audit.resetFilter")}
        </Button>
      </Group>
      {/* FR-101：滚动区与分页条共处纵向 flex 容器，分页条固定在表格下方不被裁切 */}
      <AsyncBoundary
        state={state}
        style={{ flex: 1, minHeight: 0, display: "flex", flexDirection: "column" }}
      >
        {(list) =>
          list.items.length === 0 ? (
            <EmptyState message={t("audit.replicationEmpty")} />
          ) : (
            <>
              <Box style={{ flex: 1, minHeight: 0, overflowY: "auto" }}>
                <Table striped highlightOnHover withTableBorder stickyHeader>
                  <Table.Thead>
                    <Table.Tr>
                      <Table.Th>{t("audit.replicationSourceNode")}</Table.Th>
                      <Table.Th>{t("audit.replicationSeq")}</Table.Th>
                      <Table.Th>{t("audit.replicationOp")}</Table.Th>
                      <Table.Th>{t("audit.replicationObject")}</Table.Th>
                      <Table.Th>{t("audit.replicationResult")}</Table.Th>
                      <Table.Th>{t("audit.replicationAttempts")}</Table.Th>
                      <Table.Th>{t("audit.replicationLastError")}</Table.Th>
                      <Table.Th>{t("audit.replicationTime")}</Table.Th>
                    </Table.Tr>
                  </Table.Thead>
                  <Table.Tbody>
                    {list.items.map((entry: ReplicationApplyLog) => (
                      <Table.Tr key={`${entry.sourceNode}-${entry.sourceSeq}`}>
                        <Table.Td>{entry.sourceNode || "-"}</Table.Td>
                        <Table.Td>{entry.sourceSeq}</Table.Td>
                        <Table.Td>{entry.op || "-"}</Table.Td>
                        <Table.Td>
                          <Text size="xs" style={{ wordBreak: "break-all" }}>
                            {entry.entityType}:{entry.entityKey}
                          </Text>
                        </Table.Td>
                        <Table.Td>{replicationResultBadge(entry.result)}</Table.Td>
                        <Table.Td>{entry.attemptCount}</Table.Td>
                        <Table.Td>{entry.lastError || "-"}</Table.Td>
                        <Table.Td>{new Date(entry.lastSeenAt).toLocaleString()}</Table.Td>
                      </Table.Tr>
                    ))}
                  </Table.Tbody>
                </Table>
              </Box>
              {list.total > PAGE_SIZE && (
                <Pagination
                  total={Math.ceil(list.total / PAGE_SIZE)}
                  value={page}
                  onChange={setPage}
                  size="sm"
                  style={{ flexShrink: 0 }}
                />
              )}
            </>
          )
        }
      </AsyncBoundary>
    </Stack>
  );
}

export function AuditLogPage() {
  const { t } = useTranslation();
  const [tab, setTab] = useState<string | null>("management");
  const [page, setPage] = useState(1);
  const [actor, setActor] = useState("");
  const [action, setAction] = useState("");
  const [repo, setRepo] = useState("");
  const [from, setFrom] = useState("");
  const [to, setTo] = useState("");
  const [appliedFilter, setAppliedFilter] = useState({
    actor: "",
    action: "",
    repo: "",
    from: "",
    to: "",
  });
  const state = useAsync(
    () =>
      getAuditLogs({
        actor: appliedFilter.actor || undefined,
        action: appliedFilter.action || undefined,
        repo: appliedFilter.repo || undefined,
        from: appliedFilter.from || undefined,
        to: appliedFilter.to || undefined,
        limit: PAGE_SIZE,
        offset: (page - 1) * PAGE_SIZE,
      }),
    [page, appliedFilter],
  );
  const applyFilter = useCallback(() => {
    setPage(1);
    setAppliedFilter({
      actor: actor.trim(),
      action,
      repo: repo.trim(),
      from: from ? `${from}T00:00:00.000Z` : "",
      to: to ? `${to}T23:59:59.999Z` : "",
    });
  }, [actor, action, repo, from, to]);
  const resetFilter = useCallback(() => {
    setActor("");
    setAction("");
    setRepo("");
    setFrom("");
    setTo("");
    setPage(1);
    setAppliedFilter({ actor: "", action: "", repo: "", from: "", to: "" });
  }, []);

  return (
    <Stack
      gap="sm"
      style={{
        height:
          "calc(100vh - var(--app-shell-header-offset, 56px) - 2 * var(--app-shell-padding, 12px))",
        overflow: "hidden",
      }}
    >
      <PageHeader title={t("audit.title")} description={t("audit.description")} />
      <Card
        withBorder
        radius="md"
        padding={density.cardPadding}
        style={{ flex: 1, minHeight: 0, display: "flex", overflow: "hidden" }}
      >
        <Tabs
          value={tab}
          onChange={setTab}
          style={{ flex: 1, minHeight: 0, display: "flex", flexDirection: "column" }}
        >
          <Tabs.List>
            <Tabs.Tab value="management">{t("audit.managementTab")}</Tabs.Tab>
            <Tabs.Tab value="replication">{t("audit.replicationTab")}</Tabs.Tab>
          </Tabs.List>
          <Tabs.Panel
            value="management"
            pt="sm"
            style={{ flex: 1, minHeight: 0, overflow: "hidden" }}
          >
            <Stack gap="sm" style={{ height: "100%", display: "flex", flexDirection: "column" }}>
              <Group gap="sm" align="flex-end" wrap="wrap">
                <Input.Wrapper label={t("audit.filterActor")} w={180}>
                  <Input
                    size="xs"
                    value={actor}
                    onChange={(e) => setActor(e.currentTarget.value)}
                    placeholder={t("audit.filterActorPlaceholder")}
                  />
                </Input.Wrapper>
                <Input.Wrapper label={t("audit.filterAction")} w={220}>
                  <Select
                    size="xs"
                    data={[
                      { value: "", label: t("audit.allActions") },
                      "asset.put",
                      "asset.delete",
                      "repo.create",
                      "repo.delete",
                      "acl.set",
                      "user.create",
                      "user.update",
                      "user.delete",
                      "user.password",
                      "token.create",
                      "token.revoke",
                      "setting.set",
                    ]}
                    value={action}
                    onChange={(v) => setAction(v ?? "")}
                  />
                </Input.Wrapper>
                <Input.Wrapper label={t("audit.filterRepo")} w={200}>
                  <Input
                    size="xs"
                    value={repo}
                    onChange={(e) => setRepo(e.currentTarget.value)}
                    placeholder={t("audit.filterRepoPlaceholder")}
                  />
                </Input.Wrapper>
                <Input.Wrapper label={t("audit.filterFrom")} w={190}>
                  <Input
                    type="date"
                    size="xs"
                    value={from}
                    onChange={(e) => setFrom(e.currentTarget.value)}
                  />
                </Input.Wrapper>
                <Input.Wrapper label={t("audit.filterTo")} w={190}>
                  <Input
                    type="date"
                    size="xs"
                    value={to}
                    onChange={(e) => setTo(e.currentTarget.value)}
                  />
                </Input.Wrapper>
                <Button size="xs" onClick={applyFilter}>
                  {t("audit.applyFilter")}
                </Button>
                <Button size="xs" variant="default" onClick={resetFilter}>
                  {t("audit.resetFilter")}
                </Button>
              </Group>
              {/* FR-101：滚动区与分页条共处纵向 flex 容器，分页条固定在表格下方不被裁切 */}
              <AsyncBoundary
                state={state}
                style={{ flex: 1, minHeight: 0, display: "flex", flexDirection: "column" }}
              >
                {(list) =>
                  list.items.length === 0 ? (
                    <EmptyState message={t("audit.empty")} />
                  ) : (
                    <>
                      <Box style={{ flex: 1, minHeight: 0, overflowY: "auto" }}>
                        <Table striped highlightOnHover withTableBorder stickyHeader>
                          <Table.Thead>
                            <Table.Tr>
                              <Table.Th>{t("audit.ts")}</Table.Th>
                              <Table.Th>{t("audit.actor")}</Table.Th>
                              <Table.Th>{t("audit.action")}</Table.Th>
                              <Table.Th>{t("audit.entityKey")}</Table.Th>
                              <Table.Th>{t("audit.repo")}</Table.Th>
                              <Table.Th>{t("audit.result")}</Table.Th>
                              <Table.Th>{t("audit.ip")}</Table.Th>
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
                      </Box>
                      {list.total > PAGE_SIZE && (
                        <Pagination
                          total={Math.ceil(list.total / PAGE_SIZE)}
                          value={page}
                          onChange={setPage}
                          size="sm"
                          style={{ flexShrink: 0 }}
                        />
                      )}
                    </>
                  )
                }
              </AsyncBoundary>
            </Stack>
          </Tabs.Panel>
          <Tabs.Panel
            value="replication"
            pt="sm"
            style={{ flex: 1, minHeight: 0, overflow: "hidden" }}
          >
            <ReplicationApplyLogPanel />
          </Tabs.Panel>
        </Tabs>
      </Card>
    </Stack>
  );
}
