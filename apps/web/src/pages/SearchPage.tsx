// 全局搜索结果页：/search?q=keyword
import { Group, Pagination, Stack, Table, Text, TextInput } from "@mantine/core";
import { IconSearch } from "@tabler/icons-react";
import { PageHeader } from "@jianartifact/ui";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { useNavigate, useSearchParams } from "react-router-dom";

import { searchAssets } from "../api/endpoints";
import { useAsync } from "../hooks/useAsync";
import { formatBytes } from "../lib/assetTree";

const PAGE_SIZE = 20;

export function SearchPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const [searchParams] = useSearchParams();
  const q = searchParams.get("q") ?? "";
  const [page, setPage] = useState(1);
  const [input, setInput] = useState(q);

  const state = useAsync(
    () =>
      q
        ? searchAssets({ q, page, page_size: PAGE_SIZE })
        : Promise.resolve({ items: [], total: 0 }),
    [q, page],
  );

  const totalPages = Math.ceil((state.data?.total ?? 0) / PAGE_SIZE);

  const handleSubmit = () => {
    if (input.trim()) {
      navigate(`/search?q=${encodeURIComponent(input.trim())}`);
      setPage(1);
    }
  };

  return (
    <>
      <PageHeader
        title={t("search.title", { defaultValue: "制品搜索" })}
        description={t("search.description", { defaultValue: "跨仓库搜索制品路径" })}
      />

      <Stack gap="md">
        <TextInput
          placeholder={t("search.placeholder", { defaultValue: "搜索制品..." })}
          leftSection={<IconSearch size={16} />}
          value={input}
          onChange={(e) => setInput(e.currentTarget.value)}
          onKeyDown={(e) => e.key === "Enter" && handleSubmit()}
          style={{ maxWidth: 480 }}
        />

        {q && state.data && state.data.items.length > 0 && (
          <>
            <Text size="sm" c="dimmed">
              {t("search.resultCount", {
                count: state.data.total,
                defaultValue: `共 ${state.data.total} 条结果`,
              })}
            </Text>
            <Table striped highlightOnHover>
              <Table.Thead>
                <Table.Tr>
                  <Table.Th>{t("search.repository", { defaultValue: "仓库" })}</Table.Th>
                  <Table.Th>{t("search.path", { defaultValue: "路径" })}</Table.Th>
                  <Table.Th>{t("search.size", { defaultValue: "大小" })}</Table.Th>
                </Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {state.data.items.map((item, idx) => (
                  <Table.Tr
                    key={`${item.repository}-${item.path}-${idx}`}
                    style={{ cursor: "pointer" }}
                    onClick={() => navigate(`/repositories/${encodeURIComponent(item.repository)}`)}
                  >
                    <Table.Td>
                      <Text size="sm" fw={600}>
                        {item.repository}
                      </Text>
                    </Table.Td>
                    <Table.Td>
                      <Text size="sm" lineClamp={1}>
                        {item.path}
                      </Text>
                    </Table.Td>
                    <Table.Td>
                      <Text size="sm">{formatBytes(item.size)}</Text>
                    </Table.Td>
                  </Table.Tr>
                ))}
              </Table.Tbody>
            </Table>
            {totalPages > 1 && (
              <Group justify="center">
                <Pagination value={page} onChange={setPage} total={totalPages} />
              </Group>
            )}
          </>
        )}

        {q && state.data && state.data.items.length === 0 && !state.loading && (
          <Text c="dimmed">{t("search.noResults", { defaultValue: "未找到匹配的制品" })}</Text>
        )}
      </Stack>
    </>
  );
}
