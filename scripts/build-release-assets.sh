#!/usr/bin/env bash
# 构建可发布资产：前端 embed + 多平台静态二进制 + SHA256SUMS。
# 用法：scripts/build-release-assets.sh <version> [输出目录]
# 例：  scripts/build-release-assets.sh 0.3.0 dist/release
#       scripts/build-release-assets.sh 0.3.0-dev.abc1234 dist/release
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

VERSION="${1:?用法：$0 <version> [输出目录]}"
OUT_DIR="${2:-dist/release}"
mkdir -p "$OUT_DIR"
OUT_DIR="$(cd "$OUT_DIR" && pwd)"

echo "==> 版本：$VERSION"
echo "==> 输出：$OUT_DIR"

echo "==> [1/3] 安装前端依赖并构建（供 Go embed）"
pnpm install --frozen-lockfile
pnpm build

echo "==> [2/3] 同步 web dist 到 apps/server/web/dist（覆盖占位）"
# 保留目录本身，清空除 .gitkeep 外的内容后拷入正式产物
find apps/server/web/dist -mindepth 1 ! -name 'index.html' -exec rm -rf {} + 2>/dev/null || true
# 用 rsync/cp 覆盖：先删再拷，保证无残留旧资源
rm -rf apps/server/web/dist
mkdir -p apps/server/web/dist
cp -a apps/web/dist/. apps/server/web/dist/

echo "==> [3/3] 交叉编译静态二进制"
LDFLAGS="-s -w -X main.version=${VERSION}"
build_one() {
  local goos="$1" goarch="$2"
  local ext=""
  if [ "$goos" = "windows" ]; then
    ext=".exe"
  fi
  local name="jianartifact-${VERSION}-${goos}-${goarch}${ext}"
  echo "  - $name"
  (
    cd apps/server
    CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
      go build -trimpath -ldflags="$LDFLAGS" \
      -o "$OUT_DIR/$name" ./cmd/jianartifact
  )
}

build_one linux amd64
build_one linux arm64
build_one windows amd64
build_one darwin amd64
build_one darwin arm64

echo "==> 生成校验和 SHA256SUMS"
(
  cd "$OUT_DIR"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum jianartifact-* > SHA256SUMS
  else
    shasum -a 256 jianartifact-* > SHA256SUMS
  fi
)

echo "==> 完成。产物："
ls -la "$OUT_DIR"
