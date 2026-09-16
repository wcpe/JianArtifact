// DevMock 控制台：数据量 × 网速矩阵。
//
// 用途：在开发态一次性切换"后端返回多大数据 + 网络多慢"，用来验证两类问题：
// - 数据量：前端在大数组下是否仍然流畅（趋势图降采样、列表虚拟化、渲染护栏）；
// - 网速：接口挂起 / 慢响应时页面是否仍可交互，而不是"整页卡死"。
//
// 配置持久化到 localStorage，改动立即对**后续请求**生效（无需刷新页面）。
// 默认 small + fast：与改造前行为一致，避免影响既有测试与手工验收基线。
export type MockVolume = "small" | "medium" | "large";
export type MockSpeed = "fast" | "medium" | "slow";

export interface MockConsoleConfig {
  volume: MockVolume;
  speed: MockSpeed;
}

export const MOCK_VOLUMES: MockVolume[] = ["small", "medium", "large"];
export const MOCK_SPEEDS: MockSpeed[] = ["fast", "medium", "slow"];

const STORAGE_KEY = "jianartifact.devmock.console";

/** 数据量倍数：小 = 原始种子；中 = 4 倍；大 = 16 倍（足以压出渲染瓶颈）。 */
const VOLUME_FACTOR: Record<MockVolume, number> = { small: 1, medium: 4, large: 16 };

/** 单请求模拟延迟：快 = 立即；中 = 400ms；慢 = 1.5s（接近用户报告的"接口卡住"体感）。 */
const SPEED_DELAY_MS: Record<MockSpeed, number> = { fast: 0, medium: 400, slow: 1500 };

/**
 * 档位含义的单一真源：控制台 UI 用它把矩阵标注成「少量 ×1 / 快 0ms」这类自解释文案，
 * 避免 UI 里再抄一份数字（改档位含义时只改这里）。
 */
export const MOCK_VOLUME_FACTORS: Record<MockVolume, number> = VOLUME_FACTOR;
export const MOCK_SPEED_DELAYS: Record<MockSpeed, number> = SPEED_DELAY_MS;

const DEFAULT_CONFIG: MockConsoleConfig = { volume: "small", speed: "fast" };

function isVolume(value: unknown): value is MockVolume {
  return value === "small" || value === "medium" || value === "large";
}

function isSpeed(value: unknown): value is MockSpeed {
  return value === "fast" || value === "medium" || value === "slow";
}

function readStored(): MockConsoleConfig {
  try {
    if (typeof localStorage === "undefined") return { ...DEFAULT_CONFIG };
    const raw = localStorage.getItem(STORAGE_KEY);
    if (!raw) return { ...DEFAULT_CONFIG };
    const parsed = JSON.parse(raw) as Partial<MockConsoleConfig>;
    return {
      volume: isVolume(parsed.volume) ? parsed.volume : DEFAULT_CONFIG.volume,
      speed: isSpeed(parsed.speed) ? parsed.speed : DEFAULT_CONFIG.speed,
    };
  } catch {
    return { ...DEFAULT_CONFIG };
  }
}

let config: MockConsoleConfig = readStored();
const listeners = new Set<(value: MockConsoleConfig) => void>();

/** 读取当前配置（快照）。 */
export function getMockConsole(): MockConsoleConfig {
  return config;
}

/** 更新配置并广播；持久化失败（隐私模式等）不影响本次会话生效。 */
export function setMockConsole(patch: Partial<MockConsoleConfig>): MockConsoleConfig {
  config = { ...config, ...patch };
  try {
    localStorage?.setItem(STORAGE_KEY, JSON.stringify(config));
  } catch {
    /* 忽略存储失败 */
  }
  for (const listener of listeners) listener(config);
  return config;
}

/** 订阅配置变化；返回取消订阅函数。 */
export function subscribeMockConsole(listener: (value: MockConsoleConfig) => void): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

/** 当前数据量倍数（1 / 4 / 16），供各 handler 放大响应规模。 */
export function mockVolumeFactor(): number {
  return VOLUME_FACTOR[config.volume];
}

/** 当前单请求模拟延迟（毫秒）。 */
export function mockNetworkDelayMs(): number {
  return SPEED_DELAY_MS[config.speed];
}

/**
 * 档位放大**不在本模块做**，而是「物化」进数据层：见 `store.ts` 的
 * `reconcileVolumeRepositories`（模块加载 + 档位变更时把种子仓库展开成真实存在的克隆仓库）。
 *
 * 曾经这里有一个 `scaleList(items, factor, key)`，只是把**响应里**的条目按倍数重复改名
 * （仓库名加 `-v{r}` 后缀、数值主键加 `round * 1_000_000` 偏移）。它有两个致命问题，
 * 所以整体删掉了：
 * 1. 产出的是**假条目**——store 里并不存在，用户点进去所有按名查询一律 404「仓库不存在」；
 * 2. 只偏移了传入的 key 与数值 `id`，字符串业务主键（如审计 `eventId`）在克隆之间重复，
 *    一旦被消费方当作标识就会落到错误对象上。
 *
 * 档位放大只影响：仓库域（真实物化）、仪表盘趋势桶数、主机采样条数。
 */
