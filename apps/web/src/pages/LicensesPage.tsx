// FR-72: 开源协议页——展示项目 Go / npm 依赖的协议清单（构建时生成的静态数据）。
// 数据由 scripts/generate-licenses.mjs 产出；匿名与登录用户均可访问，
// 但版本列仅登录用户可见（匿名脱敏，降低依赖版本信息的公开暴露面）。
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
import { useMemo, useState } from "react";
import { useTranslation } from "react-i18next";

import licenses from "../generated/licenses.json";
import { useAuth } from "../auth/AuthContext";
import { density } from "../theme/density";

interface DependencyRow {
  name: string;
  version: string;
  license: string;
  author: string;
}

/** 协议徽章配色：常见宽松协议蓝色系，Unknown 灰色。 */
function licenseColor(license: string): string {
  if (license === "Unknown") return "gray";
  if (/GPL/i.test(license)) return "orange";
  return "blue";
}

function matchRow(row: DependencyRow, q: string): boolean {
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
  showVersion,
}: {
  title: string;
  rows: DependencyRow[];
  linkBase: (name: string) => string;
  /** 是否展示版本列：匿名隐藏，降低已知漏洞被针对性利用的侦察价值。 */
  showVersion: boolean;
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
                {showVersion ? <Table.Th>{t("licenses.colVersion")}</Table.Th> : null}
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
                  {showVersion ? (
                    <Table.Td>
                      <Text size="sm" c="dimmed">
                        {row.version}
                      </Text>
                    </Table.Td>
                  ) : null}
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
  const { isAuthenticated } = useAuth();
  const [query, setQuery] = useState("");

  const q = query.trim().toLowerCase();
  const goRows = useMemo(
    () => (q ? licenses.go.filter((r) => matchRow(r, q)) : licenses.go),
    [q],
  );
  const npmRows = useMemo(
    () => (q ? licenses.npm.filter((r) => matchRow(r, q)) : licenses.npm),
    [q],
  );

  return (
    <>
      <PageHeader title={t("licenses.title")} description={t("licenses.description")} />
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
          showVersion={isAuthenticated}
        />
        <DependencyTable
          title={t("licenses.npmSection", { count: npmRows.length })}
          rows={npmRows}
          linkBase={(name) => `https://www.npmjs.com/package/${name}`}
          showVersion={isAuthenticated}
        />
      </Stack>
    </>
  );
}
