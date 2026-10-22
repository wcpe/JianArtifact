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
# 唯一的例外是**已发布迁移文件**（见下方「已发布迁移豁免」）：按「已发布迁移不可修改」
# 的约定，0001–0048 这些迁移的文件头注释保留着引入时的原始编号，改动它们反而违反约定。
# 豁免按版本号精确判定，新增迁移（0049 及以后）不享受豁免。
#
# 用法：bash scripts/check-fr-numbers.sh
set -euo pipefail

cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# 扫描范围：源码、脚本、契约真源。
# docs/ 与 CHANGELOG.md 是编号的正当归属，不在此列。
SCAN_ROOTS=(apps packages scripts api)

# 参与扫描的扩展名。第三方依赖与构建产物里的同名文件也在这一组扩展名内，
# 因此必须先把它们的目录剔掉（见 list_candidates）。
EXT_RE='\.(go|ts|tsx|js|mjs|sql|css|json|yaml|yml)$'

# 非 git 工作树（源码包 / 容器内解包）时的剪枝目录：依赖、构建产物、缓存。
PRUNE_DIRS=(.git node_modules dist .turbo build coverage .next)

# 编号形态：FR-123、FR-40 追加七、FR-149、FR-25c 等。
PATTERN='FR-[0-9]+'

# 已发布迁移的豁免基线：版本号 <= 本值的迁移文件不参与扫描。
#
# 为什么可以豁免：这些文件已经进入过发布包、按约定**不可再修改**，其文件头注释里的
# 编号是引入时的历史原文。要求它们去掉编号，等于要求改写已发布迁移，代价（用户库中
# 已应用的迁移与实际文件不一致）远大于「几行历史注释里留着编号」。
# 为什么只豁免到基线：新增迁移是本次改动的一部分，必须遵守规则——所以基线是一个
# 硬编码的版本号，而不是「目录下所有文件都豁免」。下次发布后若要提升基线，改这一个数。
PUBLISHED_MIGRATION_BASELINE=48
MIGRATIONS_DIR='apps/server/internal/persistence/migrations'

# 只扫存在的根目录：缺目录时 find 会往 stderr 打一行「No such file or directory」，
# 看起来像门禁自身出错（原脚本用 [ -d ] 跳过的就是这件事）。
present_roots() {
  local root
  for root in "${SCAN_ROOTS[@]}"; do
    [ -d "$root" ] && printf '%s\n' "$root"
  done
}

# list_candidates：输出待扫描文件清单（NUL 分隔）。
#
# 优先用 git ls-files 限定为**受版本控制的文件**：它天然排除 node_modules、dist、
# .turbo 等未跟踪目录，与「门禁只约束要入库的代码」这一口径一致。CI 里本脚本跑在
# pnpm install 之前，而本地跑 check.sh 时依赖早已装好——若直接 grep 目录，会把数十万
# 第三方文件扫进来，第三方代码里偶然出现的「FR-」+ 数字就会让零容忍门禁恒红，
# 且与本次改动毫无关系。
#
# 不在 git 工作树内（源码包、容器内解包）时退回 find，手工剪掉同样的目录。
list_candidates() {
  local roots=()
  while IFS= read -r root; do
    [ -n "$root" ] && roots+=("$root")
  done < <(present_roots)
  [ ${#roots[@]} -gt 0 ] || return 0

  if command -v git >/dev/null 2>&1 && git ls-files --error-unmatch -- . >/dev/null 2>&1; then
    git ls-files -z -- "${roots[@]}"
    return
  fi
  local prune=()
  local dir
  for dir in "${PRUNE_DIRS[@]}"; do
    prune+=(-name "$dir" -o)
  done
  find "${roots[@]}" -type d \( "${prune[@]}" -false \) -prune -o -type f -print0
}

# is_exempt_published_migration：判断单个路径是否属于「已发布迁移」豁免。
is_exempt_published_migration() {
  local path=$1 base ver
  case "$path" in
    "$MIGRATIONS_DIR"/*) ;;
    *) return 1 ;;
  esac
  base=${path##*/}
  ver=${base%%_*}
  # 只认「纯数字版本号」的迁移文件名（0045_xxx.sql）；其它命名形态不豁免。
  case "$ver" in
    '' | *[!0-9]*) return 1 ;;
  esac
  # 10# 前缀：bash 会把 0045 当成八进制，显式按十进制比较。
  [ $((10#$ver)) -le "$PUBLISHED_MIGRATION_BASELINE" ]
}

files=()
while IFS= read -r -d '' f; do
  [ -n "$f" ] || continue
  [[ $f =~ $EXT_RE ]] || continue
  is_exempt_published_migration "$f" && continue
  files+=("$f")
done < <(list_candidates)

hits=""
if [ ${#files[@]} -gt 0 ]; then
  # -I 跳过二进制；一次批量 grep，避免为每个文件起一个进程。
  hits=$(printf '%s\0' "${files[@]}" | xargs -0 grep -nIE "$PATTERN" 2>/dev/null || true)
fi

if [ -n "${hits//[$'\n' ]/}" ]; then
  echo "✗ 代码中发现需求编号（只允许出现在 docs/ 与 CHANGELOG.md）：" >&2
  printf '%s\n' "$hits" | sed 's/^/    /' >&2
  cat >&2 <<'EOF'

写法要求（.claude/rules/comments.md §5）：
  - 注释写行为与原因：「搜索索引后台回填：分批推进水位，不阻塞启动」，不写编号；
  - 测试函数名用行为描述：TestSearchIndexBackfillResumesFromWatermark；
  - 迁移文件注释用日期或主题描述引入背景；
  - 需要指向设计文档时写路径（docs/adr/xxx.md），不写编号。

注：0001–0048 的已发布迁移文件已豁免（不可修改的历史原文），新增迁移仍要遵守。
EOF
  exit 1
fi

echo "✓ 代码中未发现需求编号（已发布迁移 0001–${PUBLISHED_MIGRATION_BASELINE} 豁免）"
