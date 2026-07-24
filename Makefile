# JianArtifact 顶层入口：前端经 Turborepo、后端委托到 Go Task。
# 本地与 CI 复用同一组目标；实际质量门在 Docker 中执行（scripts/check.sh）。

.DEFAULT_GOAL := help
.PHONY: help install dev check build release deploy rollback gen test lint clean

help: ## 显示可用目标
	@echo "JianArtifact — 可用 make 目标："
	@echo "  install   安装前端依赖（pnpm）"
	@echo "  dev       本地开发（前端 + 后端）"
	@echo "  gen       从 api/openapi.yaml 生成 Go 接口与前端 client"
	@echo "  check     在 Docker 中运行全部质量门"
	@echo "  build     前端构建 + 后端 embed 编译单二进制"
	@echo "  release   产出多平台发布物（校验和 / 签名 / SBOM）"
	@echo "  deploy    远程部署（见 deploy/deploy.sh）"
	@echo "  rollback  远程回滚到上一个健康版本"
	@echo "  clean     清理构建产物"

install: ## 安装前端依赖
	pnpm install

dev: ## 本地开发
	pnpm dev

gen: ## 生成契约代码
	cd apps/server && task gen

check: ## 在 Docker 中运行全部质量门
	bash scripts/check.sh

build: ## 前端构建 + 后端 embed 编译
	pnpm build
	rm -rf apps/server/web/dist
	mkdir -p apps/server/web/dist
	cp -a apps/web/dist/. apps/server/web/dist/
	cd apps/server && task build

release: ## 多平台发布物
	cd apps/server && task release

deploy: ## 远程部署
	bash deploy/deploy.sh deploy

rollback: ## 远程回滚
	bash deploy/deploy.sh rollback

test: ## 前后端测试
	pnpm test
	cd apps/server && task test

lint: ## 前后端静态检查
	pnpm lint
	cd apps/server && task lint

clean: ## 清理产物
	pnpm -r exec -- rm -rf dist || true
	cd apps/server && task clean || true
