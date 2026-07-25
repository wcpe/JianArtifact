// 仓库资产目录树：点目录仅展开；点文件回调选中。文件类型图标多态化。
import { Group, ScrollArea, Text, UnstyledButton } from "@mantine/core";
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
}: {
  node: AssetTreeNode;
  depth: number;
  selectedPath: string | null;
  onSelectFile: (node: AssetTreeNode) => void;
  onSelectDir: (node: AssetTreeNode) => void;
}) {
  const [open, setOpen] = useState(false);
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
            <FileIcon name={node.name} />
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
  if (
    lower.endsWith(".gradle") ||
    lower.endsWith(".kts") ||
    lower.endsWith(".gradle.kts")
  ) {
    return <IconPackage size={16} color="var(--mantine-color-green-6)" />;
  }
  if (lower.endsWith(".js") || lower.endsWith(".mjs")) {
    return <IconBrandJavascript size={16} color="var(--mantine-color-yellow-6)" />;
  }
  if (
    lower.endsWith(".tgz") ||
    lower.endsWith(".tar.gz") ||
    lower.endsWith(".zip")
  ) {
    return <IconFileZip size={16} color="var(--mantine-color-grape-5)" />;
  }
  if (lower.endsWith(".npm") || lower === "package.json") {
    return <IconBrandNpm size={16} color="var(--mantine-color-red-6)" />;
  }
  return <IconFile size={16} color="var(--mantine-color-gray-6)" />;
}
