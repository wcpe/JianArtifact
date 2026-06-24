# 功能规格：Go 模块格式（hosted + proxy）

> 状态：已实现（待集成 / 发版）　·　关联 PRD：FR-28（含 FR-61 / FR-68 / FR-69 的 Go 维度）　·　分支：feature/fr-28-go

## 1. 背景与目标

在制品通用机理与统一 `Format` trait（见 `p1-artifact-and-raw.md`）之上，按 Go 模块代理协议（GOPROXY 协议）接入 Go 格式，使官方 `go` 客户端可通过 `GOPROXY=http://host/{repo}` 直接 `go mod download` / `go get`。属阶段 P2，是第二期"扩格式（Cargo/PyPI/Go/NuGet）"中的一种。

Go 复用既有通用机理（流式存取、四校验和、blob 先落盘再写索引、proxy 单飞缓存、覆盖策略经 `Format::can_overwrite`），只新增 Go 自身协议适配：GOPROXY 端点分派、模块路径大小写 bang 编码（`!x` ↔ `X`）解析、`@v/list` 与 `@latest` 聚合文档生成、`.info` JSON 生成等纯函数。

## 2. 需求（要什么）

- 范围内（Go 维度，端点相对 `/{repo}/`，客户端配 `GOPROXY=http://host/{repo}`）：
  - **版本列表** `GET {module}/@v/list`：返回该模块所有版本，每行一个版本号（纯文本 `text/plain`）；无版本时返回空 200。
  - **版本元信息** `GET {module}/@v/{version}.info`：返回 JSON `{"Version":"v1.2.3","Time":"<RFC3339>"}`。
  - **go.mod** `GET {module}/@v/{version}.mod`：返回该版本 go.mod 文本（`text/plain`）。
  - **模块 zip** `GET {module}/@v/{version}.zip`：返回模块 zip（内部布局 `{module}@{version}/...`，`application/zip`）。
  - **最新版本** `GET {module}/@latest`：返回最新版本的 info JSON（按语义版本排序取最大；hosted 据已存版本，proxy 回源上游）。
  - **hosted 上传约定**（Go 无原生 publish，本项目据 GOPROXY 下载端点对称定义上传约定）：
    - `PUT {module}/@v/{version}.mod`：上传该版本 go.mod 文本。
    - `PUT {module}/@v/{version}.zip`：上传该版本模块 zip。
    - `PUT {module}/@v/{version}.info`：可选上传 info JSON；**若不上传**，服务端在首次取 `.info` / `@v/list` / `@latest` 时按"已存在 `.mod` 即视为该版本存在"补齐（`Time` 取该 `.mod` 制品的 `created_at`）。推荐至少上传 `.mod` 与 `.zip`。
    - 模块路径在 URL 中以 bang 编码表达大写（如 `GitHub.com/Foo` → `!git!hub.com/!foo`），存储键即解码前的 bang 形式（与 GOPROXY 磁盘缓存布局一致）。
  - **proxy**：代理上游 GOPROXY（如 `https://proxy.golang.org`）。`.info` / `.mod` / `.zip` 走既有 cache-miss → 回源 → 校验 → 落盘 → 写索引、命中不回源、并发单飞合并；`@v/list` 与 `@latest` 为易变聚合文档，每次回源透传（不缓存），与 npm proxy packument 不缓存语义一致。
  - **FR-61 覆盖 / 不可变**：Go 模块版本一经发布即不可变——同 `{module}@{version}` 的 `.mod` / `.zip` / `.info` 已存在即不可覆盖（重复 PUT 返回 409）。
  - **FR-68 使用片段**：详情页给 `GOPROXY=...` + `go get {module}@{version}` 接入片段。
  - **FR-69 多校验和**：`.mod` / `.zip` / `.info` 落盘即算 sha256/sha1/md5/sha512，下载暴露 `x-checksum-sha256`。
  - 授权经既有 authz 编排：上传需 write；读受 visibility / ACL；private 对无权一律 404（隐藏存在性）。
- 不做（范围外）：Go checksum database（`/sumdb/...`）协议与 `go.sum` 校验代理、`@v/{version}.ziphash`、GONOSUMCHECK 协商、模块版本伪版本（pseudo-version）的时间戳生成（按上传顺序记录即可）、其他格式。

## 3. 设计（怎么做）

### 模块结构（新增 `format/go_mod.rs`、`api/go_routes.rs`，并在既有处登记）

- `format/go_mod.rs`（新增）：`GoFormat` 实现 `Format` trait，并提供 Go 专属**纯函数**（便于穷举单测）：
  - `decode_bang(s)` / `encode_bang(s)`：bang 编码互转（`!x` ↔ `X`），非法 bang 序列（`!` 后非小写字母 / 末尾孤立 `!`）返回错误。
  - `parse_request(path)`：把仓库内路径解析为 `GoRequest` 枚举（`List{module}` / `Info{module,version}` / `Mod{...}` / `Zip{...}` / `Latest{module}`）；模块段经 bang 解码得规范模块路径，存储键仍用 bang 原形。
  - `version_storage_path(module_bang, version, ext)` → `{module_bang}/@v/{version}.{ext}`。
  - `build_info_json(version, time_rfc3339)` → `.info` JSON 字节。
  - `latest_version(versions)`：按 Go 语义版本（`semver`，预发布 < 正式）排序取最大版本号。
  - trait 方法：`name()="go"`；`parse_path` 复用 `normalize_repo_path`；`can_overwrite` 恒 `false`（Go 模块不可变）；`content_type` 据扩展名（.zip→application/zip、.mod/.info→见下）；`usage_snippets` 给 GOPROXY + go get。
- `api/go_routes.rs`（新增）：薄协议适配 handler——`get`（据 `parse_request` 分派到 list / info / mod / zip / latest）、`put`（仅接受 `.mod` / `.zip` / `.info`，经通用机理 `put_hosted` 落盘，不可变预检由 `can_overwrite=false` + 既有覆盖判定保证 409）。`@v/list` / `@latest` 据 `meta.list_artifacts_by_repo` 过滤本模块版本聚合（hosted）或回源透传（proxy）。
- `api/format_routes.rs`（改）：catch-all `/{repo}/{*path}` 中据 `repo.format == "go"` 把 GET / PUT 分派到 `go_routes`；不在路由层写 Go 业务。
- `format/mod.rs`（改）：注册表 `with_builtin()` 登记 `GoFormat`；导出 `GoError` / `GoFormat` / `GoRequest`。
- `api/mod.rs`（改）：登记 `mod go_routes;`。

### 关键约束对齐

- **handler 薄**：协议适配在 `go_routes`，机理在 `service`，纯函数在 `format/go_mod.rs`；不按格式名 if-else（经 trait 多态 + 注册表分派，仅在路由层据 `repo.format` 决定走 Go 协议端点）。
- **blob 先落盘再写索引**：`.mod` / `.zip` / `.info` 经 `put_hosted` 流式落盘校验后写索引；不可变预检由 `can_overwrite=false` 触发既有 `OverwriteForbidden`→409。
- **锁外 IO / 流式**：proxy `.zip` 等回源走既有单飞与流式链路；`@v/list` / `@latest` / `.info`（proxy）经 `fetch_upstream_doc` 带上限缓冲。
- **bang 编码**：URL 段含 `!x` 表大写；解码得规范模块路径用于聚合与展示，存储键保留 bang 原形（与上游 GOPROXY 缓存布局一致，回源 rel_path 直接透传 bang 形式）。
- **聚合文档不入双真源**：`@v/list` / `@latest` 不单独存储，hosted 据已存 `.info` / `.mod` 版本制品动态聚合，proxy 回源透传；避免与索引互为权威。
- **凭据脱敏**：使用片段仅给 GOPROXY 基址，不写真实 Token。

### 对齐的 ADR

- ADR-0005（仓库类型 hosted/proxy）、ADR-0003（Bearer/Basic 鉴权）、ADR-0004（授权模型）：本批为既定决策的 Go 落地，未引入新决策，故不新增 ADR。

### 本批新增依赖

- `zip`（已批准）：仅用于集成测试构造 / 校验模块 zip 内部布局（`{module}@{version}/...`）；生产路径把 zip 当不透明 blob 流式存取，不解压。

## 4. 任务拆分

- [x] format/go_mod：`GoFormat` trait 实现 + 纯函数（bang 编解码 / parse_request / version_storage_path / build_info_json / latest_version），配套单测。
- [x] api/go_routes：get（list/info/mod/zip/latest 分派）+ put（.mod/.zip/.info 不可变落盘）薄 handler，配套单测。
- [x] api/format_routes：Go 读写分派（GET/PUT 据 `repo.format == "go"`）。
- [x] format/mod + api/mod：注册 go、登记路由模块。
- [x] repo/mod：把 `go` 登记进 `SUPPORTED_FORMATS`，否则管理 API 无法创建 Go 仓库（集成测试经 `meta` 直建仓库未覆盖此入口，实机暴露后补齐）。
- [x] HTTP 集成测试（tests/go_api.rs）：上传 .mod/.zip/.info → @v/list / .info / .mod / .zip 取回字节一致、@latest 取最大版本、bang 编码模块路径往返、重复上传 409、无写权限 403、private 无权 404、proxy cache-miss→hit（真实 mock GOPROXY 走 HttpUpstream）。
- [x] Cargo.toml：新增 `zip`（仅集成测试构造 / 校验模块 zip 内部布局；生产把 zip 当不透明 blob）。
- [x] 文档同步：本规格、CHANGELOG、API.md、ARCHITECTURE.md 加 Go 段；PRD FR-28 行置“开发中”（待集成发版后由发版流程改“已交付@vX.Y.Z”）。

## 5. 验收标准

- `cargo build` 成功。
- `cargo test` 全绿（lib 单测 Go 用例 + tests/go_api.rs 集成用例），覆盖高风险区：
  - **格式协议正确性（§2.2）**：bang 编解码、`.info` JSON、`@v/list` 行格式、`@latest` 取最大版本、zip 字节一致；hosted + proxy 分别验证。
  - **覆盖 / 不可变（§2.2/FR-61）**：重复上传同版本 `.mod` / `.zip` 409。
  - **代理缓存（§2.3）**：proxy `.zip` cache-miss→hit 不重复回源；`@v/list` 回源透传。
  - **检索 / 鉴权（§2.1）**：上传需 write（无权 403）；private 对无权读 404。
- `cargo clippy --all-targets -- -D warnings` 无警告；`cargo fmt --check` 干净。
- **实机（已通过，go1.26.2 windows/amd64）**：`go` + 临时数据目录 + `GOPROXY=http://127.0.0.1:PORT/{repo}` + `GOSUMDB=off` / `GOFLAGS=-insecure`：
  - hosted：经管理 API 建 Go hosted 仓库 + 上传 `example.com/hello@v1.0.0` 的 `.mod` + `.zip`（均 201）→ 另目录 `go mod download example.com/hello`（依次取 `.info`/`.mod`/`.zip` 均 200）成功、`go build` 通过。
  - proxy：建指向本机 hosted 的 Go proxy 仓库，首次 `go mod download` 经代理 cache-miss 落盘成功、`go build` 通过；DB 核对 proxy 缓存的 `.mod`/`.zip` sha256 与 hosted 原件一致（`@v/list`/`@latest` 不入库）。
- `#![forbid(unsafe_code)]` 生效；注释 / 日志中文分级；流式与锁外 IO 落实；凭据 / 密钥不入日志 / 响应 / DB 明文。

## 6. 风险 / 待定

- Go 无原生 publish 协议：本批据 GOPROXY 下载端点对称定义 `PUT {module}/@v/{version}.{mod|zip|info}` 上传约定（见 §2）；若上游官方将来定义发布协议，按新约定演进。
- `@latest` 与 `.info` 的 `Time` 字段：hosted 未显式上传 `.info` 时取对应 `.mod` 制品 `created_at`（非模块真实提交时间），满足 `go` 客户端对字段存在性的要求即可。
- 伪版本（pseudo-version）与 sumdb 校验代理不在本批范围；客户端如需校验和数据库需自行配 `GONOSUMCHECK` / `GOFLAGS=-insecure` 或 `GONOSUMDB`。
- proxy `@v/list` / `@latest` 不缓存（每次回源保证版本索引新鲜），仅 `.mod`/`.zip`/`.info` 走缓存；与 ARCHITECTURE 代理缓存语义一致（缓存不可变文件、不缓存易变索引）。
