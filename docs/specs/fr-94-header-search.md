# 功能规格：制品搜索重构（搜索移至页眉 + 每类型 icon + 树形结果）

> 状态：开发中　·　关联 PRD：FR-94　·　分支：feature/fr-94-header-search

## 1. 背景与目标

跨仓库制品搜索（FR-22/67）当前是一个独立页面，搜索框在页内、结果平铺成表格，关键字命中分散、难以一眼定位到「哪个仓库的哪个坐标」。本 FR 属 Web UX 重构 epic（B2b 第一棒，P2），把搜索体验升级为：搜索入口前移到全站页眉随处可用、结果改为按仓库分组的可展开树并为每个制品按格式渲染专属 icon，提升查找效率与信息密度。纯前端，不改后端搜索语义与权限过滤。

## 2. 需求（要什么）

- 页眉全局搜索：把 FR-92 在 `AppLayout` 页眉留的「禁用占位」搜索框，替换为可用的全局搜索框——用户输入关键字，回车或停止输入（防抖）后跳转到搜索结果页并带上查询参数。
- 搜索结果页树形化：`SearchPage` 不再平铺表格，改为「按仓库分组 → 仓库下按坐标/路径」的可逐级展开树；命中条目仍可点击进入制品详情。
- 每类型专属 icon：仓库分组节点与制品叶子节点按其格式（maven/npm/docker/raw 等）显示专属图标，复用 FR-93 的 `lib/formatIcon`。
- 权限过滤不放松：结果集仍由后端按调用方读权限过滤（FR-22 既有语义），前端只渲染后端返回项，不做任何放宽。
- 范围内：页眉搜索框（AppLayout）、搜索结果页（SearchPage）的搜索交互与结果呈现、搜索结果树构造纯函数。
- 不做（范围外）：不改后端 `/search` 端点与权限过滤；不碰设置 / 监控 / 浏览详情页；不破坏 FR-92 的折叠导航 / 角色门控 / 段精确高亮 / 密度基线；不动 FR-99 的「监控」导航整合；不新增前端依赖。

## 3. 设计（怎么做）

- 复用既有端点 `api.search(q, { format?, offset, limit })`（返回 `Paginated<SearchHit>`），不改契约。
- 页眉↔页面解耦（沿用本项目「深链单段路径 + 查询参数承载状态」约定）：页眉搜索框只负责「跳转到 `/search?q=<keyword>`」（回车立即跳；输入防抖 ~300ms 后自动跳），不持有结果状态；`SearchPage` 经 `useSearchParams` 读取 `q`，`q` 非空即自动发起搜索。这样 URL 可深链、页眉保持薄、两处不直接耦合。
- 结果树构造下沉为纯函数 `lib/searchTree.ts`：把 `SearchHit[]` 折叠为「仓库分组（repo_id/repo_name/format）→ 命中项（path/size 等）」两层结构，仓库按名升序、组内按路径升序；纯函数便于穷举单测。
- 树形渲染：`SearchPage` 用 Mantine 现有组件渲染可展开分组（仓库节点默认展开，点击折叠）；仓库节点前置 `FormatIcon`（按 group.format），叶子制品前置 `FormatIcon`（按 hit.format）。叶子点击 `navigate('/artifact?repo=..&path=..')`，沿用既有详情跳转。
- 格式过滤、分页保留（既有能力，不退化）。
- 复用 `theme/density` token 控制间距，贴合密度基线。

## 4. 任务拆分

- [x] 复制模板 → `docs/specs/fr-94-header-search.md` 写规格
- [x] PRD FR-94 行 计划→开发中
- [x] `lib/searchTree.ts` 纯函数 + 单测（先行）
- [x] `AppLayout` 页眉占位框 → 可用全局搜索（回车/防抖跳 `/search?q=`）+ 单测，且 FR-92 既有用例不回归
- [x] `SearchPage` 读 `q` 自动搜 + 树形结果 + 类型 icon + 单测（空结果 / 无权场景）
- [x] 文档同步：PRD 状态、ARCHITECTURE 前端搜索一句、CHANGELOG 未发布段追加一行

## 5. 验收标准

- 页眉输入关键字回车 → 跳转 `/search?q=<keyword>`（断言导航被调用、参数正确）；防抖输入同样触发跳转。
- `SearchPage` 在 `?q=...` 下自动发起 `api.search`（断言以正确 q 调用），结果按仓库分组成树渲染、每项按格式显示 icon（断言树结构与 icon 存在）。
- 空结果展示空文案；无权 / 不存在仓库的制品不出现在结果中（前端只渲染后端返回项，断言不泄露）。
- `lib/searchTree` 纯函数穷举：单仓库 / 多仓库 / 同仓库多命中 / 空输入 的分组与排序正确。
- FR-92 `AppLayout` 既有用例（折叠 / 角色门控 / 段精确高亮 / aria）全绿不回归。
- 完成判据（出示证据）：`pnpm -C frontend test`（含新用例 + FR-92 既有用例全绿）、`pnpm -C frontend lint` 过、`pnpm -C frontend build` 过。无 .rs 改动则不跑 cargo。

## 6. 风险 / 待定

- 页眉搜索框在窄屏（`hiddenFrom`/`visibleFrom`）下的显隐沿用 FR-92 既有断点策略，不另造布局。
- 防抖触发跳转可能与回车跳转重复，需保证幂等（重复 navigate 到同 URL 无副作用）。
