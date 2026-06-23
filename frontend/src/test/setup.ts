// Vitest 测试初始化：引入 jest-dom 断言扩展，提供 toBeInTheDocument 等匹配器。
import '@testing-library/jest-dom/vitest';

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
