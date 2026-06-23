# 功能规格：npm registry 格式（hosted + proxy）

> 状态：开发中　·　关联 PRD：FR-15（含 FR-61 / FR-68 / FR-69 的 npm 维度）　·　分支：feature/fr-15-npm

## 1. 背景与目标

在制品通用机理与统一 `Format` trait（见 `p1-artifact-and-raw.md`）之上，按 npm registry 协议接入第二种格式 npm，使官方 `npm` 客户端可直接 `npm publish` / `npm install`。属阶段 P1，是第一期"四种高频格式各 hosted + proxy"中的一种。

npm 复用既有通用机理（流式存取、四校验和、blob 先落盘再写索引、proxy 单飞缓存、覆盖策略经 `Format::can_overwrite`），只新增 npm 自身协议适配：packument JSON 的生成 / 合并、scoped 包 URL 解码、tarball 路径映射、dist 摘要重写与代理 packument 的 tarball URL 重写。

## 2. 需求（要什么）

- 范围内（npm 维度）：
  - **packument 获取** `GET /{repo}/{package}`：hosted 返回存储的包级 JSON 文档；proxy 回源上游 packument 后重写各版本 `dist.tarball` 指向本仓库（不改 integrity/shasum）。scoped 包以 `@scope%2Fname` 编码 URL 访问。
  - **tarball 下载** `GET /{repo}/{package}/-/{tarball}.tgz`：经通用机理流式返回；proxy cache-miss 回源缓存、命中不回源。
  - **发布** `PUT /{repo}/{package}`：解析 npm publish 体（`_attachments` 内 base64 tarball、`versions` 内单一新版本、`dist-tags`），落 tarball 得真实摘要，生成 / 合并 packument，`dist.tarball` 指向本仓库、`dist.shasum` 用 sha1（hex）、`dist.integrity` 用 `sha512-<base64>`。
  - **FR-61 覆盖 / 不可变**：已发布版本不可覆盖——重复 publish 同版本返回 409；packument（包级聚合文档）可随新版本更新，tarball（含 `/-/` 段）不可覆盖。
  - **FR-68 使用片段**：详情页给 `npm install --registry ...` 与 `.npmrc`（registry + `_authToken` 占位，不含真实凭据）接入片段。
  - **FR-69 多校验和**：tarball 落盘即算 sha256/sha1/md5/sha512，dist 暴露 npm 所需的 shasum（sha1）与 integrity（sha512）。
  - 授权经既有 authz 编排：发布需 write；读受 visibility / ACL；private 对无权一律 404（隐藏存在性）。
- 不做（范围外）：未发布版本的 unpublish / dist-tag 管理端点、npm search API、scoped registry 多 registry 路由、用户级 `npm login`（本批用预签发 API Token 经 `_authToken`）、Maven / Docker 格式。

## 3. 设计（怎么做）

### 模块结构（新增 `format/npm.rs`、`api/npm_routes.rs`，并在既有处登记）

- `format/npm.rs`（新增）：`NpmFormat` 实现 `Format` trait，并提供 npm 专属**纯函数**（便于穷举单测）：
  - `parse_publish(body)`：解析 publish 体为 `PublishRequest`（package / version / tarball_name / tarball 字节 / version manifest / dist-tags）；缺字段 / base64 失败返回 `NpmError::InvalidBody`。
  - `tarball_path(package, name)` → `{包名}/-/{文件}`；`merge_packument(existing, req, base, repo, sha1, sha512_b64)` 合并新版本并重写 dist，同版本已存在返回 `NpmError::VersionExists`；`rewrite_proxy_packument(upstream, base, repo)` 仅重写各版本 tarball URL 指向本代理（保留上游 integrity/shasum）。
  - trait 方法：`name()="npm"`；`parse_path` 复用 `normalize_repo_path`；`can_overwrite` 据是否含 `/-/` 段区分（tarball 不可覆盖、packument 可更新）；`content_type` tarball→octet-stream、其余→json；`usage_snippets` 给 install + .npmrc。
- `api/npm_routes.rs`（新增）：薄协议适配 handler——`publish`（① 版本不可变预检 409 不写 blob → ② 落 tarball 得摘要 → ③ 据摘要重写 dist 合并 packument 落定，失败回滚不留孤儿）、`get_packument`（hosted 读存储 / proxy 回源重写）、`get_tarball`（流式 / proxy cache-miss 回源）。publish 体经 `read_body_limited` 按上传上限缓冲（npm 发布体须整体解析，超限 413）。
- `api/format_routes.rs`（改）：catch-all `/{repo}/{*path}` 中据 `repo.format == "npm"` 把 PUT 分派到 `npm_routes::publish`、GET 据是否含 `/-/` 段分派到 tarball / packument；不在路由层写 npm 业务。
- `format/service.rs`（改）：新增 `fetch_upstream_doc(repo, rel_path, max_bytes)`——proxy 回源小型元数据文档（packument）到内存供服务端重写后返回（tarball 等大文件仍走流式 `get`）；带上限防超大响应撑爆内存。
- `format/mod.rs`（改）：注册表 `with_builtin()` 登记 `NpmFormat`；导出 `NpmError` / `NpmFormat` / `PublishRequest`。
- `api/mod.rs`（改）：登记 `mod npm_routes;`。

### 关键约束对齐

- **handler 薄**：协议适配在 `npm_routes`，机理在 `service`，纯函数在 `format/npm.rs`；不按格式名 if-else（经 trait 多态 + 注册表分派，仅在路由层据 `repo.format` 决定走 npm 协议端点）。
- **blob 先落盘再写索引**：tarball 经 `put_hosted` 流式落盘校验后再合并 packument；版本不可变预检在写 blob 之前。
- **锁外 IO / 流式**：proxy tarball 回源走既有单飞与流式链路；packument 回源经 `fetch_upstream_doc` 带上限缓冲。
- **scoped 包**：npm 以 `@scope%2Fname` 编码访问，axum 解码后 path 段即 `@scope/name`；存储键、tarball 路径、dist URL 均按解码后的包名拼接。上游 npmjs 对 packument / tarball 均接受未编码斜杠，故回源直接透传 rel_path。
- **packument 来源**：hosted packument 作为一条制品记录存储（路径即包名），发布时读出旧文档、合并新版本后整体落定（可更新覆盖）；不依赖运行期动态拼装，避免与索引双真源。
- **凭据脱敏**：`.npmrc` 使用片段仅给 `_authToken=${NPM_TOKEN}` 占位，不写真实 Token。

### 对齐的 ADR

- ADR-0005（仓库类型 hosted/proxy）、ADR-0003（Bearer/Basic 鉴权，npm `_authToken` 即 Bearer）、ADR-0004（授权模型）：本批为既定决策的 npm 落地，未引入新决策，故不新增 ADR。

### 本批新增依赖

- 无新增。复用既有 `base64`（发布体 tarball 解码）、`serde_json`（packument）、`sha1`/`sha2`/`digest`（测试对账，生产摘要由 BlobStore 算）。

## 4. 任务拆分

- [x] format/npm：`NpmFormat` trait 实现 + 纯函数（parse_publish / tarball_path / merge_packument / rewrite_proxy_packument），配套单测（packument 生成 / scoped / 合并 / 版本不可变 / 代理重写 / 使用片段）。
- [x] api/npm_routes：publish / get_packument / get_tarball 薄 handler + hex→base64、版本判定辅助，配套单测。
- [x] api/format_routes：npm 读写分派（PUT→publish、GET 据 `/-/` 分派）。
- [x] format/service：`fetch_upstream_doc` 代理回源 packument 文档（带上限）。
- [x] format/mod + api/mod：注册 npm、登记路由模块。
- [x] HTTP 集成测试（tests/npm_api.rs）：发布→packument→tarball 端到端、两版本合并 latest 更新、scoped 包编码 URL 往返、重复发布 409、无写权限 403、private 无权 404、proxy 回源 packument 重写 + tarball cache-miss→hit（真实 mock registry 走 HttpUpstream）。
- [x] 文档同步：本规格。（PRD 状态 / CHANGELOG 由主控统一处理；API / ARCHITECTURE 按需加 npm 段。）

## 5. 验收标准

- `cargo build` 成功。
- `cargo test` 全绿（含 lib 单测 15 个 npm 用例 + tests/npm_api.rs 7 个集成用例），覆盖高风险区：
  - **格式协议正确性（§2.2）**：packument 生成 / scoped 包 / tarball 路径 / integrity（sha512-base64）与 shasum（sha1-hex）正确；hosted + proxy 分别验证。
  - **覆盖 / 不可变（§2.2/FR-61）**：重复 publish 同版本 409；packument 可更新、tarball 不可覆盖。
  - **代理缓存（§2.3）**：proxy 回源 packument 重写 tarball 指向本仓库（保留上游 integrity）；tarball cache-miss→hit 不重复回源。
  - **检索 / 鉴权（§2.1）**：发布需 write（无权 403）；private 对无权读 404。
- `cargo clippy --all-targets -- -D warnings` 无警告。
- **实机（需用户确认通过）**：npm（node v24）+ 端口 18151 + 临时数据目录 + 临时 `.npmrc`（registry 指向 `http://127.0.0.1:18151/{repo}`，`_authToken` 用首启 admin 签发的 API Token）：
  - hosted：临时包 `npm publish` 成功 → 另目录 `npm install` 成功且 integrity 通过；重复 `npm publish` 同版本失败（客户端与服务端 409 均拒）；scoped 包 `@jian/scoped-verify` publish + install 往返成功。
  - proxy：指向 `registry.npmjs.org`，`npm install is-number` 经代理 cache-miss→install 成功（integrity 通过）、第二次 tarball 命中缓存不回源（服务日志仅一条"已回源并缓存"）。
- `#![forbid(unsafe_code)]` 生效；注释 / 日志中文分级；流式与锁外 IO 落实；凭据 / 密钥不入日志 / 响应 / DB 明文。

## 6. 风险 / 待定

- npm publish 体须整体缓冲解析（含 base64 tarball），受 `limits.max_artifact_size` 约束并映射 413；超大 tarball 的纯流式发布非本批范围（npm 协议本身把 tarball 内嵌进 JSON）。
- packument 描述等文本字段原样透传 publish 体内容；存储为 JSON 后按 Unicode 转义序列化，语义无损（仅日志 / 裸 JSON 查看时为转义形式）。
- proxy packument 不缓存（每次回源以保证版本索引新鲜），仅 tarball 走缓存；与 ARCHITECTURE 代理缓存语义一致（缓存不可变的 tarball、不缓存易变的索引文档）。
