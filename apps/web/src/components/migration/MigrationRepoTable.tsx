// 迁移计划仓库表：支持可选勾选与客户端分页/筛选，无 raw JSON。
import {
  Badge,
  Checkbox,
  Group,
  Pagination,
  ScrollArea,
  Table,
  Text,
  TextInput,
} from "@mantine/core";
import { useMemo, useState } from "react";
import { useTranslation } from "react-i18next";

import type { MigrationPlan } from "../../api/types";
import { formatColor } from "./status";

type Repo = MigrationPlan["repositories"][number];

interface Props {
  repositories: Repo[];
  /** 勾选模式：传入则显示 checkbox */
  selected?: string[];
  onToggle?: (name: string) => void;
  disabled?: boolean;
  maxHeight?: number | string;
  /** 每页条数，默认 20；0 表示不分页 */
  pageSize?: number;
}

export function MigrationRepoTable({
  repositories,
  selected,
  onToggle,
  disabled,
  maxHeight = 360,
  pageSize = 20,
}: Props) {
  const { t } = useTranslation();
  const selectable = Array.isArray(selected) && typeof onToggle === "function";
  const [page, setPage] = useState(1);
  const [filter, setFilter] = useState("");

  const filtered = useMemo(() => {
    const q = filter.trim().toLowerCase();
    if (!q) {
      return repositories;
    }
    return repositories.filter(
      (r) =>
        r.name.toLowerCase().includes(q) ||
        (r.format ?? "").toLowerCase().includes(q) ||
        (r.type ?? "").toLowerCase().includes(q),
    );
  }, [repositories, filter]);

  const totalPages = pageSize > 0 ? Math.max(1, Math.ceil(filtered.length / pageSize) || 1) : 1;
  const safePage = Math.min(page, totalPages);
  const pageItems =
    pageSize > 0 ? filtered.slice((safePage - 1) * pageSize, safePage * pageSize) : filtered;

  if (repositories.length === 0) {
    return (
      <Text size="sm" c="dimmed">
        {t("migrations.planEmpty")}
      </Text>
    );
  }

  return (
    <div>
      <Group justify="space-between" wrap="wrap" gap="xs" mb="xs">
        <TextInput
          size="xs"
          placeholder={t("migrations.repoFilterPlaceholder")}
          value={filter}
          onChange={(e) => {
            setFilter(e.currentTarget.value);
            setPage(1);
          }}
          w={220}
          disabled={disabled}
        />
        <Text size="xs" c="dimmed">
          {selectable
            ? `${t("migrations.selectRepos")}（${selected.length}/${repositories.length}）`
            : t("migrations.planRepoCount", { n: filtered.length })}
        </Text>
      </Group>
      <ScrollArea.Autosize mah={maxHeight} type="auto" offsetScrollbars>
        <Table striped highlightOnHover stickyHeader>
          <Table.Thead>
            <Table.Tr>
              {selectable && <Table.Th w={48} />}
              <Table.Th>{t("migrations.repoName")}</Table.Th>
              <Table.Th>{t("migrations.repoFormat")}</Table.Th>
              <Table.Th>{t("migrations.repoType")}</Table.Th>
              <Table.Th ta="right">{t("migrations.estimatedAssets")}</Table.Th>
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {pageItems.length === 0 ? (
              <Table.Tr>
                <Table.Td colSpan={selectable ? 5 : 4}>
                  <Text size="sm" c="dimmed" ta="center" py="sm">
                    {t("common.empty")}
                  </Text>
                </Table.Td>
              </Table.Tr>
            ) : (
              pageItems.map((r) => {
                const checked = selectable ? selected.includes(r.name) : false;
                return (
                  <Table.Tr
                    key={r.name}
                    bg={selectable && checked ? "var(--mantine-color-blue-light)" : undefined}
                    style={selectable ? { cursor: disabled ? "default" : "pointer" } : undefined}
                    onClick={() => {
                      if (selectable && !disabled) {
                        onToggle(r.name);
                      }
                    }}
                  >
                    {selectable && (
                      <Table.Td onClick={(e) => e.stopPropagation()}>
                        <Checkbox
                          checked={checked}
                          onChange={() => onToggle(r.name)}
                          aria-label={r.name}
                          disabled={disabled}
                        />
                      </Table.Td>
                    )}
                    <Table.Td>
                      <Text fw={500} size="sm">
                        {r.name}
                      </Text>
                    </Table.Td>
                    <Table.Td>
                      <Badge size="sm" variant="light" color={formatColor(r.format)}>
                        {r.format}
                      </Badge>
                    </Table.Td>
                    <Table.Td>
                      <Badge size="sm" variant="outline" color="gray">
                        {r.type ?? "hosted"}
                      </Badge>
                    </Table.Td>
                    <Table.Td ta="right">
                      <Text size="sm" ff="monospace">
                        {r.estimatedAssets != null && r.estimatedAssets > 0
                          ? r.estimatedAssets.toLocaleString()
                          : "—"}
                      </Text>
                    </Table.Td>
                  </Table.Tr>
                );
              })
            )}
          </Table.Tbody>
        </Table>
      </ScrollArea.Autosize>
      {pageSize > 0 && totalPages > 1 && (
        <Group justify="center" mt="sm">
          <Pagination
            size="sm"
            value={safePage}
            onChange={setPage}
            total={totalPages}
            siblings={1}
            boundaries={1}
          />
        </Group>
      )}
    </div>
  );
}
