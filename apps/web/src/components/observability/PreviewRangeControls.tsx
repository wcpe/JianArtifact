import { Button, Group } from "@mantine/core";

import type { PreviewRange } from "../../mocks/observabilityPreview";

export interface RangeOption<T extends string = string> {
  value: T;
  label: string;
}

const DEFAULT_RANGES: Array<{ value: PreviewRange; label: string }> = [
  { value: "24h", label: "近 24 小时" },
  { value: "7d", label: "近 7 天" },
  { value: "30d", label: "近 30 天" },
];

interface PreviewRangeControlsProps<T extends string = string> {
  /** 当前选中档位。 */
  value: T;
  onChange: (value: T) => void;
  /** 自定义档位列表；缺省为静态预览的 24h/7d/30d 三档。 */
  options?: Array<{ value: T; label: string }>;
  /** 无障碍组标签。 */
  ariaLabel?: string;
}

export function PreviewRangeControls<T extends string = string>({
  value,
  onChange,
  options = DEFAULT_RANGES as Array<{ value: T; label: string }>,
  ariaLabel = "时间范围",
}: PreviewRangeControlsProps<T>) {
  return (
    <Group gap="xs" role="group" aria-label={ariaLabel}>
      {options.map((range) => (
        <Button
          key={range.value}
          size="xs"
          variant={range.value === value ? "filled" : "default"}
          aria-pressed={range.value === value}
          onClick={() => onChange(range.value)}
        >
          {range.label}
        </Button>
      ))}
    </Group>
  );
}
