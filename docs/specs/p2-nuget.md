# 功能规格：NuGet 格式（hosted + proxy）

> 状态：已实现　·　关联 PRD：FR-29（含 FR-61 / FR-68 / FR-69 的 NuGet 维度）　·　分支：feature/fr-29-nuget

## 1. 背景与目标

在制品通用机理与统一 `Format` trait（见 `p1-artifact-and-raw.md`）之上，按 NuGet v3 协议接入 NuGet 格式，使官方 `dotnet` / `nuget` 客户端可直接 `nuget push` / `dotnet restore`。属阶段 P2（FR-29）。

NuGet 复用既有通用机理（流式存取、四校验和、blob 先落盘再写索引、proxy 单飞缓存、覆盖策略经 `Format::can_overwrite`），只新增 NuGet 自身协议适配：v3 服务索引生成、扁平容器（flat container）版本列表与 .nupkg / .nuspec 存取、`nuget push` 的 multipart 解析与 .nuspec 读取、proxy 回源服务索引 / 版本列表重写。

## 2. 需求（要什么）

- 范围内（NuGet 维度）：
  - **服务索引** `GET /{repo}/v3/index.json`：列出本仓库支持的 v3 资源（至少 `PackageBaseAddress/3.0.0` 扁平容器、`PackagePublish/2.0.0` 发布端点）。客户端 source 配 `http://host/{repo}/v3/index.json`。
  - **扁平容器版本列表** `GET /{repo}/v3-flatcontainer/{id-lower}/index.json`：返回该包所有已发布版本 `{"versions":[...]}`（由元数据索引动态生成，不另存聚合文档）。
  - **下载 .nupkg** `GET /{repo}/v3-flatcontainer/{id-lower}/{version-lower}/{id-lower}.{version-lower}.nupkg`：经通用机理流式返回；proxy cache-miss 回源缓存、命中不回源。
  - **下载 .nuspec** `GET /{repo}/v3-flatcontainer/{id-lower}/{version-lower}/{id-lower}.nuspec`：返回发布时从 .nupkg 内提取并落盘的 .nuspec。
  - **发布** `PUT /{repo}/v3/package`（`nuget push`）：`multipart/form-data` 内含 .nupkg（zip）。解压读取内嵌 `{id}.nuspec` 解析出 id / version；先落 .nupkg blob（流式落盘校验 sha256）再落 .nuspec，最后写索引。
  - **FR-61 覆盖 / 不可变**：同 id+version 已发布不可覆盖——重复 push 返回 409（NuGet 默认 server policy 不可覆盖）。
  - **FR-68 使用片段**：详情页给 `dotnet nuget add source` / `dotnet add package` 接入片段（source 指向 `/{repo}/v3/index.json`，凭据用占位不含真实 Token）。
  - **FR-69 多校验和**：.nupkg / .nuspec 落盘即算 sha256/sha1/md5/sha512，由通用机理计算。
  - 授权经既有 authz 编排：push 需 write；读受 visibility / ACL；private 对无权一律 404（隐藏存在性）。
- 不做（范围外）：`SearchQueryService`（搜索）、`RegistrationsBaseUrl`（注册/依赖图）等富资源（仅在服务索引中可选声明指向占位，本批不实现其端点）；NuGet v2（OData）协议；package delete / unlist 端点（删除走既有通用 DELETE）；符号包（.snupkg）；Maven / npm / Docker 等其他格式。

## 3. 设计（怎么做）

### 存储布局（小写规范化，对齐 NuGet flat container）

NuGet flat container 约定 id 与 version 均小写。仓库内存储键带 `v3-flatcontainer/` 前缀，与对外下载 URL 的仓库内段一致：

- .nupkg：`v3-flatcontainer/{id-lower}/{version-lower}/{id-lower}.{version-lower}.nupkg`
- .nuspec：`v3-flatcontainer/{id-lower}/{version-lower}/{id-lower}.nuspec`

存储键带前缀让 hosted 直传落键与 proxy 回源 rel_path 统一：proxy 仓库的 `upstream_url` 配为**上游服务根**（如 `https://api.nuget.org`），通用机理以 `{upstream_url}/{存储键}` 即可拼出上游扁平容器地址，无需为代理另设第二个基址，且 hosted / proxy 下载共用同一存储约定与同一下载 handler。

版本列表 `v3-flatcontainer/{id-lower}/index.json` **动态生成**：列出仓库内以 `v3-flatcontainer/{id-lower}/` 为前缀的 .nupkg 制品，提取各 version 段汇总。SQLite 仍是唯一真源，不另存聚合文档（避免双真源；与 npm packument 存储不同——NuGet 版本列表无需服务端合并语义，动态生成更简单）。

### 模块结构（新增 `format/nuget.rs`、`api/nuget_routes.rs`，并在既有处登记）

- `format/nuget.rs`（新增）：`NuGetFormat` 实现 `Format` trait，并提供 NuGet 专属**纯函数**（便于穷举单测）：
  - `normalize_id(id)` / `normalize_version(ver)`：小写规范化（flat container 约定）。
  - `nupkg_path(id, version)` / `nuspec_path(id, version)`：拼存储键。
  - `parse_nuspec(xml)`：从 .nuspec XML 解析 `<metadata>` 下 `<id>` / `<version>`，缺失 / 非法返回 `NuGetError::InvalidPackage`。
  - `read_nuspec_from_nupkg(nupkg_bytes)`：把 .nupkg 当 zip 打开，定位根级 `*.nuspec` 条目读出其字节（再交 `parse_nuspec`）。
  - `service_index(base, repo)`：生成 v3 服务索引 JSON（PackageBaseAddress + PackagePublish）。
  - `versions_index(versions)`：把版本字符串列表组装为 `{"versions":[...]}`（小写、去重、排序为纯函数）。
  - `rewrite_proxy_service_index(upstream, base, repo)`：把上游 v3 index 各 resource `@id` 重写为指向本代理（使经代理拉取扁平容器与版本列表）。
  - trait 方法：`name()="nuget"`；`parse_path` 复用 `normalize_repo_path`；`can_overwrite` 一律 false（.nupkg / .nuspec 已发布不可覆盖）；`content_type` 据扩展名（.nupkg→`application/octet-stream`、.nuspec / .json→对应类型）；`usage_snippets` 给 add source + add package。
- `api/nuget_routes.rs`（新增）：薄协议适配 handler——`publish`（解析 multipart 取 .nupkg → 读 .nuspec 得 id/version → 版本不可变预检 409 → 落 .nupkg → 落 .nuspec，失败回滚不留孤儿）、`get_service_index`（hosted 生成 / proxy 回源重写）、`get_versions_index`（hosted 动态生成 / proxy 回源重写或回退）、`get_nupkg` / `get_nuspec`（流式 / proxy cache-miss 回源）。
- `api/format_routes.rs`（改）：catch-all `/{repo}/{*path}` 中据 `repo.format == "nuget"` 把 PUT（路径 `v3/package`）分派到 `nuget_routes::publish`、GET 据子路径（`v3/index.json` / `v3-flatcontainer/...`）分派到对应端点；不在路由层写 NuGet 业务（分派函数仅做前缀匹配，业务在 `nuget_routes`）。
- `format/mod.rs`（改）：注册表 `with_builtin()` 登记 `NuGetFormat`；导出 `NuGetError` / `NuGetFormat`。
- `api/mod.rs`（改）：登记 `mod nuget_routes;`。

### 关键约束对齐

- **handler 薄**：协议适配在 `nuget_routes`，机理在 `service`，纯函数在 `format/nuget.rs`；不按格式名 if-else（经 trait 多态 + 注册表分派，仅在路由层据 `repo.format` 决定走 NuGet 协议端点；NuGet 内部子路由用前缀匹配的分派函数，非可变逻辑堆叠）。
- **blob 先落盘再写索引**：.nupkg / .nuspec 经 `put_hosted` 流式落盘校验后写索引；版本不可变预检在写 blob 之前（已存在 → 409 不写）。
- **锁外 IO / 流式**：proxy .nupkg 回源走既有单飞与流式链路；服务索引 / 版本列表回源经 `fetch_upstream_doc` 带上限缓冲。
- **id / version 小写规范化**：存储键、下载 URL、版本列表均按规范化后的小写键拼接，保证大小写不一致的客户端请求命中同一制品。
- **版本列表来源**：动态由元数据索引生成（列仓库内该包前缀的 .nupkg），SQLite 单一真源，不存聚合文档。
- **凭据脱敏**：使用片段仅给 `--api-key ${NUGET_API_KEY}` 占位，不写真实 Token。

### 对齐的 ADR

- ADR-0005（仓库类型 hosted/proxy）、ADR-0003（Bearer/Basic 鉴权，NuGet `--api-key` 即 API Token 经 Bearer）、ADR-0004（授权模型）：本批为既定决策的 NuGet 落地，未引入新决策，故不新增 ADR。

### 本批新增依赖

- `zip`：读取 .nupkg（zip 容器）内的 .nuspec 条目。手写 zip 解析不现实，且 .nupkg 即标准 zip。
- `quick-xml`：解析 .nuspec（XML）取 id / version。XML 含命名空间 / 实体 / CDATA，手写解析易错，采用轻量 quick-xml 保正确性。
- axum `multipart` 特性：解析 `nuget push` 的 `multipart/form-data` 上传体。
- 均为本批任务清单内预批准依赖；除此之外不新增。

## 4. 任务拆分

- [x] format/nuget：`NuGetFormat` trait 实现 + 纯函数（normalize / 路径拼接 / parse_nuspec / read_nuspec_from_nupkg / service_index / versions_index / rewrite_proxy_service_index），配套单测。
- [x] api/nuget_routes：publish / get_service_index / get_versions_index / get_flat_artifact（.nupkg / .nuspec）薄 handler。
- [x] api/format_routes：nuget 读写分派（PUT v3/package→publish、GET 据子路径分派）。
- [x] format/mod + api/mod：注册 nuget、登记路由模块；repo 生命周期支持的格式集合加入 nuget。
- [x] Cargo.toml：新增 zip / quick-xml 依赖、axum multipart 特性（任务清单内预批准）。
- [x] HTTP 集成测试（tests/nuget_api.rs）：push→下载 .nupkg 字节一致、重复 push 409、服务索引 / 版本列表正确、.nuspec 解析、proxy cache-miss→hit、写授权 403、private 无权 404、四校验和一致。
- [x] 文档同步：本规格、API.md、CHANGELOG、PRD 状态。

## 5. 验收标准

- `cargo build` 成功。
- `cargo test` 全绿（含 lib NuGet 单测 + tests/nuget_api.rs 集成用例），覆盖高风险区：
  - **格式协议正确性（§2.2）**：服务索引 / 版本列表生成正确；.nuspec 解析得 id / version；id / version 小写规范化；hosted + proxy 分别验证。
  - **覆盖 / 不可变（§2.2/FR-61）**：重复 push 同 id+version 409。
  - **代理缓存（§2.3）**：proxy 回源服务索引重写指向本仓库；.nupkg cache-miss→hit 不重复回源、单飞合并。
  - **检索 / 鉴权（§2.1）**：push 需 write（无权 403）；private 对无权读 404。
  - **多校验和（§2.2/FR-69）**：.nupkg 落盘四摘要由通用机理算且与独立计算一致。
- `cargo clippy --all-targets -- -D warnings` 无警告；`cargo fmt --check` 干净。
- **实机（需用户确认通过）**：若本机有 `dotnet` / `nuget`：起服务（临时端口 + 临时数据目录），hosted 临时包 `nuget push` 成功 → 另目录 `dotnet add package` / `restore` 成功；重复 push 同版本失败（409）；proxy 指向 nuget.org，`dotnet restore` 经代理 cache-miss→成功、第二次 .nupkg 命中缓存不回源。无 dotnet/nuget 则标「待真机验」。
- `#![forbid(unsafe_code)]` 生效；注释 / 日志中文分级；流式与锁外 IO 落实；凭据 / 密钥不入日志 / 响应 / DB 明文。

## 6. 风险 / 待定

- `nuget push` 体须整体缓冲解析 multipart（含 .nupkg 字节）以读取内嵌 .nuspec；受 `limits.max_artifact_size` 约束并映射 413。.nupkg 落 blob 仍走流式机理（从内存 multipart 字段读取）。
- proxy 服务索引 / 版本列表不缓存（每次回源以保证版本新鲜），仅 .nupkg 走缓存；与 ARCHITECTURE 代理缓存语义一致（缓存不可变的 .nupkg、不缓存易变的索引文档）。
- 上游 nuget.org 的服务索引中各 resource `@id` 指向 api.nuget.org / 子域，代理需逐 resource 重写为本仓库对应端点；本批仅重写已实现资源（PackageBaseAddress），其余按上游透传或省略。
- proxy `upstream_url` 配为上游**服务根**（如 `https://api.nuget.org`），服务索引回源 `v3/index.json`、版本列表与 .nupkg 回源 `v3-flatcontainer/...` 均相对该根，复用通用机理 `{upstream_url}/{rel}` 拼接，无需第二基址；存储键带 `v3-flatcontainer/` 前缀以与回源 rel_path 一致。
- nuget / dotnet 客户端对发布端点会补尾斜杠（`v3/package/`），路由分派按去尾斜杠后比较以兼容。
- Cargo.toml 属受保护构建配置：新增依赖需用户在对话中明确放行后再改（任务已预批准 zip / quick-xml / axum multipart）。
