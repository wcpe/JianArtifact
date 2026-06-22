# 范围纪律（防范围漂移 / 镀金）

> 依据 `docs/PRD.md` 的分期。**只做当前阶段该做的，不提前做、不顺手做。**

## 1. 第一期（MVP）只做

- 四种高频格式（均含 hosted + proxy）：Maven、npm、Docker/OCI、Raw 通用文件。
- 认证：本地用户名/密码（argon2 哈希）登录，Web 会话/JWT；API Token 签发/列表/吊销（哈希存储，供 CLI 使用）；Basic Auth 鉴权（兼容包管理器 CLI）。
- 角色与权限：全局角色 Admin / User；每仓库可见性 public / private；每仓库读/写 ACL；匿名仅能读 public 仓库，private 对匿名一律拒绝；写操作校验对应仓库写权限。
- 仓库管理：创建/配置/删除仓库（格式、类型 hosted/proxy、可见性）；hosted 直传与下载；proxy 代理上游并缓存；仓库列表/详情/制品浏览。
- React Web 控制台：登录与基础仪表盘、仓库管理界面、用户与权限管理界面、Token 管理界面、制品浏览/搜索界面。基础仪表盘只展示基础信息，不含使用分析 / 访问下载统计等富数据面板。
- 存储与运行：SQLite 元数据 + 文件系统 blob 存储；单一二进制打包（前端嵌入）+ TOML 配置 + env 覆盖（前缀 `JIANARTIFACT_`）；健康检查端点。
- 首个管理员引导：空库首启从环境变量或随机口令创建首个管理员（不开放公开自助注册）。
- 制品写入语义：制品删除（需写权限）与各格式覆盖/不可变策略（Maven release 不可覆盖、snapshot 可覆盖；npm 已发布不可覆盖；Docker tag 可覆盖；Raw 可覆盖）。
- 接口约定：列表分页与搜索（统一 offset/limit + 分页响应结构）、会话/JWT 生命周期与刷新（含 /me）、登录暴力破解防护（失败锁定/限流）、上传大小限制（可配置 + 413）。
- 制品查看与检索：制品详情查看、跨仓库制品搜索/索引（结果按读权限过滤）、按格式生成的使用方式片段。
- 多校验和：每个制品计算并提供 sha256 / sha1 / md5 / sha512（含格式 sidecar）。

此清单是"该做什么"的权威边界，凡不在其中的能力都属越界。

## 2. MVP 严禁出现（属后续阶段）

属 P2/P3、提前实现或预留占位即为镀金，必须删除或停下来问：

- 格式实现或占位：Cargo、PyPI、Go、NuGet、RubyGems、Terraform、Ansible Galaxy、Pub（Dart/Flutter）、Conan 任一格式。
- S3 兼容对象存储后端。
- 企业认证：OIDC 集成、LDAP 集成。
- 可观测性：审计日志、Prometheus 指标端点、速率限制。
- Nexus OSS 迁移：在线 REST API 入口、离线 blob store 入口、proxy/hosted 制品搬运。
- group/virtual 聚合仓库。
- 垃圾回收 GC 与保留策略。
- 备份与恢复工具。
- 权限增强：用户组/团队、读/写之外的细粒度权限动作（delete / admin）。
- 七层防护增强：多维限流之外的并发/连接控制、慢速攻击防护、异常检测与自动封禁、IP 黑白名单、CC 挑战、WAF 规则引擎、防护监控告警。
- 使用分析：访问/下载统计采集与数据面板（富数据面板）。
- 漏洞库对接（离线镜像 + 坐标级匹配、漏洞标记）。
- Docker 镜像层 OS 漏洞扫描（P3）。

提前出现就算镀金的占位字段名（在代码 / 数据模型 / 契约中一旦出现即处置）：
`upstream_group_members`、`s3_bucket`、`oidc_issuer`、`ldap_url`、`migration_source`、`retention_policy`、`group_id`、`principal_type`、`permission='delete'|'admin'`、`waf_rule`、`challenge_token`、`ban_until`、`access_count`、`download_count`、`access_event`、`vuln_advisories`、`artifact_vulns`、`cve_id`、`osv_id`、`advisory_` 等。

一旦在代码 / 数据模型 / 契约里看到上述能力的提前实现或占位字段 → **删除，或停下来问**，不得镀金。

## 3. 不为未来预留空壳

- 不写"以后可能用"的抽象、配置项、接口、字段。需要时再加。
- 后续阶段能力到时按域新增包，当前不留占位。

## 4. 越界先问

- 若某任务看起来需要某个后续阶段能力才能完成 → **停止并向用户确认**，不自行扩大范围。
- 简洁方案优先：实现远多于必要（如 200 行 vs 50 行）时重写。资深工程师会觉得过度复杂的，就是过度。
