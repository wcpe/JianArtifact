# 功能规格：React Web 控制台（FR-18~22）

> 状态：开发中　·　关联 PRD：FR-18 / FR-19 / FR-20 / FR-21 / FR-22　·　分支：feature/p1-web

## 1. 背景与目标

后端已具备完整的认证 / 授权 / 仓库 / 四格式 / 制品管理 API。本规格补齐 P1（MVP）的最后一块：
一个 React 单页控制台，让管理员与用户经浏览器完成登录、建仓、授权、Token 管理与制品浏览 / 搜索；
并经 `rust-embed` 在编译期把前端产物嵌入单一二进制，保持「单文件、零外部运行时依赖」的产品定位
（ADR-0001）。属第一期（MVP）。

## 2. 需求（要什么）

- FR-18 登录与仪表盘：用户名 / 口令登录拿 JWT；仪表盘展示基础信息（当前用户、角色、可见仓库数、
  格式 / 类型分布）。
- FR-19 仓库管理界面：列表 / 创建 / 配置（可见性、proxy 上游）/ 删除；展示格式、hosted/proxy、可见性。
- FR-20 用户与权限管理界面：用户 CRUD（仅 Admin）；每仓库 ACL 读 / 写授权管理（仅 Admin）。
- FR-21 Token 管理界面：自助签发 / 列表 / 吊销；签发时一次性显示明文并提示立即保存。
- FR-22 制品浏览 / 搜索界面：仓库内制品浏览 + 跨仓库搜索（`/api/v1/search`）；点制品看详情
  （四校验和 + 按格式生成的使用方式片段）。

- 范围内：上述五个界面、与真实后端契约严格对齐的 API 客户端（Bearer 鉴权、401→跳登录、统一
  `{error:{code,message}}` 解析、`/search` 分页结构）、登录守卫与刷新恢复、Admin 专属界面按角色显隐、
  `rust-embed` 嵌入 + SPA 静态资源服务与客户端路由回退。
- 不做（范围外，属 P2/P3，避免镀金）：使用分析 / 访问下载统计富面板、七层防护管理 UI、漏洞标记 UI、
  用户组 / 细粒度权限动作、OIDC/LDAP 登录、S3 配置等。

## 3. 设计（怎么做）

### 前端工程

- 位置 `frontend/`：React + Vite + TypeScript + Mantine（UI 组件库），遵 ADR-0001。
- 包管理 pnpm；脚本 `build`（`tsc -b && vite build`）、`test`（vitest）、`lint`（eslint + prettier）。
- 目录：`src/api`（`client.ts` HTTP 客户端 + `endpoints.ts` 端点封装 + `types.ts` 契约类型）、
  `src/auth`（登录态上下文与守卫）、`src/pages`（各界面）、`src/components`（布局 / ACL 面板 / 错误条）、
  `src/lib`（展示辅助与通知）。
- API 客户端同源、走相对 `/api/v1`，不硬编码后端地址；开发期 Vite dev server 代理 `/api`、`/v2`、
  `/health` 到本地后端。

#### 契约对齐要点（以后端 `src/api/**` 真实返回为准，非理想化文档）

- 列表端点（users / repositories / artifacts / tokens / acl）返回**裸数组**；仅 `/search` 返回
  `{items,total,offset,limit,has_more}` 分页结构。
- `role` 为小写 `admin` / `user`；登录 / 刷新 / `/me` 返回 `{id,username,role}`。

#### 前端路由约束

后端格式 API 占用 catch-all `/{repo}/{*path}`（两段及以上）。为避免前端深链被格式路由拦截，前端
可深链路由一律为**单段路径**（`/login`、`/repositories`、`/users`、`/tokens`、`/search`），详情视图用
查询参数承载（`/repository?id=`、`/artifact?repo=&path=`），确保任意前端 URL 都回退到 SPA 入口。

### 后端嵌入与 SPA 服务

- 新增 `web` 模块（`src/web/mod.rs`）：经 `rust-embed` 在编译期嵌入 `frontend/dist`。
- 在 `api::build_router` 中，于 API / 格式 / Docker / 健康检查路由**之后**接入：`/assets/{*path}` 提供
  静态资源（按扩展名推断 Content-Type），`fallback` 把其余未匹配 GET 回退 `index.html`（前端客户端路由）。
- 健壮性（ADR-0001 构建顺序）：干净检出下 `frontend/dist` 仅有 `.gitkeep` 占位，无 `index.html`；此时
  `fallback` 返回 503 友好提示页，使后端可独立编译 / 测试。约定构建顺序「先 `pnpm -C frontend build`
  再 `cargo build`」。

## 4. 任务拆分

- [x] 前端工程脚手架（Vite + TS + Mantine + eslint/prettier/vitest）
- [x] API 客户端与契约类型（严格对齐后端真实返回）
- [x] 登录态上下文、守卫、刷新恢复
- [x] 五个界面（登录 / 仪表盘、仓库、用户 + ACL、Token、搜索 + 制品详情）
- [x] 前端关键单测（客户端鉴权 / 错误解析 / 401、登录页、展示辅助）
- [x] `web` 模块 `rust-embed` 嵌入 + SPA 路由接入，后端 SPA 服务测试
- [x] 文档同步：PRD 状态、ARCHITECTURE、CHANGELOG、本规格

## 5. 验收标准

- 前端 `pnpm -C frontend install && build` 成功；`test`（vitest）与 `lint`（eslint + prettier）全过。
- 后端 `cargo build` 成功；`cargo test` 全绿（含 web 模块 SPA 服务测试：`GET /` 返回 index.html 或
  未构建时 503 占位、未知前端路由回退 index、`/health` 与 `/api/v1` 与 `/v2/` 不被 SPA 拦截）；
  `cargo clippy --all-targets -- -D warnings` 无警告。
- 实跑（已自动化复验）：起二进制（临时数据目录 + env 引导 admin），`curl /` 返回真实 index.html、
  `curl /assets/<hash>.js|.css` 命中静态资源且 Content-Type 正确、`/health` 仍 200、未授权 `/api/v1/me`
  返回 401 JSON 而非被回退。
- **Web 控制台浏览器全流程（登录 → 建仓 → 授权 → 浏览）属需用户在浏览器实测确认项——待用户浏览器确认。**

## 6. 风险 / 待定

- 前端 / 后端构建有先后依赖：前端产物变更后须重新 `cargo build` 才生效（开发期可用 Vite dev server
  缓解）。已用 `.gitkeep` 占位 + 503 兜底保证干净检出可编译。
- 浏览器端完整交互（拖拽、复制、各表单提交链路）未纳入自动化 E2E，留用户浏览器确认。
