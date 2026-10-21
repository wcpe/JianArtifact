// 搜索进度条（FR-152）：结果区顶部的一条**伪进度** + 阶段文案 + 已用时长 + 取消按钮。
//
// 只在请求进行中挂载：请求完成 / 失败 / 取消时父组件直接卸载本组件，因此不会残留进度条，
// 也不会出现「停在 100% 却还在等」的假卡死。定时器由 usePseudoProgress 在卸载时清理。
//
// 位置刻意放在结果区**内部顶部**而不是替换结果区：首载时骨架屏照常显示，换排序 / 翻页时
// 旧结果留在原位（keepPreviousData），进度条与它们上下共存、互不遮挡。
import { Box, Button, Group, Progress, Text } from "@mantine/core";
import { useTranslation } from "react-i18next";

import { usePseudoProgress, type SearchStage } from "../../hooks/usePseudoProgress";

/** 阶段 → i18n 键；文案在 i18n 资源里，逻辑层不写死中文。 */
const STAGE_KEYS: Record<SearchStage, string> = {
  indexing: "search.progressStageIndexing",
  filtering: "search.progressStageFiltering",
  ranking: "search.progressStageRanking",
  finalizing: "search.progressStageFinalizing",
};

interface SearchProgressProps {
  /** 取消回调：父组件据此 abort 在途请求，界面静默回到空闲态。 */
  onCancel: () => void;
}

export function SearchProgress({ onCancel }: SearchProgressProps) {
  const { t } = useTranslation();
  const { value, elapsedMs, stage } = usePseudoProgress();
  // 已用时长固定一位小数（如 1.8s）：变化细到 0.1s 才有「真的在跑」的实感。
  const seconds = (elapsedMs / 1000).toFixed(1);
  const stageLabel = t(STAGE_KEYS[stage]);

  return (
    <Box data-testid="search-progress">
      <Group justify="space-between" gap="xs" wrap="nowrap" align="center">
        <Text size="xs" c="dimmed" truncate>
          {stageLabel}
          {t("search.elapsed", { seconds })}
        </Text>
        <Button size="compact-xs" variant="subtle" color="gray" onClick={onCancel}>
          {t("search.cancel")}
        </Button>
      </Group>
      <Progress
        value={value}
        size={4}
        radius="xl"
        mt={4}
        // 进度条自带 role="progressbar" + aria-valuenow；这里补上阶段文案作为无障碍名称，
        // 避免读屏只念一个百分比。
        aria-label={`${stageLabel} ${t("search.elapsed", { seconds })}`}
      />
    </Box>
  );
}
