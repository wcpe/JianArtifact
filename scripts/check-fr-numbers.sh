#!/usr/bin/env bash
# 守护「FR 编号不得出现在代码里」这条规则（见 .claude/rules/comments.md §5）。
#
# 为什么需要脚本而不是只写在规范里：FR 编号进入代码后，两条分支各自在同一处注释写
# 不同编号时，Git 会判为冲突——即使代码逻辑完全相同。规则文本没法阻止这种事，
# 只有门禁能。
#
# 存量处理：历史存量**已全部清零**，基线文件当前为空，因此实际是「零容忍」——
# 任何一处新增都会失败。基线机制仍保留：万一将来需要临时容忍某些历史引用
# （例如从主线合并进来一段尚未清理的代码），可用 --update 生成快照再逐步收敛，
# 而不必让门禁整体失效。
#
# 用法：
#   scripts/check-fr-numbers.sh            # 门禁模式：有新增即失败
#   scripts/check-fr-numbers.sh --update   # 重新生成基线（清理存量后执行）
set -euo pipefail

cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

BASELINE="scripts/fr-number-baseline.txt"
# 代码范围：只扫源码与脚本；docs/ 与 CHANGELOG.md 明确允许出现 FR 编号。
PATTERN='FR-[0-9]+'
GLOBS=(
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
# 扫描范围包含 api/：openapi.yaml 是契约真源，也曾残留需求编号；先前漏掉它，
# 导致生成物被清理而契约源没清，两边分叉。
SEARCH_DIRS=(apps packages scripts api)

# 统计每个文件里含 FR 编号的行数（不是出现次数——行数对新增更敏感）。
collect() {
  for d in "${SEARCH_DIRS[@]}"; do
    [ -d "$d" ] || continue
    grep -rEc "$PATTERN" "${GLOBS[@]}" "$d" 2>/dev/null || true
  done | awk -F: '$2 > 0 { print $1"\t"$2 }' | sort
}

if [ "${1:-}" = "--update" ]; then
  collect > "$BASELINE"
  echo "已更新基线：$BASELINE（$(wc -l < "$BASELINE") 个文件）"
  exit 0
fi

if [ ! -f "$BASELINE" ]; then
  echo "缺少基线文件 $BASELINE；先执行：scripts/check-fr-numbers.sh --update" >&2
  exit 1
fi

current=$(collect)
violations=0

# 逐文件比较：当前行数 > 基线行数 即为新增，报出来。
# 注意：`collect` 在「全仓库已无编号」时返回空串，`while read` 会拿到一行空值，
# 此时 `count` 不是数字。显式跳过空行，否则 `[ "" -gt 0 ]` 会报
# "integer expression expected"。
while IFS=$'\t' read -r file count; do
  [ -n "${file:-}" ] || continue
  case "$count" in ''|*[!0-9]*) continue ;; esac
  base=$(awk -F'\t' -v f="$file" '$1 == f { print $2 }' "$BASELINE")
  base=${base:-0}
  if [ "$count" -gt "$base" ]; then
    echo "✗ $file：含 FR 编号的行数由 $base 增至 $count" >&2
    grep -nE "$PATTERN" "$file" | head -3 | sed 's/^/    /' >&2
    violations=$((violations + 1))
  fi
done <<< "$current"

if [ "$violations" -gt 0 ]; then
  cat >&2 <<'EOF'

FR 编号不得出现在代码注释、标识符或迁移文件名中，只允许出现在 docs/ 与 CHANGELOG.md。
原因与写法示例见 .claude/rules/comments.md §5：
  - 注释写行为与原因（"搜索索引后台回填：分批推进水位，不阻塞启动"），不写编号；
  - 测试函数名用行为描述（TestSearchIndexBackfillResumesFromWatermark）；
  - 需要指向设计文档时写路径（docs/adr/0030-fts5-search-index.md），不写编号。

若确认是清理存量导致的行数变化，执行 scripts/check-fr-numbers.sh --update 更新基线。
EOF
  exit 1
fi

echo "✓ 未发现新增的 FR 编号（基线 $(wc -l < "$BASELINE") 个文件）"
