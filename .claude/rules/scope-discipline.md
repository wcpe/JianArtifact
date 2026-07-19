# 范围纪律（防范围漂移 / 镀金）

> 依据 `docs/PRD.md` 的分期。**只做当前阶段该做的，不提前做、不顺手做。**

## 1. 第一期（MVP）只做
第一期（M1，跨 0.1.0–0.5.0）范围以 `docs/PRD.md` §4 FR 表中标 P1 的条目与 `docs/ROADMAP.md` M1 章节为唯一权威边界，概括为：

- 工程基座与契约骨架：monorepo（pnpm workspace + Turborepo + Makefile + Go Task + go.work）、Docker 质量门、`api/openapi.yaml` 全域契约骨架 + `oapi-codegen` 接线、devmock 契约比对、部署骨架（Dockerfile / docker-compose / .env.example / 远程部署脚本）。
- 认证授权与管理端：管理员自举、登录/会话/退出、API Token、CLI 鉴权、用户管理、口令修改/重置、仓库可见性/ACL/仓库管理；web 全页面对 devmock、wiki 组件验收。
- 三格式协议真连：Raw / Maven / npm 的 hosted + proxy + group；浏览、使用片段、健康检查/就绪自检；原生客户端（curl/mvn/npm）互通。
- Nexus 迁移域：三来源发现、计划预览、异步任务状态机、持久化与幂等续传、冲突策略、凭据安全、迁移报告、切换与最终增量扫描。

> 凡不在 PRD §4 P1 FR 表内的能力都属越界。M2–M5 的能力（见下）在 M1 阶段一律不做。

## 2. MVP 严禁出现（属后续阶段）
M1（MVP）阶段严禁提前实现以下后续阶段能力（对应 PRD §4 中标 P2/P3 的 FR、`docs/ROADMAP.md` M2–M5）：

- **M2（0.6.0）**：Docker/OCI、Cargo、PyPI、Go modules proxy、NuGet 格式；全局跨仓库搜索；声明式按格式启停；arm64 二进制。
- **M3（0.7.0）**：OIDC/LDAP、用户组与细粒度权限；S3 对象存储后端；审计日志与 Prometheus 指标；备份恢复、GC/保留/配额；上游断路器、定时任务调度器、mirror 离线 CLI、存储迁移 + integrity verify CLI、Helm/K8s、Windows 二进制。
- **M4（0.8.0）**：多维限流、连接/并发限制、慢请求防护、IP 名单、异常检测；漏洞库镜像与坐标匹配、镜像层扫描；使用分析与运维告警；Curation 过滤链、完整性校验门 + digest 首见隔离、cosign 签名策略。
- **M5（0.9.0）**：RubyGems/Conan/Terraform/Ansible Galaxy/Dart Pub、deb/rpm；高级虚拟仓库策略；多节点只读/HA、外部数据库/分布式存储可选方案；Webhooks + 路由规则/content selector。

一旦在代码 / 数据模型 / 契约里看到上述能力的提前实现或占位字段（如 `s3_*`、`oidc_*`、`ldap_*`、`webhook_*`、`quota_*`、`rate_limit_*` 等）→ **删除，或停下来问**，不得镀金。


## 3. 不为未来预留空壳
- 不写"以后可能用"的抽象、配置项、接口、字段。需要时再加。
- 后续阶段能力到时按域新增包，当前不留占位。

## 4. 越界先问
- 若某任务看起来需要某个后续阶段能力才能完成 → **停止并向用户确认**，不自行扩大范围。
- 简洁方案优先：实现远多于必要（如 200 行 vs 50 行）时重写。资深工程师会觉得过度复杂的，就是过度。
