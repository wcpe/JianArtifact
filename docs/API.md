# 接口契约：JianArtifact

> 对外接口的单一真源。始终原地更新到当前契约。

## 1. 通用约定

- **协议形态**：管理 API 为 REST + JSON；格式 API 按各自原生协议（Maven、npm、Docker registry v2、Raw）暴露。
- **版本**：管理 API 统一挂载在 `/api/v1` 前缀下。
- **编码**：管理 API 请求体与响应体均为 `application/json; charset=utf-8`；格式端点的内容类型遵循各自协议（如制品二进制、清单 JSON 等）。
- **认证**：支持三种方式，由认证中间件统一识别。
  - **Bearer Token**：`Authorization: Bearer <token>`，供 CLI / 包管理器客户端使用，服务端以哈希形式比对（不存明文）。
  - **Basic Auth**：`Authorization: Basic <base64(用户名:密码或令牌)>`，兼容包管理器 CLI 的登录习惯（如 mvn / docker login）。
  - **Web 会话**：浏览器登录后通过会话凭据访问管理 API。
- **匿名访问**：未携带任何凭据即视为匿名访客，仅能读取 public 仓库；对 private 仓库一律拒绝。
- **身份解析**：认证中间件解析出身份（注册用户 / 管理员）或匿名；鉴权中间件按目标仓库与操作（读 / 写）综合 public/private、全局角色、每仓库 ACL 三者判定。
- **会话生命周期**：Web 会话 / JWT 有有限有效期（TTL，默认约 1 小时，可配置）；临近过期可经刷新端点续期，过期或吊销后须重新登录。API Token 不设过期（除非吊销），与会话相互独立。
- **分页与搜索约定**：所有返回列表的管理 API 统一支持分页与过滤参数，并返回统一的分页响应结构。
  - 请求参数：`offset`（默认 0）、`limit`（默认 50，上限如 1000）、可选 `sort`（如 `name:asc` / `created_at:desc`）、可选 `q`（按名称 / 路径关键字过滤）。
  - 响应结构：`{ "items": [...], "total": <总数>, "offset": <本页起点>, "limit": <本页容量>, "has_more": <是否还有更多> }`。
  - 本文 §3 各列表端点（用户、仓库、ACL、Token、制品浏览）均遵循该结构；下文“响应”中提到的“数组”即指 `items` 字段。

## 2. 错误约定

- **管理 API**：统一返回 JSON 错误结构，并配合 HTTP 状态码语义。

  ```json
  {
    "error": {
      "code": "字符串错误码",
      "message": "面向调用方的可读说明"
    }
  }
  ```

  - `400 Bad Request`：请求体或参数不合法。
  - `401 Unauthorized`：未认证，或凭据无效 / 已吊销 / 已过期。
  - `403 Forbidden`：已认证但无权执行该操作（角色或 ACL 不足）。
  - `404 Not Found`：资源不存在；私有仓库对未授权方亦可返回 404 以避免暴露存在性。
  - `409 Conflict`：资源冲突（如同名仓库 / 用户已存在）。
  - `5xx`：服务端内部错误。
- **格式 API**：错误遵循各自原生协议的约定（如 Docker registry v2 返回其规范的错误对象，Maven / npm / Raw 主要以 HTTP 状态码表达），不套用上述统一 JSON 错误结构。
- **私有仓库安全语义（定式）**：私有仓库对匿名 / 无有效凭据 / 已认证但无读权限者，一律返回 `404`（隐藏存在性，不用 `401`/`403` 以免暴露“仓库存在但需登录”）。已能读但越权写（有读无写）返回 `403`。管理 API 端点在缺失或无效凭据时返回 `401`。公开仓库匿名可读。

## 3. 端点 / 方法

### 登录

- **方法 / 路径**：`POST /api/v1/auth/login`
- **请求**：JSON 体 `{ "username", "password" }`。
- **响应**：认证成功后返回会话凭据（JWT 访问令牌、令牌类型 `Bearer` 与有效期 `expires_in` 秒）及当前用户信息（`id`、`username`、`role`）。会话令牌放 `Authorization: Bearer` 头使用（不走 Cookie）。
- **错误**：`400` 参数缺失；`401` 用户名或密码错误；`403` 用户已被禁用（`disabled`）；`429` 登录失败次数过多被限流（暴力破解防护，见 FR-65），响应错误码 `too_many_requests`。

### 登出

- **方法 / 路径**：`POST /api/v1/auth/logout`
- **请求**：无请求体，凭当前会话凭据调用。
- **响应**：清除当前会话，返回成功状态。
- **错误**：`401` 未认证。

### 刷新会话

- **方法 / 路径**：`POST /api/v1/auth/refresh`
- **请求**：凭当前有效会话 / 刷新凭据调用，无请求体。
- **响应**：续期会话 / 签发新的会话凭据，返回新的有效期信息。
- **错误**：`401` 会话已过期或无效（须重新登录）。

### 当前用户

- **方法 / 路径**：`GET /api/v1/me`
- **请求**：凭当前会话或 Bearer Token 调用，无请求体。
- **响应**：当前调用方信息（`id`、`username`、`role`），供 Web 控制台判定登录态与权限。
- **错误**：`401` 未认证。

### 列出用户

- **方法 / 路径**：`GET /api/v1/users`
- **请求**：无请求体。
- **响应**：用户数组，每项含 `id`、`username`、`role`、`disabled`、`created_at`（不返回 `password_hash`）。
- **错误**：`401` 未认证；`403` 非管理员。

### 创建用户

- **方法 / 路径**：`POST /api/v1/users`
- **请求**：JSON 体 `{ "username", "password", "role" }`，`role` 取值 `Admin` 或 `User`；口令以 argon2 哈希存储。
- **响应**：新建用户对象（`id`、`username`、`role`、`disabled`、`created_at`）。
- **错误**：`400` 参数不合法；`401` 未认证；`403` 非管理员；`409` 用户名已存在。

### 获取用户详情

- **方法 / 路径**：`GET /api/v1/users/{id}`
- **请求**：路径参数 `id`。
- **响应**：用户对象（`id`、`username`、`role`、`disabled`、`created_at`）。
- **错误**：`401` 未认证；`403` 非管理员；`404` 用户不存在。

### 更新用户

- **方法 / 路径**：`PATCH /api/v1/users/{id}`
- **请求**：路径参数 `id`；JSON 体可含 `role`、`disabled`（禁用 / 启用）等可变字段。
- **响应**：更新后的用户对象。
- **错误**：`400` 参数不合法；`401` 未认证；`403` 非管理员；`404` 用户不存在。

### 删除用户

- **方法 / 路径**：`DELETE /api/v1/users/{id}`
- **请求**：路径参数 `id`。
- **响应**：删除成功状态。
- **错误**：`401` 未认证；`403` 非管理员；`404` 用户不存在。

### 列出仓库

- **方法 / 路径**：`GET /api/v1/repositories`
- **请求**：无请求体。按调用方身份过滤可见仓库（匿名仅见 public）。
- **响应**：仓库数组，每项含 `id`、`name`、`format`、`type`（`hosted` / `proxy`）、`visibility`（`public` / `private`）、`upstream_url`（proxy 适用）、`created_at`。
- **错误**：`401` 未认证（仅在限定接口范围时）。

### 创建仓库

- **方法 / 路径**：`POST /api/v1/repositories`
- **请求**：JSON 体 `{ "name", "format", "type", "visibility", "upstream_url"?, "upstream_auth_ref"? }`。`type` 为 `hosted` 或 `proxy`；`visibility` 为 `public` 或 `private`；`upstream_url` 与 `upstream_auth_ref` 仅 `proxy` 适用，上游凭据真值不入库，DB 仅存引用 `upstream_auth_ref`。
- **响应**：新建仓库对象。
- **错误**：`400` 参数不合法；`401` 未认证；`403` 非管理员；`409` 仓库名已存在。

### 获取仓库详情

- **方法 / 路径**：`GET /api/v1/repositories/{id}`
- **请求**：路径参数 `id`。
- **响应**：仓库对象（字段同列表项）。
- **错误**：`401`/`404` 私有仓库对未授权方拒绝；`404` 仓库不存在。

### 更新仓库

- **方法 / 路径**：`PATCH /api/v1/repositories/{id}`
- **请求**：路径参数 `id`；JSON 体可含 `visibility`、`upstream_url`、`upstream_auth_ref` 等可配置字段。
- **响应**：更新后的仓库对象。
- **错误**：`400` 参数不合法；`401` 未认证；`403` 非管理员；`404` 仓库不存在。

### 删除仓库

- **方法 / 路径**：`DELETE /api/v1/repositories/{id}`
- **请求**：路径参数 `id`。
- **响应**：删除成功状态。
- **错误**：`401` 未认证；`403` 非管理员；`404` 仓库不存在。

### 浏览仓库制品

- **方法 / 路径**：`GET /api/v1/repositories/{id}/artifacts`
- **请求**：路径参数 `id`；可选查询参数用于路径前缀过滤 / 搜索。
- **响应**：制品索引数组，每项含 `path`、`size`、`sha256`、`content_type`、`cached`、`created_at`。
- **错误**：`401`/`404` 私有仓库对未授权方拒绝；`403` 无读权限；`404` 仓库不存在。

### 删除制品

- **方法 / 路径**：`DELETE /api/v1/repositories/{id}/artifacts/{path}`
- **请求**：路径参数 `id`（仓库 id）与 `path`（制品路径）。需对应仓库写权限或管理员。
- **响应**：删除成功状态（硬删除）。对 `hosted` 仓库删除制品本体与索引；对 `proxy` 仓库删除本地缓存，下次 cache-miss 会按需重新拉取上游。
- **错误**：`401`/`404` 私有仓库对未授权方拒绝；`403` 无写权限；`404` 仓库或制品不存在。

### 制品详情与使用方式

- **方法 / 路径**：`GET /api/v1/repositories/{id}/artifacts/{path}`
- **请求**：路径参数 `id`、`path`。受 public/private 与读 ACL 约束。
- **响应**：制品详情——`path`、`size`、`content_type`、`created_at`、各校验和（`sha256`、`sha1`、`md5`、`sha512`）、所属仓库与格式，以及按格式生成的“使用方式”片段（如 Maven `<dependency>`、`npm install`、`docker pull`、Raw URL/curl，及把客户端指向本仓库的接入配置）。
- **错误**：`401`/`404` 私有仓库对未授权方拒绝；`403` 无读权限；`404` 仓库或制品不存在。

### 跨仓库搜索

- **方法 / 路径**：`GET /api/v1/search`
- **请求**：查询参数 `q`（关键字 / 坐标）、可选 `format` / `repo` 过滤，及 §1 的分页参数。
- **响应**：分页的制品命中列表，每项含所属仓库、`path`、格式、`sha256` 等。**结果仅含调用方有读权限的仓库制品**（匿名仅含 public 仓库），不泄露无权私有仓库内容。
- **错误**：`400` 查询参数不合法。

### 列出仓库 ACL

- **方法 / 路径**：`GET /api/v1/repositories/{id}/acl`
- **请求**：路径参数 `id`（仓库 id）。
- **响应**：ACL 条目数组，每项含 `id`、`user_id`、`permission`（`read` 或 `write`）。
- **错误**：`401` 未认证；`403` 非管理员；`404` 仓库不存在。

### 新增仓库 ACL 条目

- **方法 / 路径**：`POST /api/v1/repositories/{id}/acl`
- **请求**：路径参数 `id`；JSON 体 `{ "user_id", "permission" }`，`permission` 为 `read` 或 `write`。
- **响应**：新建 ACL 条目对象。
- **错误**：`400` 参数不合法；`401` 未认证；`403` 非管理员；`404` 仓库或用户不存在；`409` 该用户的同类授权已存在。

### 移除仓库 ACL 条目

- **方法 / 路径**：`DELETE /api/v1/repositories/{id}/acl/{acl_id}`
- **请求**：路径参数 `id`（仓库 id）、`acl_id`（ACL 条目 id）。
- **响应**：删除成功状态。
- **错误**：`401` 未认证；`403` 非管理员；`404` 仓库或 ACL 条目不存在。

### 签发 API Token

- **方法 / 路径**：`POST /api/v1/tokens`
- **请求**：JSON 体 `{ "name" }`，为当前用户签发用于 CLI 的 Token。
- **响应**：新建 Token 元数据（`id`、`name`、`created_at`）及**仅本次返回的明文 Token 值**；服务端只保存其哈希（`token_hash`），此后不再可见。
- **错误**：`400` 参数不合法；`401` 未认证。

### 列出 API Token

- **方法 / 路径**：`GET /api/v1/tokens`
- **请求**：无请求体，返回当前用户自己的 Token。
- **响应**：Token 元数据数组，每项含 `id`、`name`、`created_at`、`last_used_at`、`revoked`（不返回明文与哈希）。
- **错误**：`401` 未认证。

### 吊销 API Token

- **方法 / 路径**：`DELETE /api/v1/tokens/{id}`
- **请求**：路径参数 `id`，吊销当前用户自己的 Token（将 `revoked` 置为真）。
- **响应**：吊销成功状态。
- **错误**：`401` 未认证；`403` 非本人 Token；`404` Token 不存在。

### 健康检查

- **方法 / 路径**：`GET /health`
- **请求**：无请求体，无需认证。
- **响应**：`200` 表示服务正常，返回简单状态体。
- **错误**：服务不可用时由进程层体现，不返回业务错误结构。

### 格式 API 概览

格式端点按各自原生协议挂载，路径中包含仓库名以定位目标仓库；写操作校验对应仓库的写权限，读操作受 public/private 与读 ACL 约束。`hosted` 仓库支持制品直传与下载，`proxy` 仓库在 cache-miss 时从上游拉取、校验、落盘并写索引（并发请求单飞合并）。

**上传限制**：各格式上传受可配置的单文件大小上限约束，超限返回 `413 Payload Too Large`；上传走流式处理，不整体载入内存。

**覆盖 / 不可变策略**（同名版本 / 路径重复上传时）：

- Maven：release 版本不可覆盖（重复上传同 GAV 的 release 返回 `409 Conflict`）；snapshot 版本允许覆盖。
- npm：已发布版本不可覆盖（重复发布同版本返回 `409`）。
- Docker：同一 tag 允许覆盖（符合 Docker 习惯），manifest 按 digest 寻址与去重。
- Raw：同路径文件允许覆盖。

**校验和**：每个制品计算并提供 sha256 / sha1 / md5 / sha512；按各格式约定暴露 sidecar（如 Maven 的 `.sha1` / `.md5` / `.sha256` 伴随文件），下载方可据以校验完整性。sha1 / md5 主要为客户端兼容，安全完整性以 sha256 及以上为准。

各格式详细协议规格在开发该格式时落到 `docs/specs/`，此处只定覆盖语义与状态码。

- **Maven 格式**：以 Maven 仓库布局暴露，路径形如 `/{仓库名}/{groupId 路径}/{artifactId}/{version}/...`，供 `mvn deploy` / `mvn` 拉取使用；按 Maven 协议处理制品与校验和（sha256 索引）。
- **npm 格式**：以 npm registry 协议暴露，路径形如 `/{仓库名}/{包名}`、`/{仓库名}/{包名}/-/{tarball}`，供 `npm publish` / `npm install` 使用。
- **Docker / OCI 格式**：以 Docker registry v2 协议暴露，挂载于 `/v2/`，路径含仓库名与镜像名（如 `/v2/{仓库名}/{镜像名}/manifests/{ref}`、`/v2/{仓库名}/{镜像名}/blobs/{digest}`），供 `docker push` / `docker pull` 使用；错误遵循 registry v2 原生错误格式。
- **Raw 通用文件格式**：以路径直存直取暴露，路径形如 `/{仓库名}/{任意文件路径}`，支持 `curl PUT` / `curl GET`，流式上传下载，大文件不整体载入内存。

## 4. P2 规划端点（当前未实现，仅记录契约方向）

以下端点为后续分期（P2）能力，**当前形态不提供、不预留占位**；此处列出以便接口演进时对齐方向。

### 权限增强（扩展 ACL）

- 用户组/团队：`/api/v1/groups` 的 CRUD，并支持把组作为仓库 ACL 的授权主体。
- 仓库 ACL 扩展：授权主体从用户扩展为"用户或组"，`permission` 从 `read` / `write` 扩展为 `read` / `write` / `delete` / `admin`。

### 七层防护管理

- 防护策略管理（管理员）：`/api/v1/admin/protection` 下配置限流阈值、并发 / 连接上限、WAF 规则、IP 黑白名单与封禁列表。
- CC 挑战：对触发挑战的请求返回挑战质询，并提供校验端点，完成人机校验后放行。

### 使用分析

- 使用统计查询：`/api/v1/stats`（或 `/api/v1/analytics`）返回访问量、下载量、热门制品、仓库用量等聚合数据，供数据面板展示；数据本机内部、不外发。

### 漏洞（P2）

- 制品漏洞状态：制品详情与搜索结果附带漏洞标记（基于本地漏洞库离线镜像 + 坐标级匹配）；可查某制品命中的公告列表（如 `GET /api/v1/repositories/{id}/artifacts/{path}/vulnerabilities`）。制品坐标本地匹配、不外发。
