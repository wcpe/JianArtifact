# 多阶段构建：前端 dist -> Go embed 静态编译 -> distroless 最小运行镜像。
# 见 docs/adr/0005-single-binary-embed.md 与 docs/adr/0007-deployment-orchestration.md。

# ---- Stage 1：前端构建 ----
FROM node:22-bookworm-slim AS web
WORKDIR /src
RUN corepack enable
# 先拷依赖清单以利用层缓存
COPY pnpm-workspace.yaml package.json pnpm-lock.yaml* ./
COPY apps/web/package.json apps/web/
COPY apps/wiki/package.json apps/wiki/
COPY packages/ui/package.json packages/ui/
COPY packages/devmock/package.json packages/devmock/
COPY packages/eslint-config/package.json packages/eslint-config/
COPY packages/typescript-config/package.json packages/typescript-config/
RUN pnpm install --frozen-lockfile || pnpm install
COPY . .
RUN pnpm build

# ---- Stage 2：Go 编译（内嵌前端 dist）----
FROM golang:1.23-bookworm AS build
WORKDIR /src
COPY go.work ./
COPY apps/server/go.mod apps/server/go.sum* apps/server/
RUN cd apps/server && go mod download || true
COPY . .
# 将前端产物放到后端 embed 目录
COPY --from=web /src/apps/web/dist/ apps/server/web/dist/
ENV CGO_ENABLED=0
RUN cd apps/server && go build -trimpath -ldflags="-s -w -X main.version=$(cat /src/VERSION 2>/dev/null || echo dev)" \
    -o /out/jianartifact ./cmd/jianartifact

# ---- Stage 3：最小运行镜像 ----
FROM gcr.io/distroless/static-debian12:nonroot AS runtime
WORKDIR /app
COPY --from=build /out/jianartifact /app/jianartifact
# 数据目录（元数据 SQLite 与 blob）通过卷挂载到 /data
VOLUME ["/data"]
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/app/jianartifact"]
