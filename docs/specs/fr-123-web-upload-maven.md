# 功能规格：Web 上传页 Maven 适配（FR-123）

> 状态：开发中　·　关联 PRD：FR-123（增强 FR-74）　·　依赖：FR-120（jar 内嵌 pom 解析）、FR-121（pom 三级兜底）、FR-122（快照）

## 1. 背景与目标

让 Web 控制台通用上传页（FR-74）产出**合规 Maven 制品**：上传 jar 时坐标可自动识别、可附带 pom、快照版有提示、支持 jar+pom 多文件。复用已落地的后端能力（FR-120/121/122），最小改动、无新依赖。

## 2. 需求（要什么）

- **坐标自动回填**：Maven 上传时 `groupId` / `artifactId` / `version` 可留空，服务端从 jar 内嵌 pom（FR-120）自动识别；表单填了则以表单为准，缺项由 jar 补齐；都无法得到则 400 并提示。
- **可选 pom（用户上传层）**：表单可附带 `pom` 文件，落在主构件旁同基名 `.pom`，作为 FR-121 pom 三级兜底的「用户上传」层（client-priority，不被服务端兜底覆盖）。
- **快照提示**：版本含 `-SNAPSHOT` 时前端提示「服务端将生成时间戳唯一版本」（FR-122）。
- **多文件**：单次上传 jar（主 `file`）+ 可选 `pom`。

不做（范围外）：前端读 zip 解析坐标（需新依赖，改用后端识别）；classifier（sources/javadoc）多构件；批量多制品上传。

## 3. 设计（怎么做）

### 后端（`src/api/upload_routes.rs`）

- `build_upload_path`（取代旧 `resolve_upload_path` + `maybe_mint_snapshot_path`）：Maven 经 `resolve_maven_coords` 解析坐标（表单可选 → jar 内嵌 pom 兜底），快照主构件铸造时间戳唯一路径（FR-122）；npm/raw 不变。
- `resolve_maven_coords`：`optional_text` 取表单坐标，缺项用 `MavenFormat::parse_gav_from_jar` 补齐；不全则 400。
- `store_user_pom_if_present`：表单含 `pom` 文件字段时，按 `derive_pom_path`（主构件换扩展名为 `.pom`）落库 + 四校验和 sidecar；**先于**服务端兜底写入，使 `maintain_after_maven_write` 的 `ensure_pom` 视其已存在而不覆盖（client-priority）。

handler 仍薄：坐标解析 / 路径拼装 / pom 落库委托上述函数与 `MavenFormat` 纯函数及 `maven_publish` 编排；不在 handler 写协议业务。

### 前端（`frontend/src/pages/UploadPage.tsx` + i18n `upload.ts`）

- Maven 三坐标输入去掉 `required`，`coordsReady` 对 Maven 恒真（仅需选主文件）；下方提示「坐标可留空，自动识别」。
- 新增可选 pom `FileInput`（仅 Maven）；`buildFormData` 坐标留空则不附带字段、有 pom 则 `append('pom', pomFile)`。
- 版本含 `-SNAPSHOT` 时显示快照提示。
- 文案进 `zh-CN/upload.ts`（`mavenCoordsHint` / `mavenPomLabel` / `mavenPomHint` / `mavenSnapshotHint`）。

## 4. 任务拆分

- [x] 后端 upload_routes：build_upload_path + resolve_maven_coords（jar 兜底）+ store_user_pom_if_present + derive_pom_path + optional_text。
- [x] 前端 UploadPage：坐标可选 + pom 输入 + 快照提示 + buildFormData 适配；i18n 文案。
- [x] 测试：后端集成（坐标留空自动识别 / 无坐标无内嵌 pom 400 / 用户 pom 不被覆盖）；前端 vitest（坐标留空可上传且 FormData 省略坐标 / 快照提示显示）。
- [x] 文档同步：本规格、PRD 状态、CHANGELOG、API.md。

## 5. 验收标准

- `rustup run 1.96.0 cargo test`（含 tests/maven_api.rs）全绿；`cargo clippy --all-targets`、`cargo fmt --check` 通过。
- `pnpm -C frontend run lint` / `test`（314）/ `build` 通过。
- **实机（待真机，需用户确认）**：网页上传一个含内嵌 pom 的 jar（坐标留空）→ 制品落于正确 GAV、可被 `mvn dependency:get` 解析；附带 pom 上传 → pom 为用户内容；上传 `-SNAPSHOT` → 生成时间戳唯一版本与快照元数据。视觉/真机交互标「待真机」（jsdom 不复刻 Mantine FileInput 真实交互与服务端识别全链）。

## 6. 风险 / 待定

- 前端不解析 zip，坐标在提交后由服务端识别——用户提交前看不到自动填充值（以最简、无新依赖为先）。如需「提交前预览坐标」可后续加后端预览端点或前端 zip 解析（需依赖，另议）。
- 真机 `mvn` 解析属真机维度，留「手动真机验收」。
