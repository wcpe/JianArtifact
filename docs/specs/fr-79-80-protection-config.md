# 功能规格：防护配置 API 与管理页面（FR-79 / FR-80）

> 状态：开发中　·　关联 PRD：FR-79、FR-80　·　分支：feature/fr-79-protection-config

## 1. 背景与目标
ADR-0008 内置的七层（L7）应用层防护（多维限流 FR-33/FR-51、慢速攻击 FR-52、异常封禁 + IP 名单
FR-53、CC 挑战 FR-54、WAF 规则 FR-55、监控告警 FR-56）的阈值 / 开关 / 难度等均来自配置文件
（`[protection.*]`），过去只能改 TOML 并重启进程才能调整。运维在遭遇攻击或误杀时需要**在线即时**
调参（如临时收紧限流、加黑名单、关停误杀的 WAF 规则），不能依赖重启。FR-79 提供管理 API，FR-80
提供 Web 管理页面。属 P2 防护增强。

## 2. 需求（要什么）
- **FR-79 后端**：
  - `GET /api/v1/protection/config`（仅 Admin）：返回当前生效的防护配置全量（七个维度），脱敏。
  - `PATCH /api/v1/protection/config`（仅 Admin）：整体替换 `protection` 配置子树，**校验通过即时生效、
    不重启**；校验失败返回 400 且不改变现有配置。
  - 即时生效：替换后下一个请求即按新阈值 / 开关 / 名单 / WAF 规则判定；派生态（IP 名单匹配器、WAF
    规则集）按新配置重建。
  - 运行态保留：限流计数、封禁登记、告警去抖等进程内累计状态在改配置时**不清零**（避免改一次配置即
    放空已积累的防护状态）。
  - 鉴权：未认证 401，非 Admin 403。
- **FR-80 前端**：防护配置管理页面，各维度启停 / 调参表单，仅 Admin 可见可用；保存即调 PATCH 并回显。
- 范围内：仅 `protection` 子树（限流 / ip_list / ban / slowloris / cc_challenge / waf / alerts）。
- 不做（范围外）：改 server / data / auth / limits / proxy / observability / vuln 等其他配置子树；
  把配置持久化回 TOML（运行时改动是进程内热替换，重启回落文件配置，符合既有「配置真源是文件 + env」）；
  新增任何 P2/P3 之外能力。

## 3. 设计（怎么做）
- **热替换机制**（详见占位 ADR：运行时防护配置重载，扩展 ADR-0008）：
  - AppState 保持 `config: Arc<Config>` 不变（非防护配置仍读它）。
  - 新增 `protection: Arc<ProtectionState>`，内部 `RwLock<ProtectionSnapshot>`；快照含
    `Arc<ProtectionConfig>` + 派生 `Arc<IpMatcher>` + `Arc<WafRuleSet>`。
  - `snapshot()` 读锁内 clone 三个 `Arc` 立即放锁（锁外用，临界区只护内存态、短持有）。
  - `replace(cfg)` 锁外重建派生态，再短持写锁原子替换整个快照（锁外做编译型 IO / 正则编译）。
  - 防护中间件（rate_limit / cc_challenge / anomaly_ban / slowloris / waf）改读 `protection.snapshot()`。
  - `rate_limiter` / `ban_registry` / `cc_challenger` / `alert_engine` 仍为独立运行态字段（不随配置重建）。
- **校验**：`ProtectionConfig::validate()` 纯函数，PATCH 入口调用；非法（如窗口为 0、WAF 规则字段非法）
  返回 400。
- **薄 handler**：config handler 只做鉴权 + 调用校验 + 调用 `protection.replace` + 组装响应。

## 4. 任务拆分
- [ ] config 校验纯函数 + 单测
- [ ] ProtectionState / Snapshot + 单测（replace 生效、并发一致）
- [ ] GET / PATCH handler + 路由
- [ ] 中间件读取点迁移到 protection 槽
- [ ] main.rs / 测试状态组装
- [ ] 集成测试：即时生效（限流 / WAF / 名单各一）、Admin 边界、并发改配置
- [ ] 前端类型 / 端点 / 页面 / 路由 / 导航
- [ ] 文档同步：PRD 状态、API.md、CHANGELOG、ADR

## 5. 验收标准
- 后端 `cargo test` 全绿，含：
  - PATCH 后即时生效：改限流阈值后下一请求按新阈值 429；改 ip_list deny 后命中即 403；改 WAF 规则后命中即拦。
  - 鉴权：匿名 GET/PATCH → 401；普通用户 → 403；Admin → 200。
  - PATCH 非法体 → 400 且不改变 GET 返回。
  - 并发 replace + snapshot 不 panic、最终一致。
- 前端 `pnpm test` + `pnpm run build`（含 tsc）+ `pnpm run lint` 全绿。
- 真机维度（浏览器手验页面交互）：无法在 worktree 内长跑服务，标「待真机验」，由自动化集成测试覆盖 API 行为。

## 6. 风险 / 待定
- 双真源风险：`config.protection` 与 protection 槽并存 —— 约束为启动后只读 protection 槽，`config.protection`
  仅初始装载；GET 返回取自 protection 槽。
- ADR 编号由主控整合时分配，文中以 ADR-XXXX 占位。
