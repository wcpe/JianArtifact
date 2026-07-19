#!/usr/bin/env bash
# JianArtifact 远程部署 / 回滚脚本。
#
# 约束（见 docs/adr/0007-deployment-orchestration.md 与 docs/OPERATIONS.md）：
#   - 读取不入库的 deploy/.env；凭据不进日志、不进仓库
#   - 失败以非零退出并给中文错误
#   - 重复执行幂等
#   - 健康探活失败自动回滚（systemd 模式）
#
# 用法：
#   deploy/deploy.sh deploy     # 部署（Compose 主路径，或 systemd 二进制路径）
#   deploy/deploy.sh rollback   # 回滚到上一个健康版本（systemd 模式）
#   deploy/deploy.sh healthcheck# 仅探活
#
# 关键环境变量（可在 deploy/.env 或调用环境提供）：
#   DEPLOY_MODE       compose | systemd（默认 compose）
#   DEPLOY_HOST       目标主机（user@host）；留空则本机部署
#   DEPLOY_SSH_OPTS   附加 ssh 选项（可选）
#   HEALTH_URL        探活 URL（默认 http://127.0.0.1:8080/readyz）
#   KEEP_RELEASES     systemd 模式保留的历史版本数（默认 5）
#   RELEASE_DIR       systemd 模式的发布根目录（默认 /opt/jianartifact）
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENV_FILE="${SCRIPT_DIR}/.env"

log()  { echo "[部署] $*"; }
die()  { echo "[部署][错误] $*" >&2; exit 1; }

load_env() {
  if [[ -f "${ENV_FILE}" ]]; then
    # 仅加载键值，不打印内容以免泄露凭据
    set -a
    # shellcheck disable=SC1090
    source "${ENV_FILE}"
    set +a
  else
    log "未找到 ${ENV_FILE}；将使用调用环境中的变量。"
  fi
}

DEPLOY_MODE="${DEPLOY_MODE:-compose}"
HEALTH_URL="${HEALTH_URL:-http://127.0.0.1:8080/readyz}"
KEEP_RELEASES="${KEEP_RELEASES:-5}"
RELEASE_DIR="${RELEASE_DIR:-/opt/jianartifact}"

run_remote() {
  # 在目标主机执行命令；DEPLOY_HOST 留空则本机执行
  if [[ -n "${DEPLOY_HOST:-}" ]]; then
    # shellcheck disable=SC2086
    ssh ${DEPLOY_SSH_OPTS:-} "${DEPLOY_HOST}" "$@"
  else
    bash -c "$@"
  fi
}

healthcheck() {
  local url="${1:-${HEALTH_URL}}"
  local tries="${2:-30}"
  log "探活 ${url}"
  for ((i = 1; i <= tries; i++)); do
    if curl -fsS -o /dev/null "${url}"; then
      log "健康检查通过。"
      return 0
    fi
    sleep 2
  done
  return 1
}

deploy_compose() {
  log "以 Docker Compose 模式部署（DEPLOY_HOST=${DEPLOY_HOST:-本机}）。"
  [[ -f "${ENV_FILE}" ]] || die "Compose 模式需要 ${ENV_FILE}，请先从 .env.example 复制并填值。"
  local compose="docker compose -f ${SCRIPT_DIR}/docker-compose.yml"
  ${compose} pull || log "跳过 pull（本地构建镜像）。"
  ${compose} up -d --build
  if ! healthcheck; then
    die "部署后健康检查失败；请查看容器日志（docker compose logs）。"
  fi
  log "Compose 部署完成。"
}

deploy_systemd() {
  log "以 rootless systemd 二进制模式部署，发布目录 ${RELEASE_DIR}。"
  local stamp release
  stamp="$(date +%Y%m%d%H%M%S)"
  release="${RELEASE_DIR}/releases/${stamp}"
  [[ -f "${SCRIPT_DIR}/../apps/server/bin/jianartifact" ]] \
    || die "未找到已构建二进制（apps/server/bin/jianartifact）；请先执行 make build。"
  run_remote "mkdir -p '${release}'"
  # 上传二进制（本机模式为拷贝）
  if [[ -n "${DEPLOY_HOST:-}" ]]; then
    # shellcheck disable=SC2086
    scp ${DEPLOY_SSH_OPTS:-} "${SCRIPT_DIR}/../apps/server/bin/jianartifact" "${DEPLOY_HOST}:${release}/jianartifact"
  else
    cp "${SCRIPT_DIR}/../apps/server/bin/jianartifact" "${release}/jianartifact"
  fi
  # 原子切换 current 符号链接
  run_remote "ln -sfn '${release}' '${RELEASE_DIR}/current' && systemctl --user restart jianartifact"
  if ! healthcheck; then
    log "健康检查失败，自动回滚。"
    rollback_systemd
    die "部署失败并已回滚。"
  fi
  # 保留最近 N 份
  run_remote "ls -1dt '${RELEASE_DIR}/releases'/*/ | tail -n +$((KEEP_RELEASES + 1)) | xargs -r rm -rf"
  log "systemd 部署完成，当前版本 ${stamp}。"
}

rollback_systemd() {
  log "回滚到上一个历史版本。"
  local prev
  prev="$(run_remote "ls -1dt '${RELEASE_DIR}/releases'/*/ | sed -n 2p")" \
    || die "无法列出历史版本。"
  [[ -n "${prev}" ]] || die "没有可回滚的历史版本。"
  run_remote "ln -sfn '${prev%/}' '${RELEASE_DIR}/current' && systemctl --user restart jianartifact"
  healthcheck || die "回滚后健康检查仍失败，请人工介入。"
  log "已回滚至 ${prev}。"
}

main() {
  local cmd="${1:-deploy}"
  load_env
  case "${cmd}" in
    deploy)
      case "${DEPLOY_MODE}" in
        compose) deploy_compose ;;
        systemd) deploy_systemd ;;
        *) die "未知 DEPLOY_MODE：${DEPLOY_MODE}（应为 compose 或 systemd）。" ;;
      esac
      ;;
    rollback)
      case "${DEPLOY_MODE}" in
        systemd) rollback_systemd ;;
        compose) die "Compose 模式请用镜像标签回滚（重设 image 后重新 deploy）。" ;;
        *) die "未知 DEPLOY_MODE：${DEPLOY_MODE}。" ;;
      esac
      ;;
    healthcheck) healthcheck ;;
    *) die "未知命令：${cmd}（应为 deploy / rollback / healthcheck）。" ;;
  esac
}

main "$@"
