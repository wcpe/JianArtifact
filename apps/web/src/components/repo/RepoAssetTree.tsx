// 仓库资产目录树：点目录仅展开；点文件回调选中。文件类型图标多态化。
// FR-54: 支持 onExpandDir 懒加载回调——目录首次展开时触发。
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
import { useState } from "react";
import type { AssetTreeNode } from "../../lib/assetTree";

interface Props {
  nodes: AssetTreeNode[];
  selectedPath: string | null;
  onSelectFile: (node: AssetTreeNode) => void;
  onSelectDir: (node: AssetTreeNode) => void;
  /** FR-54: 目录首次展开时触发懒加载；返回 Promise 表示加载中。 */
  onExpandDir?: (node: AssetTreeNode) => void;
  maxHeight?: number | string;
}

export function RepoAssetTree({
  nodes,
  selectedPath,
  onSelectFile,
  onSelectDir,
  onExpandDir,
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
          onExpandDir={onExpandDir}
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
}: {
  node: AssetTreeNode;
  depth: number;
  selectedPath: string | null;
  onSelectFile: (node: AssetTreeNode) => void;
  onSelectDir: (node: AssetTreeNode) => void;
  onExpandDir?: (node: AssetTreeNode) => void;
}) {
  const [open, setOpen] = useState(false);
  const isDir = node.kind === "dir";
  const selected = !isDir && selectedPath === node.path;
  const pad = 8 + depth * 14;
  // FR-54: 目录未加载子节点时 children === undefined
  const isLoading = isDir && open && node.children === undefined;

  return (
    <>
      <UnstyledButton
        role="treeitem"
        aria-expanded={isDir ? open : undefined}
        onClick={() => {
          if (isDir) {
            const willOpen = !open;
            setOpen(willOpen);
            onSelectDir(node);
            // FR-54: 首次展开且无 children 时触发懒加载
            if (willOpen && node.children === undefined && onExpandDir) {
              onExpandDir(node);
            }
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
