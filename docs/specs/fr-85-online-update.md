# 功能规格：在线更新（管理员手动触发的完整自更新）

> 状态：开发中　·　关联 PRD：FR-85　·　关联 ADR：ADR-0021　·　分支：feature/fr-85-online-update

## 1. 背景与目标

当前升级靠运维手动「停进程 → 替换二进制 → 重启」（OPERATIONS §2）。FR-85 在此之上提供**管理员手动触发的完整自更新**：控制台/API 查 GitHub 最新稳定 Release、与当前版本比对，管理员确认后由程序自行下载本机平台资产、校验 sha256、原子替换二进制并自动重启——把手动三步收敛为一次点击。

数据来源是 FR-86 发布流水线产出的 Release 资产（命名契约见 `docs/specs/fr-86-ci-release.md`）。出站经 FR-84 统一出站客户端（honor `[network.proxy]`）。

属 P2。涉及「出站到 GitHub + 自替换运行中二进制 + 自重启」的架构决策 → 配 **ADR-0021**。

## 2. 需求（要什么）

### 范围内
- **更新检查**（仅 Admin）：查配置仓库的最新稳定 Release，返回 `{当前版本, 最新版本, 是否有更新, 资产名, 发布说明}`。
- **应用更新**（仅 Admin，手动触发）：按本机 target 取对应资产 → 流式下载到临时文件、边下边算 sha256 → 取同名 `.sha256` 资产比对 → 一致则**原子替换**当前二进制 → 触发**自动重启**。
- **出站默认关闭**：`[update] enabled=false` 时检查/应用端点一律拒绝（不联网）。
- **仓库源可配**：默认 `wcpe/JianArtifact`；`api_base_url` 可配（便于测试/镜像）。
- **可选凭据**：公开仓库免凭据；私有仓库经可选 token（真源 env `JIANARTIFACT_UPDATE_TOKEN`，**不入库、不进日志**）。
- **校验失败保护**：sha256 不一致即拒绝替换、删临时文件、保留旧二进制、进程仍以旧版运行。
- **跨平台替换**：Unix `rename` 覆盖（运行中进程保旧 inode）；Windows 先把运行中 `.exe` 改名为 `.old` 再落新文件，下次启动清理 `.old`。
- **自动重启**：`restart_mode` = `self`（默认，重启后自拉起新进程）或 `exit`（仅退出，交外部进程管理器 systemd/docker 重启）。

### 不做（范围外，YAGNI / 守范围纪律）
- 不做无人值守定时自动更新（仅管理员手动触发；定时另议）。
- 不做增量/差分更新、不做多版本回滚管理（仅「保留上一个旧二进制副本」做单步回退兜底）。
- 不做签名验签 / SBOM（仅 sha256 完整性校验；签名需另写 ADR + 签名基建）。
- 不做更新通道（stable/beta）切换；仅取 GitHub「最新稳定 Release」。
- 不新增第三方依赖（版本比较、target 推导、JSON 解析均用现有 `serde_json` / `sha2` / 标准库）。

## 3. 设计（怎么做）

### 3.1 模块与分层
- 新增顶层模块 `src/update/`（编排，依赖 `config` 取出站 helper 与版本；**不依赖 meta**，自更新不碰 DB），与 `migrate` / `vuln` 同级。
- `src/api/update.rs`：薄 handler，仅做鉴权、调用 `update` 模块、错误映射；不写业务。
- 依赖方向：`api → update → config`，单向无环。

### 3.2 Release 来源抽象（可测）
仿 `NexusClient` / `MirrorSource`：
```rust
trait ReleaseSource {
    async fn fetch_latest_release(&self) -> Result<Release, UpdateError>;
    async fn download_asset(&self, url: &str) -> Result<AsyncRead 流, UpdateError>;
}
```
- 生产实现 `GithubReleaseSource`：经 `build_outbound_client`（FR-84，honor 代理）请求 `{api_base_url}/repos/{repo}/releases/latest`，带 `User-Agent`（GitHub API 必需）+ 可选 `Authorization: Bearer <token>`；解析 `tag_name` / `name` / `body` / `assets[].name` / `assets[].browser_download_url`。
- 测试用 fake 实现注入构造好的 `Release` 与字节流，**不触网**。

### 3.3 平台 target 推导（纯函数，可测）
据 `std::env::consts::{OS, ARCH}` 映射到 FR-86 三目标之一：
| OS / ARCH | target | ext |
|---|---|---|
| linux / x86_64 | `x86_64-unknown-linux-gnu` | （空） |
| windows / x86_64 | `x86_64-pc-windows-msvc` | `.exe` |
| macos / aarch64 | `aarch64-apple-darwin` | （空） |
其余组合 → `UpdateError::UnsupportedPlatform`（该平台无自更新资产，明确报错不静默）。

### 3.4 版本比较（纯函数，可测）
- 当前版本 `env!("CARGO_PKG_VERSION")`；最新版本取 `tag_name` 去前导 `v`（prerelease 滚动 `tag_name=dev` 时回退取 release 标题 `name`，见 fr-89 §3.6）。
- **stable 通道**：解析 `major.minor.patch` 三段整数比较；预发布 / 构建元数据后缀忽略（`/releases/latest` 只返稳定版）。非法版本串报错。`update_available = latest > current`。
- **prerelease 通道**（FR-89）：dev 预发布常与当前正式版共享核心版本，故改按**完整版本串**判定（目标 != 当前即可更新），详见 fr-89 §3.6。判定经 `is_update_available_for_channel(channel, …)` 按通道分流。

### 3.5 资产名推导（纯函数，可测）
`jianartifact-{latestVersion}-{target}{ext}`（见 FR-86 §3.1）；对应 `.sha256` 资产为该名 + `.sha256`。在 Release `assets[]` 里按名精确匹配；缺资产或缺 sha256 → 报错。

### 3.6 下载 + 校验
- 流式下载二进制资产到 `data_dir/update-tmp/{资产名}`，边写边用 `sha2::Sha256` 计算（不二次读盘、不整体载入内存，守流式不变量）。
- 下载 `.sha256` 资产取其裸 64 位十六进制内容，与实算值**定长比较**（不一致即 `UpdateError::ChecksumMismatch`，删临时文件、不替换）。

### 3.7 原子替换（跨平台，可测核心）
`current_exe = std::env::current_exe()`。把校验过的临时文件落到 exe 所在目录（**同卷**，保证 rename 原子；跨卷 rename 失败 → 先 copy 到同目录临时名再 rename）。
- **Unix**：给临时文件置 `0755`，`std::fs::rename(tmp, current_exe)` 原子覆盖；运行中进程持旧 inode 不受影响。替换前把旧 exe 复制一份为 `{exe}.bak`（单步回退兜底）。
- **Windows**：`rename(current_exe, {exe}.old)`（运行中 exe 可改名）→ `rename(tmp, current_exe)`；失败则尽力把 `.old` 改回。`{exe}.old` 下次启动时清理。
- 替换规划函数 `plan_replace(current_exe) -> ReplacePlan` 与执行函数分离，规划/路径推导可跨平台单测；执行按 `cfg!(windows)` 分支。
- **守不变量**：只有 sha256 校验通过才进入替换；替换是最后一步，校验失败永不触碰二进制。

### 3.8 自动重启（最易出错、待真机）
经 **graceful-shutdown 先于拉起新进程** 排掉端口竞争：
1. 替换成功后，handler 置进程级 `RestartRequest{ restart_mode, exe, argv }`（随 `AppState` 共享的 `Arc<Mutex<Option<…>>>` 或 `OnceLock`）并触发关停通知（`tokio::sync::Notify` / watch，注入 `AppState`）。
2. handler 返回 `200 {状态:"已更新，正在重启", 新版本}`；axum graceful shutdown **drain 在途请求**（含本响应）后 `serve` 返回。
3. `main.rs` 在 `serve` 返回后检查 `RestartRequest`：
   - `self`：`std::process::Command::new(exe).args(argv).spawn()` 拉起新进程（此时旧进程已停止 accept、端口已释放），随后旧进程正常退出。
   - `exit`：直接退出码 0，交外部进程管理器重启（适配 systemd/docker；避免与管理器重启叠加成双实例）。
4. `shutdown_signal()` 扩展为 `select!(ctrl_c, restart_notify)`，两路皆触发优雅停机。
> 重启序列**无真机不可验**：本会话实现并单测可测部分，端到端「替换→重启→新版本起来」标**待真机验**（Linux + Windows 各一遍）。

### 3.9 配置 `[update]`
```toml
[update]
enabled = false                          # 出站默认关闭
repo = "wcpe/JianArtifact"               # 仓库源可配
api_base_url = "https://api.github.com"  # 可配（测试/镜像）
restart_mode = "self"                    # self | exit
download_timeout_secs = 300
# token 真源 env：JIANARTIFACT_UPDATE_TOKEN（私有仓库可选；不入库不进日志）
```
注册 `"update"` 进 `config.rs` 的 `KNOWN_SECTIONS`；token 经 env 覆盖，序列化时不回显。

### 3.10 API（仅 Admin，挂 `/api/v1/update/*`）
- `GET /api/v1/update/check` → `{current_version, latest_version, update_available, asset_name, notes}`；`enabled=false` 返 409「在线更新未启用」。
- `POST /api/v1/update/apply` → 下载+校验+替换+触发重启；成功 `200 {status, new_version}`。`enabled=false` 拒绝；非 Admin/匿名 403。**并发单飞**：apply 进程级互斥，已有一次自更新在途时再次触发返回 409「更新进行中」、不竞争临时文件（M2）。
- 错误映射：上游不可达/超时 → 502；校验失败 → 422/400（明确文案）；平台不支持 → 400；无更新可用 → 409；已有自更新在途 → 409「更新进行中」。
- 同步 `docs/API.md`。

## 4. 任务拆分
- [x] 写规格（本文）+ ADR-0021 + PRD §4 FR-85 计划→开发中（仅改 FR-85 行）
- [x] `config.rs`：`[update]` 配置 + `KNOWN_SECTIONS` 注册 + env 覆盖单测
- [x] `src/update/`：`ReleaseSource` trait + `GithubReleaseSource`（生产，经 build_outbound_client）+ 纯函数（target 推导 / 版本比较 / 资产名推导 / sha256 校验 / 替换规划）+ 错误类型
- [x] 替换执行（unix rename + bak / windows .old swap）+ 启动期清理 `.old`
- [x] 重启请求 + main.rs graceful-shutdown 后拉起新进程（self/exit）
- [x] `src/api/update.rs`：check / apply handler（require_admin、错误映射）+ 路由挂载 + AppState 注入关停通知
- [x] 测试先行：纯函数穷举 + fake ReleaseSource 的 check/apply（校验通过/不一致/缺资产/平台不支持/enabled=false/非 Admin）+ 替换规划跨平台单测
- [x] doc-sync：ARCHITECTURE（update 模块 + 重启机制）、OPERATIONS（在线自更新路径）、API、CONFIG、CHANGELOG

## 5. 验收标准
- `GET /update/check`：返回当前/最新/是否有更新；`enabled=false` 拒；非 Admin/匿名 403。
- `POST /update/apply`：fake 源（资产 + 正确 sha256）→ 走到替换并触发重启请求（单测断言替换计划/落地 + 重启请求置位）；**端到端真机另验**。
- sha256 不一致 → 拒绝替换、删临时文件、保留旧二进制、报错。
- 出站默认关闭时不联网；GitHub 不可达/超时 → 502，不影响主服务。
- 平台不支持（如 linux/arm64）→ 明确报错，不乱下资产。
- 出站经 `[network.proxy]`（FR-84）注入的代理。
- token/凭据不进日志、不入库、不回显。
- 单元/集成测试绿；clippy `-D warnings`、fmt 过。
- **【需用户确认 · 真机维度】** 下载→替换→自动重启→新版本运行的端到端，需在 **Linux 与 Windows 各真机跑一遍**（含 Windows `.exe` 运行中改名 `.old` + 自拉起）；真机过前本规格保持「开发中（待真机验）」。

## 6. 风险 / 待定
- **重启竞态**：靠「graceful-shutdown 排空 → 释放端口 → 拉起新进程」规避端口争用；真机验证此序列在 Linux / Windows 均无双绑定 / 无残留。
- **Windows 运行中 exe 改名**：依赖 NTFS 允许重命名运行中 `.exe`；若被杀软/句柄锁定可能失败 → 失败即尽力还原 `.old`、报错、进程续以旧版跑。
- **同卷限制**：临时文件须与 current_exe 同卷以保 rename 原子；跨卷先 copy 再 rename。
- **GitHub API 速率限制**：匿名 60 次/时；私有/高频经可选 token 提额。`/releases/latest` 不含 prerelease，FR-86 的滚动 `dev` 预发布不会被自更新选中（符合「仅稳定版」）。
- **VERSION 文件漂移**（非本功能）：版本真源是 `Cargo.toml`（`CARGO_PKG_VERSION`），自更新比较用之；根 `VERSION` 文件漂移由 FR-86 已记的 chip 另行处置。
