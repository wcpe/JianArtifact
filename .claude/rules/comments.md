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

## 5. 禁止需求编号进入任何代码

**需求编号（`FR-xxx`）只允许出现在 `docs/` 下的文档与 `CHANGELOG.md`。**

除上述两处，**任何位置**都不得出现需求编号——包括但不限于：

| 位置                  | 举例（都是错误写法）                       |
| --------------------- | ------------------------------------------ |
| 源码注释（任意语言）  | `// FR-149：搜索索引回填`                  |
| 块注释 / 文档注释     | `/** FR-40 追加七：一键重启 */`            |
| SQL 注释              | `-- FR-58: 性能索引补齐`                   |
| 配置文件注释          | `# FR-66 引入`                             |
| 标识符                | `var fr149Config`、`func FR36Check()`      |
| 测试函数名 / 用例名   | `TestFR149Backfill`、`it("...（FR-36）")`  |
| **文件名与目录名**    | `fr149_search.go`、`0049_fr149_fts.sql`    |
| 前端 i18n 文案键名    | `search.fr153Suggest`                      |
| 面向用户的字符串      | `"该功能见 FR-149"`                        |
| 契约源与生成物        | `api/openapi.yaml` 的 description 里写编号 |
| 分支名 / 提交信息标题 | `feat/fr149-search`                        |

**为什么**：

1. **制造无意义冲突**——两条分支在同一处注释写不同编号时 Git 判为冲突，而代码逻辑可能完全相同；合成本被抬高，冲突无法自动化解。
2. **对读者零价值**——读者需要的是"这段代码为什么这样写"，不是"它属于哪张需求单"。要追溯需求，按关键词搜 `docs/PRD.md` 更可靠，且能搜到完整上下文。
3. **会过期**——编号与代码的对应关系随需求拆分/合并而变化，代码里的编号很快就是错的。

✅ 正确

```
// 搜索索引的后台回填：分批推进水位，不阻塞启动，可中断续跑
func (s *SearchIndexService) Start(ctx context.Context) { ... }

// 测试函数名用行为描述
func TestSearchIndexBackfillResumesFromWatermark(t *testing.T) { ... }

// 需要指向设计依据时写文档路径
// 细节见 docs/adr/0030-fts5-search-index.md
```

❌ 错误

```
// FR-149：搜索索引回填
func (s *SearchIndexService) Start(ctx context.Context) { ... }

func TestFR149Backfill(t *testing.T) { ... }

it("包级聚合（FR-150）", () => { ... })

// 迁移 0049：FR-149 的 FTS5 索引
```

**没有存量豁免。** 仓库内历史编号已全部清理完毕，当前是零容忍；门禁脚本 `scripts/check-fr-numbers.sh` 会在质量门第一步执行，发现任何一处即失败。新代码若因从其它分支合并而带入编号，必须一并清除。
