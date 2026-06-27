# 演进与维护指南

> 本文规定文档如何随代码演进、ADR 如何迭代、新需求如何落地。**目标：防止文档腐朽、变味、漂移。**

## 1. 黄金法则：文档即代码（docs-as-code）

- 文档与代码在**同一仓库、同一次变更（PR）**里一起改。
- **完成定义（DoD）**：一个变更没有把受影响的文档改到一致，就**不算完成**。
- **单一真源**：同一事实只在一处权威描述，别复制散落到多处各说各话；要复用就引用。

## 2. 文档地图（谁管什么 · 何时更新 · 入库否）

| 文档 | 管什么 | 何时更新 | 入库 |
|---|---|---|---|
| `docs/PRD.md` | 需求（WHAT/WHY）：目标、角色、功能需求、验收 | 需求增删改时 | ✓ 活文档 |
| `docs/specs/<feature>.md` | 非平凡功能的开发期工作规格（需求/设计/任务/验收） | 开发该功能时 | ✓ 留作记录 |
| `docs/ARCHITECTURE.md` | 系统设计（HOW）：模块、数据模型、机制、部署 | 结构/机制/依赖变化时 | ✓ |
| `docs/adr/*` | 重大决策的"为什么" | 做出/推翻架构决策时（见 §3） | ✓ |
| `docs/API.md` | 对外接口契约 | 接口变更时 | ✓ |
| `CHANGELOG.md` | 变更史 | 每个用户可见变更 | ✓ |
| `README.md` | 入口与导航 | 总览变化时 | ✓ |
| `.tmp/实施计划.md` | 里程碑、勾选、探索笔记 | 随便改，做完即弃 | ✗ 易朽 |

**判据：活文档（长期维护、是真源）入库；易朽稿（做完即弃）留 `.tmp/`。**

## 3. ADR 生命周期（不可变 + 取代）

- ADR 一旦"已接受"就**不可变**：**不编辑**旧 ADR 的决策正文。它是**决策史，永久保留、只增不删、编号永不复用**。
- 决策变了 → 写一条**新 ADR 取代旧的**：
  - 新 ADR 在背景里写"取代 ADR-NNNN"。
  - 旧 ADR 只把**状态行**改为"已被 ADR-MMMM 取代"并加链接（**不动正文**）。
- 状态：`提议中 → 已接受 → 已弃用 / 已被取代`。
- **何时写 ADR**：引入新技术、采用或推翻一个架构模式、做有长期影响且有争议的取舍。小决定不用写。

### 3.1 ADR 实操（维护期）

- **编号**：= 现有最大编号 + 1，永不复用、不补洞（现有最大看 `docs/adr/` 目录，别硬记某个数）。
- **写不写**：日常加功能若落在既有决策内，不写；只有上面"何时写"的情形才写。
- **取代长什么样**：当一个新需求推翻了某条既有架构决策（例如某条决策当初为了精简而排除了某项技术，现在因新场景必须引入），按以下步骤处理：
  1. 新建一条新 ADR（取下一个空号），背景写"取代 ADR-NNNN"。
  2. 把被取代的旧 ADR 状态行从"已接受"改为"已被该新 ADR 取代" + 链接，**正文一字不动**。
  3. 同步改受影响的 `.claude/rules/*`（如撤掉对应的不变量红线）、`ARCHITECTURE.md`、相关技能。

  旧决策"当初为什么"完整留存，新决策"为什么改"也有据——这就是 ADR 防漂移的价值。

**规模与导航（别慌通读）**：ADR 有意稀少——成熟项目一辈子也就几十到一两百条，不会上万；写得很快是"把日常决策误当架构决策"的滥用信号（日常变更靠 PRD 状态列 + CHANGELOG，不靠 ADR）。理解系统看 `ARCHITECTURE.md`（永远是现状综合），ADR 只在查"当初为什么"时按需翻；被取代的归档不打扰，**当前架构 = 未被取代的活跃集**，不必也别想通读所有 ADR。

## 4. 变更工作流（新需求 / 新功能如何落地）

```
1. 改 PRD（docs/PRD.md）         增/改需求，标 P1/P2/P3
2. 影响架构？→ 写新 ADR          引入/推翻架构决策时（必要时取代旧 ADR）
3. 改 ARCHITECTURE.md            反映新模块 / 数据模型 / 机制 / 依赖
4. 改 API.md                    接口契约变更
5. 实现 + 测试                  过验证门（后端 cargo test 与前端 npm test（vitest）全绿）
6. 记 CHANGELOG（未发布段）       用户可见变更，引用具体模块/能力
7. 发版                         bump 版本号、定稿 CHANGELOG 本版本段
```

> 一次只做一件事；破坏性变更必须在 CHANGELOG + 相关文档写清迁移。

## 5. 防漂移检查清单（每次变更自检）

- [ ] 代码改了，PRD / ARCHITECTURE / API 是否还一致？
- [ ] 新增或推翻了架构决策，是否有对应 ADR（且旧 ADR 已标记取代）？
- [ ] 破坏性变更，CHANGELOG 与迁移说明是否写了？
- [ ] 文档里的版本号 / 坐标 / 路径 / 端点是否过时？
- [ ] 同一事实是否只有一个真源？

## 6. 定期复检

- 每个里程碑 / 发版前，过一遍 §2 文档地图，确认无漂移。
- 发现"代码这样、文档那样" → **当 bug 修**，不积压。

## 7. 与 AI 协作

本仓库常与 AI 代理协作。文档同步要求已固化为 `.claude/rules/doc-sync.md`，未来任何会话改代码都会被要求同步文档，避免跨会话漂移。

## 8. 分支模型与发布渠道

采用 GitHub Flow（适合小团队 + 持续发布）：

- **`master`**：始终可发布、受保护；改动经 PR 合入（PR 模板含防漂移自检）。master 每次推送由 CI 发**快照**（`dev` 预发布，见对应版本 ADR 与 `sdd-publish-snapshot` 技能）。
- **`feature/*`、`fix/*`、`refactor/*`**：短生命周期分支，做完发 PR 回 master。
- **稳定发布**：在 master 打 `vX.Y.Z` tag（`sdd-release-version` 技能），CI 据 tag 出正式 Release。
- **`hotfix/*`**：从出问题的发布 tag 切分支紧急修，出补丁版后**回流 master**（`sdd-hotfix` 技能）。
- **回滚**优先 `git revert`，不重写已 push 历史（`sdd-rollback-change` 技能）。

版本号**基线**来源是 `Cargo.toml` 的 `version`（经 `CARGO_PKG_VERSION` 编入二进制）。展示版本统一经 `src/version.rs` 的 `build_version()` 取值：CI 发布时另经环境变量 `JIANARTIFACT_BUILD_VERSION` 注入**完整发布版本串**（prerelease 含 `dev.N.sha`、tag 取版本），`build_version()` 优先取注入值、回退 `CARGO_PKG_VERSION`，使 `--version` / `/health` / 设置页 / 自更新「当前版本」自报真实发布版本（修复 prerelease 自更新版本号不收敛）；本地开发未注入时即为 `CARGO_PKG_VERSION`。

### 8.1 发布流水线（FR-86）

GitHub Actions 实现两条流水线，触发条件与产出渠道如下：

- **质量门**（`.github/workflows/ci.yml`）：push / PR 到 `master` 触发，跑前端构建 → 后端 `cargo fmt --check` / `cargo clippy --all-targets -- -D warnings` / `cargo test`；依赖安全审计（`cargo audit` / `pnpm audit`）作独立非阻断 job。**构建顺序硬约束**：先 `pnpm -C frontend build` 产出 `frontend/dist`，再编译 / clippy / test 后端（rust-embed 编译期嵌入）。
- **发布**（`.github/workflows/release.yml`）：
  - **push `master`** ⇒ **prerelease**（开发版快照，滚动 tag `dev`、标记 prerelease），版本约定 `{Cargo.toml 版本}-dev.{run_number}.{shortsha}`（如 `0.4.0-dev.42.1a2b3c4`）。此处用 `.` 点分连接（而非 `+` 构建元数据）：GitHub 上传 Release 资产时会把资产名里的 `+` 改写成 `.`，导致自更新按含 `+` 的版本重构的期望资产名匹配不到；点分形态仍是合法 SemVer 预发布标识，资产名与 GitHub 存储一致。
  - **push tag `v*`** ⇒ **正式 Release**（非预发布），版本取 tag 去前导 `v`（`v0.3.0` → `0.3.0`）。
  - 三目标各用原生 runner 编译（免交叉编译）：`x86_64-unknown-linux-musl`（ubuntu-latest，musl 静态链接、跨发行版可跑）、`x86_64-pc-windows-msvc`（windows-latest）、`aarch64-apple-darwin`（macos-14）。
  - **资产命名契约**（FR-85 在线自更新消费）：每目标产出 `jianartifact-{version}-{target}{ext}` 及配套 `jianartifact-{version}-{target}{ext}.sha256`（裸 64 位十六进制哈希）；`{ext}` 在 Windows 为 `.exe`，Linux / macOS 为空。
  - 仓库坐标用 Actions 内置 `github.repository`，凭据仅用内置 `GITHUB_TOKEN`，不入库任何密钥。

> 说明：发布流水线在 `prepare` job 推导完整版本串（prerelease=`{Cargo 版本}-dev.{run}.{sha}`、tag=版本），并在 `编译 release 二进制` 步骤经 `JIANARTIFACT_BUILD_VERSION` 注入该串到二进制；运行时 `build_version()`（`src/version.rs`）优先取注入值、回退 `CARGO_PKG_VERSION`，故 `--version` 与自更新「当前版本」反映真实发布版本，本地未注入时即基线 `CARGO_PKG_VERSION`。

## 9. 文档如何长期演进（本次会话之后）

这些文档不是一次性产物，而是随项目活下去。每篇的演进方式不同：

| 文档 | 演进方式 |
|---|---|
| `docs/PRD.md` | **增量 + 状态流转**：加需求即加一行 FR（`计划`→`开发中`→`已交付@vX.Y.Z`），已交付的保留并标版本、不删——它是活的路线图 |
| `docs/ARCHITECTURE.md` | **原地更新**：始终反映当前系统真貌；结构 / 机制变了就改它 |
| `docs/adr/*` | **只追加 + 取代**：决策变了写新 ADR 取代旧的，旧的不删（§3） |
| `docs/API.md` | **原地更新**：始终是当前契约 |
| `CHANGELOG.md` | **累积 + 发版分段**：变更先进未发布段，发版时切成 `## X.Y.Z` 段 |

**文档冷热分层**（哪些常动、哪些少动，心里有数就不会该改的没改、不该动的乱动）：

| 冷热 | 文档 | 多久动一次 |
|---|---|---|
| 🔥 高频（几乎每次迭代） | `CHANGELOG.md` | 每个用户可见变更 |
| 🔥 高频 | `docs/PRD.md` | 每个新需求 / 交付（加行 / 改状态） |
| 🌡 中频（有相应变化才动） | `docs/ARCHITECTURE.md`、`docs/API.md` | 结构 / 机制 / 接口变更时 |
| 🌡 中频 | `docs/OPERATIONS.md` | 部署 / 运维方式变化时 |
| 🌡 中频 | `docs/specs/<feature>.md` | 功能开发期；交付后基本不动 |
| ❄ 低频 | `docs/adr/*`、`README.md`、`SECURITY.md` | 架构决策时追加 / 总览或安全模型变化时 |
| 🧊 近乎不变（改它=动项目根基） | `.claude/rules/*`（尤其 `architecture-invariants`）、全局 `sdd-*` 技能（改它影响所有 SDD 项目）、`.editorconfig`、`.gitignore`、`Cargo.toml` 版本字段（仅发版动） | 极少；改不变量 / 红线要慎重并配 ADR |

把"改不变量 / 红线"当大事——它们近乎不变，真要改先走 ADR，别随手动。

**会话之间如何接续**：这套靠 `.claude/rules/`（红线，项目内）+ 全局 `sdd-*` 技能（流程）**自我维持**——任何新会话进入仓库会自动加载规则，按 `sdd-develop-feature` / `sdd-fix-bug` / `sdd-refactor-code` / `sdd-rollback-change` / `sdd-release-version` 等技能干活，每一步被 `doc-sync` 要求同步文档。所以"本次会话结束"不影响延续：下个会话读 PRD / ARCHITECTURE / ADR 接上下文，照技能与规则继续推进，文档随之演进、不漂移。

## 10. 维护迭代周期（稳态操作手册）

第一期交付后进入稳态迭代。**每个工作项的标准循环**：

1. **识别工作项**，选对应技能（路由见下表）。
2. **开分支**：`feature/*` / `fix/*` / `refactor/*` / `hotfix/*`（§8）。
3. **按技能走**：读相关 PRD / ARCHITECTURE / ADR → 测试先行 → 实现（守不变量、简单优先）→ 过验证门 → `doc-sync` 同步文档。
4. **发 PR**：填防漂移自检模板 → 评审 → 合入 `master`。
5. **master 自动出快照**（CI / `sdd-publish-snapshot`），可随时让人试用。
6. **攒够一批 → 发版**（`sdd-release-version`：CHANGELOG 分段、定 SemVer、bump `Cargo.toml` 版本、打 `vX.Y.Z` → CI 出正式 Release）。
7. **生产事故** → `sdd-hotfix` 旁路：从发布 tag 切分支最小修 → 出补丁版 → 回流 `master`。

→ 回到 1。

**工作项 → 技能 路由**：

| 来了什么 | 用哪个技能 |
|---|---|
| 新需求 / 新能力 | `sdd-develop-feature` |
| bug / 报错 / 行为不对 | `sdd-fix-bug` |
| 代码太乱 / 拆分 / 消除重复 | `sdd-refactor-code` |
| 撤掉某功能 / 回退 | `sdd-rollback-change` |
| 升级第三方依赖 | `sdd-bump-dependencies` |
| 纯文档工作（写 ADR / 改架构说明 / 修文档漂移 / 整理文档） | `sdd-update-docs` |
| 出快照 / 给人试用 | `sdd-publish-snapshot` |
| 正式发版 | `sdd-release-version` |
| 生产紧急修 | `sdd-hotfix` |
| 外部 / 计划外提交进来（队友直推、CI、合并、逆向后新提交）需对齐文档 | `sdd-reconcile-external-commits` |
| sdd-skills 更新了治理模板，要同步进本项目 | `sdd-sync-governance` |

**一句话**：稳态下你几乎不用从头想流程——认准工作项类型、调对应技能，它会带着你读文档、测试先行、同步文档、按分支与版本规矩走完。规则与技能就是把这套循环固化下来、跨会话不走样。

### 10.1 一次变更各动哪些（速查）

加很多 feat / fix，你"维护"的其实就那几样——**期数几乎不动**。

| 来了什么 | 要动 | 不用动 |
|---|---|---|
| **feat 新功能** | PRD §4 加一行 FR（贴**已有**期 + 状态 `计划`）· 非平凡写 `docs/specs/<f>.md` · 结构变更动 `ARCHITECTURE` · 接口变更动 `API` · `CHANGELOG` +1 行 · 加测试 | 期数 · `Cargo.toml` 版本（发版才动） |
| **fix 修 bug** | `CHANGELOG` +1 行 · 复现 + 回归测试 | PRD · 期数 · `Cargo.toml` 版本 · ADR · API |
| **refactor 重构** | 结构变才动 `ARCHITECTURE` · 测试前后同样全绿 | PRD · 期数 · API · 行为 |
| **rollback 回滚** | FR 状态回退 · 取代相关 ADR · `CHANGELOG` +1（移除） | 期数 |
| **依赖升级** | 锁文件 · 有感知影响才记 `CHANGELOG` · 全测试绿 | PRD · 期数 · ADR |
| **架构决策** | **ADR +1 条（或取代旧的，编号 = 现有最大 +1）** · 更新 `ARCHITECTURE` | 期数（除非顺带开新阶段） |
| **发版 release** | **`Cargo.toml` 版本改（按提交定 SemVer）** · `CHANGELOG` 未发布段 → `## X.Y.Z` · 交付的 FR 翻 `已交付@vX.Y.Z` · 打 tag | 期数 |
| **发快照** | 构建发预发布 / 快照（产物标 `dev` 预发布） | `Cargo.toml` 版本 · `CHANGELOG` · 期数 |
| **开新大阶段（罕见）** | PRD §7 加一行主题 + 启用新期号 | —— 这是**唯一**动期数的时候 |

**谁常动 / 谁不动**：
- 🔥 高频：`CHANGELOG`（几乎每次）、PRD FR 表（每个 feat 加行 / 发版翻状态）、`Cargo.toml` 版本（每次发版）。
- ❄ 低频：ADR（只在架构决策时 +1 或取代）。
- 🧊 几乎不动：**期数**（只在开新大阶段，罕见）、`.claude/rules`（项目内）/ 全局 `sdd-*` 技能（动它 = 动根基，要配 ADR）。

> 所以"很多 feat + fix 怎么维护期数"——**根本不维护期数**：fix 只进 `CHANGELOG`；feat 进 FR 表（贴个已有期标签）；期数还是那几个，纹丝不动。
