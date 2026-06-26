# 功能规格：主机 / 系统监控采集（FR-98）

> 状态：开发中　·　关联 PRD：FR-98　·　分支：feature/fr-98-host-monitor

## 1. 背景与目标

运维需要在控制台直观看到运行制品库的**这台主机**当前的基础资源状况（CPU 用了多少、内存与磁盘的已用 / 总量、进程已运行多久），以判断容量与健康。本期（P2）只做**后端采集 + 单个仅 Admin 查询端点**；监控页（FR-99，B 类 UX）不在本次范围。

主机指标是本机内部运行数据，按 ADR-0009 / ADR-0015 的隐私基调：**默认不主动外发、不向外部遥测 phone-home**；本端点纯本地采样查询，不接任何外部上报 / 导出。

## 2. 需求（要什么）

- 经 `sysinfo` 跨平台采集基础主机指标：
  - CPU：全局使用率百分比 + 逻辑核数。
  - 内存：总量 / 已用（字节）。
  - 交换分区：总量 / 已用（字节）。
  - 磁盘：每块磁盘的挂载点 / 总量 / 可用（字节），以及总量 / 可用汇总。
  - 运行时长：系统 uptime（秒）。
- 经 `GET /api/v1/monitor/host` 返回上述结构化指标，**仅 Admin**：未认证 401、非管理员 403、Admin 200。
- 范围内：按请求采样（sysinfo 单次 refresh 后读数）；把 sysinfo 读数 → DTO 的映射抽成可测纯函数。
- 不做（范围外）：
  - 后台持续轮询 / 历史时序留存（简单优先，仅按请求采样）。
  - 监控前端页面（FR-99）。
  - 进程级 / 网络 / 温度传感器等指标（裁掉 sysinfo 的 `network` / `component` / `user` features）。
  - 任何外部上报 / 导出 / 告警。

## 3. 设计（怎么做）

- **新增依赖 `sysinfo`（已获用户授权，见 ADR-0023）**：`Cargo.toml` 钉版本 `0.39.5`、`default-features = false`、仅启用 `["system", "disk"]`（覆盖 CPU / 内存 / 交换 / uptime / 磁盘），剔除 `component` / `network` / `user` 控体积。
- **新增 `monitor` 模块**（`src/monitor/`）：
  - `采集(&mut sysinfo::System, &sysinfo::Disks) -> HostMetrics`：刷新并读取，组装领域 DTO。读 sysinfo 的 IO 副作用与「读数 → DTO」分离：纯映射部分（如把字节、磁盘列表映射成 DTO、汇总磁盘总量 / 可用）做成无副作用纯函数，便于穷举单测。
  - `HostMetrics` 及其子结构（CpuMetrics / MemoryMetrics / DiskMetrics）实现 `serde::Serialize`，作为对外 DTO。
- **`api` 层薄 handler**（`src/api/monitor.rs`）：`monitor_host`，`identity.require_admin()?` 后取 `AppState` 中共享的 `Mutex<sysinfo::System>`，在锁内 `refresh` + 读数（sysinfo refresh 需 `&mut`），调用 `monitor::采集` 得 DTO 并 `Json` 返回。handler 不写采集逻辑、不算汇总。
- **`AppState` 注入**：新增字段 `host_system: Arc<Mutex<sysinfo::System>>`（refresh 需 `&mut`，单进程共享一份避免每请求重建）。磁盘列表（`sysinfo::Disks`）按请求新建刷新（其 `refresh` 取当前挂载，量小）。
- **路由**：`/api/v1/monitor/host` 挂到 `api_v1` 子路由（GET）。
- **CPU 首样取舍**：sysinfo 的 CPU 使用率需两次采样间隔才有非零值。本期按请求单次 refresh，**首次 / 间隔过近的采样 CPU 使用率可能为 0**，属已知取舍（不为它引后台轮询）。在 spec 与 ADR 说明，DTO 字段照常返回（0 是合法值）。
- 不新增配置项（端点恒在、仅 Admin、不需开关）；不落库、不入审计（GET 读取类，符合 FR-97 「读取类不入审计」）。

涉及架构决策（引入 sysinfo + 主机监控能力）另见 ADR-0023，本文不重复其决策正文。

## 4. 任务拆分

- [x] 复制 `_template.md` → 本规格；PRD FR-98 状态 计划→开发中（仅该行）。
- [x] 写 ADR-0023（主机 / 系统监控经 sysinfo 采集、仅 Admin、本机不外发、扩展 ADR-0015、为何引 sysinfo）；ARCHITECTURE §7 索引加一行。
- [x] `Cargo.toml` 引入 sysinfo（钉版本、裁 features）；`Cargo.lock` 同步。
- [x] 测试先行：鉴权矩阵（匿名 401 / User 403 / Admin 200 且结构含 cpu/mem/disk）+ 纯映射函数单测（内存 total>0、磁盘汇总正确等）。
- [x] 实现 `monitor` 采集模块 + `api/monitor.rs` 薄 handler + 路由 + AppState 注入。
- [x] 文档同步：PRD 状态、ARCHITECTURE（模块 + 机制）、API.md（端点）、CHANGELOG。

## 5. 验收标准

- `GET /api/v1/monitor/host`：匿名 401、普通 User 403、Admin 200。
- Admin 200 响应体含 `cpu` / `memory` / `disk`（或 `disks`）字段；内存 `total` > 0、CPU 逻辑核数 ≥ 1（合理范围，不断言精确值）。
- 采集纯映射函数有单测，覆盖磁盘汇总、字节透传等。
- `rustup run 1.96.0` fmt + clippy 全清；`cargo test --jobs 4` 全绿。
- 守 `#![forbid(unsafe_code)]`（sysinfo 内部 unsafe 不影响本 crate 的 forbid，仅约束本仓库代码）。

## 6. 风险 / 待定

- **CPU 使用率首样为 0**：单次 refresh 取舍，已在 §3 / ADR-0023 说明；不引后台轮询。
- **磁盘枚举跨平台差异**：不同 OS 下挂载点 / 磁盘数不同，DTO 以列表 + 汇总形式返回，不假设固定盘符。
- sysinfo 采样在持锁期间做（refresh + 读数，纯内存 + 系统调用，开销小）；不在锁内做网络 / blob IO，符合「锁外做重 IO」。单进程单次采样耗时可接受，不引后台任务。
