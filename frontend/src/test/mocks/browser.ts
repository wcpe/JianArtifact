// MSW 浏览器 worker（FR-119，ADR-0035）：运行时 Mock 模式开启时由 service worker
// 在浏览器内拦截全部 /api/v1/* 请求，复用与测试同一套有状态 store + handlers。
// 仅在 Mock 模式启用时按需动态 import 本模块并 start()，默认不注册、生产零影响。

import { setupWorker } from 'msw/browser';
import { handlers } from './handlers';

/** 运行时浏览器 worker，装载与测试一致的有状态 handlers。 */
export const worker = setupWorker(...handlers);
