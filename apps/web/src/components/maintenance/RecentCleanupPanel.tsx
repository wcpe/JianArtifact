// 最近清理活动：体检页下半屏的一句话结论区。
//
// 为什么放在体检页而不是新开一个 Tab：
//   - 体检页的六张 KPI + 占比面板只回答「现在有多少垃圾」，不回答「上次清是什么时候、
//     清掉了没有」。而"该不该现在清"恰好取决于近期是否已经清过、上次是否失败——
//     这两件事必须挨着看才有意义；
//   - 体检页在固定视口下只用到约 60% 高度，下半屏是空的（实测主内容到 y=540、
//     视口 900）。把这段空白用来回答一个真问题，比拉开行距填满更有价值。
//
// 只取最近几条、只做只读展示：完整历史仍在「清理任务」Tab，这里不复制一份表格。
import { Badge, Box, Button, Card, Group, Skeleton, Stack, Text } from "@mantine/core";
import { IconArrowRight } from "@tabler/icons-react";
import { useCallback, useEffect, useState } from "react";
import { useTranslation } from "react-i18next";

import { listCleanTasks, type CleanTask } from "../../api/endpoints";
import { formatBytes } from "../../lib/format";
import { density } from "../../theme/density";

/** 展示条数。够看清"最近几轮是否都在成功"，又不至于把卡片撑高。 */
const RECENT_LIMIT = 4;

/** 状态 → 徽章色调。与「清理任务」Tab 的表内判定保持同一套语义。 */
function statusColor(status: string): string {
  switch (status) {
    case "succeeded":
      return "green";
    case "failed":
      return "red";
    case "interrupted":
      return "orange";
    default:
      return "blue";
  }
}

/**
 * 清理类别 → i18n 键。
 *
 * **不用 `t(\`kind_${kind}\`)` 动态拼键**：键名约定是 camelCase（`kindEmptyDirs`），
 * 而后端 kind 是 snake_case（`empty_dirs`），拼接出来的键两个语言里都不存在，
 * 界面会直接显示键名。显式映射在新增类别时会在 TypeScript 层暴露缺口。
 */
const KIND_KEY: Record<string, string> = {
  empty_dirs: "maintenance.kindEmptyDirs",
  stale_uploads: "maintenance.kindStaleUploads",
  pre_restore: "maintenance.kindPreRestore",
  quarantine: "maintenance.kindQuarantine",
};

/** 类别标签；未知类别回落到原始 kind（便于诊断，也不显示成键名）。 */
function kindLabel(t: (key: string) => string, kind: string): string {
  const key = KIND_KEY[kind];
  return key ? t(key) : kind;
}

export interface RecentCleanupPanelProps {
  /** 外部刷新信号（清理/恢复动作后由父级 bump）。 */
  refreshKey: number;
  /** 「查看全部」跳到清理任务 Tab。 */
  onViewAll: () => void;
}

export function RecentCleanupPanel({ refreshKey, onViewAll }: RecentCleanupPanelProps) {
  const { t } = useTranslation();
  const [tasks, setTasks] = useState<CleanTask[] | null>(null);
  const [failed, setFailed] = useState(false);

  const load = useCallback(() => {
    listCleanTasks(1, RECENT_LIMIT)
      .then((res) => {
        setTasks(res.items ?? []);
        setFailed(false);
      })
      // 失败不弹错、也不清空：这块是体检页的补充信息，它读不到不该让整页看起来坏了。
      // 首次就失败时 `tasks` 仍为 null，由下方空态承接。
      .catch(() => setFailed(true));
  }, []);

  useEffect(load, [load, refreshKey]);

  return (
    <Card
      withBorder
      radius="md"
      padding={density.cardPadding}
      data-testid="recent-cleanup-panel"
      style={{ flexShrink: 0 }}
    >
      <Stack gap="sm">
        <Group justify="space-between" align="center" wrap="wrap" gap="xs">
          <Stack gap={2} style={{ minWidth: 0 }}>
            <Text fw={600} size="sm">
              {t("maintenance.recentTitle")}
            </Text>
            <Text size="xs" c="dimmed">
              {t("maintenance.recentHint")}
            </Text>
          </Stack>
          <Button
            size="compact-xs"
            variant="subtle"
            rightSection={<IconArrowRight size={14} />}
            onClick={onViewAll}
          >
            {t("maintenance.recentViewAll")}
          </Button>
        </Group>

        {tasks === null ? (
          // 首次加载：给两行骨架而不是"暂无记录"。后者会在慢请求下变成一句错话
          // （实际有记录，只是还没到），和本页其它区块的诚实口径不一致。
          failed ? (
            <Text size="sm" c="dimmed" data-testid="recent-cleanup-unavailable">
              {t("maintenance.recentUnavailable")}
            </Text>
          ) : (
            <Stack gap={6}>
              <Skeleton height={16} radius="sm" />
              <Skeleton height={16} radius="sm" width="70%" />
            </Stack>
          )
        ) : tasks.length === 0 ? (
          <Text size="sm" c="dimmed" data-testid="recent-cleanup-empty">
            {t("maintenance.recentEmpty")}
          </Text>
        ) : (
          <Stack gap={6}>
            {tasks.map((task) => (
              <Group
                key={task.ID}
                justify="space-between"
                wrap="nowrap"
                gap="sm"
                data-recent-task={task.ID}
              >
                <Group gap="xs" wrap="nowrap" style={{ minWidth: 0 }}>
                  <Badge size="sm" variant="light" color={statusColor(task.Status)}>
                    {task.Status}
                  </Badge>
                  <Text size="sm" truncate>
                    {/* 未知类别如实显示原值：这比显示一个 i18n 键名或空白更可诊断。 */}
                    {kindLabel(t, task.Kind)}
                  </Text>
                </Group>
                <Group gap="md" wrap="nowrap" style={{ flexShrink: 0 }}>
                  <Text size="xs" c="dimmed" style={{ fontVariantNumeric: "tabular-nums" }}>
                    {t("maintenance.recentProcessed", {
                      done: task.ProcessedItems,
                      total: task.TotalItems,
                    })}
                  </Text>
                  <Box w={72} ta="right">
                    <Text size="xs" c="dimmed" style={{ fontVariantNumeric: "tabular-nums" }}>
                      {formatBytes(task.FreedBytes)}
                    </Text>
                  </Box>
                  <Box w={92} ta="right">
                    <Text size="xs" c="dimmed">
                      {task.CreatedAt}
                    </Text>
                  </Box>
                </Group>
              </Group>
            ))}
          </Stack>
        )}
      </Stack>
    </Card>
  );
}
