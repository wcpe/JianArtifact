# ADR-0011：npm registry 端点布局与 tarball URL 重写

## 状态

已接受

## 背景

0.3.0 补齐 npm registry 格式（FR-15）。npm 客户端以一个 registry 基址交互，端点形态与 Raw/Maven 的 `/repository/:repo/*artifactPath` 截然不同：

- packument（包元数据文档）：`GET <registry>/<pkg>`，`<pkg>` 可为 scoped（`@scope/name`，含 `/`）；
- tarball（压缩包）：`GET <registry>/<pkg>/-/<file>`；
- publish（发布）：`PUT <registry>/<pkg>`，请求体是含 `_attachments`（base64 tarball）的完整 packument 片段。

需要决定三件事：

1. **端点如何挂载**：npm 端点与既有 `/api/v1`（契约面）、`/repository`（Raw/Maven）、SPA 回退如何共存不撞车，且 scoped 包的 `/` 与 tarball 的 `/-/` 分隔如何在路由层解析。
2. **packument 与 tarball 如何存储**：是否引入 npm 专用表，还是复用内容寻址 blob + `asset` 元数据。
3. **tarball URL 如何对客户端呈现**：packument 内每个版本的 `dist.tarball` 指向何处，proxy/group 场景如何保证客户端经本服务拉取。

约束：复用 ADR-0009/0010 的 blob 内容寻址、`asset` 表与 `AssetService.Resolve` 读路径分派（hosted/proxy/group）；分层恒为 `protocol → domain → repository/blobstore/upstream → persistence`；publish 仍仅 hosted，proxy/group 写→409。

## 决策

1. **独立 `/npm` 前缀 + 单 catch-all 段**：npm 端点注册于 `/npm/:repo/*rest`（registry 基址即 `<server>/npm/<repo>/`），与 `/api/v1`、`/repository`、`/healthz` 前缀互不重叠、优先于 SPA 回退。`rest` 在 handler 内解析：含 `/-/` 分隔→tarball（`<pkg>/-/<file>`），否则→packument（`<pkg>`）。单一 catch-all 规避 gin 的通配/具名段冲突，并天然支持 scoped 包（gin 已解码 `%2F`）。
2. **复用内容寻址存储，不引入 npm 专用表**：packument 文档整体作为一件 asset 存于路径 `<pkg>`；每个 tarball 存于路径 `<pkg>/-/<file>`（与请求及上游 npm 布局一致）。publish 解码 `_attachments` 落各 tarball blob，与已存 packument 合并（versions/dist-tags/time 逐键，last-writer-wins）后覆盖写 `<pkg>`。读路径直接复用 `AssetService.Resolve`：hosted 本地、proxy 回源缓存、group 有序命中。
3. **服务端统一重写 `dist.tarball` 指向本仓**：所有 packument GET（hosted/proxy/group）在返回前把每个版本的 `dist.tarball` 重写为 `<请求基址>/npm/<本仓>/<pkg>/-/<原文件名>`。客户端遂始终经本仓拉取 tarball——proxy 仓 tarball GET 经 `Resolve` 回源上游 `<pkg>/-/<file>`，group 仓经有序命中回落各成员，无需客户端感知后端拓扑。group packument 另按成员顺序合并（versions 并集、dist-tags 首成员优先）。

## 理由

- `/npm` 前缀是 npm 私服的通行做法（registry 基址可带路径前缀），既隔离命名空间又让客户端仅需 `npm config set registry <server>/npm/<repo>/`。
- 复用 blob + asset 使 npm 与 Raw/Maven 共享去重、内容寻址完整性与 proxy/group 读路径，避免格式专用表与重复的回源/聚合逻辑。
- 统一 tarball 重写让「客户端经本仓拉取」成为不变式：proxy 缓存 tarball、group 聚合 tarball 均自动生效，且 packument 发布到何种基址都被服务端归一，健壮于反代与多环境。

## 后果

- packument 作为整份文档存取（非由 asset 逐版本组装）：结构简单、发布/合并直观，但服务端不深度理解版本语义（不做 semver 排序、不校验 shasum 一致性）。
- proxy packument 经 `Resolve` 缓存且无 TTL（同 ADR-0010）：上游发布新版本后，本仓 packument 在缓存 asset 被删除前返回旧副本，install 可能看不到最新版本；tarball 因内容寻址不受影响。
- group `dist-tags` 采首成员优先、versions 并集：跨成员同名版本以首个出现者为准，非语义化择新；tarball 经 group 有序命中，成员故障静默跳过（不暴露 502/504，同 ADR-0010）。
- 重写依赖请求基址（`X-Forwarded-Proto` + `Host`）：部署于反代后须正确透传该头，否则 `dist.tarball` 的 scheme/host 可能不符对外地址。

## 备选方案

- **npm 端点直接挂根路径（`GET /:pkg`）**：被否，与 `/api/v1`、`/repository`、SPA 回退强冲突，且 gin 根级 catch-all 会吞掉全部路由。
- **packument 由 asset 元数据逐版本动态组装**：被否（本增量），需完整建模 npm 版本/依赖/dist 语义与专用表，成本高；整份文档存取即可满足 publish+install 往返（FR-15）。
- **不重写 tarball、透传上游/成员原始 URL**：被否，proxy 场景客户端会绕过本服务直连上游（缓存与访问控制失效），group 场景成员地址对客户端泄漏且不可路由。
