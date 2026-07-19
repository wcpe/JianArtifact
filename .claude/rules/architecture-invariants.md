# 架构不变量（JianArtifact）

> 以下是本项目锁定的架构约束（依据 `docs/ARCHITECTURE.md` 与 `docs/adr/`）。**违反任一条即为架构漂移。**
> 确需改变某条 → 先写新 ADR 取代旧决策、经确认后再改；**禁止在代码里静默违背**。

## 1. 后端分层与依赖方向（单向，不反向穿透）

后端 `apps/server/internal/` 按域分层，依赖方向单向向下，上层不得被下层反向依赖：

```
api（HTTP handler / 中间件）
  → protocol（Raw/Maven/npm 协议适配）
  → domain（仓库、制品、迁移等领域逻辑）
  → repository / storage / migration / auth（能力域）
  → persistence（SQLite 访问）+ blob 存储
config 为横切，供各层读取，不反向依赖业务层。
```

- **协议 handler 不得直接访问 SQLite**：必须经 domain / repository，不得在 `protocol` 或 `api` 层直连 `persistence`。
- **前端不得替代后端授权**：所有鉴权 / ACL 判定在后端完成；web / wiki 仅做展示与交互，不做权威授权决策。
- **禁止循环依赖**；禁止上帝包 / 跨层泄漏底层细节到上层。

## 2. 简单优先（禁用重型件）

当前阶段明确禁止引入以下重型组件，需要时先走 ADR：

- 外部数据库（PostgreSQL/MySQL 等）——元数据真源用**纯 Go SQLite（`modernc.org/sqlite`，无 CGO）**；外部 DB 属 M5 可选方案，需 ADR。
- 消息队列 / 分布式协调（Kafka/RabbitMQ/etcd/ZooKeeper 等）——异步任务用进程内状态机 + SQLite 持久化。
- 对象存储（S3 等）作为默认后端——默认 blob 存文件系统；S3 属 M3 可选后端，需 ADR。
- 重型 DI 框架、ORM 代码生成器以外的魔法框架——后端用 sqlx 手写 SQL，不上全功能 ORM。
- CGO / 系统级 C 依赖——交付形态是 `CGO_ENABLED=0` 单静态二进制，禁止引入需要 CGO 的库。

## 3. 真源与一致性约束

- **元数据真源 = SQLite**；**blob 内容真源 = 文件系统**（内容寻址）。二者不得互为权威、不得互相阻塞：元数据事务提交成功后 blob 才对外可见。
- **API 契约唯一真源 = `api/openapi.yaml`**（设计优先）。Go server 接口/类型由 `oapi-codegen` 从它生成，前端 client 与 devmock 据同一份比对；**不得手改生成产物、不得让代码与契约各说各话**。
- **版本号唯一来源 = 根 `VERSION` 文件**，构建注入前后端，恒一致。
- 前端 UI 组件真源在 `packages/ui`；`apps/web`、`apps/wiki` 消费它，**`packages/ui` 不得反向依赖 `apps/*`**。

## 4. 技术栈锁定

换栈 / 换框架 = 架构决策 → 走新 ADR，不擅自更换：

- **后端**：Go + Gin + sqlx + `modernc.org/sqlite`（纯 Go，无 CGO）+ argon2（口令哈希）+ JWT(HS256)。
- **前端**：React + TypeScript strict + Vite + Mantine 7 + @tabler/icons-react + i18next。
- **契约**：手写 OpenAPI + `oapi-codegen`。
- **交付形态**：前端构建产物经 Go `embed` 内嵌，单二进制交付。
- **工作区**：单 monorepo；前端 pnpm workspace + Turborepo + Makefile；后端 Go Task。

## 红线（出现即停止并先确认）

引入被禁重型件（外部 DB / MQ / S3 默认后端 / CGO 依赖）· 协议 handler 直连 SQLite · 前端替代后端授权 · 循环依赖或跨层反向穿透 · 手改 `oapi-codegen` 生成产物或让代码与 `openapi.yaml` 漂移 · 破坏「SQLite 元数据 / 文件系统 blob」真源边界 · 大文件整体入内存 · 持锁执行网络/磁盘 IO · 擅自换栈 · 静默违背任一已接受 ADR。
