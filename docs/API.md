# 接口契约：JianArtifact

> 对外接口的单一真源。始终原地更新到当前契约。

## 1. 通用约定

- **协议形态**：管理 API 为 REST + JSON；格式 API 按各自原生协议（Maven、npm、Docker registry v2、Go GOPROXY、Raw、PyPI Simple Repository API）暴露。
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
- **格式 API**：错误遵循各自原生协议的约定（如 Docker registry v2 返回其规范的错误对象，Maven / npm / Raw / PyPI 主要以 HTTP 状态码表达），不套用上述统一 JSON 错误结构。
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
- **请求**：查询参数 `q`（关键字 / 坐标，必填且非空）、可选 `format` 过滤，及 `offset` / `limit` 分页参数。
- **响应**：统一分页结构 `{ items, total, offset, limit, has_more }`，每项含所属仓库（`repo_id`、`repo_name`、`format`）、`path`、`sha256`、`size`、`created_at`。**结果仅含调用方有读权限的仓库制品**（匿名仅含 public 仓库）；`total` 与 `items` 均按读权限过滤后计数，不泄露无权私有仓库内容。
- **错误**：`400` 查询参数不合法（如 `q` 为空）。

### 查询审计日志（P2，仅 Admin）

- **方法 / 路径**：`GET /api/v1/audit`
- **请求**：查询参数均可选——`action`（按动作过滤，如 `login` / `repo.create` / `artifact.upload`）、`target_repo`（按仓库名过滤）、`actor`（按主体用户名过滤），及 `offset` / `limit` 分页参数（默认 `offset=0`、`limit=50`，上限 1000）。仅管理员可访问。
- **响应**：统一分页结构 `{ items, total, offset, limit, has_more }`，按时间倒序（最新在前）。每项含 `id`、`ts`、`actor`、`actor_kind`（`session` | `token` | `basic` | `anonymous`）、`request_id`、`source_ip`、`action`、`target_repo`、`target`、`result`（`success` | `denied` | `error`）、`detail`。审计只记元数据级安全 / 管理事件，**绝不含密码 / Token / JWT / 上游凭据**（FR-31，ADR-0015）。
- **错误**：`401` 未认证；`403` 非管理员。

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
- NuGet：已发布版本不可覆盖（重复 `nuget push` 同 id+version 返回 `409`，NuGet 默认 server policy）。
- Docker：同一 tag 允许覆盖（符合 Docker 习惯），manifest 按 digest 寻址与去重。
- Go：模块版本一经发布即不可变（重复上传同 `{module}@{version}` 的 `.mod` / `.zip` / `.info` 返回 `409`）。
- PyPI：已发布发行文件不可覆盖（重复上传同 `packages/{规范名}/{文件}` 返回 `409`）。
- Raw：同路径文件允许覆盖。
- Cargo：已发布版本不可覆盖（重复发布同 `name`+`vers` 返回 `409`）；索引文件随新版本追加更新；yank/unyank 仅翻转索引 `yanked` 标记、不删 `.crate`。

**校验和**：每个制品计算并提供 sha256 / sha1 / md5 / sha512；按各格式约定暴露 sidecar（如 Maven 的 `.sha1` / `.md5` / `.sha256` 伴随文件），下载方可据以校验完整性。sha1 / md5 主要为客户端兼容，安全完整性以 sha256 及以上为准。

各格式详细协议规格在开发该格式时落到 `docs/specs/`，此处只定覆盖语义与状态码。

- **Maven 格式**：以 Maven 仓库布局暴露，路径形如 `/{仓库名}/{groupId 路径}/{artifactId}/{version}/...`，供 `mvn deploy` / `mvn` 拉取使用；按 Maven 协议处理制品与校验和（sha256 索引）。
- **npm 格式**：以 npm registry 协议暴露，路径形如 `/{仓库名}/{包名}`、`/{仓库名}/{包名}/-/{tarball}`，供 `npm publish` / `npm install` 使用。
- **NuGet 格式**：以 NuGet v3 协议暴露，供 `dotnet nuget push` / `dotnet add package` 使用。客户端 source 配 `/{仓库名}/v3/index.json`。
  - 服务索引 `GET /{仓库名}/v3/index.json`：列出本仓库 v3 资源（扁平容器 `PackageBaseAddress/3.0.0`、发布端点 `PackagePublish/2.0.0`），`@id` 指向本仓库对应端点；`proxy` 仓库回源上游服务索引后把扁平容器 `@id` 重写为指向本仓库。
  - 扁平容器版本列表 `GET /{仓库名}/v3-flatcontainer/{id}/index.json`：返回该包所有已发布版本 `{"versions":[...]}`；`hosted` 由元数据索引动态生成，`proxy` 回源上游。
  - 下载 `GET /{仓库名}/v3-flatcontainer/{id}/{version}/{id}.{version}.nupkg`（及同目录 `{id}.nuspec`）：流式返回；`proxy` cache-miss 回源缓存、命中不回源。id 与 version 按 NuGet 约定小写规范化。
  - 发布 `PUT /{仓库名}/v3/package`（`nuget push`）：`multipart/form-data` 内含 .nupkg；服务端解压读取内嵌 `.nuspec` 解析 id / version，先落 .nupkg 再落 .nuspec。鉴权经 `Authorization: Basic`（用户口令或 API Token 作密码字段）。
- **Docker / OCI 格式**：以 Docker registry v2 协议暴露，挂载于 `/v2/`，路径含仓库名与镜像名（如 `/v2/{仓库名}/{镜像名}/manifests/{ref}`、`/v2/{仓库名}/{镜像名}/blobs/{digest}`），供 `docker push` / `docker pull` 使用；错误遵循 registry v2 原生错误格式。

  **认证（Bearer 令牌流）**：遵循 registry v2 的"挑战-应答"令牌流。

  - **探活发起发现**：`GET /v2/` 未带凭据时返回 `401 + WWW-Authenticate: Bearer realm="{基址}/v2/token",service="jianartifact"`（不带 scope），让客户端在探活阶段发现令牌 realm；带凭据 / 令牌时返回 `200` 与版本头。
  - **受保护操作质询**：受保护的 docker 操作在未认证时返回 `401 + WWW-Authenticate: Bearer realm="{基址}/v2/token",service="jianartifact",scope="repository:{仓库名}/{镜像名}:{动作}"`（写 = `pull,push`，读 = `pull`）。客户端据此到令牌端点换取范围令牌后，以 `Authorization: Bearer <token>` 重试原请求。
  - **令牌端点** `GET /v2/token`：查询参数 `service`、`scope`（形如 `repository:{name}:{actions}`，`actions` 逗号分隔，可多个 `scope`）、可选 `account`。以 `Authorization: Basic`（用户口令或 API Token）认证——无凭据按匿名、提供但无效则 `401`。对每个 `scope` 逐项判定授权，仅把通过的动作放进该 `scope` 的授予集合（仓库不存在或全拒 → 该 `scope` 授予空，不报错）。响应 `200`：`{"token","access_token","expires_in","issued_at"}`，`token` 为短期 Bearer 令牌。
  - **兼容路径**：匿名拉取 public 仓库无需用户凭据——客户端据 `/v2/` 质询透明换取仅含 public `pull` 的匿名令牌即可拉取；预先携带 `Authorization: Basic` 的请求（如 `curl -u`）继续直接生效，无需自行走令牌流。
- **Go 模块格式**：以 Go 模块代理协议（GOPROXY）暴露，路径形如 `/{仓库名}/{模块路径}/@v/...`，供客户端配置 `GOPROXY=http://host/{仓库名}` 后 `go mod download` / `go get` 使用。模块路径中的大写字母按 GOPROXY 约定用 bang 编码表达（如 `GitHub.com/Foo` → `!git!hub.com/!foo`）。

  - **版本列表** `GET /{仓库名}/{模块路径}/@v/list`：返回该模块所有版本，每行一个版本号（`text/plain`）；无版本返回空 `200`。
  - **版本元信息** `GET /{仓库名}/{模块路径}/@v/{version}.info`：返回 JSON `{"Version":"v1.2.3","Time":"<RFC3339>"}`。
  - **go.mod** `GET /{仓库名}/{模块路径}/@v/{version}.mod`：返回该版本 go.mod 文本（`text/plain`）。
  - **模块 zip** `GET /{仓库名}/{模块路径}/@v/{version}.zip`：返回模块 zip（内部布局 `{module}@{version}/...`，`application/zip`）。
  - **最新版本** `GET /{仓库名}/{模块路径}/@latest`：返回最新版本的 info JSON（按语义版本排序取最大；hosted 据已存版本，proxy 回源上游）。
  - **hosted 上传约定**（Go 无原生 publish，本项目据下载端点对称定义）：`PUT /{仓库名}/{模块路径}/@v/{version}.{mod|zip|info}` 上传对应文件；`.info` 可不传，服务端在取 `.info` / `@v/list` / `@latest` 时按已存 `.mod` 视为版本存在并以其 `created_at` 合成 `Time`。
  - **proxy**：`.mod` / `.zip` / `.info` 走 cache-miss → 回源 → 校验 → 落盘 → 写索引、命中不回源、并发单飞合并；`@v/list` 与 `@latest` 为易变聚合文档，每次回源透传不缓存。
- **Raw 通用文件格式**：以路径直存直取暴露，路径形如 `/{仓库名}/{任意文件路径}`，支持 `curl PUT` / `curl GET`，流式上传下载，大文件不整体载入内存。
- **Cargo 格式**：以 Cargo 稀疏索引（sparse registry）协议暴露，供 `cargo publish` / 依赖解析 / 下载使用。
  - **registry 配置** `GET /{仓库名}/config.json`：返回 `{"dl":"{基址}/{仓库名}/api/v1/crates","api":"{基址}/{仓库名}"}`，把下载与 API 都指回本仓库（proxy 同样指回本仓库）。
  - **稀疏索引** `GET /{仓库名}/{索引路径}`：返回某包索引文件（每行一个版本的 JSON：`name`/`vers`/`deps`/`cksum`=sha256(.crate) hex/`features`/`yanked`）。索引路径按包名长度分目录（1→`1/{name}`、2→`2/{name}`、3→`3/{name[0]}/{name}`、≥4→`{name[0..2]}/{name[2..4]}/{name}`，均小写）；proxy cache-miss 回源上游索引（不缓存，索引易变）。
  - **下载** `GET /{仓库名}/api/v1/crates/{name}/{version}/download`：返回 `.crate` 字节，proxy cache-miss 回源并缓存、命中不回源。
  - **发布** `PUT /{仓库名}/api/v1/crates/new`：请求体为二进制（4 字节 LE 长度前缀 + metadata JSON + 4 字节 LE 长度前缀 + `.crate` 字节）；落 `.crate` 得 sha256、把该版本追加进索引，返回 `{"warnings":{"invalid_categories":[],"invalid_badges":[],"other":[]}}`。同 `name`+`vers` 已发布不可覆盖（重复发布返回 `409`）。
  - **yank / unyank** `DELETE /{仓库名}/api/v1/crates/{name}/{version}/yank`（置 `yanked=true`）、`PUT /{仓库名}/api/v1/crates/{name}/{version}/unyank`（置 `yanked=false`）：翻转索引行的 `yanked` 标记、不删 blob，返回 `{"ok":true}`。
  - **认证**：以 `Authorization: Bearer <API Token>` 鉴权；发布 / yank 需写权限，读受 visibility / ACL，private 对无权一律 404。
- **PyPI 格式**：以 PyPI Simple Repository API 暴露，供 `twine upload` / `pip install` 使用。
  - **Simple 索引**：`GET /{仓库名}/simple/` 列项目、`GET /{仓库名}/simple/{规范名}/` 列发行文件，默认返回 PEP503 HTML（每文件 `href` 带 `#sha256=`）；带 `Accept: application/vnd.pypi.simple.v1+json` 时返回 PEP691 JSON。项目名按 PEP503 规范化（小写、`[-_.]+` 折叠为单个 `-`）。
  - **上传（twine）**：`POST /{仓库名}/`，`multipart/form-data`（`:action=file_upload`、`content` 文件、`name`、`version`、可选 `sha256_digest`）；落 wheel/sdist 于 `packages/{规范名}/{文件}`，提供 `sha256_digest` 时与服务端算得的 sha256 对账，不符 `400`。
  - **下载**：`GET /{仓库名}/packages/{规范名}/{文件}` 流式返回；`proxy` 仓库 cache-miss 时从上游解析文件 URL 后回源并缓存。
  - **proxy 上游约定**：`upstream_url` 指向索引服务主机根（如 `https://pypi.org`），服务端按 `simple/...` 回源；Simple 页面每次回源（索引不缓存）并把文件链接重写为本仓库 `packages/...` 路径，仅包文件走缓存。本服务不提供 PEP658/714 `.metadata` sidecar，代理重写时剥除上游 `data-core-metadata` / `data-dist-info-metadata` 属性，使 pip 回退为下载完整 wheel。

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
