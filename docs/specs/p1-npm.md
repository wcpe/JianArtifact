# 功能规格：npm 格式（FR-15）

> 状态：开发中　·　关联 PRD：FR-15　·　分支：feature/fr-15-npm

## 1. 背景与目标

为 JianArtifact 增加 npm registry 格式支持（hosted + proxy），使其能作为 npm 客户端的发布与安装源，复用通用制品机理（存储、多校验和、proxy 单飞缓存、授权）。属第一期（P1）。

## 2. 需求（要什么）

- packument 读取：`GET /{repo}/{package}`（含 scoped 包 `@scope/name` 的 URL 编码）。
- tarball 读取：`GET /{repo}/{package}/-/{tarball}.tgz`。
- 发布：`PUT /{repo}/{package}`，解析请求体的 metadata 与 `_attachments`（base64 tarball）、dist-tags，落盘 tarball 并生成/更新 packument（`dist.tarball` 指向本仓库 URL、`dist.integrity`/`shasum` 取通用机理算好的摘要）。
- 覆盖/不可变：已发布版本不可覆盖，重复 publish 同版本返回 409（npm 语义）。
- 鉴权：经既有 authz 强制——写需 write、读受 visibility/ACL、private 对无权返回 404。
- proxy：cache-miss 经既有 proxy 模块从上游（registry.npmjs.org）拉取 packument 与 tarball，缓存后服务。
- 使用方式片段：`npm install pkg@ver` 与 `.npmrc` 接入片段（registry 与 _authToken）。
- 范围外：npm 的 dist-tag 管理高级命令、deprecate、audit 等不在本期。

## 3. 设计（怎么做）

- 新增 `src/format/npm.rs` 实现统一 `Format` trait，在 `src/format/mod.rs` 注册表登记 `npm`；npm 专属路由在 `src/api/npm_routes.rs`，复用 `src/format/service.rs` 的通用存取/事务/校验和与 `src/proxy` 的单飞缓存。
- packument 由仓库内 artifacts 索引与 tarball 元数据动态拼装；tarball 按内容寻址存储，integrity 用 sha512 base64（npm 约定）+ 既有多校验和。
- 不在通用层堆 npm 专属分支，逻辑收敛在 npm Format 实现内。

## 4. 任务拆分

- [x] npm Format trait 实现 + 注册表登记
- [x] packument / tarball 读取路由
- [x] publish（PUT）解析与落盘、packument 生成、覆盖不可变（409）
- [x] proxy cache-miss 回源 npmjs
- [x] 协议单测 + `tests/npm_api.rs` HTTP 集成
- [x] 文档同步：本 spec（PRD 状态由主控预置；CHANGELOG 主控整合统一加）

## 5. 验收标准

- `cargo test` 全绿（packument 生成、scoped 包、tarball 路径、integrity/校验和、覆盖不可变 + HTTP 集成）。
- 实机：`npm publish` 到 hosted 仓库成功 → 另目录 `npm install` 成功且 integrity 校验通过；重复 publish 同版本 409。proxy 对 npmjs cache-miss→hit（出网时实测，否则 mock 上游验证、待出网复验）。
- clippy 无警告；`#![forbid(unsafe_code)]`；流式不整体载入内存；凭据不入日志/响应/DB 明文。

## 6. 风险 / 待定

- proxy 对 npmjs 的实机互通依赖出网环境；不可出网时以 mock 上游覆盖链路，真机待出网复验。
- packument 的 tarball URL 重写策略（直连上游 vs 指向本代理）按常见代理惯例处理。
