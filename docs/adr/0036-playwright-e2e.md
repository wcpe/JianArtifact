# ADR-0036：前端 Playwright E2E 测试策略（跑前端 Mock 模式、不起真后端）

## 状态
已接受

## 背景
现有前端测试（FR-116，ADR-0035）经 MSW + vitest 在 jsdom 下做组件 / 集成级强断言，覆盖请求契约与有状态 CRUD。但 jsdom 不是真浏览器：① 运行时 Mock 模式的「浏览器内真实 service worker 拦截」无法在 jsdom 全测（ADR-0035 已列为待验）；② 真实导航、路由守卫、表单提交、Mantine 组件在真渲染引擎下的端到端行为（登录 → 落地 → 浏览 → 详情）无自动化覆盖，只能人工点测。

FR-118 要求引入真浏览器端到端测试覆盖关键用户流程，并挂入 CI。需先定两件事：**用什么测试运行器**、**E2E 跑在什么后端形态之上**。

## 决策
引入 **@playwright/test** 跑真浏览器（仅 chromium）端到端测试，**E2E 目标 = 前端 Mock 模式**：

- 复用 FR-119（ADR-0035）的浏览器内有状态 mock 后端——E2E 启动前端时带 `VITE_MOCK=true`，由 MSW service worker 拦截全部 `/api/v1/*`，全操作走内存 store + handlers 的有状态 CRUD，预置种子数据（管理员 `admin/admin123` + 若干仓库 / 制品 / 令牌 / 审计）。
- Playwright `webServer` 先 `vite build` 再 `vite preview`（贴近生产构建产物、含 service worker），env 注入 `VITE_MOCK=true`，`baseURL` 指向 preview 服务；`testDir = frontend/e2e`，仅 chromium 一个 project。
- E2E 规格覆盖关键流程：公开浏览（匿名可见仓库列表）、登录 → 仪表盘、进仓库详情看文件树等。
- **CI 新增 e2e job**：装 node / pnpm + 依赖 + `playwright install --with-deps chromium`，跑 `playwright test`；与现有后端 fmt / clippy / test 质量门并行、互不影响。

## 理由
- **自包含、确定性强、CI 友好**：E2E 跑 Mock 模式则**无需起 Rust 后端、无需 bootstrap 首个管理员、无需准备数据目录 / SQLite**，种子数据固定，测试结果可复现、不受后端状态漂移影响；CI 里只需 node 工具链 + 浏览器，不必编译后端、不必拉起二进制与依赖，流水线轻、稳。
- **复用既有底座、不新造假后端**：直接用 FR-116/119 已建的 store + handlers（ADR-0035），E2E 不引入第二套 mock，避免重复造轮子与契约分叉。
- **真浏览器补 jsdom 盲区**：真渲染引擎 + 真 service worker 拦截，正好验证 ADR-0035 列为「真机待验」的运行时 Mock 拦截与端到端导航 / 路由守卫行为。
- **`vite preview` 而非 dev**：preview 跑的是 `vite build` 产物（与发布物同构），比 dev server 更接近真实前端形态，且无 HMR 干扰、更稳定。

## 后果
- 新增 `devDependency`：`@playwright/test`（仅开发 / CI 用，不进生产依赖树）；CI 需 `playwright install --with-deps chromium` 装浏览器与系统依赖（首次较慢，可缓存）。
- 新增 `frontend/playwright.config.ts` 与 `frontend/e2e/*.spec.ts`；E2E 断言依赖 Mock 模式的种子数据，种子数据变更需同步维护 E2E 期望。
- **E2E 只覆盖「前端 + Mock 后端」一侧**：不验证真 Rust 后端的协议正确性与前后端真实联通——后者仍由后端集成测试与真机验证（`mvn`/`npm`/`docker` 客户端互通等）守，E2E 不替代之。
- **真二进制端到端（前端打真后端）留后续**：其重（需编译后端、bootstrap、数据目录、清理），CI 成本与不确定性高，本期不做；如后续需要再单写 ADR。

## 备选方案
- **E2E 打真 Rust 二进制后端**：最贴近生产，但需在 CI 编译后端、bootstrap 首个管理员、准备并清理数据目录与 SQLite，流水线重、慢且易因后端状态产生 flaky；本期以 Mock 模式取轻、稳、确定性，真二进制 E2E 留后续。
- **不引 Playwright，仅靠 vitest + jsdom**：无法覆盖真浏览器 service worker 拦截与真实导航 / 渲染，FR-118「真浏览器端到端」诉求不满足。
- **用 Cypress 等其它 E2E 框架**：Playwright 原生多浏览器、`webServer` 编排、`--with-deps` 一键装浏览器、与 CI 集成成熟，且 API 现代；无引入第二套生态的理由。
