// MSW Node 测试服务器（FR-116，ADR-0035）：测试经此在网络边界拦截真实 fetch。
// 在 src/test/setup.ts 全局 listen / resetHandlers / close，每用例前重置 store 保证隔离。

import { setupServer } from 'msw/node';
import { handlers } from './handlers';

/** 全局测试用 MSW server，装载全部有状态 handlers。 */
export const server = setupServer(...handlers);
