// Package internal 是 JianArtifact 后端的内部实现根。
//
// 分层与依赖方向（见 docs/ARCHITECTURE.md 与 .claude/rules/architecture-invariants.md）：
//
//	api        -> protocol, domain            HTTP 边界：Gin 路由、管理 API handler、鉴权中间件
//	protocol   -> domain                      Raw/Maven/npm 等协议适配（hosted/proxy/group）
//	domain     -> repository, storage, auth   领域逻辑：仓库、组件、ACL、迁移状态机
//	repository -> persistence                 元数据读写（SQLite，经 sqlx）
//	storage    -> (文件系统)                    内容寻址 blob 存储
//	auth       -> persistence                 认证授权：argon2、JWT、API Token
//	migration  -> domain, repository, storage Nexus 迁移域
//	persistence                               SQLite 连接、迁移、事务
//	config                                    横切：配置加载与校验
//
// 依赖只能自上而下，禁止反向与跨层直连（如 protocol 直接访问 SQLite）。
// 各子包随 M1 迭代落地。
package internal
