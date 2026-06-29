# 功能规格：Maven 服务端权威 maven-metadata.xml + pom 三级兜底（FR-121）

> 状态：开发中　·　关联 PRD：FR-121（增强 FR-14/61）　·　ADR：ADR-0037（取代 p1-maven「客户端自管 metadata」）

## 1. 背景与目标

旧决策让 Maven `maven-metadata.xml` 与 pom 由 `mvn` 客户端自管，Web 上传来源的版本不进 metadata、缺 pom，`mvn dependency:get` 解析不到。本特性让**服务端权威生成并维护** artifact 级 `maven-metadata.xml` 与 pom（三级兜底），遵循 **client-priority**：客户端已提供的为主，服务端只补缺并维护聚合。详见 ADR-0037。

本规格覆盖 **release 级** artifact metadata（`versions/latest/release/lastUpdated`）与 pom 兜底。SNAPSHOT 时间戳唯一版本 + snapshot 级 metadata 属 FR-122，不在本规格。

## 2. 需求（要什么）

- **artifact 级 metadata 权威生成**：Maven 主版本文件写入 hosted 后，服务端按 SQLite 索引聚合该 `{g}/{a}` 全部版本，重生成 `{g路径}/{a}/maven-metadata.xml`（含 `latest` / `release` / `versions` / `lastUpdated`）+ 四校验和 sidecar。
- **pom 三级兜底**：保证主构件 GAV 下存在 `{a}-{v}.pom`——① 客户端已传 pom 不覆盖（client-priority）→ ② jar 内嵌 pom 原样提取（复用 FR-120）→ ③ 按 GAV 生成最小 pom（`modelVersion` + GAV + `packaging`）；仅缺失时生成，附四校验和 sidecar。
- **不破坏 mvn deploy**：`mvn deploy` 自带的 pom 不被改写；其 PUT 的 metadata 与服务端聚合等价。
- **client-priority 边界**：服务端绝不改写客户端 pom / 制品本体。

不做（范围外）：SNAPSHOT 时间戳 / snapshot metadata（FR-122）；Web 上传页「用户上传 pom」多文件接入（FR-123，本期只交付 jar 内嵌 + 最小 pom 两级能力）；plugin 级 `maven-metadata.xml`（插件仓库语义，本期不做）。

## 3. 设计（怎么做）

### 纯函数（`src/format/maven.rs`，可穷举单测）

- `pom_path(g, a, v)` / `artifact_metadata_path(g, a)`：按布局拼派生文件路径。
- `extract_embedded_pom(jar: &[u8]) -> Option<Vec<u8>>`：从 jar `META-INF/maven/.../pom.xml` 原样取字节（复用 FR-120 的 zip 条目定位）。
- `build_minimal_pom(g, a, v, packaging) -> Vec<u8>`：生成最小合法 pom。
- `derive_packaging(file_name) -> &str`：据主构件扩展名推断 packaging（jar/war/ear，默认 jar）。
- `collect_versions(records, g, a) -> MavenVersions`：从前缀 `{g路径}/{a}/` 下记录提取 version（去重、按首见 `created_at` 升序），并算 `last_updated`（max `created_at` 去非数字截 14 位）。
- `build_artifact_metadata(g, a, &MavenVersions) -> Vec<u8>`：拼 `maven-metadata.xml`；`latest` = 末位版本、`release` = 末位非 SNAPSHOT 版本；文本值 XML 转义。

### 写入后编排（`src/api/maven_publish.rs`，handler 保持薄）

`maintain_after_maven_write(state, repo, format, written_path, artifact_bytes: Option<&[u8]>)`：

1. 跳过条件：`written_path` 为 `maven-metadata.xml` 或 sidecar，或不能反解 GAV → 直接返回（不触发）。
2. **pom 兜底**（仅 `artifact_bytes` 为 `Some`，即 Web 上传路径；且主构件非 .pom 自身）：若 `pom_path` 不存在 → `extract_embedded_pom` 否则 `build_minimal_pom` → `put_hosted` pom + 四 sidecar。
3. **metadata 重生成**（两条写路径都做）：`list_artifacts_under_prefix({g路径}/{a}/)` → `collect_versions` → `build_artifact_metadata` → `put_hosted` metadata + 四 sidecar。

### 接线

- `src/api/format_routes.rs` `put_artifact_inner`：Maven hosted PUT 成功后调 `maintain_after_maven_write(..., None)`（client-priority，不兜底 pom，只维护 metadata）。
- `src/api/upload_routes.rs` `upload_artifact`：Maven 主构件写入并补 sidecar 后调 `maintain_after_maven_write(..., Some(&file.bytes))`（兜底 pom + 维护 metadata）。

### 复用既有机理（不重造）

`put_hosted`（blob 先落盘校验再写索引、失败回滚）、四校验和、`list_artifacts_under_prefix`、统一下载 / 浏览路径、`maven-metadata.xml` 可覆盖策略（FR-61）均原样复用。锁外 IO：编排在主写入返回后顺序 `put_hosted`，不持锁。

## 4. 任务拆分

- [x] maven.rs 纯函数 + 单测（路径拼装 / 内嵌 pom 提取 / 最小 pom / packaging / 版本聚合排序去重 / metadata 生成 latest-release-lastUpdated / XML 转义）。
- [x] api/maven_publish.rs 编排 + 注册到 api/mod.rs。
- [x] 接线 format_routes（mvn deploy 路径）+ upload_routes（Web 路径）。
- [x] 集成测试（tests/maven_api.rs）：Web 传 jar（含内嵌 pom）→ 生成 pom + metadata；Web 传 jar（无内嵌 pom）→ 最小 pom；mvn deploy 模拟（PUT jar+pom）→ metadata 聚合且不破坏；多版本 → versions/latest/release 正确；release pom client-priority 不被覆盖。
- [x] 文档同步：本规格、PRD 状态、CHANGELOG、API.md（上传端点产出 metadata/pom 说明）、ARCHITECTURE（Maven 元数据机制一句）、ADR-0037。

## 5. 验收标准

- `rustup run 1.96.0 cargo test`（单元 + tests/maven_api.rs）全绿，覆盖高风险区 §2.2：metadata 生成与版本聚合正确、pom 兜底三级、sidecar 一致、release 覆盖语义不破坏。
- `cargo clippy --all-targets`、`cargo fmt --check` 通过。
- **实机（待真机，需用户确认）**：Web 传 release jar → `mvn dependency:get` 可解析；`mvn deploy` 仍正常；`maven-metadata.xml` 版本列表含 Web 与 mvn 两来源版本。本地以集成测试断言生成结构正确，真 `mvn` 互通标「待真机」。

## 6. 风险 / 待定

- **并发写同 artifact**：metadata 以「最后一次按当前已提交版本全量重生成」收敛、最终一致；不引锁（避免热路径持锁 IO）。
- **真机 mvn 互通**：`mvn dependency:get` / `mvn deploy` 属真机维度，本地不可全验，留「手动真机验收」（绝不假装跑过 mvn）。
