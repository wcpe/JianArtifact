// 迁移结果四宫格：复制 / 跳过 / 失败 / 合计。
// 渲染统一走 OpsKpiBand（全站唯一 KPI 口径），不再自带一套卡片实现。
import {
  IconAlertTriangle,
  IconCircleCheck,
  IconPlayerSkipForward,
  IconStack2,
} from "@tabler/icons-react";
import { useTranslation } from "react-i18next";

import { OpsKpiBand } from "../ops/OpsKit";
import type { Totals } from "./status";

interface Props {
  totals: Totals;
  /** 计划估算资产数（可选） */
  estimated?: number;
}

export function MigrationStatCards({ totals, estimated }: Props) {
  const { t } = useTranslation();
  const sum = totals.copied + totals.skipped + totals.failed;

  return (
    <OpsKpiBand
      label={t("migrations.statProcessed")}
      cols={{ base: 2, sm: 4 }}
      items={[
        {
          label: t("migrations.progressCopied"),
          value: totals.copied.toLocaleString(),
          icon: <IconCircleCheck size={18} />,
          tone: "green",
        },
        {
          label: t("migrations.progressSkipped"),
          value: totals.skipped.toLocaleString(),
          icon: <IconPlayerSkipForward size={18} />,
          tone: "gray",
        },
        {
          label: t("migrations.progressFailed"),
          value: totals.failed.toLocaleString(),
          icon: <IconAlertTriangle size={18} />,
          tone: "red",
          danger: totals.failed > 0,
        },
        {
          label: t("migrations.statProcessed"),
          value: sum.toLocaleString(),
          icon: <IconStack2 size={18} />,
          tone: "blue",
          hint:
            estimated && estimated > 0
              ? t("migrations.statEstimated", { n: estimated })
              : undefined,
        },
      ]}
    />
  );
}
