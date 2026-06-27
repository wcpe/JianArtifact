# 功能规格：动态配置持久化（文件默认 + DB 覆盖 + 内存缓存）

> 状态：开发中（后端基座 + 既有可编辑端点持久化 + 新 Dynamic 节在线表单（读写端点 + 设置页「系统配置」tab）+ 穷举测试已落地；真机「面板改 → 重启 → 仍生效」验收待续）　·　关联 PRD：FR-106　·　ADR：ADR-0028（已接受）　·　分支：feature/fr-106-dynamic-config-db

## 1. 背景与目标

把"除启动必需项与凭据外的配置"做成**面板可在线配置、持久化、重启不丢**，按黄金组合：文件兜底 → DB 覆盖 → 内存缓存。设计与边界见 ADR-0028。**本 spec 是实现前的圈界文档**——用户评审通过、ADR 定为「已接受」后再写代码。

## 2. 配置项归属（边界，权威）

三类（详见 ADR-0028 §4）：

- **Bootstrap（文件/env only，不入库、不在线改）**：`server.*`、`data.data_dir`、DB 路径、`data.storage` 后端。
- **Secret（文件/env 真源 + 内存槽，不入库）**：`[network.proxy]`（含 `user:pass@`）、`[update] token`、`[auth.oidc] client_secret`、`[auth.ldap]` bind 口令、JWT 密钥。
- **Dynamic（DB 覆盖 + 文件兜底 + 内存槽，可在线改持久化）** —— **首批纳入（高频）**：
  - `limits.max_artifact_size`（上传上限）
  - `protection.*` 非密钥阈值/开关（rate_limit / ip_list / ban / slowloris / cc_challenge / waf / alerts 的窗口、阈值、enabled、名单）—— 已有 `ProtectionState` 热替换槽（FR-79），本期加"启动从 DB 预填 + PATCH 写库"
  - `observability.audit.retention_days / max_rows`、`observability.usage.detail_enabled / max_detail_rows`、`observability.metrics.enabled`、`observability.metrics_timeseries.sample_interval_secs / retention_days`
  - `update` 非密钥字段（`enabled / repo / api_base_url / restart_mode / channel`，token 除外——已是 ADR-0022 内存槽，本期加非密钥字段落库）
  - `vuln.enabled / source_base_url / refresh_interval_*`
  - `auth.session_ttl_secs / login_max_failures / login_lockout_secs`
  - **二批/按需**：其余非密钥节增量纳入（YAGNI，不一次铺满）。

> 评审点：以上 Dynamic 首批清单请用户确认增减。

## 3. 优先级与合并（须钉死并测试）

生效值 = **env 显式给值 ⊕ DB 覆盖 ⊕ 文件默认**（优先级 env > DB > 文件默认）：

- 文件默认（含内置默认）：基线。
- DB（`app_settings`）：覆盖文件默认。
- env（`JIANARTIFACT_*` 显式给值）：最高，覆盖 DB——部署环境强约束 > 面板改动；避免"面板改了但 env 钉死"产生不一致时以 env 为准（与 ADR-0020 "配置显式即真源"基调一致）。

> 难点：需能区分"env 显式给值" vs "未给"。装载阶段记录哪些键来自 env 显式；合并时 env 键不被 DB 覆盖。spec 实现时穷举测试三层叠加各组合。

## 4. 数据模型与时序

### 4.1 表（migration `0012_app_settings.sql`，经 meta）
```sql
CREATE TABLE app_settings (
  key        TEXT PRIMARY KEY,   -- 点分路径，如 limits.max_artifact_size
  value_json TEXT NOT NULL,      -- JSON 标量/片段
  updated_at INTEGER NOT NULL
);
```
`meta` 暴露：`load_settings() -> Vec<(key,json)>`、`upsert_setting(key,json)`、`delete_setting(key)`。**DB 只经 meta。**

### 4.2 启动时序（`main`，装配层）
1. `Config::load`（默认 → TOML → env），并**记录 env 显式键集合**。
2. `MetaStore::open` + 迁移。
3. `meta.load_settings()` → 解析为覆盖 map。
4. **合并**：对每个 Dynamic 键，`生效 = env显式 ? env : (DB有 ? DB : 文件默认)`；凭据 / bootstrap 键不参与（恒文件/env）。
5. 用合并后的生效值填充各热替换槽（`EditableSettings` / `ProtectionState` / 新增 Dynamic 槽）与启动任务参数。

### 4.3 变更时序（PATCH，仅 Admin）
1. 收编辑请求 → 各节 `validate()` 校验（失败 400、不改现值）。
2. **凭据字段**：只更新内存槽，**不写 DB**。
3. **非密钥字段**：`meta.upsert_setting` 落库 + 重建/原子换内存槽即时生效。
4. 读路径只读内存槽（缓存），不每请求查库。

## 5. 设计要点（守不变量）

- `config` 模块**零 DB 依赖**；覆盖在 `main` + 内存槽完成，依赖方向 `api → meta → config` 不变、无环。
- DB 只经 `meta`；`app_settings` 读写为 meta 方法。
- 凭据白名单过滤：落库前剔除一切 Secret 字段（代码层显式枚举不入库键，**默认拒绝**——新增字段若未明确标记可入库，则不入库，防误入）。
- 内存槽即缓存，读零额外 IO；PATCH 写库 + 换槽在锁外做 IO、临界区只换槽指针。
- 中文分级日志；落库 / 加载失败只 WARN + 回落文件默认，不阻断启动。
- 不外发。

## 6. 测试点（穷举）

- **三层优先级**：文件默认 / DB 覆盖 / env 显式 各组合下生效值正确（尤其 env 显式 > DB）。
- **持久化**：PATCH 非密钥项 → 写 `app_settings` → 重启（重新装载）后仍生效。
- **凭据红线**：PATCH 含凭据（代理账密 / update token）→ `app_settings` 与 DB 中**无任何凭据明文**；内存槽生效、重启回落文件/env（与 ADR-0022 一致）。
- **bootstrap 不可在线改**：尝试改 server/data 项被拒或不持久化。
- **韧性**：DB 空 / `app_settings` 损坏 / 加载失败 → 回落文件默认、应用正常起来（WARN）。
- **校验失败不改状态**：非法值 400、DB 与生效值都不变。
- **白名单默认拒绝**：未标记可入库的新字段不被写库（防误入）。

## 7. 验收标准

- 首批 Dynamic 节经面板 PATCH 改后写 `app_settings`、**重启仍生效**；env 显式项仍以 env 为准。
- 凭据 / bootstrap 严格不入库（DB 内零凭据明文、bootstrap 不在线改）。
- `config` 不反向依赖 meta/DB；DB 只经 meta；分层无环（cargo 编译 + 依赖审查）。
- 文件默认兜底：清空 `app_settings` 应用仍起得来、行为等价纯文件配置。
- fmt / clippy / 后端 test（含 §6 穷举）/ 前端 build+test 全绿。
- **【需用户确认 · 真机重启维度】** "面板改 → 重启 → 仍生效"的端到端持久化需真机重启验证。

## 8. 风险 / 待定（评审点）

1. **Dynamic 首批清单**（§2）请确认增减。
2. **优先级**（§3）env > DB > 文件——确认这个口径（vs DB 最高）。
3. **范围**：是否真要"非密钥全部节"一次到位，还是先高频节、增量纳入（推荐后者，YAGNI）。
4. 前端：配置项变多后设置页二级导航如何容纳这些 Dynamic 节（与 FR-103 新设置页协调，可能多加几个 tab）。**已落地**：设置页二级导航新增「系统配置」tab，按分组（限制与配额 / 可观测性 / 漏洞库 / 安全 · 会话）承载 limits / observability / vuln / auth 非密钥项，独立保存按钮并显著标注「保存后重启生效」（区别于代理 / 更新 / 防护的即时生效）。

## 9. 新 Dynamic 节在线表单（已落地，FR-106 收尾）

- **端点**：新增 `GET` / `PATCH /api/v1/settings/dynamic`（仅 Admin，见 `src/api/dynamic_config.rs`）。覆盖 `limits` / `observability.{audit,usage,metrics,metrics_timeseries}` / `vuln` / `auth` 三个可调标量。
  - `GET`：以启动期生效配置（`state.config`，已是 env⊕DB⊕文件 合并值）为基线、叠加当前 `app_settings` 覆盖，回显「当前 + 待生效」值（含本次 PATCH 后写入的待生效值）。
  - `PATCH`：整体校验各节边界（周期 / 间隔 / 会话 TTL 等必须 > 0）→ 通过则按节 `upsert_setting` 落库（经白名单键）；任一节非法返回 400 且不写任何节。
- **生效语义（诚实）**：这些节多在启动期装载、**无现成热替换槽**，本期落库后**重启生效**（黄金组合「变更=改 DB、下次装载生效」），不为每个后台任务强造热替换槽（YAGNI）。前端「系统配置」tab 显著标注「保存后重启生效」。
- **凭据红线**：`auth` 经 `AuthTunables` 非密钥视图序列化，结构上不可能带出 OIDC / LDAP 密钥；`limits` / `observability` / `vuln` 本就无凭据；端点只写固定白名单键，bootstrap 键（`server.*` / `data.*`）绝不经此路径（穷举测试断言落库键集限于白名单、auth 节无凭据字段名）。
