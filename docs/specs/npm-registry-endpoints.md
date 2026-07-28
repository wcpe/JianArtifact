# 功能规格：npm 标准 registry 端点补齐

> 状态：已交付　·　关联 PRD：FR-82　·　分支：feature/npm-registry-endpoints

## 1. 背景与目标

FR-15 已交付 npm 协议最小闭环（packument GET/HEAD、tarball GET/HEAD、publish PUT），真实 npm/pnpm 的 publish/install 已线上验证通过。但日常包管理命令（login/whoami/ping/dist-tag/unpublish/deprecate/search/audit）尚无对应端点，用户只能手写 .npmrc、无法删包改标签。本 FR 补齐 npm registry 标准端点，让真实客户端全部日常命令可用。属 P2，归当前 M1.5 滚动窗口。

## 2. 需求（要什么）

- 范围内（全部挂 registry 基址 `/npm/:repo/` 下）：
  1. `GET /-/ping` —— `npm ping`，返回 `{}`，无需认证
  2. `PUT /-/user/org.couchdb.user:{name}` —— `npm login/adduser`（legacy 流）：验证用户名+口令后**签发真 jat\_ API Token** 返回，npm 自动写 .npmrc；后台 token 列表可见可吊销
  3. `GET /-/whoami` —— 有主体返回 `{"username":...}`，匿名 401+WWW-Authenticate
  4. `GET/PUT/DELETE /-/package/{pkg}/dist-tags[/{tag}]` —— `npm dist-tag ls/add/rm`，读需 read、写需 write；`latest` 不可删
  5. `PUT /{pkg}/-rev/{rev}`、`DELETE /{pkg}/-rev/{rev}`、`DELETE /{pkg}/-/{file}/-rev/{rev}` —— `npm unpublish` 单版本与 `--force` 整包；**write 权限即可删**，hosted 限定（proxy/group 409）
  6. `npm deprecate` —— 复用现有 publish 合并路径（PUT 无 _attachments 写 versions[].deprecated），验证+补测试
  7. `GET /-/v1/search?text=&size=&from=` —— `npm search`，返回标准 objects/total 结构；hosted 搜本仓 packument，group 按成员合并（首见优先），proxy 仅已缓存
  8. `POST /-/npm/v1/security/advisories/bulk`、`POST /-/npm/v1/security/audits/quick` —— install 时 audit 兜底，返回空报告（本系统无漏洞库）
  9. packument GET 按 `Accept: application/vnd.npm.install-v1+json` 返回 abbreviated 文档（install 加速）
- 不做（范围外）：npm org/team/hooks/star/profile；web 登录流（`POST /-/v1/login` 保持 404，npm≥9 自动回落 legacy）；真实漏洞库；72 小时 unpublish 窗口限制。

## 3. 设计（怎么做）

改动集中于 `protocol/npm.go`（+拆出 `npm_registry.go` 放 registry 级端点），不引入新表/新 ADR。

**路由分发（前置修正）**：`RegisterNpmRoutes` 增挂 `POST`、`DELETE` 两个 method 到 `/:repo/*rest`。Get/Put/Post/Delete 入口先判 rest 是否以 `-/` 开头 → 分派到 registry 级端点表；否则走原 packument/tarball/publish 逻辑（修正 splitTarball 对 `-/` 开头 rest 误判为 packument 的坑）。未匹配的 `-/` 路径返回 404（含 `POST /-/v1/login`）。

**认证与鉴权复用**：

- 中间件不变（`authenticator.Optional()`），handler 内 `auth.PrincipalFrom(c)` 取主体、`h.authorize(c, repo, action)` 判仓库权限（401 带 WWW-Authenticate Basic 质询）。
- login：`NpmHandler` 注入 `auth.Store`（用 `PrincipalByPassword` 验口令，anonymous 禁登已内置）与 `*domain.TokenService`（`Create(userID, name)` 签发）；token 名 `npm login {username}@{date}`。响应 `201 {ok:true, id:"org.couchdb.user:<name>", token:"jat_..."}`；口令错误 401。构造函数签名变更同步 `main.go` 与 `protocol_test.go`。

**dist-tag**：读改 packument asset 的 `dist-tags` 键。GET 返回该 map；PUT body 为版本号 JSON 字符串，校验版本存在于 `versions`；DELETE 删键，`latest` 拒删（400）。写后覆盖存 packument。pkg 段支持 scoped（`@scope/pkg`，路径已由 net/http 解码）。

**unpublish**：

- `PUT /{pkg}/-rev/{rev}`：npm 删单版本流程——body 为剔除该版本后的完整 packument，**替换写**（非合并），随后客户端会单独 DELETE tarball。
- `DELETE /{pkg}/-/{file}/-rev/{rev}`：删单个 tarball asset（`AssetService.Delete`，blob 按既有约定不清理）。
- `DELETE /{pkg}/-rev/{rev}`：整包删除——删 packument asset + 遍历删除该包全部 tarball asset。
- 三者均 `authorize write`；repo.Type != hosted → 409（与 publish 语义一致）。

**search**：`authorize read` 通过后，复用 `RepositoryService.SearchAssets`（repoScope=本仓 + isAdmin=true 跳过二次 ACL——仓库级授权已由 authorize 判定），ExcludeTerms 过滤含 `/-/` 的 tarball 路径，命中 packument 后读文档提炼 `{name, version(latest), description, dist-tags…}` 组装 npm 标准响应；group 仓按成员迭代合并。

**audit**：固定返回 200 空报告（advisories/bulk → `{}`；audits/quick → 全零 metadata 结构），`authorize read`。

**abbreviated packument**：`servePackument` 检查 Accept 头含 `application/vnd.npm.install-v1+json` 时，按白名单裁剪（顶层 name/modified/dist-tags/versions；version 级 name/version/dependencies/optionalDependencies/peerDependencies/peerDependenciesMeta/bundleDependencies/bin/directories/dist/engines/deprecated/hasInstallScript/os/cpu），Content-Type 回写该媒体类型。

**契约**：协议端点豁免于 openapi.yaml（契约只管 /api/v1），不触发契约检查。

## 4. 任务拆分

- [x] 路由分发修正：POST/DELETE 挂载 + `-/` 前缀分派表 + splitTarball 误判修复
- [x] ping / whoami / login（含 NpmHandler 注入 Store+TokenService、main.go/测试装配同步）
- [x] dist-tag GET/PUT/DELETE
- [x] unpublish 三端点（rev PUT 替换写 + tarball DELETE + 整包 DELETE）+ hosted 限定
- [x] deprecate 合并路径测试补齐
- [x] search + audit 兜底
- [x] abbreviated packument
- [x] 单测：httpserver/protocol_test.go 风格覆盖各端点（含 401/403/409/scoped 包名）
- [x] 文档同步：PRD 状态、ARCHITECTURE、CHANGELOG
- [x] 部署线上 + 真机验收（20260724_npm1，npm 11.6.2 + pnpm 9.12.0 全命令通过；npm login 的 TTY 交互流受本机非 TTY 环境限制，以协议等价请求 + 签发 token 真机复用验证）

## 5. 验收标准

1. rest 以 `-/` 开头正确分流，不再误判 packument；`POST /-/v1/login` 404
2. `npm ping` 成功
3. `npm login`（npm≥9 web→legacy 回落）输入账号口令 → 签发 jat_ token 自动写 .npmrc；后台 token 列表可见可吊销；错口令 401
4. `npm whoami` 带 token 返回用户名，匿名 401
5. `npm dist-tag add/ls/rm` 全流程过，packument dist-tags 同步；latest 拒删
6. `npm unpublish pkg@ver` 删单版本（versions 移除+tarball 删）、`--force` 删整包；write 权限即可；proxy/group 409
7. `npm deprecate` 后 install 出弃用告警
8. `npm search` 命中本仓包名/描述；size/from 分页生效
9. install 时 audit 请求返回空报告，客户端无 audit 报错
10. Accept install-v1+json 返回 abbreviated packument，install 正常
11. **真机验收（需用户确认）**：线上 tmp.wcpe.top 用真实 npm + corepack pnpm 跑全部命令（测试绿 ≠ 真能用）

## 6. 风险 / 待定

- npm 客户端版本差异：login web→legacy 回落行为以线上真机验证为准（Verdaccio 同款方案，风险低）。
- 整包删除需遍历 tarball asset：按 `pkg/-/` 路径前缀查询删除，注意 scoped 包名转义一致性。
- blob 无 GC（既有约定）：unpublish 只删元数据，磁盘空间不回收——与 Raw DELETE 行为一致，不在本 FR 扩展。
