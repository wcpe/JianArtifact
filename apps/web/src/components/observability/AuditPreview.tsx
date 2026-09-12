import { Badge, Button, Card, Drawer, Group, SimpleGrid, Stack, Text } from "@mantine/core";
import { useMemo, useState } from "react";

import {
  auditCategories,
  auditPreview,
  auditPreviewEvents,
  type AuditPreviewCategory,
  type AuditPreviewEvent,
  type PreviewRange,
} from "../../mocks/observabilityPreview";
import { density } from "../../theme/density";
import {
  bucketAuditGroups,
  groupAuditEvents,
  newestFirstAuditEvents,
  prioritizeAuditGroups,
  type AuditPreviewGroup,
} from "./auditGrouping";
import { MetricCard } from "./MetricCard";
import { PreviewRangeControls } from "./PreviewRangeControls";
import { TrendChart } from "./TrendChart";

type AuditFilter = AuditPreviewCategory | "全部事件";
type AuditView = "aggregate" | "records";

function auditMetrics(events: readonly AuditPreviewEvent[]) {
  const failureCount = events.filter((event) => event.result === "失败").length;
  const accountCount = new Set(events.map((event) => event.actor)).size;
  const riskCount = events.filter((event) => event.risk).length;

  return [
    { label: "事件总量", value: String(events.length), hint: "当前节点可追溯事件" },
    {
      label: "失败次数",
      value: String(failureCount),
      hint: "最终失败事件",
      tone: "danger" as const,
    },
    { label: "涉及账号", value: String(accountCount), hint: "按稳定身份去重" },
    { label: "高风险操作", value: String(riskCount), hint: "需重点复核" },
  ];
}

function auditSummary(events: readonly AuditPreviewEvent[]) {
  const failureCount = events.filter((event) => event.result === "失败").length;
  const riskCount = events.filter((event) => event.risk).length;
  return `文本摘要：${events.length} 条当前节点事件，其中 ${failureCount} 条失败，${riskCount} 条属于高风险操作。`;
}

function resultColor(result: AuditPreviewEvent["result"]) {
  return result === "失败" ? "red" : result === "已应用" ? "blue" : "green";
}

function attentionColor(event: AuditPreviewEvent) {
  return event.result === "失败" ? "red" : "orange";
}

function AuditCategoryFilters({
  value,
  onChange,
}: {
  value: AuditFilter;
  onChange: (value: AuditFilter) => void;
}) {
  return (
    <Group gap="xs" wrap="wrap" role="group" aria-label="事件类别">
      {auditCategories.map((category) => (
        <Button
          key={category}
          size="xs"
          variant={category === value ? "light" : "subtle"}
          aria-pressed={category === value}
          onClick={() => onChange(category)}
        >
          {category}
        </Button>
      ))}
    </Group>
  );
}

function AuditViewControls({
  value,
  onChange,
}: {
  value: AuditView;
  onChange: (value: AuditView) => void;
}) {
  return (
    <Group gap="xs" role="group" aria-label="审计视图">
      <Button
        size="xs"
        variant={value === "aggregate" ? "light" : "subtle"}
        aria-pressed={value === "aggregate"}
        onClick={() => onChange("aggregate")}
      >
        聚合视图
      </Button>
      <Button
        size="xs"
        variant={value === "records" ? "light" : "subtle"}
        aria-pressed={value === "records"}
        onClick={() => onChange("records")}
      >
        完整记录
      </Button>
    </Group>
  );
}

function AuditEventCard({ event, onOpen }: { event: AuditPreviewEvent; onOpen: () => void }) {
  return (
    <Card
      withBorder
      radius="md"
      padding="sm"
      data-audit-event-id={event.id}
      style={{ borderLeft: `4px solid var(--mantine-color-${attentionColor(event)}-6)` }}
    >
      <Group justify="space-between" align="flex-start" wrap="nowrap">
        <Stack gap={2} style={{ minWidth: 0 }}>
          <Group gap="xs" wrap="wrap">
            <Badge variant="light" color={resultColor(event.result)}>
              {event.result}
            </Badge>
            {event.risk ? (
              <Badge variant="light" color="orange">
                高风险
              </Badge>
            ) : null}
          </Group>
          <Text fw={600}>{event.action}</Text>
          <Text size="sm" c="dimmed">
            {event.target}
          </Text>
          {event.summary ? <Text size="sm">{event.summary}</Text> : null}
          <Text size="xs" c="dimmed">{`${event.actor} · ${event.authSource}`}</Text>
        </Stack>
        <Stack gap="xs" align="flex-end">
          <Text size="xs" c="dimmed">
            {event.timestamp}
          </Text>
          <Button
            size="xs"
            variant="subtle"
            aria-label={`查看${event.action}详情`}
            onClick={onOpen}
          >
            查看详情
          </Button>
        </Stack>
      </Group>
    </Card>
  );
}

function CompactSuccessEventRow({
  event,
  onOpen,
}: {
  event: AuditPreviewEvent;
  onOpen: () => void;
}) {
  return (
    <Card
      withBorder
      radius="md"
      padding="xs"
      aria-label={`普通成功记录 ${event.action}`}
      data-audit-event-id={event.id}
    >
      <Group justify="space-between" align="center" wrap="nowrap">
        <Group gap="xs" wrap="nowrap" style={{ minWidth: 0 }}>
          <Badge variant="light" color="gray">
            {event.category}
          </Badge>
          <Stack gap={0} style={{ minWidth: 0 }}>
            <Text size="sm" fw={600} truncate="end">
              {event.action}
            </Text>
            <Text size="xs" c="dimmed" truncate="end">
              {event.target}
            </Text>
          </Stack>
        </Group>
        <Group gap="xs" wrap="nowrap">
          <Text size="xs" c="dimmed" visibleFrom="sm">
            {`${event.actor} · ${event.timestamp}`}
          </Text>
          <Badge variant="light" color={resultColor(event.result)}>
            {event.result}
          </Badge>
          <Button
            size="xs"
            variant="subtle"
            aria-label={`查看${event.action}详情`}
            onClick={onOpen}
          >
            详情
          </Button>
        </Group>
      </Group>
    </Card>
  );
}

function focusEventOf(group: AuditPreviewGroup) {
  return (
    group.events.find((event) => event.result === "失败") ??
    group.events.find((event) => event.risk) ??
    group.latest
  );
}

function attentionGroupColor(group: AuditPreviewGroup) {
  return group.failureCount > 0 ? "red" : "orange";
}

function AttentionGroupCard({ group, onOpen }: { group: AuditPreviewGroup; onOpen: () => void }) {
  const event = focusEventOf(group);
  const color = attentionGroupColor(group);
  const hasRisk = group.events.some((item) => item.risk);

  return (
    <Card
      withBorder
      radius="md"
      padding="md"
      data-audit-focus-event-id={event.id}
      data-audit-group-key={group.groupKey}
      style={{ borderLeft: `4px solid var(--mantine-color-${color}-6)` }}
    >
      <Group justify="space-between" align="flex-start" wrap="nowrap">
        <Stack gap={4} style={{ minWidth: 0 }}>
          <Group gap="xs" wrap="wrap">
            <Badge variant="filled" color={color}>
              {event.result}
            </Badge>
            {hasRisk ? (
              <Badge variant="light" color="orange">
                高风险
              </Badge>
            ) : null}
            <Badge variant="light" color="gray">
              {event.category}
            </Badge>
          </Group>
          <Text fw={700}>{event.action}</Text>
          <Text size="sm">{event.target}</Text>
          {event.summary ? <Text size="sm">{event.summary}</Text> : null}
          <Text
            size="sm"
            fw={600}
          >{`成功 ${group.successCount} · 失败 ${group.failureCount} · 影响 ${group.affectedCount} 项`}</Text>
          <Text
            size="xs"
            c="dimmed"
          >{`${event.actor} · ${event.authSource} · ${event.timestamp}`}</Text>
        </Stack>
        <Button
          size="xs"
          variant="light"
          color={color}
          aria-label={`查看关注批次${event.action}关联记录`}
          onClick={onOpen}
        >
          查看详情
        </Button>
      </Group>
    </Card>
  );
}

function DetailField({ label, value }: { label: string; value: string }) {
  return (
    <Stack gap={2}>
      <Text size="xs" c="dimmed">
        {label}
      </Text>
      <Text size="sm">{value}</Text>
    </Stack>
  );
}

function AuditDetail({ event, onClose }: { event: AuditPreviewEvent | null; onClose: () => void }) {
  return (
    <Drawer
      opened={event !== null}
      onClose={onClose}
      title="事件详情"
      position="right"
      size="md"
      closeButtonProps={{ "aria-label": "关闭" }}
    >
      {event ? (
        <Stack gap="md">
          <Text size="sm" c="dimmed">
            仅展示当前节点可安全追溯的必要字段。
          </Text>
          <DetailField label="结果" value={event.result} />
          <DetailField label="操作" value={event.action} />
          <DetailField label="对象" value={event.target} />
          {event.summary ? <DetailField label="结果摘要" value={event.summary} /> : null}
          <DetailField label="时间" value={event.timestamp} />
          <DetailField label="操作者" value={event.actor} />
          <DetailField label="认证来源" value={event.authSource} />
          <DetailField label="操作标识" value={event.operationId ?? "无关联记录"} />
          <Text size="sm" c="dimmed">
            敏感字段已按当前节点审计策略脱敏。
          </Text>
        </Stack>
      ) : null}
    </Drawer>
  );
}

function AuditGroupDetail({
  group,
  onClose,
}: {
  group: AuditPreviewGroup | null;
  onClose: () => void;
}) {
  const focusEvent = group ? focusEventOf(group) : null;

  return (
    <Drawer
      opened={group !== null}
      onClose={onClose}
      title="关联记录"
      position="right"
      size="md"
      closeButtonProps={{ "aria-label": "关闭" }}
    >
      {group && focusEvent ? (
        <Stack gap="md">
          <DetailField label="结果" value={focusEvent.result} />
          <DetailField label="操作" value={focusEvent.action} />
          <DetailField label="对象" value={focusEvent.target} />
          {focusEvent.summary ? <DetailField label="结果摘要" value={focusEvent.summary} /> : null}
          <DetailField label="时间" value={focusEvent.timestamp} />
          <DetailField label="操作者" value={focusEvent.actor} />
          <DetailField label="认证来源" value={focusEvent.authSource} />
          <Text
            fw={600}
          >{`成功 ${group.successCount} · 失败 ${group.failureCount} · 影响 ${group.affectedCount} 项`}</Text>
          <Text fw={600}>{`完整记录（${group.eventCount}）`}</Text>
          <Text size="sm" c="dimmed">
            仅关联本节点记录，不表示对端完成。
          </Text>
          {group.events.map((event) => (
            <Card key={event.id} withBorder radius="md" padding="sm">
              <Stack gap={4}>
                <Group justify="space-between" wrap="wrap">
                  <Text fw={600}>{event.action}</Text>
                  <Badge variant="light" color={resultColor(event.result)}>
                    {event.result}
                  </Badge>
                </Group>
                <Text size="sm" c="dimmed">
                  {`${event.timestamp} · ${event.actor} · ${event.target}`}
                </Text>
              </Stack>
            </Card>
          ))}
          <DetailField label="操作标识" value={group.latest.operationId ?? "无关联记录"} />
          <Text size="sm" c="dimmed">
            敏感字段已按当前节点审计策略脱敏。
          </Text>
        </Stack>
      ) : null}
    </Drawer>
  );
}

function AuditTrend({
  range,
  events,
}: {
  range: PreviewRange;
  events: readonly AuditPreviewEvent[];
}) {
  const preview = auditPreview[range];
  return (
    <Card withBorder radius="md" padding={density.cardPadding}>
      <TrendChart
        title="事件与失败趋势"
        summary={auditSummary(events)}
        primary={preview.events}
        secondary={preview.failures}
        primaryLabel="事件总量"
        secondaryLabel="失败事件"
      />
    </Card>
  );
}

function AuditGroupCard({ group, onOpen }: { group: AuditPreviewGroup; onOpen: () => void }) {
  const hasRisk = group.events.some((event) => event.risk);
  const oldest = group.events[group.events.length - 1] ?? group.latest;

  return (
    <Card withBorder radius="md" padding="sm">
      <Group justify="space-between" align="flex-start" wrap="nowrap">
        <Stack gap={4} style={{ minWidth: 0 }}>
          <Group gap="xs" wrap="wrap">
            {group.categories.map((category) => (
              <Badge key={category} variant="light" color="gray">
                {category}
              </Badge>
            ))}
            {hasRisk ? (
              <Badge variant="light" color="orange">
                高风险
              </Badge>
            ) : null}
            <Text size="xs" c="dimmed">
              {`起于 ${oldest.timestamp} 至 ${group.latest.timestamp}`}
            </Text>
          </Group>
        </Stack>
        <Stack gap="xs" align="flex-end">
          <Group gap={4} justify="flex-end">
            {group.results.map((result) => (
              <Badge key={result} variant="light" color={resultColor(result)}>
                {result}
              </Badge>
            ))}
          </Group>
          <Button
            size="xs"
            variant="subtle"
            aria-label={`查看${group.latest.action}关联记录`}
            onClick={onOpen}
          >
            查看记录
          </Button>
        </Stack>
      </Group>
      <Text fw={600} mt="xs">
        {group.latest.action}
      </Text>
      <Text size="sm" c="dimmed">
        {group.latest.target}
      </Text>
      <Group gap="xs" wrap="wrap">
        <Text
          size="sm"
          fw={600}
        >{`成功 ${group.successCount} · 失败 ${group.failureCount} · 影响 ${group.affectedCount} 项`}</Text>
        <Text size="xs" c="dimmed">{`${group.latest.actor} · ${group.eventCount} 条原始记录`}</Text>
      </Group>
    </Card>
  );
}

function AttentionQueue({
  groups,
  onOpen,
}: {
  groups: AuditPreviewGroup[];
  onOpen: (group: AuditPreviewGroup) => void;
}) {
  if (groups.length === 0) {
    return (
      <Card withBorder radius="md" padding={density.cardPadding} aria-label="需要关注的记录">
        <Stack gap={2}>
          <Text fw={700}>当前没有需要关注的记录</Text>
          <Text size="sm" c="dimmed">
            当前筛选条件下没有失败或高风险的操作批次。
          </Text>
        </Stack>
      </Card>
    );
  }

  return (
    <Card withBorder radius="md" padding={density.cardPadding} aria-label="需要关注的记录">
      <Stack gap="sm">
        <Stack gap={2}>
          <Text fw={700}>需要关注的记录</Text>
          <Text size="sm" c="dimmed">
            失败批次优先，其次为高风险批次；完整记录中仍保留全部原始事件。
          </Text>
        </Stack>
        {groups.map((group) => (
          <AttentionGroupCard
            key={`${group.groupKey}:${group.bucketKey}`}
            group={group}
            onOpen={() => onOpen(group)}
          />
        ))}
      </Stack>
    </Card>
  );
}

function AggregateStream({
  groups,
  onOpen,
}: {
  groups: AuditPreviewGroup[];
  onOpen: (group: AuditPreviewGroup) => void;
}) {
  const buckets = bucketAuditGroups(groups);
  const eventCount = groups.reduce((total, group) => total + group.eventCount, 0);

  return (
    <Card withBorder radius="md" padding={density.cardPadding}>
      <Stack gap="sm">
        <Stack gap={2}>
          <Text fw={600}>按时间记录</Text>
          <Text
            size="sm"
            c="dimmed"
          >{`共 ${eventCount} 条普通成功记录，${groups.length} 个关联组`}</Text>
        </Stack>
        {buckets.map((bucket) => (
          <Stack key={bucket.key} gap="xs">
            <Text size="sm" fw={600} c="dimmed">
              {bucket.label}
            </Text>
            {bucket.groups.map((group) => (
              <AuditGroupCard
                key={`${bucket.key}:${group.groupKey}`}
                group={group}
                onOpen={() => onOpen(group)}
              />
            ))}
          </Stack>
        ))}
      </Stack>
    </Card>
  );
}

function CompleteEventStream({
  events,
  onOpen,
}: {
  events: AuditPreviewEvent[];
  onOpen: (event: AuditPreviewEvent) => void;
}) {
  return (
    <Card withBorder radius="md" padding={density.cardPadding}>
      <Stack gap="sm">
        <Stack gap={2}>
          <Text fw={600}>{`完整记录 · 共 ${events.length} 条`}</Text>
          <Text size="sm" c="dimmed">
            每条记录独立展示，便于逐项复核。
          </Text>
        </Stack>
        {events.map((event) =>
          event.result === "失败" || event.risk ? (
            <AuditEventCard key={event.id} event={event} onOpen={() => onOpen(event)} />
          ) : (
            <CompactSuccessEventRow key={event.id} event={event} onOpen={() => onOpen(event)} />
          ),
        )}
      </Stack>
    </Card>
  );
}

function groupIdentity(group: AuditPreviewGroup) {
  return `${group.groupKey}\u0000${group.bucketKey}`;
}

export function AuditPreview() {
  const [range, setRange] = useState<PreviewRange>("24h");
  const [filter, setFilter] = useState<AuditFilter>("全部事件");
  const [view, setView] = useState<AuditView>("aggregate");
  const [selected, setSelected] = useState<AuditPreviewEvent | null>(null);
  const [selectedGroup, setSelectedGroup] = useState<AuditPreviewGroup | null>(null);
  const events = useMemo(
    () =>
      filter === "全部事件"
        ? auditPreviewEvents
        : auditPreviewEvents.filter((event) => event.category === filter),
    [filter],
  );
  const orderedEvents = useMemo(() => newestFirstAuditEvents(events), [events]);
  const groups = useMemo(() => groupAuditEvents(events, range), [events, range]);
  const attentionGroups = useMemo(() => prioritizeAuditGroups(groups), [groups]);
  const attentionGroupKeys = useMemo(
    () => new Set(attentionGroups.map(groupIdentity)),
    [attentionGroups],
  );
  const ordinaryGroups = useMemo(
    () => groups.filter((group) => !attentionGroupKeys.has(groupIdentity(group))),
    [attentionGroupKeys, groups],
  );

  return (
    <Stack gap={density.gridSpacing}>
      <Group justify="space-between" align="flex-start" wrap="wrap">
        <AuditCategoryFilters value={filter} onChange={setFilter} />
        <Group gap="xs" wrap="wrap">
          <AuditViewControls value={view} onChange={setView} />
          <PreviewRangeControls value={range} onChange={setRange} />
        </Group>
      </Group>
      <SimpleGrid cols={{ base: 1, xs: 2, lg: 4 }} spacing={density.gridSpacing}>
        {auditMetrics(events).map((metric) => (
          <MetricCard key={metric.label} {...metric} />
        ))}
      </SimpleGrid>
      {view === "aggregate" ? (
        <>
          <AttentionQueue groups={attentionGroups} onOpen={setSelectedGroup} />
          <AggregateStream groups={ordinaryGroups} onOpen={setSelectedGroup} />
        </>
      ) : (
        <CompleteEventStream events={orderedEvents} onOpen={setSelected} />
      )}
      <AuditTrend range={range} events={events} />
      <AuditDetail event={selected} onClose={() => setSelected(null)} />
      <AuditGroupDetail group={selectedGroup} onClose={() => setSelectedGroup(null)} />
    </Stack>
  );
}
