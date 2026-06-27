# 功能规格：统一指标时序采集与查询（FR-105）

> 状态：开发中　·　关联 PRD：FR-105　·　分支：feature/fr-105-metrics-ts

## 1. 背景与目标

FR-98（ADR-0023）只提供主机指标的**按请求实时快照**，明确「不后台轮询、不落库、不留历史时序」。
监控页重设计（FR-99）需要**多指标时序网格**——展示一段时间内各域指标的变化曲线，悬停看某时间点取值。
当下快照无法满足，需要一套统一的**时序采集 + 落库 + 查询**能力。

本功能（P2）新增：后台定时按可配间隔采样所选各域 gauge 落 SQLite + 按可配保留期滚动清理旧样本 +
仅 Admin 的时序查询 API（按指标键 / 时间范围 / 降采样步长查询）。这取代 ADR-0023「不留时序」的决策
（写新 ADR-0027 取代，旧 ADR-0023 标「被 ADR-0027 取代」）；**FR-98 的实时快照端点保留不动**，
时序是其补充而非替代。

数据是本机内部运行数据：落本地 SQLite、**默认不主动外发、不向外部遥测 phone-home**（守 ADR-0009 / 0015 基调）。

## 2. 需求（要什么）

### 范围内（本期做）

- **统一时序表**：经 `meta` 落一张通用时序表 `metric_samples(metric_key, ts, value)`；其他模块不绕过 meta 直连 DB。
- **后台采样任务**：`tokio` 定时任务按可配间隔采样一组 gauge 落库。采样间隔走配置、不硬编码。
- **保留期滚动清理**：后台任务按可配保留天数删除更早样本；另设行数硬上限兜底（沿用 audit / usage 范式），防止撑爆 SQLite。
- **查询 API**：`GET /api/v1/monitor/metrics`（仅 Admin），按 `metric` / `from` / `to` / `step` 返回时序点。
- **降采样 / 聚合纯函数**：把「原始样本按 step 桶聚合」抽成无副作用纯函数，便于穷举单测。
- **采集的指标（本期做能低成本拿到的）**：
  - 主机：CPU 使用率%、内存使用率%、磁盘使用率%（复用 FR-98 `monitor::collect` 的 sysinfo 读数，新增「字节 → 百分比」纯函数）。
  - 存储 / 仓库：仓库总数、blob（去重 sha256）总数、存储总字节（经 meta 新增轻量 COUNT/SUM 查询）。
  - 防护：当前活跃封禁 IP 数（复用 `ban_registry.active_ban_count`）、限流累计被拒数（复用 `rate_limiter.rejected_count`，counter 落点存为累计值，曲线由前端差分或直接展示累计）。
  - 使用分析：累计访问量、累计下载量（复用 `usage_total_by_action`，counter 累计值）。

### 不做（范围外 / 本期降级）

- **缓存命中率（降级，待埋点）**：proxy 模块当前**无**缓存 hit/miss 计数器（仅 `metrics_keys` 里有 Prometheus 占位键，未实现）。要采它须在 proxy + 各 format handler 的 GET 热路径新埋一套命中 / 未命中计数 —— 代价显著、且属另一条埋点改造线。**本期不做**，在采样集合里不含缓存命中率指标；待后续单独埋点后再纳入（YAGNI，不为它撑大本期范围）。
- **富数据面板 UI / 监控页前端**：归 FR-99，本期只提供查询 API（后端时序能力），不做前端图表。
- **外部导出 / 上报 / phone-home**：本期不做任何外部导出开关，守数据不外发。
- **CC / WAF / 慢速攻击维度时序**：当前这些维度未落地可读计数（仅占位），不纳入本期采样集合。
- **per-repo / 带标签的多维时序**：本期只做「单指标键 → 标量值」的扁平时序，不做高基数标签维度（避免量级失控）。

## 3. 设计（怎么做）

涉及架构决策（推翻 ADR-0023「不留时序」、新增时序存储与后台采样任务）→ 写 **ADR-0027** 取代 ADR-0023，
本节只描述落地，不重复决策正文。

### 3.1 数据模型（meta）

- 新增迁移 `migrations/0011_metric_samples.sql`：

  ```sql
  CREATE TABLE metric_samples (
      id         INTEGER PRIMARY KEY AUTOINCREMENT,
      metric_key TEXT    NOT NULL,   -- 指标键，如 host.cpu_percent
      ts         INTEGER NOT NULL,   -- 采样时刻（Unix 毫秒，UTC）
      value      REAL    NOT NULL    -- 标量取值
  );
  CREATE INDEX idx_metric_samples_key_ts ON metric_samples (metric_key, ts);
  ```

- 新增 `src/meta/metrics.rs`（与 usage.rs 同属元数据访问层，在 `MetaStore` 上扩展）：
  - `MetricSample { metric_key, ts, value }`、`NewMetricSample { metric_key, ts, value }`。
  - `insert_metric_samples(&[NewMetricSample])`：一批落同一事务（沿用 usage 批量范式）。
  - `query_metric_samples(metric_key, from, to)`：按键 + 时间范围取原始样本，按 ts 升序。
  - `prune_metric_samples_by_age(retention_days)` / `prune_metric_samples_by_max_rows(max_rows)`：保留期 + 行数兜底（沿用 usage pruner 范式）。
  - 存储 / 仓库计数辅助：`count_repositories()`、`count_distinct_blobs()`（`COUNT(DISTINCT sha256)`）、`total_blob_bytes()`（去重 sha256 后 `SUM(size)`，避免重复引用重复计字节）。
- 在 `src/meta/mod.rs` 加 `mod metrics;` 与 `pub use metrics::{MetricSample, NewMetricSample};`。

### 3.2 采样与清理后台任务（api / 新模块）

- 新增 `src/api/metrics_sampler.rs`（或 `src/monitor` 内时序子模块），提供：
  - `spawn_metrics_sampler(...)`：`tokio::time::interval(采样间隔)` 循环，每拍采样一组 gauge 组装 `Vec<NewMetricSample>`（同一 ts），经 `meta.insert_metric_samples` 落库；失败只 WARN，不影响业务。
  - `spawn_metrics_retention(meta, retention_days, max_rows)`：周期清理（沿用 `spawn_audit_retention` 范式，固定清理周期常量）。
  - 采样所需的句柄（`Arc<Mutex<System>>`、`rate_limiter`、`ban_registry`、`meta`）从 `main.rs` 注入。
  - 锁外 IO：采样时主机 refresh 在其自身 `Mutex` 内（纯内存 + 系统调用），DB 写在 meta（锁外），不在持锁期间做重 IO。
- 指标键集合用常量定义（不散落魔法串）：`host.cpu_percent` / `host.memory_percent` / `host.disk_percent` /
  `storage.repo_count` / `storage.blob_count` / `storage.total_bytes` / `protection.active_bans` /
  `protection.rate_limited_total` / `usage.access_total` / `usage.download_total`。
- 纯函数：
  - `host_percentages(&HostMetrics) -> (cpu, mem, disk)`：内存 / 磁盘「已用 / 总量 × 100」，除零保护（总量 0 → 0%）。
  - `downsample(samples, from, to, step) -> Vec<TsPoint>`：按 step 毫秒分桶，桶内取平均（或末值），无副作用、可穷举测。

### 3.3 配置（config）

- `[observability]` 下新增 `[observability.metrics_timeseries]`（`MetricsTimeseriesConfig`）：
  - `enabled: bool`（默认 true）。
  - `sample_interval_secs: u64`（默认 60）。
  - `retention_days: u32`（默认 7）。
  - `max_rows: u64`（默认 1_000_000，兜底）。
  - 默认值用文件顶部常量定义；env 覆盖经现有 `JIANARTIFACT_OBSERVABILITY_METRICS_TIMESERIES_*` 机制。
- 该配置承载本机内部时序，结构不含任何外部导出 / 上报开关。

### 3.4 查询端点（api，薄 handler）

- `src/api/metrics_query.rs`：`GET /api/v1/monitor/metrics?metric=<key>&from=<ms>&to=<ms>&step=<ms>`。
  - `identity.require_admin()?`；缺省 `from`/`to` 给合理默认（如最近 1 小时），`step` 缺省由范围估算或给默认。
  - 调 `meta.query_metric_samples` 取原始样本 → `downsample` 纯函数聚合 → 返回 `{ metric, points: [{ts, value}] }`。
  - handler 不写聚合逻辑（下沉纯函数），不做业务判断。
  - 在 `src/api/mod.rs` 路由表加 `.route("/monitor/metrics", get(metrics_query::query_metrics))`。
- `main.rs`：在现有后台任务启动区（audit / usage 之后）按 `enabled` spawn 采样与清理任务，注入句柄；启动日志中文、注明「本机内部、不外发」。

## 4. 任务拆分

- [ ] 迁移 `0011_metric_samples.sql` + `src/meta/metrics.rs`（写 / 查 / 清理 / 存储计数）+ mod.rs 导出
- [ ] 配置 `MetricsTimeseriesConfig` + 默认常量 + KNOWN_SECTIONS 校验
- [ ] 采样 / 清理后台任务 + 指标键常量 + 纯函数（host_percentages / downsample）
- [ ] 查询端点 `GET /api/v1/monitor/metrics`（Admin-only）+ 路由注册 + main.rs spawn
- [ ] 测试：采样落库、保留期清理、行数兜底、按范围查询、downsample 纯函数、端点 Admin-only、集成（缩短间隔/可控时钟累积）
- [ ] ADR-0027（取代 ADR-0023）+ adr/README 加 0023/0027 行
- [ ] 文档同步：PRD 状态、ARCHITECTURE（metrics 时序模块 + 机制）、API.md（查询端点）、CHANGELOG

## 5. 验收标准

- `meta.insert_metric_samples` 写入后 `query_metric_samples` 能按键 + 范围读回；样本按 ts 升序。
- `prune_metric_samples_by_age` 删除早于保留期的样本、保留期内样本不动；`prune_metric_samples_by_max_rows` 超限删最旧、回落上限内。
- `host_percentages`：总量 0 不 panic（返回 0%）、正常读数百分比在 0~100。
- `downsample`：空输入返回空；按 step 分桶聚合正确；穷举边界（单桶、跨桶、范围外样本被排除）。
- 查询端点：匿名 401、普通用户 403、Admin 200 且返回时序点结构。
- **集成（自动化）维度**：用缩短间隔 / 可控时钟的集成测试，断言后台采样随时间累积出多条样本、保留期清理只删旧样本。
  **长时段真机（默认 60s 间隔 / 7 天保留期下的实际滚动）须用户确认通过**——本地集成测试以缩短参数覆盖等价行为，长时段真机另行复验。
- fmt --check + clippy --all-targets -D warnings + 受影响组件 test 全绿。

## 6. 风险 / 待定

- **缓存命中率降级**：本期不采，监控页该项暂缺数据源；待后续在 proxy / format 埋点后单独纳入（已在 §2 范围外标注）。
- **counter 类指标语义**：限流被拒 / 使用访问下载是**累计值**，存为 gauge 时曲线是单调累计；前端如需「每段增量」由查询侧或前端差分（本期存累计，不在采样侧做差分，保持采样无状态）。
- **量级控制**：10 个指标键 × （默认 60s 间隔）× 7 天保留 ≈ 每键约 1 万条、合计约 10 万条，配 `max_rows` 兜底，量级可控、不撑爆 SQLite。
