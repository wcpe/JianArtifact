# 功能规格：统一出站网络代理

> 状态：开发中　·　关联 PRD：FR-84　·　分支：feature/fr-84-network-proxy

## 1. 背景与目标

二进制部署在受限网络（仅允许经企业代理出站）时，所有对外 HTTP(S) 请求都需经统一的正向代理出站。当前代码里 5 处出站 `reqwest::Client` 各自独立 `Client::builder()`，没有统一的代理注入点，运维无法用项目配置集中指定出站代理。

本功能（P2）新增 `[network.proxy]` 配置，抽一个共享的出站客户端构造 helper，把 http / https / no_proxy 代理设置统一注入到全部 5 处出站点：proxy 回源、Nexus 迁移、漏洞库镜像、OIDC、（main 装配处）。

## 2. 需求（要什么）

- 新增配置节 `[network.proxy]`，键：`http`（HTTP 代理 URL）、`https`（HTTPS 代理 URL）、`no_proxy`（逗号分隔的直连主机/域/网段列表）。
- env 覆盖前缀 `JIANARTIFACT_NETWORK_PROXY_*`（如 `JIANARTIFACT_NETWORK_PROXY_HTTPS`）。
- 抽一个共享出站 reqwest client 构造 helper，集中注入代理与既有超时；保留现有 rustls / stream 特性不变。
- 统一注入到 5 处出站点：`src/proxy/http.rs`、`src/migrate/http.rs`、`src/vuln/http.rs`、`src/auth/oidc.rs`（经 `main.rs` 装配）、`src/main.rs`。
- 凭据型代理 URL（`http://user:pass@host`）的凭据**绝不进日志 / 错误信息 / DB**。
- 范围内：正向出站代理（http/https/no_proxy）的配置与统一注入。
- 不做（范围外）：SOCKS 代理（reqwest `socks` 特性，未引入）；按出站点分别配代理（全局一套即可，YAGNI）；出站代理的运行时热替换（启动期装载即可，与 vuln/proxy 客户端生命周期一致）。

## 3. 设计（怎么做）

详见 ADR-0020（决策正文：为何统一 helper、配置真源、rustls 兼容、凭据脱敏、env-vs-config 取向）。本节只列落地要点：

- **配置**：`config` 模块新增 `NetworkConfig { proxy: NetworkProxyConfig }`，`NetworkProxyConfig { http: Option<String>, https: Option<String>, no_proxy: Option<String> }`，挂到顶层 `Config.network`，`#[serde(default)]`。三键默认 `None`（不配置 = 不显式注入）。
- **env 映射**：`KNOWN_SECTIONS` 加 `network`；`KNOWN_NESTED_PREFIXES` 加 `("network_proxy_", "network.proxy.")`，使 `JIANARTIFACT_NETWORK_PROXY_HTTPS` → `network.proxy.https`。
- **共享 helper**：`config::build_outbound_client(timeout, &NetworkProxyConfig) -> reqwest::Result<reqwest::Client>`，在 `Client::builder().timeout(..)` 基础上：
  - `https` 有值 → `Proxy::https(url)?`；`http` 有值 → `Proxy::http(url)?`；二者都注入时各管对应 scheme。
  - `no_proxy` 有值 → 经 `NoProxy::from_string` 解析后挂到所注入的 Proxy 上。
  - 三键全 `None` → 不调用任何 `.proxy()`，**保持 reqwest 既有行为不变**（含其默认 honor 系统 `HTTP(S)_PROXY`/`NO_PROXY` env）。
- **注入点改造**（不破坏现有 `*::new(timeout)` 签名，避免大面积改测试）：
  - `HttpUpstream` / `HttpNexusClient` / `HttpMirrorSource` 各保留 `new(timeout)`（内部委托 helper + 默认空代理），新增 `with_network(timeout, &NetworkProxyConfig)` 走 helper 注入；生产调用点（main.rs / api/migrate.rs / vuln 调度）改用 `with_network`。
  - OIDC：`main.rs` 用 helper 直接构造 `reqwest::Client` 注入 `OidcProvider::new`。

## 4. 任务拆分

- [ ] config：`NetworkConfig` / `NetworkProxyConfig` 结构体 + 顶层挂载 + env 映射（section + nested prefix）
- [ ] config：`build_outbound_client` helper（代理 + no_proxy + 超时 + rustls 保持）
- [ ] 5 处出站点改用 helper（生产调用点注入 `network.proxy`）
- [ ] 测试先行（红→绿）：代理注入、no_proxy 绕过、不配置等价现状、凭据脱敏
- [ ] ADR-0020 + ARCHITECTURE §7 索引 + §5 机制
- [ ] 文档同步：PRD 状态、CONFIG、OPERATIONS、CHANGELOG

## 5. 验收标准

- 配 `network.proxy.https` 后，经 helper 构造的 client 实际把出站请求发往该代理（本地 mock 代理断言收到 CONNECT / 请求；或断言 builder 注入了 proxy）。
- `no_proxy` 命中的主机直连绕过代理（mock 代理不收到该主机请求）。
- 三键不配置时，client 行为与现状一致（不显式注入 proxy，沿用 reqwest 默认）。
- 凭据型代理 URL 的用户名 / 口令不出现在日志、错误信息中。
- env `JIANARTIFACT_NETWORK_PROXY_HTTPS` 正确覆盖到 `network.proxy.https`。
- `cargo test` 全绿、`cargo clippy --all-targets -- -D warnings` 与 `cargo fmt --check` 通过。
- 无真机维度阻塞：mock 代理本地可验，不需真实企业代理。

## 6. 风险 / 待定

- env-vs-config 取向（**已决策，见 ADR-0020**）：配置显式给值时配置为真源（注入即关闭 reqwest auto-sys-proxy）；不给值时保留 reqwest 默认（仍 honor 系统 env），以满足「不配置等价现状」。不另设开关强制忽略系统 env（YAGNI）。
- 凭据脱敏：代理 URL 含凭据时，构造失败的 `reqwest::Error` 默认不回显 URL 凭据；但我方日志 / 错误信息绝不打印原始代理 URL，只记「代理配置无效」类不含凭据的消息。
