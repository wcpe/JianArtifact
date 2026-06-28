# ADR-0032：交互式终端下的自更新重启与备份健壮性（扩展 ADR-0021/0026）

## 状态

已接受　·　扩展 ADR-0021（自更新重启）、ADR-0026（自更新回滚备份）

## 背景

真机（在 tmux 前台手动运行二进制）暴露自更新机制两处缺陷 + 一处需澄清的设计点：

1. **重启后进程脱离终端、日志不见**：`restart_mode=self` 用 `Command::spawn()` 把新版本拉起为**子进程**，旧进程随即 `process::exit(0)`——新进程变孤儿（reparent 到 init）、脱离 shell / tmux 作业控制：prompt 回到提示符、`Ctrl-C` 管不到它、日志虽仍写 tty 但与提示符混杂，用户感知为「自更新后进程不知去向、看不到日志」。本质是 spawn+exit 模型把进程交给了 init 而非留在当前前台。
2. **备份文件 compound 堆积**：备份 / 暂存名由 `sibling_with_suffix` 把后缀 append 到 `current_exe` 文件名上。当用户（因上面的进程丢失而**手动跑备份文件**去恢复）运行的二进制名本身已带 `.bak` / `.rollback.bak` 后缀，再触发更新 → 叠成 `.bak.bak` / `.bak.rollback.bak`，目录里堆出一串备份。
3. **原地替换保留原文件名（澄清，非 bug）**：自更新原子替换 `current_exe`、**不改路径 / 文件名**——更新到新版后文件名仍是下载时的旧版串（如 `…-dev.8-…`）。这是**设计行为**：保路径稳定，service manager / 启动脚本 / `.rollback.bak` 派生都依赖路径不变；二进制 `--version` 自报真实版本，文件名只是会过期的标签。重命名会破坏上述引用，故不重命名。

## 决策

1. **Unix `exec` 原地替换重启**：`restart_mode=self` 在 Unix 改用 `std::os::unix::process::CommandExt::exec()`——新映像**原地接管同一进程**（同 PID / 同终端 / 同前台作业 / 同会话），tmux 等交互前台运行自更新后进程**不脱离作业控制、日志连续可见**。端口已在优雅停机（`serve` 返回）时释放，`exec` 后新映像启动再绑，无新旧争用。`exec` 成功永不返回，返回即失败按错误上抛。Windows 无 `exec` 语义，**保持 `spawn`+`exit`**（新进程脱离当前控制台、端口已释放，交外部进程管理器 / 用户感知）。
2. **派生名防 compound + 启动清理 compound 残留**：
   - 派生 `staged` / `.new` / `.bak` / `.old` / `.rollback.bak` 前，先剥离 exe 名末尾已叠的更新管理后缀（`strip_managed_suffixes`，集合 `{.rollback.bak, .bak, .old, .new}`）得「规范 exe 名」再追加——正常运行时 exe 名不带这些后缀，剥离为**恒等**、行为不变；仅在用户跑了备份文件的异常路径上收敛、不再叠加。
   - `execute_replace`（Unix `.bak` copy / Windows `.old` 删除）与 apply 的 `.rollback.bak` copy 处**守自拷贝 / 自删**：剥离后规范名可能恰等于当前 exe，跳过避免毁源 / 误删运行二进制。
   - 启动清理（`cleanup_stale_artifacts`）增删「规范名 + ≥2 层管理后缀」的 **compound 残留**（`.bak.bak` 等）；**保留**单层 `.bak`（ADR-0021 事务兜底）与 `.rollback.bak`（ADR-0026 持久回滚源）两份不动。
3. **原地替换保留原文件名**：不重命名二进制（路径稳定优先），以本 ADR + 文档澄清此为预期行为、非缺陷。

## 理由

- **`exec` 是交互前台自更新的正解**：进程映像替换天然保住 PID / 终端 / 前台作业，无需把进程交给 init；端口在 `exec` 前已由优雅停机释放，时序安全。Windows 无对应语义只能 `spawn`+`exit`，按平台分支。
- **剥离后缀只在异常路径生效、零正常态影响**：守「正常运行 exe 名不带管理后缀 → 恒等」，故对既有路径推导无行为变化（既有用例不变）；只把用户手动跑备份导致的 compound 收住。
- **保留两份备份不动**：用户明确选择保守口径——`.bak`（事务兜底）与 `.rollback.bak`（回滚源）各有用途，本 ADR 只修 compound、不裁撤其一。

## 后果

- 正面：tmux / 交互前台自更新后进程不脱离、日志连续；备份目录不再随异常恢复操作 compound 堆积；既有清理 + 回滚链路不变。
- 约束 / 负面：`exec` 路径为 Unix 专属、`spawn`+`exit` 为 Windows 专属，二者由 `cfg` 分支，端到端「替换→重启→新版本接管终端」**仍依赖真机验证**（exec 的终端 / 端口时序无法在进程内单测）。compound 清理为 best-effort（失败仅 WARN、下次启动重试）。
- 不变：原子替换语义、`.rollback.bak` 持久回滚源、`[update] enabled` 出站门控、凭据不外发等均不改。

## 备选方案

- **保持 spawn+exit，仅补文档（用 systemd 等外部管理器）**：不修交互前台体验，用户在 tmux 仍丢进程，落选（选 Unix exec 原地修好）。
- **自更新后把二进制重命名为新版本名**：破坏 service manager / 脚本 / `.rollback.bak` 派生对路径的引用，落选（保留原名 + 文档澄清）。
- **裁撤 `.bak` 或 `.rollback.bak` 只留一份**：减少冗余但牵动 ADR-0021/0026 既定语义，超出本次「修 compound」范围，落选（保留两份、只防叠加）。
