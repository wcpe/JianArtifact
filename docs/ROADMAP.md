# 版本路线图：JianArtifact

> 版本落地的**单一真源**：把 PRD §7 的分期（M1–M5）映射为从 `0.1.0` 到 `1.0.0-rc` 的具体版本。PRD §4 FR 表登记"做什么 + 状态"，本文登记"哪个版本交付什么 + 整期验收门"。每个版本发布均须过整期验收门禁（测试 + 真机证据）后，才由 `sdd-release-version` 把对应 FR 标 `已交付@vX.Y.Z`。

## 版本总览

| 版本     | 里程碑 | 主题                                 | 覆盖 FR         |
| -------- | ------ | ------------------------------------ | --------------- |
| 0.1.0    | M1     | 工程基座与契约骨架 + 部署骨架        | FR-01–05, FR-24 |
| 0.2.0    | M1     | 认证授权 + 管理端骨架                | FR-06–12        |
| 0.3.0    | M1     | 三格式协议真连（Raw/Maven/npm）      | FR-13–19        |
| 0.4.0    | M1     | Nexus 迁移域                         | FR-20–23        |
| 0.5.0    | M1     | M1 整期验收（首个稳定 MVP）          | 全 M1 复验      |
| 0.6.0    | M2     | 高频格式扩展 + 工程能力              | FR-25–33        |
| 0.7.0    | M3     | 企业认证、存储与运维                 | FR-34–48        |
| 0.8.0    | M4     | 安全、供应链与运营可见性             | FR-49–56        |
| 0.9.0    | M5     | 长尾格式与成熟期能力                 | FR-57–62        |
| 1.0.0-rc | RC     | 发布候选：兼容矩阵回归 + semver 冻结 | FR-65           |

> FR-63（签名发布/SBOM/Scorecard）、FR-64（Fuzz）为**贯穿项**：作为质量审查机制的一部分，在各版发布时持续增强，不单独占版本。

## M1 — 可迁移的核心制品库（MVP），跨 0.1.0–0.5.0

### 0.1.0 工程基座与契约骨架

- monorepo（pnpm workspace + Turborepo + Makefile + Go Task + go.work）；Docker 质量门；`api/openapi.yaml` 全域契约骨架 + `oapi-codegen` 接线；devmock 契约比对；单二进制 embed 打通。
- **部署骨架先落地可演练**：Dockerfile / docker-compose / .env.example / 远程部署脚本。
- 门禁：质量门在 Docker 与 CI 同脚本全绿；契约↔devmock 一致；`/readyz` 可探活。
- 状态：**开发中**（工作树内五项验收门 AC-0.1.0-1~5 均已自测通过，详见 `docs/specs/0.1.0-foundation.md`；待整期收敛后由 `sdd-release-version` 打版并标 `已交付@v0.1.0`）。

### 0.2.0 认证授权 + 管理端骨架

- 管理员自举、登录 / 会话 / 退出、API Token、CLI 鉴权、用户管理、口令修改 / 重置、仓库可见性 / ACL / 仓库管理。
- web 全页面对 devmock 完成、wiki 组件验收。
- 门禁：鉴权 / ACL 越权用例全拒（401/403）；令牌不入库不进日志。

### 0.3.0 三格式协议真连

- Raw / Maven / npm 的 hosted + proxy + group；通用能力（浏览、使用片段、健康检查 / 就绪自检）；single-flight + 大文件流式；内容寻址 blob + 校验。
- 门禁（实机·须用户确认）：curl / mvn / npm 原生客户端互通验收通过；并发与大文件测试穷举通过。

### 0.4.0 Nexus 迁移域

- 三来源发现（在线 REST / 离线原生目录 / 自有离线包）、计划预览、异步任务状态机、持久化与幂等续传、冲突策略、凭据安全、迁移报告、切换与最终增量扫描、迁移向导真连。
- 门禁（实机·须用户确认）：从真实 Nexus OSS 迁移，制品与元数据零丢失、可续传、报告一致。

### 0.5.0 M1 整期验收（首个稳定 MVP）

- PRD §6 全部验收标准通过；NFR 数值（大文件 / 缓冲 / 并发 / 内存 / 磁盘 / 迁移规模）实机达标并冻结基线；真实 Nexus OSS 3.70.x 三路径验收通过。
- 门禁：M1 全部 FR 经 `sdd-accept-phase` 整期验收通过后统一标 `已交付@v0.5.0`。

## M2 — 高频格式扩展 + 工程能力 → 0.6.0

- Docker/OCI（hosted/proxy/group）、Cargo sparse registry、PyPI、Go modules proxy、NuGet；全局跨仓库搜索；上述格式的 Nexus 迁移扩展。
- 新增（对标 NORA）：声明式按格式启停（未启用格式零路由零后台任务、缩小攻击面）；arm64 Linux 二进制。
- 前置门禁：M1（0.5.0）全部真机验收通过。

## M3 — 企业认证、存储与运维 → 0.7.0

- OIDC、LDAP；用户组与更细粒度权限动作；S3 兼容对象存储后端；审计日志与 Prometheus 指标；备份与恢复；GC / 保留策略 / 配额 / 存储清理；出站代理与受控运行时管理（含热重载）。
- 新增（对标 NORA）：上游断路器；定时任务调度器；离线镜像 CLI（`mirror`）；存储后端迁移 CLI（local↔S3）+ `integrity verify` 离线全量校验；Helm Chart + K8s 清单；Windows 二进制。
- 说明：S3 后端与外部化存储须先立 ADR 再实现；Helm/K8s 采单实例 RWO 卷 + `Recreate`。

## M4 — 安全、供应链与运营可见性 → 0.8.0

- 多维限流、连接 / 并发限制、慢请求防护；IP 名单、异常检测、应用层防护；本地漏洞库镜像与坐标匹配；Docker 镜像层扫描；使用分析、容量趋势、缓存命中率与运维告警。
- 新增（NORA 免费、Nexus 付费的核心差异化）：Curation 内容治理链（blocklist/allowlist + namespace 隔离 + 最小发布年龄，enforce/audit 双模式、fail-closed）；完整性校验门（Hash Pin）+ 代理 digest 首见隔离；cosign 签名策略（上游拉取验签，不通过则阻断）。

## M5 — 长尾格式与成熟期能力 → 0.9.0

- RubyGems、Conan、Terraform、Ansible Galaxy、Dart/Flutter Pub 等长尾格式；高级虚拟仓库策略与跨成员 metadata 聚合；多节点只读扩展或 HA 可选方案（先独立 ADR）；外部数据库 / 分布式存储可选方案（不破坏默认轻量形态）。
- 新增（对标 Nexus）：deb/rpm（apt/yum）Linux 系统包仓库；Webhooks（制品事件通知）+ 仓库内路径路由规则 / content selector。

## 发布候选 → 1.0.0-rc

- 全格式与迁移兼容矩阵回归；文档 / 运维 / 升级手册完善（含从任意 0.x 升级路径的 migration guide）；semver 契约冻结（API / 配置格式 / 存储布局稳定保证）；`integrity verify` 全量制品校验通过；性能与安全基线冻结；长跑稳定性验收通过后发 `1.0.0-rc`，为正式 `1.0.0` 铺路。

## 产品供应链（贯穿全期）

- release 产物签名（cosign / 校验和）+ 生成 SBOM + 接入 OpenSSF Scorecard（FR-63）。
- 对协议解析 / 路径校验等入口做 Fuzz 模糊测试并纳入 CI 门禁（FR-64）。
- 多架构 / 多平台发布矩阵（arm64 + Windows）产物经校验。

## 与参考项目的架构差异声明

NORA 采「文件系统即真源、无数据库、启动重建内存索引」；JianArtifact 定 **SQLite 元数据真源 + 文件系统 blob**（向前追加迁移，见 ADR-0002）。二者各有取舍，本项目不重开此决策，仅在 ARCHITECTURE 标注对比。

> 版本粒度原则：M1 因体量大跨 5 个 minor（0.1.0–0.5.0）；M2–M5 各占 1 个 minor（0.6.0–0.9.0）。后续如某期过大，可在 PATCH / 额外 minor 细分并更新本路线图，整体主线仍收敛到 1.0.0-rc。
