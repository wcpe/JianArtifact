# 运维手册：JianArtifact

> 部署、升级、备份恢复、回滚、排障的操作指南。运维方式变化时更新。部署交付物见 `deploy/`。

## 1. 部署

默认形态：单静态二进制或单容器，零外部依赖。数据由两部分组成——SQLite 元数据库 + blob 存储目录，均需持久化。

### 1.1 环境变量（清单）

配置经环境变量注入（`deploy/.env.example` 为无真实值模板，`.env` 不入库）：

| 变量                      | 含义                | 示例 / 默认                |
| ------------------------- | ------------------- | -------------------------- |
| `JIAN_LISTEN`             | 监听地址:端口       | `0.0.0.0:8080`             |
| `JIAN_DATA_DIR`           | 数据根目录          | `/var/lib/jianartifact`    |
| `JIAN_BLOB_DIR`           | blob 存储目录       | `${JIAN_DATA_DIR}/blobs`   |
| `JIAN_SQLITE_PATH`        | SQLite 元数据库路径 | `${JIAN_DATA_DIR}/meta.db` |
| `JIAN_JWT_SECRET`         | JWT(HS256) 签名密钥 | （必填，强随机）           |
| `JIAN_BOOTSTRAP_USER`     | 首启管理员用户名    | `admin`                    |
| `JIAN_BOOTSTRAP_PASSWORD` | 首启管理员引导口令  | （首启后应改）             |
| `JIAN_LOG_LEVEL`          | 日志级别            | `INFO`                     |
| `JIAN_TRUSTED_WORKDIR`    | 可信工作目录根      | `${JIAN_DATA_DIR}`         |

> 代理上游凭据用引用名 → 环境变量注入 `Authorization`，不硬编码、不入库、不进日志（见 `SECURITY.md`）。

### 1.2 Docker / Compose 部署（主路径）

1. 复制 `deploy/.env.example` 为 `deploy/.env`，填入密钥与引导凭据。
2. `docker compose -f deploy/docker-compose.yml up -d`。
3. 探活：`curl -fsS http://<host>:8080/readyz`。
4. 首次登录用引导凭据，立即修改管理员口令。

### 1.3 二进制部署（rootless systemd，可选）

- 由 `deploy/deploy.sh` SSH 到目标主机：上传 / 远端构建二进制到 `releases/<版本>`，原子切换 `current` 符号链接，保留 N 份，`/readyz` 探活失败自动回滚。

### 1.4 Helm / Kubernetes（M3 交付）

- `deploy/helm/`：Deployment（`Recreate` 策略）、Service、PVC（RWO 单卷）、ConfigMap、Secret、Ingress；就绪 / 存活探针打 `/readyz` `/healthz`。

## 2. 升级

- **兼容性**：1.0 前小版本可能含存储 / 配置演进；升级前读该版本 CHANGELOG 的迁移说明。
- **策略**：单实例采停机升级（`Recreate`）——SQLite + blob 为单写者，不做滚动多副本（默认无 HA）。
- **步骤**：备份（§3）→ 拉新镜像 / 新二进制 → 启动（自动执行 schema 迁移）→ `/readyz` 探活 → 验证关键流程。

## 3. 数据备份与恢复

- **备份什么**：SQLite 元数据库（`JIAN_SQLITE_PATH` 及其 `-wal`/`-shm`）+ blob 目录（`JIAN_BLOB_DIR`）。二者须一致快照（建议停写或用一致性快照）。
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
