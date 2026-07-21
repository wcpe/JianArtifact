// Node 端 Mock：拦截 vitest / jsdom 环境下的 fetch，供组件与集成测试复用同一契约行为。
import { setupServer } from "msw/node";

import { handlers } from "./msw";

/** Node 端 MSW server；测试 setup 中 `server.listen()` / `server.close()`。 */
export const server = setupServer(...handlers);
