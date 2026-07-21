// 浏览器端 Mock：Service Worker 拦截。web 开发态在 main.tsx 中按环境开关启动。
// 需 public/mockServiceWorker.js（由 `pnpm --filter @jianartifact/web msw:init` 生成）。
import { setupWorker } from "msw/browser";

import { handlers } from "./msw";

/** 浏览器端 MSW worker；调用 `worker.start()` 后拦截同源 fetch。 */
export const worker = setupWorker(...handlers);
