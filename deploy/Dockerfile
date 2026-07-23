# 多阶段构建：前端 dist -> Go embed 静态编译 -> distroless 最小运行镜像。
# 见 docs/adr/0005-single-binary-embed.md 与 docs/adr/0007-deployment-orchestration.md。
#
# 基础镜像通过 ARG 参数化：默认取生产推荐镜像（distroless nonroot）。
# 在离线 / 内网环境可用 --build-arg 覆盖为本地已缓存镜像，例如：
#   --build-arg WEB_IMAGE=node:22-alpine \
#   --build-arg BUILD_IMAGE=golang:1.26.5-bookworm \
#   --build-arg RUNTIME_IMAGE=alpine:3.20
ARG WEB_IMAGE=node:22-bookworm-slim
ARG BUILD_IMAGE=golang:1.26-bookworm
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12:nonroot

# ---- Stage 1：前端构建 ----
FROM ${WEB_IMAGE} AS web
WORKDIR /src
RUN corepack enable
# 先拷依赖清单以利用层缓存
COPY pnpm-workspace.yaml package.json pnpm-lock.yaml* .npmrc* ./
COPY apps/web/package.json apps/web/
COPY packages/ui/package.json packages/ui/
COPY packages/devmock/package.json packages/devmock/
COPY packages/eslint-config/package.json packages/eslint-config/
COPY packages/typescript-config/package.json packages/typescript-config/
RUN pnpm install --frozen-lockfile || pnpm install
COPY . .
# 仅构建管理端 web 及其依赖（ui/devmock 等）；wiki 是独立验收站，不进二进制 embed
RUN pnpm --filter @jianartifact/web... build

# ---- Stage 2：Go 编译（内嵌前端 dist）----
FROM ${BUILD_IMAGE} AS build
WORKDIR /src
# 用国内可达的 Go 模块代理，避免默认 proxy.golang.org 在内网 / 离线环境不可达
ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY}
ENV GOTOOLCHAIN=local
COPY go.work ./
COPY apps/server/go.mod apps/server/go.sum* apps/server/
RUN cd apps/server && go mod download || true
COPY . .
# 将前端产物放到后端 embed 目录
COPY --from=web /src/apps/web/dist/ apps/server/web/dist/
ENV CGO_ENABLED=0
# VERSION 可由构建参数注入（CI 预览版带 -dev.<sha>；正式版与 VERSION 文件一致）
ARG VERSION=
RUN V="${VERSION:-$(cat /src/VERSION 2>/dev/null || echo dev)}" \
 && cd apps/server \
 && go build -trimpath -ldflags="-s -w -X main.version=${V}" \
    -o /out/jianartifact ./cmd/jianartifact

# ---- Stage 3：最小运行镜像 ----
FROM ${RUNTIME_IMAGE} AS runtime
WORKDIR /app
COPY --from=build /out/jianartifact /app/jianartifact
# 数据目录（元数据 SQLite 与 blob）通过卷挂载到 /data
VOLUME ["/data"]
EXPOSE 8080
# 65532 即 distroless nonroot 的 UID，用数字形式以便在任意最小基础镜像上都能非 root 运行
USER 65532:65532
ENTRYPOINT ["/app/jianartifact"]
# 默认子命令为 run（启动 HTTP 服务）；无参数只打印用法，故须显式传 run。
# 探活 / 运维经其它子命令覆盖，例如 docker run ... jianartifact status。
CMD ["run"]
