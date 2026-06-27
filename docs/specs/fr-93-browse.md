# 功能规格：仓库浏览重构（文件树 + 右侧详情 + 多格式依赖坐标 + HTML View 外链）

> 状态：开发中　·　关联 PRD：FR-93（增强 FR-13/66/68/75/76）　·　分支：feature/fr-93-browse

## 1. 背景与目标

现仓库浏览是「制品浏览」与「文件浏览」两个并列 Tab 的表格视图（FR-22 / FR-76），点文件再整页跳转到独立的制品详情页（FR-66 / FR-68），交互割裂、信息密度低，不符合第四期 Nexus-like UX 重构方向。

本 FR（纯前端、属第四期 Web UX 重构 epic）把仓库浏览重构成 **Nexus-like 左侧文件树 + 右侧制品详情面板** 的一体化视图：

- 左：仓库内文件树，目录逐级展开、文件可点选；每种格式专属 icon。
- 右：选中制品的详情（名称 / 坐标 / 大小 / 格式 / 四校验和 / 上传时间），**多格式依赖坐标**下拉切换，**HTML View** 外链（指向 FR-75 的 HTML 仓库索引视图），下载按钮。

不动后端：复用既有目录列表（FR-75 的 `listArtifacts` 索引）与制品详情端点（FR-66 的 `getArtifactDetail`）。多格式坐标在前端用 JS 模板生成。

## 2. 需求（要什么）

### 范围内
- **文件树 + 右侧详情布局**：把 `RepositoryDetailPage` 的「制品浏览 / 文件浏览」两 Tab 合并为单一「浏览」视图——左列文件树（仓库根 → 目录逐级展开 → 文件叶子），右列详情面板；点目录展开 / 收起，点文件在右侧加载详情。配置 / ACL Tab（仅管理员）保留不动。
- **每种格式专属 icon**：仓库根与文件叶子按仓库格式（maven / npm / docker / pypi / cargo / go / nuget / raw）显示专属图标；目录用文件夹图标。仅用现有 `@tabler/icons-react`。
- **右侧详情面板**：复用 `getArtifactDetail` 返回的元数据 / 四校验和 / 后端 usage 片段；新增前端生成的多格式依赖坐标区与 HTML View 外链、下载链接。
- **多格式依赖坐标**（前端 JS 模板生成）：下拉切换以下格式，显示对应片段 + 复制按钮：
  - Apache Maven、Gradle Groovy DSL、Gradle Kotlin DSL、Scala SBT、Apache Ivy、Groovy Grape、Leiningen、PURL（package URL）。
  - 仅对能反解出 GAV 的 **Maven 制品主构件** 产出上述全套坐标；GAV 反解规则与后端 `Gav::from_path` 对齐（路径段去空后，倒数第三段=artifactId、倒数第二段=version、其余前缀段以 `.` 连接=groupId，段数<4 不产出）。
  - PURL 对所有有标准坐标的格式给原生 purl（Maven `pkg:maven/group/artifact@version`）；其余格式（npm / docker / raw 等）不产出 Maven 全套坐标区（保留后端 usage 片段即可），坐标区合理留空 / 不渲染。
- **HTML View 外链**：在详情面板给一个外链按钮，指向 FR-75 的 HTML 仓库索引视图——即该制品所在目录的索引 URL `/{repoName}/{父目录}/`（尾斜杠 → 后端按 `Accept: text/html` 渲染 HTML 索引页）。`target="_blank"`，浏览器默认 Accept 命中 HTML 分支。
- **下载按钮**：指向制品原始下载 URL `/{repoName}/{path}`（无尾斜杠，单文件下载）。
- **鉴权 / 不泄露**：复用既有端点，私有 / 无权场景沿用后端过滤——`listArtifacts` 与 `getArtifactDetail` 已按读权限过滤 / 404，前端不额外暴露资源存在性，不新增绕过路径。

### 不做（范围外）
- 不碰导航 shell（FR-92）、页眉搜索（FR-94）、设置 / 监控页。
- 不改后端：不动 FR-68 后端 usage 片段生成、不动 FR-75 / FR-66 端点契约、不加坐标字段到 DTO。
- 不做目录级写 / 删 / 重命名；不做超大目录虚拟滚动（一次性 `listArtifacts` 客户端折叠，与 FR-76 现状一致）。
- 不为 npm / docker / raw 造「类 Maven 全套坐标」（它们无统一 GAV），保留各自后端原生 usage 片段。

## 3. 设计（怎么做）

纯前端重构，**无需新 ADR**（属 FR-75/76 既有浏览能力的 UI 重排 + 前端坐标模板）。

- **坐标模板（纯函数，新文件 `frontend/src/lib/coordinates.ts`）**：
  - `parseMavenGav(path): { groupId, artifactId, version } | null`——与后端 `Gav::from_path` 同规则的纯函数，便于穷举单测。
  - `buildCoordinateSnippets(format, path): CoordinateSnippet[]`——对 Maven 主构件产出 8 种坐标片段（Maven / Gradle Groovy / Gradle Kotlin / SBT / Ivy / Grape / Leiningen / PURL）；其余格式返回空数组。每片段含 `{ label, language, content }`。
- **HTML View / 下载 URL（纯函数，同上文件）**：
  - `htmlViewUrl(repoName, path)`——制品父目录索引 URL（`/{repo}/{dir}/`，根目录文件回退到 `/{repo}/`）。
  - `downloadUrl(repoName, path)`——`/{repo}/{path}`（逐段编码保留 `/`）。
- **格式 icon 映射（`frontend/src/lib/format.tsx` 或就近）**：`formatIcon(format)` 返回对应 `@tabler/icons-react` 组件。
- **页面重构（`RepositoryDetailPage.tsx`）**：
  - 把「制品浏览 + 文件浏览」两 Tab 合并为单一「浏览」Tab：左 `Tree`（Mantine `<Tree>` 或自绘可展开列表，复用 `buildDirectoryListing` 逐层折叠）、右详情面板。
  - 选中文件 → 调 `getArtifactDetail` 加载右侧详情；不再整页跳转 `ArtifactDetailPage`（该独立页保留供深链 `/artifact?repo=&path=`，复用同一详情面板组件）。
  - 详情面板抽为可复用组件 `ArtifactDetailPanel`（供内嵌与独立页共用），承载元数据 / 校验和 / 后端 usage / 多格式坐标 / HTML View / 下载。
  - 树的展开态由顶层 `FileTree` 集中持有（按目录**完整前缀路径**键，如 `com/example/`），向下传给各 `TreeLevel`；子层不再各自持有局部 state。这样折叠父目录再重新展开时，深层子目录的展开态不丢失（逐级展开不被重置）。
- **复用端点**：`listArtifacts`（FR-75 索引）、`getArtifactDetail`（FR-66/68）；不新增端点。

## 4. 任务拆分
- [x] `lib/coordinates.ts`：`parseMavenGav` + `buildCoordinateSnippets` + `htmlViewUrl` + `downloadUrl` 纯函数
- [x] `lib/coordinates.test.ts`：GAV 反解（含段数不足 / sidecar）、8 种坐标片段内容断言（Maven 全套）、HTML View / 下载 URL、非 Maven 格式留空
- [x] 格式 icon 映射（`lib/formatIcon.tsx`）+ 详情面板组件 `ArtifactDetailPanel`（元数据 / 校验和 / 后端 usage / 多格式坐标下拉 + 复制 / HTML View 外链 / 下载）
- [x] `RepositoryDetailPage.tsx`：合并两 Tab 为「浏览」（左树 + 右详情），点文件加载右侧详情
- [x] `ArtifactDetailPage.tsx`：改用同一 `ArtifactDetailPanel`（深链复用）
- [x] 测试：树渲染 + 展开、点文件 → 右侧详情、坐标下拉切换（Maven 全套断言）、HTML View 链接存在且指向正确；沿用既有私有 / 无权过滤（不泄露）
- [x] 文档同步：PRD 状态、ARCHITECTURE 前端结构一句、CHANGELOG 未发布段末尾追加一行

## 5. 验收标准
- 前端 `pnpm -C frontend test` 全绿，含以下穷举：
  - **坐标纯函数**：`parseMavenGav('com/example/lib/1.0/lib-1.0.jar')` → `{ com.example, lib, 1.0 }`；段数<4 / 目录级 metadata → null。
  - **8 种坐标片段**（Maven 主构件）：逐项断言关键内容——Maven `<dependency>`、Gradle Groovy `implementation 'g:a:v'`、Gradle Kotlin `implementation("g:a:v")`、SBT `"g" % "a" % "v"`、Ivy `<dependency org="g" name="a" rev="v"/>`、Grape `@Grab(...)`、Leiningen `[g/a "v"]`、PURL `pkg:maven/g/a@v`。
  - **非 Maven**：npm / docker / raw 制品 → 坐标片段为空数组（不渲染坐标下拉）。
  - **HTML View URL**：`htmlViewUrl('files', 'dir/a.txt')` → `/files/dir/`；根目录文件 → `/files/`。
  - **下载 URL**：`downloadUrl('files', 'dir/a.txt')` → `/files/dir/a.txt`（逐段编码）。
  - **组件**：文件树渲染并可展开子目录；点文件 → 右侧详情面板加载（mock `getArtifactDetail`）并出现坐标下拉与 HTML View 外链（`href` 指向正确）；坐标下拉切换 → 片段内容随之变化（至少 Maven 全套）；**折叠父目录再重展开，深层子层展开态保留**（展开 com→example→lib→1.0→折叠 com→重展 com，子层一次性恢复）。
  - **不泄露**：沿用 FR-76 既有测试——非管理员 / 无权场景下端点返回受控，前端不额外暴露（既有 `RepositoryDetailPage.test.tsx` 鉴权门控不破）。
- `pnpm -C frontend lint` 过、`pnpm -C frontend build` 过（build 后 `git checkout -- frontend/dist/.gitkeep`）。
- 无 `.rs` 改动 → 不跑 cargo。
- **实机维度**：浏览器实操左树 + 右详情、坐标切换、HTML View 外链跳转——worktree 内不长跑服务，**标「待真机验」**，以 Vitest 组件测试与 build / lint 覆盖渲染与链接生成。

## 6. 风险 / 待定
- HTML View 外链依赖 FR-75 后端按 `Accept: text/html` 渲染目录索引：浏览器默认 Accept 以 `text/html` 起头，命中 HTML 分支；仅对 browsable 格式（raw / maven 等非原生协议）生效，npm/docker 等原生格式目录索引行为由其自身分支决定——HTML View 仅对有目录索引语义的制品有意义，不强行对所有格式渲染。
- 坐标反解纯前端启发式（与后端 `Gav::from_path` 同规则），对非标准布局的 Maven 路径可能解析为空 → 不产坐标（合理留空），不误导。
