#!/usr/bin/env bash
set -euo pipefail

# ==============================================================================
# Volo-go macOS launchd Service Installer (LaunchAgent & LaunchDaemon)
# Supports: macOS Intel (darwin/amd64) & Apple Silicon (darwin/arm64)
# Usage:
#   bash scripts/install-service-macos.sh install [options]          # User LaunchAgent (no sudo)
#   sudo bash scripts/install-service-macos.sh install --system ...  # System LaunchDaemon
#   bash scripts/install-service-macos.sh status
#   bash scripts/install-service-macos.sh uninstall
# ==============================================================================

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

ACTION="${1:-install}"
if [[ "${ACTION}" == -* ]]; then
  ACTION="install"
else
  shift || true
fi

SYSTEM_MODE=0
BIN_SOURCE="${REPO_ROOT}/volo"
INSTALL_BIN=""
WORK_DIR=""
ENV_FILE=""
LISTEN_ADDR="127.0.0.1:8080"
PUBLIC_URL="http://localhost:8080"
ADMIN_PATH="/console"
TRUSTED_PROXIES="127.0.0.1/32,::1/128"
DRY_RUN=0
PURGE_DATA=0

usage() {
  cat <<'EOF'
Usage: install-service-macos.sh <install|upgrade|uninstall|status> [options]

Options:
  --system                  Install as system-wide /Library/LaunchDaemons/io.volo.blog.plist (requires sudo)
  --binary <path>           Path to compiled volo binary (default: ./volo)
  --install-bin <path>      Target binary path (default: ~/.local/bin/volo or /usr/local/bin/volo)
  --work-dir <dir>          Data & assets root (default: ~/Library/Application Support/Volo or /usr/local/var/volo)
  --env-file <path>         Environment file path
  --listen-addr <host:port> TCP listen address (default: 127.0.0.1:8080)
  --public-url <url>        Canonical public URL (default: http://localhost:8080)
  --admin-path <prefix>     Custom stealth admin path (default: /console)
  --trusted-proxies <cidrs> Comma-separated reverse proxy CIDRs (default: 127.0.0.1/32,::1/128)
  --purge-data              With 'uninstall': also delete work-dir and env-file
  --dry-run                 Print planned commands and generated plist without modifying host
  -h, --help                Show this help message
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --system) SYSTEM_MODE=1; shift ;;
    --binary) BIN_SOURCE="$2"; shift 2 ;;
    --install-bin) INSTALL_BIN="$2"; shift 2 ;;
    --work-dir) WORK_DIR="$2"; shift 2 ;;
    --env-file) ENV_FILE="$2"; shift 2 ;;
    --listen-addr) LISTEN_ADDR="$2"; shift 2 ;;
    --public-url) PUBLIC_URL="$2"; shift 2 ;;
    --admin-path) ADMIN_PATH="$2"; shift 2 ;;
    --trusted-proxies) TRUSTED_PROXIES="$2"; shift 2 ;;
    --purge-data) PURGE_DATA=1; shift ;;
    --dry-run) DRY_RUN=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "Unknown option: $1" >&2; usage; exit 2 ;;
  esac
done

if (( SYSTEM_MODE == 1 )); then
  INSTALL_BIN="${INSTALL_BIN:-/usr/local/bin/volo}"
  WORK_DIR="${WORK_DIR:-/usr/local/var/volo}"
  ENV_FILE="${ENV_FILE:-/usr/local/etc/volo/volo.env}"
  LOG_DIR="/usr/local/var/log/volo"
  PLIST_PATH="/Library/LaunchDaemons/io.volo.blog.plist"
  LAUNCH_DOMAIN="system"
else
  INSTALL_BIN="${INSTALL_BIN:-${HOME}/.local/bin/volo}"
  WORK_DIR="${WORK_DIR:-${HOME}/Library/Application Support/Volo}"
  ENV_FILE="${ENV_FILE:-${WORK_DIR}/volo.env}"
  LOG_DIR="${HOME}/Library/Logs/Volo"
  PLIST_PATH="${HOME}/Library/LaunchAgents/io.volo.blog.plist"
  LAUNCH_DOMAIN="gui/$(id -u)"
fi

run_cmd() {
  if (( DRY_RUN == 1 )); then
    echo "[dry-run] $*"
  else
    "$@"
  fi
}

generate_secret_hex() {
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -hex 32
  else
    head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n'
  fi
}

render_plist() {
  local tpl="${REPO_ROOT}/deploy/launchd/io.volo.blog.plist"
  sed \
    -e "s|__VOLO_BIN__|${INSTALL_BIN}|g" \
    -e "s|__VOLO_ENV_FILE__|${ENV_FILE}|g" \
    -e "s|__VOLO_WORK_DIR__|${WORK_DIR}|g" \
    -e "s|__VOLO_LOG_DIR__|${LOG_DIR}|g" \
    "${tpl}"
}

case "${ACTION}" in
  install|upgrade)
    if (( SYSTEM_MODE == 1 )) && (( DRY_RUN == 0 )) && [[ "${EUID:-$(id -u)}" -ne 0 ]]; then
      echo "Error: --system mode requires sudo." >&2
      exit 1
    fi
    if [[ ! -f "${BIN_SOURCE}" ]] && (( DRY_RUN == 0 )); then
      echo "Error: binary not found at '${BIN_SOURCE}'. Pass --binary /path/to/volo." >&2
      exit 1
    fi

    run_cmd mkdir -p \
      "$(dirname "${INSTALL_BIN}")" \
      "$(dirname "${ENV_FILE}")" \
      "$(dirname "${PLIST_PATH}")" \
      "${LOG_DIR}" \
      "${WORK_DIR}/data/uploads" \
      "${WORK_DIR}/data/backups" \
      "${WORK_DIR}/data/geoip" \
      "${WORK_DIR}/plugins" \
      "${WORK_DIR}/themes" \
      "${WORK_DIR}/locales"

    if [[ -d "${REPO_ROOT}/plugins/kanbanniang" ]] && [[ ! -d "${WORK_DIR}/plugins/kanbanniang" ]]; then
      run_cmd cp -R "${REPO_ROOT}/plugins/kanbanniang" "${WORK_DIR}/plugins/kanbanniang"
    fi
    if [[ -d "${REPO_ROOT}/themes/volo-journal" ]] && [[ ! -d "${WORK_DIR}/themes/volo-journal" ]]; then
      run_cmd cp -R "${REPO_ROOT}/themes/volo-journal" "${WORK_DIR}/themes/volo-journal"
    fi

    run_cmd install -m 0755 "${BIN_SOURCE}" "${INSTALL_BIN}"

    if [[ ! -f "${ENV_FILE}" ]]; then
      SECRET="$(generate_secret_hex)"
      ENV_BODY="$(cat <<EOF
# Generated by install-service-macos.sh
VOLO_LISTEN_ADDR=${LISTEN_ADDR}
VOLO_PUBLIC_URL=${PUBLIC_URL}
VOLO_TRUSTED_PROXIES=${TRUSTED_PROXIES}
VOLO_ADMIN_PATH=${ADMIN_PATH}
VOLO_SECRET_KEY=${SECRET}
VOLO_GEO_ACCESS_ENABLED=true

VOLO_DB_DRIVER=sqlite
VOLO_DB_DSN=${WORK_DIR}/data/volo.db

VOLO_DATA_DIR=${WORK_DIR}/data
VOLO_UPLOAD_DIR=${WORK_DIR}/data/uploads
VOLO_BACKUP_DIR=${WORK_DIR}/data/backups
VOLO_GEOIP_DIR=${WORK_DIR}/data/geoip
VOLO_PLUGINS_DIR=${WORK_DIR}/plugins
VOLO_THEMES_DIR=${WORK_DIR}/themes
VOLO_LOCALES_DIR=${WORK_DIR}/locales
EOF
)"
      if (( DRY_RUN == 1 )); then
        echo "[dry-run] write ${ENV_FILE} (0600)"
      else
        umask 0077
        printf '%s\n' "${ENV_BODY}" > "${ENV_FILE}"
        chmod 0600 "${ENV_FILE}"
      fi
    fi

    PLIST_BODY="$(render_plist)"
    if (( DRY_RUN == 1 )); then
      echo "[dry-run] write ${PLIST_PATH}:"
      echo "${PLIST_BODY}"
      echo "[dry-run] launchctl bootout ${LAUNCH_DOMAIN}/io.volo.blog || true"
      echo "[dry-run] launchctl bootstrap ${LAUNCH_DOMAIN} '${PLIST_PATH}'"
    else
      printf '%s\n' "${PLIST_BODY}" > "${PLIST_PATH}"
      chmod 0644 "${PLIST_PATH}"
      launchctl bootout "${LAUNCH_DOMAIN}/io.volo.blog" >/dev/null 2>&1 || true
      launchctl bootstrap "${LAUNCH_DOMAIN}" "${PLIST_PATH}"
      launchctl kickstart -k "${LAUNCH_DOMAIN}/io.volo.blog" >/dev/null 2>&1 || true
    fi
    echo "Volo-go macOS launchd service ${ACTION} completed (${LAUNCH_DOMAIN})."
    ;;

  status)
    run_cmd launchctl print "${LAUNCH_DOMAIN}/io.volo.blog"
    ;;

  uninstall)
    run_cmd launchctl bootout "${LAUNCH_DOMAIN}/io.volo.blog" || true
    run_cmd rm -f "${PLIST_PATH}" "${INSTALL_BIN}"
    if (( PURGE_DATA == 1 )); then
      run_cmd rm -rf "${WORK_DIR}" "${ENV_FILE}" "${LOG_DIR}"
      echo "Volo-go launchd service and data purged."
    else
      echo "Volo-go launchd service uninstalled. Data preserved at '${WORK_DIR}'."
    fi
    ;;

  *)
    echo "Unknown action: ${ACTION}" >&2
    usage
    exit 2
    ;;
esac
