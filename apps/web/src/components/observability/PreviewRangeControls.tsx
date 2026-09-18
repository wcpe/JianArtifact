import { Button, Group } from "@mantine/core";

export interface RangeOption<T extends string = string> {
  value: T;
  label: string;
}

interface PreviewRangeControlsProps<T extends string = string> {
  /** 当前选中档位。 */
  value: T;
  onChange: (value: T) => void;
  /**
   * 档位列表（必填）。此前有「缺省为静态预览 24h/7d/30d」的内置默认值，但唯一调用点
   * 始终自行传入经 i18n 翻译的档位——那份内置默认值是永不生效的死代码，且带着硬编码
   * 中文会绕过翻译，故删除、改为必填。
   */
  options: Array<{ value: T; label: string }>;
  /** 无障碍组标签（必填，同理由调用方提供翻译后的文案）。 */
  ariaLabel: string;
}

export function PreviewRangeControls<T extends string = string>({
  value,
  onChange,
  options,
  ariaLabel,
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
