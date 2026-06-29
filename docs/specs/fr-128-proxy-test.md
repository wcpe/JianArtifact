# 功能规格：出站代理连通性测试

> 状态：开发中　·　关联 PRD：FR-128　·　分支：feature/fr-128-proxy-test

## 1. 背景与目标

P2 功能，增强 FR-84（出站代理配置）与 FR-87（设置页代理节）。

管理员配置代理后，难以在页面内即时验证代理是否通畅。本功能在设置页代理节新增「测试」按钮：
填入目标 URL → 点测试 → 后端经当前生效出站代理对该 URL 发请求 → 返回连通性结果（状态码 / 耗时 / 失败原因）。

仅 Admin 显式触发、只访问用户给定 URL，不外发任何使用数据（符合 ADR-0009）。

## 2. 需求（要什么）

- 后端新端点 `POST /api/v1/settings/proxy-test`（仅 Admin）：
  - 请求体：`{ "url": "https://..." }`（http/https scheme，非法 URL 返回 400）
  - 逻辑：用 `state.settings.network.client()`（含当前生效出站代理）对目标 URL 发 GET，超时约 10s
  - 响应：`{ "ok": bool, "status": u16|null, "elapsed_ms": u64, "error": string|null }`
  - 非法 URL / 非 http/https scheme → 400；非 Admin → 403；未认证 → 401
- 前端设置页代理节：
  - 新增 URL 输入框 + 「测试」按钮
  - 点击调后端端点，展示结果（成功：绿色状态码 + 耗时；失败：红色失败原因）
  - 测试中禁用按钮（防重复触发）
  - i18n 文案写入 `proxy.test*` 键

范围内：只做连通性测试，不做代理可用性监控、不轮询、不落库。
不做：不支持带自定义请求头 / 请求体的高级测试；不暴露测试历史；不改代理配置本身。

## 3. 设计（怎么做）

### 后端

`src/api/settings.rs` 新增 handler `proxy_test`：

```
POST /api/v1/settings/proxy-test
{ "url": "https://example.com" }
→ { "ok": true, "status": 200, "elapsed_ms": 123, "error": null }
  { "ok": false, "status": null, "elapsed_ms": 500, "error": "连接超时" }
```

- `identity.require_admin()?` 鉴权
- 校验 URL：`url::Url::parse` 成功且 scheme 为 http/https，否则 400
- `let client = state.settings.network.client();`（读锁极短、锁外发请求）
- 带 10s 超时发 GET，记录 `elapsed_ms`
- 成功：`ok=true, status=响应状态码`；失败：`ok=false, error=错误描述`
- handler 保持薄：校验 + 调 client + 返回，无业务逻辑

路由注册：在 `src/api/mod.rs` 的 `/settings` 路由组之后追加：
```
.route("/settings/proxy-test", post(settings::proxy_test))
```

### 前端

`frontend/src/pages/SettingsPage.tsx` 代理节（id="proxy" Card）末尾追加连通性测试组：
- TextInput：输入测试 URL
- Button「测试」：调 `api.testProxy(url)`，loading 状态下禁用
- Alert 展示结果（绿色/红色，含状态码、耗时、错误原因）

`frontend/src/api/types.ts` 追加 `ProxyTestRequest` / `ProxyTestResult`。
`frontend/src/api/endpoints.ts` 追加 `testProxy(url)`。
`frontend/src/i18n/locales/zh-CN/settings.ts` 追加 `proxy.test*` 文案键。

依赖：无新第三方依赖（reqwest 已有）。

## 4. 任务拆分

- [x] 写 spec（本文件）
- [x] PRD FR-128 状态改为「开发中」
- [ ] 后端测试（红）：`proxy_test_匿名被拒_401` / `proxy_test_普通用户被拒_403` / `proxy_test_非法url_400` / `proxy_test_非http_scheme_400` / `proxy_test_不可达地址返回_ok_false`
- [ ] 后端实现：`ProxyTestRequest` / `ProxyTestResult` 结构体 + `proxy_test` handler + 路由注册
- [ ] 前端类型：`ProxyTestRequest` / `ProxyTestResult` in types.ts
- [ ] 前端端点：`testProxy` in endpoints.ts
- [ ] 前端 i18n：`proxy.test*` 文案
- [ ] 前端组件：代理节追加测试组件 + 测试
- [ ] 文档同步：CHANGELOG / API.md

## 5. 验收标准

- Admin 填入可达 URL，点「测试」，响应返回 `{ ok: true, status: 2xx, elapsed_ms: N }`，前端显绿色结果。
- Admin 填入不可达 URL，响应返回 `{ ok: false, error: "..." }`，前端显红色失败原因。
- 非法 URL（如 `ftp://...`、空串、乱码）返回 400。
- 非 Admin 用户请求 403；未认证 401。
- 后端测试全绿；前端 vitest 测试全绿。
- 真机经代理测试一次（待真机验收）。

## 6. 风险 / 待定

- 测试 URL 由用户提供，后端只接受 http/https，防止 SSRF 利用 file://、ftp:// 等协议。
- 10s 超时是合理保守值，不挂死请求；可在配置中未来可调（本期硬编码）。
