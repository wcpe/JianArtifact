# 功能规格：Maven 格式（hosted + proxy）

> 状态：开发中　·　关联 PRD：FR-14 / FR-61 / FR-68 / FR-69　·　分支：feature/fr-14-maven

## 1. 背景与目标

在制品通用机理与统一 `Format` trait（Raw 参考格式已端到端验证）之上，接入第一期高频格式之一 **Maven**（hosted + proxy 两类）。让 `mvn` / `mvnd` 等官方客户端可对本服务执行 `deploy`（发布）与 `dependency:get` / 解析下载，proxy 仓库可镜像上游 Maven 仓库（如 Maven Central）并本地缓存。属阶段 P1。

本规格覆盖：Maven 仓库布局路径映射、覆盖 / 不可变策略（release 不可覆盖、SNAPSHOT 可覆盖、`maven-metadata.xml` 可更新）、校验和 sidecar（`.sha1` / `.md5` / `.sha256` / `.sha512`）、使用方式片段（`<dependency>` + settings.xml 接入）。**不含** npm / Docker 格式（FR-15/16，各自批次）与 Web 控制台。Maven 格式复用既有通用机理（存储 / 代理 / 单飞 / 四校验和 / 流式 / 授权），**不重造**这些机制。

## 2. 需求（要什么）

- 范围内：
  - **FR-14**：Maven 格式 hosted + proxy。
    - hosted：`mvn deploy`（PUT 主构件 + pom + sidecar 校验和）与解析下载（GET），按 Maven 仓库布局直存直取。
    - proxy：cache-miss 经既有 proxy 单飞从上游 Maven 仓库拉取 → 校验 → 缓存；命中不回源。
  - **FR-61**：Maven 覆盖 / 不可变策略——release 正式构件不可覆盖（重复部署同 GAV 返回 `409`）；SNAPSHOT 版本可覆盖（`200`）；`maven-metadata.xml` 随发布可更新；校验和 / 签名 sidecar 随主文件镜像、允许更新。
  - **FR-68**：使用方式片段——据制品路径反解 GAV 给 `<dependency>` 坐标片段，并给 settings.xml `<repository>` 接入片段（指向本仓库）。
  - **FR-69**：四校验和（sha256 / sha1 / md5 / sha512）由通用机理边写边算并入 artifacts 行；Maven 的 `.sha1` / `.md5` / `.sha256` / `.sha512` sidecar 作为独立文件由客户端 PUT、服务端逐文件存取（与通用机理一致，不二次聚合）。
- 不做（范围外）：npm / Docker 格式、Web 控制台、服务端主动生成 sidecar（Maven 客户端自带上传 sidecar，服务端逐文件存取即可）、Maven SNAPSHOT 元数据的服务端聚合 / 重写（客户端上传 `maven-metadata.xml`，服务端按文件存取并允许更新）、Maven group/virtual 聚合仓库（P2）。

## 3. 设计（怎么做）

### 模块改动（仅新增 `format/maven.rs` + 注册，不动通用机理）

- `format/maven.rs`（新增）：`MavenFormat` 实现统一 `Format` trait，仅负责 Maven 自身协议：
  - `parse_path`：复用 `normalize_repo_path` 把仓库内路径归一化并拒目录穿越（`.` / `..` / 空）；Maven 以归一化后的仓库内路径作为制品键。
  - `can_overwrite(existing)`：据既有制品路径判定——`maven-metadata.xml` 可更新；sidecar（`.sha1` / `.md5` / `.sha256` / `.sha512` / `.asc`）可更新；任一路径段以 `-SNAPSHOT` 结尾即为快照、可覆盖；其余为 release 正式构件、不可覆盖。判定为纯函数 `is_overwritable(path)`，便于穷举单测。
  - `content_type(coords)`：按 Maven 常见扩展名粗判（`.jar`/`.war`/`.ear` → java-archive，`.pom`/`.xml`/`maven-metadata.xml` → xml，`.module`/`.json` → json，sidecar / `.asc` → text/plain，`.zip`/`.tar`/`.gz` 等）；无法判断返回 None 交默认层。
  - `usage_snippets`：内部 `Gav::from_path` 据布局 `{group路径}/{artifactId}/{version}/{文件}` 反解 GAV（≥4 段），能解出则给 `<dependency>` 片段；恒给 settings.xml `<repository>` 接入片段（URL 去重斜杠指向本仓库）。
- `format/mod.rs`：`FormatRegistry::with_builtin` 增注册 `MavenFormat`；`pub use maven::MavenFormat`。
- `main.rs`：注释更新为"注册已实现格式（Raw、Maven）"，注册装配走 `with_builtin` 无额外改动。

### 复用的既有机理（不在本批重造）

- **路由**：`api/format_routes.rs` 的 `PUT/GET/DELETE /{repo}/{*path}` 按仓库 `format` 字段经注册表多态分发——Maven 自动套用，无需新增端点。
- **存储 / 事务**：`format/service.rs` 的 `put_hosted`（blob 先落盘校验 sha256 再写索引、失败回滚无孤儿、超限 413 流式）与 `get`（hosted 命中 / proxy cache-miss 单飞回源）原样复用。
- **proxy**：proxy cache-miss 用 `coords.path`（即 Maven 仓库内布局路径）拼到上游基址回源（`proxy/http.rs`），Maven 布局天然就是上游相对路径，无需特殊映射。
- **授权**：`authz` 编排原样生效——写需 write、读受 visibility/ACL、private 对无权 404、有读无写 403。
- **校验和 / 详情**：四校验和由通用机理算，制品详情 `GET /api/v1/repositories/{id}/artifacts/{*path}` 暴露 `checksums` 与 `usage`（Maven 片段）。

### 对齐的 ADR

- ADR-0005（仓库类型 hosted + proxy）：Maven 两类均落地于既有通用机理，未引入新决策，**不新增 ADR**。
- 路径映射、覆盖语义、sidecar 既有 API.md §3「格式 API 概览」已定（Maven 布局、release 409 / snapshot 覆盖、sidecar 暴露），本批为其落地，文档无需新增决策。

### 新增依赖

- 无。`sha1` 仅在集成测试中用于客户端独立计算 sidecar 校验和，已是既有运行期依赖（多校验和机理使用）。

## 4. 任务拆分

- [x] format/maven：`MavenFormat` 实现 `Format` trait（路径 / 覆盖 / content-type / GAV 反解 / 使用片段），配套单测（布局解析、release 不可覆盖、SNAPSHOT 可覆盖、metadata 可更新、sidecar 可覆盖、content-type、GAV 反解、使用片段）。
- [x] format/mod + main：注册表登记 Maven，注释更新。
- [x] HTTP 集成测试（tests/maven_api.rs）：release deploy→resolve 字节一致、release 重复 deploy→409、SNAPSHOT 覆盖→200、metadata 更新→200、sidecar 校验和与制品摘要一致、制品详情含依赖坐标片段、无写权限 deploy→403、private 对无权 GET→404、proxy cache-miss→hit（真实 HttpUpstream 走本地 mock 上游）。
- [x] 文档同步：本规格。PRD §4 FR-14 已预置「开发中」（主控统一收口，不在本批改状态）；API.md Maven 段已存在、与实现一致，无需改动；CHANGELOG 由主控整合时统一加。

## 5. 验收标准

- `cargo build` 成功。
- `cargo test` 全绿（含 Maven 布局解析 / metadata / 校验和 / 覆盖不可变单测 + tests/maven_api.rs 9 项 HTTP 集成），覆盖高风险区：
  - **各格式协议正确性（§2.2）**：Maven 布局路径解析、覆盖 / 不可变（release 409、SNAPSHOT 覆盖、metadata 更新、sidecar 覆盖）、sidecar 校验和与服务端边算摘要一致。
  - **代理缓存（§2.3）**：Maven proxy cache-miss → 回源一次 → hit 不再回源（走真实 HttpUpstream + 本地 mock 上游）。
  - **鉴权矩阵（§2.1）**：Maven 端点写需 write（无权 403）、private 对匿名 404 隐藏存在性。
- `cargo clippy --all-targets -- -D warnings` 无警告。
- **实机（需用户确认通过）**：`mvnd -s <临时 settings> deploy` 成功 → `mvnd -s <临时 settings> dependency:get` 成功且校验和一致；重复 release deploy → 409。proxy 可出网则验 Maven Central cache-miss → hit，否则 mock 上游验证并注明待出网复验。
- `#![forbid(unsafe_code)]` 生效；注释 / 日志中文分级；流式与锁外 IO 由复用的通用机理保证；凭据 / 密钥不入日志 / 响应 / DB 明文。

## 6. 风险 / 待定

- **SNAPSHOT 时间戳构件**：带时间戳的 SNAPSHOT 文件（如 `lib-1.0-20240101.120000-1.jar`）其版本目录仍以 `-SNAPSHOT` 结尾，故按路径段命中 `-SNAPSHOT` 即判可覆盖；服务端不重写 `maven-metadata.xml`，由客户端上传并允许更新。
- **proxy 出网复验**：Maven Central 真机 cache-miss → hit 需出网环境；离线时以本地 mock 上游走真实 `HttpUpstream` 链路验证，出网后由用户补一次真实上游复验。
- **服务端不生成 sidecar**：Maven 客户端 `deploy` 自带上传 `.sha1` / `.md5` 等 sidecar，服务端逐文件存取即可；制品本体的四校验和另由通用机理边写边算并在详情 / 下载头暴露，两者一致性已由集成测试断言。
