#!/usr/bin/env bash
# 统一质量门入口：本地与 CI 复用同一脚本。
# 依赖工具链：node/pnpm、go、golangci-lint、govulncheck、gcc（-race 需 CGO）。
# Go 模块 / 构建缓存与代理由调用方环境提供（见 README 环境说明）；本脚本不篡改缓存位置。
# CI 中可将本脚本置于带完整工具链的构建容器内执行，以保证宿主零构建产物。
#
# 为什么不带参数即可跑全量：本地只需要一条命令，不应逼人记住分片细节。
# CI 用 JIAN_GATE_SCOPE 选择单个阶段并行跑（见 .github/workflows/ci.yml）：
#   build-dist   仅安装依赖并产出前端产物（apps/web/dist → apps/server/web/dist）
#   frontend     前端格式 / 静态 / 类型 / 测试（不含构建）
#   go-static    Go 格式 / 静态检查 / 漏洞扫描 / 编译
#   go-test-1    Go 测试分片一
#   go-test-2    Go 测试分片二
# 任一分片失败即整体失败；本地默认 all 会把它们顺序跑一遍。
set -euo pipefail

# 切到仓库根（脚本位于 scripts/ 下）。
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

SCOPE="${JIAN_GATE_SCOPE:-all}"

# 分片清单：按**实测耗时**配平，不是按包数量平均分。
#
# 本机（4 核口径，-race）实测：api 276s、repository 352s、domain 108s、migration/runner 105s、
# cmd 78s、protocol 50s、httpserver 39s、persistence 34s，其余各包合计约 25s。总量约 1067s。
# 三片各约 355s 时最平衡：repository 单独成片（它是最大单包，和谁都放一起都会把该片拖长），
# api+domain 一片，其余一片。
#
# 新增慢包时要重算这三片，否则某一片会明显更慢，并行收益被它吃掉。
#
# 分片用显式包列表而不是 ./...，因此**没有测试文件的包也必须列出**：它们今天不产生测试，
# 但一旦有人补上，漏列就会让测试静默不跑。三片的并集必须等于 go list ./... 的全部输出
# （internal、internal/licenses、web 目前无测试，仍列在此处以保持这条不变量）。
GO_TEST_SHARD_1=(
  ./internal/api/...
  ./internal/domain/...
)
GO_TEST_SHARD_2=(
  # repository 是最大单包（约 352s），单独成片才能与另外两片配平。
  ./internal/repository/...
)
GO_TEST_SHARD_3=(
  ./internal/migration/...
  ./internal/httpserver/...
  ./cmd/...
  ./internal/protocol/...
  ./internal/persistence/...
  ./internal/config/...
  ./internal/formats/...
  ./internal/auditctx/...
  ./internal/auth/...
  ./internal/blobstore/...
  ./internal/metrics/...
  ./internal/scheduler/...
  ./internal/upstream/...
  ./internal/archive/...
  ./internal/nugetpackage/...
  ./internal
  ./internal/licenses
  ./web
)

# 分片清单的单一来源：case 分支按下标取用，避免新增分片时漏改某处。
GO_TEST_SHARDS=("${GO_TEST_SHARD_1[*]}" "${GO_TEST_SHARD_2[*]}" "${GO_TEST_SHARD_3[*]}")
GO_TEST_SHARD_COUNT=3

sync_dist() {
  # 后端 //go:embed all:dist：dist 目录不入库（见 .gitignore），没有产物时 go build 会直接
  # 报 "pattern all:dist: no matching files found"。因此任何 Go 步骤都必须在产物就绪之后。
  #
  # 两种就绪来源，按顺序判断：
  #   1. 本机刚构建出 apps/web/dist（build-dist 或 all 分支的前一步）；
  #   2. CI 的后端 job 通过 artifact 直接把产物下载到 apps/server/web/dist —— 那里没有
  #      apps/web/dist，若仍无条件 rm -rf 再复制，就会把已下载的产物删掉并因 cp 失败而中断
  #      （拆分 CI 时踩过：四个后端 job 全部在 15 秒内失败）。
  if [ -d apps/web/dist ] && [ -n "$(ls -A apps/web/dist 2>/dev/null)" ]; then
    echo "==> 同步前端产物到后端 embed 目录（apps/server/web/dist）"
    rm -rf apps/server/web/dist
    mkdir -p apps/server/web/dist
    cp -a apps/web/dist/. apps/server/web/dist/
    return
  fi
  if [ -d apps/server/web/dist ] && [ -n "$(ls -A apps/server/web/dist 2>/dev/null)" ]; then
    # 校验解包结果：artifact 若被套成了 dist/dist/，embed 会打进错误层级，
    # 表现为「构建成功但页面 404」。这里提前失败并说清原因。
    if [ ! -f apps/server/web/dist/index.html ] && [ -f apps/server/web/dist/dist/index.html ]; then
      echo "后端 embed 产物层级异常（apps/server/web/dist/dist/），正在上提一层" >&2
      mv apps/server/web/dist/dist/* apps/server/web/dist/ 2>/dev/null || true
    fi
    echo "==> 复用已有的后端 embed 产物（apps/server/web/dist 已就绪，无需从 apps/web/dist 同步）"
    return
  fi
  echo "缺少前端产物：apps/web/dist 与 apps/server/web/dist 均为空。" >&2
  echo "本地请先跑 JIAN_GATE_SCOPE=build-dist（或直接跑不带参数的全量）；CI 中该 job 需下载 web-dist artifact。" >&2
  exit 1
}

run_fr_numbers() {
  echo "==> [0] 规范门禁：需求编号不得进入代码（见 .claude/rules/comments.md §5）"
  # 放在最前：只读文件、耗时极短，让违规在跑完几分钟测试之前就暴露。
  bash scripts/check-fr-numbers.sh
}

run_frontend() {
  echo "==> [1] 前端质量门（格式 / 静态 / 类型 / 测试）"
  pnpm format:check
  pnpm lint
  pnpm typecheck
  pnpm test # 含 devmock ↔ OpenAPI 契约一致性（对齐 AC-06）
  # 构建验证单独串行跑：它 spawn 一次真实 vite build（60–120s），与单元测试并行会互相拖慢
  # 且自身易超时；放在单元测试之后、CPU 空闲时执行（实测 20–45s）。
  pnpm --filter @jianartifact/web exec vitest run --config vitest.build.config.ts --reporter=json --outputFile="$PWD/.tmp/build-isolation.json"
}

run_build_dist() {
  echo "==> [1.5] 构建前端产物（供后端 embed 与前端构建验证）"
  pnpm build
}

run_slow_tests() {
  echo "==> [1.2] 慢用例观测（仅告警、不阻断）：放宽过等待上限的负载敏感用例在此留痕"
  node scripts/watch-slow-tests.mjs
}

run_go_static() {
  echo "==> [2a] 后端质量门：格式 / 静态 / 编译"
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
    CGO_ENABLED=0 go build -trimpath -o bin/jianartifact ./cmd/jianartifact
  )
}

# run_go_vuln 单独跑漏洞扫描。
#
# 为什么从 run_go_static 里拆出来：本地的 all 分支要保持「先跑完测试再报漏洞」的原有顺序。
# 漏洞扫描依赖被检查方本机安装的 Go 工具链版本（govulncheck 按构建它的工具链判定标准库
# 漏洞），在 Go 版本落后于 go.mod 声明 toolchain 的开发机上会稳定报出标准库漏洞——若把它
# 排在测试之前，本地一次全量会直接卡在这里、后面的测试根本跑不到。CI 里各 job 独立，
# 不受影响。
run_go_vuln() {
  echo "==> [2c] 后端漏洞扫描"
  (cd apps/server && govulncheck ./...)
}

# run_go_test 跑一个测试分片。
#
# -parallel 4：与 CI（4 核 runner）口径一致；同时避免在核多的开发机上并行度过高——
# -race 的 ThreadSanitizer 会为并发 goroutine 预留大量影子内存，实测 32 核默认并行 32 时
# 在 Windows 上直接分配失败（ThreadSanitizer failed to allocate …，error code 1455）。
#
# 覆盖率只产出、不做阈值门禁：存量代码覆盖水平未知，一上来设阈值会卡死质量门。
# 分片并行跑时各片写自己的文件，避免相互覆盖。
run_go_test() {
  local name="$1"
  shift
  echo "==> [2b] 后端测试分片 $name（${*:-全部}）"
  (
    cd apps/server
    mkdir -p ../../.tmp
    CGO_ENABLED=1 go test -race -count=1 -parallel 4 \
      -coverprofile="../../.tmp/go-coverage-${name}.out" "$@"
    go tool cover -func="../../.tmp/go-coverage-${name}.out" | tail -1
  )
}

case "$SCOPE" in
  build-dist)
    pnpm install --frozen-lockfile
    run_build_dist
    sync_dist
    ;;
  frontend)
    pnpm install --frozen-lockfile
    run_frontend
    ;;
  go-static)
    sync_dist
    run_go_static
    run_go_vuln
    ;;
  go-test-*)
    # 分片编号从 1 起；越界时明确报错，避免"看起来跑了其实什么都没跑"。
    shard="${SCOPE#go-test-}"
    if ! [[ "$shard" =~ ^[0-9]+$ ]] || [ "$shard" -lt 1 ] || [ "$shard" -gt "$GO_TEST_SHARD_COUNT" ]; then
      echo "未知的测试分片 '$SCOPE'；当前共 $GO_TEST_SHARD_COUNT 片（go-test-1..$GO_TEST_SHARD_COUNT）" >&2
      exit 2
    fi
    sync_dist
    # shellcheck disable=SC2086  # 分片清单按空格拆分后逐个传给 go test
    run_go_test "$shard" ${GO_TEST_SHARDS[$((shard - 1))]}
    ;;
  all)
    # 本地顺序跑：与 CI 各 job 的结果等价，只是不并行。
    pnpm install --frozen-lockfile
    run_fr_numbers
    run_frontend
    run_slow_tests
    run_build_dist
    sync_dist
    run_go_static
    for i in $(seq 1 "$GO_TEST_SHARD_COUNT"); do
      # shellcheck disable=SC2086  # 分片清单按空格拆分后逐个传给 go test
      run_go_test "$i" ${GO_TEST_SHARDS[$((i - 1))]}
    done
    # 漏洞扫描放在测试之后：它依赖本机 Go 工具链版本，落后于 go.mod 的 toolchain 时会稳定
    # 报标准库漏洞。若排在前面，本地一次全量会直接卡在这里、测试根本跑不到。
    run_go_vuln
    echo "==> [3] 契约一致性：已由 devmock 契约测试覆盖（见前端 test 步骤）"
    echo "==> 质量门全绿。"
    ;;
  *)
    echo "未知的 JIAN_GATE_SCOPE='$SCOPE'；可选：build-dist|frontend|go-static|go-test-1..$GO_TEST_SHARD_COUNT|all" >&2
    exit 2
    ;;
esac
