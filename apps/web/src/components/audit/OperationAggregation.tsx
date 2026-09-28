// 操作聚合视图：把当前结果集内的事件聚合成「一次完整操作」（上传 / 删除 / 移动 / 其他），
// 先按**操作类型分区**，再在分区内按 仓库 → groupId → artifactId → 版本（或目录）→ 文件 展开。
//
// 与事件流列表并存，由 RecordsStream 顶部的视图切换器选择；同一结果集、同一筛选口径，只换呈现方式。
// 归并逻辑全部复用 `operations.buildOperations`（纯函数，单独测试）；本组件只负责组装展示树，
// 并复用 `uploadTree` 的坐标反解（parseUploadCoordinates / uploadArtifactTarget / artifactSearchText），
// 不重写解析逻辑。
//
// 非制品操作（仓库 / 用户 / 设置等）归入「其他」，直接以动作名展示，**不强行编造坐标**。
import { useMemo, useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import {
  Badge,
  Box,
  Button,
  Divider,
  Group,
  SegmentedControl,
  Stack,
  Text,
  UnstyledButton,
} from "@mantine/core";
import {
  IconChevronDown,
  IconChevronRight,
  IconDatabase,
  IconExternalLink,
  IconFile,
} from "@tabler/icons-react";

import type { AuditEvent } from "../../api/types";
import { actionLabel, formatShortTime, resultColor, resultLabelKey } from "./labels";
import { artifactSearchText, parseUploadCoordinates, uploadArtifactTarget } from "./uploadTree";
import {
  buildOperations,
  operationFileCount,
  type AuditOperation,
  type OperationCoordinates,
  type OperationKind,
} from "./operations";

interface OperationAggregationProps {
  /** 当前查询结果集内的事件（当前页）。 */
  events: readonly AuditEvent[];
  /** 结果集是否仍在加载（空态时用于区分「加载中」与「本就没有」）。 */
  loading: boolean;
  /** 点击文件叶子时把关键字（后端 EntityKey `repo/path`）交给事件流调查。 */
  onJumpToEvent: (keyword: string) => void;
}

/** 类型筛选值：`all` 或某一操作类型。 */
type KindFilter = "all" | OperationKind;

/** 分区展示顺序：固定为 上传 → 删除 → 移动 → 其他。 */
const KIND_ORDER: readonly OperationKind[] = ["upload", "delete", "move", "other"];

/** 操作类型 → i18n 显示名键。 */
const KIND_LABEL_KEYS: Record<OperationKind, string> = {
  upload: "auditWorkbench.opKindUpload",
  delete: "auditWorkbench.opKindDelete",
  move: "auditWorkbench.opKindMove",
  other: "auditWorkbench.opKindOther",
};

/** Maven 坐标分段的分支徽章样式：groupId / artifactId / version 各用一色，便于区分。 */
const SEGMENT_STYLE: Record<"group" | "artifact" | "version", { color: string; caption: string }> =
  {
    group: { color: "grape", caption: "groupId" },
    artifact: { color: "blue", caption: "artifactId" },
    version: { color: "teal", caption: "version" },
  };

/** 单个操作默认预览的文件数；超出部分折叠，避免一次 deploy 的几十个文件淹没层级。 */
const FILE_PREVIEW_LIMIT = 5;

/** 树节点角色：仓库 / 坐标分段 / 目录 / 操作 / 文件 / 无仓库的散落操作。 */
type NodeRole =
  "repo" | "group" | "artifact" | "version" | "directory" | "operation" | "file" | "loose";

interface MutableNode {
  key: string;
  role: NodeRole;
  label: string;
  /** 子树内的文件数（= 事件数）。 */
  files: number;
  /** 子树内的操作数。 */
  ops: number;
  /** 子树内最新事件时间（毫秒；无样本为 0）。 */
  latestAt: number;
  operation?: AuditOperation;
  event?: AuditEvent;
  children: Map<string, MutableNode>;
}

interface DisplayNode extends Omit<MutableNode, "children"> {
  children: DisplayNode[];
}

/** 文件叶子：展示文件名，点击跳回事件流调查该文件。 */
function fileNode(event: AuditEvent, prefix: string): MutableNode {
  const target = uploadArtifactTarget(event);
  const filename = target ? parseUploadCoordinates(target.path).filename : "";
  return {
    key: `${prefix}/file:${event.eventId}`,
    role: "file",
    label: filename || artifactSearchText(event) || "—",
    files: 1,
    ops: 0,
    latestAt: 0,
    event,
    children: new Map(),
  };
}

/**
 * 操作 → 层级链（含仓库，不含操作与文件节点）。
 *
 * Maven 坐标齐全时走 groupId → artifactId → 版本；反解不出 GAV 时降级到目录；
 * 无仓库的非制品操作返回空数组（由调用方直接挂到分区根下，不编造层级）。
 */
function coordinateChain(
  operation: AuditOperation,
): Array<{ key: string; role: NodeRole; label: string }> {
  if (!operation.repo) return [];
  const steps: Array<{ key: string; role: NodeRole; label: string }> = [
    { key: `repo:${operation.repo}`, role: "repo", label: operation.repo },
  ];
  const coords: OperationCoordinates | null = operation.coordinates;
  if (coords?.groupId && coords.artifactId && coords.version) {
    steps.push({ key: `group:${coords.groupId}`, role: "group", label: coords.groupId });
    steps.push({
      key: `artifact:${coords.artifactId}`,
      role: "artifact",
      label: coords.artifactId,
    });
    steps.push({ key: `version:${coords.version}`, role: "version", label: coords.version });
  } else if (coords?.directory) {
    steps.push({ key: `dir:${coords.directory}`, role: "directory", label: coords.directory });
  }
  return steps;
}

/** 把某一分区的操作组装成展示树（仓库 / 坐标分段为分支，操作与文件为叶子）。 */
function buildKindTree(operations: readonly AuditOperation[]): DisplayNode[] {
  const root = new Map<string, MutableNode>();

  for (const operation of operations) {
    const opKey = `op:${operation.key}`;
    const opNode: MutableNode = {
      key: opKey,
      role: "operation",
      label: operation.action,
      files: operationFileCount(operation),
      ops: 1,
      latestAt: operation.latestAt,
      operation,
      children: new Map(),
    };
    for (const event of operation.events) {
      opNode.children.set(event.eventId, fileNode(event, opKey));
    }

    const chain = coordinateChain(operation);
    if (chain.length === 0) {
      // 非制品操作：直接挂在分区根下（role=loose），以动作名标识。
      root.set(opKey, { ...opNode, role: "loose" });
      continue;
    }

    let map = root;
    let prefix = "";
    const path: MutableNode[] = [];
    for (const step of chain) {
      const key = `${prefix}/${step.key}`;
      let node = map.get(key);
      if (!node) {
        node = {
          key,
          role: step.role,
          label: step.label,
          files: 0,
          ops: 0,
          latestAt: 0,
          children: new Map(),
        };
        map.set(key, node);
      }
      path.push(node);
      map = node.children;
      prefix = key;
    }
    map.set(opKey, opNode);
    // 祖先分支累计子树的文件数 / 操作数与最新时间（操作节点自身已带计数）。
    for (const node of path) {
      node.files += opNode.files;
      node.ops += 1;
      if (opNode.latestAt > node.latestAt) node.latestAt = opNode.latestAt;
    }
  }

  return finalize([...root.values()]);
}

function finalize(nodes: MutableNode[]): DisplayNode[] {
  return nodes
    .map((node) => ({
      key: node.key,
      role: node.role,
      label: node.label,
      files: node.files,
      ops: node.ops,
      latestAt: node.latestAt,
      operation: node.operation,
      event: node.event,
      children: finalize([...node.children.values()]),
    }))
    .sort(compareNodes);
}

function compareNodes(a: DisplayNode, b: DisplayNode): number {
  // 无仓库的散落操作恒排最后，让有层级的制品操作排前面。
  if (a.role === "loose" && b.role !== "loose") return 1;
  if (b.role === "loose" && a.role !== "loose") return -1;
  // 版本与操作按最新时间倒序（最近发布的排前），比字符串排序更符合直觉。
  if (
    (a.role === "version" || a.role === "operation") &&
    (b.role === "version" || b.role === "operation") &&
    a.latestAt !== b.latestAt
  ) {
    return b.latestAt - a.latestAt;
  }
  return a.label < b.label ? -1 : a.label > b.label ? 1 : 0;
}

export function OperationAggregation({
  events,
  loading,
  onJumpToEvent,
}: OperationAggregationProps) {
  const { t } = useTranslation();
  const operations = useMemo(() => buildOperations(events), [events]);
  const [filter, setFilter] = useState<KindFilter>("all");
  // 默认展开所有分区分支，仅记录被折叠的节点，避免大数据集下一开始就隐藏内容。
  const [collapsedSections, setCollapsedSections] = useState<ReadonlySet<string>>(new Set());
  const [collapsedNodes, setCollapsedNodes] = useState<ReadonlySet<string>>(new Set());
  // 单个操作默认只预览前 N 个文件，展开态记在此处。
  const [expandedFiles, setExpandedFiles] = useState<ReadonlySet<string>>(new Set());

  const toggleIn = (
    set: ReadonlySet<string>,
    updater: (next: Set<string>) => void,
    key: string,
  ) => {
    const next = new Set(set);
    if (next.has(key)) next.delete(key);
    else next.add(key);
    updater(next);
  };

  const sections = useMemo(() => {
    const byKind = new Map<OperationKind, AuditOperation[]>();
    for (const operation of operations) {
      const bucket = byKind.get(operation.kind);
      if (bucket) bucket.push(operation);
      else byKind.set(operation.kind, [operation]);
    }
    return KIND_ORDER.filter((kind) => filter === "all" || filter === kind)
      .map((kind) => {
        const ops = byKind.get(kind) ?? [];
        return {
          kind,
          ops: ops.length,
          files: ops.reduce((total, operation) => total + operationFileCount(operation), 0),
          nodes: buildKindTree(ops),
        };
      })
      .filter((section) => section.ops > 0);
  }, [operations, filter]);

  const totalOps = sections.reduce((total, section) => total + section.ops, 0);
  const totalFiles = sections.reduce((total, section) => total + section.files, 0);

  if (operations.length === 0) {
    return (
      <Text size="sm" c="dimmed" py="lg" px="md" data-testid="audit-op-empty">
        {loading ? t("auditWorkbench.loading") : t("auditWorkbench.opEmpty")}
      </Text>
    );
  }

  return (
    <Stack gap="sm" py="xs" data-testid="audit-op-aggregation">
      <Group
        justify="space-between"
        align="center"
        gap="sm"
        wrap="wrap"
        data-testid="audit-op-header"
      >
        <SegmentedControl
          size="xs"
          data-testid="audit-op-filter"
          aria-label={t("auditWorkbench.opFilterLabel")}
          value={filter}
          onChange={(value) => setFilter(value as KindFilter)}
          data={[
            { value: "all", label: t("auditWorkbench.opFilterAll") },
            ...KIND_ORDER.map((kind) => ({ value: kind, label: t(KIND_LABEL_KEYS[kind]) })),
          ]}
        />
        <Text size="xs" c="dimmed" data-testid="audit-op-summary">
          {t("auditWorkbench.opSummary", { ops: totalOps, files: totalFiles })}
        </Text>
      </Group>

      {sections.length === 0 ? (
        <Text size="sm" c="dimmed" py="lg" px="md">
          {t("auditWorkbench.opEmpty")}
        </Text>
      ) : (
        sections.map((section) => {
          const collapsed = collapsedSections.has(section.kind);
          return (
            <Box key={section.kind} data-testid={`audit-op-section-${section.kind}`}>
              <Divider mb={4} />
              <UnstyledButton
                onClick={() => toggleIn(collapsedSections, setCollapsedSections, section.kind)}
                aria-expanded={!collapsed}
                style={{ width: "100%" }}
              >
                <Group gap="xs" py={4}>
                  {collapsed ? <IconChevronRight size={14} /> : <IconChevronDown size={14} />}
                  <Text size="sm" fw={600}>
                    {t(KIND_LABEL_KEYS[section.kind])}
                  </Text>
                  <Text size="xs" c="dimmed">
                    {t("auditWorkbench.opSectionTitle", {
                      ops: section.ops,
                      files: section.files,
                    })}
                  </Text>
                </Group>
              </UnstyledButton>
              {collapsed ? null : (
                <Stack gap={1} pt={2}>
                  {section.nodes.map((node) => (
                    <AggNode
                      key={node.key}
                      node={node}
                      depth={1}
                      collapsed={collapsedNodes}
                      expandedFiles={expandedFiles}
                      onToggleNode={(key) => toggleIn(collapsedNodes, setCollapsedNodes, key)}
                      onToggleFiles={(key) => toggleIn(expandedFiles, setExpandedFiles, key)}
                      onJumpToEvent={onJumpToEvent}
                    />
                  ))}
                </Stack>
              )}
            </Box>
          );
        })
      )}
    </Stack>
  );
}

function AggNode({
  node,
  depth,
  collapsed,
  expandedFiles,
  onToggleNode,
  onToggleFiles,
  onJumpToEvent,
}: {
  node: DisplayNode;
  depth: number;
  collapsed: ReadonlySet<string>;
  expandedFiles: ReadonlySet<string>;
  onToggleNode: (key: string) => void;
  onToggleFiles: (key: string) => void;
  onJumpToEvent: (keyword: string) => void;
}) {
  const { t } = useTranslation();

  if (node.role === "file" && node.event) {
    const event = node.event;
    return (
      <Box style={{ paddingLeft: depth * 16 }} data-testid="audit-op-file">
        <UnstyledButton
          onClick={() => onJumpToEvent(artifactSearchText(event))}
          aria-label={node.label}
          style={{ display: "block", width: "100%" }}
        >
          <Group gap={6} wrap="nowrap" align="center" py={2} px="xs">
            <Text size="xs" c="dimmed" style={{ flexShrink: 0 }}>
              ↳
            </Text>
            <IconFile size={14} style={{ flexShrink: 0 }} />
            <Text size="xs" style={{ fontFamily: "monospace", overflowWrap: "anywhere" }}>
              {node.label}
            </Text>
            <IconExternalLink size={12} style={{ flexShrink: 0, marginLeft: "auto" }} />
          </Group>
        </UnstyledButton>
      </Box>
    );
  }

  if (node.role === "operation" && node.operation) {
    const operation = node.operation;
    const hasChildren = node.children.length > 0;
    const isCollapsed = collapsed.has(node.key);
    const label = actionLabel(operation.action, t);
    const visibleFiles = expandedFiles.has(node.key)
      ? node.children
      : node.children.slice(0, FILE_PREVIEW_LIMIT);
    const hiddenFiles = node.children.length - visibleFiles.length;
    return (
      <Box style={{ paddingLeft: depth * 16 }}>
        <Group
          gap={6}
          wrap="nowrap"
          align="center"
          py={3}
          px="xs"
          data-testid="audit-op-leaf"
          data-op-kind={operation.kind}
        >
          {hasChildren ? (
            <UnstyledButton
              onClick={() => onToggleNode(node.key)}
              aria-label={
                isCollapsed ? t("auditWorkbench.opExpand") : t("auditWorkbench.opCollapse")
              }
              aria-expanded={!isCollapsed}
              style={{ display: "flex", alignItems: "center" }}
            >
              {isCollapsed ? <IconChevronRight size={14} /> : <IconChevronDown size={14} />}
            </UnstyledButton>
          ) : (
            <Box w={14} style={{ flexShrink: 0 }} />
          )}
          <Badge
            size="xs"
            variant="light"
            color={resultColor(operation.result)}
            style={{ flexShrink: 0 }}
          >
            {t(resultLabelKey(operation.result))}
          </Badge>
          <Text size="xs" fw={600} style={{ flexShrink: 0 }}>
            {label}
          </Text>
          {operation.latestAt ? (
            <Text size="xs" c="dimmed" style={{ flexShrink: 0 }}>
              {formatShortTime(new Date(operation.latestAt).toISOString())}
            </Text>
          ) : null}
          <Text size="xs" c="dimmed" truncate>
            {operation.actor}
          </Text>
          <Badge size="xs" variant="light" color="gray" ml="auto" style={{ flexShrink: 0 }}>
            {t("auditWorkbench.opFileCount", { count: node.files })}
          </Badge>
        </Group>

        {hasChildren && !isCollapsed ? (
          <>
            {visibleFiles.map((child) => (
              <AggNode
                key={child.key}
                node={child}
                depth={depth + 1}
                collapsed={collapsed}
                expandedFiles={expandedFiles}
                onToggleNode={onToggleNode}
                onToggleFiles={onToggleFiles}
                onJumpToEvent={onJumpToEvent}
              />
            ))}
            {hiddenFiles > 0 ? (
              <Box style={{ paddingLeft: (depth + 1) * 16 }}>
                <Button
                  size="compact-xs"
                  variant="subtle"
                  color="gray"
                  px="xs"
                  onClick={() => onToggleFiles(node.key)}
                >
                  {t("auditWorkbench.opMoreFiles", { count: hiddenFiles })}
                </Button>
              </Box>
            ) : null}
            {expandedFiles.has(node.key) && node.children.length > FILE_PREVIEW_LIMIT ? (
              <Box style={{ paddingLeft: (depth + 1) * 16 }}>
                <Button
                  size="compact-xs"
                  variant="subtle"
                  color="gray"
                  px="xs"
                  onClick={() => onToggleFiles(node.key)}
                >
                  {t("auditWorkbench.opCollapse")}
                </Button>
              </Box>
            ) : null}
          </>
        ) : null}
      </Box>
    );
  }

  // 分支节点：仓库 / 坐标分段 / 目录 / 无仓库的散落操作。
  const hasChildren = node.children.length > 0;
  const isCollapsed = collapsed.has(node.key);
  const countText = t("auditWorkbench.opFileCount", { count: node.files });
  return (
    <Box style={{ paddingLeft: depth * 16 }}>
      <Group gap={6} wrap="nowrap" align="center" py={3} px="xs">
        {hasChildren ? (
          <UnstyledButton
            onClick={() => onToggleNode(node.key)}
            aria-label={isCollapsed ? t("auditWorkbench.opExpand") : t("auditWorkbench.opCollapse")}
            aria-expanded={!isCollapsed}
            style={{ display: "flex", alignItems: "center" }}
          >
            {isCollapsed ? <IconChevronRight size={14} /> : <IconChevronDown size={14} />}
          </UnstyledButton>
        ) : (
          <Box w={14} style={{ flexShrink: 0 }} />
        )}

        {renderBranchIdentity(node, t)}

        <Badge size="xs" variant="light" color="gray" ml="auto" style={{ flexShrink: 0 }}>
          {node.role === "loose" ? null : countText}
        </Badge>
      </Group>

      {hasChildren && !isCollapsed
        ? node.children.map((child) => (
            <AggNode
              key={child.key}
              node={child}
              depth={depth + 1}
              collapsed={collapsed}
              expandedFiles={expandedFiles}
              onToggleNode={onToggleNode}
              onToggleFiles={onToggleFiles}
              onJumpToEvent={onJumpToEvent}
            />
          ))
        : null}
    </Box>
  );
}

/** 分支身份展示：仓库名 / Maven 坐标分段徽章 / 目录路径 / 无仓库操作的动作名。 */
function renderBranchIdentity(node: DisplayNode, t: (key: string) => string): ReactNode {
  switch (node.role) {
    case "repo":
      return (
        <>
          <IconDatabase size={16} style={{ flexShrink: 0 }} />
          <Text size="sm" fw={600} truncate>
            {node.label}
          </Text>
        </>
      );
    case "group":
    case "artifact":
    case "version": {
      const style = SEGMENT_STYLE[node.role];
      return (
        <>
          <Text size="xs" c="dimmed" style={{ flexShrink: 0 }}>
            {style.caption}
          </Text>
          <Badge
            size="sm"
            variant="light"
            color={style.color}
            style={{ fontFamily: "monospace", textTransform: "none" }}
          >
            {node.label}
          </Badge>
        </>
      );
    }
    case "directory":
      return (
        <Text size="xs" style={{ fontFamily: "monospace", overflowWrap: "anywhere" }}>
          {node.label}
        </Text>
      );
    case "loose":
      // 非制品操作：以动作名展示，不编造仓库 / 坐标。
      return (
        <Badge size="sm" variant="filled" color="gray" style={{ flexShrink: 0 }}>
          {actionLabel(node.label, t)}
        </Badge>
      );
    default:
      return <Text size="sm">{node.label}</Text>;
  }
}
