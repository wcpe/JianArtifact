// 全局顶部进度条（FR-127）：从 FR-112 TopProgressBar 提升而来，
// fixed 定位于视口最顶端（页眉上方），消费 GlobalProgressContext。
// 自研，不引第三方依赖；伪进度逻辑与 TopProgressBar 复用同一模式。

import { useEffect, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useGlobalProgress } from '../hooks/useGlobalProgress';

/** 伪进度每步逼近目标 90% 的衰减步进间隔（毫秒）。 */
const TICK_MS = 200;
/** 完成后补满 100% 到淡出移除的停留时长（毫秒）。 */
const DONE_MS = 300;

/**
 * 全局顶部进度条：fixed 定位于视口最顶端（z-index 高于页眉），
 * 页面切换 / 数据加载时显示并缓慢爬升，完成后补满 100% 并淡出。
 *
 * 注意：fixed 定位不依赖父容器，可直接放入任意祖先节点（如 AppShell.Header 内部或之前）。
 */
export function GlobalTopProgressBar() {
  const { t } = useTranslation('common');
  const { loading } = useGlobalProgress();

  // visible 控制是否挂载渲染；width 为当前进度百分比。
  const [visible, setVisible] = useState(loading);
  const [width, setWidth] = useState(0);
  const timerRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  useEffect(() => {
    if (loading) {
      setVisible(true);
      setWidth(0);
      // 伪进度：每 tick 朝 90% 逼近其剩余距离的一小段，永不自达 100%。
      const tick = () => {
        setWidth((w) => (w >= 90 ? w : w + (90 - w) * 0.15));
        timerRef.current = setTimeout(tick, TICK_MS);
      };
      timerRef.current = setTimeout(tick, TICK_MS);
      return () => {
        if (timerRef.current) clearTimeout(timerRef.current);
      };
    }
    // 加载结束：补满到 100%，停留片刻后淡出移除。
    if (timerRef.current) clearTimeout(timerRef.current);
    setWidth(100);
    const done = setTimeout(() => setVisible(false), DONE_MS);
    return () => clearTimeout(done);
  }, [loading]);

  if (!visible) return null;

  return (
    <div
      role="progressbar"
      aria-label={t('loadingProgress')}
      data-testid="global-progress-bar"
      style={{
        // fixed 定位于视口最顶端，z-index 高于页眉（AppShell header 约 200 层级）。
        position: 'fixed',
        top: 0,
        left: 0,
        right: 0,
        height: 3,
        width: `${width}%`,
        backgroundColor: 'var(--mantine-primary-color-filled)',
        borderRadius: 2,
        transition: 'width 200ms ease-out, opacity 300ms ease-out',
        opacity: width >= 100 ? 0 : 1,
        zIndex: 9999,
        pointerEvents: 'none',
      }}
    />
  );
}
