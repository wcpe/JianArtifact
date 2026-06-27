# ADR-0024：SOCKS5 出站代理与网页代理凭据管理

## 状态

已接受（取代 ADR-0020「后果」中「不支持 SOCKS（未引入 reqwest `socks` 特性）」一条；ADR-0020 其余决策、ADR-0022 热替换决策均沿用）

## 背景

真机使用在线更新（FR-85）经代理拉取 GitHub 资产时暴露两处不足：

1. **无 SOCKS5**：ADR-0020 把 `[network.proxy]` 落为 `http` / `https` 两键、经 `reqwest::Proxy::http/https` 注入，并在「后果」显式记「不支持 SOCKS（未引入 reqwest `socks` 特性），需 SOCKS 时另写 ADR」。处在仅有 SOCKS5 出口的网络中无法出站。
2. **网页无法管理带凭据的代理**：ADR-0022 把设置做成运行时可编辑热替换，但代理 URL 在 GET 经 `sanitize_proxy_url` 去 `user:pass@` 脱敏后回显，设置页又无独立凭据字段，"未改动即原样回传脱敏值"会冲掉已有凭据——带账号密码的代理在网页加不进、存不住。

约束不变：rustls-only、单一二进制零原生运行时依赖；凭据不入库、不进日志、不进 DB 明文（架构不变量 §3）；简单优先。

## 决策

1. **启用 reqwest `socks` 特性**，`[network.proxy]` 新增单键 `all`（env `JIANARTIFACT_NETWORK_PROXY_ALL`），接受 `socks5://` / `socks5h://` / `http(s)://`（可含 `user:pass@`），经 `reqwest::Proxy::all(url)` 注入为全 scheme 兜底代理。`http` / `https` 仍为 scheme 专属代理，注入顺序 `http → https → all`：scheme 专属者对各自 scheme 优先，`all` 兜底其余（仅配 `all` 即覆盖 http+https，正是 SOCKS5 单代理常见用法）。
2. **网页代理凭据按"用户名回显、密码三态不回显"管理**：设置页每代理（http / https / all）拆为 URL（脱敏 host）+ 用户名 + 密码三字段。
   - **用户名可回显**（GET 返回）：判定用户名是**标识、非密钥**。
   - **密码绝不回显**：GET 只回 `has_password: bool`；PATCH 密码沿用 update token 三态——缺省=保留现有、空串=清空、非空=设置。
   - 后端以纯函数 `rebuild_proxy_url(patch, current)` 据三字段 + 当前存储值重建含凭据的存储 URL（改 host 留密码仅当用户名一致时沿用），凭据只入内存热替换槽，不写 TOML / 不入 DB / 不进日志（沿用 ADR-0018 / ADR-0022）。

## 理由

- **`all` 单键而非每 scheme 加 socks 键**：SOCKS 代理本就全 scheme 通吃，`reqwest::Proxy::all` 一键即覆盖；避免 `socks_http` / `socks_https` 等冗余键，简单优先。
- **复用既有 `build_outbound_client` 入口**：仍是出站唯一构造点（ADR-0020 §决策），只多注一个 `Proxy::all`，5 处出站点零改动自动获得 SOCKS5 能力。
- **用户名回显 / 密码不回显**：在"凭据不回显"红线与"网页可管理凭据"可用性之间取真正的安全边界——密码（与 token）是秘密，绝不回显；用户名是连接标识（如同登录名），回显它才能让运维在网页确认/编辑代理而不必每次重填，且不泄露秘密。该放宽相对 ADR-0020「去整个 `user:pass@`」是有意为之、范围限于用户名。
- **三态密码 + 纯函数重建**：与 update token 已验证的三态语义一致，降低心智负担；纯字符串逻辑便于穷举测试各分支（清除 / 设新 / 留密码 / 清密码 / scheme 切换）。

## 后果

- 正面：受限网络（SOCKS5 出口、带认证 HTTP 代理）可用；代理凭据可在网页安全管理（密码不泄露），保存其它设置不丢密码。
- 负面 / 约束：reqwest `socks` 特性引入 `tokio-socks` 等纯 Rust 传递依赖（无 native，纳入 `cargo audit`）；代理仍为全局一套、不区分各出站点（沿用 ADR-0020）；用户名在 GET 中可见（密码、token 仍绝不可见）。
- 仍不支持 SOCKS4 / 按 host 多代理路由表 / 代理凭据持久化到 DB（重启回落配置 + env）——需要时另走 ADR。

## 备选方案

- **每代理拆独立用户名/密码键写入 TOML**：把凭据落 TOML 与"凭据不入库基调 + env 真源"冲突，落选；凭据仍只入内存槽，TOML 仅作重启回落基线。
- **URL 单字段、重填含凭据完整 URL**：用户需手拼 `socks5://user:pass@host` 且脱敏回显后易误冲凭据，可用性差，落选（选独立三字段 + 三态密码）。
- **沿用 ADR-0020 不支持 SOCKS**：真机网络环境确有仅 SOCKS5 出口者，FR-85 无法出站，落选。
