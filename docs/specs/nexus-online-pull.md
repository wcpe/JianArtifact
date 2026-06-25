# 功能规格：Nexus 在线拉取制品迁移（Maven hosted）

> 状态：开发中　·　关联 PRD：FR-82　·　分支：feature/nexus-online-pull

## 1. 背景与目标

ADR-0006 决策的「在线 REST API 入口」原意是「经其 REST API **读取并搬运**」，但既有实现（FR-36/38/39）只做了：在线 REST **发现/预览** + **离线 blob store** 搬运制品本体。结果是：当源 Nexus 仍在线、但运维**拿不到其磁盘 blob store 目录**（如迁移一台远程/他人维护的 Nexus）时，只能预览、无法搬运制品。

本功能补齐 ADR-0006 在线入口的「搬运」部分：**在源 Nexus 在线时，经其 REST API 枚举制品并通过 HTTP 逐个下载、流式落入本系统**，无需离线 blob store 目录。属 P2（Nexus 迁移能力随已实现格式扩展）。

## 2. 需求（要什么）

- 新增**在线拉取迁移**模式，与既有**离线目录**模式并列（迁移页二选一）。
- 在线拉取：给定源 Nexus base URL + 可选凭据引用 + 选中的源仓库，经 `service/rest/v1/components?repository=X`（`continuationToken` 分页）枚举该仓库全部 asset，按各 asset 的 `downloadUrl` HTTP 流式下载，经既有 `ingest_hosted` 落为本系统 hosted 制品。
- **文件一致**：搬运后的制品（含 `.pom` / `.jar` 及其 `.sha1/.md5/.sha256/.sha512` sidecar——Nexus 把它们作为独立 asset 暴露）与源**字节一致**；下载内容的 sha256 须与 Nexus 报告的 `checksum.sha256` 一致，不一致即视为损坏、回滚该制品并计跳过。
- **目标仓库名可自定义**：默认与源同名，允许指定不同的本系统仓库名；同名已存在则复用。
- 凭据真源仍在 env（`auth_ref` 引用），绝不入库、不进日志。
- 范围内：**Maven（`maven2`）hosted** 仓库的在线拉取。
- 不做（范围外）：proxy / group 仓库在线拉取；npm/nuget 等其余格式的在线拉取（机制通用，后续按需扩展）；离线模式改动（保持不变）。

## 3. 设计（怎么做）

对齐 ADR-0006（在线入口读取并搬运），**不新增 ADR**——本功能是其既定决策的补齐实现，机制细节记于本规格。

- `migrate` 模块：
  - `NexusClient` trait 扩展两法：`fetch_components(base_url, repo, continuation_token, credential) -> JSON 文本`（分页枚举）；`download_asset(download_url, credential) -> Box<dyn AsyncRead>`（流式下载，不整体载入内存）。
  - 纯函数 `parse_components(body) -> (Vec<NexusAsset>, Option<continuation_token>)`：取 `items[].assets[]` 的 `path` / `downloadUrl` / `checksum.sha256`，与顶层 `continuationToken`。
  - `migrate_online_repositories(client, meta, artifacts, formats, base_url, credential, selections, max_size)`：逐选中仓库——仅 `maven2` + `hosted` 参与（其余计入 `skipped_repos`）→ 建/复用目标 hosted 仓库（名取 `target_repo`）→ 分页枚举 asset → 逐 asset 流式下载 + `ingest_hosted` 落于 `asset.path` → 落定后比对 sha256，不符则删除该制品并计跳过；单 asset 失败记 WARN 跳过、不中断整批。
- `api` 层（薄）：新增 `POST /api/v1/migrate/nexus/online/migrate`（仅 Admin），请求 `{ base_url, auth_ref?, repositories: [{ source, target? }] }`，编排 discover → 过滤选中 → `migrate_online_repositories`，返回迁移报告。
- 下载流式：reqwest `bytes_stream()` 经 `tokio_util::io::StreamReader` 适配为 `AsyncRead` 喂给 `ingest_hosted`（既有依赖，不新增）。
- 前端：迁移页「选源与预览」增加**迁移方式**单选（在线拉取 / 离线目录）；在线拉取执行走新端点，可填目标仓库名。
- 依赖方向不变：`migrate` 仅依赖 `config` 级以下；`api` 薄编排。锁外做 IO，流式不整体载入内存。

## 4. 任务拆分

- [ ] 扩展 `NexusClient` trait + `HttpNexusClient`（components 分页 + asset 流式下载）
- [ ] `parse_components` 纯函数 + 单元测试（分页、asset 字段、空页、非法）
- [ ] `migrate_online_repositories` + 集成测试（mock client：建仓/复用、字节一致、sha256 不符回滚、目标改名、非 maven/非 hosted 跳过、单条失败不中断）
- [ ] API 端点 `online/migrate` + 请求/响应类型 + 鉴权（Admin / 匿名 401）
- [ ] 前端迁移页在线拉取模式（方式单选 + 目标仓库名 + 执行 + 报告）
- [ ] 文档同步：PRD 状态、ARCHITECTURE（migrate 在线搬运）、API.md（新端点）、CHANGELOG

## 5. 验收标准

- 单元：`parse_components` 穷举（多页 `continuationToken`、asset 的 path/downloadUrl/sha256、空 items、非法 JSON）。
- 集成（mock NexusClient）：在线拉取建/复用 hosted 仓库、按 asset.path 落制品**字节一致**、下载 sha256 与报告不符时回滚并计跳过、目标仓库改名生效、非 `maven2`/非 hosted 仓库整体跳过、单 asset 失败不中断整批。
- 鉴权：`online/migrate` 仅 Admin，匿名 401、普通用户 403。
- **真机（需用户确认通过）**：对一个真实在线 Nexus 实例在线拉取某 maven2 hosted 仓库的制品到本系统，校验取回字节与源一致、sidecar 齐备、目标仓库可经 `mvn` 拉取。worktree/CI 内以 mock 覆盖协议，真机由用户在单实例上复验。

## 6. 风险 / 待定

- 大仓库在线拉取耗时/带宽大：本批同步执行、逐 asset 流式；超大仓库的后台任务化不在本批。
- 源 Nexus components API 分页字段（`continuationToken`）与 asset 结构以真实 Nexus 实测为准（已确认含 sidecar asset 与四校验和）。
- sha256 不符的回滚依赖既有删除路径；并发同仓库在线拉取不在本批保证（单次迁移为管理员手动触发）。
