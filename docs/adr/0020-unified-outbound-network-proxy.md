# ADR-0020：统一出站网络代理与共享出站客户端

## 状态

已接受

## 背景

二进制部署在受限网络（仅允许经企业正向代理出站）时，所有对外 HTTP(S) 请求都需经统一代理。当前代码里 5 处出站点各自独立 `reqwest::Client::builder()` 构造客户端（proxy 回源 `src/proxy/http.rs`、Nexus 迁移 `src/migrate/http.rs`、漏洞库镜像 `src/vuln/http.rs`、OIDC `src/auth/oidc.rs` 经 `main.rs` 装配、`main.rs` 自身），各自只配了超时，没有集中的出站代理注入点。运维无法用项目配置统一指定出站代理，散落的构造点也难以保证一致的 TLS / 流式特性。

约束：技术栈锁定 rustls（不引 native-tls / openssl）、单一二进制零原生运行时依赖（ADR-0001）；密钥 / 凭据不入库、不进日志、不进 DB 明文（架构不变量 §3）；简单优先，不为未来预留空壳。

## 决策

新增配置节 `[network.proxy]`（`http` / `https` / `no_proxy`，env 前缀 `JIANARTIFACT_NETWORK_PROXY_*`）作为出站代理的**唯一真源**，并在 `config` 模块抽一个共享出站客户端构造 helper `build_outbound_client(timeout, &NetworkProxyConfig)`，把代理与既有超时 / rustls / stream 特性统一注入。全部 5 处出站点经该 helper 构造 `reqwest::Client`，不再各自散落 `Client::builder()` 配置。

env-vs-config 取向：**配置显式给值时配置为真源**——注入 `Proxy` 即关闭 reqwest 的 auto-sys-proxy，配置压过系统环境；**三键全不配置时不调用任何 `.proxy()`，保留 reqwest 既有默认行为**（仍 honor 系统 `HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY`），以保证「不配置即与现状一致」。不另设开关强制忽略系统 env（YAGNI）。

## 理由

- **统一 helper 而非各自构造**：消除复制粘贴的 builder 配置，保证 5 处出站点的代理 / 超时 / TLS 特性一致；新增出站点（如 FR-85 更新检查）复用同一入口，不会漏注代理。
- **helper 落在 `config` 层**：依赖方向 `api → (auth/authz/repo/format) → (proxy/storage/meta) → config` 中，`config` 是所有出站模块的共同最底层，放这里不产生反向跨层依赖与环。
- **配置为真源**：与项目「凭据 / 配置真源在文件 + env」基调一致；出站代理是部署环境属性，集中在 TOML / env 便于运维。
- **rustls 兼容**：helper 在现有 `rustls-tls` 特性的 builder 上加 `.proxy(..)`，reqwest 经 rustls 与代理协作，不引入 native-tls / openssl，守 ADR-0001。
- **凭据脱敏**：代理 URL 可含 `user:pass@`；我方日志 / 错误信息绝不打印原始代理 URL，构造失败只记不含凭据的「代理配置无效」类消息；代理凭据不入库、不进 DB。
- **保留系统 env 默认**：不配置即等价现状，避免破坏既有依赖系统代理 env 的部署（向后兼容）。

## 后果

- 正面：出站代理集中可配、一处注入处处生效；出站客户端构造收敛为单一 helper，易测、易扩。
- 负面 / 约束：出站代理为全局一套（不区分各出站点）；不支持 SOCKS（未引入 reqwest `socks` 特性）；代理配置在启动期装载，运行时不热替换（与 vuln / proxy 客户端生命周期一致）。需 SOCKS / 分点代理 / 热替换时另写 ADR。
- 各出站客户端构造点须经 helper；新增出站点（FR-85）必须复用，禁止再散落 `Client::builder()`。

## 备选方案

- **每出站点各自读 `[network.proxy]` 自行注入**：复制粘贴、易漏配、特性易漂移，落选。
- **完全交给系统环境变量（不引项目配置）**：运维无法在单一配置文件集中管理，且与项目「配置 + env 双源、文件为基线」基调不符，落选（但作为「不配置时」的回退保留）。
- **引入 SOCKS / 分出站点代理 / 运行时热替换**：当前无明确需求，属镀金，按需再走新 ADR。
