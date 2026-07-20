#!/usr/bin/env bash
# 统一质量门入口：本地与 CI 复用同一脚本。
# 依赖工具链：node/pnpm、go、golangci-lint、govulncheck、gcc（-race 需 CGO）。
# Go 模块 / 构建缓存与代理由调用方环境提供（见 README 环境说明）；本脚本不篡改缓存位置。
# CI 中可将本脚本置于带完整工具链的构建容器内执行，以保证宿主零构建产物。
set -euo pipefail

# 切到仓库根（脚本位于 scripts/ 下）。
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

echo "==> [1/3] 前端质量门（格式 / 静态 / 类型 / 测试 / 构建）"
pnpm install --frozen-lockfile
pnpm format:check
pnpm lint
pnpm typecheck
pnpm test # 含 devmock ↔ OpenAPI 契约一致性（对齐 AC-06）
pnpm build # 产出 apps/web/dist 供后端 embed

echo "==> [2/3] 后端质量门（格式 / 静态 / 漏洞 / 测试 / 构建）"
(
  cd apps/server
  fmt_out=$(gofmt -l .)
  if [ -n "$fmt_out" ]; then
    echo "以下 Go 文件未格式化（请运行 gofmt -w）：" >&2
    echo "$fmt_out" >&2
    exit 1
  fi
  go vet ./...
  golangci-lint run
  CGO_ENABLED=1 go test -race -count=1 ./...
  govulncheck ./...
  CGO_ENABLED=0 go build -trimpath -o bin/jianartifact ./cmd/jianartifact
)

echo "==> [3/3] 契约一致性：已由 devmock 契约测试覆盖（见前端 test 步骤）"
echo "==> 质量门全绿。"
