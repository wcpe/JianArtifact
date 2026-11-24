import { Badge, Card, Divider, Group, Loader, SimpleGrid, Stack, Table, Tabs, Text, Title } from "@mantine/core";
import { useCallback, useEffect, useState } from "react";
import { useParams } from "react-router-dom";
import {
  getSecurityCase,
  getSecurityCaseSection,
  markSecurityCaseRead,
  type SecurityCaseDetail,
  type SecurityCaseSectionKind,
} from "../api/endpoints";

interface SectionRows { items?: Record<string, unknown>[]; findings?: Record<string, unknown>[]; holds?: Record<string, unknown>[] }

const SEV_COLOR: Record<string, string> = { low: "gray", medium: "blue", high: "orange", critical: "red" };

function kv(label: string, value: unknown): { label: string; value: string } {
  return { label, value: value == null || value === "" ? "-" : String(value) };
}

function MiniTable({ cols, rows }: { cols: { label: string; key: string }[]; rows: Record<string, unknown>[] }) {
  if (rows.length === 0) return <Text size="sm" c="dimmed">暂无记录</Text>;
  return (
    <Table striped highlightOnHover>
      <Table.Thead>
        <Table.Tr>{cols.map((c) => <Table.Th key={c.key}>{c.label}</Table.Th>)}</Table.Tr>
      </Table.Thead>
      <Table.Tbody>
        {rows.map((r, i) => (
          <Table.Tr key={i}>
            {cols.map((c) => <Table.Td key={c.key}><Text size="xs" style={{ wordBreak: "break-all" }}>{kv("", r[c.key]).value}</Text></Table.Td>)}
          </Table.Tr>
        ))}
      </Table.Tbody>
    </Table>
  );
}

// FR-171 案件详情页：头部 + 关联 + 四栏（timeline/uploads/identity/actions-evidence）。
export function SecurityCaseDetailPage() {
  const { caseId } = useParams<{ caseId: string }>();
  const [detail, setDetail] = useState<SecurityCaseDetail | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [sections, setSections] = useState<Partial<Record<SecurityCaseSectionKind, SectionRows>>>({});
  const [loading, setLoading] = useState<SecurityCaseSectionKind | null>(null);

  const loadSection = useCallback(async (s: SecurityCaseSectionKind) => {
    if (!caseId) return;
    setLoading(s);
    try {
      const rows = (await getSecurityCaseSection(caseId, s)) as SectionRows;
      setSections((prev) => ({ ...prev, [s]: rows }));
    } catch {
      setSections((prev) => ({ ...prev, [s]: { items: [] } }));
    } finally {
      setLoading(null);
    }
  }, [caseId]);

  useEffect(() => {
    if (!caseId) return;
    getSecurityCase(caseId).then(async (d) => {
      setDetail(d);
      // 详情落地即记录既読水位（静默失败不影响浏览）。
      void markSecurityCaseRead(caseId, { seenVersion: d.case.targetVersion }).catch(() => undefined);
      void loadSection("timeline");
    }).catch((e: Error) => setError(e.message));
  }, [caseId, loadSection]);

  if (error) return <Text c="red">加载失败：{error}</Text>;
  if (!detail) return <Loader />;

  const c = detail.case;
  const items = (s: SecurityCaseSectionKind) => sections[s]?.items ?? [];

  return (
    <Stack>
      <Group>
        <Title order={3}>案件 {c.caseId}</Title>
        <Badge color={SEV_COLOR[c.severity] ?? "gray"}>{c.severity}</Badge>
        <Badge variant="outline">{c.state}</Badge>
      </Group>
      <Card withBorder>
        <SimpleGrid cols={{ base: 1, sm: 2 }}>
          <Text size="sm">标题：{c.title}</Text>
          <Text size="sm">原因：{c.reason ?? "-"}</Text>
          <Text size="sm">创建：{c.createdUnix ? new Date(c.createdUnix * 1000).toLocaleString() : "-"}</Text>
          <Text size="sm">版本：v{c.targetVersion}</Text>
        </SimpleGrid>
      </Card>

      <Divider label="规则命中与关联事件" />
      <MiniTable
        cols={[{ label: "规则", key: "ruleId" }, { label: "级别", key: "severity" }, { label: "主体", key: "subjectKey" }, { label: "摘要", key: "summary" }]}
        rows={detail.findings as unknown as Record<string, unknown>[]}
      />
      <MiniTable
        cols={[{ label: "事件", key: "eventId" }, { label: "关联类型", key: "linkKind" }, { label: "关联人", key: "linkedBy" }]}
        rows={detail.links as unknown as Record<string, unknown>[]}
      />

      <Tabs defaultValue="timeline" onChange={(v) => { if (v && !sections[v as SecurityCaseSectionKind]) void loadSection(v as SecurityCaseSectionKind); }}>
        <Tabs.List>
          <Tabs.Tab value="timeline">时间线</Tabs.Tab>
          <Tabs.Tab value="uploads">对象</Tabs.Tab>
          <Tabs.Tab value="identity">身份来源</Tabs.Tab>
          <Tabs.Tab value="actions-evidence">处置与保全</Tabs.Tab>
        </Tabs.List>
        <Tabs.Panel value="timeline" pt="sm">
          {loading === "timeline" ? <Loader size="sm" /> : <MiniTable
            cols={[{ label: "时间", key: "occurredAt" }, { label: "事件", key: "eventType" }, { label: "结果", key: "outcome" }, { label: "错误", key: "errorCode" }, { label: "来源", key: "clientIp" }, { label: "关联", key: "linkKind" }]}
            rows={items("timeline")} />}
        </Tabs.Panel>
        <Tabs.Panel value="uploads" pt="sm">
          {loading === "uploads" ? <Loader size="sm" /> : <MiniTable
            cols={[{ label: "路径", key: "canonicalPath" }, { label: "仓库", key: "repository" }, { label: "摘要", key: "digest" }, { label: "大小", key: "sizeBytes" }, { label: "版本", key: "targetVersion" }]}
            rows={items("uploads")} />}
        </Tabs.Panel>
        <Tabs.Panel value="identity" pt="sm">
          {loading === "identity" ? <Loader size="sm" /> : <MiniTable
            cols={[{ label: "主体", key: "actorName" }, { label: "凭据", key: "credentialId" }, { label: "认证", key: "authMethod" }, { label: "来源", key: "clientIp" }, { label: "事件数", key: "events" }]}
            rows={items("identity")} />}
        </Tabs.Panel>
        <Tabs.Panel value="actions-evidence" pt="sm">
          {loading === "actions-evidence" ? <Loader size="sm" /> : <>
            <MiniTable
              cols={[{ label: "命中", key: "findingId" }, { label: "规则", key: "ruleId" }, { label: "级别", key: "severity" }, { label: "摘要", key: "summary" }]}
              rows={sections["actions-evidence"]?.findings ?? []} />
            <MiniTable
              cols={[{ label: "保全", key: "holdId" }, { label: "摘要", key: "digest" }, { label: "字节", key: "byteState" }, { label: "状态", key: "state" }]}
              rows={sections["actions-evidence"]?.holds ?? []} />
          </>}
        </Tabs.Panel>
      </Tabs>
    </Stack>
  );
}
