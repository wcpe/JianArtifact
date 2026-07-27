// FR-72: 开源协议页——展示项目 Go / npm 依赖的协议清单。
// 数据由 scripts/generate-licenses.mjs 构建时产出并内嵌到后端二进制，
// 经 admin 专属端点 GET /api/v1/licenses 运行时拉取（不打进前端 bundle，
// 避免依赖名与精确版本清单随静态资源公开暴露）。
import {
  Anchor,
  Badge,
  Card,
  Stack,
  Table,
  Text,
  TextInput,
  Title,
} from "@mantine/core";
import { EmptyState, PageHeader } from "@jianartifact/ui";
import { IconSearch } from "@tabler/icons-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";

import { AsyncBoundary } from "../components/AsyncBoundary";
import { getLicenses } from "../api/endpoints";
import type { LicenseEntry } from "../api/endpoints";
import { useAsync } from "../hooks/useAsync";
import { density } from "../theme/density";

/** 协议徽章配色：常见宽松协议蓝色系，Unknown 灰色。 */
function licenseColor(license: string): string {
  if (license === "Unknown") return "gray";
  if (/GPL/i.test(license)) return "orange";
  return "blue";
}

function matchRow(row: LicenseEntry, q: string): boolean {
  return (
    row.name.toLowerCase().includes(q) ||
    row.license.toLowerCase().includes(q) ||
    row.author.toLowerCase().includes(q)
  );
}

function DependencyTable({
  title,
  rows,
  linkBase,
}: {
  title: string;
  rows: LicenseEntry[];
  linkBase: (name: string) => string;
}) {
  const { t } = useTranslation();
  return (
    <Card withBorder radius="md" padding={density.cardPadding}>
      <Stack gap="sm">
        <Title order={5}>{title}</Title>
        {rows.length === 0 ? (
          <EmptyState message={t("licenses.empty")} />
        ) : (
          <Table striped highlightOnHover withTableBorder={false} verticalSpacing={6}>
            <Table.Thead>
              <Table.Tr>
                <Table.Th>{t("licenses.colPackage")}</Table.Th>
                <Table.Th>{t("licenses.colVersion")}</Table.Th>
                <Table.Th>{t("licenses.colLicense")}</Table.Th>
                <Table.Th>{t("licenses.colAuthor")}</Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {rows.map((row) => (
                <Table.Tr key={row.name + row.version}>
                  <Table.Td>
                    <Anchor
                      href={linkBase(row.name)}
                      target="_blank"
                      rel="noopener noreferrer"
                      size="sm"
                    >
                      {row.name}
                    </Anchor>
                  </Table.Td>
                  <Table.Td>
                    <Text size="sm" c="dimmed">
                      {row.version}
                    </Text>
                  </Table.Td>
                  <Table.Td>
                    <Badge variant="light" color={licenseColor(row.license)} size="sm">
                      {row.license}
                    </Badge>
                  </Table.Td>
                  <Table.Td>
                    <Text size="sm">{row.author}</Text>
                  </Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        )}
      </Stack>
    </Card>
  );
}

export function LicensesPage() {
  const { t } = useTranslation();
  const [query, setQuery] = useState("");
  const state = useAsync(getLicenses, []);

  const q = query.trim().toLowerCase();

  return (
    <>
      <PageHeader title={t("licenses.title")} description={t("licenses.description")} />
      <AsyncBoundary state={state}>
        {(licenses) => {
          const goRows = q ? licenses.go.filter((r) => matchRow(r, q)) : licenses.go;
          const npmRows = q ? licenses.npm.filter((r) => matchRow(r, q)) : licenses.npm;
          return (
            <Stack gap="md">
              <TextInput
                placeholder={t("licenses.searchPlaceholder")}
                leftSection={<IconSearch size={16} />}
                value={query}
                onChange={(e) => setQuery(e.currentTarget.value)}
                maw={420}
              />
              <DependencyTable
                title={t("licenses.goSection", { count: goRows.length })}
                rows={goRows}
                linkBase={(name) => `https://pkg.go.dev/${name}`}
              />
              <DependencyTable
                title={t("licenses.npmSection", { count: npmRows.length })}
                rows={npmRows}
                linkBase={(name) => `https://www.npmjs.com/package/${name}`}
              />
            </Stack>
          );
        }}
      </AsyncBoundary>
    </>
  );
}
