#!/usr/bin/env bash
# Unix 发布入口：调用仓库统一 Go 发布工具，和 Windows 使用同一目标清单。
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VERSION="${1:-}"
OUT_DIR="${2:-dist/release}"
ARGS=(run ./apps/server/cmd/release-assets --root "$ROOT" --output "$OUT_DIR")
if [[ -n "$VERSION" ]]; then
  ARGS+=(--version "$VERSION")
fi
go "${ARGS[@]}"
