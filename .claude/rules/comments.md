# 注释规范

> 适用于仓库内所有源代码与配置文件（按本项目实际技术栈，如 Go / Kotlin / Java / TypeScript / Python / Rust / Gradle KTS / YAML 等）。

## 1. 强制规则

- **所有注释必须使用简体中文。** 禁止英文、日文、拼音等非中文语言。
- 包括但不限于：
  - 行注释 `//` / `#`、块注释 `/* */`、文档注释（包/导出标识符上方）
  - 各语言的文档注释（KDoc / Javadoc / JSDoc / docstring 等）
  - `TODO` / `FIXME` / `NOTE` 标记后的说明文字
  - YAML / Properties / JSON5 / Dockerfile / compose 中的注释

## 2. 例外（不视为注释）

- 代码中的英文标识符（包名、类型名、函数名、变量名）。
- 字符串字面量、日志文案、用户可见文本。
- 第三方生成代码中的原始注释（应隔离在 `build/` / `dist/` 等非 VCS 目录）。
- 许可证头（如需保留英文版权声明可豁免）。

## 3. 示例

✅ 正确

```
// 校验用户是否在线，离线时直接返回空
function resolve(id) { ... }
```

❌ 错误

```
// check if user online, return null when offline
function resolve(id) { ... }
```

## 4. 处理已有英文注释

- 改一处代码时，发现该处或紧邻位置有英文注释，可顺手译为中文。
- 禁止为"统一注释语言"对无关代码做大规模翻译式改动（精准修改原则）。

## 5. 禁止在代码中出现 FR 编号

**FR 编号只允许出现在 `docs/` 下的文档**（PRD、specs、adr、API、OPERATIONS、ARCHITECTURE、ROADMAP 等）与 `CHANGELOG.md`。

以下位置**一律不得**出现 FR 编号（`FR-123`、`FR-40 追加七` 等任何形式）：

- 源代码注释（Go、TypeScript、SQL、CSS、配置文件）
- 代码标识符与命名：变量名、函数名、类型名、测试函数名、文件名
- 迁移 SQL 的文件名与文件内注释
- 前端 i18n 文案键名

**为什么**：FR 编号进入代码后，两条分支各自在同一处注释里写不同编号时，Git 会判为冲突——即使代码逻辑完全相同。合并与 rebase 的成本被无谓抬高，冲突无法自动化解。编号对代码读者也没有价值：读者需要的是"这段代码为什么这样写"，而不是"它属于哪张需求单"；需要追溯需求时按关键词搜 `docs/PRD.md` 更可靠。

✅ 正确

```
// 搜索索引的后台回填：分批推进水位，不阻塞启动，可中断续跑
func (s *SearchIndexService) Start(ctx context.Context) { ... }

// 测试函数名用行为描述
func TestSearchIndexBackfillResumesFromWatermark(t *testing.T) { ... }
```

❌ 错误

```
// FR-149：搜索索引回填
func (s *SearchIndexService) Start(ctx context.Context) { ... }

func TestFR149Backfill(t *testing.T) { ... }

// 迁移 0049：FR-149 的 FTS5 索引
```

**存量处理**（与第 4 条同一精准修改原则）：仓库内已有大量历史 FR 编号引用，**不对其做无差别清理**——大面积改动本身会与主线产生新的冲突，与规则目的相悖。但：

- **新增代码必须遵守本规则**；
- **改动到某一行时，若该行含 FR 编号，顺手去掉**；
- 需要指向设计文档时，写文档路径（如 `docs/adr/0030-fts5-search-index.md`），不写编号。
