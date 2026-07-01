# 功能规格：仓库列表增强

> 状态：开发中　·　关联 PRD：FR-135　·　分支：feature/fr-135-repo-list-enhance

## 1. 背景与目标

P2 功能，增强 FR-31（审计日志）与 FR-76（文件浏览器）。

仓库列表当前仅展示名称、格式、类型、可见性、upstream URL 五列，缺少统计信息与连通性验证能力。
本功能为仓库列表增加以下内容（仅 Admin 操作）：

① proxy 仓库「测试连通性」按钮：经当前出站客户端访问其 upstream URL，复用 FR-128 机制；
② 每仓统计：制品数 + 总大小（去重 sha256，与仪表盘 KPI 口径一致）；
③ 状态标识：当前均为 active（P1 阶段无禁用仓库逻辑，预留字段供后续扩展）；
④ proxy 仓库 upstream URL 在列表内明确展示（已有但无 title，本次加强展示）。

## 2. 需求

### 后端

- `meta` 层新增一次性批量聚合查询 `list_repo_stats()`：返回按仓库 id 分组的制品数
  （`COUNT(*)`）与去重字节数（子查询 `GROUP BY sha256 + MAX(size)` 再 `SUM + COALESCE`），
  避免 N+1；空仓返回 0。
- `RepositoryDto` 增加字段：
  - `artifact_count: i64` — 制品条目数（不去重）
  - `total_size: i64` — 去重 sha256 后的总字节数
  - `status: String` — 仓库状态，P1 固定返回 `"active"`（无禁用逻辑）
- `list_repositories` handler：先批量取所有仓库，再批量取统计，合并填入 DTO，避免 N+1。
- 新增端点 `POST /api/v1/repositories/{id}/test-connectivity`（仅 Admin）：
  - 取该仓库的 `upstream_url`，若无（非 proxy 或未配置）返回 400
  - 用 `state.settings.network.client()` 带 10s 超时 GET upstream_url
  - 返回 `{ "ok": bool, "status": u16|null, "elapsed_ms": u64, "error": string|null }`
  - 非 proxy/无 upstream → 400；非 Admin → 403；仓库不存在 → 404
- 路由注册：在 `src/api/mod.rs` 仓库路由组后追加新端点。

### 前端

- `frontend/src/api/types.ts`：`RepositoryDto` 加 `artifact_count`、`total_size`、`status` 字段
- `frontend/src/api/endpoints.ts`：新增 `testRepoConnectivity(id)` 函数
- `frontend/src/pages/RepositoriesPage.tsx`：
  - 列表新增「制品数」「大小」「状态」三列
  - proxy 行在操作列加「测试」按钮（仅 Admin）→ 调端点 → Modal/Alert 展示结果
  - upstream URL 列保持，不截断，加 title 提示完整 URL
- MSW mock：
  - `store.ts` 中 `RepositoryDto` 加统计字段
  - `handlers.ts` 中加 `POST /api/v1/repositories/:id/test-connectivity` handler

## 3. 设计

### 3.1 meta 统计聚合

```sql
-- 一次性按仓库聚合：制品数 + 去重字节
SELECT
  repo_id,
  COUNT(*) AS artifact_count,
  COALESCE(SUM(size_per_sha), 0) AS total_size
FROM (
  SELECT repo_id, sha256, MAX(size) AS size_per_sha
  FROM artifacts
  GROUP BY repo_id, sha256
) GROUP BY repo_id
```

返回 `Vec<RepoStatRow>` (`repo_id`, `artifact_count`, `total_size`)，
调用方用 `HashMap<String, RepoStatRow>` 按 repo_id 查找。

注：`artifact_count` 使用内层 `COUNT(*)` 前的原始行数（不去重），
外层 GROUP BY repo_id, sha256 后内层记录数对应去重 sha256 数，
因此制品数 = 对该仓库的所有制品索引行数。
（实现时用两层查询或单层 COUNT(*) + SUM 区分）

实际 SQL 实现：为简单起见，用两步子查询分别算制品数和去重字节，或用
单 SQL 同时算：内层按 (repo_id, sha256) 去重后外层 COUNT + SUM。

### 3.2 连通性端点

```
POST /api/v1/repositories/{id}/test-connectivity
→ 404  仓库不存在
→ 400  仓库非 proxy 或无 upstream_url
→ { ok: true, status: 200, elapsed_ms: 123 }
→ { ok: false, elapsed_ms: 500, error: "连接超时" }
```

实现参照 `settings::proxy_test`，逻辑完全对称。

### 3.3 前端列表布局

| 列 | 内容 |
|---|---|
| 名称 | 链接，点击跳详情 |
| 格式 | maven/npm/docker/raw/... |
| 类型 | hosted/proxy |
| 可见性 | badge |
| 制品数 | 数字 |
| 大小 | 人类可读格式（formatBytes） |
| 状态 | badge，active=绿色 |
| upstream URL | proxy 行显示，加 title；hosted 显示 - |
| 操作 | 配置按钮 + 删除按钮（Admin）+ 测试按钮（proxy && Admin）|

## 4. 验收标准

- 仓库列表每行显示制品数、总大小（人类可读）、状态（active）
- proxy 仓库显示 upstream URL（完整，加 title 提示）
- Admin 点 proxy 仓库的「测试连通性」按钮返回连通性结果（ok/error）
- 非 Admin 不显示测试按钮；非 proxy 仓库不显示测试按钮
- 后端聚合无 N+1（一次批量取统计）
- 后端测试：统计正确（建仓+传制品后数/字节对，同 sha256 去重）；
  连通性端点 Admin 200 / 非 Admin 403 / 非 proxy 400 / 不存在 404
- 前端 vitest：列表渲染统计字段、upstream URL；测试按钮出现/不出现条件
