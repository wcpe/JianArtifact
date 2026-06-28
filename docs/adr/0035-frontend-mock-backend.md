# ADR-0035：前端测试 mock 与运行时 Mock 模式策略（MSW 有状态内存后端）

## 状态
已接受

## 背景
前端（FR-116）现有组件 / 集成测试以手工 `vi.mock('../api/endpoints')` 打桩：直接替换 endpoints 模块的导出函数，测试断言落在"某函数被以某参数调用 + 把固定返回值渲染出来"。这种弱断言有三处不足：

1. **绕过真实请求层**：`vi.mock` 把 `endpoints.ts` 整体换掉，`client.ts` 的 URL 拼接、查询串、Bearer 头、错误结构解析、204 / 空体处理全部不被覆盖；endpoints 与 client 的契约（方法 / 路径 / 请求体）无人校验。
2. **无状态**：每个用例自行 `mockResolvedValue` 摆固定数据，"POST 建一条 → 后续 GET 应能查到"这类有状态时序无法表达，CRUD 一致性测不到。
3. **与运行时脱节**：测试桩只服务测试，无法复用为"无后端跑前端"的开发底座。

同时（FR-119）需要一个**运行时可开关的 Mock 模式**：开启后由浏览器在前端内拦截全部 `/api/v1/*`，全操作走内存 CRUD，无需真实后端即可登录 / 建仓库 / 浏览 / 造数据探索；默认关闭、生产不受影响。

两处需求（测试契约强断言 + 运行时无后端探索）本质都需要"一个实现了各 REST 端点有状态 CRUD 的假后端"。

## 决策
引入 **MSW（Mock Service Worker）** 建一套**有状态内存 mock 后端**，作为测试与运行时 Mock 模式的**共用底座**：

- **内存 store**（`src/test/mocks/store.ts`）：纯内存数据结构，按领域持有可变集合（用户 / 仓库 / 制品 / 令牌 / 审计 / 设置 / 防护 / 监控 等），提供 `reset()` 与 `seed()`（种子数据）。store 不依赖浏览器或 Node，测试与运行时同一份。
- **MSW handlers**（`src/test/mocks/handlers.ts`）：对 `endpoints.ts` 的每个端点写一条 `http.<method>('/api/v1/...')` handler，读写内存 store 实现有状态 CRUD（POST 建 → 后续 GET 查得到、DELETE 删除、重复 / 越权按后端契约返回 409 / 401 / 403 / 404），响应结构严格对齐 `types.ts` 与后端契约（列表裸数组、`/search` 分页结构、错误体 `{error:{code,message}}`）。
- **两种装载方式**，共享 store + handlers：
  - **测试（Node）**：`setupServer(...handlers)`（`msw/node`），在 `src/test/setup.ts` 全局 `listen()`，`beforeEach` 重置 store + handlers。测试不再 `vi.mock('../api/endpoints')`，而是让组件走真实 `client.ts` 发请求、被 MSW 拦截，从而**强断言真实请求方法 / 路径 / 体 + 响应渲染**。
  - **运行时（浏览器）**：`setupWorker(...handlers)`（`msw/browser`），仅当 Mock 模式开启时 `start()`，由 service worker（`public/mockServiceWorker.js`，`msw init` 生成）拦截真实 fetch。

## 理由
- **一处实现、两处复用**：内存 store + handlers 同时满足 FR-116（测试强断言）与 FR-119（运行时探索），避免两套假后端各写一遍（防复制粘贴）。
- **测真实请求层**：MSW 在网络边界（fetch）拦截，`client.ts` 的 URL / 头 / 查询串 / 错误解析全部进入被测路径，断言"真实发出的 HTTP 请求"而非"某函数被调用"，契约回归能力远强于 `vi.mock`。
- **有状态**：内存 store 让 CRUD 时序、唯一性冲突、删除可见性等成为可断言事实。
- **运行时默认关、生产零影响**：worker 仅在显式开启 Mock 模式时 `start()`；未开启则不注册、不拦截，生产构建走真实后端。MSW 仅为 `devDependency`，运行时 worker 文件按需懒加载，不进生产关键路径。

## 后果
- 新增 `devDependency`：`msw`（仅开发 / 测试用，不进生产依赖树）。
- 新增 `frontend/public/mockServiceWorker.js`（`msw init public/` 生成，运行时浏览器拦截所需；测试不需要它）。
- `src/test/setup.ts` 增 MSW server 装载 + 每用例重置；既有大量 `vi.mock('../api/endpoints')` 测试**不强制改写**——本期建立可复用 MSW 夹具并把若干关键页面（仓库 / 令牌 / 登录流）改 / 加为走 MSW 的强断言示范，其余逐步迁移。
- handlers 须随 `endpoints.ts` / `types.ts` 契约变更同步维护（视为契约的可执行镜像）。
- 运行时 Mock 模式的"浏览器内真实 service worker 拦截"端到端体验在 jsdom 下不可全测；store CRUD 与 handler 契约下沉为单测覆盖，浏览器实拦截列为真机待验。

## 备选方案
- **继续手工 `vi.mock`**：维持现状，弱断言、无状态、不能复用为运行时底座——不满足 FR-116 强断言与 FR-119 复用诉求。
- **自写 fetch 打桩 / 假 client**：要么不覆盖 `client.ts` 真实路径，要么自造一套拦截框架（重复造轮子、易漏边界），不如 MSW 成熟稳定。
- **运行时 Mock 用独立的内存适配层（不经网络拦截）**：在 `client.ts` 内按开关分流到内存实现——会让生产代码长期携带 mock 分支、污染主路径，且与测试底座不共享；MSW worker 在网络边界拦截，生产代码零改动、更干净。
