// 安全治理 风险调查页：安全事件队列（多维度关联筛选）+ 案件列表与认领/结案。
import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { Alert, Anchor, Box, Button, Table, Text, TextInput } from "@mantine/core";
import { IconAlertTriangle, IconRefresh, IconSearch } from "@tabler/icons-react";
import {
  getSecurityCases,
  getSecurityEvents,
  getSecurityFindings,
  type SecurityFinding,
  transitionSecurityCase,
  type SecurityCase,
  type SecurityEvent,
} from "../api/endpoints";

export function SecurityPage() {
  const [events, setEvents] = useState<SecurityEvent[]>([]);
  const [cases, setCases] = useState<SecurityCase[]>([]);
  const [findings, setFindings] = useState<SecurityFinding[]>([]);
  const [failed, setFailed] = useState<string>("");
  const [filter, setFilter] = useState({ ip: "", eventType: "", digest: "", outcome: "" });

  const load = useCallback(async () => {
    try {
      const to = new Date();
      const from = new Date(to.getTime() - 24 * 3600 * 1000);
      const [ev, cs, fd] = await Promise.all([
        getSecurityEvents({
          from: from.toISOString(),
          to: to.toISOString(),
          ip: filter.ip || undefined,
          eventType: filter.eventType || undefined,
          digest: filter.digest || undefined,
          outcome: filter.outcome || undefined,
          limit: 50,
        }),
        getSecurityCases(),
        getSecurityFindings(),
      ]);
      setEvents(ev.items);
      setCases(cs);
      setFindings(fd);
      setFailed("");
    } catch {
      setFailed("安全数据加载失败");
    }
  }, [filter]);

  useEffect(() => {
    void load();
  }, [load]);

  const act = async (c: SecurityCase, action: "claim" | "close") => {
    try {
      await transitionSecurityCase(c.caseId, {
        action,
        targetVersion: c.targetVersion,
        reason: action === "close" ? "人工结案" : undefined,
        resolutionCode: action === "close" ? "resolved" : undefined,
      });
      await load();
    } catch {
      setFailed("案件操作失败（版本漂移或权限不足）");
    }
  };

  return (
    <Box p="md">
      {failed ? (
        <Alert color="yellow" variant="light" icon={<IconAlertTriangle size={16} />} mb="sm">
          {failed}
        </Alert>
      ) : null}

      <Box mb="md" style={{ display: "flex", gap: 8, flexWrap: "wrap" }}>
        <TextInput placeholder="IP" size="xs" value={filter.ip}
          onChange={(e) => setFilter({ ...filter, ip: e.currentTarget.value })} />
        <TextInput placeholder="事件类型" size="xs" value={filter.eventType}
          onChange={(e) => setFilter({ ...filter, eventType: e.currentTarget.value })} />
        <TextInput placeholder="摘要" size="xs" value={filter.digest}
          onChange={(e) => setFilter({ ...filter, digest: e.currentTarget.value })} />
        <TextInput placeholder="结果" size="xs" value={filter.outcome}
          onChange={(e) => setFilter({ ...filter, outcome: e.currentTarget.value })} />
        <Button size="xs" variant="subtle" leftSection={<IconRefresh size={14} />} onClick={() => void load()}>
          刷新
        </Button>
      </Box>

      <Text fw={600} mb="xs">案件</Text>
      <Table striped verticalSpacing={6} mb="lg" miw={720}>
        <Table.Thead>
          <Table.Tr>
            <Table.Th>案件</Table.Th><Table.Th>严重度</Table.Th><Table.Th>状态</Table.Th>
            <Table.Th>版本</Table.Th><Table.Th>操作</Table.Th>
          </Table.Tr>
        </Table.Thead>
        <Table.Tbody>
          {cases.length === 0 ? (
            <Table.Tr><Table.Td colSpan={5}><Text size="sm" c="dimmed">无案件</Text></Table.Td></Table.Tr>
          ) : cases.map((c) => (
            <Table.Tr key={c.caseId}>
              <Table.Td><Anchor component={Link} to={`/security/cases/${c.caseId}`}><Text size="sm">{c.title}</Text></Anchor></Table.Td>
              <Table.Td>{c.severity}</Table.Td>
              <Table.Td>{c.state}</Table.Td>
              <Table.Td>{c.targetVersion}</Table.Td>
              <Table.Td>
                {c.state === "open" && (
                  <Button size="xs" variant="light" leftSection={<IconSearch size={12} />} onClick={() => void act(c, "claim")}>认领</Button>
                )}
                {c.state === "claimed" && (
                  <Button size="xs" variant="light" color="red" onClick={() => void act(c, "close")}>结案</Button>
                )}
              </Table.Td>
            </Table.Tr>
          ))}
        </Table.Tbody>
      </Table>

      <Text fw={600} mb="xs">规则命中</Text>
      <Table striped verticalSpacing={6} mb="lg" miw={720}>
        <Table.Thead>
          <Table.Tr>
            <Table.Th>规则</Table.Th><Table.Th>严重度</Table.Th><Table.Th>主体</Table.Th>
            <Table.Th>说明</Table.Th><Table.Th>案件</Table.Th>
          </Table.Tr>
        </Table.Thead>
        <Table.Tbody>
          {findings.length === 0 ? (
            <Table.Tr><Table.Td colSpan={5}><Text size="sm" c="dimmed">无命中</Text></Table.Td></Table.Tr>
          ) : findings.map((f) => (
            <Table.Tr key={f.findingId}>
              <Table.Td><Text size="xs">{f.ruleId} v{f.ruleVersion}</Text></Table.Td>
              <Table.Td>{f.severity}</Table.Td>
              <Table.Td><Text size="xs">{f.subjectKey}</Text></Table.Td>
              <Table.Td><Text size="xs">{f.summary}</Text></Table.Td>
              <Table.Td><Text size="xs">{f.caseId ?? "-"}</Text></Table.Td>
            </Table.Tr>
          ))}
        </Table.Tbody>
      </Table>

      <Text fw={600} mb="xs">安全事件（近 24 小时）</Text>
      <Table striped verticalSpacing={6} stickyHeader miw={720}>
        <Table.Thead>
          <Table.Tr>
            <Table.Th>时间</Table.Th><Table.Th>类型</Table.Th><Table.Th>结果</Table.Th>
            <Table.Th>主体</Table.Th><Table.Th>IP</Table.Th><Table.Th>错误码</Table.Th>
          </Table.Tr>
        </Table.Thead>
        <Table.Tbody>
          {events.length === 0 ? (
            <Table.Tr><Table.Td colSpan={6}><Text size="sm" c="dimmed">窗口内无事件</Text></Table.Td></Table.Tr>
          ) : events.map((e) => (
            <Table.Tr key={e.eventId}>
              <Table.Td><Text size="xs">{e.occurredAt ?? "-"}</Text></Table.Td>
              <Table.Td><Text size="xs">{e.eventType}</Text></Table.Td>
              <Table.Td>{e.outcome ?? "-"}</Table.Td>
              <Table.Td>{e.actorNameSnapshot ?? e.actorUserId ?? "-"}</Table.Td>
              <Table.Td>{e.clientIp ?? "-"}</Table.Td>
              <Table.Td>{e.errorCode ?? "-"}</Table.Td>
            </Table.Tr>
          ))}
        </Table.Tbody>
      </Table>
    </Box>
  );
}
