// Vitest 测试初始化：引入 jest-dom 断言扩展，提供 toBeInTheDocument 等匹配器。
import '@testing-library/jest-dom/vitest';

// 初始化 i18n（FR-111）：测试中组件经 useTranslation 渲染中文文案，须在渲染前装载全局 i18n 单例。
import '../i18n';

// Mantine 组件依赖 window.matchMedia（响应式与配色方案），jsdom 不提供，需打桩。
// 用普通函数而非 vi.fn()，以免测试里的 vi.restoreAllMocks() 把它清除导致后续渲染报错。
Object.defineProperty(window, 'matchMedia', {
  writable: true,
  configurable: true,
  value: (query: string): MediaQueryList =>
    ({
      matches: false,
      media: query,
      onchange: null,
      addListener: () => {},
      removeListener: () => {},
      addEventListener: () => {},
      removeEventListener: () => {},
      dispatchEvent: () => false,
    }) as unknown as MediaQueryList,
});

// Mantine 的 ScrollArea / 部分组件在测试环境会用到 ResizeObserver，jsdom 不提供，需打桩。
globalThis.ResizeObserver = class {
  observe() {}
  unobserve() {}
  disconnect() {}
};

// Mantine Combobox（Select 下拉）打开时会在定时器里调用选项的 scrollIntoView，jsdom 不提供，需打桩。
// 缺失会在下拉打开后抛 Uncaught Exception（异步定时器，污染测试运行），故全局补一个空实现。
Element.prototype.scrollIntoView = () => {};

// 设置页锚点导航（FR-103）用 IntersectionObserver 维护当前高亮节，jsdom 不提供，需打桩为空实现。
// 单测不断言可视区计算（无真实布局），只断言导航存在与点击滚动，故空桩即可、不触发回调。
globalThis.IntersectionObserver = class {
  observe() {}
  unobserve() {}
  disconnect() {}
  takeRecords() {
    return [];
  }
} as unknown as typeof IntersectionObserver;
