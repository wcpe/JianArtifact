// 趋势序列渲染护栏：把超大序列压到可渲染规模。
//
// 背景：观测页（业务仪表盘 / 主机监控）的趋势数据点数是**后端决定**的——同一档时间范围
// 在不同采样粒度下可能返回几十到几万个点。recharts 会对每个点做路径插值，且每次 hover
// 都会重算整条 path；点数上万时单帧成本可达数百毫秒，表现就是"接口一慢 / 一返回大数组，
// 整页卡死"。这里在图表入口统一降采样，作为与后端采样粒度解耦的硬护栏。
//
// 口径：
// - 索引等步长抽样，主/次/第三条系列**共用同一组索引**，保持"按索引配对"的既有语义；
// - 首点与末点必留（区间变动、最新值读数依赖它们）；
// - 抽样碰撞去重，输出长度恒 ≤ maxPoints，且输入不超限时原样返回（零开销）。

/** 单张趋势图的最大渲染点数。超过即降采样；240 点在 200px 高的图面上已超出一像素一点。 */
export const MAX_TREND_POINTS = 240;

/**
 * 计算降采样后的索引列表（升序、去重、含首末）。
 * 输入长度不超过 maxPoints 时返回恒等序列（length 个连续索引）。
 */
export function downsampleIndexes(length: number, maxPoints: number = MAX_TREND_POINTS): number[] {
  if (length <= 0) return [];
  if (maxPoints < 2 || length <= maxPoints) {
    return Array.from({ length }, (_, index) => index);
  }
  const step = (length - 1) / (maxPoints - 1);
  const indexes: number[] = [];
  let previous = -1;
  for (let i = 0; i < maxPoints - 1; i += 1) {
    const index = Math.round(i * step);
    // round 会把相邻步长映射到同一索引，去重后仍保证整体不超过 maxPoints。
    if (index > previous) {
      indexes.push(index);
      previous = index;
    }
  }
  const last = length - 1;
  if (previous < last) indexes.push(last);
  return indexes;
}

/**
 * 按索引抽取序列。索引为空 / 序列缺失时返回 undefined（保持"该系列不存在"的语义，
 * 让 TrendChart 的 hasSecondary / hasTertiary 判定不受降采样影响）。
 */
export function pickByIndexes<T>(items: T[] | undefined, indexes: number[]): T[] | undefined {
  if (!items) return undefined;
  if (indexes.length === items.length) return items;
  return indexes.flatMap((index) => {
    const item = items[index];
    return item === undefined ? [] : [item];
  });
}
