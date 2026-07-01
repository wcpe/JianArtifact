# 规格：FR-132 任务中心 + 通知中心

> 依赖：FR-131（统一任务注册表，已在 master，后端只消费不改）。  
> 本规格为**前端专项**，不改后端。

## 1. 目标

在前端控制台（仅 Admin）提供：

1. **任务中心页面**（`/tasks`，仅 Admin）：展示统一任务注册表的活跃+近期任务队列，每条显示 kind/状态/进度（时间+标签）；点入可续看各类进度（按 kind 路由到对应进度视图）；**后台完成不丢**——轮询 `GET /api/v1/tasks` 刷新，离开页面回来仍能在列表找回。
2. **通知中心**（右上角页眉图标+下拉）：轮询 `GET /api/v1/tasks`，对比上次快照，状态跃迁（`Running→Succeeded/Failed/Cancelled`）推 `@mantine/notifications` 通知。

## 2. 后端契约（只消费，不改）

### `GET /api/v1/tasks` — 列出活跃+近期任务（仅 Admin）

返回 `TaskRecord[]`：

```typescript
interface TaskRecord {
  id: string;
  kind: 'migration' | 'update' | 'vuln';
  state: 'running' | 'paused' | 'succeeded' | 'failed' | 'cancelled';
  label?: string;
  started_at: number;       // Unix 秒
  updated_at: number;       // Unix 秒
  finished_at?: number;     // Unix 秒，终态才有
  error?: string;
}
```

### `GET /api/v1/tasks/{id}` — 单任务详情（仅 Admin）

返回 `TaskDetailDto`：TaskRecord 展平 + 可选 `migration`（`OnlinePullProgress`） / `update`（`UpdateProgress`）进度字段。

## 3. 前端设计

### 3.1 任务中心页（`TaskCenterPage`，路由 `/tasks`，仅 Admin）

- 进入即启动轮询（`setInterval`，间隔 3s）`GET /api/v1/tasks`，离开组件卸载清除定时器。
- 列表展示：
  - 每行：kind 图标 + label（回落 `kind` 中文名）+ 状态 Badge（颜色映射）+ 起始时间（格式化）+ 终态时间（若有）
  - 按状态分组：活跃（Running/Paused）在上、近期完成（Succeeded/Failed/Cancelled）在下
  - 点击行 → 跳转续看：
    - `migration` → `/migration`（迁移页，该页已有轮询与进度面板）
    - `update` → `/system`（系统页，该页已有更新进度面板）
    - `vuln` → `/settings`（设置页，漏洞库刷新状态在此）
- 空状态：「暂无任务」友好提示

### 3.2 通知中心（`NotificationCenter`，嵌入 `AppLayout` 页眉右上）

- 仅 Admin 可见，每 **5s** 轮询一次 `GET /api/v1/tasks`（轮询仅在组件挂载期间）。
- 使用 `useRef` 维持上次快照 `Map<id, state>`，对比本次快照：
  - 新出现 Running → 「任务已开始」通知（蓝色，autoClose 4s）
  - Running/Paused → Succeeded → 「任务已完成」通知（绿色，autoClose 5s）
  - → Failed → 「任务失败」通知（红色，autoClose 8s）
  - → Cancelled → 「任务已取消」通知（灰色，autoClose 4s）
- 图标：`IconBell`（`@tabler/icons-react`，已装），无读/未读计数（简单实现），点击打开下拉列表（Mantine `Menu` / `Popover`）显示最近 10 条任务概要；底部「查看全部」→ `/tasks`。
- 轮询错误（鉴权失败 / 网络）静默忽略，不影响页面。

### 3.3 导航入口

在 `AppLayout.tsx` 的「系统·监控」段，在「监控」之前加「任务中心」导航项（`adminOnly: true`，path `/tasks`，icon `IconListCheck`）。

## 4. 文件清单

新增：
- `frontend/src/pages/TaskCenterPage.tsx` — 任务中心页
- `frontend/src/pages/TaskCenterPage.test.tsx` — 测试
- `frontend/src/components/NotificationCenter.tsx` — 通知中心组件
- `frontend/src/components/NotificationCenter.test.tsx` — 测试
- `frontend/src/i18n/locales/zh-CN/taskCenter.ts` — i18n 文案

修改：
- `frontend/src/api/types.ts` — 加 `TaskRecord`、`TaskDetailDto`
- `frontend/src/api/endpoints.ts` — 加 `listTasks()`、`getTask(id)`
- `frontend/src/App.tsx` — 加 `/tasks` 路由（Admin 层）
- `frontend/src/components/AppLayout.tsx` — 加「任务中心」导航项 + 引入 `NotificationCenter`
- `frontend/src/i18n/index.ts` — 注册 `taskCenter` 命名空间
- `frontend/src/i18n/locales/zh-CN/nav.ts` — 加 `tasks` 键
- `docs/ARCHITECTURE.md` — `web` 模块描述补一句
- `CHANGELOG.md` — 未发布段末尾追加一行

## 5. 验收标准

1. **任务列表渲染**：`GET /api/v1/tasks` 返回含 Migration/Update/Vuln 三类任务时，页面各行对应 kind 图标与状态 Badge 均正确渲染。
2. **轮询刷新**：mock 时钟后触发轮询，列表自动更新新增任务。
3. **点入续看路由**：点击 migration 行跳 `/migration`；update 行跳 `/system`；vuln 行跳 `/settings`。
4. **后台完成不丢**：任务在后台完成（状态变为 `succeeded`）后，刷新列表仍能找到该任务行（历史保留）。
5. **通知中心状态跃迁推通知**：对比前后快照，`running→succeeded` 触发「已完成」通知，`running→failed` 触发「失败」通知。
6. **无任务时空状态**：列表为空时显示「暂无任务」提示，不崩溃。
7. **轮询时序（待真机）**：真浏览器中任务在后台完成，离开页面再回来仍能在列表找回；通知图标点击能展开任务列表。

## 6. 技术约束

- 无新依赖：全用 `@mantine/notifications`（已装）、`@tabler/icons-react`（已装）、`i18next`（已装）。
- i18n：新建 `taskCenter` 命名空间，仅 `zh-CN`。
- 轮询：任务中心页 3s，通知中心 5s；组件卸载立即 clearInterval。
- 状态管理：不引入 Redux/Zustand，纯 `useState`/`useRef`/`useEffect`。
