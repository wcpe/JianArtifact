# JianArtifact 本地开发 / 构建命令集（开发便利，非 CI 真源；CI 见 .github/workflows/ci.yml）。
#
# 约定：
# - 后端 cargo 一律经 rustup run 1.96.0 跑（匹配 CI 浮动 @stable=1.96，本地默认 1.94.1 编不过）。
# - 构建顺序硬约束：必须先建前端（产出 frontend/dist）再编后端——rust-embed 编译期读 dist，
#   否则后端嵌入的是占位空集、与发布产物不一致。
# - 前端经 pnpm（lockfileVersion 9 / pnpm 9）。
#
# 可覆盖变量：make CARGO="cargo" test  （如本机默认工具链已达标）

CARGO ?= rustup run 1.96.0 cargo
PNPM  ?= pnpm -C frontend

.DEFAULT_GOAL := help

.PHONY: help install web-dev run build build-web release \
        test test-rs test-web fmt fmt-check lint check audit clean

help: ## 列出所有可用命令
	@echo "JianArtifact 开发命令："
	@echo "  make install     安装前端依赖（pnpm install）"
	@echo "  make web-dev     启动前端开发服务器（vite，热重载，仅 UI）"
	@echo "  make run         建前端 + cargo run，跑完整单二进制（含嵌入 UI）"
	@echo "  make build       建前端 + 后端 debug 构建（产出可运行二进制）"
	@echo "  make build-web   仅构建前端（产出 frontend/dist）"
	@echo "  make release     建前端 + 后端 release 构建（strip+LTO，发布用）"
	@echo "  make test        全量测试（后端 cargo test + 前端 vitest）"
	@echo "  make test-rs     仅后端测试"
	@echo "  make test-web    仅前端测试（vitest）"
	@echo "  make fmt         格式化（cargo fmt + prettier --write）"
	@echo "  make fmt-check   格式检查（cargo fmt --check + eslint/prettier check）"
	@echo "  make lint        静态检查（cargo clippy -D warnings + 前端 lint）"
	@echo "  make check       本地提交前全门：建前端→fmt-check→clippy→cargo test→vitest（对齐 CI）"
	@echo "  make audit       依赖漏洞审计（cargo audit + pnpm audit --prod）"
	@echo "  make clean       清理 cargo target 与前端 dist 产物"

install: ## 安装前端依赖
	$(PNPM) install

web-dev: ## 前端开发服务器（vite，仅 UI 热重载）
	$(PNPM) dev

build-web: ## 仅构建前端（产出 frontend/dist；构建会清掉 .gitkeep，构建后恢复）
	$(PNPM) build
	-@git checkout -- frontend/dist/.gitkeep

run: build-web ## 建前端后跑完整二进制（含嵌入 UI）
	$(CARGO) run

build: build-web ## 建前端 + 后端 debug 构建
	$(CARGO) build

release: build-web ## 建前端 + 后端 release 构建（strip+LTO）
	$(CARGO) build --release

test-rs: ## 仅后端测试
	$(CARGO) test

test-web: ## 仅前端测试（vitest）
	$(PNPM) test

test: test-rs test-web ## 全量测试（后端 + 前端）

fmt: ## 格式化后端与前端
	$(CARGO) fmt
	$(PNPM) format

fmt-check: ## 格式检查（不改文件）
	$(CARGO) fmt --check
	$(PNPM) lint

lint: ## 静态检查（clippy 告警即错 + 前端 lint）
	$(CARGO) clippy --all-targets -- -D warnings
	$(PNPM) lint

# 本地提交前全门：严格对齐 CI（ci.yml）——先建前端供 rust-embed，再后端 fmt/clippy/test，最后前端测试。
check: build-web ## 提交前全门（对齐 CI）
	$(CARGO) fmt --check
	$(CARGO) clippy --all-targets -- -D warnings
	$(CARGO) test
	$(PNPM) test

audit: ## 依赖漏洞审计
	$(CARGO) audit
	$(PNPM) audit --prod

clean: ## 清理构建产物
	$(CARGO) clean
	rm -rf frontend/dist/assets frontend/dist/index.html
	-@git checkout -- frontend/dist/.gitkeep
