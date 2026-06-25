# 功能规格：通用制品上传 API + Web 上传页面

> 状态：开发中　·　关联 PRD：FR-73、FR-74　·　分支：feature/fr-73-upload

## 1. 背景与目标

为 Web 控制台用户提供一个统一的制品上传入口：无需安装 / 配置各格式原生客户端（mvn / npm / curl），
直接在浏览器选仓库、填坐标、拖文件即可把制品发布到 hosted 仓库。属 P2。

## 2. 需求（要什么）

### FR-73 通用制品上传 API（后端）

- 端点：`POST /api/v1/repositories/{id}/upload`，`multipart/form-data`。
- 支持格式：Maven、npm、Raw（**仅 hosted 仓库**）。proxy 仓库拒绝。
- 坐标来源（按目标仓库格式区分表单字段）：
  - **Maven**：表单填 `group_id` / `artifact_id` / `version`，坐标路径 = `{group 点转斜杠}/{artifact}/{version}/{上传文件名}`。
  - **npm**：表单填 `name` / `version`，坐标路径 = `{name}/-/{上传文件名}`（**不解包 .tgz**，name/version 由表单提供）。
  - **Raw**：表单填 `path`，坐标即该路径。
- 校验：写权限（复用既有写授权编排，无读 404 / 有读无写 403）、目标仓库类型（proxy 拒绝）、格式受支持（仅上述三种，其余 400）、上传大小上限（超 `limits.max_artifact_size` 返回 413）。
- 覆盖语义沿用各格式既有策略（Maven release 不可覆盖→409 / npm tarball 已发布不可覆盖→409 / Raw 可覆盖）。
- 范围内：上述三格式 hosted 上传。
- 不做（范围外）：proxy 上传、docker/pypi/go/cargo/nuget 等其余格式经本端点上传、.tgz 解包读 package.json（按方案 A 由表单提供 name/version）、断点续传。

### FR-74 Web 上传页面（前端）

- 统一入口页 `/upload`：选仓库（仅列 hosted）→ 按所选仓库格式渲染动态表单（Maven: GAV / npm: name+version / Raw: path）→ 选文件 → 上传 → 进度条 → 结果提示。
- 导航新增"上传"入口。
- 范围内：上述交互。
- 不做（范围外）：批量多文件上传、目录上传、文件浏览器（FR-76 另做）。

## 3. 设计（怎么做）

- 后端新增薄 handler 模块 `src/api/upload_routes.rs`：解析 multipart → 据仓库格式收集对应表单字段并拼仓库内路径 → 复用 `ArtifactService::put_hosted` 落 blob + 写索引。
  - 写授权复用 `repo_access::load_writable_repo(id)`。
  - proxy 拒绝由 `put_hosted` 内置（非 hosted → `InvalidOperation` → 400）。
  - 路径拼装委托各格式纯函数（`MavenFormat::artifact_path`、`NpmFormat::tarball_path`、Raw 直接用表单 path，再经 `Format::parse_path` 归一化拒穿越）。
  - multipart 读取复用 PyPI 同款"逐字段读入内存 + 累计上限 413"范式。
- 前端新增 `UploadPage`、路由、导航项；API 客户端新增 `uploadArtifact`（用 `XMLHttpRequest` 以支持上传进度）。

## 4. 任务拆分

- [x] PRD §4 FR-73 / FR-74 状态 计划→开发中
- [ ] 后端：`MavenFormat::artifact_path` 纯函数 + `upload_routes` handler + 路由注册
- [ ] 后端测试：`tests/upload_api.rs`（Maven/npm/Raw 成功 + 无写 403 + proxy 拒绝 + 413 + 不支持格式 400）
- [ ] 前端：UploadPage + 路由 + 导航 + `uploadArtifact` 客户端 + 组件测试
- [ ] 文档同步：API.md、CHANGELOG 末尾追加

## 5. 验收标准

- `cargo test`（含 `tests/upload_api.rs`）全绿：三格式上传后制品可经既有下载端点取回、字节一致；无写权限上传返回 403；向 proxy 仓库上传返回 400；超上限返回 413；不支持格式返回 400。
- 前端 `pnpm test` + `pnpm run build`（含 tsc）+ `pnpm run lint` 全绿。
- 真机维度（待真机验）：浏览器手动走一遍"选仓库→填表单→拖文件→见进度条→成功提示"——worktree 并行不长跑服务，留作整合后真机验收，需用户确认。

## 6. 风险 / 待定

- 方案 A 不解 .tgz：npm name/version 完全信任表单输入，与表单文件名不一致时仍按表单值定位（与原生 `npm publish` 由客户端给元数据同源，可接受）。
