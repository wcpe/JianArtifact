// 可回收占比面板：把体检结论从「各类别有多少」推进到「这些垃圾占总量多少」。
//
// 为什么需要它：六张 KPI 卡只回答了各类别的绝对值，没回答「值不值得清」。
// 用户看到「中断上传 9.6 MB」无法判断这是该立刻处理的隐患还是可以忽略的噪声，
// 得自己拿 totalBytes 做除法。这一块把除法做完，并把构成摊开——占比高的那一类
// 才是清理收益所在，占比极低的类别即使数字看着大也不必优先处理。
//
// 口径必须诚实（宁可少报，不可误导）：
//   - 只累加**有字节数**的类别。空分片目录本身是空目录、占 0 字节，不参与占比，
//     在脚注里单独说明——否则「21 个空目录」会被误读成一笔可观的空间；
//   - 分母为 0 时不渲染任何百分比。0/0 说成 0% 或 100% 都是错的，此时只说明目录为空；
//   - 每一项都标注它由谁回收（脚注统一交代），避免用户以为这里点一下就能全清掉：
//     孤儿 blob 只由 blob-gc 作业回收，其余三类才对应本页的清理按钮。
import { Box, Card, Group, Progress, Stack, Text } from "@mantine/core";
import { useTranslation } from "react-i18next";

import type { StorageHealth } from "../../api/endpoints";
import { formatBytes } from "../../lib/format";
import { density } from "../../theme/density";

/**
 * 百分比文案。
 *
 * 小到看不见的比例必须写成「<0.1%」而不是四舍五入成「0.0%」：后者会让用户以为
 * 该类别真的一个字节都没有，而实际上它非零。同理大比例不再保留小数位。
 */
function formatPercent(value: number): string {
  if (!Number.isFinite(value) || value <= 0) return "0%";
  if (value < 0.1) return "<0.1%";
  if (value >= 10) return `${value.toFixed(0)}%`;
  return `${value.toFixed(1)}%`;
}

export function ReclaimablePanel({ health }: { health: StorageHealth }) {
  const { t } = useTranslation();

  // 四类有字节数的可回收项。`count` 为 null 表示接口没有给该项的条数
  // （中断上传只有占用大小），此时不渲染条数，不拿 0 冒充。
  const rows = [
    {
      key: "orphan",
      label: t("maintenance.orphanBlobs"),
      count: health.orphanBlobs as number | null,
      bytes: health.orphanBlobBytes,
      color: "red",
    },
    {
      key: "quarantine",
      label: t("maintenance.quarantined"),
      count: health.quarantinedBlobs as number | null,
      bytes: health.quarantinedBytes,
      color: "orange",
    },
    {
      key: "preRestore",
      label: t("maintenance.preRestore"),
      count: health.preRestoreDirs as number | null,
      bytes: health.preRestoreBytes,
      color: "grape",
    },
    {
      key: "staleUploads",
      label: t("maintenance.staleUploads"),
      count: null,
      bytes: health.backupUploadsBytes,
      color: "cyan",
    },
  ];

  // 负值只可能来自异常的接口数据，按 0 处理而不是让占比变成负数。
  const safe = (n: number) => (Number.isFinite(n) && n > 0 ? n : 0);
  const reclaimable = rows.reduce((sum, row) => sum + safe(row.bytes), 0);
  const total = safe(health.totalBytes);
  const ratio = total > 0 ? (reclaimable / total) * 100 : 0;
  const share = (bytes: number) => (total > 0 ? (safe(bytes) / total) * 100 : 0);

  return (
    <Card
      withBorder
      radius="md"
      padding={density.cardPadding}
      data-testid="reclaim-panel"
      // 结论区不参与拉伸：整卡按内容自然高度排布，不为了填满视口而留出大块空白。
      style={{ flexShrink: 0 }}
    >
      <Stack gap="sm">
        <Group justify="space-between" align="baseline" wrap="wrap" gap="xs">
          <Text fw={600} size="sm">
            {t("maintenance.reclaimTitle")}
          </Text>
          <Text size="xs" c="dimmed">
            {t("maintenance.reclaimFormula")}
          </Text>
        </Group>

        {total === 0 ? (
          <Text size="sm" c="dimmed" data-testid="reclaim-empty">
            {t("maintenance.reclaimEmpty")}
          </Text>
        ) : (
          <Group align="flex-start" gap="xl" wrap="wrap">
            {/* 左：一句结论。占比是这一屏的主数字，字号明显大于右侧明细。 */}
            <Box style={{ flex: "0 1 240px", minWidth: 180 }}>
              <Text
                size="1.9rem"
                fw={700}
                lh={1.1}
                data-testid="reclaim-percent"
                style={{ fontVariantNumeric: "tabular-nums" }}
              >
                {formatPercent(ratio)}
              </Text>
              <Text size="xs" c="dimmed" mt={4}>
                {t("maintenance.reclaimHeadline", {
                  bytes: formatBytes(reclaimable),
                  total: formatBytes(total),
                })}
              </Text>
              <Progress
                value={ratio}
                size="md"
                radius="xl"
                color="teal"
                mt="sm"
                aria-label={t("maintenance.reclaimTitle")}
              />
            </Box>

            {/* 右：构成明细。每一行都带自己的占比条，用来横向比较哪一类值得先清。 */}
            <Stack gap={8} style={{ flex: "1 1 320px", minWidth: 0 }}>
              {rows.map((row) => (
                <Box key={row.key} data-reclaim-category={row.key}>
                  <Group justify="space-between" wrap="nowrap" gap="sm">
                    <Group gap={6} wrap="nowrap" style={{ minWidth: 0 }}>
                      <Box
                        w={8}
                        h={8}
                        style={{
                          borderRadius: 2,
                          flexShrink: 0,
                          background: `var(--mantine-color-${row.color}-5)`,
                        }}
                      />
                      <Text size="xs" truncate>
                        {row.label}
                      </Text>
                      {row.count !== null ? (
                        <Text size="xs" c="dimmed" style={{ flexShrink: 0 }}>
                          {t("maintenance.reclaimCount", { count: row.count })}
                        </Text>
                      ) : null}
                    </Group>
                    <Group gap="xs" wrap="nowrap" style={{ flexShrink: 0 }}>
                      <Text size="xs" c="dimmed" style={{ fontVariantNumeric: "tabular-nums" }}>
                        {formatBytes(row.bytes)}
                      </Text>
                      <Text
                        size="xs"
                        fw={600}
                        w={44}
                        ta="right"
                        style={{ fontVariantNumeric: "tabular-nums" }}
                      >
                        {formatPercent(share(row.bytes))}
                      </Text>
                    </Group>
                  </Group>
                  <Progress value={share(row.bytes)} size={4} color={row.color} mt={4} />
                </Box>
              ))}
            </Stack>
          </Group>
        )}

        {/* 脚注交代两件事：空目录为何不计入占比、各类别分别由谁回收。 */}
        <Text size="xs" c="dimmed">
          {t("maintenance.reclaimNote")}
        </Text>
      </Stack>
    </Card>
  );
}
