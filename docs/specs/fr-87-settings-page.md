# 功能规格：Web 控制台设置页（网络代理 + 在线更新）

> 状态：开发中　·　关联 PRD：FR-87　·　依赖：FR-84（网络代理）/ FR-85（在线更新）　·　分支：feature/fr-87-settings-page

## 1. 背景与目标

FR-84（统一出站网络代理）与 FR-85（在线更新）已落后端（配置 + API），但无 Web 控制台入口。FR-87 把二者收口为一个**仅 Admin** 的「设置」页：① 查看当前网络代理配置/状态；② 在线更新的版本检查与一键升级入口。

**只读取向（守 ADR-0020）**：网络代理与在线更新的配置**真源是 TOML 文件 + env，运行时不热替换**（ADR-0020），故本页对配置**只读展示、不提供编辑**；页面唯一的写动作是「检查更新 / 应用更新」（FR-85 既有端点）。

属 P2（前端 UI + 一个只读后端聚合端点）。无新架构决策 → **不写 ADR**（脱敏沿用既有 token/凭据脱敏惯例）。

## 2. 需求（要什么）

### 范围内
- **后端只读聚合端点** `GET /api/v1/settings`（仅 Admin）：返回脱敏后的网络代理 + 在线更新配置 + 当前版本，供页面展示。**绝不回显任何凭据**（代理 URL 的 `user:pass@` 脱敏；更新 token 只回 `has_token: bool`）。
- **前端设置页** `/settings`（仅 Admin，`RequireAdmin` 守卫 + 导航入口）：
  - **网络代理**区：展示 http/https 代理（脱敏 URL）、no_proxy；标注「配置真源为 config.toml / 环境变量，运行时不可改」。
  - **在线更新**区：展示 enabled 状态、仓库源、当前版本；
    - 「检查更新」按钮 → `GET /api/v1/update/check` → 展示最新版本 / 是否有更新 / 发布说明；
    - 有更新时「升级到 vX.Y.Z」按钮 → 二次确认弹窗 → `POST /api/v1/update/apply` → 成功后提示「已触发升级，服务正在重启…」并进入等待/重连态。
  - 错误友好提示：`409 在线更新未启用` → 引导去配置开启；`502` → 上游不可达；`422` → 下载校验失败已拒绝替换；`400` → 平台不支持。

### 不做（范围外）
- 不提供配置编辑（代理/更新配置只读；真源在文件/env，守 ADR-0020 不热替换）。
- 不做自动轮询更新 / 定时检查（仅手动点「检查更新」）。
- 不展示/回显任何凭据（代理凭据、更新 token）。
- 不改 FR-85 的 check/apply 端点行为，仅消费。

## 3. 设计（怎么做）

### 3.1 后端 `GET /api/v1/settings`（仅 Admin）
- 新增 `src/api/settings.rs`：薄 handler，`identity.require_admin()?`，读 `state.config` 组装脱敏 DTO，返回 `Json<SettingsView>`。
- 在 `build_router` 挂 `GET /api/v1/settings`。
- DTO（serde snake_case）：
  ```
  SettingsView {
    current_version: String,        // CARGO_PKG_VERSION
    network_proxy: {
      http: Option<String>,         // 脱敏：去掉 user:pass@
      https: Option<String>,        // 脱敏
      no_proxy: Option<String>,
    },
    update: {
      enabled: bool,
      repo: String,
      api_base_url: String,
      restart_mode: String,
      has_token: bool,              // 仅布尔，绝不回显 token 本体
    },
  }
  ```
- **脱敏纯函数** `sanitize_proxy_url(&str) -> String`：去掉 URL 中的 `userinfo@`（`scheme://user:pass@host` → `scheme://host`），其余原样；空/无 `@` 原样返回。单测穷举（含/不含凭据、含端口、非法串不 panic）。

### 3.2 前端
- `frontend/src/pages/SettingsPage.tsx`：Mantine 卡片两区（网络代理 / 在线更新），仿 `ProtectionConfigPage` 的数据加载与错误处理风格。
- `frontend/src/api/`：`types.ts` 加 `SettingsView` / `UpdateCheck` / `ApplyResponse` 类型；`endpoints.ts` 加 `getSettings` / `checkUpdate` / `applyUpdate`；沿用 `client.ts` 既有请求封装与错误结构。
- `App.tsx`：在 `RequireAdmin` 下加 `<Route path="settings" element={<SettingsPage />} />`。
- 导航：`components/AppLayout`（或导航组件）加「设置」入口（仅 Admin 可见，仿其他 Admin 链接）。
- 「应用更新」交互：确认弹窗 → 调 apply → 成功后页面进入「正在重启」态（apply 成功即服务将停机重启，前端展示提示并可引导用户稍后刷新；不强求自动重连）。

### 3.3 安全
- 端点仅 Admin；脱敏在后端完成（前端永远拿不到凭据）。
- token 只回 `has_token`；代理 URL 经 `sanitize_proxy_url` 去凭据。

## 4. 任务拆分
- [x] 写规格（本文）+ PRD §4 FR-87 计划→开发中（仅改 FR-87 行）
- [x] 后端 `src/api/settings.rs`（GET /settings + 脱敏纯函数）+ 路由挂载 + 单测（脱敏穷举、admin-only、token 不回显）
- [x] 前端 types/endpoints + SettingsPage + 路由 + 导航入口
- [x] 前端测试 `SettingsPage.test.tsx`（加载展示、检查更新、应用更新确认流、各错误态、非 Admin 不可达）
- [x] doc-sync：API.md（/settings 端点）、ARCHITECTURE（settings 只读聚合端点）、CHANGELOG 未发布段追加

## 5. 验收标准
- `GET /api/v1/settings`：Admin 返回脱敏配置；匿名 401 / 普通用户 403；**响应绝不含代理凭据或 update token**（单测断言含 `user:pass@` 的代理被脱敏、token 仅 `has_token`）。
- 设置页仅 Admin 可达（`RequireAdmin`），非 Admin 重定向。
- 网络代理区正确展示脱敏配置并标注只读。
- 在线更新区：检查更新展示版本对比；有更新时可触发升级（确认弹窗 → apply）；`enabled=false` 时展示「未启用」并禁用升级按钮、检查返回 409 友好提示。
- 各错误码（409/502/422/400）前端有可读提示，不抛裸错误。
- 前端 `pnpm -C frontend test`（Vitest）绿、`pnpm -C frontend build` 通过；后端 `cargo test` / `clippy -D warnings` / `fmt` 绿。

## 6. 风险 / 待定
- **应用更新后前端体验**：apply 成功即触发服务重启，当前连接会断；前端只做「已触发、正在重启」提示 + 引导手动刷新，不实现自动健康轮询重连（YAGNI，避免与重启时序耦合）。如需自动重连体验可后续另议。
- **脱敏完备性**：`sanitize_proxy_url` 只去 `userinfo@`；若代理 URL 形态异常（无 scheme 等）按原样返回但仍不应含凭据——单测覆盖边界。
- 真机维度：本页依赖 FR-85 真机验证（升级实际生效）；FR-87 自身验收以前端测试 + 后端单测 + 浏览器手测（用户）为准。
