# 接口契约：JianArtifact

> 对外接口的概览与定位。**管理 REST API 的唯一真源是 `api/openapi.yaml`**（设计优先，`oapi-codegen` 据其生成 Go 接口与前端 client）；本文只给概览，细节以契约文件为准。协议端点（Raw/Maven/npm）由各格式规范定义，不进 OpenAPI，但受同一鉴权 / ACL 保护。

## 1. 通用约定

- **管理 API 协议**：REST over HTTP，JSON 编解码，路径前缀 `/api/v1`。
- **认证**：
  - 会话：登录换取 JWT(HS256)，后续请求 `Authorization: Bearer <jwt>`。
  - 机器：API Token（`Authorization: Bearer <token>` 或格式客户端约定的方式），供 CLI / CI。
- **版本**：API 版本走路径前缀（`/api/v1`）；破坏性变更升版本并在 CHANGELOG 写迁移。
- **分页**：列表端点用 `page` / `page_size`（或 `cursor`），响应含总数 / 下一页游标。
- **编码**：请求 / 响应体 UTF-8 JSON；制品内容走原生协议端点（二进制流式）。

## 2. 错误约定

统一错误返回结构（JSON）：

```json
{ "error": { "code": "REPOSITORY_NOT_FOUND", "message": "仓库不存在" } }
```

- **HTTP 状态**：400 参数错误、401 未认证、403 越权、404 不存在、409 冲突、422 校验失败、429 限流、500 内部错误、502/504 上游代理错误。
- `code` 为稳定的机器可读枚举，`message` 为中文可读说明。协议端点的错误按各格式客户端期望的形态返回（如 Maven/npm 的原生错误语义）。

## 3. 端点 / 方法（概览）

> 完整参数与 schema 见 `api/openapi.yaml`。以下为分组概览。

### 认证与会话

- `POST /api/v1/auth/login`：用户名 + 口令 → JWT + 用户信息。
- `POST /api/v1/auth/logout`：注销当前会话。
- `POST /api/v1/auth/bootstrap`：首启管理员自举（仅在未初始化时可用）。

### 用户与令牌

- `GET/POST/PATCH/DELETE /api/v1/users`：用户管理。
- `POST /api/v1/users/{id}/password`：口令修改 / 重置。
- `GET/POST/DELETE /api/v1/tokens`：API Token 签发 / 列表 / 吊销。

### 仓库与 ACL

- `GET/POST/PATCH/DELETE /api/v1/repositories`：仓库管理（格式 raw/maven/npm、类型 hosted/proxy/group、可见性、上游 / 成员配置）。
- `GET/PUT /api/v1/repositories/{name}/acl`：读写 ACL。

### 制品浏览

- `GET /api/v1/repositories/{name}/browse`：浏览目录 / 组件。
- `GET /api/v1/repositories/{name}/usage`：使用片段（客户端接入示例）。

### Nexus 迁移（0.4.0；见 ADR-0012）

- `POST /api/v1/migrations/discover`：三来源发现；成功后 **落库为 `planned` 任务**，返回 `taskId` + 计划预览（不自动执行）。
- `POST /api/v1/migrations`：创建 **`planned`** 任务（含冲突策略 / plan 或 source）；不自动执行。
- `GET /api/v1/migrations` / `GET /api/v1/migrations/{id}`：列表与任务状态 / 进度。
- `POST /api/v1/migrations/{id}/start`：**显式启动**（`planned → running`）。
- `POST /api/v1/migrations/{id}/resume`：自 `failed`/`cancelled` 断点续传。
- `POST /api/v1/migrations/{id}/cancel`：取消 planned 或协作取消 running。
- `GET /api/v1/migrations/{id}/report`：迁移报告。
- 进程崩溃后残留 `running` 启动时标 `failed`，须 resume；仅 **admin**。

### 健康

- `GET /healthz`：存活探针。
- `GET /readyz`：就绪探针（SQLite + blob 目录自检）。
- `GET /api/v1/status`：运行时状态（版本、就绪、迁移版本、初始化标志、用户数），供 CLI `status` 与首启 web 设置页判定实例是否已初始化。

### 协议端点（非 OpenAPI，按格式规范）

- **Raw**：`GET/PUT/DELETE /repository/{repo}/{path}`。
- **Maven**：`GET/PUT /repository/{repo}/{group-path}/{artifact}/{version}/...`（含 `maven-metadata.xml`）。
- **npm**：`GET /repository/{repo}/{package}`、`PUT /repository/{repo}/{package}`（publish）、dist-tags 等。

> 端点随实现细化；新增 / 变更接口必须先改 `api/openapi.yaml` 再重生成，并同步本文与 CHANGELOG。
