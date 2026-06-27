# 功能规格：GitHub Actions CI/CD 发布流水线

> 状态：开发中　·　关联 PRD：FR-86　·　分支：feature/fr-86-ci-release

## 1. 背景与目标

项目当前无任何 CI/CD 自动化：质量门（fmt / clippy / test）与发布产物全靠本地手工执行，既无法在 PR 合并前挡住低级问题，也没有可供下载的预编译二进制。FR-85（管理员手动触发的在线自更新）需要从 GitHub Release 按本机 target 下载资产并校验 sha256，因此必须先有一条稳定的、产出**命名契约固定**的发布流水线作为其数据来源。

本功能落地一条 GitHub Actions 流水线，分两条职责：

- **质量门**（`ci.yml`）：push / PR 到 `master` 时跑前端构建 + 后端 `fmt` / `clippy` / `test`，把风格与回归问题挡在合并前（对齐 `.claude/rules/static-analysis.md`）。
- **发布**（`release.yml`）：push `master` 出 **prerelease**（开发版快照）、push `v*` tag 出**正式 Release**；三目标（Linux / Windows / macOS）各用原生 runner 编译，产出按命名契约重命名的二进制 + 每资产一份 `.sha256`，上传为 GitHub Release 资产。

属 P2（工程化 / 发布流程）。CI/CD 属发布与工程流程、非运行时架构决策，**不新增 ADR**（与 ARCHITECTURE / 既有 ADR 不冲突，仅落规格 + OPERATIONS + CONTRIBUTING）。

## 2. 需求（要什么）

### 范围内

- **质量门 job**：
  - 触发：push / pull_request 到 `master`。
  - 步骤：检出 → 装 Node + pnpm → 装 Rust stable（带 `clippy` / `rustfmt` 组件）→ `pnpm -C frontend install --frozen-lockfile` → `pnpm -C frontend build`（产出 `frontend/dist` 供 rust-embed 编译期嵌入）→ `cargo fmt --check` → `cargo clippy --all-targets -- -D warnings` → `cargo test`。
  - **构建顺序硬约束**：必须先建前端再编译 / clippy / test 后端——`rust-embed` 在编译期读 `frontend/dist`，否则后端 clippy / test 拿到的是占位空集（虽能编译，但与发布产物不一致）。
  - 依赖安全审计（`cargo audit` / `pnpm audit`）作**独立、不阻断主门**的 job（对齐 static-analysis.md「漏洞发现工具作入口」，但不因上游公告波动阻断合并）。
- **发布 job**（矩阵 3 目标，各自原生 runner，免交叉编译）：

  | target | runner | 产物扩展名 `{ext}` |
  |---|---|---|
  | `x86_64-unknown-linux-gnu` | `ubuntu-latest` | （空） |
  | `x86_64-pc-windows-msvc` | `windows-latest` | `.exe` |
  | `aarch64-apple-darwin` | `macos-14` | （空） |

  - 每个矩阵分支：检出 → 装 Node + pnpm + Rust stable → 建前端 → `cargo build --release`（默认特性，**不启用 `s3`**，守单一二进制零外部运行时依赖）→ 按命名契约重命名二进制 + 生成 `.sha256` → 上传为 Release 资产。
  - 触发与产物渠道：
    - **push `master`** ⇒ prerelease（`prerelease: true`），版本按 §3 dev 约定，覆盖式更新同一 prerelease（滚动「开发版快照」）。
    - **push tag `v*`** ⇒ 正式 Release（`prerelease: false`），版本取 tag（去掉前导 `v`）。
- **资产命名契约**（钉死，FR-85 消费，见 §3）。
- 仓库坐标用 Actions 内置 `github.repository` / `GITHUB_REPOSITORY`，**不硬编码** `wcpe/JianArtifact`。
- 凭据仅用内置 `GITHUB_TOKEN`，**不入库任何密钥**。
- 所有 YAML 注释中文（对齐 `.claude/rules/comments.md`）。

### 不做（范围外）

- 不实现 FR-85 自更新逻辑本身（本功能只产出其依赖的资产与命名契约）。
- 不做交叉编译（如在 Linux 上编 Windows / macOS）——每目标用原生 runner，简单优先。
- 不发布 `s3` 特性专用构建（默认构建不含 S3；如需可后续按 OPERATIONS §1 单独构建，不进默认发布矩阵）。
- 不做容器镜像发布、不做包管理器分发（homebrew / scoop 等）、不做签名 / SBOM——均非本期范围。
- 不动版本真源：版本由 `Cargo.toml` 经 clap `version`（`CARGO_PKG_VERSION`）注入二进制，流水线只**读取**它、不改写。

## 3. 设计（怎么做）

### 3.1 资产命名契约（与 FR-85 的契约边界）

每个发布目标产出**一个二进制资产** + **一份 sha256 校验文件**，命名固定：

```
jianartifact-{version}-{target}{ext}
jianartifact-{version}-{target}{ext}.sha256
```

- `{version}`：正式 Release 取 tag 去前导 `v`（如 `v0.3.0` → `0.3.0`）；prerelease 取 §3.2 dev 版本串。
- `{target}`：Rust target triple，取上表三者之一。
- `{ext}`：Windows（`*-windows-msvc`）为 `.exe`，Linux / macOS 为空。

示例（正式 Release `v0.3.0`）：

```
jianartifact-0.3.0-x86_64-unknown-linux-gnu
jianartifact-0.3.0-x86_64-unknown-linux-gnu.sha256
jianartifact-0.3.0-x86_64-pc-windows-msvc.exe
jianartifact-0.3.0-x86_64-pc-windows-msvc.exe.sha256
jianartifact-0.3.0-aarch64-apple-darwin
jianartifact-0.3.0-aarch64-apple-darwin.sha256
```

- `.sha256` 内容为该资产的小写十六进制 sha256（裸 64 位十六进制串，不带文件名），供 FR-85 下载后逐字节校验、不一致即拒绝替换。
- **FR-85 据此约定推导本机资产名**：`jianartifact-{latestVersion}-{当前 target}{ext}`，无需流水线额外提供索引清单。

### 3.2 prerelease 版本约定

push `master` 的 prerelease 版本串：

```
{cargo版本}-dev.{run_number}.{shortsha}
```

- `{cargo版本}`：从 `Cargo.toml` 读出的 `version`（当前 `0.4.0`）。
- `{run_number}`：`github.run_number`，单调递增，区分同一基线版本的多次快照。
- `{shortsha}`：`github.sha` 前 7 位，定位具体提交。

示例：`0.4.0-dev.42.1a2b3c4`。该串符合 SemVer 预发布语法（`-dev.N.sha` 整体为点分预发布标识，排在正式版之前）。

> **为何用 `.` 而非 `+` 连接 shortsha**：SemVer 允许 `+{sha}` 作为构建元数据，但 **GitHub 上传 Release 资产时会把资产名里的 `+`（及其它非 `[A-Za-z0-9._-]` 字符）改写成 `.`**——资产实际存为 `…dev.N.sha…`，而 FR-85 自更新按含 `+` 的版本串重构期望资产名，二者不一致致匹配不到、prerelease 拉不到。改用 `.` 连接，使资产名与 GitHub 存储一致、自更新可精确匹配。

prerelease 的 GitHub Release 用固定 tag `dev`（滚动覆盖），资产名内嵌上述完整版本串以便区分；release 标题 `name` 也置为该完整版本串（tag 是固定 `dev`、非版本，FR-85 据 `name` 解析真实版本）。

> 版本真源说明：二进制版本经 clap `version` 由 `CARGO_PKG_VERSION`（即 `Cargo.toml` 的 `version`）注入。流水线读取 `Cargo.toml` 的 `version` 作为 `{cargo版本}`，与二进制内注入值一致。

### 3.3 工作流文件

- `.github/workflows/ci.yml`：质量门 + 审计。
- `.github/workflows/release.yml`：发布矩阵。

两文件复用一致的环境准备步骤（装 Node+pnpm、装 Rust、建前端）；Node 经 `actions/setup-node` + `pnpm/action-setup`（pnpm 版本从 `frontend/package.json` 的 `packageManager` 缺省时显式钉版），Rust 经 `dtolnay/rust-toolchain@stable`。版本读取经 shell 从 `Cargo.toml` 提取（不引额外依赖）。

## 4. 任务拆分

- [x] 写规格 `docs/specs/fr-86-ci-release.md`
- [x] PRD §4 FR-86 状态 计划 → 开发中（仅改 FR-86 行）
- [x] 实现 `.github/workflows/ci.yml`（质量门 + 独立审计 job）
- [x] 实现 `.github/workflows/release.yml`（3 目标矩阵 + 命名契约 + sha256 + 触发分流）
- [x] 本地 YAML 语法自检 + 逐条逻辑核对
- [x] 文档同步：OPERATIONS §2（发布产物来源 / 自更新衔接）、CONTRIBUTING（发布流程 / 版本约定）、CHANGELOG 未发布段追加

## 5. 验收标准

- 两个 workflow 文件 YAML 语法合法（本地 `python -c "import yaml; yaml.safe_load(...)"` 解析通过）。
- 质量门 job 步骤顺序正确：**先建前端再 fmt / clippy / test**；clippy 带 `-D warnings`；触发限定 push / PR 到 `master`。
- 发布矩阵三目标与上表 target / runner / `{ext}` 一致；产物命名严格符合 §3.1 契约；每资产配套 `.sha256`。
- 触发分流正确：push `master` ⇒ `prerelease: true` 且版本走 §3.2 dev 约定；push `v*` ⇒ `prerelease: false` 且版本取 tag。
- 仓库坐标不硬编码、凭据仅用 `GITHUB_TOKEN`、无任何密钥入库。
- 全部 YAML 注释为中文。
- **【需用户确认 · 真 CI 维度】** 上述自检为本地静态校验；流水线在 GitHub Actions 上的真实运行（质量门绿、三目标真出资产且命名 / sha256 正确、prerelease 与 tag 两条触发各出预期 Release）**需用户 `git push` 到 `wcpe/JianArtifact` 后由真 CI 验证**，本地无法替代。本规格在真 CI 跑通前状态保持「开发中（待真 CI 验）」。

## 6. 风险 / 待定

- **真 CI 未验**：本仓库本地无法跑 GitHub Actions，所有运行期正确性（runner 镜像可用性、action 版本兼容、`aarch64-apple-darwin` 在 `macos-14` 原生编译）待用户 push 后由真 CI 确认。`dtolnay/rust-toolchain` / `pnpm/action-setup` / `softprops/action-gh-release` 等第三方 action 的具体行为以其真实执行为准。
- **VERSION 文件漂移（非本功能范围）**：根 `VERSION` 文件为 `0.1.0`，与二进制实际注入的 `Cargo.toml` `0.3.0` 不一致；clap `version` 用的是 `CARGO_PKG_VERSION`（`Cargo.toml`），`VERSION` 文件未被代码消费。CONTRIBUTING §8 仍称「版本号唯一来源是根 VERSION 文件」，与实际不符。本流水线以 `Cargo.toml` 为版本来源（与二进制一致），`VERSION` 文件的清理 / 对齐另行处置，不在 FR-86 内。
- **prerelease 滚动 tag `dev`**：用固定 tag `dev` 滚动覆盖快照，只保留「最新开发版」。因快照资产名内嵌 `{run_number}+{sha}`、各次不同，`action-gh-release` 仅追加不删旧资产会堆积；故发布前显式 `gh release delete dev --cleanup-tag`（仅 prerelease 渠道）删旧 dev 预发布及其 tag，使每次发布为只含当前快照的全新 dev 预发布。正式 `v*` tag 渠道不删、保留历史正式版。若日后需保留历史开发版快照需另设策略。
