// 搜索结果左栏：包 / 版本 / 文件三级折叠树。
//
// 职责边界：只渲染 + 抛事件，展开与选中状态由 SearchPage 持有——这样窄屏切到详情时
// 树只是被隐藏（display:none），展开态与被隐藏前的滚动位置都原样保留。
import { Badge, Box, Group, Stack, Text, UnstyledButton } from "@mantine/core";
import { IconChevronDown, IconChevronRight, IconFile } from "@tabler/icons-react";
import { useMemo } from "react";
import { useTranslation } from "react-i18next";

import type { SearchPackage, SearchPackageFile } from "../../api/endpoints";
import {
  compileHighlightPattern,
  fileKey,
  fileKindChips,
  packageKey,
  precisionMeta,
  splitChecksumFiles,
  splitHighlightWithPattern,
  versionKey,
  type HighlightSegment,
  type SearchSelection,
} from "../../lib/searchPackage";

/**
 * 命中词高亮的统一渲染：命中片段包 `<mark>`（规格 §3.9 的配色与圆角）。
 *
 * 入参是**已编译好的正则**而不是关键词数组：高亮按行切分，一个展开的大包可能含数千个
 * 文件行，逐行现编译会让同样几个关键词被编译上千次。正则由树顶层的
 * `compileHighlightPattern` + `useMemo` 提供，全树共享一个实例。
 */
function Highlighted({ text, pattern }: { text: string; pattern: RegExp | null }) {
  const segments: HighlightSegment[] = splitHighlightWithPattern(text, pattern);
  return (
    <>
      {segments.map((segment, index) =>
        segment.hit ? (
          <mark
            // 片段数组来自文本切分，顺序稳定且无独立标识，用下标作键即可。
            key={index}
            style={{
              background: "var(--mantine-color-yellow-2)",
              color: "inherit",
              borderRadius: 2,
              // 规格要求「不加粗」：高亮只靠底色，避免整行字重在命中处突变。
              fontWeight: "inherit",
              padding: 0,
            }}
          >
            {segment.text}
          </mark>
        ) : (
          <span key={index}>{segment.text}</span>
        ),
      )}
    </>
  );
}

/** 协议徽标文案：无坐标伪包的「徽标显示仓库名」（规格 §3.2 / 任务书）。 */
function kindBadge(pkg: SearchPackage): string {
  return pkg.kind === "file" ? (pkg.repositories[0] ?? pkg.coordinate) : pkg.kind;
}

interface PackageSearchTreeProps {
  packages: readonly SearchPackage[];
  /** 高亮关键词（来自当前搜索表达式，仅用于坐标 / 版本号 / 文件名）。 */
  keywords: readonly string[];
  expandedPackages: ReadonlySet<string>;
  expandedVersions: ReadonlySet<string>;
  expandedChecksums: ReadonlySet<string>;
  selection: SearchSelection | null;
  onTogglePackage: (pkg: SearchPackage) => void;
  onToggleVersion: (pkg: SearchPackage, version: string) => void;
  onToggleChecksums: (pkg: SearchPackage, version: string) => void;
  onSelectVersion: (pkg: SearchPackage, version: string) => void;
  onSelectFile: (pkg: SearchPackage, version: string | null, file: SearchPackageFile) => void;
}

/**
 * 文件类型 chip 组（前 4 个 + `+N`）。
 * 单独成组件是因为版本行与伪包行都要用，而 chip 计算依赖版本内文件集合。
 */
function FileKindChips({ files }: { files: readonly SearchPackageFile[] }) {
  const { kinds, overflow } = fileKindChips(files);
  if (kinds.length === 0 && overflow === 0) {
    return null;
  }
  return (
    <Group gap={4} wrap="wrap">
      {kinds.map((kind) => (
        <Badge key={kind} variant="light" color="gray" size="xs" radius="sm">
          {kind}
        </Badge>
      ))}
      {overflow > 0 && (
        <Text size="xs" c="dimmed">
          +{overflow}
        </Text>
      )}
    </Group>
  );
}

/** 文件行：`fileKind` + 文件名。 */
function FileRow({
  file,
  pattern,
  depth,
  selected,
  onSelect,
}: {
  file: SearchPackageFile;
  pattern: RegExp | null;
  depth: number;
  selected: boolean;
  onSelect: () => void;
}) {
  return (
    <UnstyledButton
      onClick={onSelect}
      aria-current={selected ? "true" : undefined}
      data-testid="search-file-row"
      // 稳定定位用（高亮会把文件名切成多个元素，测试与端到端脚本不能依赖纯文本查询）。
      data-path={file.path}
      style={{
        display: "block",
        width: "100%",
        padding: `3px 8px 3px ${8 + depth * 14}px`,
        borderRadius: 4,
        background: selected ? "var(--mantine-color-blue-light)" : undefined,
      }}
    >
      <Group gap={6} wrap="nowrap">
        <IconFile size={13} color="var(--mantine-color-gray-6)" style={{ flexShrink: 0 }} />
        <Text size="xs" c="dimmed" style={{ flexShrink: 0 }}>
          {file.fileKind}
        </Text>
        <Text
          size="xs"
          fw={selected ? 600 : 400}
          lineClamp={1}
          style={{ flex: 1, minWidth: 0, wordBreak: "break-all" }}
        >
          <Highlighted text={file.name} pattern={pattern} />
        </Text>
      </Group>
    </UnstyledButton>
  );
}

/** 版本行：版本号 + 精度徽标 + `N 文件` + 文件类型 chip 组。 */
function VersionRow({
  version,
  pattern,
  open,
  selectedVersion,
  selectedFileKey,
  onToggle,
  onSelect,
  onSelectFile,
  showChecksums,
  onToggleChecksums,
}: {
  version: SearchPackage["versions"][number];
  pattern: RegExp | null;
  open: boolean;
  selectedVersion: boolean;
  selectedFileKey: string | null;
  onToggle: () => void;
  onSelect: () => void;
  onSelectFile: (file: SearchPackageFile) => void;
  showChecksums: boolean;
  onToggleChecksums: () => void;
}) {
  const { t } = useTranslation();
  const { primary, checksums } = splitChecksumFiles(version.files);
  const meta = precisionMeta(version.precision);
  const toggleLabel = open ? t("common.collapse") : t("common.expand");

  return (
    <>
      <Group gap={0} wrap="nowrap" align="flex-start">
        <UnstyledButton
          onClick={onToggle}
          aria-label={toggleLabel}
          aria-expanded={open}
          style={{ padding: "4px 2px 4px 8px", borderRadius: 4 }}
        >
          {open ? <IconChevronDown size={12} /> : <IconChevronRight size={12} />}
        </UnstyledButton>
        <UnstyledButton
          onClick={onSelect}
          data-testid="search-version-row"
          data-version={version.version}
          aria-current={selectedVersion ? "true" : undefined}
          style={{
            flex: 1,
            minWidth: 0,
            padding: "3px 8px 3px 0",
            borderRadius: 4,
            background: selectedVersion ? "var(--mantine-color-blue-light)" : undefined,
          }}
        >
          <Group gap={6} wrap="wrap">
            <Text size="xs" fw={selectedVersion ? 600 : 500}>
              <Highlighted text={version.version} pattern={pattern} />
            </Text>
            {meta.labelKey && (
              <Badge variant="light" color={meta.color} size="xs">
                {t(meta.labelKey)}
              </Badge>
            )}
            <Text size="xs" c="dimmed">
              {t("search.filesCount", { count: version.fileCount })}
            </Text>
            <FileKindChips files={version.files} />
          </Group>
        </UnstyledButton>
      </Group>
      {open && (
        <Stack gap={0} pl={14}>
          {primary.map((file) => (
            <FileRow
              key={fileKey(file)}
              file={file}
              pattern={pattern}
              depth={1}
              selected={fileKey(file) === selectedFileKey}
              onSelect={() => onSelectFile(file)}
            />
          ))}
          {checksums.length > 0 && (
            // 校验和默认折成一行（与既有「隐藏校验和」心智一致，规格 §3.9）。
            <UnstyledButton
              onClick={onToggleChecksums}
              aria-expanded={showChecksums}
              style={{ display: "block", width: "100%", padding: `3px 8px 3px ${8 + 14}px` }}
            >
              <Group gap={6} wrap="nowrap">
                {showChecksums ? <IconChevronDown size={12} /> : <IconChevronRight size={12} />}
                <Text size="xs" c="dimmed">
                  {t("search.checksums", { count: checksums.length })}
                </Text>
              </Group>
            </UnstyledButton>
          )}
          {showChecksums &&
            checksums.map((file) => (
              <FileRow
                key={fileKey(file)}
                file={file}
                pattern={pattern}
                depth={2}
                selected={fileKey(file) === selectedFileKey}
                onSelect={() => onSelectFile(file)}
              />
            ))}
          {primary.length === 0 && checksums.length === 0 && (
            <Text size="xs" c="dimmed" pl={8}>
              {t("common.empty")}
            </Text>
          )}
        </Stack>
      )}
    </>
  );
}

/** 包搜索树：包行 → 版本行 → 文件行。 */
export function PackageSearchTree({
  packages,
  keywords,
  expandedPackages,
  expandedVersions,
  expandedChecksums,
  selection,
  onTogglePackage,
  onToggleVersion,
  onToggleChecksums,
  onSelectVersion,
  onSelectFile,
}: PackageSearchTreeProps) {
  const { t } = useTranslation();
  const selectedFileKey = useMemo(
    () => (selection ? fileKey({ repository: selection.repository, path: selection.path }) : null),
    [selection],
  );
  // 高亮正则按「关键词集合」编译一次，供全树所有行共享：逐行现编译会让同样几个关键词
  // 在展开千行级大包时被重复编译上千次。关键词不变（同一表达式）时实例保持不变。
  const pattern = useMemo(() => compileHighlightPattern(keywords), [keywords]);

  return (
    <Stack gap={2} data-testid="search-package-tree">
      {packages.map((pkg) => {
        const pkgKey = packageKey(pkg);
        const open = expandedPackages.has(pkgKey);
        const pseudo = pkg.kind === "file";
        const pkgFiles = pkg.files ?? [];
        return (
          <Box key={pkgKey}>
            <Group gap={0} wrap="nowrap" align="flex-start">
              <UnstyledButton
                onClick={() => onTogglePackage(pkg)}
                aria-label={open ? t("common.collapse") : t("common.expand")}
                aria-expanded={open}
                style={{ padding: "4px 2px 4px 8px", borderRadius: 4 }}
              >
                {open ? <IconChevronDown size={14} /> : <IconChevronRight size={14} />}
              </UnstyledButton>
              <UnstyledButton
                onClick={() => onTogglePackage(pkg)}
                data-testid="search-package-row"
                data-coordinate={pkg.coordinate}
                style={{ flex: 1, minWidth: 0, padding: "3px 8px 3px 0", borderRadius: 4 }}
              >
                <Group gap={6} wrap="nowrap">
                  <Text size="sm" fw={open ? 600 : 500} lineClamp={1} style={{ minWidth: 0 }}>
                    {pseudo ? (
                      t("search.noCoordinate")
                    ) : (
                      <Highlighted text={pkg.coordinate} pattern={pattern} />
                    )}
                  </Text>
                  <Badge variant="light" size="xs" style={{ flexShrink: 0 }}>
                    {kindBadge(pkg)}
                  </Badge>
                </Group>
                <Text size="xs" c="dimmed" pl={14}>
                  {pseudo
                    ? t("search.filesCount", { count: pkg.fileCount })
                    : t("search.packageSummary", {
                        versions: pkg.versionCount,
                        files: pkg.fileCount,
                      })}
                </Text>
              </UnstyledButton>
            </Group>
            {open && (
              <Stack gap={0} pl={14}>
                {pseudo
                  ? // 无坐标伪包没有版本层：文件直接挂在包下（规格 §3.2）。
                    pkgFiles.map((file) => (
                      <FileRow
                        key={fileKey(file)}
                        file={file}
                        pattern={pattern}
                        depth={1}
                        selected={fileKey(file) === selectedFileKey}
                        onSelect={() => onSelectFile(pkg, null, file)}
                      />
                    ))
                  : pkg.versions.map((version) => {
                      const vKey = versionKey(pkgKey, version.version);
                      return (
                        <VersionRow
                          key={vKey}
                          version={version}
                          pattern={pattern}
                          open={expandedVersions.has(vKey)}
                          selectedVersion={
                            selection?.packageKey === pkgKey &&
                            selection.version === version.version
                          }
                          selectedFileKey={selectedFileKey}
                          onToggle={() => onToggleVersion(pkg, version.version)}
                          onSelect={() => onSelectVersion(pkg, version.version)}
                          onSelectFile={(file) => onSelectFile(pkg, version.version, file)}
                          showChecksums={expandedChecksums.has(vKey)}
                          onToggleChecksums={() => onToggleChecksums(pkg, version.version)}
                        />
                      );
                    })}
              </Stack>
            )}
          </Box>
        );
      })}
    </Stack>
  );
}
