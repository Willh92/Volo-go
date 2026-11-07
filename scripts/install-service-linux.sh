#!/usr/bin/env bash
set -euo pipefail

# ==============================================================================
# Volo-go Linux System Service Installer (systemd & OpenRC)
# Supports: Debian / Ubuntu / RHEL / Rocky / Arch / openSUSE (systemd)
#           Alpine Linux / Gentoo (OpenRC)
# Usage:
#   sudo bash scripts/install-service-linux.sh install [options]
#   sudo bash scripts/install-service-linux.sh upgrade --binary ./volo
#   sudo bash scripts/install-service-linux.sh status
#   sudo bash scripts/install-service-linux.sh uninstall
# ==============================================================================

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

ACTION="${1:-install}"
if [[ "${ACTION}" == -* ]]; then
  ACTION="install"
else
  shift || true
fi

BIN_SOURCE="${REPO_ROOT}/volo"
INSTALL_BIN="/usr/local/bin/volo"
WORK_DIR="/var/lib/volo"
CONF_DIR="/etc/volo"
ENV_FILE="/etc/volo/volo.env"
SERVICE_USER="volo"
SERVICE_GROUP="volo"
LISTEN_ADDR="127.0.0.1:8080"
LISTEN_SOCKET=""
SOCKET_MODE="0660"
PUBLIC_URL="http://localhost:8080"
ADMIN_PATH="/console"
TRUSTED_PROXIES="127.0.0.1/32,::1/128"
INIT_SYSTEM="auto"
DRY_RUN=0
PURGE_DATA=0

usage() {
  cat <<'EOF'
Usage: install-service-linux.sh <install|upgrade|uninstall|status> [options]

Options:
  --binary <path>           Path to compiled volo binary (default: ./volo)
  --install-bin <path>      Target binary path (default: /usr/local/bin/volo)
  --work-dir <dir>          Data & assets root directory (default: /var/lib/volo)
  --env-file <path>         Environment file path (default: /etc/volo/volo.env)
  --user <name>             System user to run service (default: volo)
  --group <name>            System group to run service (default: volo; set www-data/nginx for UDS)
  --listen-addr <host:port> TCP listen address (default: 127.0.0.1:8080)
  --listen-socket <path>    Unix Domain Socket path (e.g. /run/volo/volo.sock; overrides TCP when set)
  --socket-mode <octal>     Unix Socket chmod mode (default: 0660)
  --public-url <url>        Canonical public URL (default: http://localhost:8080)
  --admin-path <prefix>     Custom stealth admin path (default: /console)
  --trusted-proxies <cidrs> Comma-separated reverse proxy CIDRs (default: 127.0.0.1/32,::1/128)
  --init <auto|systemd|openrc> Override init system detection
  --purge-data              With 'uninstall': also delete /var/lib/volo and /etc/volo (destructive!)
  --dry-run                 Print planned commands and generated configs without modifying host
  -h, --help                Show this help message
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --binary) BIN_SOURCE="$2"; shift 2 ;;
    --install-bin) INSTALL_BIN="$2"; shift 2 ;;
    --work-dir) WORK_DIR="$2"; shift 2 ;;
    --env-file) ENV_FILE="$2"; CONF_DIR="$(dirname "$2")"; shift 2 ;;
    --user) SERVICE_USER="$2"; shift 2 ;;
    --group) SERVICE_GROUP="$2"; shift 2 ;;
    --listen-addr) LISTEN_ADDR="$2"; shift 2 ;;
    --listen-socket) LISTEN_SOCKET="$2"; shift 2 ;;
    --socket-mode) SOCKET_MODE="$2"; shift 2 ;;
    --public-url) PUBLIC_URL="$2"; shift 2 ;;
    --admin-path) ADMIN_PATH="$2"; shift 2 ;;
    --trusted-proxies) TRUSTED_PROXIES="$2"; shift 2 ;;
    --init) INIT_SYSTEM="$2"; shift 2 ;;
    --purge-data) PURGE_DATA=1; shift ;;
    --dry-run) DRY_RUN=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "Unknown option: $1" >&2; usage; exit 2 ;;
  esac
done

run_cmd() {
  if (( DRY_RUN == 1 )); then
    echo "[dry-run] $*"
  else
    "$@"
  fi
}

detect_init() {
  if [[ "${INIT_SYSTEM}" != "auto" ]]; then
    echo "${INIT_SYSTEM}"
    return
  fi
  if command -v systemctl >/dev/null 2>&1 && [[ -d /run/systemd/system ]]; then
    echo "systemd"
  elif command -v rc-service >/dev/null 2>&1; then
    echo "openrc"
  elif command -v systemctl >/dev/null 2>&1; then
    echo "systemd"
  else
    echo "unknown"
  fi
}

require_root_unless_dry() {
  if (( DRY_RUN == 0 )) && [[ "${EUID:-$(id -u)}" -ne 0 ]]; then
    echo "Error: root privileges are required (run with sudo, or pass --dry-run to preview)." >&2
    exit 1
  fi
}

generate_secret_hex() {
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -hex 32
  else
    head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n'
  fi
}

ensure_user_and_group() {
  if (( DRY_RUN == 1 )); then
    echo "[dry-run] ensure system group '${SERVICE_GROUP}' and user '${SERVICE_USER}'"
    return
  fi
  if ! getent group "${SERVICE_GROUP}" >/dev/null 2>&1; then
    if command -v groupadd >/dev/null 2>&1; then
      groupadd --system "${SERVICE_GROUP}"
    elif command -v addgroup >/dev/null 2>&1; then
      addgroup -S "${SERVICE_GROUP}"
    fi
  fi
  if ! id -u "${SERVICE_USER}" >/dev/null 2>&1; then
    if command -v useradd >/dev/null 2>&1; then
      useradd --system --gid "${SERVICE_GROUP}" --home-dir "${WORK_DIR}" --no-create-home --shell /usr/sbin/nologin "${SERVICE_USER}"
    elif command -v adduser >/dev/null 2>&1; then
      adduser -S -D -H -h "${WORK_DIR}" -s /sbin/nologin -G "${SERVICE_GROUP}" "${SERVICE_USER}"
    fi
  fi
}

write_env_if_missing() {
  if [[ -f "${ENV_FILE}" ]]; then
    echo "Keeping existing environment file: ${ENV_FILE}"
    return
  fi
  local secret
  secret="$(generate_secret_hex)"
  local content
  content="$(cat <<EOF
# Generated by install-service-linux.sh
VOLO_LISTEN_ADDR=${LISTEN_ADDR}
VOLO_LISTEN_SOCKET=${LISTEN_SOCKET}
VOLO_LISTEN_SOCKET_MODE=${SOCKET_MODE}
VOLO_PUBLIC_URL=${PUBLIC_URL}
VOLO_TRUSTED_PROXIES=${TRUSTED_PROXIES}
VOLO_ADMIN_PATH=${ADMIN_PATH}
VOLO_SECRET_KEY=${secret}
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
    echo "[dry-run] write ${ENV_FILE} (mode 0640, owner root:${SERVICE_GROUP})"
    echo "${content}" | sed 's/VOLO_SECRET_KEY=.*/VOLO_SECRET_KEY=<redacted-generated-hex>/'
  else
    mkdir -p "${CONF_DIR}"
    chmod 0750 "${CONF_DIR}"
    umask 0077
    printf '%s\n' "${content}" > "${ENV_FILE}"
    chown "root:${SERVICE_GROUP}" "${ENV_FILE}"
    chmod 0640 "${ENV_FILE}"
  fi
}

install_unit_systemd() {
  local unit_path="/etc/systemd/system/volo.service"
  local unit_body
  unit_body="$(cat <<EOF
[Unit]
Description=Volo-go Pure-Go Sovereign Blog Engine
Documentation=https://github.com/Willh92/Volo-go
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${SERVICE_USER}
Group=${SERVICE_GROUP}
WorkingDirectory=${WORK_DIR}
EnvironmentFile=-${ENV_FILE}
ExecStart=${INSTALL_BIN} serve
KillSignal=SIGTERM
TimeoutStopSec=20
Restart=on-failure
RestartSec=3
RuntimeDirectory=volo
RuntimeDirectoryMode=0755
LimitNOFILE=65536
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ProtectControlGroups=true
ProtectKernelModules=true
ProtectKernelTunables=true
RestrictSUIDSGID=true
LockPersonality=true
UMask=0027
ReadWritePaths=${WORK_DIR} /run/volo

[Install]
WantedBy=multi-user.target
EOF
)"
  if (( DRY_RUN == 1 )); then
    echo "[dry-run] write ${unit_path}:"
    echo "${unit_body}"
    echo "[dry-run] systemctl daemon-reload && systemctl enable --now volo.service"
  else
    printf '%s\n' "${unit_body}" > "${unit_path}"
    chmod 0644 "${unit_path}"
    systemctl daemon-reload
    systemctl enable --now volo.service
  fi
}

install_unit_openrc() {
  local init_path="/etc/init.d/volo"
  if (( DRY_RUN == 1 )); then
    echo "[dry-run] install OpenRC script to ${init_path} && rc-update add volo default && rc-service volo restart"
    return
  fi
  cp "${REPO_ROOT}/deploy/openrc/volo.initd" "${init_path}"
  sed -i "s|/usr/local/bin/volo|${INSTALL_BIN}|g" "${init_path}"
  sed -i "s|/var/lib/volo|${WORK_DIR}|g" "${init_path}"
  sed -i "s|/etc/volo/volo.env|${ENV_FILE}|g" "${init_path}"
  chmod 0755 "${init_path}"
  rc-update add volo default
  rc-service volo restart
}

TARGET_INIT="$(detect_init)"

case "${ACTION}" in
  install|upgrade)
    require_root_unless_dry
    if [[ ! -f "${BIN_SOURCE}" ]] && (( DRY_RUN == 0 )); then
      echo "Error: binary not found at '${BIN_SOURCE}'. Pass --binary /path/to/volo or build first." >&2
      exit 1
    fi
    if [[ "${TARGET_INIT}" == "unknown" ]]; then
      echo "Error: could not detect systemd or OpenRC. Pass --init systemd|openrc." >&2
      exit 1
    fi

    ensure_user_and_group
    run_cmd mkdir -p \
      "${WORK_DIR}/data/uploads" \
      "${WORK_DIR}/data/backups" \
      "${WORK_DIR}/data/geoip" \
      "${WORK_DIR}/plugins" \
      "${WORK_DIR}/themes" \
      "${WORK_DIR}/locales"

    if [[ -d "${REPO_ROOT}/plugins/kanbanniang" ]] && [[ ! -d "${WORK_DIR}/plugins/kanbanniang" ]]; then
      run_cmd cp -r "${REPO_ROOT}/plugins/kanbanniang" "${WORK_DIR}/plugins/kanbanniang"
    fi
    if [[ -d "${REPO_ROOT}/themes/volo-journal" ]] && [[ ! -d "${WORK_DIR}/themes/volo-journal" ]]; then
      run_cmd cp -r "${REPO_ROOT}/themes/volo-journal" "${WORK_DIR}/themes/volo-journal"
    fi

    run_cmd chown -R "${SERVICE_USER}:${SERVICE_GROUP}" "${WORK_DIR}"
    run_cmd chmod 0750 "${WORK_DIR}" "${WORK_DIR}/data"

    run_cmd install -m 0755 "${BIN_SOURCE}" "${INSTALL_BIN}"
    write_env_if_missing

    if [[ "${TARGET_INIT}" == "systemd" ]]; then
      if [[ "${ACTION}" == "upgrade" ]] && (( DRY_RUN == 0 )) && systemctl is-active --quiet volo.service; then
        systemctl restart volo.service
      else
        install_unit_systemd
      fi
    else
      install_unit_openrc
    fi
    echo "Volo-go ${ACTION} completed (${TARGET_INIT}). Run '${INSTALL_BIN} doctor --env-file ${ENV_FILE}' to verify."
    ;;

  status)
    if [[ "${TARGET_INIT}" == "systemd" ]]; then
      run_cmd systemctl status volo.service --no-pager
    elif [[ "${TARGET_INIT}" == "openrc" ]]; then
      run_cmd rc-service volo status
    else
      echo "Unknown init system." >&2
      exit 1
    fi
    ;;

  uninstall)
    require_root_unless_dry
    if [[ "${TARGET_INIT}" == "systemd" ]]; then
      run_cmd systemctl disable --now volo.service || true
      run_cmd rm -f /etc/systemd/system/volo.service
      run_cmd systemctl daemon-reload
    elif [[ "${TARGET_INIT}" == "openrc" ]]; then
      run_cmd rc-service volo stop || true
      run_cmd rc-update del volo default || true
      run_cmd rm -f /etc/init.d/volo
    fi
    run_cmd rm -f "${INSTALL_BIN}"
    if (( PURGE_DATA == 1 )); then
      run_cmd rm -rf "${WORK_DIR}" "${CONF_DIR}"
      echo "Volo-go service, binary, and data purged."
    else
      echo "Volo-go service uninstalled. Data preserved in ${WORK_DIR} and ${ENV_FILE} (pass --purge-data to remove)."
    fi
    ;;

  *)
    echo "Unknown action: ${ACTION}" >&2
    usage
    exit 2
    ;;
esac
