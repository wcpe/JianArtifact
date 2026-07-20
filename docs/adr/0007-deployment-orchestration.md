# ADR-0007：部署编排以 Docker/Compose 为主，Helm/K8s 与 rootless systemd 为可选路径

## 状态

已接受

## 背景

产品需可远程部署、可升级、可回滚，且覆盖从单机到 Kubernetes 的不同运维场景。需确定默认部署路径与可选路径，并明确容器化构建、健康探活、原子切换与回滚约定。

## 决策

- **主路径**：多阶段 **Dockerfile**（前端 dist → Go embed 编译 → `distroless/static` 最小运行镜像，非 root，`HEALTHCHECK` 打 `/readyz`）+ **docker-compose.yml**（命名卷挂元数据与 blob、`env_file: .env`、`restart: unless-stopped`）。
- **远程部署脚本** `deploy/deploy.sh` + `make deploy`：SSH 到目标主机，支持 Compose 部署与可选 rootless systemd 二进制部署（`current` 符号链接原子切换、保留 N 份、健康失败自动回滚）。
- **可选路径**：`deploy/helm/` Helm Chart + K8s 清单（单实例 RWO 卷 + `Recreate` 策略，探针打 `/readyz` `/healthz`），M3 交付。

## 理由

- Docker/Compose 覆盖绝大多数自托管场景，配合单二进制镜像（ADR-0005）部署与升级极简。
- 部署脚本读取不入库 `.env`、失败非零退出给中文错误、重复执行幂等、凭据不进日志不进仓库，沿用旧项目久经验证的回滚 / 幂等约束。
- Helm/K8s 满足集群化运维；单实例 RWO + `Recreate` 与 SQLite 单写者模型（ADR-0002）一致，不假装高可用。

## 后果

- 正面：一套编排覆盖单机到集群；探活 / 原子切换 / 自动回滚使升级安全可逆。
- 负面 / 约束：K8s 路径受限于单实例（无 HA，与 NORA 取舍一致），HA 为后续独立 ADR；`.env` 与 Secret 严禁入库，密钥经环境变量 / Secret 注入（见 SECURITY.md）。

## 备选方案

- **仅提供二进制不做容器**：部署一致性差、依赖宿主环境，落选为可选 systemd 路径之一。
- **默认上 Kubernetes**：对单机自托管用户过重，降级为可选路径。
- **裸机手工部署文档**：不可复现、易错，由脚本化部署取代。
