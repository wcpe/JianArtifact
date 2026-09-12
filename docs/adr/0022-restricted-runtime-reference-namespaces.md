# ADR-0022：受限运行时凭据与迁移来源引用命名空间

## 状态

已接受（online REST 迁移直接 URL 与加密任务凭据部分由 [ADR-0025](0025-direct-online-migration-url.md) 取代）

## 背景

ADR-0015 要求私有上游凭据和 online migration 来源只以不透明引用保存，但未限定引用可读取的进程环境变量范围。若接受任意环境变量名，管理员可将同步令牌、JWT 密钥或其他进程秘密配置为引用，再借由外部上游请求带出该值。

本决策取代 ADR-0015 第 4、6 项中“由运行时环境解析引用”的命名空间约束；协议鉴权、凭据认证方式、出站地址策略及审计身份等其余决策继续有效。

## 决策

1. `credentialRef` 和 `sourceRef` 都是 1–63 个字符的逻辑名称：首字符为大写 ASCII 字母，其余字符仅可为大写 ASCII 字母、数字或下划线。它们不是环境变量名、URL、主机名、IP、用户信息、Token 或用户可见标签。
2. 私有上游凭据仅从 `JIAN_UPSTREAM_CREDENTIAL_<credentialRef>` 读取；不得读取任意环境变量。`credentialRef` 仅允许 proxy 仓库配置，并可作为迁移任务的独立凭据引用保存。
3. online REST 迁移来源仅从 `JIAN_MIGRATION_SOURCE_<sourceRef>` 读取；不得使用 JSON 映射或任意环境变量回退。`sourceRef` 只出现在 online REST 的持久化 `sourceConfig` 中。
4. 持久化的 `sourceConfig` 采用封闭形状：`online_rest` 只保存 `{ "sourceRef": "<逻辑名称>" }`，`offline_dir` 和 `offline_bundle` 只保存 `{ "path": "<路径>" }`。来源 URL 只在运行时从受限来源变量解析，绝不写入任务、checkpoint、计划、报告、错误、日志或审计。
5. 缺失、空值或不合法引用必须在发起出站请求前安全失败。API、数据库、审计、日志和错误响应最多显示逻辑引用名或脱敏状态，绝不回显解析后的地址、用户名、口令、令牌、Authorization 头或其它环境变量值。
6. proxy 的 `remoteUrl` 必须是无用户信息、查询参数和片段的绝对 `http`/`https` URL。创建或更新时发现上述任一禁用部分即拒绝；读取历史遗留的不合法配置时不得在 API 输出中回显该 URL。
7. 所有经统一出站客户端的重定向在跨 origin（协议、主机或有效端口任一不同）时，必须在发送下一跳前剥离 `Authorization`；同 origin 重定向可保留认证头。online REST 迁移的网络或传输失败在任务、报告和日志中只能记录脱敏分类，不得包含来源 URL、主机名或 IP。
8. online REST 的 Nexus assets API 返回的 `downloadUrl` 是不可信响应数据。仅当其与受信 `sourceRef` 解析出的来源基址同 origin（协议、主机和有效端口均相同）时，迁移执行器才可发起下载并附加迁移凭据；跨 origin、无效或相对地址必须在请求前拒绝，不能把凭据发送给该地址。
9. 管理端 `POST /api/v1/migrations/remote-repositories` 只接受 `sourceRef` 作为在线来源定位，`credentialRef` 仍是可选的独立逻辑凭据引用；传入原始 URL、主机名或 IP 作为 `sourceRef` 必须拒绝，禁止该端点成为任意 URL 出站通道。管理端界面同样只填写逻辑 `sourceRef`。

## 理由

- 专用前缀将可被远端使用的值与进程其它秘密隔离，阻断通过管理配置外带同步令牌、JWT 密钥等无关秘密的路径。
- 严格的逻辑名称和封闭持久化形状可被 API、数据库、审计与恢复测试一致验证，也使重启恢复只依赖部署侧显式配置。
- 不增加新依赖或秘密存储系统，保留现有单二进制部署模型。

## 后果

- 部署私有上游时须将值放在 `JIAN_UPSTREAM_CREDENTIAL_<名称>`；在线迁移须将来源基址放在 `JIAN_MIGRATION_SOURCE_<名称>`，并在请求中传递相同逻辑名称。
- 旧的任意环境变量引用、`JIAN_MIGRATION_SOURCES` 映射和持久化 online URL 不再兼容；运营者须使用专用变量重新配置，已有不合规 online 任务不能依靠回写 URL 恢复。
- 所有协议 proxy、健康探测、online REST 发现、迁移下载和 resume 必须复用此解析边界与 ADR-0015 的统一安全出站客户端；迁移下载还必须满足同 origin `downloadUrl` 约束，不能仅依赖重定向时剥离认证头。

## 备选方案

- **允许任意环境变量名**：拒绝。无法阻止引用到无关进程秘密。
- **将凭据或来源地址加密后持久化**：拒绝。扩大密钥管理和备份攻击面，也违背现有运行时引用模型。
- **用单个 JSON 环境变量映射所有来源**：拒绝。映射本身扩大配置解析面，且无法提供更强的秘密隔离。
