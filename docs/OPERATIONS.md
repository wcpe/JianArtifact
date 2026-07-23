# 运维手册：JianArtifact

> 部署、升级、备份恢复、回滚、排障的操作指南。运维方式变化时更新。部署交付物见 `deploy/`。

## 1. 部署

默认形态：单静态二进制或单容器，零外部依赖。数据由两部分组成——SQLite 元数据库 + blob 存储目录，均需持久化。

### 1.1 环境变量（清单）

配置经环境变量注入（`deploy/.env.example` 为无真实值模板，`.env` 不入库）：

| 变量                    | 含义                              | 示例 / 默认             |
| ----------------------- | --------------------------------- | ----------------------- |
| `JIAN_HTTP_ADDR`        | HTTP 监听地址:端口                | `:8080`                 |
| `JIAN_DATA_DIR`         | 数据根目录（SQLite 与 blob 存放） | `/var/lib/jianartifact` |
| `JIAN_JWT_SECRET`       | JWT(HS256) 签名密钥               | （建议必填，强随机）    |
| `JIAN_UPSTREAM_TIMEOUT` | proxy 回源上游 HTTP 超时（秒）    | `30`                    |

> 派生约定（不单独配置）：SQLite 元数据库固定为 `${JIAN_DATA_DIR}/jianartifact.db`，blob 目录为 `${JIAN_DATA_DIR}/blobs`，二者随进程启动自动创建。`JIAN_JWT_SECRET` 缺省时进程生成随机密钥并持久化到数据目录（附告警），生产务必显式配置。首个管理员不再经环境变量引导，改为经网页自举端点或 CLI `admin reset` 创建（见 §1.2、§1.5）。

> 代理上游凭据用引用名 → 环境变量注入 `Authorization`，不硬编码、不入库、不进日志（见 `SECURITY.md`）。

### 1.1.1 Nexus 迁移凭据引用（0.4.0）

迁移任务 **只存凭据引用名**（如 `NEXUS_BASIC`），运行时从**同名环境变量**读取实际用户名/口令或 Token：

| 约定 | 说明 |
| ---- | ---- |
| 请求字段 | `credentialRef`：环境变量名，可出现在 API/DB |
| 环境变量值 | 明文密钥（Basic 的 `user:pass` 或 Token），**永不入库、不进日志、不进报告** |
| 缺失引用 | 创建/discover 时若引用名在环境中不存在或为空 → `400 validation_error` |
| 离线源 | `offline_dir` / `offline_bundle` 通常无需 `credentialRef` |

进程启动时会将残留 `running` 迁移任务标为 `failed`（提示 resume），**不会自动续跑**（见 ADR-0012）。

### 1.1.2 自有离线包布局（offline_bundle）

```
bundle/
  manifest.json   # { "repositories": [ { "name", "format", "type" } ] }
  content/
    <repo>/<path...>   # 原始制品文件
```

- `format` 支持 `raw` / `maven` / `maven2` / `npm`；其它进 warnings，不纳入可执行计划。
- 无 `manifest.json` 时扫描 `content/*` 一级目录为仓库名，format 默认 `raw` 并 warning。

### 1.1.3 离线 Nexus 目录夹具（offline_dir）

验收/简化布局：

```
<data>/
  repositories/
    <repo-name>/
      .format          # raw|maven2|npm|…
      content/         # 制品树
```

真实 3.70 blob store 布局可后续用录制样例替换。

### 1.1.4 真机小范围 / 多选仓库迁移（推荐）

全量迁移可能占满磁盘。支持 **多选** 仓库（在线 REST 与离线包/目录均适用）：

1. **发现前预过滤**（可选）：`sourceConfig.includeRepositories: ["repo-a","repo-b"]`  
2. **启动时多选**（推荐）：`POST /api/v1/migrations/{id}/start` body：
   ```json
   { "includeRepositories": ["repo-a", "repo-b"] }
   ```
3. 管理端向导：配置步 TagsInput 预过滤；预览步 **Checkbox 多选** 后点「开始迁移」。

```json
{
  "sourceType": "online_rest",
  "sourceConfig": {
    "url": "http://nexus:8081",
    "includeRepositories": ["raw-hosted-small"]
  },
  "credentialRef": "NEXUS_BASIC",
  "conflictPolicy": "skip"
}
```

- 复测前可在管理端 **删除** 目标仓库（`DELETE /api/v1/repositories/{name}`）：`asset` 表有 `ON DELETE CASCADE`，元数据会一并清除；blob 文件暂不 GC。

### 1.1.4b 远程 SSH 部署（真机验收）

```bash
bash deploy/remote-ssh.sh setup-key   # 生成 deploy/ssh/jianartifact_ed25519（不入库）
# 将公钥写入目标机 authorized_keys
cp deploy/.env.example deploy/.env    # 填 DEPLOY_HOST / DEPLOY_PORT / JIAN_JWT_SECRET
bash deploy/remote-ssh.sh deploy
bash deploy/remote-ssh.sh health
```

### 1.1.5 迁移切换（cutover）检查清单

1. 将 CI / 客户端 registry 指向本 JianArtifact 实例  
2. 将源 Nexus 置为只读（或断开写入）  
3. 调用 `POST /api/v1/migrations/{id}/finalize` 做最终增量  
4. 抽样校验关键路径可下载且校验和一致  
5. 确认备份覆盖 SQLite 与 blob 目录  

报告 `GET .../report` 的 `cutover.checklist` 与此对齐；`cutover.delta` 记录 finalize 增量计数。

### 1.2 Docker / Compose 部署（主路径）

1. 复制 `deploy/.env.example` 为 `deploy/.env`，填入 `JIAN_JWT_SECRET` 等配置。
2. `docker compose -f deploy/docker-compose.yml up -d`。
3. 探活：`curl -fsS http://<host>:8080/readyz`。
4. 首次初始化：经网页自举端点创建首个管理员（`POST /api/v1/auth/bootstrap`，仅未初始化时开放），或用 CLI `jianartifact admin reset` 创建（见 §1.5）。

### 1.5 CLI 运维子命令

单二进制内置以下子命令。**须显式传子命令**：无参数或未知命令只打印用法（不再默认启动服务），以避免误触发。`help` / `-h` / `--help` 打印用法。

- `jianartifact run`（别名 `serve`）：启动 HTTP 服务，监听 `JIAN_HTTP_ADDR`（默认 `:8080`），启动即执行 schema 迁移。容器 / systemd 均须显式传 `run`（见 `deploy/`）。
- `jianartifact admin reset [--username <名>] [--password <口令>]`：离线直连 SQLite 重置 / 创建管理员账号与口令，用于账号锁死后的恢复。该用户名不存在则以 `admin` 角色创建，已存在则重置口令并确保角色 `admin`、状态 `active`。省略 `--password` 时在交互式终端安全录入（两次校验、不回显）；非交互环境须显式传 `--password`。示例：`jianartifact admin reset --username admin`。
- `jianartifact status`：服务在跑时在线探测本地 `/api/v1/status` 打印版本 / 就绪 / 已初始化 / 迁移版本 / 用户数；服务未跑时回退输出版本与解析后的配置（data 目录、DB 路径、监听地址、离线读取的迁移版本与用户数）。
- `jianartifact healthcheck`：容器探活用，读 `/readyz` 决定退出码。

### 1.3 二进制部署（rootless systemd，可选）

- 由 `deploy/deploy.sh` SSH 到目标主机：上传 / 远端构建二进制到 `releases/<版本>`，原子切换 `current` 符号链接，保留 N 份，`/readyz` 探活失败自动回滚。

### 1.4 Helm / Kubernetes（M3 交付）

- `deploy/helm/`：Deployment（`Recreate` 策略）、Service、PVC（RWO 单卷）、ConfigMap、Secret、Ingress；就绪 / 存活探针打 `/readyz` `/healthz`。

## 1.6 CI 质量门与发布

GitHub Actions 工作流（`.github/workflows/`）与本地 `scripts/check.sh` 对齐：

| 触发                   | 工作流        | 行为                                                                                                                                                                                                                              |
| ---------------------- | ------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| PR / 任意分支 push     | `ci.yml`      | 跑统一质量门（前端 + 后端 + 契约）                                                                                                                                                                                                |
| push 到 `main`         | `release.yml` | 质量门通过后发**开发预览版**：固定 git tag / GitHub Release `dev-preview`（prerelease，每次覆盖更新）；版本号 `${VERSION}-dev.<sha7>`；多平台二进制 + `SHA256SUMS`；推送镜像 `ghcr.io/<owner>/<repo>:dev-preview` 与 `:<version>` |
| push `vX.Y.Z` 正式 tag | `release.yml` | 质量门通过后发**正式版** GitHub Release（latest）；校验 tag 与 `VERSION` 一致；多平台二进制；镜像 `:<version>` + `:latest`                                                                                                        |

本地复现发布资产构建：

```bash
bash scripts/build-release-assets.sh 0.3.0 dist/release
# 或预览版号
bash scripts/build-release-assets.sh 0.3.0-dev.abc1234 dist/release
```

> 首次使用 GHCR 时，仓库 Settings → Actions → General 确保允许 GITHUB_TOKEN 写 packages；镜像默认私有时可在 Packages 设置可见性。

## 2. 升级

- **兼容性**：1.0 前小版本可能含存储 / 配置演进；升级前读该版本 CHANGELOG 的迁移说明。
- **策略**：单实例采停机升级（`Recreate`）——SQLite + blob 为单写者，不做滚动多副本（默认无 HA）。
- **步骤**：备份（§3）→ 拉新镜像 / 新二进制 → 启动（自动执行 schema 迁移）→ `/readyz` 探活 → 验证关键流程。

## 3. 数据备份与恢复

- **备份什么**：SQLite 元数据库（`${JIAN_DATA_DIR}/jianartifact.db` 及其 `-wal`/`-shm`）+ blob 目录（`${JIAN_DATA_DIR}/blobs`）。二者须一致快照（建议停写或用一致性快照）。
- **频率**：按数据重要性定期（如每日）；保留策略按容量与合规定。
- **恢复步骤**：停实例 → 还原 SQLite 与 blob 到同一时间点 → 启动 → `/readyz` → 抽样校验制品可拉取。

### 恢复演练

- 定期在非生产环境用真实备份恢复并抽样校验制品与元数据一致，记录演练结论；发现备份不可用当缺陷处理。

## 4. 回滚

- **代码 / 镜像**：切回上一个已知良好版本（二进制部署经 `current` 符号链接回滚 / `make rollback`；Compose 切回上一镜像 tag）。
- **数据边界**：若新版本执行了不可逆 schema 迁移，回滚代码需同时还原对应备份；能否只回代码不回数据视该版本迁移说明。破坏性迁移必须在 CHANGELOG 标注。

## 5. 排障

- **启动失败**：查日志（级别 `JIAN_LOG_LEVEL`）；确认 `JIAN_DATA_DIR` / blob 目录可写、SQLite 路径可访问、`JIAN_JWT_SECRET` 已设。
- **`/readyz` 不就绪**：SQLite 打不开或 blob 目录不可写；查磁盘空间与权限。
- **proxy 拉取失败**：查上游可达性与凭据；上游超时 / 5xx 时按重试与降级策略处理（M3 断路器）。
- **迁移卡住**：查迁移任务状态与报告；用断点续传恢复；确认 Nexus 来源可达与凭据有效。
- **关键位置**：应用日志（分级中文）、`/healthz` `/readyz`、（M3 起）Prometheus 指标与审计日志。
