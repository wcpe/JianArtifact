# 功能规格：监控页重设计（跨域 KPI + 多指标时序网格）+ 审计拆出（FR-99）

> 状态：开发中　·　关联 PRD：FR-99　·　分支：feature/fr-99-monitor-redesign
>
> 本规格**取代**原「统一 tab 化监控页」设计（旧版把使用分析 / 审计 / 防护 tab 化塞进统一监控页，
> 用户不满意 → 重做）。原设计的 tab 整合、把三页折进 tab 的做法本规格不再采用。

## 1. 背景与目标

第四期（Web 控制台 UX 重构）epic 的一环（属 P2）。原 FR-99 把使用分析（FR-58）/ 审计（FR-77）/ 防护监控（FR-78）
tab 化塞进一个统一「监控」页，并把主机监控（FR-98）作为新 tab。该方案被推翻：tab 化只是把割裂的视图叠进一个壳，
没有提供「一眼看全运行状况」的跨域总览，且把审计这种高频独立查询场景埋进 tab 不便。

FR-105 已落地后端**时序采集 + 查询**能力（`GET /api/v1/monitor/metrics`，仅 Admin，按指标键 / 时间范围返时序点，
覆盖 10 个指标键）。本次重做利用它，把监控页变成**跨域 KPI 指标 + 多指标时序网格**的运行总览页，
并把审计**拆出**为独立路由 + 导航入口。

目标：
- 监控页重做为**跨域 KPI 行 + 多指标时序网格**（分类切换 + 时间范围切换），消费 FR-105 时序 API，悬停看某时间点取值。
- 审计从监控页移除，恢复 `AuditPage`（FR-77）为**独立路由 + 导航入口**（仅 Admin）。
- 使用分析（FR-58）/ 防护监控（FR-78）**可达性不回归**：恢复为各自独立导航入口 + 路由。
- 仅 Admin 可达；纯本机内部数据、不外发。

## 2. 需求（要什么）

### 范围内（本期做）

- **监控总览页**（路由 `/monitor`，仅 Admin）：
  - **顶部控制条**：分类切换（全部 / 主机 / 使用分析 / 防护 / 缓存 / 存储仓库）+ 时间范围切换（1h / 24h / 7d）。
  - **KPI 指标行**：跨域汇总卡片，取各指标在所选范围内的**最新值**（counter 类直接展示累计值），
    给出当前 CPU / 内存 / 磁盘使用率、累计访问 / 下载、活跃封禁、限流被拒、仓库数 / blob 数 / 存储用量等。
  - **多指标时序网格**：每个指标一张卡（标题 + 小**时序折线图** + 当前值），消费 `GET /monitor/metrics` 拿时序点，
    **悬停某点看该时刻 ts + value**。分类切换过滤展示哪些指标卡。
  - **时间范围**映射为 `from`/`to`（毫秒）查询参数；`step` 由前端按范围估算（如 1h→不降采样 / 7d→给一个桶宽）传给后端降采样。
  - 缓存命中率等**无数据指标**：优雅显示「暂无数据」占位卡，不报错（FR-105 本期未采缓存命中率，时序为空）。
- **审计独立页**：`AuditPage`（FR-77，自包含组件）恢复为独立路由 `/audit` + 导航入口「审计日志」（仅 Admin）。监控页不再含审计。
- **使用分析 / 防护监控独立入口**：恢复 `AnalyticsPage`（FR-58）路由 `/analytics` + 入口「使用分析」、
  `ProtectionMonitorPage`（FR-78）路由 `/protection-monitor` + 入口「防护监控」（均仅 Admin），可达性不回归。
- **手搓时序折线组件**（新建 `components/charts/LineChart.tsx`，纯 SVG，零依赖）：折线 + hover tooltip，
  空数据走空态；复用既有 `RingChart` / `BarList` 风格（CSS 变量适配主题）。
- **导航整合**：`AppLayout` 的 `NAV_ITEMS` 把单一「监控」入口拆为：监控（`/monitor` 总览）、使用分析、审计日志、防护监控，
  四个独立 Admin 入口；`isNavActive` 段精确匹配不变（`/protection` vs `/protection-monitor` 不串台，FR-92/fix-B 不回归）。

### 不做（范围外 / 本期降级）

- **不改后端**：FR-105 端点（`/monitor/metrics`）、FR-98（`/monitor/host`）均已在基线，按需只读消费，不动契约。
- **缓存命中率指标**：FR-105 本期未采（待埋点），监控页该指标卡显「暂无数据」，本期不补埋点。
- **counter 差分**：限流被拒 / 访问 / 下载是累计值；本期折线**直接展示累计曲线**，不在前端做逐段增量差分（保持简单，YAGNI）。
- **不引图表库**：折线 / KPI 卡均手搓 SVG/CSS，不引 `@mantine/charts` 等。
- **不碰** FR-93 浏览页 / FR-96 设置页 / 被复用三页（AnalyticsPage / AuditPage / ProtectionMonitorPage）的数据逻辑本身。
- **主机监控**：原 `HostMonitorPanel`（按请求快照）不再单列 tab；主机使用率改由时序网格的「主机」分类承载（消费 FR-105 主机指标键）。
  `HostMonitorPanel` 组件本期退役（不再被监控页引用），避免双份主机视图。

## 3. 设计（怎么做）

无新 ADR：纯前端重组 + 消费基线已有 FR-105 / FR-98 端点，未引入新技术 / 新模式 / 推翻旧决策。

### 3.1 指标元数据（前端常量，单一真源）

- 新增 `frontend/src/lib/metrics.ts`：集中定义 FR-105 的 10 个指标键、显示名、所属分类、值格式（百分比 / 字节 / 计数）。
  - 指标键常量对齐后端：`host.cpu_percent` / `host.memory_percent` / `host.disk_percent` /
    `storage.repo_count` / `storage.blob_count` / `storage.total_bytes` /
    `protection.active_bans` / `protection.rate_limited_total` / `usage.access_total` / `usage.download_total`。
  - 缓存命中率作为「已知但本期无数据源」的占位条目（分类=缓存），渲染时走空态。
  - 分类：`host` / `usage` / `protection` / `cache` / `storage`；外加「全部」聚合视图。
  - 值格式纯函数：百分比（`xx%`）、字节（复用 `formatBytes`）、计数（原值）。
- 时间范围常量：`1h` / `24h` / `7d` → 各自 `rangeMs` 与建议 `stepMs`（如 1h→0 不降采样、24h→5min 桶、7d→1h 桶）。

### 3.2 API 类型与端点封装

- `api/types.ts` 新增（对齐 FR-105 端点契约）：
  - `MetricPoint { ts: number; value: number }`。
  - `MetricSeries { metric: string; points: MetricPoint[] }`。
- `api/endpoints.ts` 新增 `getMetricSeries(metric, opts?: { from?; to?; step? }): Promise<MetricSeries>`
  → `GET /monitor/metrics?metric=&from=&to=&step=`（仅 Admin；缺省由后端补 from/to）。

### 3.3 手搓时序折线组件 `LineChart`

- `components/charts/LineChart.tsx`（纯 SVG，零依赖）：
  - 入参：`points: MetricPoint[]`、`emptyText`、可选 `valueFormat`（值→显示串）、可选 `height`。
  - 渲染：按 points 的 ts/value 归一到 viewBox，画 `polyline` 折线 + 末值点；空 points 走 `emptyText` 占位。
  - **hover tooltip**：鼠标移到某数据点（或最近点）显示该点 `ts`（本地时间）+ 格式化 value；用 SVG `<title>` 或受控 state 实现，
    保证测试可断言（点暴露 `aria-label` / `data-*` 承载该点取值，hover 时浮层文本可查询）。
  - 颜色经 CSS 变量（`--mantine-primary-color-filled` 等）适配主题，不引图表库。

### 3.4 监控总览页 `MonitorPage` 重写

- 顶部 `SegmentedControl`（分类）+ `SegmentedControl`（时间范围），受控 state。
- 选定分类 → 过滤出该分类的指标键集合（「全部」= 全集）。
- 对每个可见指标键并发 `getMetricSeries`（按当前时间范围的 from/to/step）；各自 loading / error 独立，单个失败不拖垮整页。
- **KPI 行**：对每个指标取其 series 末点 value（无点→「—」），按值格式渲染一组紧凑卡片（`SimpleGrid`）。
- **时序网格**：`SimpleGrid` 每指标一张 `Card`（标题 + `LineChart` + 当前值）；无数据指标卡显「暂无数据」。
- 切换分类 / 时间范围 → 重新取数。沿用 `density` 与 Mantine 风格；数据本机内部、不外发提示沿用既有文案基调。
- 移除对 `AnalyticsPage` / `AuditPage` / `ProtectionMonitorPage` / `HostMonitorPanel` 的引用（不再 tab 化）。

### 3.5 路由与导航

- `App.tsx`：在 Admin 守卫层新增 `/analytics`（AnalyticsPage）、`/audit`（AuditPage）、`/protection-monitor`（ProtectionMonitorPage）三条路由；
  `/monitor` 保留指向重写后的 `MonitorPage`。
- `AppLayout.tsx` `NAV_ITEMS`：把当前单一「监控」项扩为四项（均 `adminOnly`）——
  「监控」`/monitor`、「使用分析」`/analytics`、「审计日志」`/audit`、「防护监控」`/protection-monitor`，
  插在「防护配置」`/protection` 附近，保持现有顺序合理。`isNavActive` 不变。

## 4. 任务拆分

- [ ] `lib/metrics.ts`：指标键 / 显示名 / 分类 / 值格式 / 时间范围常量（+ 单测）
- [ ] `api/types.ts` 加 `MetricPoint` / `MetricSeries`；`api/endpoints.ts` 加 `getMetricSeries`
- [ ] `components/charts/LineChart.tsx` 手搓时序折线 + hover tooltip（+ 单测：折线渲染 / 空态 / hover 看点值）
- [ ] 测试先行：`MonitorPage` KPI 行渲染、分类切换过滤指标、时序网格消费 metrics API（mock）、折线 hover、无数据指标空态
- [ ] 重写 `MonitorPage`（KPI + 时序网格 + 分类 / 范围切换）
- [ ] `App.tsx` 加 `/analytics` `/audit` `/protection-monitor` 路由；`AppLayout` 导航扩为四项 + 改导航测试
- [ ] 文档同步：本 spec、PRD 状态（已 `开发中` 无需翻）、ARCHITECTURE（监控页结构一句）、CHANGELOG 末尾追加一行

## 5. 验收标准

- **KPI 行**：mock `getMetricSeries` 返回各指标时序后，KPI 行渲染各指标当前值（末点 value）；无点指标显「—」/「暂无数据」。
- **分类切换**：切到「主机」只展示 host.* 指标卡，切到「防护」只展示 protection.* 指标卡，「全部」展示全集；切换触发按需取数。
- **时间范围切换**：切 1h / 24h / 7d 改变传给 `getMetricSeries` 的 from/to/step（断言调用参数随范围变化）。
- **时序网格**：每个可见指标一张卡，含手搓 `LineChart`；mock 出多点时折线渲染、当前值展示。
- **折线 hover**：`LineChart` 对某数据点 hover 时可查询到该点 ts + value 文案；空 points 走空态文案。
- **无数据指标**：缓存命中率指标卡显「暂无数据」、不报错。
- **审计独立页**：`/audit` 路由渲染 `AuditPage`；导航有「审计日志」入口（仅 Admin）；监控页 DOM 不含审计内容。
- **使用分析 / 防护监控可达**：`/analytics`、`/protection-monitor` 路由可达并渲染对应页；导航各有入口（仅 Admin）。
- **导航不回归**：`/monitor` 时仅「监控」高亮、`/protection` vs `/protection-monitor` 不串台；FR-92 折叠 / 角色门控 / 段精确高亮不回归。
- **FR-58 / 77 / 78 不回归**：三页既有测试全绿（组件未改）。
- 验证门：`pnpm -C frontend test` + `pnpm -C frontend lint` + `pnpm -C frontend build` 全绿；恢复 `frontend/dist/.gitkeep`。

## 6. 风险 / 待定

- **缓存命中率无数据源**：FR-105 本期未采，监控页该卡常态空态；待后续 proxy / format 埋点后纳入（不在本期范围）。
- **counter 单调累计**：限流被拒 / 访问 / 下载折线为单调累计曲线（非每段增量），符合 FR-105「存累计、不在采样侧差分」语义；
  前端本期不做差分，照实展示累计。
- **首样缺数据**：后台采样需运行一段时间才有点；新部署 / 短运行时序可能为空，UI 走空态、不报错。
