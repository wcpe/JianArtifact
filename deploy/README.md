# 部署编排（deploy/）

本目录提供 JianArtifact 的部署编排。完整流程、环境变量清单与升级/回滚判定见 [`../docs/OPERATIONS.md`](../docs/OPERATIONS.md)；决策背景见 [`../docs/adr/0007-deployment-orchestration.md`](../docs/adr/0007-deployment-orchestration.md)。

> 当前为 0.1.0 骨架：编排可演练，服务能力随 M1 迭代落地。

## 内容

| 文件 / 目录 | 用途 |
|---|---|
| `Dockerfile` | 多阶段构建：前端 dist → Go embed 静态编译 → distroless 最小运行镜像（非 root） |
| `docker-compose.yml` | 单机 Compose 部署（命名卷、`env_file`、健康检查、`restart: unless-stopped`） |
| `.env.example` | 环境变量模板；复制为 `.env` 填值。**`.env` 不入库** |
| `deploy.sh` | 远程部署 / 回滚脚本（Compose 主路径 + 可选 rootless systemd 二进制路径） |
| `helm/` | Helm Chart（Deployment/Service/PVC/ConfigMap/Secret/Ingress，单实例 + Recreate） |
| `k8s/` | 等价的手写 K8s 清单（无 Helm 环境直接 `kubectl apply`） |
| `systemd/` | rootless systemd 用户单元模板 |

## 主路径（Docker Compose）

```bash
cp deploy/.env.example deploy/.env    # 填入 JIAN_JWT_SECRET 等真实值
bash deploy/deploy.sh deploy          # 构建镜像并启动，探活 /readyz
```

远程部署：设置 `DEPLOY_HOST=user@host` 后执行同一命令。

## 可选路径（Helm）

```bash
helm install jianartifact deploy/helm \
  --set image.tag=0.1.0 \
  --set secrets.JIAN_JWT_SECRET=... \
  --set secrets.JIAN_BOOTSTRAP_ADMIN_USER=... \
  --set secrets.JIAN_BOOTSTRAP_ADMIN_PASSWORD=...
```

## 安全约束

- 密钥（`JIAN_JWT_SECRET`、上游凭据、管理员自举口令）经环境变量 / Secret 注入，**严禁硬编码、严禁入库**。
- 部署脚本失败以非零退出并给中文错误；重复执行幂等；凭据不进日志。
- 详见 [`../SECURITY.md`](../SECURITY.md)。
