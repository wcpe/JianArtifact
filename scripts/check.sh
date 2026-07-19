#!/usr/bin/env bash
# 统一质量门入口：本地与 CI 复用同一脚本，全部在 Docker 中执行以保证宿主零构建产物。
# 当前为 0.1.0 骨架：随工具链就绪逐步接入实际检查步骤。
set -euo pipefail

echo "==> JianArtifact 质量门（骨架）"
echo "TODO(M1/0.1.0)：在容器中依次执行"
echo "  1) 前端：prettier --check、eslint --max-warnings 0、tsc --noEmit、vitest、build"
echo "  2) 后端：gofmt -l、go vet、golangci-lint、go test -race、govulncheck、go build"
echo "  3) 契约：OpenAPI <-> devmock 一致性比对"
echo "  4) 治理：架构依赖方向检查、敏感信息扫描、许可证检查、文档漂移检查"

# 骨架阶段直接通过；接入真实步骤后，任一失败须以非零退出阻断。
exit 0
