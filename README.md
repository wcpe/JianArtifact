# JianArtifact

> JianArtifact 是一个用 Rust 编写、打包为单一可执行二进制的轻量级多格式制品库管理器：原生支持 13 种主流包格式，内置多用户认证、全局角色与每仓库 ACL，支持公开/私有仓库隔离与匿名访客，零外部运行时依赖。

## 状态

开发中 · v0.2.0

## 架构一览

后端采用 Rust + axum（基于 tokio 异步运行时）提供 HTTP 服务，元数据存于嵌入式 SQLite（经 sqlx 访问），制品 blob 本体落本地文件系统，DB 仅保存索引与 sha256。前端为 React + Vite + TypeScript（UI 组件库 Mantine），构建产物在编译期经 rust-embed 嵌入二进制，最终交付单一可执行文件，无需独立数据库、中间件或静态资源服务器。请求经认证中间件（识别 Bearer / Basic / 会话或匿名）与鉴权中间件（综合 public/private 可见性、全局角色与每仓库 ACL 判定）后，分流到管理 API 与各格式的原生协议端点。

## 能力

- 本地用户登录（用户名+密码，argon2 哈希）与 Web 会话/JWT。
- API Token 签发/列表/吊销（供 CLI 使用，哈希存储），并兼容包管理器的 Basic Auth 登录习惯。
- 全局角色 Admin / User，管理员可新增/禁用用户、调整角色。
- 仓库可见性 public / private，每仓库读/写 ACL；匿名仅能读 public 仓库，private 对匿名一律拒绝。
- 创建/配置/删除仓库（格式、类型 hosted/proxy、可见性），支持制品直传下载、上游代理与缓存、仓库列表/详情/制品浏览。
- 内置 Maven、npm、Docker/OCI、Raw 四种格式（均含 hosted 与 proxy），按各自原生协议挂载。
- React Web 控制台：登录与仪表盘、仓库管理、用户与权限管理、Token 管理、制品浏览/搜索。
- SQLite 元数据存储 + 文件系统 blob 存储；单一二进制打包（前端嵌入）+ TOML 配置 + 环境变量覆盖；提供健康检查端点。
- 首个管理员引导（空库自举）、会话生命周期与刷新、登录暴力破解防护、制品删除与按格式的覆盖/不可变策略、列表分页与搜索。
- 制品详情查看、跨仓库搜索、按格式的“使用方式”片段，以及每制品 sha256/sha1/md5/sha512 多校验和。

> 更多格式（Cargo、PyPI、Go、NuGet、RubyGems、Terraform、Ansible、Pub、Conan）、S3 兼容对象存储、企业认证（OIDC/LDAP）、Nexus 迁移、权限增强（用户组/细粒度权限）、七层（L7）防护、使用分析数据面板、漏洞库离线扫描等为后续分期能力，详见需求文档。

## 结构

后端按单向无环的分层组织为以下模块：

- `api`：axum 路由与中间件（认证、鉴权、请求 ID、统一错误处理），HTTP 层薄，不写业务。
- `auth`：认证——本地用户名/密码、Bearer Token、Basic Auth，并提供认证 provider 抽象边界。
- `authz`：授权——全局角色 + 每仓库可见性 + 每仓库读写 ACL 的判定。
- `repo`：仓库模型与生命周期（hosted/proxy 配置、可见性）。
- `format`：各格式处理器（maven/npm/docker/raw），经统一 trait 抽象注册。
- `proxy`：上游代理与缓存（拉取、落盘、单飞合并、上游失败回退）。
- `storage`：blob 存储抽象（本地文件系统）。
- `meta`：SQLite 元数据访问层（users / repositories / repo_acl / tokens / artifacts 索引）。
- `web`：React + Mantine 前端 + rust-embed 静态资源嵌入与服务。
- `config`：TOML 配置加载 + 环境变量覆盖。

依赖方向：`api` → (`auth`/`authz`/`repo`/`format`) → (`proxy`/`storage`/`meta`) → `config`；`format` 依赖 `storage`/`meta`/`proxy`。严禁反向依赖与环。

## 文档导航

- 需求：[`docs/PRD.md`](docs/PRD.md)
- 架构：[`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md)
- 接口：[`docs/API.md`](docs/API.md)
- 运维：[`docs/OPERATIONS.md`](docs/OPERATIONS.md)
- 配置：[`docs/CONFIG.md`](docs/CONFIG.md)
- 安全：[`SECURITY.md`](SECURITY.md)
- 决策：[`docs/adr/`](docs/adr/)
- 演进与维护：[`docs/CONTRIBUTING.md`](docs/CONTRIBUTING.md)
- 变更史：[`CHANGELOG.md`](CHANGELOG.md)

## 快速开始

最少步骤即可跑起来（命令为示意，路径一律相对）：

1. 构建单一可执行二进制：

   ```bash
   cargo build --release
   ```

2. 准备配置文件：复制示例配置 `config.example.toml` 为 `config.toml`，按需填写监听地址、数据目录等（敏感凭据用环境变量 `JIANARTIFACT_*` 覆盖，不要写入入库文件）。

3. 指定一个数据目录用于存放 SQLite 文件与制品 blob，例如 `./data`（首次启动会初始化）。

4. 启动服务：

   ```bash
   ./jianartifact --config ./config.toml --data-dir ./data
   ```

   首次启动会引导创建首个管理员：设置 `JIANARTIFACT_ADMIN_USERNAME` / `JIANARTIFACT_ADMIN_PASSWORD` 则据此创建，否则系统生成随机口令并打印到启动日志（请从日志取得并尽快改密）。系统不开放公开自助注册。

5. 访问健康检查端点确认运行正常：

   ```bash
   curl http://127.0.0.1:8080/health
   ```

   随后可在浏览器打开根路径 `/` 进入 Web 控制台。

## 约定

贡献流程、提交信息（含 scope 约定）与分支策略见 [`docs/CONTRIBUTING.md`](docs/CONTRIBUTING.md)。

## 许可

本项目采用 MIT 许可证，详见 [`LICENSE`](LICENSE)。
