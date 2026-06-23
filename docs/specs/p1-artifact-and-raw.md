# 功能规格：制品通用机理与统一格式 trait + Raw 参考格式

> 状态：开发中　·　关联 PRD：FR-11 / FR-12 / FR-17 / FR-60 / FR-61 / FR-62 / FR-64 / FR-66 / FR-67 / FR-68 / FR-69　·　分支：feature/p1-artifact

## 1. 背景与目标

在仓库模型与授权层（Batch 2：判定"能否读写某仓库"）之上，立起第一期与具体格式无关的**制品通用机理**、**统一 `Format` trait**，并以 **Raw 通用文件格式**作为首个端到端实现验证整套机理。这是 Maven / npm / Docker 三种格式（Batch 4 按同一 trait 并行接入）与制品浏览 / 详情 / 搜索 UI（Batch 5）赖以工作的核心。属阶段 P1。

本规格覆盖：制品流式存取（hosted 直传下载、proxy 代理缓存）、统一格式 trait 与按名注册表、Raw 格式处理器、制品删除与覆盖策略、上传大小限制、制品详情（四校验和 + 使用方式片段）、跨仓库搜索（按读权限过滤）。**不含** Maven / npm / Docker 格式处理器（Batch 4 并行做）与 Web 控制台（Batch 5）——提前实现即越界（违反 scope-discipline）。

## 2. 需求（要什么）

- 范围内：
  - **FR-11**：hosted 仓库制品直传与下载（流式）。
  - **FR-12**：proxy 仓库代理上游并缓存（cache-miss 拉上游→校验→落盘→写索引；命中不回源；并发单飞合并一次回源；上游失败回退不写坏缓存）。
  - **FR-17**：Raw 通用文件格式（hosted + proxy，路径直存直取 `/{repo}/{path...}`）。
  - **FR-60**：制品删除（hosted 删本体 + 索引；proxy 删缓存，下次可重拉）。
  - **FR-61**：覆盖 / 不可变策略——通用机制经 `Format::can_overwrite` 暴露每格式策略；Raw = 同路径可覆盖。
  - **FR-62**：仓库内列表分页与搜索（沿用统一分页结构，跨仓库搜索端点落地）。
  - **FR-64**：上传大小限制（超 `limits.max_artifact_size` 返回 413，不留半截 blob）。
  - **FR-66**：制品详情（元数据 + 四校验和 + 所属仓库 / 格式 + 使用方式片段）。
  - **FR-67**：跨仓库搜索 `GET /api/v1/search`，结果严格按读权限过滤（匿名仅 public，不泄露无权 private 的存在 / 计数）。
  - **FR-68**：使用方式片段，经 `Format` trait 按格式 + `public_base_url` 生成；Raw 给 URL / curl。
  - **FR-69**：多校验和（sha256 / sha1 / md5 / sha512）入 artifacts 行并在详情 / 下载头暴露；sidecar serving 机制由 `Format` trait 声明，Raw 不 serve sidecar。
- 不做（范围外）：Maven / npm / Docker 格式处理器（FR-14/15/16，Batch 4）、Web 控制台（FR-18~22，Batch 5）、Maven 等格式的 sidecar serving（留各格式批次）、S3 后端、用户组 / 细粒度权限动作。

## 3. 设计（怎么做）

### 模块结构（新增 `format` / `proxy`，并在 `api` 上扩展）

- `format/mod.rs`（新增）：统一 `Format` trait——`name()`、`parse_path()`（归一化 + 拒目录穿越）、`can_overwrite(existing)`、`content_type(coords)`、`usage_snippets(base_url, repo, coords)`。`FormatRegistry` 按格式名注册并多态查得，**不在业务路径用 if-else / switch 按格式名分支**。`normalize_repo_path` 作各格式路径校验基础（拒 `.` / `..` / 空段）。本批仅注册 Raw。
- `format/raw.rs`（新增）：`RawFormat` 为 trait 首个实现——路径即制品键、允许覆盖、按扩展名粗判 content-type、给 URL / curl 使用片段。
- `format/service.rs`（新增）：与格式无关的通用 `ArtifactService<S: BlobStore, U: Upstream>`，编排 `put_hosted` / `get` / `delete`：
  - **blob 先落盘并校验 sha256（内容寻址即校验），再写元数据索引**；写索引失败回滚 blob（按 sha256 引用计数，无其他引用才删），不留孤儿索引 / 孤儿 blob。
  - 流式：`LimitedReader` 包裹请求体，超 `max_artifact_size` 在写入途中即报错映射 413，BlobStore 清理半截临时文件。
  - 覆盖策略经 `Format::can_overwrite` 判定（Raw 允许；其余格式各自语义）。
- `proxy/mod.rs`（新增）：`Upstream` trait（生产 `HttpUpstream`，测试注入计数 mock）、`UpstreamBody`（AsyncRead 流式）、`SingleFlight<T>` 单飞合并器——临界区只做 in-flight 归属判定（`Mutex<HashMap<Key, Weak<Shared>>>`），**实际拉取 / 落盘 / 写索引在锁外**由唯一 leader 跑一次，followers 等其结果；leader 失败不缓存"成功值"，下次重试。
- `proxy/http.rs`（新增）：`HttpUpstream` 基于 reqwest（纯 rustls 校验上游 HTTPS 证书、流式响应体），非 2xx 一律按上游错误，绝不把错误体当制品缓存；超时来自配置。
- `api/format_routes.rs`（新增）：Raw 格式端点 `PUT/GET/DELETE /{repo}/{*path}`，经 authz 强制（写需 write、读受 visibility/ACL、private 对无权 404、有读无写 403），handler 薄、流式 IO 下沉到 service。
- `api/artifacts.rs`（新增）：制品详情 `GET /api/v1/repositories/{id}/artifacts/{*path}`（四校验和 + 使用片段）与删除 `DELETE`（写授权）。
- `api/search.rs`（新增）：跨仓库搜索 `GET /api/v1/search`——先检索候选，再按读权限过滤（管理员见全部、其余仅 public + 自己有读 ACL 的 private），最后分页；total 取过滤后数量，**绝不经计数泄露无权制品**。
- `api/repo_access.rs`（新增）：把"查 ACL → 构造 RepoView → authorize → 按定式映射 404/403"集中复用，供仓库管理、制品详情 / 浏览、Raw 端点共用。
- `api/repositories.rs`：制品浏览端点接真实 `list_artifacts_by_repo` 数据。
- `api/mod.rs`：挂载格式 catch-all 路由、制品详情 / 删除、搜索；`AppState` 增 `artifacts`（通用机理服务）与 `formats`（注册表）；新增 `ServiceError` / `PathError` → `ApiError` 映射（413 / 409 / 502 等）。

### 关键机制（对齐 ARCHITECTURE §5）

- 代理缓存单飞、流式先落盘再写索引、多校验和、跨仓库搜索、使用方式片段均落地，与文档一致。

### 对齐的 ADR

- ADR-0005（仓库类型）：hosted + proxy 两类，proxy cache-miss 拉取 / 校验 / 落盘 / 写索引 + 单飞合并 + 上游失败回退。本批为其落地，未引入新决策，故不新增 ADR。

### 本批新增依赖（理由见 Cargo 注释）

- `reqwest`（default-features=false + `rustls-tls` + `stream`）：proxy 上游拉取，纯 rustls 避开 native-tls/openssl，守单一二进制零原生依赖。
- `tokio-util`（`io`）：把 reqwest 字节流适配为 AsyncRead 喂给 BlobStore 流式写入。
- `futures-util`：单飞与上游字节流的 Stream 适配。

## 4. 任务拆分

- [x] format/mod：`Format` trait + `FormatRegistry` + 路径归一化，配套单测。
- [x] format/raw：Raw 处理器（路径 / 覆盖 / content-type / 使用片段），配套单测。
- [x] proxy：`Upstream` trait + `HttpUpstream`（reqwest rustls 流式）+ `SingleFlight` 单飞合并，配套单测。
- [x] format/service：通用 `ArtifactService`（put_hosted / get / delete，先落盘再写索引、413、回滚无孤儿、proxy 单飞回源），配套单测（含单飞 12 并发只回源一次、上游失败不缓存、删缓存可重拉、共享 sha256 不误删）。
- [x] api/format_routes：Raw `PUT/GET/DELETE`，写授权边界 + 路径穿越拒绝。
- [x] api/artifacts + api/search + api/repo_access：制品详情 / 删除 / 跨仓库搜索（读权限过滤）。
- [x] api/mod + main：路由挂载、AppState 装配、错误映射。
- [x] HTTP 集成测试（tests/artifact_api.rs）：Raw 直传 / 下载 / 覆盖 / 删除、详情四校验和 + 使用片段、413、搜索读权限过滤、proxy cache-miss→hit→删→重拉（真实 mock 上游走 HttpUpstream）、上游不可用 502 不缓存。
- [x] 文档同步：本规格、PRD 状态（FR-11/12/17/60/61/62/64/66/67/68/69 改开发中）、CHANGELOG。

## 5. 验收标准

- `cargo build` 成功。
- `cargo test` 全绿（109 lib 单测 + 14 artifact 集成 + 22 auth 集成 + 16 repo_authz 集成 = 161 通过），覆盖六块高风险区：
  - **代理单飞（§2.3）**：12 并发同 key cache-miss → 上游恰好 1 次；命中不回源；上游失败不写坏缓存；删缓存后可重拉。
  - **流式与限制（§2.4）**：超 max_artifact_size → 413 且无半截 blob；blob 落盘内容寻址即校验。
  - **元数据事务（§2.5）**：blob 先落盘再写索引；写索引失败回滚无孤儿；共享 sha256 删一条不误删。
  - **多校验和（§2.2）**：四算法对 "abc" / 空内容标准向量正确，详情 API 暴露一致。
  - **检索鉴权过滤（§2.1）**：匿名 / 无权用户 search 私有仓库制品 → 结果与计数均不含、不泄露存在。
  - **Raw E2E**：curl PUT→GET（hosted，覆盖允许）、proxy cache-miss→hit（真实 mock 上游）、DELETE。
- `cargo clippy --all-targets -- -D warnings` 无警告。
- 实跑：起二进制，Raw hosted PUT→GET 字节一致 + 详情看四校验和；proxy 仓库对 mock 上游 cache-miss→hit；匿名 / 越权对 private 的 Raw GET → 404。
- `#![forbid(unsafe_code)]` 生效；注释 / 日志中文分级；流式与锁外 IO 落实；凭据 / 密钥不入日志 / 响应 / DB 明文。

## 6. 风险 / 待定

- Maven / npm / Docker sidecar serving 由各格式批次（Batch 4）按 `Format` trait 落地；本批 Raw 不 serve sidecar，四校验和已入 artifacts 行并在详情 / 下载头暴露。
- 仓库内列表分页参数（offset/limit/sort）当前浏览端点返回数组形态；跨仓库搜索已用统一分页结构；浏览端点的统一分页可在后续补齐时同步 API.md。
- 单飞键为 `仓库 id + 路径`，合并同一制品并发回源；跨仓库同内容不合并（各自独立 cache-miss，blob 按 sha256 去重）。
