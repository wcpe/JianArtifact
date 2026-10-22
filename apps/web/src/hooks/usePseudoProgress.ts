// 伪进度：搜索耗时不可预知，真实百分比无从得到，因此按**时间**推进出一条
// 「看上去一直在动、到末段又能干净收尾」的进度——这是用户明确要求的形态：
// 「进度条做成缓慢推进直到后段快速推进」，所以它刻意不与真实数据量绑定。
//
// 曲线（见 pseudoProgressValue）：
// - 0 → 3.2s：幂函数缓出（progress^0.6）爬到 88%。起步快、越接近 88% 越慢，
//   但**全程不停顿**（每 200ms 至少推进 1 个百分点）——用三次缓出的话末段会慢到
//   0.8s 只走 1 个百分点，那就正好是用户抱怨的「停在接近完成处、像卡死」；
// - 3.2s → 3.9s：0.7s 内线性冲刺跑完最后 12%，进一步保证收尾干脆；
// - 3.9s 之后：进度停在 100%，等请求真正结束（组件卸载）即消失。到顶那一刻把 50ms 采样
//   定时器换成 1Hz 读秒——进度已无可推进，继续 20Hz setState 只是空转重渲染；而「已用时长」
//   仍要如实往前走（冻住的读数会让人以为界面卡死，正是要避免的那种观感）。
//
// 时间来源用「累加采样间隔」而不是读挂钟：假进度只关心「过了多久」，累加值在假定时器
// 下完全确定（单测可精确断言），也避免系统时钟回拨导致进度倒退。
import { useEffect, useState } from "react";

/** 前段缓出爬升时长（毫秒）。 */
export const CRAWL_MS = 3_200;
/** 前段缓出终点百分比：末段冲刺只负责跑完最后这 12%。 */
export const CRAWL_TARGET = 88;
/** 前段缓出指数：< 1 即缓出（起步快、后段慢），0.6 是「明显减速但不呆滞」的取值。 */
export const CRAWL_EASING = 0.6;
/** 末段冲刺时长（毫秒）。 */
export const SPRINT_MS = 700;
/** 采样间隔（毫秒）：50ms 一次，肉眼足够顺滑，又不至于每帧都 setState。 */
export const TICK_MS = 50;
/** 进度到顶的时长（毫秒）。 */
export const TERMINAL_MS = CRAWL_MS + SPRINT_MS;
/** 到顶后的读秒间隔（毫秒）：进度不再动，但「已用时长」还要如实往前走。 */
export const TAIL_TICK_MS = 1_000;

/** 阶段键：文案由调用方按 i18n 渲染，这里只给语义，避免把文案写死在逻辑里。 */
export type SearchStage = "indexing" | "filtering" | "ranking" | "finalizing";

/** 阶段切点（按进度百分比，而不是另一套时间口径，免得两个阈值互相打架）。 */
const STAGE_CUTS: readonly [number, SearchStage][] = [
  [45, "indexing"],
  [70, "filtering"],
  [90, "ranking"],
];

/**
 * 由已用时长推算进度百分比（0–100 的整数）。
 * 纯函数：无副作用、无随机数、无挂钟依赖，便于穷举单测。
 */
export function pseudoProgressValue(elapsedMs: number): number {
  if (Number.isNaN(elapsedMs) || elapsedMs <= 0) {
    return 0;
  }
  if (elapsedMs < CRAWL_MS) {
    // 幂函数缓出：前段快、越接近 CRAWL_TARGET 越慢，但每个采样窗口都还在动。
    const progress = elapsedMs / CRAWL_MS;
    return Math.round(CRAWL_TARGET * progress ** CRAWL_EASING);
  }
  if (elapsedMs < CRAWL_MS + SPRINT_MS) {
    // 末段冲刺：线性跑完剩余 12%。
    const progress = (elapsedMs - CRAWL_MS) / SPRINT_MS;
    return Math.round(CRAWL_TARGET + (100 - CRAWL_TARGET) * progress);
  }
  return 100;
}

/** 由进度百分比给出阶段（阈值见 STAGE_CUTS）。 */
export function stageForProgress(value: number): SearchStage {
  for (const [cut, stage] of STAGE_CUTS) {
    if (value < cut) return stage;
  }
  return "finalizing";
}

export interface PseudoProgress {
  /** 进度百分比（0–100 的整数），直接喂给 Progress 组件。 */
  value: number;
  /** 已用时长（毫秒）。 */
  elapsedMs: number;
  /** 当前阶段。 */
  stage: SearchStage;
}

/**
 * 伪进度状态机：**只在调用方渲染期间计时**，组件卸载（请求完成 / 失败 / 取消）时
 * 定时器随之清除，不会残留进度条，也不会有卸载后 setState 的告警。
 */
export function usePseudoProgress(): PseudoProgress {
  const [elapsedMs, setElapsedMs] = useState(0);

  useEffect(() => {
    // 两段计时：
    // - 爬升段按 50ms 采样，进度与读数一起走；
    // - 到顶后进度恒为 100，只剩「已用时长」在变，把 50ms 定时器停掉，换 1Hz 读秒——
    //   继续 20Hz setState 只是让进度区空转重渲染，而请求再慢也等不出新的百分比。
    // 累计时长记在闭包里（而不是读 state）：tick 内自增才是这一刻的真实读数，
    // 读 state 只能拿到上次渲染时的旧值，节流判定会跟着漂。
    let elapsed = 0;
    let timer = window.setInterval(() => {
      elapsed = Math.min(elapsed + TICK_MS, TERMINAL_MS);
      setElapsedMs(elapsed);
      if (elapsed < TERMINAL_MS) {
        return;
      }
      window.clearInterval(timer);
      timer = window.setInterval(() => {
        elapsed += TAIL_TICK_MS;
        setElapsedMs(elapsed);
      }, TAIL_TICK_MS);
    }, TICK_MS);
    return () => window.clearInterval(timer);
  }, []);

  const value = pseudoProgressValue(elapsedMs);
  return { value, elapsedMs, stage: stageForProgress(value) };
}
