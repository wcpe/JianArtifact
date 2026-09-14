# 功能规格：消息中心页面与页眉通知下拉打磨

> 状态：已交付@v0.8.0（消息中心页面已于本版整体退役）　·　关联 PRD：FR-117（原 FR-127 并入）、FR-118　·　目标版本：v0.8.0

## 1. 背景与目标

页眉风险通知中心（FR-117）目前每次打开都全量渲染最多 20 条预览且没有任何数据刷新机制：下拉列表滚动到底会带动整页滚动（滚动穿透）、无轮询也无页眉刷新联动、加载/失败/零未读三态难以区分。同时通知只覆盖「最近 24 小时未确认风险」，管理员没有任何入口可以看到超过 24 小时的历史风险批次（FR-117）。

本功能交付三件事：

- **FR-B（ref）**：审计页 `AuditLogView` 信息架构重排为「概览 / 记录」两个 Tab，纯重构，行为与数据口径完全不变。
- **FR-C（fix）**：页眉通知下拉修复滚动穿透、改为缩略展示、接入轮询与全局刷新、三态可区分，文案全部 i18n。
- **FR-117（feat）**：新增全量消息中心页面（仅展示 + 跳转，不含页内确认动作），支撑分页、未确认/已确认筛选与时间排序，可回看超过 24 小时的历史记录。

## 2. 需求

### 2.1 API 契约（扩展既有 endpoint，不新增路径）

取舍：**扩展 `GET /api/v1/observability/audit/notifications`**，而非新增 endpoint。理由：消息中心与页眉下拉消费同一份「风险批次摘要」读模型，扩展参数即可复用既有快照、风险归组、脱敏与排序实现；新增路径会复制一套几乎相同的契约面与实现，只减少一个可选参数的复杂度，得不偿失。

- `GET /api/v1/observability/audit/notifications`（仅全局管理员）新增可选查询参数：
  - `from` / `to`（复用 `AuditFromParam` / `AuditToParam`）：UTC 时间范围，必须同时提供或同时省略；省略时服务端固定最近 24 小时；范围最长 30 天（`maxAuditRange`）。
  - `status`（新增 `AuditNotificationStatusParam`）：`unacknowledged`（缺省）｜`acknowledged`｜`all`。`acknowledged` 表示批次内全部风险来源均已确认；`all` 不筛选确认状态。
  - `limit`（新增 `AuditNotificationLimitParam`）：1–100，缺省 20（与既有 20 条预览上限一致）。
  - `cursor`（复用 `AuditCursorParam`）：不透明分页游标，从上一页 `nextCursor` 取得，客户端不得解析。
- 响应体 `AuditAttentionNotificationList` 就地演进：
  - `items`：`AuditAttentionPreview[]`，`maxItems` 放宽到 100。条目复用批次安全摘要（含 `state: unacknowledged | acknowledged` 与可选 `firstAcknowledgement`）；原仅限 `unacknowledged` 的 `AuditUnacknowledgedAttentionPreview` 删除。
  - `total`（新增，必填）：与请求 `status` 筛选同口径的通知批次总数（分页权威总数）。
  - `totalUnacknowledged`（改为可选）：仅**缺省请求**（未携带任何新增参数）返回，等于该请求下的 `total`；语义不变（最近 24 小时未确认风险批次总数），供页眉徽标使用，保持既有客户端兼容。
  - `hasMore`（保留，必填）、`nextCursor`（新增，可选）：没有下一页时省略。
- 排序口径：
  - 缺省请求：沿用既有「失败优先 → 严重性 → 最新时间 → attentionId」排序（页眉预览语义不变）。
  - 显式请求（任一 `from`/`to`/`status`/`limit`/`cursor` 出现）：按 `latestOccurredAt` 倒序、`attentionId` 倒序打破平局（页面时间排序语义）。
- 错误码：`400 bad_request`（时间范围无效/超过 30 天、`status` 或 `limit`/`cursor` 非法）、`401`、`403`；聚合源事件超过 5,000 条上限时沿用 `409 audit_query_too_large`。脱敏边界与既有通知一致：不返回 IP、User-Agent、请求标识、来源节点、内部地址或任何凭据。

### 2.2 消息中心页面（FR-117，最小版）

- 路由 `/notifications`（对齐现有短横线路由命名习惯），页面组件懒加载；入口仅管理员可见，路由守卫同现有 admin 路由（`RequireAuth` + `RequireAdmin`），非管理员直链访问渲染 `ForbiddenState`。
- 信息架构：`PageHeader`（标题「消息中心」+ 描述）→ 状态筛选（全部 / 未确认 / 已确认）→ 通知批次列表。最小版固定读取最近 30 天（服务端上限内最宽窗口），不做自定义时间范围。
- 列表行为：游标分页「加载更多」；每条展示结果徽章、确认状态徽章（待确认 / 已确认处理）、操作与对象、脱敏摘要、批次起止时间与影响计数。已确认条目展示首个确认人快照与确认时间（只读）。
- 跳转：未确认条目点击进入 `/audit-logs?attentionId=…` 借助审计页风险抽屉完成确认（本页不做确认动作）；不做页内确认、撤销确认或批量操作。
- 页面行为对齐现有页面模式：加载 / 失败（重试）/ 空态、403 Forbidden 态；文案全部 i18n。

### 2.3 页眉通知下拉（FR-C）

1. **滚动穿透**：下拉列表改为固定高度上限的 `ScrollArea` 独立滚动容器，并设置 `overscroll-behavior: contain` 阻断滚动链，列表滚到底不再带动整页。
2. **缩略展示**：下拉只渲染最近 8 条（缺省请求本身即未确认优先排序）；底部保留「查看全部审计」（`/audit-logs`）并新增「查看全部消息」（`/notifications`）；空态文案下同样提供「查看全部消息」入口。
3. **数据新鲜度**：60 秒轮询（组件卸载清理）+ 接入页眉刷新按钮派发的 `REFRESH_EVENT`（`useAsync` 已内置监听，轮询经 `reload()` 触发）；红点计数随每次拉取及时更新。
4. **三态可区分**：加载中 = 铃铛转圈（`loading` 态）；失败 = 铃铛异常态（警示图标 + 灰点徽标），且下拉内文案明确「读取失败」，不伪装为零未读；零未读 = 隐藏红点仅剩铃铛。文案（含无障碍名称）全部接入 i18n，删除硬编码中文。

### 2.4 审计页 Tab 化（FR-B，纯重构）

- `AuditLogView` 由单 `Stack` 纵向堆叠改为 Mantine `Tabs`：「概览」（4 张指标卡 + 趋势图）/「记录」（筛选控件 + 聚合/完整记录切换 + 列表 + 无限 `loadMore`），默认落在「概览」。
- 数据获取、快照、聚合键、确认流程、`attention_stale` 黄条、加载/失败/空态门槛（gates）完全不变；所有现有测试语义保持通过（交互入口按 Tab 调整）。
- 从通知跳转携带 `?attentionId=` 时自动落在「记录」Tab 并照旧打开风险批次抽屉定位目标批次；确认/失效路径不变。

范围内：上述契约扩展、消息中心页、页眉下拉四项修复、审计页 Tab 化、devmock 契约同步。

不做：页内确认动作、消息已读/删除、WebSocket/推送、跨节点通知、通知保留策略清理、自定义时间范围 UI。

## 3. 设计

- **后端**：`audit_observability_handlers.go` 的 `ListAuditAttentionNotifications` 接收生成参数结构，时间窗复用 `auditFilter` 解析（缺省 24h、上限 30 天），风险归组复用 `auditGroups` + `Acknowledgements`，按 `status` 过滤批次后排序、以 `cursorOffset` 做内存分页（源事件已有 5,000 条硬上限）。`totalUnacknowledged` 仅缺省请求计算返回。
- **前端**：`endpoints.ts` 的 `getAuditAttentionNotifications` 增加可选查询参数并新增 `listNotificationCenter` 语义入口；`AuditNotificationCenter` 重写（ScrollArea + 缩略 8 条 + 轮询 + 三态 + i18n）；新页面 `NotificationCenterPage`（`PageHeader` + 筛选 + 游标分页列表）；`router.tsx` 注册 `/notifications`；`AppLayout` 不变（下拉入口已在页眉）。i18n 在 `zh.ts` 新增 `notifications` 段（本仓库 i18n 当前仅内置中文资源 `zh.ts`，无 `en.ts`，键位为多语言扩展预留）。
- **devmock**：`packages/devmock/src/msw.ts` 通知 handler 解析新查询参数，`observability.ts` store 增加 `notifications(query)` 分页/筛选实现与 `emptyNotifications(query)` 空态；`loading` / `error` 场景由既有 `interceptDevMockScenario` 全局拦截覆盖；重新生成 `schema.gen.ts`。说明：`apps/web/src/mocks/observabilityPreview.ts` 是旧版固定预览夹具（仅被遗留 `AuditLogPreview` 组件引用，不在通知中心与审计页的真实 MSW 数据链路上），全量列表的正常/空/加载/失败态按项目 DevMock 状态契约落在 `packages/devmock`（store + MSW + 场景拦截），不在该夹具文件中复制一份死数据。
- **契约流**：改 `api/openapi.yaml` → `cd apps/server && task gen`（重生成 `api.gen.go`）+ `packages/devmock` 的 `pnpm gen`（重生成 `schema.gen.ts`，前端 `api/types.ts` 经 devmock schema 复用自动获得新类型）。

## 4. 任务拆分

- [x] 撰写本规格，明确契约扩展取舍与三端行为。
- [x] 扩展 `api/openapi.yaml`（notifications 参数与响应 schema）并重生成 Go 接口与 devmock schema。
- [x] 后端 handler 扩展 + handler/httpserver 测试（默认请求兼容、status 筛选、分页、排序口径、参数校验）。
- [x] FR-B：`AuditLogView` Tab 化重构，`?attentionId=` 落「记录」Tab，现有测试对齐新 IA 后全绿。
- [x] FR-C：`AuditNotificationCenter` 滚动容器、缩略 8 条、60s 轮询 + `REFRESH_EVENT`、三态、i18n。
- [x] FR-117：`NotificationCenterPage` + `/notifications` 路由（admin 守卫）+ 页眉「查看全部消息」入口。
- [x] devmock：通知 handler/store 支持筛选分页，契约测试补全量列表正常/空/加载/失败态。
- [x] 文档同步：PRD、ARCHITECTURE、API、CHANGELOG 由协调者统一处理。

## 5. 验收标准

- **契约**：`task gen` 与 devmock `pnpm gen` 后无手工改动；缺省无参调用 `GET /notifications` 的响应与旧契约字段语义一致（items 全部 `unacknowledged`、≤20 条、`totalUnacknowledged` = `total`、`hasMore` 正确）。
- **后端测试**：默认请求兼容（确认后清零）、`status=acknowledged` 只含已确认批次、`status=all` 总数与两态之和一致、`limit`/`cursor` 分页连续且 `nextCursor` 终止、缺省失败优先与显式请求时间倒序两种排序、非法 `from`/`to`/`status`/`limit` 返回 400、非管理员 403。
- **前端（FR-B）**：默认渲染「概览」Tab（指标卡 + 趋势）；「记录」Tab 首屏可达且筛选/聚合/完整记录/loadMore 行为与重构前一致；`?attentionId=` 进入「记录」Tab 并打开风险抽屉；`attention_stale` 黄条与确认后刷新路径不变。
- **前端（FR-C）**：下拉列表在固定高度容器内滚动且 `overscroll-behavior: contain`；最多渲染 8 条；底部「查看全部消息」跳 `/notifications`，空态仍提供该入口；60s 轮询触发重新拉取且 `REFRESH_EVENT` 立即刷新；加载/失败/零未读三态视觉可区分；无硬编码中文文案。
- **前端（FR-117）**：`/notifications` 仅管理员可达（普通用户 403 态）；筛选全部/未确认/已确认正确；「加载更多」按 `nextCursor` 续拉直至 `hasMore=false`；条目展示确认状态与时间排序；未确认条目可跳转审计页 `attentionId`。
- **验证门**：`cd apps/server && task test` 全绿；`cd apps/web && pnpm typecheck && pnpm test && pnpm lint` 全绿；devmock 契约测试覆盖全量列表正常/空/加载/失败态。

## 6. 风险 / 待定

- 30 天窗口下源事件可能触达 5,000 条聚合上限，此时消息中心与页眉下拉按 `409 audit_query_too_large` 呈现失败态（页眉不伪装为零未读）；缩小窗口或等待历史归档是既有约束，不在本期扩容。
- `totalUnacknowledged` 由必填改为可选属契约演进；本仓库唯一消费方（页眉 + httpserver 测试）在同次变更内对齐，旧版独立客户端若存在需升级。
- 页眉轮询固定 60s、不感知页面可见性；多标签页会各自轮询，频率可接受，后续可按 `document.visibilityState` 优化（不做项）。
- 本仓库 i18n 仅内置中文资源（`apps/web/src/i18n/zh.ts`），无 `en.ts`；键位按多语言扩展预留，双语资源待多语言期补齐。
