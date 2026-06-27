# 功能规格：在线更新 prerelease / 测试通道（增强 FR-85）

> 状态：开发中　·　关联 PRD：FR-89（增强 FR-85）　·　关联 ADR：ADR-0021（扩展，不另写新 ADR）　·　分支：feature/fr-89-prerelease-channel

## 1. 背景与目标

FR-85 的自更新只查 GitHub `/releases/latest`，而该端点**按设计只返稳定版**、不含 prerelease（见 fr-85-online-update §3.4、ADR-0021 后果）。真机测试常以 prerelease 形式发版（FR-86 push 默认分支 → prerelease），导致 check / apply「拉不到」——稳定通道下这是**符合预期**的，但测试 / 灰度场景需要一条能选中预发布版的通道。

FR-89 给在线更新加**通道开关**：默认 `stable`（维持现状只认稳定版），可切到 `prerelease`（发现并升级到最新预发布版）。该开关同样经 FR-88（ADR-0022）的可编辑设置机制承载——设置页 / `PATCH /api/v1/settings` 即时生效、无须重启。属 P2，增强既有 FR-85。

## 2. 需求（要什么）

### 范围内
- **通道字段 `channel`**：枚举 `stable`（默认）| `prerelease`，纳入 `[update]` 配置（启动期 TOML / env）。
- **stable 通道**（默认）：维持现状，查 `{api_base_url}/repos/{repo}/releases/latest`，只认稳定版、忽略预发布。
- **prerelease 通道**：查 `{api_base_url}/repos/{repo}/releases`（列表），取**最新一条非 draft 的 release**（含预发布），据其比对 / 下载 / 替换。
- **可在线编辑**：`channel` 经设置页 / `PATCH /api/v1/settings` 编辑，校验合法值后即时生效（融入 FR-88 可编辑槽 `EditableUpdate`）。
- **GET 设置回显**：`update` 视图含当前 `channel`（非凭据，原样回显）。

### 不做（范围外，YAGNI / 守范围纪律）
- 不做更多通道（beta / nightly / 自定义 tag 前缀过滤）；仅 stable / prerelease 两态。
- **stable 通道**不改版本比较语义：仍以 `major.minor.patch` 三段整数比较（忽略预发布 / 构建后缀），仅当核心版本严格更高才更新。
- 不新增预发布优先级排序（不实现 `-rc.1 < -rc.2` 等 SemVer 预发布序比较）。
- 不改下载 / 校验 / 替换 / 重启链路：通道只影响「取哪一条 release」与「是否判可更新」，其后流程与 FR-85 完全一致。
- 不新增第三方依赖：列表解析复用现有 `serde_json`。

## 3. 设计（怎么做）

### 3.1 通道类型（纯函数，可测）
新增 `UpdateChannel` 枚举（`Stable` | `Prerelease`），`from_config(&str)` 解析（未知值在校验阶段拒绝、运行时取值时回退 `Stable` 兜底）。配置承载用字符串（与 `restart_mode` 同风格，便于 TOML / env / PATCH 统一处理 + `validate` 校验合法值）。

### 3.2 配置 `[update] channel`
- `UpdateConfig` / `EditableUpdate` 各加 `channel: String` 字段，默认 `"stable"`（新增 `DEFAULT_UPDATE_CHANNEL` 常量）。
- `EditableUpdate::validate` 增校验：`channel` 仅允许 `stable` / `prerelease`（与 `restart_mode` 校验同处）。
- `EditableUpdate::from_config` 装载 `channel`。

```toml
[update]
channel = "stable"   # stable（默认，仅稳定版）| prerelease（含预发布，取最新一条）
```

### 3.3 `ReleaseSource` 按通道取 release
- `ReleaseSource::fetch_latest_release` 加 `channel: UpdateChannel` 参数（trait 方法签名调整；fake 源同步）。
- `GithubReleaseSource`：
  - `Stable`：维持现状请求 `/releases/latest`，`parse_release` 解析单对象。
  - `Prerelease`：请求 `/releases`（列表），新增 `parse_release_list` 解析数组，**跳过 `draft=true`**，取数组首个（GitHub 列表按发布时间倒序）作为「最新含预发布」的一条；列表空 → `UpdateError::Upstream`（无可用 release）。
- `build_check` / `apply_update` 把 `channel` 透传给 `fetch_latest_release`，并据通道做**版本判定分流**（见 §3.6）。

### 3.6 版本判定按通道分流（`is_update_available_for_channel`）

dev 预发布常与当前正式版**共享核心版本**（如当前 `0.4.0` vs 最新 `0.4.0-dev.5.<sha>`），若仍按 `major.minor.patch` 三段比，核心相等 → 误判「无更新」、prerelease 拉不到。故新增 `is_update_available_for_channel(channel, current, latest)` 纯函数分流：

- **`Stable`**：维持 `is_update_available`（SemVer 三段严格更高，忽略后缀），语义不变。
- **`Prerelease`**：改按**完整版本串**判定——归一化（去首尾空白与前导 `v`）后，目标与当前**不同即视为可更新 / 可切换**；完全相同则无更新（避免无意义自替换）。两侧仍各过一次 `parse_version` 校验合法性，非法版本串报错、不静默放行。

`build_check` 与 `apply_update` 的防御性「无更新」判定均改调本函数（透传 `channel`）。

另：prerelease 滚动发布的 `tag_name` 是固定标签 `dev`（非版本串，见 fr-86 §3.2），故 `Release::version()` 在 `tag_name` 不可解析为版本时**回退取 release 标题 `name`**（内嵌完整 dev 版本串）；正式版 `tag_name=vX.Y.Z` 仍走 tag。

### 3.4 API 与设置页（融入 FR-88）
- `src/api/update.rs`：`build_source` 读 `EditableUpdate.channel` 解析为 `UpdateChannel`，`check_update` / `apply_update` 调 `fetch_latest_release(channel)`。
- `src/api/settings.rs`：`UpdateView` 加 `channel`（GET 回显）；`UpdatePatch` 加 `channel`，`patch_settings` 组装进 `EditableUpdate`，复用其 `validate`（非法 `channel` → 400、不改现有生效值）。
- 前端 `SettingsPage.tsx`：在线更新区加「更新通道」`Select`（stable / prerelease）；`types.ts` 的 `UpdateView` / `UpdatePatch` 加 `channel`。

### 3.5 文档
- 扩展 ADR-0021（在「后果 / 备选方案」语境内本属「更新通道」原列为落选项；FR-89 按需引入 stable/prerelease 两态，**不另写新 ADR**，仅在 fr-85 spec 风险项与本 spec 记录差异）。
- 同步 `docs/API.md`（settings 可编辑字段 + update check 行为）、`docs/OPERATIONS.md`（`[update] channel`）、`docs/CONFIG.md`、`CHANGELOG.md`。

## 4. 任务拆分
- [x] 写规格（本文）+ PRD §4 FR-89 计划→开发中（仅改 FR-89 行）
- [x] `config.rs`：`UpdateChannel` 枚举 + `channel` 字段（`UpdateConfig` / `EditableUpdate`）+ `DEFAULT_UPDATE_CHANNEL` + `validate` 校验 + 单测
- [x] `src/update/source.rs`：`fetch_latest_release(channel)` + `parse_release_list`（跳 draft、取最新）+ fake 源同步
- [x] `src/update/mod.rs`：`build_check` / `apply_update` 透传 channel
- [x] `src/api/update.rs`：`build_source` 读 channel 透传
- [x] `src/api/settings.rs`：`UpdateView` / `UpdatePatch` 加 channel + 组装 + 校验 + 测试
- [x] 测试先行：prerelease 选预发布、stable 只认稳定、版本比较预发布串、列表解析跳 draft、PATCH 改 channel 即时生效
- [x] 前端：`SettingsPage.tsx` + `types.ts` 加 channel + 前端测试
- [x] doc-sync：API、OPERATIONS、CONFIG、CHANGELOG

## 5. 验收标准
- `channel=prerelease` 时，fake 源（列表含预发布 + draft）→ 选中最新非 draft 的预发布版并据其比对 / 升级（单测断言选中版本 + 走到替换）。
- `channel=stable`（默认）时，走 `/releases/latest`、只认稳定版、不选预发布（单测断言请求路径与选中版本）。
- prerelease 列表解析跳过 `draft=true`、取最新一条；列表空 → 上游错误。
- `PATCH /api/v1/settings` 改 `channel` 即时生效（热槽当前值变更）；非法 `channel` → 400 且不改现有生效值。
- GET `/api/v1/settings` 回显当前 `channel`。
- **stable 通道**版本比较对预发布串（`0.4.0-rc.1`）的处理与现状一致（忽略后缀比较）。
- **prerelease 通道**按完整版本串判定：当前 `0.4.0` 对最新 `0.4.0-dev.N.<sha>`（核心相等）判 `update_available=true`；完全相同的版本串判 `false`（单测断言）。
- prerelease 滚动 `tag_name=dev` 时版本回退取 `name`（单测断言）；资产名据完整 dev 版本串重构、与 release 资产精确匹配。
- 单元 / 集成测试绿；clippy `-D warnings`、fmt 过；前端 test + build 过。
- token / 凭据仍不进日志、不入库、不回显（沿用 FR-85 / FR-88 红线，本功能不触碰凭据）。

## 6. 风险 / 待定
- **「最新预发布」语义**：GitHub `/releases` 按发布时间倒序，取首个非 draft 即「最新」。
- **prerelease 通道判定口径**：改为「目标 != 当前即可更新」（§3.6）。这意味着即便最新 prerelease 的核心版本**低于**当前正式版（回退预发布），只要版本串不同也会判可更新——这是 prerelease 通道「切到某个 dev 构建」的预期语义（灰度 / 真机切换），与 stable 通道「仅更高才升级」的防御不同。stable 通道不受影响。
- **真机维度**：FR-85 的下载→替换→重启端到端真机验收不受本功能影响；本修以 fake 源单测覆盖版本判定与资产名匹配，**Linux 实机开启 prerelease 真正拉取并升级到 dev 构建**待用户真机验。
