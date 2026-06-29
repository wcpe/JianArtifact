# 功能规格：完整 Maven 快照规则（FR-122）

> 状态：开发中　·　关联 PRD：FR-122（增强 FR-14/61）　·　ADR：ADR-0037（服务端权威元数据，本特性为其快照级扩展）

## 1. 背景与目标

在 FR-121（服务端权威 artifact 级 `maven-metadata.xml` + pom 三级兜底）之上，补齐 **SNAPSHOT 完整规则**：服务端为快照生成**唯一时间戳版本**与**快照级 `maven-metadata.xml`**，使 `mvn` 客户端按 Maven 快照规则解析「最新快照」。属 ADR-0037 决策的快照级扩展，不另立 ADR。

## 2. 需求（要什么）

- **唯一时间戳版本**：SNAPSHOT 主构件经 **Web 上传**时，服务端按 `{a}-{base}-{yyyyMMdd.HHmmss}-{buildNumber}.{ext}` 铸造唯一时间戳版本落库（版本目录仍为 `{base}-SNAPSHOT`），时间戳取真实 now、构建号据目录现有最大值 +1。
- **快照级 metadata**：写入快照时间戳构件后，服务端按目录内时间戳构建聚合生成 `{base}-SNAPSHOT/maven-metadata.xml`（`snapshot/timestamp/buildNumber`、`snapshotVersions/snapshotVersion(extension/value/updated)`、`lastUpdated`）+ 四校验和 sidecar。
- **client-priority**：`mvn deploy` 自带的时间戳构件不被改写（artifact_bytes 为 None、不兜底 pom）；服务端据目录扫描权威重生成快照级 metadata（与客户端计算等价/超集）。
- **覆盖语义不破坏**：release 不可覆盖；snapshot 时间戳唯一、可拉最新（FR-61 既有策略不变）。

不做（范围外）：classifier（`-sources` / `-javadoc`）独立 snapshotVersion 细分；snapshot 清理 / 保留策略（GC，后续期）。

## 3. 设计（怎么做）

### 纯函数（`src/format/maven.rs`，可穷举单测）

- `is_snapshot_version` / `snapshot_base`：判定与基版本提取。
- `snapshot_artifact_filename` / `snapshot_artifact_path` / `snapshot_metadata_path`：时间戳构件与快照 metadata 路径拼装。
- `parse_snapshot_build(file_name, a, base) -> (ts, buildNumber, ext)`：解析时间戳构件名（字面 `-SNAPSHOT` 名与 sidecar 不匹配）。
- `collect_snapshot_builds -> SnapshotBuilds`：扫描目录内时间戳构建；`next_build_number()` / `latest_key()`。
- `build_snapshot_metadata`：生成快照级 XML（最新构建的 snapshot 块 + 各扩展 snapshotVersions + lastUpdated）。
- `epoch_to_snapshot_timestamp(secs)`：Unix 秒（UTC）→ `yyyyMMdd.HHmmss`，用 Hinnant `civil_from_days` 自算，**不引入时间库依赖**。

### 编排（`src/api/maven_publish.rs`）

- `mint_snapshot_path`：Web 上传前据现有构建号 + 真实 now 铸造时间戳路径（upload 路由在 `put_hosted` 前改写落库路径）。
- `maintain_after_maven_write` 扩展：识别快照时间戳构件 → pom 兜底用同时间戳唯一名 → 重生成快照级 metadata（`regenerate_snapshot_metadata`）→ 再重生成 artifact 级 metadata（FR-121，把 `{base}-SNAPSHOT` 列为一个版本）。

### 接线

- `src/api/upload_routes.rs`：`maybe_mint_snapshot_path` 对 Maven 快照主构件改写存储路径；其余不变。
- `src/api/format_routes.rs`：mvn deploy 时间戳构件 PUT 后经 `maintain_after_maven_write(None)` 触发快照级 metadata 重生成。

时间戳唯一版本铸造的 `now` 与构建号扫描属编排层（impure），纯协议逻辑全部下沉 `MavenFormat`。

## 4. 任务拆分

- [x] maven.rs 快照纯函数 + 单测（判定 / 基版本 / 路径 / 解析 / 收集 / metadata 生成 / epoch 转时间戳）。
- [x] maven_publish.rs：mint_snapshot_path + maintain 扩展 + regenerate_snapshot_metadata。
- [x] upload_routes 接线（maybe_mint_snapshot_path）。
- [x] 集成测试（tests/maven_api.rs）：Web 传 SNAPSHOT → 时间戳版本 + 快照 metadata；连传两次 → 构建号递增；mvn deploy 时间戳构件 → 服务端重生成快照 metadata。
- [x] 文档同步：本规格、PRD 状态、CHANGELOG、API.md、ARCHITECTURE。

## 5. 验收标准

- `rustup run 1.96.0 cargo test`（单元 + tests/maven_api.rs）全绿；`cargo clippy --all-targets`、`cargo fmt --check` 通过。
- **实机（待真机，需用户确认）**：`mvn deploy` 一个 SNAPSHOT → 远端出现时间戳构件 + 快照 `maven-metadata.xml`；另一项目 `mvn dependency:get` 拉该 SNAPSHOT 解析到最新时间戳构件；Web 上传 SNAPSHOT 后 `mvn` 同样可解析。本地以集成测试断言生成结构正确，真 `mvn` 互通标「待真机」。

## 6. 风险 / 待定

- **并发铸造**：同一快照并发 Web 上传可能取到相同构建号（last-writer），与 FR-121 metadata 一致性同属「最终一致、不引锁」取舍。
- **真机 mvn 快照解析**：属真机维度，本地不可全验，留「手动真机验收」（绝不假装跑过 mvn）。
