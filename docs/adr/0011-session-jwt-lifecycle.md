# ADR-0011：会话与 JWT 生命周期

## 状态

已接受

## 背景

Web 控制台是 React SPA，需要登录态管理；同时 CLI / 包管理器用 Bearer Token / Basic Auth（见 ADR-0003）。需明确 Web 会话的承载形式、有效期、续期与 CSRF 防护，避免与 API Token 混淆，避免会话相关漏洞。

## 决策

Web 控制台采用有限有效期的会话 / JWT：

- 默认 TTL 约 1 小时（可配置）；提供刷新端点 `POST /api/v1/auth/refresh` 续期；过期或吊销后须重新登录。
- 提供 `GET /api/v1/me` 返回当前用户，供前端判定登录态与权限。
- 会话凭据与 API Token 相互独立：API Token 不设过期、仅可吊销。
- CSRF 策略随承载方式确定：若会话以 Cookie 承载，对改变状态的请求施加 CSRF 防护（如 `SameSite` + CSRF Token）；若以 `Authorization` 头承载 Bearer，则不依赖 Cookie、天然规避 CSRF。

## 理由

- 有限 TTL + 刷新兼顾安全与体验。
- 会话与 API Token 分离，职责清晰，便于分别穷举测试过期 / 吊销。
- CSRF 策略覆盖两种实现路径，不留空白。

## 后果

- 正面：会话安全边界清晰，过期 / 刷新 / 吊销可穷举测试（见 `testing-and-quality` §2.6）。
- 负面/约束：刷新与过期逻辑增加实现与测试面；若选 Cookie 承载须正确实现 CSRF 防护；前端须处理 `401` 后的重新登录 / 续期。

## 备选方案

- 长期不过期会话：安全性差。落选。
- 完全复用 API Token 作为 Web 登录态：职责混淆，难以区分吊销与过期语义。落选。
