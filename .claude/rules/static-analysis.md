# 代码风格与静态检查（防风格 / 质量漂移）

> 统一各组件的格式化与静态检查工具，并要求 CI 强制门禁——风格一致、低级问题挡在合并前。

## 1. 各组件工具链（按本项目技术栈选）

按本仓库各组件实际所用语言，从下表选取对应工具并固定到工程配置里：

- **Go**：`gofmt` / `goimports` 格式化 + `golangci-lint`（可启用 govet / staticcheck / errcheck / ineffassign / revive / bodyclose / sqlclosecheck 等）。
- **JavaScript / TypeScript（含 React / Vue 等）**：`eslint` + `prettier`。
- **Kotlin**：`ktlint` 或 `detekt`。
- **Java**：`spotless`（google-java-format / palantir）+ `checkstyle` 或 `pmd`。
- **Python**：`ruff`（lint）+ `black`（格式化），或 `flake8` + `isort`。
- **Rust**：`rustfmt` + `clippy`。
- 其他语言：选该生态主流的"格式化 + lint"组合，写进工程配置。

> 多语言仓库逐组件各选其一，配置文件放在对应组件工程内或仓库根，版本固定。

## 2. 强制要求

- **CI 门禁**：lint 与格式检查未过 → 不允许合并（与测试同级，见 `testing-and-quality.md`）。
- **本地**：提交前自行跑对应组件的 format + lint，不把格式问题留给 CI。
- **依赖漏洞**：用对应生态的漏洞发现工具作入口并纳入 CI（如 Go 用 `govulncheck`、Node 用 `npm audit` / `osv-scanner`、Python 用 `pip-audit`、Rust 用 `cargo audit` 等）。
- 工具与规则版本固定（写进配置 / 构建），避免不同机器结果不一致。

## 3. 与现有规则的关系

- 本规则是 `testing-and-quality.md` 的补充：测试管"行为对不对"，静态检查管"写法干不干净"。
- 禁用某条 lint 要在**配置里集中声明并注明原因**，不在代码里零散 `//nolint` / `// eslint-disable` 关闭（除非有明确理由并写明）。
