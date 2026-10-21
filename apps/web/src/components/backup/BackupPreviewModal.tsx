// 备份包内容预览。
//
// # 为什么需要这个弹窗
//
// 恢复会用这个包**整份覆盖**本机数据，是不可撤销的操作。而此前用户能看到的只有
// "大小 / 多少制品 / 什么时候生成"——全是抽象计数，无法回答"这里面到底是不是
// 我要的那个包"。结果是把恢复变成一次赌博。
//
// 本弹窗把包里的内容摊开：仓库清单、账号清单（不含口令）、制品路径样例。
// 用户据此判断"这确实是我需要的那份"，再决定是否恢复。
//
// # 加载时机
//
// 打开即请求。预览是只读的、服务端解到临时目录查完即删，因此没有副作用，
// 也不需要二次确认。失败只影响"看不到内容"，不影响包本身可用。
import {
  Alert,
  Badge,
  Box,
  Code,
  Group,
  Loader,
  Modal,
  ScrollArea,
  Stack,
  Table,
  Text,
} from "@mantine/core";
import { IconInfoCircle } from "@tabler/icons-react";
import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";

import { previewBackup } from "../../api/endpoints";
import type { BackupPreview } from "../../api/types";

export function BackupPreviewModal({
  packageId,
  onClose,
}: {
  /** 要预览的包；null 表示关闭。 */
  packageId: string | null;
  onClose: () => void;
}) {
  const { t } = useTranslation();
  const [data, setData] = useState<BackupPreview | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);

  useEffect(() => {
    if (!packageId) {
      return;
    }
    let cancelled = false;
    setLoading(true);
    setError(null);
    setData(null);
    previewBackup(packageId)
      .then((res) => {
        if (!cancelled) setData(res);
      })
      .catch((err: unknown) => {
        if (!cancelled) setError(err instanceof Error ? err.message : String(err));
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [packageId]);

  const manifest = data?.manifest;

  return (
    <Modal
      opened={packageId !== null}
      onClose={onClose}
      title={t("backups.previewTitle")}
      size="lg"
      centered
    >
      <Stack gap="sm">
        <Group gap="xs" wrap="nowrap" align="center">
          <Text size="xs" c="dimmed">
            {t("backups.previewTarget")}
          </Text>
          <Code>{packageId ?? ""}</Code>
        </Group>

        {loading ? (
          <Group gap="xs">
            <Loader size="xs" />
            <Text size="sm" c="dimmed">
              {t("backups.previewLoading")}
            </Text>
          </Group>
        ) : null}

        {error ? (
          <Alert color="red" variant="light">
            {t("backups.previewFailed", { message: error })}
          </Alert>
        ) : null}

        {data ? (
          <>
            <Alert color="blue" variant="light" icon={<IconInfoCircle size={16} />}>
              <Text size="xs">{t("backups.previewHint")}</Text>
            </Alert>

            {/* 规模：让用户先知道"这个包多大、包含多少东西"。 */}
            <Group gap="md" wrap="wrap">
              <Box>
                <Text size="xs" c="dimmed">
                  {t("backups.previewAssets")}
                </Text>
                <Text size="sm" fw={600}>
                  {data.totalAssets}
                </Text>
              </Box>
              <Box>
                <Text size="xs" c="dimmed">
                  {t("backups.previewRepos")}
                </Text>
                <Text size="sm" fw={600}>
                  {data.repositories.length}
                </Text>
              </Box>
              <Box>
                <Text size="xs" c="dimmed">
                  {t("backups.previewUsers")}
                </Text>
                <Text size="sm" fw={600}>
                  {data.users.length}
                </Text>
              </Box>
              {manifest ? (
                <Box>
                  <Text size="xs" c="dimmed">
                    {t("backups.previewSchema")}
                  </Text>
                  <Text size="sm" fw={600}>
                    {manifest.dbSchemaVersion}
                  </Text>
                </Box>
              ) : null}
            </Group>

            {/* 仓库清单：这是"是不是我要的那个包"最直接的判据。 */}
            <Box>
              <Text size="sm" fw={600} mb={4}>
                {t("backups.previewRepoList")}
              </Text>
              {data.repositories.length === 0 ? (
                <Text size="xs" c="dimmed">
                  {t("backups.previewEmptyRepos")}
                </Text>
              ) : (
                <ScrollArea.Autosize mah={180}>
                  <Table striped verticalSpacing={4} data-testid="preview-repos">
                    <Table.Thead>
                      <Table.Tr>
                        <Table.Th>{t("backups.previewColName")}</Table.Th>
                        <Table.Th>{t("backups.previewColFormat")}</Table.Th>
                        <Table.Th>{t("backups.previewColType")}</Table.Th>
                      </Table.Tr>
                    </Table.Thead>
                    <Table.Tbody>
                      {data.repositories.map((r) => (
                        <Table.Tr key={r.name}>
                          <Table.Td>
                            <Text size="xs" ff="monospace">
                              {r.name}
                            </Text>
                          </Table.Td>
                          <Table.Td>
                            <Badge size="xs" variant="light">
                              {r.format}
                            </Badge>
                          </Table.Td>
                          <Table.Td>
                            <Text size="xs" c="dimmed">
                              {r.type}
                            </Text>
                          </Table.Td>
                        </Table.Tr>
                      ))}
                    </Table.Tbody>
                  </Table>
                </ScrollArea.Autosize>
              )}
            </Box>

            {/* 账号清单：只显示用户名/角色/状态，**不含**口令哈希。 */}
            <Box>
              <Text size="sm" fw={600} mb={4}>
                {t("backups.previewUserList")}
              </Text>
              <Text size="xs" c="dimmed" mb={4}>
                {t("backups.previewUserNoSecret")}
              </Text>
              {data.users.length === 0 ? (
                <Text size="xs" c="dimmed">
                  {t("backups.previewEmptyUsers")}
                </Text>
              ) : (
                <ScrollArea.Autosize mah={140}>
                  <Table striped verticalSpacing={4} data-testid="preview-users">
                    <Table.Tbody>
                      {data.users.map((u) => (
                        <Table.Tr key={u.username}>
                          <Table.Td>
                            <Text size="xs" ff="monospace">
                              {u.username}
                            </Text>
                          </Table.Td>
                          <Table.Td>
                            <Badge size="xs" variant="light">
                              {u.role}
                            </Badge>
                          </Table.Td>
                          <Table.Td>
                            <Text size="xs" c="dimmed">
                              {u.status}
                            </Text>
                          </Table.Td>
                        </Table.Tr>
                      ))}
                    </Table.Tbody>
                  </Table>
                </ScrollArea.Autosize>
              )}
            </Box>

            {/* 制品样例：给人"这批数据长什么样"的直观印象。 */}
            <Box>
              <Text size="sm" fw={600} mb={4}>
                {t("backups.previewSampleAssets")}
              </Text>
              {data.sampleAssets.length === 0 ? (
                <Text size="xs" c="dimmed">
                  {t("backups.previewEmptyAssets")}
                </Text>
              ) : (
                <ScrollArea.Autosize mah={200}>
                  <Stack gap={2} data-testid="preview-assets">
                    {data.sampleAssets.map((p) => (
                      <Text key={p} size="xs" ff="monospace">
                        {p}
                      </Text>
                    ))}
                  </Stack>
                </ScrollArea.Autosize>
              )}
              {data.totalAssets > data.sampleAssets.length ? (
                <Text size="xs" c="dimmed" mt={4}>
                  {t("backups.previewMoreAssets", {
                    n: data.totalAssets - data.sampleAssets.length,
                  })}
                </Text>
              ) : null}
            </Box>
          </>
        ) : null}
      </Stack>
    </Modal>
  );
}
