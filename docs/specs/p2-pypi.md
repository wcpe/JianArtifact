# 功能规格：PyPI 格式（hosted + proxy）

> 状态：开发中　·　关联 PRD：FR-27（含 FR-61 / FR-68 / FR-69 的 PyPI 维度）　·　分支：feature/fr-27-pypi

## 1. 背景与目标

在制品通用机理与统一 `Format` trait（见 `p1-artifact-and-raw.md`）之上，按 PyPI Simple Repository API（PEP 503）与 legacy 上传协议接入 PyPI 格式，使官方 `pip` / `twine` 客户端可直接 `pip install`（含 proxy 镜像）与 `twine upload`（hosted 直传）。属阶段 P2，是 PRD FR-27。

PyPI 复用既有通用机理（流式存取、四校验和、blob 先落盘再写索引、proxy 单飞缓存、覆盖策略经 `Format::can_overwrite`），只新增 PyPI 自身协议适配：PEP503 项目名规范化、Simple HTML 页面生成（项目列表 / 文件列表，链接带 `#sha256=`）、multipart 上传体解析、上游 Simple 页面链接重写（指向本代理）。

## 2. 需求（要什么）

- 范围内（PyPI 维度）：
  - **Simple 根索引** `GET /{repo}/simple/`：返回 PEP503 HTML，每个已存项目一个 `<a>`（文本为规范化项目名，`href` 指向 `{project}/`）。proxy 回源上游 `/simple/` 项目列表 HTML（链接保持相对形式）。
  - **Simple 项目页** `GET /{repo}/simple/{project}/`：返回该项目所有发行文件的 PEP503 HTML，每个 `<a>` 文本为文件名、`href` 指向本仓库 `../../packages/{规范名}/{文件}#sha256=<hex>`。proxy 回源上游 `/simple/{project}/` 后把各文件链接重写为指向本代理的 `packages/...` 路径（保留 `#sha256=` 片段，校验照常）。
  - **JSON（PEP691，可选）**：Simple 两端点据 `Accept: application/vnd.pypi.simple.v1+json` 协商返回 JSON；默认（无该 Accept）返回 HTML。
  - **包文件下载** `GET /{repo}/packages/{规范名}/{文件}`：经通用机理流式返回；proxy cache-miss 回源缓存、命中不回源。
  - **上传（twine）** `POST /{repo}/`：解析 `multipart/form-data`（`:action=file_upload`、`content` 文件、`name`、`version`、`sha256_digest` 及其余 metadata 字段），落 wheel/sdist blob（先落盘校验 sha256 再写索引），存于 `packages/{规范名}/{文件}`；若客户端给了 `sha256_digest` 则与服务端算得的 sha256 对账，不符 400。
  - **FR-61 覆盖 / 不可变**：PyPI 已发布文件不可覆盖——同 `packages/{规范名}/{文件}` 已存在时返回 409。
  - **FR-68 使用片段**：详情页给 `pip install --index-url ...` 与 `twine upload --repository-url ...` 接入片段（凭据用占位，不含真实 Token）。
  - **FR-69 多校验和**：上传文件落盘即算 sha256/sha1/md5/sha512，Simple 页面 hash 用 sha256。
  - 授权经既有 authz 编排：上传需 write；读受 visibility / ACL；private 对无权一律 404（隐藏存在性）。
- 不做（范围外）：包删除 / yank 端点、PEP658 core-metadata sidecar、attestations / PEP740、`requires-python` 与 `data-yanked` 属性透传（仅在代理透传上游原属性，不主动生成）、用户级 `pip` 登录管理（用预签发 API Token 经 Basic）、其它格式。

## 3. 设计（怎么做）

### 模块结构（新增 `format/pypi.rs`、`api/pypi_routes.rs`，并在既有处登记）

- `format/pypi.rs`（新增）：`PypiFormat` 实现 `Format` trait，并提供 PyPI 专属**纯函数**（便于穷举单测）：
  - `normalize_project(name)`：PEP503 规范化——小写、`[-_.]+` 折叠为单个 `-`。
  - `package_path(project, filename)` → `packages/{规范名}/{文件}`；`project_of_package_path(path)` 反解规范名。
  - `parse_upload(boundary, body)`：解析 multipart 上传体为 `UploadRequest`（name / version / sha256_digest / filename / content 字节）；缺 `content` / 缺文件名返回 `PypiError::InvalidBody`。
  - `simple_index_html(projects)`：据规范名集合生成 PEP503 根索引 HTML；`simple_project_html(project, files)`：据 `(文件名, sha256)` 列表生成项目页 HTML（href 指向本仓库 packages 路径 + `#sha256=`）。
  - `simple_index_json(projects)` / `simple_project_json(...)`：PEP691 JSON 形态。
  - `rewrite_proxy_project_html(upstream_html, repo, project)`：把上游项目页各文件链接重写为指向本代理 `packages/{规范名}/{文件}`（保留 `#sha256=` 片段）。
  - trait 方法：`name()="pypi"`；`parse_path` 复用 `normalize_repo_path`；`can_overwrite` 一律 false（PyPI 已发布不可覆盖）；`content_type` 据扩展名（.whl / .tar.gz）；`usage_snippets` 给 pip / twine 接入。
- `api/pypi_routes.rs`（新增）：薄协议适配 handler——`upload`（multipart 解析 → 落 blob 校验 sha256_digest → 409 不可覆盖）、`simple_index`、`simple_project`（hosted 据存储文件生成 / proxy 回源重写）、`download`（流式 / proxy cache-miss 回源）。上传体经 axum `Multipart` 流式分块读取，单文件按上传上限约束（超限 413）。
- `api/format_routes.rs`（改）：catch-all `/{repo}/{*path}` 中据 `repo.format == "pypi"` 把 GET 分派——`simple/` → 根索引、`simple/{project}/` → 项目页、`packages/...` → 下载；不在路由层写 PyPI 业务。
- `api/mod.rs`（改）：新增 `POST /{repo}/`（twine upload，catch-all 的空 path 不匹配，故单列路由）与 `POST /{repo}/{*path}` 兜底；登记 `mod pypi_routes;`。
- `format/mod.rs`（改）：注册表 `with_builtin()` 登记 `PypiFormat`；导出 `PypiError` / `PypiFormat` / `UploadRequest`。
- `format/service.rs`：复用既有 `fetch_upstream_doc`（回源 Simple HTML 文档到内存供重写）与 `get`（包文件流式回源 + 缓存），不新增机理。

### 关键约束对齐

- **handler 薄**：协议适配在 `pypi_routes`，机理在 `service`，纯函数在 `format/pypi.rs`；不按格式名 if-else（经 trait 多态 + 注册表分派，仅在路由层据 `repo.format` 决定走 PyPI 协议端点）。
- **blob 先落盘再写索引**：上传文件经 `put_hosted` 流式落盘校验后写索引；不可覆盖预检由 `Format::can_overwrite` + service 把关。
- **锁外 IO / 流式**：proxy 包文件回源走既有单飞与流式链路；Simple HTML 回源经 `fetch_upstream_doc` 带上限缓冲。
- **项目名规范化**：存储键、Simple 链接、上传落盘路径均按 PEP503 规范名拼接，保证 `Foo_Bar` 与 `foo-bar` 指向同一项目。
- **Simple 来源**：hosted Simple 页面由存储文件实时枚举生成（不另存索引文档，避免与制品索引双真源）；proxy 每次回源上游 Simple（索引易变不缓存），仅包文件走缓存。
- **proxy 上游约定**：PyPI proxy 仓库的 `upstream_url` 指向索引服务**主机根**（如 `https://pypi.org`），服务端按 `simple/...` 相对路径回源（`Upstream::fetch` 以 `base + "/" + rel` 拼接，故 `upstream_url` 不含 `/simple/`，否则路径段重复）。
- **PEP658/714 sidecar 剥除**：本服务不提供 `.metadata` core-metadata sidecar（范围外）。proxy 重写上游 Simple 项目页时剥除 `data-core-metadata` / `data-dist-info-metadata` 属性——否则 pip 会据该属性去拉取本代理不存在的 `.metadata` 而 404 致安装失败；剥除后 pip 回退为下载完整 wheel（经代理缓存照常）。
- **凭据脱敏**：使用片段仅给占位凭据，不写真实 Token。

### 对齐的 ADR

- ADR-0005（仓库类型 hosted/proxy）、ADR-0003（Bearer/Basic 鉴权，twine / pip 用 Basic 带 API Token）、ADR-0004（授权模型）：本批为既定决策的 PyPI 落地，未引入新决策，故不新增 ADR。

### 本批新增依赖

- 启用 axum `multipart` 特性（已批准）：解析 twine 的 `multipart/form-data` 上传体。其余复用既有 `serde_json`（PEP691 JSON）、`sha2`/`sha1`/`digest`（测试对账，生产摘要由 BlobStore 算）。

## 4. 任务拆分

- [x] format/pypi：`PypiFormat` trait 实现 + 纯函数（normalize_project / package_path / parse_upload / simple_*_html / simple_*_json / rewrite_proxy_project_html / strip_metadata_attrs），配套单测。
- [x] api/pypi_routes：upload / simple_index / simple_project / download 薄 handler。
- [x] api/format_routes + api/mod：PyPI 读分派（simple / packages）、POST 上传路由登记。
- [x] format/mod：注册 pypi、导出类型；repo/mod 支持的格式集合加入 pypi。
- [x] Cargo.toml：启用 axum `multipart` 特性。
- [x] HTTP 集成测试（tests/pypi_api.rs）：twine 上传→下载字节一致、重复上传 409、摘要不符 400、Simple 列表含 sha256、规范化、JSON 协商、无写权限 403、private 无权 404、上传上限 413、proxy Simple 回源重写 + 包文件 cache-miss→hit（真实 mock 上游走 HttpUpstream）。
- [x] 文档同步：本规格、CHANGELOG、API/ARCHITECTURE/PRD。

## 5. 验收标准

- `cargo build` 成功。
- `cargo test` 全绿（lib 单测 PyPI 纯函数 + tests/pypi_api.rs 集成用例），覆盖高风险区：
  - **格式协议正确性（§2.2）**：PEP503 规范化 / Simple HTML（含 `#sha256=`）/ multipart 上传解析正确；hosted + proxy 分别验证。
  - **覆盖 / 不可变（§2.2/FR-61）**：重复上传同文件 409。
  - **代理缓存（§2.3）**：proxy 回源 Simple 重写文件链接指向本仓库；包文件 cache-miss→hit 不重复回源。
  - **检索 / 鉴权（§2.1）**：上传需 write（无权 403）；private 对无权读 404。
- `cargo clippy --all-targets -- -D warnings` 无警告；`cargo fmt --check` 干净。
- **实机（需用户确认通过）**：本机 python 3.12 + twine 6.2 + pip 已验通过——hosted：`twine upload`（wheel + sdist）成功 → 另 venv `pip install --index-url .../simple/` 成功并可 import；重复 upload 同文件失败（409）。proxy：`upstream_url` 指向主机根 `https://pypi.org`，`pip install six` 经代理 cache-miss→install 成功、二次安装命中缓存不再回源（服务端「已回源并缓存制品」计数不增）。
- `#![forbid(unsafe_code)]` 生效；注释 / 日志中文分级；流式与锁外 IO 落实；凭据 / 密钥不入日志 / 响应 / DB 明文。

## 6. 风险 / 待定

- twine 上传体须经 multipart 解析；wheel/sdist 文件以分块读入内存缓冲（受 `limits.max_artifact_size` 约束并映射 413）。纯流式 multipart 落盘非本批范围（multipart 分隔与字段交织，简化为按上限缓冲单文件字段）。
- proxy Simple 页面不缓存（每次回源以保证索引新鲜），仅包文件走缓存；与 ARCHITECTURE 代理缓存语义一致（缓存不可变的包文件、不缓存易变的索引）。
- 上游 PyPI 包文件实际托管在 `files.pythonhosted.org`（与 `pypi.org/simple` 不同主机）；本批 proxy 把上游项目页文件链接重写为本仓库 `packages/...`，包文件回源使用重写前提取的上游绝对 URL —— 经 service `get` 的相对路径回源模型仅适配「同主机相对路径」上游。故 proxy 包文件回源以「项目页内记录的上游绝对 URL」为准（见实现说明），不假设与 Simple 同主机。
