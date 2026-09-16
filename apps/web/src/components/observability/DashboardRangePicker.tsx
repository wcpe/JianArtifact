// 仪表盘时间范围选择器：用 DateRangePicker 做真实时间筛选，并内置快捷预设。
// 预设：24 小时 / 三天 / 七天 / 三十天 / 半年 / 一年；也可自由选起止日期。
import { Button, Group, Popover, Stack, Text } from "@mantine/core";
import { DatePicker } from "@mantine/dates";
import { IconCalendar, IconCheck, IconChevronDown } from "@tabler/icons-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";

export interface DashboardRange {
  /** 当前选中的快捷预设 key；自定义范围时为 "custom"。 */
  preset: string;
  /** ISO 时间字符串。 */
  from: string;
  to: string;
}

interface Preset {
  key: string;
  labelKey: string;
  /** rolling：距今 N 天的滚动窗口；calendar：自然日边界（今天=当天 00:00 → 现在，昨天=昨天 00:00 → 今天 00:00）。 */
  days: number;
  kind?: "rolling" | "calendar";
}

const PRESETS: Preset[] = [
  { key: "today", labelKey: "dashboard.rangeToday", days: 0, kind: "calendar" },
  { key: "yesterday", labelKey: "dashboard.rangeYesterday", days: 1, kind: "calendar" },
  { key: "24h", labelKey: "dashboard.range24h", days: 1 },
  { key: "3d", labelKey: "dashboard.range3d", days: 3 },
  { key: "7d", labelKey: "dashboard.range7d", days: 7 },
  { key: "30d", labelKey: "dashboard.range30d", days: 30 },
  { key: "6m", labelKey: "dashboard.range6m", days: 182 },
  { key: "1y", labelKey: "dashboard.range1y", days: 365 },
];

function calendarRange(kind: "today" | "yesterday"): { from: Date; to: Date } {
  const now = new Date();
  const dayStart = new Date(now.getFullYear(), now.getMonth(), now.getDate());
  if (kind === "today") {
    return { from: dayStart, to: now };
  }
  const yesterdayStart = new Date(dayStart.getTime() - 86_400_000);
  return { from: yesterdayStart, to: dayStart };
}

function presetRange(preset: Preset): { from: string; to: string } {
  if (preset.kind === "calendar") {
    const { from, to } = calendarRange(preset.key === "today" ? "today" : "yesterday");
    return { from: from.toISOString(), to: to.toISOString() };
  }
  const now = new Date();
  const from = new Date(now.getTime() - preset.days * 86_400_000);
  return { from: from.toISOString(), to: now.toISOString() };
}

function presetLabel(t: (key: string) => string, preset: string): string {
  switch (preset) {
    case "today":
      return t("dashboard.rangeToday");
    case "yesterday":
      return t("dashboard.rangeYesterday");
    case "24h":
      return t("dashboard.range24h");
    case "3d":
      return t("dashboard.range3d");
    case "7d":
      return t("dashboard.range7d");
    case "30d":
      return t("dashboard.range30d");
    case "6m":
      return t("dashboard.range6m");
    case "1y":
      return t("dashboard.range1y");
    default:
      return preset;
  }
}

export function DashboardRangePicker({
  value,
  onChange,
}: {
  value: DashboardRange;
  onChange: (next: DashboardRange) => void;
}) {
  const { t } = useTranslation();
  const [opened, setOpened] = useState(false);
  const [custom, setCustom] = useState<[Date | null, Date | null]>([
    value.preset === "custom" ? new Date(value.from) : null,
    value.preset === "custom" ? new Date(value.to) : null,
  ]);

  const label =
    value.preset === "custom"
      ? `${new Date(value.from).toLocaleDateString("zh-CN")} ~ ${new Date(value.to).toLocaleDateString("zh-CN")}`
      : presetLabel(t, value.preset);

  const applyPreset = (preset: Preset) => {
    const { from, to } = presetRange(preset);
    onChange({ preset: preset.key, from, to });
    setOpened(false);
  };

  const applyCustom = () => {
    if (!custom[0] || !custom[1]) return;
    const from = custom[0] < custom[1] ? custom[0] : custom[1];
    const to = custom[0] < custom[1] ? custom[1] : custom[0];
    onChange({
      preset: "custom",
      from: from.toISOString(),
      to: to.toISOString(),
    });
    setOpened(false);
  };

  return (
    <Popover opened={opened} onChange={setOpened} position="bottom-end" withArrow shadow="md">
      <Popover.Target>
        <Button
          variant="default"
          size="xs"
          leftSection={<IconCalendar size={16} />}
          rightSection={<IconChevronDown size={14} />}
          onClick={() => setOpened((v) => !v)}
          aria-label={label}
          title={t("dashboard.rangeLabel")}
        >
          {label}
        </Button>
      </Popover.Target>
      <Popover.Dropdown>
        <Stack gap="sm">
          <Text size="xs" fw={600} c="dimmed">
            {t("dashboard.rangeQuick")}
          </Text>
          <Group gap="xs">
            {PRESETS.map((preset) => (
              <Button
                key={preset.key}
                size="xs"
                variant={value.preset === preset.key ? "filled" : "light"}
                aria-pressed={value.preset === preset.key}
                onClick={() => applyPreset(preset)}
              >
                {t(preset.labelKey)}
              </Button>
            ))}
          </Group>
          <Text size="xs" fw={600} c="dimmed" mt="xs">
            {t("dashboard.rangeCustom")}
          </Text>
          <DatePicker
            type="range"
            value={custom}
            onChange={setCustom}
            locale="zh-CN"
            numberOfColumns={2}
            size="xs"
          />
          <Button
            size="xs"
            onClick={applyCustom}
            disabled={!custom[0] || !custom[1]}
            leftSection={<IconCheck size={14} />}
          >
            {t("dashboard.rangeApply")}
          </Button>
        </Stack>
      </Popover.Dropdown>
    </Popover>
  );
}
