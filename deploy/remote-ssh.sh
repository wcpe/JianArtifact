#!/usr/bin/env bash
# JianArtifact 远程 SSH 部署辅助脚本（systemd 二进制路径）。
#
# 与 deploy.sh 配合：本脚本专注「本机构建 → scp 上传 → 远端 systemd 切换 → 探活」。
# 凭据与主机信息不入库：读 deploy/.env 或环境变量。
#
# 用法：
#   bash deploy/remote-ssh.sh setup-key     # 生成 deploy/ssh/ 下密钥（不入库）
#   bash deploy/remote-ssh.sh show-pubkey  # 打印公钥，供粘贴到主机 authorized_keys
#   bash deploy/remote-ssh.sh deploy       # 构建并远程部署
#   bash deploy/remote-ssh.sh health       # 远程探活
#   bash deploy/remote-ssh.sh ssh          # 交互登录
#
# 环境变量（deploy/.env 或 export）：
#   DEPLOY_HOST          必填，user@host 或仅 host（配合 DEPLOY_USER）
#   DEPLOY_USER          可选，默认 root
#   DEPLOY_PORT          SSH 端口，默认 22
#   DEPLOY_SSH_KEY       私钥路径，默认 deploy/ssh/jianartifact_ed25519
#   RELEASE_DIR          远端发布根，默认 /opt/jianartifact
#   HEALTH_URL           远端探活（经 SSH 本地 curl），默认 http://127.0.0.1:8080/readyz
#   JIAN_HTTP_ADDR       远端监听，写入 unit 环境时可选
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
ENV_FILE="${SCRIPT_DIR}/.env"
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
RELEASE_DIR="${RELEASE_DIR:-/opt/jianartifact}"
HEALTH_URL="${HEALTH_URL:-http://127.0.0.1:8080/readyz}"

resolve_host() {
  local host="${DEPLOY_HOST:-}"
  [[ -n "${host}" ]] || die "请设置 DEPLOY_HOST（user@ip 或 ip）"
  if [[ "${host}" != *"@"* ]]; then
    host="${DEPLOY_USER:-root}@${host}"
  fi
  echo "${host}"
}

ssh_opts() {
  local opts=(-p "${DEPLOY_PORT}" -o StrictHostKeyChecking=accept-new -o IdentitiesOnly=yes)
  if [[ -f "${DEPLOY_SSH_KEY}" ]]; then
    opts+=(-i "${DEPLOY_SSH_KEY}")
  fi
  printf '%q ' "${opts[@]}"
}

run_ssh() {
  local host
  host="$(resolve_host)"
  # shellcheck disable=SC2046
  ssh $(ssh_opts) "${host}" "$@"
}

run_scp() {
  local host src dest
  host="$(resolve_host)"
  src="$1"
  dest="$2"
  # shellcheck disable=SC2046
  scp -P "${DEPLOY_PORT}" -o StrictHostKeyChecking=accept-new -o IdentitiesOnly=yes \
    ${DEPLOY_SSH_KEY:+-i "${DEPLOY_SSH_KEY}"} \
    "${src}" "${host}:${dest}"
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
  log "构建后端二进制（CGO_ENABLED=0）…"
  (
    cd "${ROOT_DIR}/apps/server"
    CGO_ENABLED=0 go build -trimpath -o bin/jianartifact ./cmd/jianartifact
  )
  [[ -f "${ROOT_DIR}/apps/server/bin/jianartifact" ]] \
    || die "构建失败：未找到 apps/server/bin/jianartifact"
  log "构建完成。"
}

cmd_deploy() {
  load_env
  cmd_build
  local stamp release host
  stamp="$(date +%Y%m%d%H%M%S)"
  release="${RELEASE_DIR}/releases/${stamp}"
  host="$(resolve_host)"
  log "部署到 ${host}:${release}"

  run_ssh "mkdir -p '${release}' '${RELEASE_DIR}/data' '${RELEASE_DIR}/bin'"
  run_scp "${ROOT_DIR}/apps/server/bin/jianartifact" "${release}/jianartifact"
  run_ssh "chmod +x '${release}/jianartifact' && ln -sfn '${release}' '${RELEASE_DIR}/current'"

  # 若存在用户级 systemd unit 则重启；否则提示手动 run
  if run_ssh "systemctl --user is-enabled jianartifact >/dev/null 2>&1"; then
    run_ssh "systemctl --user restart jianartifact"
  elif run_ssh "systemctl is-enabled jianartifact >/dev/null 2>&1"; then
    run_ssh "sudo systemctl restart jianartifact"
  else
    log "未检测到 jianartifact systemd unit；请在远端手动启动："
    log "  ${RELEASE_DIR}/current/jianartifact run"
    log "  或安装 deploy/systemd/jianartifact.service"
  fi

  log "等待探活 ${HEALTH_URL} …"
  local ok=0
  for i in $(seq 1 30); do
    if run_ssh "curl -fsS -o /dev/null '${HEALTH_URL}'"; then
      ok=1
      break
    fi
    sleep 2
  done
  [[ "${ok}" -eq 1 ]] || die "远程探活失败，请检查进程与端口。"
  log "部署成功。current → ${stamp}"
}

cmd_health() {
  load_env
  run_ssh "curl -fsS '${HEALTH_URL}' && echo"
  log "探活通过。"
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
  deploy       构建 + scp + 远端切换 + 探活
  health       远端 /readyz 探活
  ssh          使用部署密钥登录

先 setup-key，把公钥放到主机，再在 deploy/.env 写：
  DEPLOY_HOST=root@x.x.x.x
  DEPLOY_PORT=22
USAGE
      exit 1
      ;;
  esac
}

main "$@"
