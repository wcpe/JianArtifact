// Vitest 全局装置：注入 jsdom 垫片，并以 MSW Node server 拦截 /api/v1 请求。
// 每个用例后重置 handlers 与内存态 store、清理 localStorage，保证用例间隔离。
import { server } from "@jianartifact/devmock/node";
import { resetStore } from "@jianartifact/devmock";
import { afterAll, afterEach, beforeAll } from "vitest";

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

// jsdom 未实现 scrollIntoView；Mantine Combobox（Select/MultiSelect）选项聚焦时会调用它。
if (!Element.prototype.scrollIntoView) {
  Element.prototype.scrollIntoView = () => {};
}

beforeAll(() => {
  server.listen({ onUnhandledRequest: "error" });
});

afterEach(() => {
  server.resetHandlers();
  resetStore();
  localStorage.clear();
});

afterAll(() => {
  server.close();
});
