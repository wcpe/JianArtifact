#!/usr/bin/env bash
# JianArtifact 远程部署辅助：密钥管理 + 本机构建 + 登录/探活入口。
#
# 分工（重要）：
#   - 本脚本**不自己启动远端服务**。早期版本用「kill + nohup + pid 文件」启动，与现网的
#     systemd --user 托管冲突（双进程抢端口），且只写 3 个环境变量（丢掉格式配置、把 JWT
#     换成脚本内置的默认值）。现改为：构建后**委托 deploy/deploy.sh** 执行部署
#     （releases/<stamp> + 切 current + systemctl --user restart + 探活失败回滚）。
#   - 运行环境变量的真源是远端的 EnvironmentFile（如 ~/jianartifact/jianartifact.env），
#     由运维维护；本脚本与 deploy.sh 都不会写入它。
#
# 用法：
#   bash deploy/remote-ssh.sh setup-key     # 生成 deploy/ssh/ 下密钥（不入库）
#   bash deploy/remote-ssh.sh show-pubkey   # 打印公钥，供粘贴到主机 authorized_keys
#   bash deploy/remote-ssh.sh build         # 仅本机构建 Linux amd64 二进制
#   bash deploy/remote-ssh.sh deploy        # 构建 + 委托 deploy.sh 远程部署
#   bash deploy/remote-ssh.sh health        # 远程探活（委托 deploy.sh healthcheck）
#   bash deploy/remote-ssh.sh ssh           # 交互登录
#
# 环境变量（deploy/.env 或 export）：
#   DEPLOY_ENV            环境名：读 deploy/.env.<环境>（如 DEPLOY_ENV=prod → deploy/.env.prod）
#   DEPLOY_HOST          必填，user@host 或仅 host（配合 DEPLOY_USER）
#   DEPLOY_USER          可选，默认 root
#   DEPLOY_PORT          SSH 端口，默认 22
#   DEPLOY_SSH_KEY       私钥路径，默认 deploy/ssh/jianartifact_ed25519
#   DEPLOY_DIR           远端发布根（未设则由 deploy.sh 按远端 home 推导）
#   DEPLOY_SERVICE       systemd 用户服务名，默认 jianartifact
#   HEALTH_URL           远端探活，默认 http://127.0.0.1:8080/readyz
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
# 环境文件选择：DEPLOY_ENV=prod → deploy/.env.prod（现网按环境存放）；未设则 deploy/.env。
# 环境文件含主机与密钥路径等凭据，已被 .gitignore 忽略（deploy/.env.*）。
DEPLOY_ENV="${DEPLOY_ENV:-}"
if [[ -n "${DEPLOY_ENV}" ]]; then
  ENV_FILE="${SCRIPT_DIR}/.env.${DEPLOY_ENV}"
else
  ENV_FILE="${SCRIPT_DIR}/.env"
fi
SSH_DIR="${SCRIPT_DIR}/ssh"
DEFAULT_KEY="${SSH_DIR}/jianartifact_ed25519"

log() { echo "[remote-ssh] $*"; }
die() { echo "[remote-ssh][错误] $*" >&2; exit 1; }

load_env() {
  if [[ -f "${ENV_FILE}" ]]; then
    set -a
    # shellcheck disable=SC1090
    source "${ENV_FILE}"
    set +a
  fi
}

DEPLOY_PORT="${DEPLOY_PORT:-22}"
DEPLOY_SSH_KEY="${DEPLOY_SSH_KEY:-${DEFAULT_KEY}}"

resolve_host() {
  local host="${DEPLOY_HOST:-}"
  [[ -n "${host}" ]] || die "请设置 DEPLOY_HOST（user@ip 或 ip）"
  if [[ "${host}" != *"@"* ]]; then
    host="${DEPLOY_USER:-root}@${host}"
  fi
  echo "${host}"
}

ssh_opts() {
  local key="${DEPLOY_SSH_KEY/#\~/$HOME}"
  local opts=(-p "${DEPLOY_PORT}" -o StrictHostKeyChecking=accept-new -o IdentitiesOnly=yes)
  if [[ -f "${key}" ]]; then
    opts+=(-i "${key}")
  fi
  printf '%q ' "${opts[@]}"
}

cmd_setup_key() {
  mkdir -p "${SSH_DIR}"
  chmod 700 "${SSH_DIR}" 2>/dev/null || true
  if [[ -f "${DEFAULT_KEY}" ]]; then
    log "密钥已存在：${DEFAULT_KEY}（不覆盖）。删除后重跑可重新生成。"
  else
    ssh-keygen -t ed25519 -f "${DEFAULT_KEY}" -N "" -C "jianartifact-deploy@$(date +%Y%m%d)"
    log "已生成密钥对："
    log "  私钥：${DEFAULT_KEY}（切勿入库、切勿外传）"
    log "  公钥：${DEFAULT_KEY}.pub"
  fi
  echo
  log "请将下列公钥追加到目标主机 ~/.ssh/authorized_keys ："
  echo "-----"
  cat "${DEFAULT_KEY}.pub"
  echo "-----"
  log "主机上执行示例："
  log "  mkdir -p ~/.ssh && chmod 700 ~/.ssh"
  log "  echo '（粘贴公钥一行）' >> ~/.ssh/authorized_keys && chmod 600 ~/.ssh/authorized_keys"
}

cmd_show_pubkey() {
  [[ -f "${DEFAULT_KEY}.pub" ]] || die "未找到公钥，请先：bash deploy/remote-ssh.sh setup-key"
  cat "${DEFAULT_KEY}.pub"
}

cmd_build() {
  # 始终交叉编译 Linux amd64（本机可能是 Windows/macOS）
  # 版本号优先读仓库根 VERSION 文件，注入 main.version（管理端 /status 展示）
  local out="${ROOT_DIR}/apps/server/bin/jianartifact-linux-amd64"
  local ver="0.0.0-dev"
  if [[ -f "${ROOT_DIR}/VERSION" ]]; then
    ver="$(tr -d '[:space:]' < "${ROOT_DIR}/VERSION")"
    [[ -n "${ver}" ]] || ver="0.0.0-dev"
  fi
  log "构建 Linux amd64 二进制（CGO_ENABLED=0 GOOS=linux version=${ver}）…"
  (
    cd "${ROOT_DIR}/apps/server"
    GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath \
      -ldflags="-s -w -X main.version=${ver}" \
      -o bin/jianartifact-linux-amd64 ./cmd/jianartifact
  )
  [[ -f "${out}" ]] || die "构建失败：未找到 ${out}"
  # deploy.sh 按 apps/server/bin/jianartifact 上传（与 make build 的产物路径一致）
  cp "${out}" "${ROOT_DIR}/apps/server/bin/jianartifact"
  log "构建完成：${out}（version=${ver}）"
}

cmd_deploy() {
  load_env
  cmd_build
  log "委托 deploy.sh 执行部署（releases/current + systemctl 重启 + 失败回滚）…"
  # 现网形态固定为 systemd 二进制路径；发布根/服务名由 deploy.sh 从同一份 .env 读取
  DEPLOY_MODE=systemd bash "${SCRIPT_DIR}/deploy.sh" deploy
}

cmd_health() {
  load_env
  DEPLOY_MODE="${DEPLOY_MODE:-systemd}" bash "${SCRIPT_DIR}/deploy.sh" healthcheck
}

cmd_ssh() {
  load_env
  local host
  host="$(resolve_host)"
  # shellcheck disable=SC2046
  exec ssh $(ssh_opts) "${host}"
}

main() {
  local cmd="${1:-}"
  case "${cmd}" in
    setup-key) cmd_setup_key ;;
    show-pubkey) cmd_show_pubkey ;;
    build) load_env; cmd_build ;;
    deploy) cmd_deploy ;;
    health) cmd_health ;;
    ssh) cmd_ssh ;;
    *)
      cat <<'USAGE'
用法：bash deploy/remote-ssh.sh <命令>

  setup-key    生成 ed25519 密钥到 deploy/ssh/（不入库）
  show-pubkey  打印公钥
  build        仅本机构建二进制
  deploy       构建 + 委托 deploy.sh 部署（releases/current + systemctl 重启 + 失败回滚）
  health       远端 /readyz 探活
  ssh          使用部署密钥登录

前置：
  1) 先 setup-key，把公钥放到主机 ~/.ssh/authorized_keys
  2) 在 deploy/.env 写 DEPLOY_HOST / DEPLOY_PORT / DEPLOY_DIR / DEPLOY_SERVICE 等
  3) 主机上已有 EnvironmentFile（见 deploy.sh 的前置检查提示）
USAGE
      exit 1
      ;;
  esac
}

main "$@"
