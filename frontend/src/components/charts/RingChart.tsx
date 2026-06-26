// 手搓环形占比图（FR-99）：纯 SVG，零依赖。
// 用一段背景圆 + 一段前景圆弧（stroke-dasharray）表示 0~100% 占比，中心叠加百分比与标签。
// 颜色经 currentColor / CSS 变量适配主题，不引图表库。

/** 环形图属性。 */
interface RingChartProps {
  /** 占比百分比（0~100，越界自动钳制）。 */
  value: number;
  /** 标签（如 CPU / 内存 / 磁盘），同时用作无障碍名。 */
  label: string;
  /** 辅助说明（可选，如「8 / 16 GB」），展示在标签下方。 */
  caption?: string;
  /** 直径（像素），默认 120。 */
  size?: number;
}

/** 单值占比环形图。 */
export function RingChart({ value, label, caption, size = 120 }: RingChartProps) {
  // 钳制到 0~100，避免越界弧长
  const pct = Math.max(0, Math.min(100, Math.round(value)));
  const stroke = 10;
  const radius = (size - stroke) / 2;
  const circumference = 2 * Math.PI * radius;
  // 前景弧长 = 占比 × 周长；其余为间隔，形成「填充到 pct%」的视觉
  const dash = (pct / 100) * circumference;
  const center = size / 2;

  return (
    <div style={{ display: 'inline-flex', flexDirection: 'column', alignItems: 'center' }}>
      <svg
        width={size}
        height={size}
        viewBox={`0 0 ${size} ${size}`}
        role="img"
        aria-label={`${label} ${pct}%`}
      >
        {/* 背景圆环（淡色轨道） */}
        <circle
          cx={center}
          cy={center}
          r={radius}
          fill="none"
          stroke="var(--mantine-color-gray-3)"
          strokeWidth={stroke}
        />
        {/* 前景弧（占比），从 12 点方向起顺时针 */}
        <circle
          cx={center}
          cy={center}
          r={radius}
          fill="none"
          stroke="var(--mantine-primary-color-filled)"
          strokeWidth={stroke}
          strokeLinecap="round"
          strokeDasharray={`${dash} ${circumference - dash}`}
          transform={`rotate(-90 ${center} ${center})`}
        />
        {/* 中心百分比 */}
        <text
          x={center}
          y={center}
          textAnchor="middle"
          dominantBaseline="central"
          fontSize={size * 0.22}
          fontWeight={700}
          fill="var(--mantine-color-text)"
        >
          {pct}%
        </text>
      </svg>
      <span style={{ marginTop: 4, fontSize: 14 }}>{label}</span>
      {caption && (
        <span style={{ fontSize: 12, color: 'var(--mantine-color-dimmed)' }}>{caption}</span>
      )}
    </div>
  );
}
