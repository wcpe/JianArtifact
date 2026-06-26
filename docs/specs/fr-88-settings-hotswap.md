# 功能规格：设置可编辑与运行时热替换

> 状态：开发中　·　关联 PRD：FR-88　·　分支：feature/fr-88-settings-hotswap

## 1. 背景与目标

网络代理 `[network.proxy]`（FR-84，ADR-0020）与在线更新 `[update]`（FR-85，ADR-0021）配置过去只能改 TOML 并**重启进程**才能调整；设置页（FR-87）也只读展示。受限网络下运维换代理、临时开 / 关在线更新都得重启，不便。

FR-88 仿 FR-79 防护配置热替换（ADR-0018）机制，把这两类配置改为**经网页 PATCH 即时生效、无须重启**：设置页从只读改**可编辑**（仅 Admin），凭据仍不回显。属 P2。

## 2. 需求（要什么）

- 范围内：
  - 新增进程内**运行时设置热替换槽**，随 `AppState` 共享，收拢两块可运行时调整的配置：
    - 出站代理 `NetworkProxyConfig` + 据其构造的 `reqwest::Client`（热替换的关键：换代理须重建 client）。
    - 在线更新可运行时调的字段：`enabled` / `repo` / `api_base_url` / `restart_mode` / `token`。
  - 把 FR-84 当前在启动期被 5 处构造并**持有**的出站 client，改为各出站点**按需从槽取当前 client**（读锁内 clone 一个 `reqwest::Client` 立即放锁、锁外用）；PATCH 改代理后**锁外重建 client**、再短持写锁换快照 → 下个出站请求即用新代理。
  - 在线更新端点（check / apply）改为读热槽的 `enabled` 等字段，PATCH 可翻 `enabled`。
  - 新增 `PATCH /api/v1/settings`（仅 Admin）：校验后重建并换槽、即时生效；`GET /api/v1/settings`（FR-87 已有）改为回显**当前热值**。
  - 凭据：PATCH 可接受含 `user:pass@` 的代理 URL 与 update token，但**只存内存热槽、不写回 TOML、不入 DB**（重启回落文件 / env 配置，与 FR-79 一致）；GET 仍脱敏（代理去凭据、token 只回 `has_token`）。
  - 前端设置页从只读改可编辑（代理 http / https / no_proxy 表单 + 在线更新 enabled / repo / api_base_url / restart_mode + 可选填 token），保存调 PATCH。
- 不做（范围外）：
  - 其余配置子树（server / data / auth / limits / proxy / observability / vuln / protection）仍只能改 TOML + 重启；本 FR 不动它们的读取点（protection 已由 FR-79 热替换）。
  - 不把运行时改动持久化写回 TOML（守「配置真源是文件 + env」；重启回落文件配置）。
  - 不引入新依赖（用 std `RwLock<Arc<..>>`，与 ADR-0018 一致）。

## 3. 设计（怎么做）

详见 **ADR-0022**（运行时可编辑设置与热替换，取代 ADR-0020「代理只读 / 运行时不热替换」取向）。要点：

- 在 `config` 层新增 `NetworkState`（`RwLock<Arc<NetworkSnapshot>>`）：`NetworkSnapshot` 含当前 `NetworkProxyConfig` + 据其 `build_outbound_client` 构造的 `reqwest::Client` 与构造该 client 用的超时。`config` 是所有出站模块的共同最底层，放这里不产生反向跨层依赖（守分层不变量）。
  - `client()`：读锁内 clone `reqwest::Client`（内部 `Arc`，clone 廉价）立即放锁，调用方锁外用。
  - `replace_proxy(cfg)`：**锁外** `build_outbound_client` 重建 client（失败不换、返回错误），再短持写锁原子换快照；写临界区只做一次指针赋值。
- 把 5 处出站 client 持有者（`HttpUpstream` / `HttpNexusClient` / `HttpMirrorSource` / `OidcProvider`，及 main.rs 装配）改为持 `Arc<NetworkState>`，出站时 `network.client()` 取当前 client。
- 在线更新可调字段收进 `EditableUpdate`（同一槽并列，`RwLock<Arc<EditableUpdate>>` 或合进设置槽）：`build_source` / check / apply 改读热槽。token 仅入内存槽、不回显、不入库。
- `EditableSettings` 槽（统一封装上述两块）随 `AppState` 加一个字段 `settings: Arc<EditableSettings>`；启动期由文件 / env 配置装载初值。
- `PATCH /api/v1/settings` 薄 handler：仅 Admin → 校验（代理 URL 可构造、restart_mode 合法）→ 锁外重建 → 换槽 → 回显脱敏后的当前热值。校验失败 400 且**不改**现有生效值。
- `GET /api/v1/settings` 改为读热槽当前值组装脱敏 DTO（不再读 `state.config`）。

## 4. 任务拆分

- [ ] 写 ADR-0022 + ADR-0020 标「已被 ADR-0022 取代」+ ARCHITECTURE §7 索引加 0022
- [ ] `config`：新增 `EditableSettings`（`NetworkState` + `EditableUpdate`）热替换槽 + 校验
- [ ] 5 处出站点改为持 `Arc<NetworkState>`、按需取 client
- [ ] 在线更新 check / apply 改读热槽 enabled 等字段
- [ ] `AppState` 加 `settings` 字段 + 补齐所有构造点（main.rs + api helper + tests/*）
- [ ] 测试先行：PATCH 代理后出站走新 client；PATCH enabled=true 后 check 不再 409；非 Admin 403；凭据不回显；非法配置 400 不改生效值
- [ ] `PATCH /api/v1/settings` + `GET` 改读热值
- [ ] 前端设置页改可编辑 + types / endpoints 加 PATCH + 测试
- [ ] 文档同步：PRD 状态、ARCHITECTURE、API、CONFIG、OPERATIONS、CHANGELOG

## 5. 验收标准

- `PATCH /api/v1/settings` 改代理后，出站客户端经热槽即用新代理（单测：替换后 `NetworkState::client` 反映新构造、旧持有快照不受影响；语义同 ADR-0018 并发自洽性测试）。
- `PATCH` 把 `update.enabled` 从 false 翻 true 后，`GET /api/v1/update/check` 不再返回 409（改走出站；测试可用 fake API base 或断言不再因 Disabled 而 409）。
- 非 Admin / 匿名 `PATCH /api/v1/settings` → 403 / 401。
- 非法配置（代理 URL 无法构造 / restart_mode 非法）→ 400，且现有生效值不变（GET 仍回旧值）。
- 凭据脱敏：PATCH 传含 `user:pass@` 代理与 token 后，GET 响应不含任何凭据明文（代理去 userinfo、token 只回 `has_token`）。
- 凭据不入库、不进日志、不写回 TOML：重启回落文件 / env 配置。
- `rustup run 1.96.0 cargo fmt --all --check` + `clippy --all-targets -D warnings` + `cargo test` 全绿；前端 `pnpm -C frontend test` + `build` 过。
- **真机维度**（需用户确认）：实际网页改代理后出站请求经新代理生效；改 enabled 后 check 走出站。单测 / e2e 不替代该项。

## 6. 风险 / 待定

- OIDC / vuln / migrate 三处出站点 client 持有方式不同（启动期长持 vs 请求级），改为按需取 client 后须保证热替换期间出站不中断、后台任务下次迭代自动用新 client。
- `reqwest::Client` clone 廉价（内部 `Arc`），热路径取 client 开销可忽略；但每次出站 `client()` 多一次读锁 + clone，相对原「持有 client 直接用」略有开销，可接受（读多写极少、无写争用）。
