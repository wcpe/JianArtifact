# 功能规格：首启自动生成默认 config.toml

> 状态：开发中　·　关联 PRD：FR-90　·　分支：feature/fr-90-config-gen

## 1. 背景与目标
单二进制交付后，运维拿到的目录里没有现成的 `config.toml`：仓库只入库 `config.example.toml` 占位，需手动复制改名才有配置。这导致「config 不释放、想开启在线更新 / 改代理却无处下手」的 UX 痛点。

目标：二进制首启时若配置文件路径（默认 `./config.toml`，或 `--config` 指定）**不存在**，自动写一份带中文注释的默认配置到该路径，并记 INFO 日志提示；**已存在则绝不覆盖**。运维开箱即有可编辑的配置文件。属阶段 P2。

## 2. 需求（要什么）
- 配置文件不存在 → 写入一份带中文注释的默认配置，且写出内容能被 `Config::load` 成功加载。
- 配置文件已存在 → 一字节不改（不覆盖、不合并）。
- 生成发生在 `main` 启动早期、在 `Config::load(&cli.config)` **之前**，使生成的文件随即被加载。
- 写入失败（目录无权限等）只记 WARN、不阻断启动：回落到「文件不存在」语义，照常用默认值 + env 加载。
- 范围内：仅 `config.toml` 的「缺失即生成」；模板取已入库的 `config.example.toml`（`include_str!` 编译期嵌入，保真带注释）。
- 不做（范围外）：不做配置项「补缺 / 迁移 / 升级合并」；不改 `Config::load` 的加载语义；不生成 `.env` 等其它文件；不动 `config.example.toml` 自身内容。

## 3. 设计（怎么做）
- **模板来源**：`include_str!("../config.example.toml")` 编译期嵌入仓库已维护的示例配置作为默认模板写出。选它而非从 `Config::default()` 反序列化生成 TOML，理由：示例文件是已评审、带丰富中文注释、随配置项演进同步维护的活模板（见 `docs/CONFIG.md`），保真且零额外序列化代码（简单优先）。
  - 已知局限：当前 `config.example.toml` 是「占位」示例，未穷举全部节（如 `[network.proxy]` / `[update]` 等未列出）。这不影响正确性——未列出的节由 `Config::default()` 内置默认值兜底，生成的文件仍能被 `Config::load` 成功加载；后续按 `docs/CONFIG.md` 同步补全示例时，生成内容自动跟进。
- **纯逻辑下沉到 `config` 层、IO 留在 `main`**（守分层与可测性）：
  - `config` 层新增常量 `DEFAULT_CONFIG_TEMPLATE`（嵌入的模板文本）与纯函数 `default_config_template() -> &'static str`，便于单测断言「模板非空且能被 `Config::load` 解析」。
  - `main.rs` 新增「缺失即生成」调用点：在 `Config::load` 之前，若 `cli.config` 不存在则用 `std::fs::write` 写入模板，成功记 INFO、失败记 WARN 后继续。判定与写入是一次性启动期 IO，用同步 `std::fs` 即可（早于 tokio 运行时大量并发，简单直接）。
- 不引入新依赖；不改依赖方向（`main` → `config`，单向）。无新 ADR（既有 ADR-0001 单二进制 + TOML 配置取向不变，本功能是其 UX 补全）。

## 4. 任务拆分
- [x] spec + PRD 状态置「开发中」（仅 FR-90 行）
- [x] 测试先行：①不存在→生成且可被 `Config::load` 加载；②已存在→不覆盖；③模板纯函数非空且可解析
- [x] `config` 层：嵌入模板常量 + `default_config_template()` 纯函数
- [x] `main.rs`：`Config::load` 前「缺失即生成」调用点（成功 INFO / 失败 WARN 不阻断）
- [x] 文档同步：PRD 状态、OPERATIONS（首启自动生成）、ARCHITECTURE（一句）、CHANGELOG 未发布段

## 5. 验收标准
- 单测：配置文件不存在时调用生成后，目标文件存在、内容非空、且 `Config::load` 成功加载（断言关键默认值）。
- 单测：配置文件已存在（写入哨兵内容）时调用生成后，文件内容逐字节不变。
- 单测：`default_config_template()` 返回非空文本，且把它写到临时文件后 `Config::load` 成功。
- 验证门：`cargo fmt --all --check`、`cargo clippy --all-targets -- -D warnings`、`cargo test` 全绿。
- 手动复验（best-effort，需用户确认）：空目录跑二进制，启动日志出现「已生成默认配置文件」INFO，且生成的 `config.toml` 带中文注释、可直接编辑；二次启动不再覆盖。

## 6. 风险 / 待定
- 模板与 `config.example.toml` 同源：示例若漏节，生成文件也漏该节（由内置默认值兜底、不影响加载）。属已知局限，随示例同步演进，不在本功能内补全示例。
