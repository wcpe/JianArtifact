# 功能规格：Cargo 格式（hosted + proxy）

> 状态：开发中　·　关联 PRD：FR-26（含 FR-61 / FR-68 / FR-69 的 Cargo 维度）　·　分支：feature/fr-26-cargo

## 1. 背景与目标

在制品通用机理与统一 `Format` trait（见 `p1-artifact-and-raw.md`）之上，按 Cargo **稀疏索引（sparse registry）** 协议接入 Cargo 格式，使官方 `cargo` 客户端可直接 `cargo publish` / 依赖解析 / 下载。属阶段 P2 扩格式批次中的一种。

Cargo 复用既有通用机理（流式存取、四校验和、blob 先落盘再写索引、proxy 单飞缓存、覆盖策略经 `Format::can_overwrite`），只新增 Cargo 自身协议适配：稀疏索引文件（每版本一行 JSON）的生成 / 合并、按包名长度分目录的索引路径映射、`config.json` 生成、publish 体（`[4 字节 LE json 长度][metadata JSON][4 字节 LE crate 长度][.crate]`）解析，以及 yank / unyank 标记翻转。

## 2. 需求（要什么）

- 范围内（Cargo 维度）：
  - **registry 配置** `GET /{repo}/config.json`：返回 `{"dl":"{base}/{repo}/api/v1/crates","api":"{base}/{repo}"}`，把下载与 API 都指回本仓库（proxy 同样指回本仓库，使客户端经代理拉取一切）。
  - **稀疏索引** `GET /{repo}/{index_path}`：返回某包的索引文件（每行一个版本的 JSON：`name`/`vers`/`deps`/`cksum`=sha256(.crate) hex/`features`/`yanked`）。索引路径按包名长度分目录：1 字符 → `1/{name}`；2 字符 → `2/{name}`；3 字符 → `3/{name[0]}/{name}`；≥4 字符 → `{name[0..2]}/{name[2..4]}/{name}`（均小写）。proxy cache-miss 回源上游索引（不缓存，索引易变）。
  - **下载** `GET /{repo}/api/v1/crates/{name}/{version}/download`：返回 `.crate` 字节；经通用机理流式返回，proxy cache-miss 回源缓存、命中不回源。
  - **发布** `PUT /{repo}/api/v1/crates/new`：解析二进制 publish 体（4 字节 LE 长度前缀 + metadata JSON + 4 字节 LE 长度前缀 + `.crate` 字节）；落 `.crate` blob 得真实 sha256，把该版本追加进索引文件（`cksum` 用 sha256），返回 `{"warnings":{"invalid_categories":[],"invalid_badges":[],"other":[]}}`。
  - **yank / unyank** `DELETE /{repo}/api/v1/crates/{name}/{version}/yank`（置 `yanked=true`）、`PUT /{repo}/api/v1/crates/{name}/{version}/unyank`（置 `yanked=false`）：翻转索引行的 `yanked` 字段，不删 blob。返回 `{"ok":true}`。
  - **FR-61 覆盖 / 不可变**：同 `name`+`vers` 已发布不可覆盖（Cargo 语义不可覆盖）——重复 publish 同版本返回 409。索引文件随新版本追加更新（可覆盖落定）；`.crate` blob 不可覆盖。
  - **FR-68 使用片段**：详情页给 `cargo add {name}` 与 registry 接入片段（`.cargo/config.toml` 的 `[registries.xxx] index = "sparse+{base}/{repo}/"`，及凭据用 `cargo login` 占位、不含真实 Token）。
  - **FR-69 多校验和**：`.crate` 落盘即算 sha256/sha1/md5/sha512，索引 `cksum` 用 sha256（hex）。
  - 授权经既有 authz 编排：发布 / yank 需 write；读受 visibility / ACL；private 对无权一律 404（隐藏存在性）。
- 不做（范围外）：crates.io Web API（owners / search `GET /api/v1/crates`）、git 索引协议（仅稀疏索引）、registry 级别的多 registry 路由、用户级登录态（用预签发 API Token 经 `Authorization` 头）、其余 P2/P3 格式。

## 3. 设计（怎么做）

### 模块结构（新增 `format/cargo.rs`、`api/cargo_routes.rs`，并在既有处登记）

- `format/cargo.rs`（新增）：`CargoFormat` 实现 `Format` trait，并提供 Cargo 专属**纯函数**（便于穷举单测）：
  - `index_path(name)` → 按包名长度返回索引相对路径（`1/a`、`2/ab`、`3/a/abc`、`se/rd/serde`）。
  - `parse_publish(body)` → `PublishRequest`（name / vers / metadata JSON / crate 字节）；长度前缀越界 / JSON 失败返回 `CargoError::InvalidBody`。
  - `index_line(req, cksum)` → 据 metadata 与 sha256 生成一行索引 JSON（name/vers/deps/cksum/features/yanked=false）。
  - `merge_index(existing_lines, new_line, vers)` → 把新版本行追加进既有索引（按行的 JSON 文本）；同 `vers` 已存在返回 `CargoError::VersionExists`。
  - `set_yanked(existing_lines, vers, yanked)` → 翻转指定版本行的 `yanked`；版本不存在返回 `CargoError::VersionNotFound`。
  - `crate_storage_path(name, vers)` → `.crate` 在仓库内的存储键（`crates/{name}/{name}-{vers}.crate`）。
  - `config_json(base, repo)` → registry config.json 字节。
  - trait 方法：`name()="cargo"`；`parse_path` 复用 `normalize_repo_path`；`can_overwrite` 据存储路径区分（索引文件 `index/...` 可更新、`.crate`（`crates/...`）不可覆盖、`config.json` 可更新）；`content_type`（索引 / config → json、`.crate` → octet-stream）；`usage_snippets` 给 `cargo add` + 接入片段。
- `api/cargo_routes.rs`（新增）：薄协议适配 handler——
  - `get`（读分派）：据子路径判定 `config.json` / `api/v1/crates/{n}/{v}/download` / 索引文件，分别走 config 生成 / 下载（通用机理 `get`）/ 索引读取（hosted 读存储 / proxy 回源）。
  - `publish`：① 解析 publish 体 → ② 版本不可变预检（读既有索引含该版本 → 409，不写 blob）→ ③ 落 `.crate` 得 sha256 → ④ 生成索引行、合并索引、落定（失败回滚不留孤儿）。
  - `yank` / `unyank`：读既有索引、翻转 `yanked`、落定索引。
- `api/format_routes.rs`（改）：catch-all `/{repo}/{*path}` 中据 `repo.format == "cargo"`，PUT 据子路径分派（`api/v1/crates/new` → publish、`.../yank` → yank/unyank、其余索引文件 PUT 不支持）、GET 走 `cargo_routes::get`、DELETE `.../yank` → yank。沿用既有 npm 分派范式，不在路由层写 Cargo 业务。
- `format/mod.rs`（改）：注册表 `with_builtin()` 登记 `CargoFormat`；导出 `CargoError` / `CargoFormat` / `CargoPublishRequest`。
- `api/mod.rs`（改）：登记 `mod cargo_routes;`。

### 存储约定（仓库内路径）

- 索引文件：`index/{index_path}`（如 `index/se/rd/serde`），随发布追加更新（可覆盖落定）。
- `.crate` blob：`crates/{name}/{name}-{vers}.crate`，不可覆盖。
- `config.json` 不落存储，按请求动态生成（依赖对外基址，避免与配置双真源）。

### 关键约束对齐

- **handler 薄**：协议适配在 `cargo_routes`，机理在 `service`，纯函数在 `format/cargo.rs`；不按格式名 if-else（经 trait 多态 + 注册表分派，仅在路由层据 `repo.format` 决定走 Cargo 协议端点）。
- **blob 先落盘再写索引**：`.crate` 经 `put_hosted` 流式落盘校验后再合并索引；版本不可变预检在写 blob 之前。
- **锁外 IO / 流式**：proxy `.crate` 回源走既有单飞与流式链路；索引回源经 `fetch_upstream_doc` 带上限缓冲。
- **索引来源**：hosted 索引作为一条制品记录存储（路径 `index/{index_path}`），发布时读出旧文件、追加新版本行后整体落定（可更新覆盖）；不依赖运行期动态拼装，避免与索引双真源。
- **proxy 索引不缓存**：索引易变，每次回源以保证版本列表新鲜；仅 `.crate`（内容不可变）走缓存。与 ARCHITECTURE 代理缓存语义一致。
- **凭据脱敏**：接入片段仅给 `cargo login` 占位说明，不写真实 Token。

### Cargo 鉴权头（裸 Token）

Cargo registry 客户端把 API Token 裸放进 `Authorization` 头（`Authorization: <token>`，**无 Bearer/Basic scheme 前缀**），与既有 Bearer/Basic 通道不同。身份解析中间件（`api/identity.rs`）在无识别 scheme 前缀时，按 API Token 校验整个头值（哈希比对、命中授予身份、未命中回退匿名），从而让 `cargo publish` / `cargo yank` 用 API Token 鉴权。属 ADR-0003「兼容包管理器 CLI 鉴权」的延伸落地，未改授权模型、未引入新决策。

### 对齐的 ADR

- ADR-0005（仓库类型 hosted/proxy）、ADR-0003（Bearer/Basic 鉴权 + 兼容包管理器 CLI，Cargo 走裸 Token 头）、ADR-0004（授权模型）：本批为既定决策的 Cargo 落地，未引入新决策，故不新增 ADR。

### 本批新增依赖

- 无新增。复用既有 `serde_json`（索引行 / metadata）、`sha1`/`sha2`/`digest`（测试对账，生产摘要由 BlobStore 算）。publish 体的 4 字节 LE 长度前缀用标准库 `u32::from_le_bytes` 解析，无需额外库。

## 4. 任务拆分

- [x] format/cargo：`CargoFormat` trait 实现 + 纯函数（index_path / parse_publish / index_line / merge_index / set_yanked / crate_storage_path / config_json），配套单测。
- [x] api/cargo_routes：get（config/download/index 分派）、publish、yank/unyank 薄 handler，配套辅助单测。
- [x] api/format_routes：cargo 读写分派（PUT→publish/yank、GET→cargo_routes::get、DELETE→yank）。
- [x] format/mod + api/mod：注册 cargo、登记路由模块；repo 创建格式白名单纳入 cargo；前端格式选项加 Cargo。
- [x] HTTP 集成测试（tests/cargo_api.rs）：config.json、发布→索引→下载字节一致、重复发布同版本 409、yank/unyank 翻转、无写权限 403、private 无权 404、proxy 回源索引 + `.crate` cache-miss→hit（真实 mock 上游走 HttpUpstream）。
- [x] 文档同步：本规格 + PRD 状态 + CHANGELOG 未发布段；API.md 加 Cargo 段。

## 5. 验收标准

- `cargo build` 成功。
- `cargo test` 全绿（lib 单测 Cargo 用例 + tests/cargo_api.rs 集成用例），覆盖高风险区：
  - **格式协议正确性（§2.2）**：索引路径分目录规则、publish 体二进制解析、索引行 cksum（sha256-hex）、config.json 正确；hosted + proxy 分别验证。
  - **覆盖 / 不可变（§2.2/FR-61）**：重复 publish 同版本 409；索引可更新、`.crate` 不可覆盖。
  - **代理缓存（§2.3）**：proxy 回源索引（不缓存）；`.crate` cache-miss→hit 不重复回源。
  - **检索 / 鉴权（§2.1）**：发布 / yank 需 write（无权 403）；private 对无权读 404。
- `cargo clippy --all-targets -- -D warnings` 无警告；`cargo fmt --check` 干净。
- **实机（best-effort / 需用户确认通过）**：本机有 `cargo`，对 hosted 仓库经临时 `.cargo/config.toml`（sparse index 指向 `{base}/{repo}/`）`cargo publish` 并解析下载；无条件则如实标「待真机验」。
- `#![forbid(unsafe_code)]` 生效；注释 / 日志中文分级；流式与锁外 IO 落实；凭据 / 密钥不入日志 / 响应 / DB 明文。

## 6. 风险 / 待定

- Cargo publish 体须整体缓冲解析（含内嵌 `.crate` 字节），受 `limits.max_artifact_size` 约束并映射 413；超大 crate 的纯流式发布非本批范围（Cargo 协议本身把 `.crate` 内嵌进 publish 体）。
- 仅实现稀疏索引协议（`sparse+` 前缀），不实现 git 索引；现代 cargo（1.70+）默认稀疏索引，覆盖主流。
- proxy 索引不缓存（每次回源以保证版本列表新鲜），仅 `.crate` 走缓存；与 npm packument 处理一致（缓存不可变本体、不缓存易变索引）。
- yank 仅翻转索引 `yanked` 标记、不删 `.crate`（Cargo 语义：yank 后仍可被已有 lockfile 下载，只是不参与新解析）。
