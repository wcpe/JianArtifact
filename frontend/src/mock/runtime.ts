// 运行时 Mock 模式开关与启动（FR-119，ADR-0035）。
//
// 复用 FR-116 的内存 store + MSW handlers，做一个**运行时可开关**的 Mock 模式：
// 开启后由 MSW 浏览器 worker 拦截全部 /api/v1/*，全操作走内存 CRUD，无需真实后端。
// 默认关闭、生产不受影响。开关来源（任一为真即启用）：
//   1) 构建期 env：import.meta.env.VITE_MOCK === 'true'（如 `VITE_MOCK=true pnpm dev`）；
//   2) 运行时持久开关：localStorage[MOCK_FLAG_KEY] === 'on'（由页内开关写入，刷新保留）；
//   3) 一次性 URL 参数：?mock=1 开启并落盘、?mock=0 关闭并落盘（便于分享带 Mock 的链接）。

/** localStorage 中持久化运行时 Mock 开关的键名。 */
const MOCK_FLAG_KEY = 'jianartifact.mock';

/** 读取构建期 env 开关（缺省视为关闭）。 */
function envEnabled(): boolean {
  return import.meta.env.VITE_MOCK === 'true';
}

/**
 * 处理 URL 上的一次性 ?mock= 参数：?mock=1 开启、?mock=0 关闭，落盘 localStorage。
 * 处理后从地址栏移除该参数，避免污染后续导航。
 */
export function applyUrlOverride(): void {
  const params = new URLSearchParams(window.location.search);
  const value = params.get('mock');
  if (value === null) {
    return;
  }
  if (value === '1' || value === 'on' || value === 'true') {
    localStorage.setItem(MOCK_FLAG_KEY, 'on');
  } else if (value === '0' || value === 'off' || value === 'false') {
    localStorage.removeItem(MOCK_FLAG_KEY);
  }
  params.delete('mock');
  const qs = params.toString();
  const url = window.location.pathname + (qs ? `?${qs}` : '') + window.location.hash;
  window.history.replaceState(null, '', url);
}

/** 当前运行时 Mock 模式是否启用（综合 env / localStorage，URL 覆盖已在启动时落盘）。 */
export function isMockEnabled(): boolean {
  return envEnabled() || localStorage.getItem(MOCK_FLAG_KEY) === 'on';
}

/** 设置运行时 Mock 开关并刷新页面使之生效（worker 注册 / 注销需重载）。 */
export function setMockEnabled(enabled: boolean): void {
  if (enabled) {
    localStorage.setItem(MOCK_FLAG_KEY, 'on');
  } else {
    localStorage.removeItem(MOCK_FLAG_KEY);
  }
  window.location.reload();
}

/**
 * 若启用则启动运行时 Mock 模式：动态 import 浏览器 worker + 种子数据并 start()。
 * 在 main.tsx 渲染前 await 调用；未启用则立即返回，**不引入 worker 代码、不拦截**（生产零影响）。
 * 经动态 import 隔离，未开启时该分支可被打包器摇树 / 懒加载，不进首屏关键路径。
 */
export async function startMockRuntime(): Promise<void> {
  applyUrlOverride();
  if (!isMockEnabled()) {
    return;
  }
  const { worker } = await import('../test/mocks/browser');
  const { seed } = await import('../test/mocks/store');
  // 预置示例数据，便于一开即用地登录 / 建仓库 / 浏览。
  seed();
  await worker.start({
    // 未被 handlers 覆盖的请求（如静态资源）放行，不报错。
    onUnhandledRequest: 'bypass',
    quiet: true,
  });
}
