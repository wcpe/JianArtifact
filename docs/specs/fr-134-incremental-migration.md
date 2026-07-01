# 功能规格：离线迁移增量幂等续传

> 状态：开发中　·　关联 PRD：FR-134　·　分支：feature/fr-134-incremental

## 1. 背景与目标

FR-38/39 已实现离线 proxy/hosted 仓库搬运，并在 `ArtifactService::ingest_cached` /
`ingest_hosted` 服务层已天然幂等（同坐标同 sha256 复用既有记录、回滚多余落盘），可重复跑而
不破坏数据一致性。

但搬运编排层（`proxy.rs` / `hosted.rs`）当前**无法区分「新写入」vs「命中既有一致制品而跳过」**，
全部计入 `migrated`；跳过计数 `skipped` 仅含「失败跳过」，前端进度无法呈现增量效果。

FR-134 目标：让重跑时「命中既有一致制品」的路径**单独计数并上报**，支撑
「旧仓库未停时边迁边用、多次跑直到全量一致」的运维场景，并在进度页清晰展示跳过 / 新搬分类。

## 2. 需求（要什么）

范围内：
- 搬运单个 blob 时，若目标已存在且 sha256 一致（命中既有记录），该条计入**增量跳过**（`skipped_existing`），不计入新搬（`migrated`）；
- 进度快照 `OnlinePullProgress` 新增 `skipped_existing: usize` 字段，与现有 `skipped`（失败跳过）分开；
- 仓库级结果 `OnlineRepoMigrationOutcome`（及 proxy/hosted 的局部 `RepoMigrationOutcome` / `HostedRepoMigrationOutcome`）同步新增 `skipped_existing_artifacts: usize`；
- `ingest_cached` / `ingest_hosted` 返回值改为携带「是否命中既有记录」信息的枚举 `IngestOutcome`（`Written(ArtifactRecord)` / `AlreadyExists(ArtifactRecord)`），搬运层据此分类；
- 首次跑：`migrated` = N，`skipped_existing` = 0；再跑：`migrated` = 0（或仅新增/变化的），`skipped_existing` = N（全部命中）；
- 不破坏既有取消/暂停/失败跳过/单飞/blob 先落盘再写索引等不变量。

不做（范围外）：
- 不改在线拉取迁移（`online.rs`）的搬运路径（该路径当前不走离线 blob，下期再扩展）；
- 不修改前端（进度字段直接序列化暴露，前端可选择展示，但本 FR 不做前端 UI 改动）；
- 不引入新的外部依赖。

## 3. 设计（怎么做）

### 3.1 新增 `IngestOutcome` 枚举

在 `src/format/service.rs`（或 `src/format/mod.rs`）新增：

```rust
/// 制品写入结果（FR-134）：区分「新写」与「命中既有一致记录而跳过落盘」。
pub enum IngestOutcome {
    /// 本次写入为新写（blob 落盘 + 写索引均执行）。
    Written(ArtifactRecord),
    /// 同坐标同 sha256 已存在，本次为幂等重入（回滚多余落盘，复用既有记录）。
    AlreadyExists(ArtifactRecord),
}
```

`ingest_cached` / `ingest_hosted` 返回值从 `Result<ArtifactRecord, ServiceError>` 改为
`Result<IngestOutcome, ServiceError>`。既有调用处（非搬运路径，如测试）改为 `.map(|o| o.into_record())`。

### 3.2 `OnlinePullProgress` 新增 `skipped_existing` 字段

```rust
pub struct OnlinePullProgress {
    // ...（现有字段不变）
    /// 增量跳过数：目标已存在且 sha256 一致，本次幂等重入跳过落盘（FR-134）。
    pub skipped_existing: usize,
}
```

### 3.3 `OnlineRepoMigrationOutcome` 新增 `skipped_existing_artifacts`

```rust
pub struct OnlineRepoMigrationOutcome {
    // ...
    pub skipped_existing_artifacts: usize,
}
```

proxy/hosted 局部 Outcome 结构体同步新增该字段。

### 3.4 搬运层 `bump_progress` 改为 3 态

```rust
/// 进度计数三态（FR-134）。
enum BumpKind {
    /// 新写入。
    Migrated,
    /// 增量跳过（已存在一致）。
    SkippedExisting,
    /// 失败跳过。
    SkippedFailed,
}

fn bump_progress(progress: &Mutex<OnlinePullProgress>, kind: BumpKind) { ... }
```

`migrate_repo_artifacts` 对 `ingest_cached` / `ingest_hosted` 返回的 `IngestOutcome::Written`
→ `BumpKind::Migrated`，`IngestOutcome::AlreadyExists` → `BumpKind::SkippedExisting`，
错误 → `BumpKind::SkippedFailed`。

### 3.5 依赖关系

改动仅在 `src/format/service.rs`（ingest 返回值）、`src/migrate/online.rs`（OnlinePullProgress /
OnlineRepoMigrationOutcome）、`src/migrate/proxy.rs`、`src/migrate/hosted.rs`。
API 层 `src/api/migrate.rs` 进度直接序列化 `OnlinePullProgress`，新字段自动暴露，无需改 handler。

## 4. 任务拆分

- [x] 读懂现状（ingest 幂等路径、progress 结构、bump_progress）
- [x] 写规格（本文档）
- [ ] PRD §4 FR-134 行改「开发中」
- [ ] 新增 `IngestOutcome` 枚举，改 `ingest_cached` / `ingest_hosted` 签名及实现
- [ ] 新增 `skipped_existing` 到 `OnlinePullProgress` 和 `OnlineRepoMigrationOutcome`
- [ ] 改 proxy/hosted 搬运层 `bump_progress` 三态 + `migrate_repo_artifacts` 区分计数
- [ ] 单元测试（红→绿）：二次跑同源 `skipped_existing` = N、`migrated` = 0；进度计数正确区分三态
- [ ] 文档同步：CHANGELOG + API.md（若进度字段影响客户端）
- [ ] 验证门：clippy + fmt + test 全绿

## 5. 验收标准

- 首次搬运：`migrated` = N，`skipped_existing` = 0，`skipped`（失败跳过）= 失败数。
- 二次搬运同源（无新增）：`migrated` = 0，`skipped_existing` = N，`skipped` = 0。
- 部分新增：`migrated` = 新增数，`skipped_existing` = 既有一致数。
- 失败跳过不计入 `skipped_existing`：路径非法 / 读本体失败 / 写入失败仍计入 `skipped`。
- `done_assets` = `migrated` + `skipped_existing` + `skipped`（三态之和守恒）。
- 现有所有测试仍全绿（取消/暂停/失败/不可覆盖语义不变）。
- `GET /migrate/jobs/{id}` 进度响应自动含 `skipped_existing` 字段（序列化自动）。

## 6. 风险 / 待定

- `ingest_cached` / `ingest_hosted` 签名变更影响所有直接调用处（测试 + 非搬运路径），
  需逐一改 `.map(|o| o.into_record())`；改动量可控（由 `rustc` 报错引导）。
- 在线拉取迁移（`online.rs`）也调用 `ingest_hosted`，同步改其 `Ok(_) =>` 分支为 `Ok(IngestOutcome::Written(_)) | Ok(IngestOutcome::AlreadyExists(_))`（当前在线路径不需要区分计数，统一算 migrated 即可，本 FR 不改其语义）。
