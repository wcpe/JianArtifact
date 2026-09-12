// 仓库资产目录树：点目录展开并可作为资产目标选择；点文件回调选中。文件类型图标多态化。
// FR-54: 支持 onExpandDir 懒加载回调——目录首次展开时触发。
// FR-105: 管理员可选择文件和目录，选择状态由父组件维护。
import { Group, Loader, ScrollArea, Text, UnstyledButton } from "@mantine/core";
import {
  IconBrandJavascript,
  IconBrandNpm,
  IconChevronDown,
  IconChevronRight,
  IconFile,
  IconFileText,
  IconFileTypeXml,
  IconFileZip,
  IconFingerprint,
  IconFolder,
  IconFolderOpen,
  IconPackage,
} from "@tabler/icons-react";
import { useEffect, useState } from "react";
import type { MouseEvent as ReactMouseEvent } from "react";
import type { AssetTreeNode } from "../../lib/assetTree";
import { formatBytes } from "../../lib/assetTree";

interface Props {
  nodes: AssetTreeNode[];
  selectedPath: string | null;
  onSelectFile: (node: AssetTreeNode) => void;
  onSelectDir: (node: AssetTreeNode) => void;
  /** FR-54: 目录首次展开时触发懒加载；返回 Promise 表示加载中。 */
  onExpandDir?: (node: AssetTreeNode) => void;
  maxHeight?: number | string;
  /** 目录默认展开（搜索结果树等已全量加载的场景）。 */
  defaultExpanded?: boolean;
  /** 文件行右侧显示大小（搜索结果等需要直观信息的场景）。 */
  showSize?: boolean;
  /** FR-105: 管理员启用文件和目录选择。 */
  selectable?: boolean;
  /** FR-105: 当前选中的文件和目录路径集合。 */
  selectedPaths?: Set<string>;
  /** FR-105: 普通、Ctrl/Meta、Shift 点击选择回调。 */
  onNodeInteraction?: (node: AssetTreeNode, event: ReactMouseEvent<HTMLButtonElement>) => void;
  /** FR-105: 右键菜单回调。 */
  onContextMenu?: (node: AssetTreeNode, event: ReactMouseEvent<HTMLButtonElement>) => void;
  /** 兼容旧调用方的选择回调。 */
  onToggleSelect?: (node: AssetTreeNode) => void;
}

export function RepoAssetTree({
  nodes,
  selectedPath,
  onSelectFile,
  onSelectDir,
  onExpandDir,
  maxHeight = 480,
  defaultExpanded = false,
  showSize = false,
  selectable = false,
  selectedPaths,
  onNodeInteraction,
  onContextMenu,
  onToggleSelect,
}: Props) {
  const content = (
    <div role="tree">
      {nodes.map((n) => (
        <TreeNodeRow
          key={n.path}
          node={n}
          depth={0}
          selectedPath={selectedPath}
          onSelectFile={onSelectFile}
          onSelectDir={onSelectDir}
          onExpandDir={onExpandDir}
          defaultExpanded={defaultExpanded}
          showSize={showSize}
          selectable={selectable}
          selectedPaths={selectedPaths}
          onNodeInteraction={onNodeInteraction}
          onContextMenu={onContextMenu}
          onToggleSelect={onToggleSelect}
        />
      ))}
    </div>
  );

  // maxHeight="none" 时由父容器管理滚动，不再包裹 ScrollArea
  if (maxHeight === "none") {
    return content;
  }

  return (
    <ScrollArea.Autosize mah={maxHeight} type="auto" offsetScrollbars>
      {content}
    </ScrollArea.Autosize>
  );
}

function TreeNodeRow({
  node,
  depth,
  selectedPath,
  onSelectFile,
  onSelectDir,
  onExpandDir,
  defaultExpanded = false,
  showSize = false,
  selectable = false,
  selectedPaths,
  onNodeInteraction,
  onContextMenu,
  onToggleSelect,
}: {
  node: AssetTreeNode;
  depth: number;
  selectedPath: string | null;
  onSelectFile: (node: AssetTreeNode) => void;
  onSelectDir: (node: AssetTreeNode) => void;
  onExpandDir?: (node: AssetTreeNode) => void;
  defaultExpanded?: boolean;
  showSize?: boolean;
  selectable?: boolean;
  selectedPaths?: Set<string>;
  onNodeInteraction?: (node: AssetTreeNode, event: ReactMouseEvent<HTMLButtonElement>) => void;
  onContextMenu?: (node: AssetTreeNode, event: ReactMouseEvent<HTMLButtonElement>) => void;
  onToggleSelect?: (node: AssetTreeNode) => void;
}) {
  const [open, setOpen] = useState(defaultExpanded);
  const isDir = node.kind === "dir";
  const selected =
    selectedPath === node.path || (selectable && selectedPaths?.has(node.path) === true);
  const pad = 8 + depth * 14;
  // FR-54: 目录未加载子节点时 children === undefined
  const isLoading = isDir && open && node.children === undefined;

  useEffect(() => {
    if (isDir && open && node.children === undefined && onExpandDir) {
      onExpandDir(node);
    }
  }, [isDir, node, onExpandDir, open]);

  return (
    <>
      <UnstyledButton
        role="treeitem"
        data-path={node.path}
        data-kind={node.kind}
        aria-expanded={isDir ? open : undefined}
        onClick={(event) => {
          if (isDir) {
            const willOpen = !open;
            setOpen(willOpen);
          }
          if (onNodeInteraction) {
            onNodeInteraction(node, event);
          } else if (isDir) {
            onSelectDir(node);
          } else {
            onSelectFile(node);
          }
        }}
        onContextMenu={(event) => onContextMenu?.(node, event)}
        style={{
          display: "block",
          width: "100%",
          padding: `4px 8px 4px ${pad}px`,
          borderRadius: 4,
          background: selected ? "var(--mantine-color-blue-light)" : undefined,
        }}
      >
        <Group gap={6} wrap="nowrap">
          <span style={{ width: 14 }} />
          {isDir ? (
            open ? (
              <IconChevronDown size={14} />
            ) : (
              <IconChevronRight size={14} />
            )
          ) : (
            <span style={{ width: 14 }} />
          )}
          {isDir ? (
            open ? (
              <IconFolderOpen size={16} color="var(--mantine-color-yellow-7)" />
            ) : (
              <IconFolder size={16} color="var(--mantine-color-yellow-7)" />
            )
          ) : (
            <FileIcon name={node.name} />
          )}
          <Text size="sm" lineClamp={1} fw={selected ? 600 : 400} style={{ flex: 1, minWidth: 0 }}>
            {node.name}
          </Text>
          {showSize && !isDir && node.asset && (
            <Text size="xs" c="dimmed" style={{ flexShrink: 0 }}>
              {formatBytes(node.asset.size)}
            </Text>
          )}
        </Group>
      </UnstyledButton>
      {isDir && open && (
        <div role="group">
          {isLoading && (
            <div style={{ paddingLeft: pad + 20, paddingTop: 4 }}>
              <Loader size={14} />
            </div>
          )}
          {node.children &&
            node.children.map((c) => (
              <TreeNodeRow
                key={c.path}
                node={c}
                depth={depth + 1}
                selectedPath={selectedPath}
                onSelectFile={onSelectFile}
                onSelectDir={onSelectDir}
                onExpandDir={onExpandDir}
                defaultExpanded={defaultExpanded}
                showSize={showSize}
                selectable={selectable}
                selectedPaths={selectedPaths}
                onNodeInteraction={onNodeInteraction}
                onContextMenu={onContextMenu}
                onToggleSelect={onToggleSelect}
              />
            ))}
        </div>
      )}
    </>
  );
}

/** 根据文件名后缀返回对应图标。 */
function FileIcon({ name }: { name: string }) {
  const lower = name.toLowerCase();
  if (lower.endsWith(".jar") || lower.endsWith(".war") || lower.endsWith(".ear")) {
    return <IconPackage size={16} color="var(--mantine-color-orange-6)" />;
  }
  if (lower.endsWith(".pom") || lower.endsWith(".xml")) {
    return <IconFileTypeXml size={16} color="var(--mantine-color-red-5)" />;
  }
  if (lower.endsWith(".json") || lower.endsWith(".module")) {
    return <IconFileText size={16} color="var(--mantine-color-yellow-6)" />;
  }
  if (
    lower.endsWith(".md5") ||
    lower.endsWith(".sha1") ||
    lower.endsWith(".sha256") ||
    lower.endsWith(".sha512")
  ) {
    return <IconFingerprint size={16} color="var(--mantine-color-gray-5)" />;
  }
  if (lower.endsWith(".gradle") || lower.endsWith(".kts") || lower.endsWith(".gradle.kts")) {
    return <IconPackage size={16} color="var(--mantine-color-green-6)" />;
  }
  if (lower.endsWith(".js") || lower.endsWith(".mjs")) {
    return <IconBrandJavascript size={16} color="var(--mantine-color-yellow-6)" />;
  }
  if (lower.endsWith(".tgz") || lower.endsWith(".tar.gz") || lower.endsWith(".zip")) {
    return <IconFileZip size={16} color="var(--mantine-color-grape-5)" />;
  }
  if (lower.endsWith(".npm") || lower === "package.json") {
    return <IconBrandNpm size={16} color="var(--mantine-color-red-6)" />;
  }
  return <IconFile size={16} color="var(--mantine-color-gray-6)" />;
}
