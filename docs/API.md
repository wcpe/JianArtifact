# 接口契约：JianArtifact

> 对外接口的单一真源。始终原地更新到当前契约。

## 1. 通用约定

- **协议形态**：管理 API 为 REST + JSON；格式 API 按各自原生协议（Maven、npm、Docker registry v2、Go GOPROXY、Raw、PyPI Simple Repository API）暴露。
- **版本**：管理 API 统一挂载在 `/api/v1` 前缀下。
- **编码**：管理 API 请求体与响应体均为 `application/json; charset=utf-8`；格式端点的内容类型遵循各自协议（如制品二进制、清单 JSON 等）。
- **认证**：支持以下方式，由认证中间件统一识别。
  - **Bearer Token**：`Authorization: Bearer <token>`，供 CLI / 包管理器客户端使用，服务端以哈希形式比对（不存明文）。
  - **Basic Auth**：`Authorization: Basic <base64(用户名:密码或令牌)>`，兼容包管理器 CLI 的登录习惯（如 mvn / docker login）。
  - **Web 会话**：浏览器登录后通过会话凭据访问管理 API。
  - **NuGet api-key 头**：兼容 NuGet 规范的 `X-NuGet-ApiKey: <API Token>`（`dotnet nuget push` 原生方式）；仅在无 `Authorization` 头时回退按 API Token 校验该头值，非法值仍按匿名处理。
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
  - `403 Forbidden`：已认证但无权执行该操作（角色或 ACL 不足）；启用 IP 黑名单 / 自动封禁（`[protection.ip_list]` / `[protection.ban]`，FR-53 / ADR-0008）后，命中黑名单或处于封禁中的来源 IP 在进入业务前即被拒 `403`（错误码 `forbidden`），与认证 / 鉴权无关、对**任意端点**生效。
  - `404 Not Found`：资源不存在；私有仓库对未授权方亦可返回 404 以避免暴露存在性。
  - `409 Conflict`：资源冲突（如同名仓库 / 用户已存在）。
  - `429 Too Many Requests`：请求过于频繁被限流，错误码 `too_many_requests`，响应携带 `Retry-After`（建议等待秒数；并发上限触顶时无固定重试窗、不带该头）。除登录暴力破解防护（见 FR-65）外，启用多维速率限制（`[protection.rate_limit]`，FR-33 + FR-51 / ADR-0008）后**任意端点**都可能在 IP / 用户 / 仓库维度单窗超阈值或超在途并发上限时返回 429；默认关闭，启用与阈值由运维承担。
  - `5xx`：服务端内部错误。
- **速率限制（全局，FR-33 + FR-51 / ADR-0008）**：启用后，限流中间件按**连接来源 IP**、**已认证身份/用户**（用户及其所有 Token / 会话）与**目标仓库**（按格式路径首段仓库名）分别在固定时间窗内计数，任一维度超阈值即对该请求返回 `429`（带 `Retry-After`），不进入业务；并按 IP / 用户 / 仓库限制**在途并发请求数**，任一维度超并发上限同样返回 `429`（并发上限触顶不带 `Retry-After`），占用的名额在请求结束后可靠归还。来源 IP 取连接级地址、**不采信 `X-Forwarded-For`**（伪造来源不绕过）。默认阈值保守、新增的仓库与并发维度默认不启用，不影响正常包管理器批量并发拉取。
- **IP 黑/白名单与自动封禁（全局，FR-53 / ADR-0008）**：启用后，置于热路径前端的中间件按**连接来源 IP** 判定：命中黑名单（`[protection.ip_list].deny`，IP / CIDR）或处于自动封禁中（启用 `[protection.ban]` 后，单 IP 一窗内 4xx 异常信号达阈值即封禁一个时长）的来源，对**任意端点**直接返回 `403`（错误码 `forbidden`），不进入业务；封禁到期自动解封。命中白名单（`[protection.ip_list].allow`）的来源**豁免一切应用层防护**（限流 / 封禁 / 异常统计），优先级高于黑名单。来源 IP 取连接级地址、**不采信 `X-Forwarded-For`**（伪造来源不绕过）。默认关闭、阈值保守宽放，正常包管理器偶发 404 / 鉴权重试不触顶。
- **慢速攻击防护与通用请求体大小限制（全局，FR-52 / ADR-0008）**：启用后（`[protection.slowloris]`，默认关闭），慢速防护中间件对**任意端点**的请求体设超时与通用大小上限：等待首个数据块超过 `header_timeout_secs`、或相邻数据块间隔超过 `body_read_timeout_secs`（**块间空闲超时**，非整体时长）即判为慢速连接并断开；请求体超过通用上限 `max_body_bytes`（>0 时启用）则返回 `413 Payload Too Large`（带 `Content-Length` 时在进入业务前即拒）。该通用体上限区别于各格式上传的 `limits.max_artifact_size`（仅约束制品上传体）。超时按块间空闲判定，**不影响正常大文件流式上传**（持续发数据即不触发）。L3/L4 体积型攻击仍由前置反向代理 / CDN / WAF 承担。
- **CC 挑战 / 工作量证明 PoW（全局，FR-54 / ADR-0008）**：启用后（`[protection.cc_challenge]`，**默认关闭**），CC 挑战中间件对疑似 CC（HTTP 洪水）攻击的**匿名**请求要求工作量证明：无 / 错误证明时对**任意端点**返回 `429 Too Many Requests`（错误码 `cc_challenge_required`），响应体携带挑战参数——客户端须找到 `nonce` 使 `sha256(challenge_token + ":" + nonce)` 的二进制前导零位数达 `difficulty`，再以请求头 `X-CC-Solution: <challenge_token>:<nonce>` 重发原请求方放行。挑战令牌服务端 HMAC 无状态签名、绑定**连接级来源 IP**（**不采信 `X-Forwarded-For`**，换 IP 的证明不可复用）+ 难度 + 签发时刻，超 `ttl_secs` 过期。**默认豁免已认证（Bearer / Basic / 会话）请求**（`exempt_authenticated=true`），挑战只面向匿名可疑流量。⚠️ 正常包管理器 CLI 不会解 PoW，启用后会拦截匿名拉取——见 OPERATIONS / CONFIG 的误伤提示，仅在确有 CC 攻击时由运维开启。响应体示例：
- **可配置 WAF 规则引擎（全局，FR-55 / ADR-0008）**：启用后（`[protection.waf]`，默认关闭），置于热路径前端的 WAF 中间件对**任意端点**按有序规则匹配请求属性——`field` 取 `method` / `path` / `query` / `header`（`header` 须配 `header_name`，大小写不敏感），`match_type` 取 `literal`（子串包含）/ `wildcard`（`*` 任意多字符、`?` 任意单字符，整体匹配）/ `regex`（正则子串搜索）。规则**按声明顺序、首个命中生效**：命中 `action="block"` 的请求对**任意端点**直接返回 `403`（错误码 `forbidden`，统一 JSON 错误体），不进入业务；命中 `action="allow"` 即放行并短路后续规则（用于给合法模式开豁免口子）；无命中亦放行。WAF 按请求属性匹配、**与来源 IP 无关**（不采信 `X-Forwarded-For`）。非法规则启动时记 WARN 跳过、不阻断启动。**默认空规则集 + 关闭**，不影响正常包管理器请求。L3/L4 体积型攻击仍由前置反向代理 / CDN / WAF 承担。
  ```json
- **私有仓库安全语义（定式）**：私有仓库对匿名 / 无有效凭据 / 已认证但无读权限者，一律返回 `404`（隐藏存在性，不用 `401`/`403` 以免暴露“仓库存在但需登录”）。已能读但越权写（有读无写）返回 `403`。管理 API 端点在缺失或无效凭据时返回 `401`。公开仓库匿名可读。

## 3. 端点 / 方法

### 登录

- **方法 / 路径**：`POST /api/v1/auth/login`
- **请求**：JSON 体 `{ "username", "password" }`。
- **响应**：认证成功后返回会话凭据（JWT 访问令牌、令牌类型 `Bearer` 与有效期 `expires_in` 秒）及当前用户信息（`id`、`username`、`role`）。会话令牌放 `Authorization: Bearer` 头使用（不走 Cookie）。
- **错误**：`400` 参数缺失；`401` 用户名或密码错误；`403` 用户已被禁用（`disabled`）；`429` 登录失败次数过多被限流（暴力破解防护，见 FR-65），响应错误码 `too_many_requests`。
- **LDAP 登录（P2，FR-35 / ADR-0016）**：配置 `[auth.ldap]` 后，本地口令未命中时本端点委托 LDAP 做 bind 校验（见下「LDAP 登录」）；bind 成功经「外部身份 → 本地用户」映射得本地用户后照常签发会话 JWT。bind 失败 / 目录不可达 / JIT 关闭且无对应本地用户，统一记一次登录失败并返回 `401`（不泄露细节、不区分原因）。

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

### OIDC 登录（P2，FR-34 / ADR-0016）

- **方法 / 路径**：`GET /api/v1/auth/oidc/login`
- **请求**：浏览器直接访问，无请求体。
- **响应**：`303` 重定向到 IdP 授权端点（携带 `state`、`nonce`、PKCE `code_challenge`（S256）等参数）。
- **错误**：`404` 未配置 OIDC（`[auth.oidc]` 缺失，端点视为不存在）；`502` IdP discovery 不可达；`429` 登录流程暂存表已满。

### OIDC 回调（P2，FR-34 / ADR-0016）

- **方法 / 路径**：`GET /api/v1/auth/oidc/callback`
- **请求**：IdP 重定向回调，查询参数含 `code`、`state`（或 `error`）。服务端校验 `state`（一次性、防 CSRF/重放），用 `code` 换 ID Token 并校验签名（JWKS）/`iss`/`aud`/`exp`/`nonce`，经「外部身份 → 本地用户」映射得本地用户。
- **响应**：认证成功后**照常签发既有会话 JWT**，`303` 回跳前端 `/login`，会话令牌经 URL fragment（`#access_token=...&token_type=Bearer`）交给 SPA。
- **错误**：`404` 未配置 OIDC；`400` 缺 `code`/`state`；`401` `state` 校验失败、ID Token 校验失败、IdP 回错，或外部身份无对应本地用户（JIT 关闭，`auto_provision=false`）/ 绑定用户已禁用。
- **JIT 即时开通**：`auto_provision=false`（默认）时无对应本地用户一律拒绝（守不自助注册红线，ADR-0010）；显式开启时首次外部登录即时建用户，**默认角色固定为 `User`，绝不自动 `Admin`**。

### LDAP 登录（P2，FR-35 / ADR-0016）

- **无独立端点**：LDAP 不新增端点，经既有口令型登录入口接入——Web 表单 `POST /api/v1/auth/login`、Basic Auth（`Authorization: Basic`，含 Docker v2 令牌端点 `GET /v2/token`）。仅当配置 `[auth.ldap]` 时启用。
- **校验流程**：口令通道在本地口令 / API Token 均未命中时委托 LDAP——服务账号（`bind_dn` + bind 口令）连接目录，按 `user_search_base` + 过滤模板（`{username}` 占位，RFC 4515 转义防注入）搜出唯一用户 DN，再用该 DN + 用户提交口令做一次 bind；bind 成功即认证通过，外部 `subject` 取用户 DN，经「外部身份 → 本地用户」映射得本地用户后照常签发既有会话 / 收敛为本地身份。
- **传输安全**：连接走 LDAPS / StartTLS（TLS 由 rustls/ring 提供）；默认拒绝明文 `ldap://`（除非运维显式 `allow_insecure`，限可信内网）。
- **失败语义**：bind 失败 / 目录不可达 / 无唯一匹配 / JIT 关闭且无对应本地用户，Web 表单返回 `401`（并计一次登录失败），Basic Auth 通道回退匿名（受保护端点据私有仓库语义返回 `401`/`404`）；均不泄露目录存在性与失败原因。
- **JIT 即时开通**：与 OIDC 同——`auto_provision=false`（默认）无对应本地用户即拒（守 ADR-0010），开启时即时建用户默认角色固定 `User`、绝不自动 `Admin`。
- **真机互通**：对接 AD / OpenLDAP 的端到端 bind 待真机验（需 LDAP 目录）。

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

### 目录列表 / 仓库索引（FR-75，Accept 驱动双形态）

- **方法 / 路径**：`GET /{repo}/`（仓库根）或 `GET /{repo}/{dir}/`（子目录）。以**路径尾斜杠**作为目录请求信号；无尾斜杠仍为单文件下载。仅通用格式（`raw` / `maven` 等经统一 trait 落库者）参与；`npm` / `docker` / `cargo` / `pypi` / `nuget` / `go` 等原生协议格式不走此目录浏览（其尾斜杠语义由各自协议处理）。
- **内容协商**：按 `Accept` 头返回双形态——
  - `Accept: application/json`（或不带偏好）：返回 `{ repo, path, entries }`。`entries` 为当前目录一层条目（目录在前、文件在后，各自名称升序），每项含 `name`、`type`（`folder` / `file`）；文件项另含 `size`、`sha256`、`created_at`。**只下钻一层**，不扁平铺开整棵子树。
  - `Accept: text/html`（浏览器）：返回类 Apache 目录索引的 HTML 页（条目链接、类型、大小）；文件名 / 路径经 HTML 转义防注入。
- **鉴权**：受 public/private 与读 ACL 约束，三身份通道（Basic / Bearer / 会话）一致；**私有仓库对匿名 / 无权一律 404**，JSON 与 HTML 两形态均不泄露资源存在性。结果按读权限过滤。
- **错误**：`401`/`404` 私有仓库对未授权方拒绝；`404` 仓库不存在 / 无读权限。

### 删除制品

- **方法 / 路径**：`DELETE /api/v1/repositories/{id}/artifacts/{path}`
- **请求**：路径参数 `id`（仓库 id）与 `path`（制品路径）。需对应仓库写权限或管理员。
- **响应**：删除成功状态（硬删除）。对 `hosted` 仓库删除制品本体与索引；对 `proxy` 仓库删除本地缓存，下次 cache-miss 会按需重新拉取上游。
- **错误**：`401`/`404` 私有仓库对未授权方拒绝；`403` 无写权限；`404` 仓库或制品不存在。

### 制品详情与使用方式

- **方法 / 路径**：`GET /api/v1/repositories/{id}/artifacts/{path}`
- **请求**：路径参数 `id`、`path`。受 public/private 与读 ACL 约束。
- **响应**：制品详情——`path`、`size`、`content_type`、`created_at`、各校验和（`sha256`、`sha1`、`md5`、`sha512`）、所属仓库与格式，按格式生成的“使用方式”片段（如 Maven `<dependency>`、`npm install`、`docker pull`、Raw URL/curl，及把客户端指向本仓库的接入配置），以及 `vulnerabilities` 数组（FR-71）：该制品命中的已知漏洞公告，每项含 `id`（如 GHSA / CVE）、`severity`（可空）、`summary`（可空）。基于本地漏洞库离线镜像（FR-70）按制品生态坐标 `(ecosystem, package, version)` 做坐标级本地匹配，**制品坐标绝不外发到外部漏洞服务**；无标准生态坐标的格式（Raw / Docker）或未命中时为空数组 `[]`。
- **错误**：`401`/`404` 私有仓库对未授权方拒绝；`403` 无读权限；`404` 仓库或制品不存在。

### 通用制品上传（FR-73）

- **方法 / 路径**：`POST /api/v1/repositories/{id}/upload`
- **用途**：Web 控制台统一上传入口——无需各格式原生客户端，经 `multipart/form-data` 向 **hosted** 仓库直传制品。仅支持 **Maven / npm / Raw** 三格式；`proxy` 仓库与其余格式拒绝。
- **请求**：路径参数 `id`（仓库 id）；需对应仓库写权限或管理员。`multipart/form-data` 体含：
  - `file`：上传文件字段（含文件名），承载制品字节（必填）。
  - 按目标仓库格式区分的坐标字段：
    - **Maven**：`group_id` / `artifact_id` / `version`；存储路径 = `{group 点转斜杠}/{artifact}/{version}/{上传文件名}`。服务端为该主构件**自动补齐四校验和 sidecar**（`.sha1` / `.md5` / `.sha256` / `.sha512`，内容为对应摘要的小写十六进制）——服务端上传无客户端逐文件 PUT 的 sidecar，补齐后产出制品与 `mvn deploy` 一致、可被官方客户端独立取回校验和并校验。此外（FR-121，ADR-0037）服务端**权威生成派生文件**使网页上传产出完整 Maven 制品：缺 pom 时**三级兜底**生成 `{artifact}-{version}.pom`（jar 内嵌 `META-INF/maven/.../pom.xml` 原样提取 → 按 GAV 生成最小 pom），并按 SQLite 索引聚合该 `{groupId}/{artifactId}` 全部版本**重生成 artifact 级 `maven-metadata.xml`**（`versions`/`latest`/`release`/`lastUpdated`），二者均附四校验和 sidecar，供 `mvn dependency:get` 解析。**坐标可选（FR-123）**：`group_id` / `artifact_id` / `version` 可留空，缺失时服务端从 jar 内嵌 pom（FR-120）自动识别（表单为主、缺项 jar 补齐）；都无法得到则 `400`。**可选 `pom` 文件字段（FR-123）**：随主构件附带 `pom`（multipart 文件字段）则按主构件同基名落 `.pom`（client-priority，作为 pom 三级兜底「用户上传」层、不被服务端兜底覆盖）。**SNAPSHOT**：Web 上传快照主构件由服务端铸造时间戳唯一版本（见下「Maven 格式」段）。
    - **npm**：`name` / `version`；存储路径 = `{name}/-/{上传文件名}`（不解包 .tgz，name/version 由表单提供）。
    - **Raw**：`path`；存储路径即该路径。
- **响应**：新建返回 `201`、覆盖返回 `200`（覆盖语义沿用各格式策略）。上传后可经各格式既有下载端点取回。
- **错误**：`401`/`404` 私有仓库对未授权方拒绝；`403` 无写权限；`400` 向 `proxy` 仓库上传 / 格式不支持 / 缺必填字段；`409` 命中不可覆盖策略（Maven release / npm 已发布 tarball）；`413` 超过 `limits.max_artifact_size`。

### 跨仓库搜索

- **方法 / 路径**：`GET /api/v1/search`
- **请求**：查询参数 `q`（关键字 / 坐标，必填且非空）、可选 `format` 过滤，及 `offset` / `limit` 分页参数。
- **响应**：统一分页结构 `{ items, total, offset, limit, has_more }`，每项含所属仓库（`repo_id`、`repo_name`、`format`）、`path`、`sha256`、`size`、`created_at`。**结果仅含调用方有读权限的仓库制品**（匿名仅含 public 仓库）；`total` 与 `items` 均按读权限过滤后计数，不泄露无权私有仓库内容。
- **错误**：`400` 查询参数不合法（如 `q` 为空）。

### 查询开源许可清单（P2，公开 / 匿名可读，FR-102）

- **方法 / 路径**：`GET /api/v1/licenses`
- **请求**：无查询参数。**公开端点，匿名可读**（不经鉴权门）。
- **响应**：开源许可清单对象 `{ generated, entries, summary }`——`generated` 为是否已由构建期脚本生成（`false` 表示本地未生成 / 占位，`entries` 为空，客户端显空态）；`entries` 为逐条依赖归因数组，每项 `{ name, version, license, author, kind, source }`，`kind` 取 `runtime` | `dev`（运行时 / 开发依赖）、`source` 取 `rust` | `frontend`（Rust crate / 前端 npm）；`summary` 为 `{ total, runtime, dev, licenses }`（依赖总数 / 运行时数 / 开发数 / 许可证种类数）。清单由构建期 `cargo-about`（Rust）+ `pnpm licenses list`（前端）扫描生成并**嵌入二进制**，运行时只读、**纯本机内部、绝不外发、不向外部 phone-home**（ADR-0025，守 ADR-0009）。
- **错误**：无（公开只读；本地未生成时仍返回 `200` + `generated=false` 空清单，不报错）。

### 查询审计日志（P2，仅 Admin）

- **方法 / 路径**：`GET /api/v1/audit`
- **请求**：查询参数均可选——`action`（按动作过滤，如 `login` / `repo.create` / `artifact.upload`）、`target_repo`（按仓库名过滤）、`actor`（按主体用户名过滤），及 `offset` / `limit` 分页参数（默认 `offset=0`、`limit=50`，上限 1000）。仅管理员可访问。
- **响应**：统一分页结构 `{ items, total, offset, limit, has_more }`，按时间倒序（最新在前）。每项含 `id`、`ts`、`actor`、`actor_kind`（`session` | `token` | `basic` | `anonymous`）、`request_id`、`source_ip`、`action`、`target_repo`、`target`、`result`（`success` | `denied` | `error`）、`detail`。审计只记元数据级安全 / 管理事件，**绝不含密码 / Token / JWT / 上游凭据**（FR-31，ADR-0015）。
- **错误**：`401` 未认证；`403` 非管理员。

### 查询系统运行日志（P2，仅 Admin，FR-107）

- **方法 / 路径**：`GET /api/v1/system-logs`
- **请求**：查询参数均可选——`level`（按级别过滤，大小写不敏感：`ERROR` / `WARN` / `INFO` / `DEBUG` / `TRACE`，精确匹配该级别；无法识别的值视为不过滤），及 `offset` / `limit` 分页参数（默认 `offset=0`、`limit=200`，上限 1000）。仅管理员可访问。
- **响应**：统一分页结构 `{ items, total, offset, limit, has_more }`，**tail 语义、最新在前**（`offset` 从最新行起向更旧偏移）。每项为一条结构化日志条目 `{ timestamp, level, message }`——`timestamp` 为 RFC3339 字符串（无法解析为 `null`）、`level` 为级别规范大写串（无法识别为 `null`）、`message` 为消息正文（含 target 与字段）。数据来自应用运行时技术日志文件 `{data_dir}/logs/app.log`（tracing 输出，经文件 sink 落盘 + 大小滚动，ADR-0029），**与审计日志（业务留痕落 SQLite）区分**：本端点是运行时技术日志、不落库。日志文件不存在 / 为空时返回 `200` + 空清单（`total=0`），不报错。**纯本机内部数据、绝不外发**（守 ADR-0009 / 0015 基调）。
- **错误**：`401` 未认证；`403` 非管理员。

### 查询使用分析（P2，仅 Admin）

- **方法 / 路径**：`GET /api/v1/analytics/usage`
- **请求**：查询参数可选 `top`（热门制品 / 仓库用量各取前 N 条，默认 10，上限 100）。仅管理员可访问。
- **响应**：聚合总览对象 `{ total_access, total_download, top_downloads, repo_usage }`——`total_access` / `total_download` 为全局累计访问 / 下载量；`top_downloads` 为按下载量倒序的热门制品（每项含 `repo_name`、`repo_path`、`count`、`last_at`）；`repo_usage` 为按下载量汇总到仓库的用量（每项含 `repo_name`、`count`），均倒序。数据为本机内部聚合统计（消费 FR-57 采集的 `usage_stats`），**纯本地查询、绝不外发、不向外部遥测 phone-home**（FR-58，ADR-0009）。
- **错误**：`401` 未认证；`403` 非管理员。

### 查询仪表盘概览（P2，仅 Admin，FR-108）

- **方法 / 路径**：`GET /api/v1/dashboard/summary`
- **请求**：无查询参数。仅管理员可访问。
- **响应**：KPI 概览对象 `{ repo_count, artifact_count, total_bytes, user_count }`——`repo_count` 为仓库总数；`artifact_count` 为制品**索引条目数**（不去重，含同一 blob 被多仓库引用的多条）；`total_bytes` 为存储用量字节（按 `sha256` **去重**求和，同一 blob 只计一次）；`user_count` 为用户总数。供控制台首页仪表盘 KPI 卡（FR-108，增强 FR-18）。各计数经 `meta` 既有计数方法纯本地聚合查询取得，**纯本机内部数据、绝不外发**。普通用户 / 匿名的首页降级展示由前端只用可见仓库列表承载，不调本端点。
- **错误**：`401` 未认证；`403` 非管理员。

### 查询主机监控（P2，仅 Admin，FR-98）

- **方法 / 路径**：`GET /api/v1/monitor/host`
- **请求**：无查询参数。仅管理员可访问。
- **响应**：主机指标快照对象 `{ cpu, memory, disk, uptime_secs }`——`cpu` 为 `{ usage_percent, logical_cores }`（全局 CPU 使用率百分比 0~100 + 逻辑核数）；`memory` 为 `{ total_bytes, used_bytes, swap_total_bytes, swap_used_bytes }`（物理内存与交换分区的总量 / 已用，单位字节）；`disk` 为 `{ total_bytes, available_bytes, disks }`（磁盘总量 / 可用汇总 + 逐盘明细数组，每项 `{ mount_point, total_bytes, available_bytes }`）；`uptime_secs` 为系统运行时长（秒）。经 `sysinfo` **按请求单次采样**（不后台轮询、不落库），**纯本机内部数据、绝不外发、不向外部遥测 phone-home**（ADR-0023，守 ADR-0009 / 0015 基调）。注意：`cpu.usage_percent` 在首次 / 间隔过近的采样可能为 `0`（CPU 使用率需两次采样间隔才有非零值，属已知取舍）。
- **错误**：`401` 未认证；`403` 非管理员。

### 查询指标时序（P2，仅 Admin，FR-105）

- **方法 / 路径**：`GET /api/v1/monitor/metrics`
- **请求**：查询参数 `metric`（必填，指标键）、`from` / `to`（可选，Unix 毫秒 UTC；`to` 缺省取「现在」、`from` 缺省取 `to` 往前 1 小时）、`step`（可选，降采样步长毫秒；缺省 `0` 表示不降采样、返回原始样本点）。仅管理员可访问。
- **指标键**：`host.cpu_percent` / `host.memory_percent` / `host.disk_percent`（主机使用率%）、`storage.repo_count` / `storage.blob_count` / `storage.total_bytes`（仓库数 / 去重 blob 数 / 去重存储字节）、`protection.active_bans` / `protection.rate_limited_total`（活跃封禁数 / 限流累计被拒数）、`usage.access_total` / `usage.download_total`（累计访问量 / 下载量）。后两类与限流被拒为**累计值**（counter），曲线为单调累计，按需增量由调用方差分。缓存命中率本期未采（待埋点）。
- **响应**：`{ metric, points }`——`points` 为时序点数组（按 ts 升序，每项 `{ ts, value }`，`ts` 为 Unix 毫秒；`step>0` 时为「桶起点 + 桶内平均」的降采样点）。后台按可配间隔采样各域 gauge 落 SQLite（经 `meta`）、按保留期 + 行数滚动清理；**纯本机内部数据、绝不外发、不向外部遥测 phone-home**（ADR-0027，取代 ADR-0023「不留时序」；FR-98 实时快照端点保留）。
- **错误**：`400` 缺失 / 空 `metric`；`401` 未认证；`403` 非管理员。

### 查询防护状态快照（P2，仅 Admin）

- **方法 / 路径**：`GET /api/v1/protection/status`
- **请求**：无查询参数。仅管理员可访问。
- **响应**：防护健康快照对象 `{ alerts_enabled, window_secs, window_counts, active_banned_ips, dropped_alerts, recent_alerts }`——`alerts_enabled` 为阈值告警是否启用；`window_secs` 为当前评估窗时长（秒）；`window_counts` 为各防护维度当前窗内计数数组（每项 `{ dimension, count }`，`dimension` 取 `rate_limit` / `ban` / `cc_challenge` / `waf` / `slowloris`）；`active_banned_ips` 为当前处于封禁中的 IP 数；`dropped_alerts` 为因队列满被丢弃的告警累计数（采集降级观测）；`recent_alerts` 为最近若干条告警（结构同下「查询告警历史」的项）。窗内计数 / 封禁数取自进程内态、告警查本地 SQLite，**纯本机聚合、绝不外发、不向外部遥测 phone-home**（FR-56，ADR-0017）。
- **错误**：`401` 未认证；`403` 非管理员。

### 查询告警历史（P2，仅 Admin）

- **方法 / 路径**：`GET /api/v1/protection/alerts`
- **请求**：查询参数均可选——`dimension`（按防护维度过滤，取 `rate_limit` / `ban` / `cc_challenge` / `waf` / `slowloris`），及 `offset` / `limit` 分页参数（默认 `offset=0`、`limit=50`，上限 1000）。仅管理员可访问。
- **响应**：统一分页结构 `{ items, total, offset, limit, has_more }`，按时间倒序（最新在前）。每项含 `id`、`ts`、`dimension`、`severity`（`warn` | `error`）、`observed_value`（触发告警时的窗内观测计数）、`threshold`（触发阈值）、`window_secs`（评估窗时长）、`detail`（中文文案）。告警是本机内部数据，**绝不含凭据**，**纯本地查询、绝不外发**（FR-56，ADR-0017）。
- **错误**：`401` 未认证；`403` 非管理员。

### 读取防护配置（P2，仅 Admin，FR-79）

- **方法 / 路径**：`GET /api/v1/protection/config`
- **请求**：无查询参数。仅管理员可访问。
- **响应**：当前**生效**的防护配置全量对象（七个维度），取自运行时防护配置真源（含运行时 PATCH 在内的最新值），结构为 `{ rate_limit, ip_list, ban, slowloris, cc_challenge, waf, alerts }`，各子对象字段与 `[protection.*]` TOML 配置同名同义（如 `rate_limit.{enabled,window_secs,ip_max_requests,identity_max_requests,repo_max_requests,ip_max_concurrent,user_max_concurrent,repo_max_concurrent}`、`ip_list.{allow,deny}`、`ban.{enabled,window_secs,threshold,duration_secs}`、`slowloris.{enabled,body_read_timeout_secs,header_timeout_secs,max_body_bytes}`、`cc_challenge.{enabled,difficulty,ttl_secs,exempt_authenticated}`、`waf.{enabled,rules[]}`、`alerts.{enabled,window_secs,...}`）。防护配置**不含任何密码 / Token / 上游凭据**，整体回显无敏感项。
- **错误**：`401` 未认证；`403` 非管理员。

### 修改防护配置（P2，仅 Admin，FR-79）

- **方法 / 路径**：`PATCH /api/v1/protection/config`
- **请求**：JSON 体为一份**完整**的防护配置对象（结构同上 `GET` 的响应；前端应先 `GET` 当前值、改后整体回传）。仅管理员可调用。
- **行为**：**整体替换** `protection` 配置子树，校验通过即**即时生效、无须重启**——派生态（IP 名单匹配器、WAF 规则集）按新配置重建，下一个请求即按新阈值 / 开关 / 名单 / 规则判定。限流计数 / 封禁登记 / 告警去抖等进程内累计运行态在改配置时**不清零**。**持久化（FR-106，ADR-0028）**：`protection` 为非密钥节，校验通过后整节序列化落库 `app_settings`（key=`protection`），**重启仍生效**（启动经覆盖层 `env 显式 > DB > 文件默认` 合并）；落库失败只 WARN、不阻断即时生效（**不写回 TOML**，配置文件兜底真源不变）。
- **响应**：替换后生效的防护配置全量对象（同 `GET`）。
- **错误**：`400` 配置非法（如某时间窗为 0、CC 难度超 64 位等），错误码 `bad_request`，**且不改变现有生效配置**；`401` 未认证；`403` 非管理员。

### 预览 Nexus 可迁移仓库（在线 REST 入口）

- **方法 / 路径**：`POST /api/v1/migrate/nexus/preview`
- **请求**：JSON 体 `{ "base_url", "auth_ref"? }`。`base_url` 为源 Nexus 基址（如 `https://nexus.example`）；`auth_ref` 为上游凭据引用（仅引用，真值走环境变量 `JIANARTIFACT_MIGRATE_<NAME>_USERNAME` / `JIANARTIFACT_MIGRATE_<NAME>_PASSWORD`，不入库、不回显），匿名可访问的源系统可省略。仅管理员可调用。
- **行为**：连接在线 Nexus，经其 `service/rest/v1/repositories` 枚举可迁移仓库列表与基本元数据。这是迁移的**发现 / 预览**步骤，**不搬运任何制品**（搬运为后续分期能力）。
- **响应**：仓库摘要数组，每项含 `name`、`format`（Nexus 原样格式，如 `maven2` / `npm`）、`type`（`hosted` / `proxy` / `group`）、`upstream_url`（仅 proxy 仓库有值，取自源系统 `attributes.proxy.remoteUrl`）。
- **错误**：`400` 参数不合法或 `auth_ref` 对应凭据未在环境变量中配置；`401` 未认证；`403` 非管理员；`502` 连接源系统失败 / 鉴权失败 / 响应异常（不向调用方泄露源系统内部细节）。

### 预览 Nexus 可迁移内容（离线 blob store 入口）

- **方法 / 路径**：`POST /api/v1/migrate/nexus/offline/preview`
- **请求**：JSON 体 `{ "path" }`。`path` 为本地 Nexus 文件型 blob store 根目录路径（服务进程可访问的本地文件系统路径，其下应含 `content/` 子目录）。仅管理员可调用。
- **行为**：当源 Nexus 已下线、只剩其文件型 blob store 目录时，从该本地目录解析磁盘布局（`content/` 分片目录 + 每个 blob 一份 `.properties` 元数据），按所属仓库枚举可迁移的 blob 及基本元数据。这是迁移的**发现 / 预览**步骤，**仅解析 `.properties` 元数据、不读取也不搬运 blob 本体**（搬运为后续分期能力）。软删（`deleted=true`）、损坏或缺必要字段的元数据被容错跳过，不中断整次枚举。
- **响应**：按仓库分组的数组，每项含 `repo_name`（仓库名，取自 `@Repo.repo-name`）、`blob_count`（该仓库枚举到的 blob 数）、`blobs`（blob 预览项数组，每项含 `blob_name`（坐标 / 路径，取自 `@BlobStore.blob-name`）、`sha1`（缺失为 `null`）、`size`（字节数，缺失或非法为 `null`））。结果按仓库名、仓库内按 blob 名字典序稳定排序。
- **错误**：`400` 路径为空、不存在 / 非目录，或其下缺 `content/` 目录（疑似不是 Nexus 文件型 blob store）；`401` 未认证；`403` 非管理员。

### 迁移 Nexus proxy 仓库（配置 + 缓存制品搬运）

- **方法 / 路径**：`POST /api/v1/migrate/nexus/proxy/migrate`
- **请求**：JSON 体 `{ "base_url", "auth_ref"?, "offline_path" }`。`base_url` 为源 Nexus 基址（经其 REST API 枚举 proxy 仓库配置：格式 / 上游地址）；`auth_ref` 为在线访问凭据引用（仅引用，真值走环境变量 `JIANARTIFACT_MIGRATE_<NAME>_USERNAME` / `PASSWORD`，不入库、不回显，匿名可访问的源系统可省略）；`offline_path` 为源 Nexus 文件型 blob store 根目录的本地路径（其下应含 `content/` 子目录），提供已缓存 proxy 制品本体。仅管理员可调用。
- **行为**：把源 Nexus 的 **proxy 类型仓库**搬到本系统：① 据在线枚举的 proxy 仓库配置在本系统创建对应 proxy 仓库（映射 Nexus 格式名 → 本系统已实现格式：`maven2`→`maven` 等；同名仓库已存在则复用、不重复建仓、不改其既有配置；格式未实现或缺上游地址的仓库整体跳过）；② 从离线 blob store 按仓库名取该仓库的缓存制品本体（成对的 `.properties` + `.bytes`，缺本体者跳过），经既有制品机理流式写入缓存——**blob 先落盘并校验 sha256 再写元数据索引（标记 `cached`），写索引失败回滚不留孤儿，不整体载入内存**。搬运幂等可重入（同坐标同内容跳过），单个制品搬运失败（路径非法 / 读本体失败 / 写入失败）记录跳过、不中断整批。仅迁移 proxy 仓库；hosted 仓库制品完整搬运为后续分期能力。迁移**不搬运源系统上游凭据**（凭据真源 env / 配置，需运维另行配置）。
- **响应**：迁移报告 `{ "repos": [...], "skipped_repos": [...] }`。`repos` 为各被迁移 proxy 仓库的明细数组，每项含 `repo_name`、`format`（映射后本系统格式）、`created`（`true` 新建 / `false` 复用已存在）、`migrated_artifacts`（成功搬运的缓存制品数）、`skipped_artifacts`（跳过 / 失败的制品数）；`skipped_repos` 为因格式未实现或缺上游地址而整体跳过的源仓库名列表。
- **错误**：`400` `offline_path` 为空、离线目录不存在 / 非目录或缺 `content/` 子目录；`401` 未认证；`403` 非管理员；`502` 连接 / 鉴权 / 解析源 Nexus 失败（在线枚举阶段）。

### 迁移 Nexus hosted 仓库（配置 + 完整制品搬运）

- **方法 / 路径**：`POST /api/v1/migrate/nexus/hosted/migrate`
- **请求**：JSON 体 `{ "base_url", "auth_ref"?, "offline_path" }`。`base_url` 为源 Nexus 基址（经其 REST API 枚举 hosted 仓库配置：格式 / 可见性）；`auth_ref` 为在线访问凭据引用（仅引用，真值走环境变量 `JIANARTIFACT_MIGRATE_<NAME>_USERNAME` / `PASSWORD`，不入库、不回显，匿名可访问的源系统可省略）；`offline_path` 为源 Nexus 文件型 blob store 根目录的本地路径（其下应含 `content/` 子目录），提供 hosted 仓库制品本体。仅管理员可调用。
- **行为**：把源 Nexus 的 **hosted 类型仓库**完整搬到本系统：① 据在线枚举的 hosted 仓库配置在本系统创建对应 hosted 仓库（映射 Nexus 格式名 → 本系统已实现格式：`maven2`→`maven` 等；同名仓库已存在则复用、不重复建仓、不改其既有配置；格式未实现的仓库整体跳过）；② 从离线 blob store 按仓库名取该仓库的全部制品本体（成对的 `.properties` + `.bytes`，缺本体者跳过），经既有制品机理流式写入——**blob 先落盘并校验 sha256 再写元数据索引（`cached=false`，hosted 正常制品语义），写索引失败回滚不留孤儿，不整体载入内存**。按各格式覆盖 / 不可变策略处理重复搬运（同坐标不同内容且不可覆盖如 Maven release 则跳过该制品、不中断整批；可覆盖如 Raw / Docker tag 则落定新内容）；超过 `limits.max_artifact_size` 的制品按跳过处理（不留半截 blob）。搬运幂等可重入（同坐标同内容跳过），单个制品搬运失败记录跳过、不中断整批。仅迁移 hosted 仓库（proxy 走 `proxy/migrate` 端点）。迁移**不搬运源系统上游凭据**。
- **响应**：迁移报告 `{ "repos": [...], "skipped_repos": [...] }`。`repos` 为各被迁移 hosted 仓库的明细数组，每项含 `repo_name`、`format`（映射后本系统格式）、`created`（`true` 新建 / `false` 复用已存在）、`migrated_artifacts`（成功搬运的制品数）、`skipped_artifacts`（跳过 / 失败的制品数，含路径非法 / 不可覆盖 / 超限）；`skipped_repos` 为因格式未实现而整体跳过的源仓库名列表。
- **错误**：`400` `offline_path` 为空、离线目录不存在 / 非目录或缺 `content/` 子目录；`401` 未认证；`403` 非管理员；`502` 连接 / 鉴权 / 解析源 Nexus 失败（在线枚举阶段）。

### Nexus 在线拉取迁移（FR-82 / 异步任务 FR-83）

- **方法 / 路径**：`POST /api/v1/migrate/nexus/online/migrate`
- **请求**：JSON 体 `{ "base_url", "auth_ref"?, "repositories": [{ "source", "target"? }] }`。`base_url` 为源 Nexus 基址；`auth_ref` 为在线访问凭据引用（仅引用，真值走环境变量 `JIANARTIFACT_MIGRATE_<NAME>_USERNAME` / `PASSWORD`，不入库、不回显，匿名源可省略）；`repositories` 为选中的源仓库，`source` 为源仓库名、`target` 为本系统目标仓库名（省略 / 空则与源同名，允许改名）。仅管理员可调用。**与 `hosted/migrate` 的区别：本端点不需离线 blob store 目录**——经源 Nexus REST 在线拉取制品。
- **行为**：把所选 **Maven（`maven2`）hosted** 仓库经在线 HTTP 搬到本系统。**同步阶段**只做：枚举源仓库配置、匹配所选 `source`、解析凭据（失败即 `400` / `502`，不开任务）；随后**立即返回 `job_id`（`202`）**，实际搬运在**后台任务**异步执行（FR-83），进度经 `GET /migrate/jobs/{id}` 轮询。后台任务：① 建 / 复用目标 hosted 仓库（名取 `target`，允许与源不同名）；② 经 `service/rest/v1/components?repository=X`（`continuationToken` 分页）枚举该仓库全部 asset，按各 asset 的 `downloadUrl` HTTP 流式下载（不整体载入内存），经既有制品机理写入——**blob 先落盘并校验 sha256 再写元数据索引（`cached=false`），写索引失败回滚不留孤儿**；落定后比对下载内容 sha256 与源报告值，不符即视为损坏、回滚该制品并跳过（保证文件字节一致，含 `.sha1`/`.md5`/`.sha256`/`.sha512` sidecar 一并搬运）。下载 / 写入瞬时失败（网络中断 / 流式解码失败）**自动重试、指数退避**（确定性失败不重试）。按各格式覆盖 / 不可变策略处理重复，超 `limits.max_artifact_size` 跳过；搬运幂等可重入，单 asset 失败记录跳过、不中断整批。**仅 `maven2` hosted 参与**，其余整体跳过。迁移**不搬运源系统上游凭据**。
- **响应**：`202 Accepted`，体 `{ "job_id": string }`——供轮询 `GET /api/v1/migrate/jobs/{id}`。
- **错误**：`400` 未选择仓库 / 源仓库不存在；`401` 未认证；`403` 非管理员；`502` 连接 / 鉴权 / 解析源 Nexus 失败（同步枚举阶段）。

### Nexus 迁移任务进度（FR-83）

- **方法 / 路径**：`GET /api/v1/migrate/jobs/{id}`（单任务进度）、`GET /api/v1/migrate/jobs`（任务列表，供客户端重连找回）。仅管理员可调用。
- **进度响应**（`jobs/{id}`）：`{ "job_id", "phase", "total_assets", "done_assets", "migrated", "skipped", "current_repo", "current_path", "paused", "repos": [...], "skipped_repos": [...], "error" }`。`phase` ∈ `enumerating` / `downloading` / `paused` / `cancelled` / `done` / `failed`；`paused` 为暂停态布尔（FR-91，暂停期间为真）；`repos` 项同迁移报告明细（`source_repo` / `target_repo` / `format` / `created` / `migrated_artifacts` / `skipped_artifacts`）；`error` 仅 `failed` 时非空。任务为**进程内、有界、不落库**——服务器重启即丢失，靠迁移幂等重跑恢复（见 ADR-0019）。
- **列表响应**（`jobs`）：`[{ "job_id", "phase", "total_assets", "done_assets", "migrated", "skipped", "current_repo", "paused" }]`，按登记时序。
- **错误**：`401` 未认证；`403` 非管理员；`404` 未知 `job_id`（含已被淘汰的旧任务）。

### Nexus 迁移任务控制（取消 / 暂停 / 继续，FR-91）

- **方法 / 路径**：`POST /api/v1/migrate/jobs/{id}/cancel`、`POST /api/v1/migrate/jobs/{id}/pause`、`POST /api/v1/migrate/jobs/{id}/resume`。无请求体。仅管理员可调用。
- **行为**：对在线拉取（FR-82/83）异步任务做协作式生命周期控制——后台循环在**下一个 asset 边界**响应信号（正在下载的单个 asset 跑完再停）。`cancel`：停止后续搬运、任务标 `cancelled`（**不算失败**，已搬运的制品保留）；`pause`：后台循环挂起、不再推进、进度 `paused` 置真、`phase` 置 `paused`；`resume`：唤醒挂起的循环恢复搬运。
- **响应**：`200 OK`，无响应体。对**已结束**任务（`done` / `failed` / `cancelled`）的控制为**幂等空操作**（不报错、不改终态）；`pause` 对已取消任务亦为空操作。
- **错误**：`401` 未认证；`403` 非管理员；`404` 未知 `job_id`（含已被淘汰的旧任务）。

### 检查在线更新（仅 Admin，FR-85）

- **方法 / 路径**：`GET /api/v1/update/check`
- **请求**：无请求体。仅管理员可调用。
- **行为**：查配置仓库（`[update] repo`，默认 `wcpe/JianArtifact`）的 Release，与当前运行版本比对，返回是否有更新。**更新通道**（`[update] channel`，FR-89）决定取哪一条：`stable`（默认）取最新稳定版（`/releases/latest`，忽略预发布）；`prerelease` 取最新一条非草稿的 release（`/releases` 列表首条，含预发布）。出站经 `[network.proxy]`（FR-84）注入的代理。**`[update] enabled=false` 时一律拒绝、不联网**；token / 凭据不进日志、不回显。
- **响应**：`{ "current_version", "latest_version", "update_available", "asset_name", "notes" }`。`current_version` 为当前运行版本；`latest_version` 为最新稳定版本（`tag_name` 去前导 `v`）；`update_available` 为 `latest > current`；`asset_name` 为本机平台对应资产名（`jianartifact-{version}-{target}{ext}`）；`notes` 为发布说明（Release `body`）。
- **错误**：`400` 当前平台无自更新资产 / 版本串非法；`401` 未认证；`403` 非管理员；`409` 在线更新未启用（`enabled=false`）；`502` 上游不可达 / 超时 / 响应异常（不向调用方泄露内部细节）。

### 应用在线更新（仅 Admin，FR-85）

- **方法 / 路径**：`POST /api/v1/update/apply`
- **请求**：无请求体。仅管理员可调用。
- **行为**：按本机平台取对应资产 → 流式下载并边算 sha256（不整体载入内存）→ 取同名 `.sha256` 资产比对 → 一致则**原子替换**当前二进制 → 置位重启请求触发优雅停机，排空在途请求后由 `main` 据 `[update] restart_mode`（`self` 自拉起 / `exit` 仅退出交外部进程管理器）拉起新进程或退出。**仅 sha256 校验通过才替换、替换是最后一步**：校验失败即删临时文件、保留旧二进制、进程续以旧版运行。`enabled=false` 时一律拒绝、不联网；token / 凭据不进日志、不回显。
- **并发单飞**：apply 为进程级互斥，已有一次自更新在途时再次触发立即返回 `409`「更新进行中」，不竞争下载临时文件与替换中间态（`.bak`/`.old`/`.new`）；占用标志在更新结束（含出错 / 早返回）时可靠复位。check 端点不受影响。
- **响应**：`200`，体 `{ "status": "已更新，正在重启", "new_version" }`。`new_version` 为替换后的新版本号；返回后服务排空在途请求并按 `restart_mode` 重启。
- **错误**：`400` 当前平台无自更新资产 / 版本串非法；`401` 未认证；`403` 非管理员；`409` 在线更新未启用（`enabled=false`）/ 无更新可用（最新版本不高于当前）/ 已有自更新在途（「更新进行中」）；`422` 下载内容 sha256 不一致或发布缺所需资产（拒绝替换、保留旧二进制）；`500` 本地替换 / 落盘失败；`502` 上游不可达 / 超时 / 响应异常。
- **错误响应体**：统一为 `{ "error": { "code", "message" } }`（`422` 对应错误码 `unprocessable_entity`）。

### 回滚到上一版本（仅 Admin，FR-104）

- **方法 / 路径**：`POST /api/v1/update/rollback`
- **请求**：无请求体。仅管理员可调用。
- **行为**：用升级时留下的持久回滚备份 `{exe}.rollback.bak`（单备份，只保留上一版）**原子还原**当前二进制 → 置位重启请求触发优雅停机，排空在途请求后由 `main` 据 `[update] restart_mode`（`self` / `exit`）拉起上一版进程或退出。回滚是**纯本地操作、不出站**，故**不受 `[update] enabled` 开关约束**（与是否允许联网升级无关）。失败尽力回退、不留半截。
- **并发单飞**：回滚与 apply **共用**进程级单飞互斥——同一时刻只允许一个二进制变更在途（升级或回滚），已有一次在途时再次触发立即返回 `409`「更新进行中」，不竞争替换中间态（`.new`/`.old`/备份）。
- **响应**：`200`，体 `{ "status": "已回滚，正在重启" }`。返回后服务排空在途请求并按 `restart_mode` 重启。
- **错误**：`401` 未认证；`403` 非管理员；`409` 无可回滚的备份版本（从未成功升级过或备份缺失）/ 已有自更新在途（「更新进行中」）；`500` 本地替换失败。

### 系统重启（仅 Admin，FR-109）

- **方法 / 路径**：`POST /api/v1/system/restart`
- **请求**：无请求体。仅管理员可调用。
- **行为**：置位重启请求触发优雅停机，排空在途请求后由 `main` 据运行时 `restart_mode`（`self` 原地 `exec` 拉起 / `exit` 交进程管理器）**重启进程，不换二进制**（复用 ADR-0021/0032 自更新重启链路）。纯本地操作、不出站，**不受 `[update] enabled` 约束**。
- **并发单飞**：与 apply / rollback **共用**进程级单飞互斥，忙时 `409`「更新进行中」。
- **响应**：`200`，体 `{ "status": "正在重启" }`。
- **错误**：`401` 未认证；`403` 非管理员；`409` 已有进程级变更在途（「更新进行中」）；`500` 无法定位当前可执行文件。

### 系统关闭（仅 Admin，FR-109）

- **方法 / 路径**：`POST /api/v1/system/shutdown`
- **请求**：无请求体。仅管理员可调用。
- **行为**：置位重启请求（强制 `RestartMode::Exit`）触发优雅停机，排空在途请求后**进程退出、不自拉起**。**运维前提（ADR-0033）**：若部署配了自动重启的进程管理器（systemd `Restart=always` / docker `restart: always`），进程会被其再起——真正停机须经该管理器（`systemctl stop` 等）。纯本地操作、不出站，**不受 `[update] enabled` 约束**。
- **并发单飞**：与 apply / rollback / restart **共用**单飞互斥，忙时 `409`「更新进行中」。
- **响应**：`200`，体 `{ "status": "正在关闭" }`。
- **错误**：`401` 未认证；`403` 非管理员；`409` 已有进程级变更在途；`500` 无法定位当前可执行文件。

### 读取设置聚合（仅 Admin，FR-87）

- **方法 / 路径**：`GET /api/v1/settings`
- **请求**：无请求体。仅管理员可调用。
- **行为**：聚合网络代理（FR-84）与在线更新（FR-85）配置及当前版本，供控制台「设置」页展示。读**运行时可编辑设置热替换槽当前值**（含运行时 PATCH 在内，FR-88 / ADR-0022）；真源为 `config.toml` / env，运行时改动只入内存槽、重启回落文件配置。
- **脱敏**：响应**绝不含任何凭据**——代理 URL 去除 `user:pass@` 凭据（`scheme://user:pass@host` → `scheme://host`）；更新 token 仅以 `has_token: bool` 暴露、绝不回显 token 本体。
- **响应**：`{ "current_version", "network_proxy": { "http", "https", "no_proxy" }, "update": { "enabled", "repo", "api_base_url", "restart_mode", "channel", "has_token", "rollback_available" } }`。`network_proxy` 各 URL 为脱敏后字符串或 `null`；`update.channel` 为更新通道（`stable` / `prerelease`，FR-89）；`update.has_token` 表示是否已配置访问 token；`update.rollback_available`（FR-104）表示是否有可回滚的上一版本备份（持久回滚备份存在），供控制台启用 / 禁用回滚按钮。
- **错误**：`401` 未认证；`403` 非管理员。

### 编辑设置（仅 Admin，FR-88，运行时热替换）

- **方法 / 路径**：`PATCH /api/v1/settings`
- **请求**：仅管理员可调用。JSON 体 `{ "network_proxy"?: { "http", "https", "no_proxy" }, "update"?: { "enabled", "repo", "api_base_url", "restart_mode", "channel", "token"? } }`。
  - **部分更新（FR-109）**：`network_proxy` 与 `update` 两块**均可选**——只提供哪块就只改哪块（设置页只发 `network_proxy`、系统页只发 `update`）；两块都给则整体替换，向后兼容。
  - `network_proxy` 各项为字符串或 `null`；空串 / 空白视为不配置（清空）。代理 URL 可含 `user:pass@` 凭据。
  - `update.channel`（FR-89）：更新通道，仅允许 `stable` / `prerelease`。
  - `update.token` 三态：**缺省 / `null`** 保留现有 token 不变；**空串 `""`** 清空 token；**非空串**设置为新 token。
- **行为**：校验后锁外重建出站 `reqwest::Client`、原子换槽，**即时生效、无须重启**（FR-88 / ADR-0022）。代理凭据与 token **只入内存槽、不写回 TOML / 不入 DB / 不进日志 / 不回显**，重启回落文件 + env 配置。**持久化（FR-106，ADR-0028）**：在线更新的**非密钥字段**（`enabled` / `repo` / `api_base_url` / `restart_mode` / `channel`）落库 `app_settings`（key=`update`，经专用非密钥视图序列化、**token 自动剔除**），**重启仍生效**；网络代理（含账密）与 update token 继续**只入内存槽、不落库**（重启回落文件 + env，凭据红线不破）。落库失败只 WARN、不阻断即时生效。
- **校验失败不改状态**：任一校验未过返回 `400` 且**不改变**现有生效值（再次 GET 仍返回旧值）。
- **响应**：`200`，体同 GET（脱敏后的当前生效值）。
- **错误**：`400` 网络代理 URL 无法构造 / `restart_mode` 非 `self`|`exit` / `channel` 非 `stable`|`prerelease` / `repo` / `api_base_url` 为空；`401` 未认证；`403` 非管理员。

### 读取动态配置（仅 Admin，FR-106）

- **方法 / 路径**：`GET /api/v1/settings/dynamic`
- **请求**：无请求体。仅管理员可调用。
- **行为**：读取「新 Dynamic 节」的**非密钥**项当前 / 待生效值，供控制台「设置」页「系统配置」tab 回显。以启动期生效配置（`env 显式 > DB > 文件默认` 合并值）为基线、叠加**当前** `app_settings` 覆盖——回显含本次 PATCH 后写入 DB 的**待生效**值。这些节多在启动期装载、无热替换槽，**改动重启生效**（与代理 / 更新 / 防护的即时生效不同）。
- **响应**：`{ "limits": { "max_artifact_size" }, "audit": { "retention_days", "max_rows" }, "usage": { "detail_enabled", "max_detail_rows" }, "metrics": { "enabled", "allow_anonymous" }, "metrics_timeseries": { "enabled", "sample_interval_secs", "retention_days", "max_rows" }, "vuln": { "enabled", "source_base_url", "ecosystems", "refresh_interval_secs", "download_timeout_secs" }, "auth": { "session_ttl_secs", "login_max_failures", "login_lockout_secs" } }`。`auth` 仅三个可调标量，**绝不含 OIDC / LDAP 密钥**；各节均无凭据。
- **错误**：`401` 未认证；`403` 非管理员。

### 编辑动态配置（仅 Admin，FR-106，保存后重启生效）

- **方法 / 路径**：`PATCH /api/v1/settings/dynamic`
- **请求**：仅管理员可调用。JSON 体同 GET 响应形态（各非密钥节整体提交）。
- **行为**：整体校验各节数值边界（`metrics_timeseries.sample_interval_secs` / `vuln.refresh_interval_secs` / `vuln.download_timeout_secs` / `auth.session_ttl_secs` / `auth.login_lockout_secs` 必须 > 0）→ 通过则按节序列化落库 `app_settings`（key 为 `limits` / `observability.audit` / `observability.usage` / `observability.metrics` / `observability.metrics_timeseries` / `vuln` / `auth`，经白名单），**重启生效**（启动经覆盖层 `env 显式 > DB > 文件默认` 合并装载）。这些节无热替换槽，本期**不即时换槽**——下次启动装载生效（黄金组合）。
- **凭据红线**：`auth` 经专用非密钥视图序列化（仅三个标量），**OIDC / LDAP 密钥绝不入库**；其余节本就无凭据；端点只写固定白名单键，bootstrap 项（`server.*` / `data.*`）不经此路径。env 显式给值的节仍以 env 为准、重启时不被 DB 覆盖。
- **校验失败不落库**：任一节校验未过返回 `400` 且**不写任何节**（再次 GET 仍返回旧值）。
- **响应**：`200`，体同 GET（叠加刚写入的待生效值）。
- **错误**：`400` 校验未过（含中文原因）；`401` 未认证；`403` 非管理员；`500` 落库 / 序列化失败。

### 列出仓库 ACL

- **方法 / 路径**：`GET /api/v1/repositories/{id}/acl`
- **请求**：路径参数 `id`（仓库 id）。
- **响应**：ACL 条目数组，每项含 `id`、`user_id`、`permission`（`read` / `write` / `delete` / `admin`，四级动作，高动作蕴含低动作）。
- **错误**：`401` 未认证；`403` 非管理员；`404` 仓库不存在。

### 新增仓库 ACL 条目

- **方法 / 路径**：`POST /api/v1/repositories/{id}/acl`
- **请求**：路径参数 `id`；JSON 体 `{ "user_id", "permission" }`，`permission` 为 `read` / `write` / `delete` / `admin`（大小写不敏感）。
- **响应**：新建 ACL 条目对象。
- **错误**：`400` 参数不合法；`401` 未认证；`403` 非管理员；`404` 仓库或用户不存在；`409` 该用户的同类授权已存在。

### 移除仓库 ACL 条目

- **方法 / 路径**：`DELETE /api/v1/repositories/{id}/acl/{acl_id}`
- **请求**：路径参数 `id`（仓库 id）、`acl_id`（ACL 条目 id）。
- **响应**：删除成功状态。
- **错误**：`401` 未认证；`403` 非管理员；`404` 仓库或 ACL 条目不存在。

### 列出用户组（P2，仅 Admin）

- **方法 / 路径**：`GET /api/v1/groups`
- **请求**：无请求体。
- **响应**：用户组数组，每项含 `id`、`name`、`created_at`。
- **错误**：`401` 未认证；`403` 非管理员。

### 创建用户组（P2，仅 Admin）

- **方法 / 路径**：`POST /api/v1/groups`
- **请求**：JSON 体 `{ "name" }`。
- **响应**：新建用户组对象（`201`）。
- **错误**：`400` 组名为空；`401` 未认证；`403` 非管理员；`409` 组名已存在。

### 获取用户组详情（P2，仅 Admin）

- **方法 / 路径**：`GET /api/v1/groups/{id}`
- **请求**：路径参数 `id`（组 id）。
- **响应**：用户组对象。
- **错误**：`401` 未认证；`403` 非管理员；`404` 组不存在。

### 删除用户组（P2，仅 Admin）

- **方法 / 路径**：`DELETE /api/v1/groups/{id}`
- **请求**：路径参数 `id`（组 id）。删除时级联清理其成员关系与组 ACL。
- **响应**：删除成功状态。
- **错误**：`401` 未认证；`403` 非管理员；`404` 组不存在。

### 列出组成员（P2，仅 Admin）

- **方法 / 路径**：`GET /api/v1/groups/{id}/members`
- **请求**：路径参数 `id`（组 id）。
- **响应**：成员数组，每项含 `user_id`、`username`。
- **错误**：`401` 未认证；`403` 非管理员；`404` 组不存在。

### 加入组成员（P2，仅 Admin）

- **方法 / 路径**：`POST /api/v1/groups/{id}/members`
- **请求**：路径参数 `id`（组 id）；JSON 体 `{ "user_id" }`。
- **响应**：加入成功状态（`201`）。
- **错误**：`401` 未认证；`403` 非管理员；`404` 组或用户不存在；`409` 该用户已在组内。

### 移出组成员（P2，仅 Admin）

- **方法 / 路径**：`DELETE /api/v1/groups/{id}/members/{user_id}`
- **请求**：路径参数 `id`（组 id）、`user_id`（用户 id）。
- **响应**：移出成功状态。
- **错误**：`401` 未认证；`403` 非管理员；`404` 组不存在或该用户本不在组内。

### 列出仓库组 ACL（P2，仅 Admin）

- **方法 / 路径**：`GET /api/v1/repositories/{id}/group-acl`
- **请求**：路径参数 `id`（仓库 id）。
- **响应**：组 ACL 条目数组，每项含 `id`、`group_id`、`permission`（`read` / `write` / `delete` / `admin`）。
- **错误**：`401` 未认证；`403` 非管理员；`404` 仓库不存在。

### 对组授予仓库 ACL（P2，仅 Admin）

- **方法 / 路径**：`POST /api/v1/repositories/{id}/group-acl`
- **请求**：路径参数 `id`（仓库 id）；JSON 体 `{ "group_id", "permission" }`，`permission` 为 `read` / `write` / `delete` / `admin`（大小写不敏感）。组成员据此经组继承对该仓库的权限（与直接-用户 ACL 取并集后判定）。
- **响应**：新建组 ACL 条目对象（`201`）。
- **错误**：`400` 参数不合法；`401` 未认证；`403` 非管理员；`404` 仓库或组不存在；`409` 该组的同类授权已存在。

### 撤销组仓库 ACL（P2，仅 Admin）

- **方法 / 路径**：`DELETE /api/v1/repositories/{id}/group-acl/{acl_id}`
- **请求**：路径参数 `id`（仓库 id）、`acl_id`（组 ACL 条目 id）。
- **响应**：删除成功状态。
- **错误**：`401` 未认证；`403` 非管理员；`404` 仓库或组 ACL 条目不存在。

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

### Prometheus 指标（P2，默认仅 Admin）

- **方法 / 路径**：`GET /metrics`
- **请求**：无请求体。鉴权由配置决定——默认 `observability.metrics.allow_anonymous=false`，要求认证且仅管理员可访问；运维显式设 `allow_anonymous=true` 时免认证抓取（须把端点限定在内网 / 反向代理之后）。
- **响应**：`200`，`Content-Type: text/plain; version=0.0.4; charset=utf-8`，体为 Prometheus 文本格式的进程内注册表快照。指标为进程内自采（pull 模型），仅在被抓取时渲染，**不向任何外部端点 push / remote-write**（FR-32，ADR-0015）。
- **指标项**：HTTP 请求计数 / 延迟直方图（标签 `method` / `status_class` / `format`）、上传 / 下载字节累计、并发上传数、代理缓存命中 / 未命中（`result=hit|miss`）、上游回源耗时 / 失败、审计丢弃累计；**七层防护监控指标（FR-56，ADR-0017）**：限流被拒 `jianartifact_rate_limit_rejected_total`（标签 `dimension=ip|token|repo|concurrency`）、自动封禁触发 `jianartifact_ban_triggered_total`、当前封禁 IP 数 `jianartifact_ban_active_ips`（gauge）、CC 挑战下发 `jianartifact_cc_challenge_issued_total` / 失败 `jianartifact_cc_challenge_failed_total`、WAF 阻断 `jianartifact_waf_blocked_total`、慢速超时 `jianartifact_slowloris_timeout_total`。所有标签均为**有界枚举值**，**不以仓库名 / 路径 / 用户名 / 制品坐标 / 规则模式串作标签**（守低基数纪律）。
- **错误**：`observability.metrics.enabled=false` 时返回 `404`（端点形同不存在）；默认鉴权下 `401` 未认证、`403` 非管理员。

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

- **Maven 格式**：以 Maven 仓库布局暴露，路径形如 `/{仓库名}/{groupId 路径}/{artifactId}/{version}/...`，供 `mvn deploy` / `mvn` 拉取使用；按 Maven 协议处理制品与校验和（sha256 索引）。**服务端权威维护元数据**（FR-121/122，ADR-0037）：主版本文件写入后据 SQLite 索引重生成 artifact 级 `maven-metadata.xml` + pom 三级兜底（jar 内嵌 → 用户上传 → 按 GAV 最小 pom），遵循 client-priority（`mvn deploy` 自带的 pom 不被改写）。**SNAPSHOT**：Web 上传的快照主构件由服务端铸造唯一时间戳版本（`{artifact}-{base}-{yyyyMMdd.HHmmss}-{buildNumber}.{ext}`）并生成快照级 `{base}-SNAPSHOT/maven-metadata.xml`（snapshot/snapshotVersions/lastUpdated），供 `mvn` 解析最新快照；`mvn deploy` 自带的时间戳构件由服务端据目录扫描权威重生成快照 metadata。
- **npm 格式**：以 npm registry 协议暴露，路径形如 `/{仓库名}/{包名}`、`/{仓库名}/{包名}/-/{tarball}`，供 `npm publish` / `npm install` 使用。
- **NuGet 格式**：以 NuGet v3 协议暴露，供 `dotnet nuget push` / `dotnet add package` 使用。客户端 source 配 `/{仓库名}/v3/index.json`。
  - 服务索引 `GET /{仓库名}/v3/index.json`：列出本仓库 v3 资源（扁平容器 `PackageBaseAddress/3.0.0`、发布端点 `PackagePublish/2.0.0`），`@id` 指向本仓库对应端点；`proxy` 仓库回源上游服务索引后把扁平容器 `@id` 重写为指向本仓库。
  - 扁平容器版本列表 `GET /{仓库名}/v3-flatcontainer/{id}/index.json`：返回该包所有已发布版本 `{"versions":[...]}`；`hosted` 由元数据索引动态生成，`proxy` 回源上游。
  - 下载 `GET /{仓库名}/v3-flatcontainer/{id}/{version}/{id}.{version}.nupkg`（及同目录 `{id}.nuspec`）：流式返回；`proxy` cache-miss 回源缓存、命中不回源。id 与 version 按 NuGet 约定小写规范化。
  - 发布 `PUT /{仓库名}/v3/package`（`nuget push`）：`multipart/form-data` 内含 .nupkg；服务端解压读取内嵌 `.nuspec` 解析 id / version，先落 .nupkg 再落 .nuspec。鉴权支持两种：`Authorization: Basic`（用户口令或 API Token 作密码字段），或 NuGet 规范的 api-key 头 `X-NuGet-ApiKey: <API Token>`（即 `dotnet nuget push -k <token>` 的原生方式，无 `Authorization` 头时按 API Token 校验该头值）。
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

- 用户组/团队与对组授予仓库 ACL（FR-49）已落地，见上文“列出用户组”至“撤销组仓库 ACL”各端点；动作维度 `read` / `write` / `delete` / `admin` 也已在现有 ACL 端点落地（见“新增仓库 ACL 条目”）。增强管理 UI（FR-50）已接入 Web 控制台：仓库详情「权限」页签可对用户与用户组授予 / 撤销四级动作 ACL，新增「用户组管理」页可建组 / 删组与加移成员（均仅 Admin）。FR-50 不新增后端端点，前端复用上述 FR-48 / FR-49 既有契约。

### 七层防护管理

- 防护策略管理（管理员，FR-79）**已落地**：见上文 §3「读取防护配置」「修改防护配置」（`GET` / `PATCH /api/v1/protection/config`）——在线读取 / 整体替换限流阈值、并发上限、WAF 规则、IP 黑白名单、慢速 / CC / 告警等各维度配置，校验通过即时生效、无须重启。
- CC 挑战（FR-54 / ADR-0008）已落地，但**无独立管理端点**：经 `[protection.cc_challenge]` 配置开关 / 难度 / 过期 / 豁免，由中间件对匿名请求按工作量证明（PoW）挑战 / 校验（见上文「错误约定」的 CC 挑战说明）；不另设质询 / 校验端点（挑战令牌无状态、随 `429` 响应体下发，证明经请求头 `X-CC-Solution` 在原请求上提交）。

### 使用分析

- 使用统计查询：`GET /api/v1/analytics/usage`（仅 Admin）返回访问量、下载量、热门制品、仓库用量等聚合数据，供数据面板展示；数据本机内部、不外发（详见上文「查询使用分析」端点）。

### 漏洞（P2）

- 制品漏洞状态：制品详情与搜索结果附带漏洞标记（基于本地漏洞库离线镜像 + 坐标级匹配）；可查某制品命中的公告列表（如 `GET /api/v1/repositories/{id}/artifacts/{path}/vulnerabilities`）。制品坐标本地匹配、不外发。
