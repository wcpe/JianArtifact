# ADR-0025：管理员直接填写在线 Nexus 迁移地址

## 状态

草拟；拟部分取代 ADR-0015 第 4、6 项与 ADR-0022 第 3、4、9 项对 online REST 迁移来源和凭据的限制。

## 背景

ADR-0022 规定 online REST 迁移仅能通过部署环境中的 `sourceRef` 定位来源；ADR-0015 规定迁移凭据仅能通过部署环境中的 `credentialRef` 读取。该机制适合长期固定上游，但一次性迁移外部 Nexus 时要求额外修改部署环境和重启服务，阻碍管理员完成迁移。

## 决策

管理员创建 online REST 迁移或查询远程仓库索引时，可提交无用户信息、无查询参数、无片段的绝对 `http` / `https` 来源 URL。任务持久化校验后的 URL，以支持异步执行、恢复和最终增量。

新请求可提交仅写入的 `sourceAuth`：匿名、Basic（用户名/密码或 Nexus User Token 的名称/Passcode）或 Bearer Token（JWT、Access Token、npm Token）。认证材料用随机 nonce 的 AES-256-GCM 密文保存到任务；明文认证类型可保存用于运行时分派。密钥由 `JIAN_MIGRATION_CREDENTIAL_KEY` 提供，未配置时在数据目录生成仅服务账户可读的独立密钥文件。

`sourceRef` 和 `credentialRef` 继续兼容已有任务；新 online REST 输入只能提供 `sourceRef` 或 `url` 之一。统一出站客户端的地址、DNS 重绑定、重定向和同 origin 下载保护不变；认证头仅允许发送给同 origin 目标。

来源 URL 仅向管理员任务配置返回。用户名、密码、Passcode、Token、密文与密钥不得写入 API 响应、浏览器草稿、报告、错误摘要、日志或审计 detail。

## 理由

该方式直接满足管理员迁移公有或私有外部 Nexus 的操作需求，同时复用现有出站安全边界和迁移状态机。只增加任务专用的加密凭据持久化，不引入来源管理或通用秘密管理系统。

## 后果

- OpenAPI、任务来源配置、在线发现/执行和管理端向导均需支持 `url` 和 `sourceAuth`。
- 直接 URL 任务可跨进程恢复，不依赖 `JIAN_MIGRATION_SOURCE_*` 或 `JIAN_UPSTREAM_CREDENTIAL_*`。
- 管理员应仅填写预期的外部 Nexus 基址；私网和危险地址仍由统一出站客户端拒绝。
- 迁移凭据密钥丢失或变更后，既有加密凭据无法恢复；任务必须明确失败并要求管理员重新创建，不得降级为匿名或回显密文。

## 备选方案

- **继续只允许 sourceRef**：拒绝。一次性外部迁移仍需修改部署环境。
- **新增来源白名单或来源管理页**：拒绝。超出当前直接导入需求。
- **仅在内存保存 URL**：拒绝。异步迁移无法恢复。
- **仅在内存保存凭据**：拒绝。任务恢复和最终增量无法访问私有仓库。
