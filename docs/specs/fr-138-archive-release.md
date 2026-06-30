# FR-138 规格：发布产物打包压缩包 + 自更新适配解压

## 背景

线上下载的裸 musl 静态 ELF 被部分杀软（如 Kaspersky）误报为 `Trojan/Linux.Agent.bp`，
系启发式扫描对静态链接 ELF 的误判。改为发布**压缩包**可绕过该误报。

增强 FR-86（CI 发布流程）与 FR-89（自更新通道），改动 CI 工作流与 `src/update/` 模块。

---

## 1. 依赖取舍决策

### 方案选择：统一打 ZIP，不引入新依赖

**结论**：三平台（Linux / macOS / Windows）均打 `.zip`，不引入 `tar`/`flate2` 直接依赖。

**理由**：
- `zip = "8"` 已是直接依赖（用于读取 NuGet `.nupkg`），其读写能力（`ZipArchive::new` 解压）
  已可直接使用，无需新增依赖。
- 统一格式简化逻辑：资产名推导、解压代码、CI 打包脚本三处均无需按平台分支。
- `tar.gz` 方案需新增 `flate2`（直接依赖，当前仅传递依赖）和 `tar` crate，违反
  ADR-0021「不引新依赖」精神，且 zip 格式下载方解压同样便利。

---

## 2. 资产命名契约变更

### 2.1 过渡版双产物（本 FR 实施期间）

CI 每次构建同时上传两类资产（旧裸 exe + 新压缩包），保证在途的旧版自更新客户端
（仍按裸 exe 名查找资产）不受影响：

| 资产类型 | 命名格式 | 备注 |
|---|---|---|
| 裸可执行（过渡保留） | `jianartifact-{version}-{target}{ext}` | 维持现有命名，兼容旧版自更新 |
| 裸 exe sha256 | `jianartifact-{version}-{target}{ext}.sha256` | 裸 exe 校验和 |
| 压缩包 | `jianartifact-{version}-{target}.zip` | 三平台统一 zip |
| 压缩包 sha256 | `jianartifact-{version}-{target}.zip.sha256` | 压缩包校验和 |
| 压缩包 md5 | `jianartifact-{version}-{target}.zip.md5` | 额外 md5（PRD 要求） |

注：zip 内含单个二进制文件，文件名为 `jianartifact{ext}`（无版本/target 前缀，解压即用）。

### 2.2 自更新资产选择优先级

自更新（`apply_update`）在 Release 资产列表中按以下优先级选择资产：

1. **优先**：压缩包 `jianartifact-{version}-{target}.zip` + 其 `.sha256`（存在则选）
2. **回落**：裸可执行 `jianartifact-{version}-{target}{ext}` + 其 `.sha256`（向后兼容）

回落逻辑确保：当用旧版二进制（不含解压逻辑）更新到含压缩包的新版 Release 时，
若出于某种原因压缩包不可用，仍可用裸 exe 完成更新。

---

## 3. 自更新解压流程

### 3.1 下载压缩包路径（优先）

```
fetch_latest_release
  → 推导 zip_name = "{ASSET_PREFIX}-{version}-{target}.zip"
  → find_asset(zip_name) 命中 → 下载 zip 到 tmp_dir
  → 边流式写盘边算 sha256
  → 下载 zip.sha256 资产 → parse_sha256_content → verify_checksum
  → 校验通过 → 解压 zip（spawn_blocking）→ 取出单个二进制到 staged
  → stage_file(staged, exe同目录/.new)
  → execute_replace（原子替换，保留 rollback.bak）
```

解压细节（`zip::ZipArchive`，在 `spawn_blocking` 中执行）：
- 打开 zip → 找第一个文件（应有且仅有一个二进制）
- 流式读出写到 staged 临时路径
- 解压失败报 `UpdateError::Io`，清理临时文件

### 3.2 回落裸 exe 路径（向后兼容）

```
find_asset(zip_name) 未命中 → 回落到裸 exe 路径（现有逻辑不变）
```

### 3.3 sha256 校验对象

压缩包路径下，sha256 校验的是**压缩包整体**（与 CI 生成的 `.sha256` 对应），
不是解压后的二进制 sha256。这与现有裸 exe 路径（校验二进制本身）的口径一致——
均校验「下载到本地的那个文件」。

---

## 4. CI 工作流变更（`.github/workflows/release.yml`）

在现有「打包资产与校验和」步骤后追加打包 zip + 多校验和：

```bash
# 现有：生成裸 exe + .sha256（保留）
# 新增：打包 zip + .zip.sha256 + .zip.md5
zip_asset="jianartifact-${version}-${target}.zip"
zip -j "dist/${zip_asset}" "dist/${asset}"   # -j：不保留目录结构，只放二进制本身

# zip sha256
if command -v sha256sum >/dev/null 2>&1; then
  zip_sha=$(sha256sum "dist/${zip_asset}" | awk '{print $1}')
else
  zip_sha=$(shasum -a 256 "dist/${zip_asset}" | awk '{print $1}')
fi
printf '%s' "$zip_sha" > "dist/${zip_asset}.sha256"

# zip md5
if command -v md5sum >/dev/null 2>&1; then
  zip_md5=$(md5sum "dist/${zip_asset}" | awk '{print $1}')
else
  zip_md5=$(md5 -q "dist/${zip_asset}")
fi
printf '%s' "$zip_md5" > "dist/${zip_asset}.md5"
```

上传构件步骤同时上传裸 exe（含 .sha256）和 zip（含 .sha256 / .md5）。

---

## 5. 变更影响范围

| 文件 | 变更类型 |
|---|---|
| `.github/workflows/release.yml` | 新增打包 zip + 校验和步骤；上传构件含 zip |
| `src/update/mod.rs` | 新增 `archive_asset_name`、`extract_zip_binary`、修改 `apply_update_with_progress` 优先选 zip |
| `src/update/tests.rs` | 新增 zip 解压 + 回落 + 资产名推导测试 |
| `docs/specs/fr-138-archive-release.md` | 本文件（活文档）|
| `docs/PRD.md` | FR-138 状态 计划→开发中 |
| `docs/ARCHITECTURE.md` | 在线更新段补充发布产物格式与解压回落 |
| `docs/API.md` | 自更新资产契约补充压缩包命名与回落说明 |
| `CHANGELOG.md` | 未发布段追加一行 |

---

## 6. 验收标准

- [ ] CI：release 资产同时含裸 exe（含 .sha256）与 zip（含 .sha256 / .md5）——「待 CI」
- [ ] 自更新：压缩包资产存在时优先下载解压，裸 exe 回落路径保留——单测覆盖
- [ ] 自更新：zip sha256 校验不过时拒绝解压替换，临时文件清理——单测覆盖
- [ ] 真机更新一次（通过压缩包路径）——「待真机」
- [ ] 验证门：`cargo test`（update 模块全绿）、`clippy 0 warn`、`fmt`

---

## 7. 不做的事（范围边界）

- 不对 zip 内二进制再算 sha256（仅校验 zip 本身）
- 不改现有裸 exe 路径的任何语义（纯回落保留）
- 不引入 `tar`/`flate2` 直接依赖（统一 zip 已覆盖需求）
- 不实现 S3 存储、审计日志等 P2/P3 能力
