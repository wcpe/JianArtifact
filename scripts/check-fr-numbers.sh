#!/usr/bin/env bash
# 门禁：需求编号（FR-xxx）不得出现在代码里。
#
# 规则见 .claude/rules/comments.md §5。需求编号只允许出现在 docs/ 与 CHANGELOG.md。
#
# 为什么需要门禁而不是只写规范：编号进入代码后，两条分支各自在同一处注释写不同
# 编号时，Git 会判为冲突——即使代码逻辑完全相同。规则文本拦不住这种事。
#
# 零容忍：不做基线、不设豁免。发现一处即失败。
#
# 用法：bash scripts/check-fr-numbers.sh
set -euo pipefail

cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# 扫描范围：源码、脚本、契约真源。
# docs/ 与 CHANGELOG.md 是编号的正当归属，不在此列。
SCAN_DIRS=(apps packages scripts api)
INCLUDES=(
  --include=*.go
  --include=*.ts
  --include=*.tsx
  --include=*.js
  --include=*.mjs
  --include=*.sql
  --include=*.css
  --include=*.json
  --include=*.yaml
  --include=*.yml
)

# 编号形态：FR-123、FR-40 追加七、FR-149、FR-25c 等。
PATTERN='FR-[0-9]+'

hits=""
for dir in "${SCAN_DIRS[@]}"; do
  [ -d "$dir" ] || continue
  found=$(grep -rnE "$PATTERN" "${INCLUDES[@]}" "$dir" 2>/dev/null || true)
  [ -n "$found" ] && hits="${hits}${found}"$'\n'
done

if [ -n "${hits//[$'\n' ]/}" ]; then
  echo "✗ 代码中发现需求编号（只允许出现在 docs/ 与 CHANGELOG.md）：" >&2
  printf '%s' "$hits" | sed 's/^/    /' >&2
  cat >&2 <<'EOF'

写法要求（.claude/rules/comments.md §5）：
  - 注释写行为与原因：「搜索索引后台回填：分批推进水位，不阻塞启动」，不写编号；
  - 测试函数名用行为描述：TestSearchIndexBackfillResumesFromWatermark；
  - 迁移文件注释用日期或主题描述引入背景；
  - 需要指向设计文档时写路径（docs/adr/xxx.md），不写编号。
EOF
  exit 1
fi

echo "✓ 代码中未发现需求编号"
