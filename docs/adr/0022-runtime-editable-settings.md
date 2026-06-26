# ADR-0022：运行时可编辑设置与出站客户端热替换

> 本 ADR **取代 ADR-0020**（统一出站网络代理与共享出站客户端）：沿用其「`[network.proxy]` 为出站代理唯一真源、`config` 层统一 `build_outbound_client` helper、rustls-only、凭据脱敏」诸决策，但**推翻 ADR-0020 后果中「代理配置在启动期装载、运行时不热替换」一条**，改为运行时经网页 PATCH 可编辑 + 热替换。在线更新（ADR-0021）的运行时可调字段一并纳入同一热替换槽（ADR-0021 其余裁定不变）。

## 状态

已接受

## 背景

ADR-0020 把出站代理 `[network.proxy]` 收敛为唯一真源、`config` 层抽 `build_outbound_client` 统一注入全部出站 reqwest 客户端，并明确「代理配置在启动期装载、运行时不热替换」。其落地形态是：5 处出站点（proxy 回源 `proxy::HttpUpstream`、Nexus 迁移 `migrate::HttpNexusClient`、漏洞库镜像 `vuln::HttpMirrorSource`、OIDC `auth::OidcProvider` 经 `main` 装配、在线更新 `update::GithubReleaseSource`）在构造时各自**持有**一份据启动期配置构造好的 `reqwest::Client`，之后不再变化。在线更新 `[update]`（ADR-0021）的 `enabled` / `repo` / `api_base_url` / `restart_mode` / `token` 同样在启动期固化进 `Arc<Config>`。

FR-88 要求把网络代理与在线更新开关等改为**经网页 PATCH 即时生效、无须重启**：受限网络下运维换正向代理、临时开 / 关在线更新都不应依赖重启。约束（架构不变量）：进程内、无外部 DB / MQ / Redis；`#![forbid(unsafe_code)]`；**用 std 实现**（不引入 arc-swap 等外部依赖，与 ADR-0018 一致）；锁外做 IO（重建 client 含 TLS / 代理初始化属高开销，须在锁外）、临界区只护内存态、短持有；凭据（含 `user:pass@` 代理与 update token）不入库、不进日志、不进 DB 明文、不回显。

## 决策

新增一个进程内**运行时设置热替换槽** `EditableSettings`，随 `AppState` 经 `Arc` 共享，收拢两块可运行时调整的配置：

1. **出站网络槽 `NetworkState`**（`RwLock<Arc<NetworkSnapshot>>`，落在 `config` 层）：
   - `NetworkSnapshot` 收拢「当前生效的 `NetworkProxyConfig` + 据其经 `build_outbound_client` 构造的 `reqwest::Client` + 构造该 client 用的出站超时」为不可变、整体替换的快照。
   - 读：`client()` 读锁内 clone 一个 `reqwest::Client`（其内部为 `Arc`，clone 廉价、仅引用计数 +1）立即放锁，调用方在锁外发起出站请求。
   - 写：`replace_proxy(cfg)` 先在**锁外**按新代理配置 `build_outbound_client` 重建 client（含 TLS / 代理初始化开销；构造失败即返回错误、**不替换**），再短持写锁原子换快照指针（写临界区只做一次指针赋值）。
   - 5 处出站点不再各自持有启动期 client，改为**持 `Arc<NetworkState>`，每次出站经 `client()` 取当前 client**。PATCH 改代理后，下一个出站请求即用新代理。
2. **在线更新槽 `EditableUpdate`**（`RwLock<Arc<EditableUpdate>>`，与 `NetworkState` 并列于 `EditableSettings`）：收拢 `enabled` / `repo` / `api_base_url` / `restart_mode` / `token` 可运行时调字段。`update` 端点（check / apply）改为读本槽的当前值（含 `enabled` 开关），不再读 `config.update`。

经 `api::settings` 暴露：

- `PATCH /api/v1/settings`（仅 Admin）：薄 handler 校验（代理 URL 可构造、`restart_mode` 合法）→ 锁外重建 → 原子换槽 → 即时生效；**校验失败 400 且不触碰现有生效值**。
- `GET /api/v1/settings`（FR-87 已有）改为读热槽**当前值**组装脱敏 DTO（代理去 `user:pass@`、token 只回 `has_token`），不再读 `config`。

运行时改动**不写回 TOML / 不入 DB**：重启回落文件 + env 配置，守「配置 / 凭据真源是文件 + 环境变量」（与 ADR-0018 一致）。

## 理由

- **沿用 ADR-0020 的真源与 helper**：`[network.proxy]` 仍是唯一真源、`build_outbound_client` 仍是唯一构造入口，本 ADR 只把「启动期固化的 client」升级为「热替换槽里的当前 client」，不另起炉灶、不破坏统一注入。
- **`RwLock<Arc<快照>>` 是 std 即可表达的「读多写极少」热替换原语**（与 ADR-0018 同范式）：读路径无写争用、仅一次廉价 clone；写路径把 client 重建（高开销 IO / TLS）放在锁外，临界区只做指针赋值。无须 arc-swap 等外部依赖（守零外部依赖 + 简单优先）。
- **槽落在 `config` 层**：`config` 是所有出站模块的共同最底层，`NetworkState` 放这里，proxy / migrate / vuln / auth / update 皆可依赖而不产生反向跨层依赖与环（守分层不变量）。
- **配置 + client 打包整体替换**：保证代理配置与据其构造的 client 始终一致，绝不出现「配置已换、client 仍是旧代理」中间态。
- **凭据只入内存槽**：PATCH 接受的代理凭据与 token 仅存热槽、不写回 TOML / 不入 DB / 不进日志 / GET 不回显，重启回落文件 / env，守凭据红线。

## 后果

- 正面：网络代理与在线更新开关在线可编辑、即时生效、无须重启；配置与出站 client 强一致；零外部依赖、读路径开销可忽略；设置页从只读升级为可编辑（仅 Admin）。
- 约束 / 负面：
  - `AppState` 多一个 `settings` 槽；出站代理真源从「`Arc<Config>` 启动期固化 client」转为「热替换槽当前 client」，5 处出站点改为持 `Arc<NetworkState>` 按需取 client（须避免再有出站点直接持启动期 client，防双真源漂移）；`update` 端点改读 `EditableUpdate`（不再读 `config.update`）。
  - 每次出站多一次读锁 + `reqwest::Client` clone（廉价、无写争用），相对原「直接持有 client」开销略增，可接受。
  - 运行时改动不持久化，重启回落文件 + env 配置（运维须知；写入 CONFIG / OPERATIONS）。
  - 后台长生命周期出站任务（漏洞库镜像周期刷新）须在每次迭代从槽取当前 client，方能在热替换后用新代理（启动期一次性取则不生效）。
- 真机维度：实际网页改代理后出站经新代理生效、改 `enabled` 后 check 走出站，依赖真机复验；单测覆盖热替换语义与脱敏 / 鉴权 / 非法配置不改生效值。

## 备选方案

- **沿用 ADR-0020 启动期固化、不热替换**：不满足 FR-88「网页改完即时生效、无须重启」，落选（本 ADR 即为取代此取向）。
- **改 `config` 字段为 `Arc<RwLock<Arc<Config>>>` 整体热替换**：会波及全代码库几十处 `state.config.xxx` 直接字段访问，改动面巨大且越界（多数子树本期不需要热替换）。落选，仅把 network / update 两块收进专用槽。
- **引入 `arc-swap` 无锁热替换**：属新增外部依赖，违背「用 std 实现 / 零外部依赖」（与 ADR-0018 同理）。落选。
- **PATCH 写回 TOML 持久化**：与「配置 / 凭据真源是文件 + env、运行时改动不落库」基调相悖，且把凭据写入磁盘配置导出风险。落选，运行时改动只入内存、重启回落文件。
