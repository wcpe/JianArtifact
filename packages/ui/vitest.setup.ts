// jsdom 环境垫片：Mantine 依赖 window.matchMedia 与 ResizeObserver，jsdom 未实现。
// 以 window 存在与否守卫，兼容 node 环境的纯逻辑测试（如设计令牌）。
if (typeof window !== "undefined") {
  if (!window.matchMedia) {
    window.matchMedia = (query: string): MediaQueryList =>
      ({
        matches: false,
        media: query,
        onchange: null,
        addListener: () => {},
        removeListener: () => {},
        addEventListener: () => {},
        removeEventListener: () => {},
        dispatchEvent: () => false,
      }) as MediaQueryList;
  }

  if (!window.ResizeObserver) {
    class ResizeObserverStub implements ResizeObserver {
      observe(): void {}
      unobserve(): void {}
      disconnect(): void {}
    }
    window.ResizeObserver = ResizeObserverStub;
  }
}
