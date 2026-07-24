// 仓库资产目录树：点目录仅展开；点文件回调选中。
import { Group, ScrollArea, Text, UnstyledButton } from "@mantine/core";
import {
  IconChevronDown,
  IconChevronRight,
  IconFile,
  IconFolder,
  IconFolderOpen,
} from "@tabler/icons-react";
import { useState } from "react";
import type { AssetTreeNode } from "../../lib/assetTree";

interface Props {
  nodes: AssetTreeNode[];
  selectedPath: string | null;
  onSelectFile: (node: AssetTreeNode) => void;
  onSelectDir: (node: AssetTreeNode) => void;
  maxHeight?: number | string;
}

export function RepoAssetTree({
  nodes,
  selectedPath,
  onSelectFile,
  onSelectDir,
  maxHeight = 480,
}: Props) {
  return (
    <ScrollArea.Autosize mah={maxHeight} type="auto" offsetScrollbars>
      <div role="tree">
        {nodes.map((n) => (
          <TreeNodeRow
            key={n.path}
            node={n}
            depth={0}
            selectedPath={selectedPath}
            onSelectFile={onSelectFile}
            onSelectDir={onSelectDir}
          />
        ))}
      </div>
    </ScrollArea.Autosize>
  );
}

function TreeNodeRow({
  node,
  depth,
  selectedPath,
  onSelectFile,
  onSelectDir,
}: {
  node: AssetTreeNode;
  depth: number;
  selectedPath: string | null;
  onSelectFile: (node: AssetTreeNode) => void;
  onSelectDir: (node: AssetTreeNode) => void;
}) {
  const [open, setOpen] = useState(depth < 1);
  const isDir = node.kind === "dir";
  const selected = !isDir && selectedPath === node.path;
  const pad = 8 + depth * 14;

  return (
    <>
      <UnstyledButton
        role="treeitem"
        aria-expanded={isDir ? open : undefined}
        onClick={() => {
          if (isDir) {
            setOpen((v) => !v);
            onSelectDir(node);
          } else {
            onSelectFile(node);
          }
        }}
        style={{
          display: "block",
          width: "100%",
          padding: `4px 8px 4px ${pad}px`,
          borderRadius: 4,
          background: selected ? "var(--mantine-color-blue-light)" : undefined,
        }}
      >
        <Group gap={6} wrap="nowrap">
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
            <IconFile size={16} color="var(--mantine-color-gray-6)" />
          )}
          <Text size="sm" lineClamp={1} fw={selected ? 600 : 400}>
            {node.name}
          </Text>
        </Group>
      </UnstyledButton>
      {isDir && open && node.children && node.children.length > 0 && (
        <div role="group">
          {node.children.map((c) => (
            <TreeNodeRow
              key={c.path}
              node={c}
              depth={depth + 1}
              selectedPath={selectedPath}
              onSelectFile={onSelectFile}
              onSelectDir={onSelectDir}
            />
          ))}
        </div>
      )}
    </>
  );
}
