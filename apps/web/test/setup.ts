// Vitest 全局装置：注入 jsdom 垫片，并以 MSW Node server 拦截 /api/v1 请求。
// 每个用例后卸载 React、释放场景挂起请求并重置浏览器态，保证用例间隔离。
import { Blob as NodeBlob, File as NodeFile } from "node:buffer";

import { configure } from "@testing-library/react";
import { server } from "@jianartifact/devmock/node";
import { resetDevMockScenario, resetStore, resetMockBackups } from "@jianartifact/devmock";

import { clearAsyncCache } from "../src/hooks/useAsync";
import { notifications } from "@mantine/notifications";
import { cleanup } from "@testing-library/react";
import { afterAll, afterEach, beforeAll } from "vitest";

// 懒加载路由 + 图表重页面在全量并行负载下可能超过 findBy* 默认 1s 等待，
// 统一放宽到 5s，与用例内显式 timeout: 5_000 的口径一致（真失败仍会稳定超时）。
configure({ asyncUtilTimeout: 5_000 });

// jsdom 的 FormData/File/Blob 与 Node fetch（undici）不同源：undici 不认 jsdom 的
// FormData，multipart 请求会丢失 Content-Type 边界导致 formData() 解析失败。
// 统一替换为 Node 实现（Response.formData() 返回的即 undici FormData 类）。
const nodeFormData = (
  await new Response("a=b", {
    headers: { "content-type": "application/x-www-form-urlencoded" },
  }).formData()
).constructor;
globalThis.FormData = nodeFormData as typeof FormData;
globalThis.File = NodeFile as unknown as typeof File;
globalThis.Blob = NodeBlob as unknown as typeof Blob;

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

// jsdom 无布局引擎：recharts 的 ResponsiveContainer 依赖 ResizeObserver 上报
// 容器尺寸，空壳 stub 会让图表停在 0×0（产生渲染告警并反复空跑）。
// 这里给固定 800×300 的逻辑尺寸，observe 后经微任务回调一次，让图表正常测量渲染。
const CHART_STUB_WIDTH = 800;
const CHART_STUB_HEIGHT = 300;
const chartStubSizes = new WeakMap<Element, { width: number; height: number }>();

function makeResizeObserverEntry(
  target: Element,
  width: number,
  height: number,
): ResizeObserverEntry {
  const rect = {
    x: 0,
    y: 0,
    top: 0,
    left: 0,
    bottom: height,
    right: width,
    width,
    height,
    toJSON: () => ({}),
  } as DOMRectReadOnly;
  return {
    target,
    contentRect: rect,
    borderBoxSize: [{ inlineSize: width, blockSize: height }],
    contentBoxSize: [{ inlineSize: width, blockSize: height }],
    devicePixelContentBoxSize: [{ inlineSize: width, blockSize: height }],
  } as unknown as ResizeObserverEntry;
}

class ResizeObserverStub implements ResizeObserver {
  private callbacks = new Map<Element, (element: Element) => void>();

  observe(target: Element): void {
    const previous = chartStubSizes.get(target);
    if (previous && previous.width === CHART_STUB_WIDTH && previous.height === CHART_STUB_HEIGHT) {
      return;
    }
    chartStubSizes.set(target, { width: CHART_STUB_WIDTH, height: CHART_STUB_HEIGHT });
    const fire = (element: Element) => {
      const callback = this.callbacks.get(element);
      callback?.(this.toEntry(element));
    };
    this.callbacks.set(target, fire);
    // 模拟真实 RO 的首次回调：在微任务中上报一次固定尺寸。
    queueMicrotask(() => fire(target));
  }

  unobserve(target: Element): void {
    this.callbacks.delete(target);
  }

  disconnect(): void {
    this.callbacks.clear();
  }

  private toEntry(target: Element): ResizeObserverEntry {
    return makeResizeObserverEntry(target, CHART_STUB_WIDTH, CHART_STUB_HEIGHT);
  }
}

// 无论 jsdom/vitest 是否已声明，都保证裸全局 ResizeObserver（recharts 用 `new ResizeObserver`）
// 指向固定尺寸的 stub；若环境自带真实实现则保留。
const ExistingResizeObserver = (globalThis as { ResizeObserver?: typeof ResizeObserver })
  .ResizeObserver;
if (!ExistingResizeObserver) {
  window.ResizeObserver = ResizeObserverStub;
  globalThis.ResizeObserver = ResizeObserverStub as unknown as typeof ResizeObserver;
}

// jsdom 未实现 scrollIntoView；Mantine Combobox（Select/MultiSelect）选项聚焦时会调用它。
if (!Element.prototype.scrollIntoView) {
  Element.prototype.scrollIntoView = () => {};
}

beforeAll(() => {
  server.listen({ onUnhandledRequest: "error" });
});

afterEach(() => {
  cleanup();
  clearAsyncCache();
  resetDevMockScenario();
  server.resetHandlers();
  resetStore();
  // 备份包 mock 状态是独立内存态，不随 resetStore 复位，需单独清理。
  resetMockBackups();
  localStorage.clear();
  window.history.replaceState({}, "", "/");
  // 清空 Mantine 通知，避免上一个用例的通知泄漏到下一用例的 DOM。
  notifications.clean();
});

afterAll(() => {
  server.close();
});
