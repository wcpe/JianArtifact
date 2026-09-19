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

echo "==> [1.2/3] 慢用例观测（仅告警、不阻断）：放宽过等待上限的负载敏感用例在此留痕"
node scripts/watch-slow-tests.mjs

echo "==> [1.5/3] 同步前端产物到后端 embed 目录（apps/server/web/dist；//go:embed all:dist 依赖非空目录）"
rm -rf apps/server/web/dist
mkdir -p apps/server/web/dist
cp -a apps/web/dist/. apps/server/web/dist/

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
  # 覆盖率只产出、不做阈值门禁：存量代码覆盖水平未知，一上来设阈值会卡死质量门。
  # 数据落在 .tmp/ 供本地查看与后续接入（CI 里可上传或做趋势比较）。
  mkdir -p ../../.tmp
  CGO_ENABLED=1 go test -race -count=1 -coverprofile=../../.tmp/go-coverage.out ./...
  go tool cover -func=../../.tmp/go-coverage.out | tail -1
  govulncheck ./...
  CGO_ENABLED=0 go build -trimpath -o bin/jianartifact ./cmd/jianartifact
)

echo "==> [3/3] 契约一致性：已由 devmock 契约测试覆盖（见前端 test 步骤）"
echo "==> 质量门全绿。"
