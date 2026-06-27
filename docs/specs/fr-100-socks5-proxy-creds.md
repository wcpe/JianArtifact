# 功能规格：SOCKS5 出站代理 + 带凭据代理的网页可管理

> 状态：开发中　·　关联 PRD：FR-100（扩展 FR-84 出站代理 / FR-88 设置热替换）　·　分支：feature/fr-100-socks5-proxy-creds

## 1. 背景与目标

现状两处不足，真机使用在线更新（FR-85）拉取 GitHub 资产时暴露：

1. **不支持 SOCKS5**：`[network.proxy]` 仅 `http` / `https` 两键，经 `reqwest::Proxy::http/https` 注入；reqwest 未启 `socks` 特性（ADR-0020 当初明确不引），无法走 `socks5://` 代理。处在仅有 SOCKS5 出口的网络中即无法出站。
2. **网页无法管理带账号密码的代理**：设置页（FR-88）代理框填的是**脱敏值**（GET 经 `sanitize_proxy_url` 去 `user:pass@`），且无独立凭据字段；"未改动即原样回传脱敏值"会把已有凭据冲掉，注释直接写"如需保留凭据请改 config.toml"。导致带账号密码的代理在网页上加不进、也存不住。

本功能补齐两点（守"凭据不回显"红线）：

- 加 **SOCKS5**（含账号密码）出站代理支持：新增 `[network.proxy] all` 单键，接受 `socks5://` / `socks5h://` / `http(s)://`（可含 `user:pass@`），经 `reqwest::Proxy::all` 注入；启用 reqwest `socks` 特性。
- 设置页支持**填/改带账号密码的代理**（HTTP / HTTPS / SOCKS5）：每代理拆为 **URL + 用户名 + 密码** 三字段。**用户名可回显**（标识、非密钥），**密码绝不回显**（沿用 update token 的三态语义：缺省=保留、空串=清空、非空=设置）；PATCH 重建出站 client 即时生效。

属 P2（FR-84/88 增强）。引入新依赖特性（reqwest `socks`）+ 改既有架构决策（ADR-0020 原"不支持 SOCKS"）→ **新增 ADR-0024 取代 ADR-0020 相关条目**。

## 2. 需求（要什么）

### 范围内

- `[network.proxy]` 新增 `all` 键：单一全 scheme 代理 URL，接受 `socks5://` / `socks5h://` / `http://` / `https://`，可含 `user:pass@`；经 `reqwest::Proxy::all` 注入。
- reqwest 启用 `socks` 特性（`Cargo.toml`，新依赖特性）。
- 出站 client 构造：`http` / `https` 仍为 scheme 专属代理；`all` 为兜底全 scheme 代理。三者均挂 `no_proxy` 绕过列表。
- 设置页每代理（http / https / all）三字段：URL（脱敏 host，无凭据）、用户名（回显）、密码（三态、不回显）。
- GET `/settings`：每代理回 `{ url: 脱敏host, username: 回显用户名, has_password: bool }`；**密码绝不出现在响应**。
- PATCH `/settings`：每代理收 `{ url, username, password(三态) }`，后端按 §3.3 纯函数重建存储 URL；校验失败 400 不改现值；成功锁外换槽即时生效。
- 凭据（用户名 / 密码）只入内存热替换槽，**不写回 TOML、不入 DB、不进日志**（沿用 ADR-0018 / ADR-0022）。

### 不做（范围外）

- 不支持 SOCKS4、不支持 `https` 经 SOCKS 之外的其它代理协议。
- 不做按 host 的多代理路由表（仅 http / https / all 三槽 + no_proxy）。
- 不把代理凭据持久化到 DB / TOML（重启回落配置文件 + env，与现状一致）。
- 不动在线更新 / CI 既有逻辑（仅复用出站 client）。

## 3. 设计（怎么做）

### 3.1 配置模型（`config.rs`）

`NetworkProxyConfig` 新增字段：

```rust
pub struct NetworkProxyConfig {
    pub http: Option<String>,
    pub https: Option<String>,
    pub all: Option<String>,      // 新增：全 scheme 代理（socks5:// / http(s)://，可含 user:pass@）
    pub no_proxy: Option<String>,
}
```

TOML：
```toml
[network.proxy]
http = "http://user:pass@host:3128"
https = "http://user:pass@host:3128"
all = "socks5://user:pass@host:1080"   # 新增；优先级见 §3.2
no_proxy = "localhost,127.0.0.1"
```

### 3.2 出站 client 构造（`build_outbound_client`）

- 注入顺序：`http` → `https` → `all`（reqwest 按注册顺序首个匹配生效，故 scheme 专属的 http/https 对各自 scheme 优先，`all` 兜底其余；仅配 `all` 时覆盖 http+https，正是 SOCKS5 单代理的常见用法）。
- `all` 经 `reqwest::Proxy::all(url)` 构造（启 `socks` 特性后认 `socks5://` 并解析 `user:pass@` 为认证）。
- 三代理均 `.no_proxy(no_proxy)`。
- 四键全空 → 不调 `.proxy()`，保持现状（honor 系统环境）。
- 构造失败错误信息**不含原始 URL**（守凭据脱敏，沿用现注释）。

### 3.3 凭据重建纯函数（`api/settings.rs`，可穷举测试）

```rust
/// 据 PATCH 单代理三字段 + 当前存储值，重建存储 URL（含凭据）。返回 None 表示清除该代理。
fn rebuild_proxy_url(patch: &ProxyEntryPatch, current: Option<&str>) -> Option<String>
```

规则（穷举）：
1. `patch.url` 空白 / 缺省 → `None`（清除该代理；用户名 / 密码忽略）。
2. 否则取 `host = sanitize_proxy_url(url)`（去掉用户误带的 userinfo，只留 `scheme://host:port[/path]`）。
3. `username = patch.username.unwrap_or_default().trim()`。
4. 密码三态：
   - `patch.password == None`（未改）→ **保留现有密码**：当 `current` 的 userinfo 用户名与本次 `username` **一致**时沿用其密码；否则视为无密码。
   - `Some("")` → 无密码（清空）。
   - `Some(p)` → 设为 `p`。
5. 组装：`username` 空 → 直接 `host`（无 userinfo，即便给了密码也忽略——无用户不能单挂密码）；否则在 `scheme://` 后插入 `username[:password]@`。
   - userinfo 按 RFC 3986 对 `:` `@` `/` 等保留字符百分号编码（用户名 / 密码可能含特殊字符）。

> 该函数纯字符串逻辑、无副作用，便于穷举各分支。`current` 取自热替换槽当前生效的该代理存储 URL。

### 3.4 DTO 形态

GET 视图（`SettingsView.network_proxy`）：
```rust
pub struct ProxyEntryView {
    pub url: Option<String>,      // 脱敏：scheme://host:port（无 userinfo）
    pub username: Option<String>, // 回显用户名（非密钥；无则 None）
    pub has_password: bool,       // 是否已配置密码（绝不回显密码本体）
}
pub struct NetworkProxyView {
    pub http: ProxyEntryView,
    pub https: ProxyEntryView,
    pub all: ProxyEntryView,      // 新增
    pub no_proxy: Option<String>,
}
```

PATCH 请求（`SettingsPatch.network_proxy`）：
```rust
pub struct ProxyEntryPatch {
    #[serde(default)] pub url: Option<String>,       // host(无凭据)；空 / 缺省 = 清除该代理
    #[serde(default)] pub username: Option<String>,  // 用户名；空 = 无用户
    #[serde(default)] pub password: Option<String>,  // 三态：缺省=保留现有 / ""=清空 / 非空=设置
}
pub struct NetworkProxyPatch {
    #[serde(default)] pub http: ProxyEntryPatch,
    #[serde(default)] pub https: ProxyEntryPatch,
    #[serde(default)] pub all: ProxyEntryPatch,
    #[serde(default)] pub no_proxy: Option<String>,
}
```

辅助：从存储 URL 解析 `username`（回显）与 `has_password`（GET 用）；与 `sanitize_proxy_url` 同口径只看 authority 段 userinfo。

### 3.5 前端（`SettingsPage.tsx` + api 类型）

- 网络代理区每代理（HTTP / HTTPS / SOCKS5(all)）一组三字段：URL、用户名、密码。
  - URL / 用户名用 GET 回显值预填；密码框始终空，`PasswordInput`，占位"留空保留现有密码"，旁注 `has_password` 时标"已配置"。
  - SOCKS5 填到 `all`，占位示例 `socks5://host:1080`。
- 保存：每代理组装 `{ url, username, password }`——密码框非空才带 `password`（设置），留空则**省略** `password` 字段（保留现有）；要清空密码另给一个"清除密码"动作发 `password: ""`。
- api `SettingsView` / `getSettings` / `patchSettings` 类型同步新形态。

### 3.6 文档同步

- 新增 ADR-0024（取代 ADR-0020 关于"不支持 SOCKS"的条目：支持 SOCKS5 / 全 scheme `all` 代理；网页凭据管理——用户名回显、密码三态不回显），ADR-0020 状态标"§相关条目被 ADR-0024 取代"。
- ARCHITECTURE 出站代理段：补 `all` / SOCKS5 与凭据回显口径。
- CONFIG / OPERATIONS：补 `[network.proxy] all` 键与 socks5 示例、网页凭据管理说明。
- CHANGELOG 未发布段、PRD §4 FR-100 行。

## 4. 任务拆分

- [x] 写规格（本文）+ PRD §4 新增 FR-100 行（开发中）+ ADR-0024
- [x] `Cargo.toml`：reqwest 启 `socks` 特性
- [x] `config.rs`：`NetworkProxyConfig.all` + `build_outbound_client` 注入 `Proxy::all` + 默认模板 / KNOWN keys
- [x] `api/settings.rs`：DTO 改形 + `rebuild_proxy_url` 纯函数 + GET/PATCH 接线 + 解析 username/has_password
- [x] 前端：SettingsPage 三字段 × 三代理 + api 类型
- [x] 测试：config（all/socks5 注入、auth、非法 url）、settings（rebuild_proxy_url 穷举、GET 回显/不回显、PATCH 不写 TOML）；前端组件（前端 agent）
- [x] 文档同步：ADR-0024 / ARCHITECTURE / CONFIG / OPERATIONS（CHANGELOG 主代理统一加）

## 5. 验收标准

- `[network.proxy] all = "socks5://user:pass@host:1080"` 能构造出站 client 且经 SOCKS5 出站（reqwest `socks` 特性已启）；非法 socks url → 构造失败、错误不含 URL。
- 出站优先级：同时配 `http` 与 `all` 时，http 走 http 代理、其余走 all；仅配 all 时 http+https 均走 all。
- GET `/settings` 每代理回 `url`（脱敏）/ `username`（回显）/ `has_password`；**任何情况下响应不含密码本体**。
- PATCH 凭据三态正确（穷举 `rebuild_proxy_url`）：清除 / 设新 / 改 host 留密码（同 username 保留、改 username 不保留）/ 清空密码 / socks5 scheme；校验失败 400 不改现值。
- 凭据只入内存槽：PATCH 后 `config.toml` 与 DB 均无凭据、日志无凭据。
- 网页：能新增/修改带账号密码的 HTTP / HTTPS / SOCKS5 代理并保存生效；保存其它设置不丢已有代理密码。
- fmt / clippy（`-D warnings`）/ 后端 test / 前端 build + test 全绿。
- **【需用户确认 · 真机维度】** 真实 SOCKS5 / 带认证 HTTP 代理出口下经设置页配置并成功拉取 GitHub 资产，需用户在真实代理环境验证；本地以 fake / 回环代理覆盖逻辑，真出口连通性待真机确认。

## 6. 风险 / 待定

- reqwest `socks` 特性引入 `tokio-socks` 等传递依赖（纯 Rust、无 native），`cargo audit` 纳入门禁。
- `username` 回显放宽了 ADR-0020 原"去整个 `user:pass@`"口径：判定**用户名是标识非密钥**、密码才是秘密——仅密码绝不回显。该放宽在 ADR-0024 显式记录。
- userinfo 百分号编码须覆盖含 `@` `:` `/` 的口令，避免重组出歧义 URL。
