# 规则索引

本目录规则约束本仓库一切协作（人类与 AI 代理），**核心目标：防止漂移**——架构、范围、决策、文档、质量都不偏离既定方向。项目规则优先于全局规则（如有冲突以本目录为准）。

| 规则 | 防什么漂移 |
|---|---|
| [architecture-invariants.md](architecture-invariants.md) | **架构漂移**：锁定的架构不变量，违反即漂移 |
| [scope-discipline.md](scope-discipline.md) | **范围漂移**：阶段边界，禁越界 / 禁镀金 |
| [decision-alignment.md](decision-alignment.md) | **决策漂移**：动手前对齐 PRD/架构/ADR，不静默推翻 |
| [doc-sync.md](doc-sync.md) | **文档漂移**：文档随代码同 PR 更新 |
| [testing-and-quality.md](testing-and-quality.md) | **质量漂移**：验证门与质量底线 |
| [static-analysis.md](static-analysis.md) | **风格漂移**：格式化与静态检查工具链 + CI 门禁 |
| [comments.md](comments.md) | 注释中文 |
| [git-commit.md](git-commit.md) | 提交规范与文档入库边界 |

> 完整演进与维护流程见 [../../docs/CONTRIBUTING.md](../../docs/CONTRIBUTING.md)。
