#!/usr/bin/env bash
# JianArtifact 远程部署 / 回滚脚本（systemd 二进制路径为现网主路径）。
#
# 约束（见 docs/adr/0007-deployment-orchestration.md 与 docs/OPERATIONS.md）：
#   - 读取不入库的 deploy/.env（或 .env.<环境>，见 §环境文件）；凭据不进日志、不进仓库
#   - 失败以非零退出并给中文错误；重复执行幂等
#   - 健康探活失败自动回滚（systemd 模式）
#   - **运行环境变量的真源是远端的 EnvironmentFile**（如 ~/jianartifact/jianartifact.env，600 权限）：
#     本脚本只切版本与重启，绝不写入或覆盖它——否则会丢掉格式配置、并把 JWT 密钥换成脚本里的值。
#
# 用法：
#   deploy/deploy.sh deploy     # 部署（Compose 主路径，或 systemd 二进制路径）
#   deploy/deploy.sh rollback   # 回滚到上一个健康版本（systemd 模式）
#   deploy/deploy.sh healthcheck# 仅探活
#
# 关键环境变量（可在 deploy/.env 或调用环境提供）：
#   DEPLOY_ENV        环境名：读 deploy/.env.<环境>（如 DEPLOY_ENV=prod → deploy/.env.prod）
#   DEPLOY_MODE       compose | systemd | systemd-user（后两者为 systemd 二进制路径：
#                     systemd=系统级单元 /etc/systemd/system，systemd-user=用户级 rootless
#                     单元 ~/.config/systemd/user；默认 compose）
#   DEPLOY_HOST       目标主机（user@host 或仅 host）；留空则本机部署
#   DEPLOY_USER       SSH 用户（DEPLOY_HOST 未含 user@ 时使用，默认 root）
#   DEPLOY_PORT       SSH 端口（默认 22）
#   DEPLOY_SSH_KEY    SSH 私钥路径（默认 deploy/ssh/jianartifact_ed25519）
#   DEPLOY_SSH_OPTS   附加 ssh 选项（可选，追加在标准选项之后）
#   DEPLOY_DIR        远端发布根（现网为部署用户 home 下的 jianartifact；未设则自动推导）
#   RELEASE_DIR       同上（旧名，兼容保留）
#   DEPLOY_SERVICE    systemd 用户服务名（默认 jianartifact）
#   HEALTH_URL        探活 URL（默认 http://127.0.0.1:8080/readyz）
#   KEEP_RELEASES     systemd 模式保留的历史版本数（默认 5）
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# 环境文件选择：DEPLOY_ENV=prod → deploy/.env.prod（现网按环境存放）；未设则 deploy/.env。
# 环境文件含主机与密钥路径等凭据，已被 .gitignore 忽略（deploy/.env.*）。
DEPLOY_ENV="${DEPLOY_ENV:-}"
if [[ -n "${DEPLOY_ENV}" ]]; then
  ENV_FILE="${SCRIPT_DIR}/.env.${DEPLOY_ENV}"
else
  ENV_FILE="${SCRIPT_DIR}/.env"
fi

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
DEPLOY_SERVICE="${DEPLOY_SERVICE:-jianartifact}"
DEPLOY_PORT="${DEPLOY_PORT:-22}"
# 发布根：RELEASE_DIR/DEPLOY_DIR 由环境文件或调用环境提供，**在 ensure_release_dir 里解析**
# （不能在此处用 ${DEPLOY_DIR:-} 预展开——那时 load_env 还没跑，会拿到空串）
HEALTH_URL="${HEALTH_URL:-http://127.0.0.1:8080/readyz}"
KEEP_RELEASES="${KEEP_RELEASES:-5}"

# 连接参数（在 load_env 之后组装，因为端口 / 密钥路径来自配置）
SSH_OPTS=""
SCP_OPTS=""
build_conn_opts() {
  local key="${DEPLOY_SSH_KEY:-${SCRIPT_DIR}/ssh/jianartifact_ed25519}"
  key="${key/#\~/$HOME}"
  SSH_OPTS="-p ${DEPLOY_PORT} -o StrictHostKeyChecking=accept-new -o IdentitiesOnly=yes"
  SCP_OPTS="-P ${DEPLOY_PORT} -o StrictHostKeyChecking=accept-new -o IdentitiesOnly=yes"
  if [[ -f "${key}" ]]; then
    SSH_OPTS="${SSH_OPTS} -i ${key}"
    SCP_OPTS="${SCP_OPTS} -i ${key}"
  fi
  [[ -n "${DEPLOY_SSH_OPTS:-}" ]] && SSH_OPTS="${SSH_OPTS} ${DEPLOY_SSH_OPTS}"
  return 0
}

ssh_target() {
  local host="${DEPLOY_HOST:-}"
  [[ -n "${host}" ]] || { echo ""; return; }
  if [[ "${host}" != *"@"* ]]; then host="${DEPLOY_USER:-root}@${host}"; fi
  echo "${host}"
}

run_remote() {
  # 在目标主机执行命令；DEPLOY_HOST 留空则本机执行
  if [[ -n "${DEPLOY_HOST:-}" ]]; then
    # shellcheck disable=SC2086
    ssh ${SSH_OPTS} "$(ssh_target)" "$@"
  else
    bash -c "$@"
  fi
}

# 发布根解析**必须晚于 load_env**：环境文件里的 DEPLOY_DIR 在 source 之后才可见，
# 若在 load_env 之前用 ${DEPLOY_DIR:-} 展开，会得到空串并静默回落到 home 推导
# （正式站的 home 推导恰好等于真实路径，所以只在 t1 这类自定义发布根上暴露）。
ensure_release_dir() {
  if [[ -z "${RELEASE_DIR:-}" ]]; then
    RELEASE_DIR="${DEPLOY_DIR:-}"
  fi
  if [[ -n "${RELEASE_DIR:-}" ]]; then
    log "发布根 ${RELEASE_DIR}（来自 RELEASE_DIR/DEPLOY_DIR）"
    return 0
  fi
  local home_dir
  if [[ -n "${DEPLOY_HOST:-}" ]]; then
    home_dir="$(run_remote 'echo $HOME')"
  else
    home_dir="${HOME}"
  fi
  RELEASE_DIR="${home_dir}/jianartifact"
  log "未配置 DEPLOY_DIR/RELEASE_DIR，按部署用户 home 推导：${RELEASE_DIR}"
}

healthcheck() {
  local url="${1:-${HEALTH_URL}}"
  local tries="${2:-30}"
  log "探活 ${url}"
  for ((i = 1; i <= tries; i++)); do
    if run_remote "curl -fsS -o /dev/null '${url}'"; then
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

# systemd 作用域：systemd=系统级单元（/etc/systemd/system，通常以 root 连接），
# systemd-user=用户级 rootless 单元（~/.config/systemd/user，需 lingering）。
# 两者重启命令不同，用错会报 "Unit not found" 或 "Failed to connect to bus"。
SYSTEMD_SCOPE="user"
systemctl_restart() {
  local svc="$1"
  if [[ "${SYSTEMD_SCOPE}" == "system" ]]; then
    run_remote "systemctl restart '${svc}'" || die "重启失败：${svc} 不在系统级作用域。
若该单元其实位于 ~/.config/systemd/user（用户级），请把环境文件里的 DEPLOY_MODE 改为 systemd-user。"
  else
    run_remote "systemctl --user restart '${svc}'" || die "重启失败：${svc} 不在用户级作用域。
若该单元其实位于 /etc/systemd/system（系统级），请把环境文件里的 DEPLOY_MODE 改为 systemd，
并以有权重启该单元的用户连接。"
  fi
}

# 切换 current 并重启服务：部署与回滚共用的唯一动作。
switch_and_restart() {
  local target="$1"
  run_remote "ln -sfn '${target}' '${RELEASE_DIR}/current'"
  systemctl_restart "${DEPLOY_SERVICE}"
}

# 部署前置检查：远端必须已有 EnvironmentFile，否则重启后会因缺关键变量而起不来。
# 这里只检查并给出指引，**不自动创建**——创建会覆盖既有密钥与格式配置。
preflight_env_file() {
  local bin="$1"
  local env_file="${RELEASE_DIR}/jianartifact.env"
  if ! run_remote "test -f '${env_file}'"; then
    die "远端缺少环境文件 ${env_file}。
请先在该主机创建（600 权限），例如：
  JIAN_DATA_DIR=${RELEASE_DIR}/data
  JIAN_HTTP_ADDR=0.0.0.0:8080
  JIAN_JWT_SECRET=\$(head -c 48 /dev/urandom | base64 | tr -d '\n')
  JIAN_PUBLIC_URL=<对外地址>
  JIAN_ENABLED_FORMATS=raw,maven,npm
并在 systemd unit 中引用：EnvironmentFile=%h/jianartifact/jianartifact.env
（本脚本不会代写该文件，以免覆盖既有密钥与配置。）"
  fi
  return 0
}

deploy_systemd() {
  ensure_release_dir
  local scope_desc="用户级（rootless）"
  [[ "${SYSTEMD_SCOPE}" == "system" ]] && scope_desc="系统级"
  log "以 systemd 二进制模式部署（${scope_desc}作用域），发布目录 ${RELEASE_DIR}，服务 ${DEPLOY_SERVICE}。"
  local stamp release
  stamp="$(date +%Y%m%d%H%M%S)"
  release="${RELEASE_DIR}/releases/${stamp}"
  local bin="${SCRIPT_DIR}/../apps/server/bin/jianartifact"
  [[ -f "${bin}" ]] || die "未找到已构建二进制（apps/server/bin/jianartifact）；请先执行 make build 或 bash deploy/remote-ssh.sh build。"
  preflight_env_file "${bin}"

  run_remote "mkdir -p '${release}'"
  if [[ -n "${DEPLOY_HOST:-}" ]]; then
    # shellcheck disable=SC2086
    scp ${SCP_OPTS} "${bin}" "$(ssh_target):${release}/jianartifact"
    run_remote "chmod +x '${release}/jianartifact'"
  else
    cp "${bin}" "${release}/jianartifact"
    chmod +x "${release}/jianartifact"
  fi

  # 记下当前版本，供探活失败时精确回滚（不用"列表第二个"，避免并发部署时回滚错版本）
  local previous
  previous="$(run_remote "readlink -f '${RELEASE_DIR}/current' 2>/dev/null || true")"

  switch_and_restart "${release}"
  if ! healthcheck; then
    log "健康检查失败，自动回滚。"
    if [[ -n "${previous}" ]]; then
      switch_and_restart "${previous}"
      healthcheck || die "回滚后健康检查仍失败，请人工介入。"
      die "部署失败并已回滚至 ${previous}。"
    fi
    die "部署失败且无可回滚的历史版本（current 原为空）。"
  fi

  # 保留最近 N 份
  run_remote "ls -1dt '${RELEASE_DIR}/releases'/*/ | tail -n +$((KEEP_RELEASES + 1)) | xargs -r rm -rf"
  log "systemd 部署完成，当前版本 ${stamp}。"
}

rollback_systemd() {
  ensure_release_dir
  log "回滚到上一个历史版本（服务 ${DEPLOY_SERVICE}）。"
  local prev
  prev="$(run_remote "ls -1dt '${RELEASE_DIR}/releases'/*/ | sed -n 2p")" \
    || die "无法列出历史版本。"
  [[ -n "${prev}" ]] || die "没有可回滚的历史版本。"
  switch_and_restart "${prev%/}"
  healthcheck || die "回滚后健康检查仍失败，请人工介入。"
  log "已回滚至 ${prev}。"
}

main() {
  local cmd="${1:-deploy}"
  load_env
  build_conn_opts
  case "${cmd}" in
    deploy)
      case "${DEPLOY_MODE}" in
        compose) deploy_compose ;;
        # systemd=系统级单元（deploy/.env.test：t1 测试实例），systemd-user=用户级 rootless 单元
        # （deploy/.env.prod：正式站）。作用域由环境文件里的 DEPLOY_MODE 决定。
        systemd) SYSTEMD_SCOPE="system"; deploy_systemd ;;
        systemd-user) SYSTEMD_SCOPE="user"; deploy_systemd ;;
        *) die "未知 DEPLOY_MODE：${DEPLOY_MODE}（应为 compose、systemd 或 systemd-user）。" ;;
      esac
      ;;
    rollback)
      case "${DEPLOY_MODE}" in
        systemd) SYSTEMD_SCOPE="system"; rollback_systemd ;;
        systemd-user) SYSTEMD_SCOPE="user"; rollback_systemd ;;
        compose) die "Compose 模式请用镜像标签回滚（重设 image 后重新 deploy）。" ;;
        *) die "未知 DEPLOY_MODE：${DEPLOY_MODE}。" ;;
      esac
      ;;
    healthcheck) healthcheck ;;
    *) die "未知命令：${cmd}（应为 deploy / rollback / healthcheck）。" ;;
  esac
}

main "$@"
