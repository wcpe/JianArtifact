# JianArtifact

> 自托管、单二进制交付的多格式制品仓库（artifact repository），用 Go 重写，支持从 Nexus OSS 平滑迁移。

## 状态

开发中 · v0.1.0（工程基座与契约骨架阶段）。版本路线图见 [`docs/ROADMAP.md`](docs/ROADMAP.md)（0.1.0 → 1.0.0-rc）。

## 架构一览

前后端同处一个 monorepo：Go 后端（Gin + sqlx + `modernc.org/sqlite`）承载协议端点、管理 API 与 Nexus 迁移，元数据落 SQLite、制品内容落文件系统 blob；React 管理端（Mantine 7 + i18next）构建产物经 Go `embed` 内嵌，**单二进制交付、零外部依赖**。API 以 `api/openapi.yaml` 为唯一真源，`oapi-codegen` 生成后端接口、前端生成 client、devmock 据同一契约比对防漂移。详见 [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md)。

## 能力

- 多格式仓库：Raw / Maven / npm 起步（hosted + proxy + group），逐步扩展 Docker/Cargo/PyPI/Go/NuGet 等。
- 从 Nexus OSS 迁移：在线 REST / 离线原生目录 / 自有离线包三来源，计划预览、幂等续传、冲突策略、迁移报告。
- 认证授权：管理员自举、JWT 会话、API Token、用户 / 仓库 / ACL 管理。
- 单二进制 / 单容器部署，Docker / Compose / Helm / rootless systemd 多路径。

## 结构

```
apps/{server,web,wiki}        后端 / 管理端 / 组件验收站
packages/{ui,devmock,eslint-config,typescript-config}  前端共享
api/openapi.yaml              API 契约唯一真源
deploy/                       Dockerfile / compose / .env.example / 部署脚本 / helm
docs/                         PRD / ARCHITECTURE / API / ROADMAP / ADR / specs / OPERATIONS
scripts/                      质量门与容器化开发入口
Makefile · Taskfile.yml       前端顶层入口 / 后端任务编排
```

## 文档导航

- 需求：[`docs/PRD.md`](docs/PRD.md)
- 架构：[`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md)
- 接口：[`docs/API.md`](docs/API.md)
- 路线图：[`docs/ROADMAP.md`](docs/ROADMAP.md)
- 运维：[`docs/OPERATIONS.md`](docs/OPERATIONS.md)
- 安全：[`SECURITY.md`](SECURITY.md)
- 决策：[`docs/adr/`](docs/adr/)
- 演进与维护：[`docs/CONTRIBUTING.md`](docs/CONTRIBUTING.md)
- 变更史：[`CHANGELOG.md`](CHANGELOG.md)

## 快速开始

> 代码实现随 M1 迭代逐步落地；当前为脚手架阶段。工具链就绪后：

```bash
make install        # 安装前端依赖（pnpm）
make dev            # 本地开发
make check          # 在 Docker 中跑全部质量门
make build          # 前端构建 + 后端 embed 编译单二进制
```

后端任务经 Go Task 编排（`task gen`=oapi-codegen、`task lint/test/vet/vuln`、`task build`）。部署见 [`docs/OPERATIONS.md`](docs/OPERATIONS.md)。

## 约定

提交、分支、文档同步等约定见 [`docs/CONTRIBUTING.md`](docs/CONTRIBUTING.md) 与 `.claude/rules/`。本项目遵循 SDD（规格驱动开发）：需求先行、文档即代码、跨会话不漂移。

## 许可

[MIT](LICENSE)。
