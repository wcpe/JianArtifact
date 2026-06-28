// 顶部加载进度条（FR-112，自研、不引依赖）：加载中显示一条固定在容器顶部的细进度条，
// 进度从 0 缓增逼近但不达 100%（伪进度，避免“假完成”），加载结束时补满到 100% 再淡出消失。
// 仅用 React state + setTimeout 实现，约 30 行内，刻意不引入 @mantine/nprogress 等第三方件。

import { useEffect, useRef, useState } from 'react';

/** 伪进度每步逼近目标 90% 的衰减步进间隔（毫秒）。 */
const TICK_MS = 200;
/** 完成后补满 100% 到淡出移除的停留时长（毫秒）。 */
const DONE_MS = 300;

/**
 * 顶部进度条。`loading` 为 true 时显示并缓慢爬升；转 false 时补满 100% 后淡出。
 * 进度条绝对定位于最近的定位父容器顶部，故使用方需给容器加 position: relative。
 */
export function TopProgressBar({ loading }: { loading: boolean }) {
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
      aria-label="页面加载进度"
      style={{
        position: 'absolute',
        top: 0,
        left: 0,
        height: 3,
        width: `${width}%`,
        backgroundColor: 'var(--mantine-primary-color-filled)',
        borderRadius: 2,
        transition: 'width 200ms ease-out, opacity 300ms ease-out',
        opacity: width >= 100 ? 0 : 1,
        zIndex: 10,
      }}
    />
  );
}
