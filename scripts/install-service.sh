#!/usr/bin/env sh
# ==============================================================================
# Volo-go Cross-Platform Auto-Start Service Installer & Updater (Linux & macOS)
# Repository : https://github.com/Willh92/Volo-go
# License    : Apache-2.0
#
# Supports:
#   - Linux systemd (System unit /etc/systemd/system/volo.service
#                    or User unit ~/.config/systemd/user/volo.service)
#   - Linux OpenRC  (/etc/init.d/volo for Alpine / Gentoo)
#   - macOS launchd (System LaunchDaemon /Library/LaunchDaemons/io.volo.blog.plist
#                    or User LaunchAgent ~/Library/LaunchAgents/io.volo.blog.plist)
#
# Examples:
#   # One-line remote install (auto-downloads from GitHub Releases & verifies SHA-256):
#   curl -fsSL https://raw.githubusercontent.com/Willh92/Volo-go/main/scripts/install-service.sh | sudo sh
#
#   # One-command online upgrade / update (preserves /etc/volo/volo.env & data/):
#   sudo ./scripts/install-service.sh upgrade
#   curl -fsSL https://raw.githubusercontent.com/Willh92/Volo-go/main/scripts/install-service.sh | sudo sh -s -- upgrade
# ==============================================================================

set -eu
umask 022

SERVICE_NAME="volo"
MACOS_LABEL="io.volo.blog"

ACTION="install"
ACTION_SET="0"
BIN_SRC=""
ARCHIVE_SRC=""
INSTALL_DIR=""
INSTALL_BIN_OVERRIDE=""
INSTALL_BIN_EXPLICIT="0"
HOST_ADDR="127.0.0.1"
PORT="8080"
LISTEN_ADDR_EXPLICIT=""
HOST_PORT_EXPLICIT="0"
UNIX_SOCKET=""
UNIX_SOCKET_PERM="0660"
SOCKET_GROUP=""
DATA_DIR=""
DATA_DIR_EXPLICIT="0"
DATA_DIR_MODE="0750"
ENV_FILE_OVERRIDE=""
ENV_FILE_EXPLICIT="0"
PUBLIC_URL="http://localhost:8080"
ADMIN_PATH="/console"
TRUSTED_PROXIES="127.0.0.1/32,::1/128"
SVC_USER=""
USER_EXPLICIT="0"
SVC_GROUP=""
GROUP_EXPLICIT="0"
SUPP_GROUPS=""
USER_MODE="0"
FORCE_SYSTEM="0"
INIT_OVERRIDE="auto"
TARGET_OS_OVERRIDE="auto"
DRY_RUN="0"
PURGE_DATA="0"
SKIP_BACKUP="0"
NO_START="0"

GITHUB_REPO="${VOLO_REPO:-Willh92/Volo-go}"
RELEASE_VERSION="${VOLO_VERSION:-}"
RELEASE_CHANNEL="${VOLO_CHANNEL:-release}"
GITHUB_MIRROR="${VOLO_MIRROR:-}"
FORCE_DOWNLOAD="0"
SKIP_VERIFY="0"

RESOLVED_BIN_PATH=""
RESOLVED_BUNDLE_DIR=""
TMP_DOWNLOAD_DIR=""

cleanup_tmp() {
  if [ -n "$TMP_DOWNLOAD_DIR" ] && [ -d "$TMP_DOWNLOAD_DIR" ]; then
    rm -rf "$TMP_DOWNLOAD_DIR" 2>/dev/null || true
  fi
}
trap 'cleanup_tmp' EXIT INT TERM

log_info() {
  printf "\033[1;34m[Volo]\033[0m %s\n" "$*"
}

log_ok() {
  printf "\033[1;32m[Volo]\033[0m %s\n" "$*"
}

log_warn() {
  printf "\033[1;33m[Volo]\033[0m %s\n" "$*" >&2
}

log_err() {
  printf "\033[1;31m[Volo ERROR]\033[0m %s\n" "$*" >&2
}

usage() {
  cat <<'EOF'
Volo-go Service Manager (Linux systemd / OpenRC & macOS launchd)

Usage:
  install-service.sh [command] [options]

Commands:
  install      Install binary (local or auto-downloaded from GitHub Releases), register service, and start (default)
  upgrade      Backup DB, download/verify release, smoke-test, replace binary, healthcheck, and auto-rollback on failure (alias: update)
  uninstall    Stop and remove auto-start service registration (preserves data directory unless --purge-data)
  start        Start the Volo-go service
  stop         Stop the Volo-go service
  restart      Restart the Volo-go service
  status       Display current service status
  logs         Tail service runtime logs

Options:
  --bin <path>                    Path to local volo binary (skips GitHub download)
  --archive <path>                Path to local release .tar.gz bundle (skips GitHub download)
  --install-dir <dir>             Target installation directory for binary
  --download                      Force downloading from GitHub Releases even if a local binary exists
  --version <tag>                 GitHub Release version to download (default: latest; e.g. v1.0.0 or preview)
  --channel <release|preview>     Release channel when --version is omitted (default: release)
  --mirror <url>                  GitHub download mirror prefix (e.g. https://ghfast.top)
  --repo <owner/repo>             GitHub repository for release downloads (default: Willh92/Volo-go)
  --no-verify                     Skip SHA256SUMS.txt verification when downloading from GitHub
  --skip-backup                   Skip pre-upgrade database backup during 'upgrade'
  --no-start                      Do not start/restart service or poll healthcheck after install/upgrade
  --host <addr>                   TCP listen host address (default: 127.0.0.1)
  --port <port>                   TCP listen HTTP port (default: 8080)
  --listen-addr <host:port>       Explicit TCP listen address (overrides --host/--port)
  --unix-socket <path>            Listen on Unix domain socket instead of TCP (e.g. /run/volo/volo.sock)
  --socket-mode <octal>           Octal permissions for Unix domain socket (default: 0660; alias: --unix-socket-perm)
  --socket-group <group>          Group ownership for Unix socket access (e.g. www-data or nginx)
  --public-url <url>              Canonical public URL written on initial .env creation (default: http://localhost:8080)
  --admin-path <prefix>           Stealth admin console path written on initial .env creation (default: /console)
  --trusted-proxies <cidrs>       Trusted reverse-proxy CIDRs (default: 127.0.0.1/32,::1/128)
  --data-dir <dir>                Persistent root directory for data, plugins, themes, and locales
  --data-dir-mode <octal>         Octal permission mask for data directory (default: 0750)
  --env-file <path>               Custom path to volo.env configuration file
  --user <username>               Service execution user (Linux system default: volo)
  --group <groupname>             Primary permission group for service & data dir (Linux default: same as --user)
  --supplementary-groups <g1,g2>  Comma-separated supplementary groups for systemd service (e.g. www-data)
  --user-mode                     Install as current non-root user service (systemd --user / LaunchAgents)
  --system                        Force system-wide LaunchDaemon on macOS (/Library/LaunchDaemons)
  --init <auto|systemd|openrc>    Override Linux init system detection
  --purge-data                    With 'uninstall': also delete data directory and volo.env (destructive!)
  --dry-run                       Print planned actions and generated unit/plist without modifying host
  -h, --help                      Show this help message
EOF
}

require_arg() {
  if [ "$#" -lt 2 ]; then
    log_err "Option '$1' requires a value."
    exit 1
  fi
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    install|upgrade|update|uninstall|start|stop|restart|status|logs)
      if [ "$ACTION_SET" = "1" ]; then
        log_err "Multiple commands specified ('$ACTION' and '$1')."
        exit 1
      fi
      ACTION="$1"
      [ "$ACTION" = "update" ] && ACTION="upgrade"
      ACTION_SET="1"
      shift 1
      ;;
    help|-h|--help) usage; exit 0 ;;
    --bin|--binary) require_arg "$@"; BIN_SRC="$2"; shift 2 ;;
    --bin=*|--binary=*) BIN_SRC="${1#*=}"; shift 1 ;;
    --archive) require_arg "$@"; ARCHIVE_SRC="$2"; shift 2 ;;
    --archive=*) ARCHIVE_SRC="${1#*=}"; shift 1 ;;
    --install-dir) require_arg "$@"; INSTALL_DIR="$2"; INSTALL_BIN_EXPLICIT="1"; shift 2 ;;
    --install-dir=*) INSTALL_DIR="${1#*=}"; INSTALL_BIN_EXPLICIT="1"; shift 1 ;;
    --install-bin) require_arg "$@"; INSTALL_BIN_OVERRIDE="$2"; INSTALL_BIN_EXPLICIT="1"; shift 2 ;;
    --install-bin=*) INSTALL_BIN_OVERRIDE="${1#*=}"; INSTALL_BIN_EXPLICIT="1"; shift 1 ;;
    --download|--from-github) FORCE_DOWNLOAD="1"; shift 1 ;;
    --version) require_arg "$@"; RELEASE_VERSION="$2"; shift 2 ;;
    --version=*) RELEASE_VERSION="${1#*=}"; shift 1 ;;
    --channel) require_arg "$@"; RELEASE_CHANNEL="$2"; shift 2 ;;
    --channel=*) RELEASE_CHANNEL="${1#*=}"; shift 1 ;;
    --mirror|--github-proxy) require_arg "$@"; GITHUB_MIRROR="$2"; shift 2 ;;
    --mirror=*|--github-proxy=*) GITHUB_MIRROR="${1#*=}"; shift 1 ;;
    --repo) require_arg "$@"; GITHUB_REPO="$2"; shift 2 ;;
    --repo=*) GITHUB_REPO="${1#*=}"; shift 1 ;;
    --no-verify|--skip-verify) SKIP_VERIFY="1"; shift 1 ;;
    --skip-backup|--no-backup) SKIP_BACKUP="1"; shift 1 ;;
    --no-start) NO_START="1"; shift 1 ;;
    --host) require_arg "$@"; HOST_ADDR="$2"; HOST_PORT_EXPLICIT="1"; shift 2 ;;
    --host=*) HOST_ADDR="${1#*=}"; HOST_PORT_EXPLICIT="1"; shift 1 ;;
    --port) require_arg "$@"; PORT="$2"; HOST_PORT_EXPLICIT="1"; shift 2 ;;
    --port=*) PORT="${1#*=}"; HOST_PORT_EXPLICIT="1"; shift 1 ;;
    --listen-addr) require_arg "$@"; LISTEN_ADDR_EXPLICIT="$2"; HOST_PORT_EXPLICIT="1"; shift 2 ;;
    --listen-addr=*) LISTEN_ADDR_EXPLICIT="${1#*=}"; HOST_PORT_EXPLICIT="1"; shift 1 ;;
    --unix-socket|--listen-socket) require_arg "$@"; UNIX_SOCKET="$2"; shift 2 ;;
    --unix-socket=*|--listen-socket=*) UNIX_SOCKET="${1#*=}"; shift 1 ;;
    --unix-socket-perm|--socket-mode) require_arg "$@"; UNIX_SOCKET_PERM="$2"; shift 2 ;;
    --unix-socket-perm=*|--socket-mode=*) UNIX_SOCKET_PERM="${1#*=}"; shift 1 ;;
    --socket-group) require_arg "$@"; SOCKET_GROUP="$2"; shift 2 ;;
    --socket-group=*) SOCKET_GROUP="${1#*=}"; shift 1 ;;
    --public-url) require_arg "$@"; PUBLIC_URL="$2"; shift 2 ;;
    --public-url=*) PUBLIC_URL="${1#*=}"; shift 1 ;;
    --admin-path) require_arg "$@"; ADMIN_PATH="$2"; shift 2 ;;
    --admin-path=*) ADMIN_PATH="${1#*=}"; shift 1 ;;
    --trusted-proxies) require_arg "$@"; TRUSTED_PROXIES="$2"; shift 2 ;;
    --trusted-proxies=*) TRUSTED_PROXIES="${1#*=}"; shift 1 ;;
    --data-dir|--work-dir) require_arg "$@"; DATA_DIR="$2"; DATA_DIR_EXPLICIT="1"; shift 2 ;;
    --data-dir=*|--work-dir=*) DATA_DIR="${1#*=}"; DATA_DIR_EXPLICIT="1"; shift 1 ;;
    --data-dir-mode) require_arg "$@"; DATA_DIR_MODE="$2"; shift 2 ;;
    --data-dir-mode=*) DATA_DIR_MODE="${1#*=}"; shift 1 ;;
    --env-file) require_arg "$@"; ENV_FILE_OVERRIDE="$2"; ENV_FILE_EXPLICIT="1"; shift 2 ;;
    --env-file=*) ENV_FILE_OVERRIDE="${1#*=}"; ENV_FILE_EXPLICIT="1"; shift 1 ;;
    --user) require_arg "$@"; SVC_USER="$2"; USER_EXPLICIT="1"; shift 2 ;;
    --user=*) SVC_USER="${1#*=}"; USER_EXPLICIT="1"; shift 1 ;;
    --group) require_arg "$@"; SVC_GROUP="$2"; GROUP_EXPLICIT="1"; shift 2 ;;
    --group=*) SVC_GROUP="${1#*=}"; GROUP_EXPLICIT="1"; shift 1 ;;
    --supplementary-groups) require_arg "$@"; SUPP_GROUPS="$2"; shift 2 ;;
    --supplementary-groups=*) SUPP_GROUPS="${1#*=}"; shift 1 ;;
    --user-mode) USER_MODE="1"; shift 1 ;;
    --system) FORCE_SYSTEM="1"; USER_MODE="0"; shift 1 ;;
    --init) require_arg "$@"; INIT_OVERRIDE="$2"; shift 2 ;;
    --init=*) INIT_OVERRIDE="${1#*=}"; shift 1 ;;
    --target-os) require_arg "$@"; TARGET_OS_OVERRIDE="$2"; shift 2 ;;
    --target-os=*) TARGET_OS_OVERRIDE="${1#*=}"; shift 1 ;;
    --purge-data) PURGE_DATA="1"; shift 1 ;;
    --dry-run) DRY_RUN="1"; shift 1 ;;
    *)
      log_err "Unknown argument: $1"
      usage
      exit 1
      ;;
  esac
done

# Support passing --unix-socket="/path/to.sock,0660" or separate --unix-socket-perm
if [ -n "$UNIX_SOCKET" ]; then
  case "$UNIX_SOCKET" in
    *,*)
      UNIX_SOCKET_PERM="${UNIX_SOCKET##*,}"
      UNIX_SOCKET="${UNIX_SOCKET%,*}"
      ;;
  esac
  case ",${TRUSTED_PROXIES}," in
    *,unix,*) ;;
    *)
      if [ -n "$TRUSTED_PROXIES" ]; then
        TRUSTED_PROXIES="${TRUSTED_PROXIES},unix"
      else
        TRUSTED_PROXIES="unix"
      fi
      ;;
  esac
fi

if [ "$TARGET_OS_OVERRIDE" != "auto" ]; then
  OS_RAW="$TARGET_OS_OVERRIDE"
else
  OS_RAW="$(uname -s | tr '[:upper:]' '[:lower:]')"
fi
ARCH_RAW="$(uname -m)"

case "$OS_RAW" in
  linux*)  TARGET_OS="linux" ;;
  darwin*|macos*) TARGET_OS="darwin" ;;
  *)
    log_err "Unsupported OS: $OS_RAW (use scripts/install-service.ps1 on Windows)"
    exit 1
    ;;
esac

case "$ARCH_RAW" in
  x86_64|amd64)  TARGET_ARCH="amd64" ;;
  aarch64|arm64) TARGET_ARCH="arm64" ;;
  *)
    log_err "Unsupported CPU architecture: $ARCH_RAW"
    exit 1
    ;;
esac

IS_ROOT="0"
if [ "$(id -u)" -eq 0 ]; then
  IS_ROOT="1"
fi

if [ "$TARGET_OS" = "darwin" ] && [ "$IS_ROOT" = "0" ] && [ "$USER_MODE" = "0" ] && [ "$FORCE_SYSTEM" = "0" ]; then
  USER_MODE="1"
fi

if [ "$DRY_RUN" = "0" ]; then
  if [ "$TARGET_OS" = "linux" ] && [ "$IS_ROOT" = "0" ] && [ "$USER_MODE" = "0" ]; then
    log_err "System-wide service management on Linux requires root (sudo), or pass --user-mode."
    exit 1
  fi
  if [ "$TARGET_OS" = "darwin" ] && [ "$IS_ROOT" = "0" ] && [ "$USER_MODE" = "0" ]; then
    log_err "System-wide LaunchDaemon management on macOS requires sudo, or omit --system for LaunchAgent."
    exit 1
  fi
fi

if [ -n "$LISTEN_ADDR_EXPLICIT" ]; then
  LISTEN_ADDR="$LISTEN_ADDR_EXPLICIT"
else
  LISTEN_ADDR="${HOST_ADDR}:${PORT}"
fi

if [ -f "$0" ]; then
  SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")" 2>/dev/null && pwd || pwd)"
else
  SCRIPT_DIR="$(pwd)"
fi

normalize_dir_path() {
  RAW_P="$1"
  case "$RAW_P" in
    /) printf "/" ;;
    /*) printf "%s" "${RAW_P%/}" ;;
    *)
      if [ -d "$RAW_P" ]; then
        (CDPATH= cd -- "$RAW_P" && pwd)
      else
        CLEAN_REL="${RAW_P#./}"
        printf "%s/%s" "$(pwd)" "${CLEAN_REL%/}"
      fi
      ;;
  esac
}

validate_safe_purge_dir() {
  CAND_DIR="$1"
  if [ -z "$CAND_DIR" ]; then
    log_err "Refusing to purge an empty directory path."
    exit 1
  fi
  NORM_DIR="$(normalize_dir_path "$CAND_DIR")"
  HOME_NORM=""
  if [ -n "${HOME:-}" ]; then
    HOME_NORM="${HOME%/}"
  fi
  if [ -n "$HOME_NORM" ] && [ "$NORM_DIR" = "$HOME_NORM" ]; then
    log_err "Refusing to purge user home directory: ${NORM_DIR}"
    exit 1
  fi
  case "$NORM_DIR" in
    ""|/|/bin|/sbin|/etc|/var|/var/lib|/var/log|/run|/var/run|/usr|/usr/bin|/usr/sbin|/usr/local|/usr/local/bin|/opt|/home|/root|/tmp|/var/tmp|/Users|/Library|"/Library/Application Support"|/System)
      log_err "Refusing to purge protected system directory: ${NORM_DIR}"
      exit 1
      ;;
  esac
}

safe_remove_env_file() {
  TARGET_E="$1"
  [ -z "$TARGET_E" ] && return 0
  rm -f "$TARGET_E"
  ENV_PARENT="$(dirname "$TARGET_E")"
  if [ "$ENV_PARENT" = "/etc/volo" ]; then
    rmdir /etc/volo 2>/dev/null || true
  fi
}

xml_escape() {
  printf "%s" "$1" | sed \
    -e 's/&/\&amp;/g' \
    -e 's/</\&lt;/g' \
    -e 's/>/\&gt;/g' \
    -e 's/"/\&quot;/g' \
    -e "s/'/\&apos;/g"
}

openrc_escape_dq() {
  printf "%s" "$1" | sed \
    -e 's/\\/\\\\/g' \
    -e 's/"/\\"/g' \
    -e 's/\$/\\$/g' \
    -e 's/`/\\`/g'
}

case "$SERVICE_NAME" in
  *[!A-Za-z0-9._-]*|"")
    log_err "Invalid SERVICE_NAME '${SERVICE_NAME}': only [A-Za-z0-9._-] characters are allowed."
    exit 1
    ;;
esac

case "$DATA_DIR_MODE" in
  [0-7][0-7][0-7]|[0-7][0-7][0-7][0-7]) ;;
  *)
    log_err "Invalid --data-dir-mode '${DATA_DIR_MODE}': expected octal permissions such as 0750."
    exit 1
    ;;
esac

safe_chown_tree() {
  OWNER_SPEC="$1"
  shift
  for TARGET_P in "$@"; do
    [ -e "$TARGET_P" ] || continue
    if [ -L "$TARGET_P" ]; then
      log_warn "Skipping symlink root during recursive chown: ${TARGET_P}"
      continue
    fi
    chown -RP "$OWNER_SPEC" "$TARGET_P"
  done
}

safe_chgrp_tree() {
  GROUP_SPEC="$1"
  shift
  for TARGET_P in "$@"; do
    [ -e "$TARGET_P" ] || continue
    if [ -L "$TARGET_P" ]; then
      log_warn "Skipping symlink root during recursive chgrp: ${TARGET_P}"
      continue
    fi
    chgrp -RP "$GROUP_SPEC" "$TARGET_P"
  done
}

safe_chmod_dir() {
  MODE_SPEC="$1"
  shift
  for TARGET_P in "$@"; do
    if [ -d "$TARGET_P" ] && [ ! -L "$TARGET_P" ]; then
      chmod "$MODE_SPEC" "$TARGET_P"
    fi
  done
}

is_standard_bin_dir() {
  case "$1" in
    /bin|/sbin|/usr/bin|/usr/sbin|/usr/local/bin|/usr/local/sbin|*/.local/bin|*/bin|*/sbin)
      return 0
      ;;
    *)
      return 1
      ;;
  esac
}

generate_secret_hex() {
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -hex 32
  else
    od -An -N32 -tx1 /dev/urandom | tr -d ' \n'
  fi
}

copy_binary_safe() {
  SRC_BIN="$1"
  DST_BIN="$2"
  if [ "$DRY_RUN" = "1" ]; then
    printf "[dry-run] atomic install -m 0755 %s -> %s\n" "$SRC_BIN" "$DST_BIN"
    return 0
  fi
  DST_DIR="$(dirname "$DST_BIN")"
  mkdir -p "$DST_DIR"
  chmod 0755 "$DST_DIR" 2>/dev/null || true
  SRC_ABS="$(CDPATH= cd -- "$(dirname "$SRC_BIN")" && pwd)/$(basename "$SRC_BIN")"
  DST_ABS="$(CDPATH= cd -- "$DST_DIR" && pwd)/$(basename "$DST_BIN")"
  if [ "$SRC_ABS" != "$DST_ABS" ]; then
    TMP_DST="${DST_BIN}.new.$$"
    cp -f "$SRC_BIN" "$TMP_DST"
    chmod 0755 "$TMP_DST"
    mv -f "$TMP_DST" "$DST_BIN"
  else
    chmod 0755 "$DST_BIN"
  fi
}

read_env_var() {
  EF_PATH="$1"
  EF_KEY="$2"
  [ -z "$EF_PATH" ] && return 0
  [ ! -f "$EF_PATH" ] && return 0
  awk -v k="$EF_KEY" '
    /^[ \t]*#/ { next }
    {
      line = $0
      sub(/^[ \t]*(export[ \t]+)?/, "", line)
      eq = index(line, "=")
      if (eq > 0) {
        key = substr(line, 1, eq - 1)
        sub(/[ \t]+$/, "", key)
        if (key == k) {
          val = substr(line, eq + 1)
          sub(/^[ \t]+/, "", val)
          sub(/[ \t\r]+$/, "", val)
          if ((substr(val, 1, 1) == "\"" && substr(val, length(val), 1) == "\"") ||
              (substr(val, 1, 1) == "\047" && substr(val, length(val), 1) == "\047")) {
            val = substr(val, 2, length(val) - 2)
          }
          result = val
        }
      }
    }
    END { if (result != "") printf "%s", result }
  ' "$EF_PATH"
}

resolve_extension_dirs() {
  EXT_ROOT="$1"
  EXT_ENV="${2:-}"
  RESOLVED_THEMES_DIR=""
  RESOLVED_PLUGINS_DIR=""
  RESOLVED_LOCALES_DIR=""
  if [ -n "$EXT_ENV" ] && [ -f "$EXT_ENV" ]; then
    RESOLVED_THEMES_DIR="$(read_env_var "$EXT_ENV" "VOLO_THEMES_DIR")"
    RESOLVED_PLUGINS_DIR="$(read_env_var "$EXT_ENV" "VOLO_PLUGINS_DIR")"
    RESOLVED_LOCALES_DIR="$(read_env_var "$EXT_ENV" "VOLO_LOCALES_DIR")"
  fi
  if [ -z "$RESOLVED_THEMES_DIR" ]; then RESOLVED_THEMES_DIR="${EXT_ROOT}/themes"; fi
  if [ -z "$RESOLVED_PLUGINS_DIR" ]; then RESOLVED_PLUGINS_DIR="${EXT_ROOT}/plugins"; fi
  if [ -z "$RESOLVED_LOCALES_DIR" ]; then RESOLVED_LOCALES_DIR="${EXT_ROOT}/locales"; fi
  return 0
}

sync_extensions() {
  DEST_ROOT="$1"
  ENV_REF="${2:-}"
  [ -z "$RESOLVED_BUNDLE_DIR" ] && return 0
  [ ! -d "$RESOLVED_BUNDLE_DIR" ] && return 0

  resolve_extension_dirs "$DEST_ROOT" "$ENV_REF"

  for kind in plugins themes; do
    if [ "$kind" = "plugins" ]; then
      DEST_KIND_DIR="$RESOLVED_PLUGINS_DIR"
    else
      DEST_KIND_DIR="$RESOLVED_THEMES_DIR"
    fi
    if [ -d "${RESOLVED_BUNDLE_DIR}/${kind}" ]; then
      SRC_KIND_ABS="$(normalize_dir_path "${RESOLVED_BUNDLE_DIR}/${kind}")"
      DST_KIND_ABS="$(normalize_dir_path "$DEST_KIND_DIR")"
      [ "$SRC_KIND_ABS" = "$DST_KIND_ABS" ] && continue
      for item in "${RESOLVED_BUNDLE_DIR}/${kind}"/*; do
        [ -e "$item" ] || continue
        name="$(basename "$item")"
        if [ "$DRY_RUN" = "1" ]; then
          printf "[dry-run] sync official %s/%s -> %s/%s (preserving custom extensions)\n" "$kind" "$name" "$DEST_KIND_DIR" "$name"
        else
          mkdir -p "$DEST_KIND_DIR"
          STAGE_EXT="${DEST_KIND_DIR}/.${name}.sync.$$"
          rm -rf "$STAGE_EXT"
          cp -R "$item" "$STAGE_EXT"
          chmod -R a+rX "$STAGE_EXT" 2>/dev/null || true
          rm -rf "${DEST_KIND_DIR}/${name}"
          mv -f "$STAGE_EXT" "${DEST_KIND_DIR}/${name}"
        fi
      done
    fi
  done

  if [ -d "${RESOLVED_BUNDLE_DIR}/locales" ]; then
    SRC_LOC_ABS="$(normalize_dir_path "${RESOLVED_BUNDLE_DIR}/locales")"
    DST_LOC_ABS="$(normalize_dir_path "$RESOLVED_LOCALES_DIR")"
    if [ "$SRC_LOC_ABS" != "$DST_LOC_ABS" ]; then
      for loc in "${RESOLVED_BUNDLE_DIR}/locales"/*.json; do
        [ -f "$loc" ] || continue
        name="$(basename "$loc")"
        if [ "$DRY_RUN" = "1" ]; then
          printf "[dry-run] sync official locales/%s -> %s/%s\n" "$name" "$RESOLVED_LOCALES_DIR" "$name"
        else
          mkdir -p "$RESOLVED_LOCALES_DIR"
          STAGE_LOC="${RESOLVED_LOCALES_DIR}/.${name}.sync.$$"
          cp -f "$loc" "$STAGE_LOC"
          chmod 0644 "$STAGE_LOC" 2>/dev/null || true
          mv -f "$STAGE_LOC" "${RESOLVED_LOCALES_DIR}/${name}"
        fi
      done
    fi
  fi
}

sync_bundled_extensions() {
  sync_extensions "$@"
}

smoke_test_binary() {
  CAND_BIN="$1"
  if [ "$DRY_RUN" = "1" ]; then
    printf "[dry-run] smoke-test binary: '%s' version >/dev/null\n" "$CAND_BIN"
    return 0
  fi
  if [ ! -f "$CAND_BIN" ]; then
    log_err "Candidate binary not found: ${CAND_BIN}"
    exit 1
  fi
  if [ "$(uname -s | tr '[:upper:]' '[:lower:]')" = "$TARGET_OS" ]; then
    if [ ! -x "$CAND_BIN" ]; then
      chmod 0755 "$CAND_BIN" 2>/dev/null || true
    fi
    if [ "$TARGET_OS" = "darwin" ]; then
      xattr -d com.apple.quarantine "$CAND_BIN" >/dev/null 2>&1 || true
    fi
    if ! "$CAND_BIN" version >/dev/null 2>&1; then
      log_err "Candidate binary failed smoke-test ('${CAND_BIN} version'); aborting before modifying live binary."
      exit 1
    fi
  fi
}

perform_pre_upgrade_backup() {
  LIVE_BIN="$1"
  ROOT_DATA_DIR="$2"
  CFG_ENV_FILE="$3"
  CHOWN_SPEC="$4"

  if [ "$SKIP_BACKUP" = "1" ]; then
    log_info "Skipping pre-upgrade database backup (--skip-backup specified)."
    return 0
  fi

  EFF_DATA_DIR=""
  EFF_BACKUP_DIR=""
  CFG_DB_DSN=""
  if [ -n "$CFG_ENV_FILE" ] && [ -f "$CFG_ENV_FILE" ]; then
    EFF_DATA_DIR="$(read_env_var "$CFG_ENV_FILE" "VOLO_DATA_DIR")"
    EFF_BACKUP_DIR="$(read_env_var "$CFG_ENV_FILE" "VOLO_BACKUP_DIR")"
    CFG_DB_DSN="$(read_env_var "$CFG_ENV_FILE" "VOLO_DB_DSN")"
  fi
  if [ -z "$EFF_DATA_DIR" ]; then
    if [ -d "${ROOT_DATA_DIR}/data" ] || [ "$DRY_RUN" = "1" ]; then
      EFF_DATA_DIR="${ROOT_DATA_DIR}/data"
    else
      EFF_DATA_DIR="$ROOT_DATA_DIR"
    fi
  fi
  [ -z "$EFF_BACKUP_DIR" ] && EFF_BACKUP_DIR="${EFF_DATA_DIR}/backups"

  if [ "$DRY_RUN" = "1" ]; then
    printf "[dry-run] pre-upgrade backup: '%s' backup create --data-dir '%s' --backup-dir '%s'\n" "$LIVE_BIN" "$EFF_DATA_DIR" "$EFF_BACKUP_DIR"
    return 0
  fi

  if [ ! -x "$LIVE_BIN" ]; then
    log_info "No existing executable at ${LIVE_BIN}; skipping pre-upgrade backup."
    return 0
  fi

  if [ ! -f "${EFF_DATA_DIR}/volo.db" ] && [ ! -f "${EFF_DATA_DIR}/db.json" ] && [ -z "$CFG_DB_DSN" ]; then
    log_info "Database in ${EFF_DATA_DIR} is not initialized yet; skipping pre-upgrade backup."
    return 0
  fi

  mkdir -p "$EFF_BACKUP_DIR" 2>/dev/null || true
  log_info "Creating pre-upgrade database backup in ${EFF_BACKUP_DIR}..."
  BACKUP_RC="0"
  if [ -n "$CFG_ENV_FILE" ] && [ -f "$CFG_ENV_FILE" ]; then
    "$LIVE_BIN" backup create --env-file "$CFG_ENV_FILE" --data-dir "$EFF_DATA_DIR" --backup-dir "$EFF_BACKUP_DIR" >/dev/null 2>&1 || BACKUP_RC="$?"
  else
    "$LIVE_BIN" backup create --data-dir "$EFF_DATA_DIR" --backup-dir "$EFF_BACKUP_DIR" >/dev/null 2>&1 || BACKUP_RC="$?"
  fi
  if [ "$BACKUP_RC" = "0" ]; then
    if [ -n "$CHOWN_SPEC" ]; then
      safe_chown_tree "$CHOWN_SPEC" "$EFF_BACKUP_DIR" 2>/dev/null || true
    fi
    log_ok "Pre-upgrade backup saved in ${EFF_BACKUP_DIR}"
  else
    log_warn "Pre-upgrade backup returned exit code ${BACKUP_RC} (database may be uninitialized or unreachable); preserving existing data and continuing."
  fi
}

backup_live_binary() {
  LIVE_BIN="$1"
  HAD_PREV_BIN="0"
  if [ "$DRY_RUN" = "1" ]; then
    printf "[dry-run] backup live binary: cp -pf '%s' '%s.bak'\n" "$LIVE_BIN" "$LIVE_BIN"
    return 0
  fi
  if [ -f "$LIVE_BIN" ]; then
    cp -pf "$LIVE_BIN" "${LIVE_BIN}.bak" 2>/dev/null || cp -f "$LIVE_BIN" "${LIVE_BIN}.bak"
    chmod 0755 "${LIVE_BIN}.bak" 2>/dev/null || true
    HAD_PREV_BIN="1"
  fi
}

normalize_probe_addr() {
  RAW_LISTEN="$1"
  case "$RAW_LISTEN" in
    :*)           printf "127.0.0.1%s" "$RAW_LISTEN" ;;
    0.0.0.0:*)    printf "127.0.0.1:%s" "${RAW_LISTEN#0.0.0.0:}" ;;
    \[::\]:*)     printf "127.0.0.1:%s" "${RAW_LISTEN#\[::\]:}" ;;
    :::*)         printf "127.0.0.1:%s" "${RAW_LISTEN#:::}" ;;
    \*:*)         printf "127.0.0.1:%s" "${RAW_LISTEN#\*:}" ;;
    "")           printf "127.0.0.1:8080" ;;
    *)            printf "%s" "$RAW_LISTEN" ;;
  esac
}

poll_service_healthcheck() {
  LIVE_BIN="$1"
  CFG_ENV_FILE="$2"

  ACTIVE_SOCK="$UNIX_SOCKET"
  if [ -z "$ACTIVE_SOCK" ] && [ -n "$CFG_ENV_FILE" ] && [ -f "$CFG_ENV_FILE" ]; then
    ACTIVE_SOCK="$(read_env_var "$CFG_ENV_FILE" "VOLO_LISTEN_SOCKET")"
  fi
  ACTIVE_SOCK="${ACTIVE_SOCK%,*}"

  CFG_LISTEN=""
  if [ "$HOST_PORT_EXPLICIT" = "1" ]; then
    CFG_LISTEN="$LISTEN_ADDR"
  elif [ -n "$CFG_ENV_FILE" ] && [ -f "$CFG_ENV_FILE" ]; then
    CFG_LISTEN="$(read_env_var "$CFG_ENV_FILE" "VOLO_LISTEN_ADDR")"
  fi
  [ -z "$CFG_LISTEN" ] && CFG_LISTEN="$LISTEN_ADDR"
  PROBE_HOST_PORT="$(normalize_probe_addr "$CFG_LISTEN")"
  HC_URL="http://${PROBE_HOST_PORT}/healthz"

  if [ "$DRY_RUN" = "1" ]; then
    if [ -n "$ACTIVE_SOCK" ]; then
      printf "[dry-run] poll '%s' healthcheck --listen-socket '%s' (up to 15s; auto-rollback %s.bak on failure)\n" "$LIVE_BIN" "$ACTIVE_SOCK" "$LIVE_BIN"
    else
      printf "[dry-run] poll '%s' healthcheck --url '%s' (up to 15s; auto-rollback %s.bak on failure)\n" "$LIVE_BIN" "$HC_URL" "$LIVE_BIN"
    fi
    return 0
  fi

  ATTEMPT=1
  while [ "$ATTEMPT" -le 15 ]; do
    if [ -n "$ACTIVE_SOCK" ]; then
      if (HTTP_PROXY="" HTTPS_PROXY="" http_proxy="" https_proxy="" NO_PROXY="*" no_proxy="*" "$LIVE_BIN" healthcheck --listen-socket "$ACTIVE_SOCK" >/dev/null 2>&1); then
        return 0
      fi
    else
      if (HTTP_PROXY="" HTTPS_PROXY="" http_proxy="" https_proxy="" NO_PROXY="*" no_proxy="*" "$LIVE_BIN" healthcheck --url "$HC_URL" >/dev/null 2>&1); then
        return 0
      fi
    fi
    sleep 1
    ATTEMPT=$((ATTEMPT + 1))
  done
  return 1
}

build_release_base_url() {
  REL_REF="$1"
  if [ "$REL_REF" = "latest" ]; then
    RAW_BASE="https://github.com/${GITHUB_REPO}/releases/latest/download"
  else
    RAW_BASE="https://github.com/${GITHUB_REPO}/releases/download/${REL_REF}"
  fi
  if [ -n "$GITHUB_MIRROR" ]; then
    printf "%s/%s" "${GITHUB_MIRROR%/}" "$RAW_BASE"
  else
    printf "%s" "$RAW_BASE"
  fi
}

fetch_url() {
  DL_URL="$1"
  DL_DEST="$2"
  DL_QUIET="$3"
  if command -v curl >/dev/null 2>&1; then
    if [ "$DL_QUIET" = "1" ]; then
      curl -fsSL --retry 3 --connect-timeout 15 -o "$DL_DEST" "$DL_URL"
    else
      curl -fL --progress-bar --retry 3 --connect-timeout 15 -o "$DL_DEST" "$DL_URL"
    fi
  elif command -v wget >/dev/null 2>&1; then
    if [ "$DL_QUIET" = "1" ]; then
      wget -q --tries=3 --timeout=15 -O "$DL_DEST" "$DL_URL"
    else
      wget --tries=3 --timeout=15 -O "$DL_DEST" "$DL_URL"
    fi
  else
    log_err "Neither 'curl' nor 'wget' is installed. Install curl/wget or pass a local binary via --bin <path>."
    exit 1
  fi
}

compute_sha256() {
  FILE_TO_HASH="$1"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$FILE_TO_HASH" | awk '{print tolower($1)}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$FILE_TO_HASH" | awk '{print tolower($1)}'
  elif command -v openssl >/dev/null 2>&1; then
    openssl dgst -sha256 "$FILE_TO_HASH" | awk '{print tolower($NF)}'
  else
    printf ""
  fi
}

extract_tar_bundle() {
  TAR_FILE="$1"
  DEST_STAGE="$2"
  mkdir -p "$DEST_STAGE"
  if ! tar --no-same-owner --no-same-permissions -xzf "$TAR_FILE" -C "$DEST_STAGE" 2>/dev/null; then
    tar -o -xzf "$TAR_FILE" -C "$DEST_STAGE"
  fi
  [ ! -L "$DEST_STAGE" ] && chmod -R a-s,go-w "$DEST_STAGE" 2>/dev/null || true
  FOUND_BIN=""
  if [ -f "${DEST_STAGE}/volo" ]; then
    FOUND_BIN="${DEST_STAGE}/volo"
    RESOLVED_BUNDLE_DIR="$DEST_STAGE"
  else
    for sub in "$DEST_STAGE"/*; do
      if [ -d "$sub" ] && [ -f "${sub}/volo" ]; then
        FOUND_BIN="${sub}/volo"
        RESOLVED_BUNDLE_DIR="$sub"
        break
      fi
    done
  fi
  if [ -z "$FOUND_BIN" ]; then
    log_err "Archive '${TAR_FILE}' does not contain a 'volo' executable."
    exit 1
  fi
  chmod 0755 "$FOUND_BIN"
  if [ "$TARGET_OS" = "darwin" ]; then
    xattr -d com.apple.quarantine "$FOUND_BIN" >/dev/null 2>&1 || true
  fi
  RESOLVED_BIN_PATH="$FOUND_BIN"
}

resolve_latest_tag_fallback() {
  TAG_URL="https://github.com/${GITHUB_REPO}/releases/latest"
  [ -n "$GITHUB_MIRROR" ] && TAG_URL="${GITHUB_MIRROR%/}/${TAG_URL}"
  if command -v curl >/dev/null 2>&1; then
    LOC_HDR="$(curl -fsSI --connect-timeout 10 "$TAG_URL" 2>/dev/null | awk 'tolower($1) == "location:" { sub(/\r$/, "", $2); print $2; exit }' || true)"
    case "$LOC_HDR" in
      */releases/tag/*)
        printf "%s" "${LOC_HDR##*/releases/tag/}" | sed 's/[?#].*$//'
        return 0
        ;;
    esac
    EFF_URL="$(curl -fsSL --connect-timeout 10 -o /dev/null -w '%{url_effective}' "$TAG_URL" 2>/dev/null || true)"
    case "$EFF_URL" in
      */releases/tag/*)
        printf "%s" "${EFF_URL##*/releases/tag/}" | sed 's/[?#].*$//'
        return 0
        ;;
    esac
  elif command -v wget >/dev/null 2>&1; then
    LOC_HDR="$(wget -S --spider --max-redirect=0 --timeout=10 "$TAG_URL" 2>&1 | awk 'tolower($1) == "location:" { sub(/\r$/, "", $2); print $2; exit }' || true)"
    case "$LOC_HDR" in
      */releases/tag/*)
        printf "%s" "${LOC_HDR##*/releases/tag/}" | sed 's/[?#].*$//'
        return 0
        ;;
    esac
  fi
  printf ""
}

resolve_preview_release_tag() {
  API_URL="https://api.github.com/repos/${GITHUB_REPO}/releases?per_page=10"
  RAW_JSON=""
  if command -v curl >/dev/null 2>&1; then
    RAW_JSON="$(curl -fsSL --connect-timeout 10 -H "Accept: application/vnd.github+json" -H "User-Agent: Volo-go-Installer" "$API_URL" 2>/dev/null || true)"
  elif command -v wget >/dev/null 2>&1; then
    RAW_JSON="$(wget -qO- --timeout=10 --header="Accept: application/vnd.github+json" --header="User-Agent: Volo-go-Installer" "$API_URL" 2>/dev/null || true)"
  fi
  [ -z "$RAW_JSON" ] && return 0

  if command -v python3 >/dev/null 2>&1; then
    PARSED_TAG="$(printf "%s" "$RAW_JSON" | python3 -c '
import json, sys
try:
    data = json.load(sys.stdin)
    if isinstance(data, list):
        for rel in data:
            if isinstance(rel, dict) and not rel.get("draft") and rel.get("tag_name"):
                print(rel["tag_name"])
                break
except Exception:
    pass
' 2>/dev/null || true)"
    if [ -n "$PARSED_TAG" ]; then
      printf "%s" "$PARSED_TAG"
      return 0
    fi
  fi

  printf "%s" "$RAW_JSON" | awk '
    BEGIN { in_str = 0; esc = 0; depth = 0; cur_str = ""; last_key = ""; tag = ""; draft = "false"; expect_val = 0 }
    {
      n = length($0)
      for (i = 1; i <= n; i++) {
        c = substr($0, i, 1)
        if (in_str) {
          if (esc) { cur_str = cur_str c; esc = 0 }
          else if (c == "\\") { esc = 1 }
          else if (c == "\"") {
            in_str = 0
            if (depth == 1) {
              if (expect_val) {
                if (last_key == "tag_name" && tag == "") tag = cur_str
                expect_val = 0
              } else {
                last_key = cur_str
              }
            }
            cur_str = ""
          } else {
            cur_str = cur_str c
          }
        } else {
          if (c == "\"") { in_str = 1; cur_str = "" }
          else if (c == "{") {
            depth++
            if (depth == 1) { tag = ""; draft = "false"; last_key = ""; expect_val = 0 }
          } else if (c == "}") {
            if (depth == 1) {
              if (tag != "" && draft != "true") { print tag; exit }
            }
            depth--
          } else if (c == ":" && depth == 1) {
            expect_val = 1
          } else if (c == "," && depth == 1) {
            expect_val = 0
          } else if (depth == 1 && expect_val && last_key == "draft") {
            if (substr($0, i, 4) == "true") { draft = "true"; expect_val = 0 }
            else if (substr($0, i, 5) == "false") { draft = "false"; expect_val = 0 }
          }
        }
      }
    }
  '
}

download_release_bundle() {
  PINNED="0"
  WANT_PREVIEW="0"
  if [ -n "$RELEASE_VERSION" ]; then
    case "$RELEASE_VERSION" in
      latest)  PRIMARY_TARGET="latest" ;;
      preview) PRIMARY_TARGET="preview"; WANT_PREVIEW="1" ;;
      v*|V*)   PRIMARY_TARGET="v${RELEASE_VERSION#[vV]}"; PINNED="1" ;;
      *)       PRIMARY_TARGET="v${RELEASE_VERSION}"; PINNED="1" ;;
    esac
  else
    case "$RELEASE_CHANNEL" in
      preview)     PRIMARY_TARGET="preview"; WANT_PREVIEW="1" ;;
      release|"")  PRIMARY_TARGET="latest" ;;
      *)
        log_err "Invalid --channel '${RELEASE_CHANNEL}' (expected 'release' or 'preview')"
        exit 1
        ;;
    esac
  fi

  STABLE_ASSET="volo-${TARGET_OS}-${TARGET_ARCH}.tar.gz"
  if [ "$DRY_RUN" = "1" ]; then
    if [ "$WANT_PREVIEW" = "1" ]; then
      printf "[dry-run] query /repos/%s/releases?per_page=10 for newest release/prerelease (fallback: latest)\n" "$GITHUB_REPO"
    fi
    printf "[dry-run] download %s from %s (%s) + verify SHA256SUMS.txt\n" "$STABLE_ASSET" "$GITHUB_REPO" "$PRIMARY_TARGET"
    RESOLVED_BIN_PATH="/tmp/volo-dry-run-bin"
    return 0
  fi

  if [ "$WANT_PREVIEW" = "1" ]; then
    PREVIEW_TAG="$(resolve_preview_release_tag)"
    if [ -n "$PREVIEW_TAG" ]; then
      log_info "Preview channel resolved to newest release tag: ${PREVIEW_TAG}"
      PRIMARY_TARGET="$PREVIEW_TAG"
      PINNED="1"
    else
      log_info "No preview/prerelease tag returned from /releases?per_page=10 (or API rate-limited); falling back cleanly to 'latest'."
      PRIMARY_TARGET="latest"
      PINNED="0"
    fi
  fi

  TMP_DOWNLOAD_DIR="$(mktemp -d "${TMPDIR:-/tmp}/volo-dl.XXXXXX")"
  BASE_URL="$(build_release_base_url "$PRIMARY_TARGET")"
  DOWNLOADED_ASSET="$STABLE_ASSET"
  DL_ARC="${TMP_DOWNLOAD_DIR}/${DOWNLOADED_ASSET}"
  DL_SUM="${TMP_DOWNLOAD_DIR}/SHA256SUMS.txt"

  log_info "Downloading ${DOWNLOADED_ASSET} from GitHub Releases (${GITHUB_REPO} @ ${PRIMARY_TARGET})..."
  if ! fetch_url "${BASE_URL}/${DOWNLOADED_ASSET}" "$DL_ARC" "0"; then
    # Fallback 1: check version-tagged archive name (volo-vX.Y.Z-os-arch.tar.gz)
    TAG_CAND="$PRIMARY_TARGET"
    if [ "$TAG_CAND" = "latest" ]; then
      TAG_CAND="$(resolve_latest_tag_fallback)"
    fi
    FALLBACK_OK="0"
    if [ -n "$TAG_CAND" ] && [ "$TAG_CAND" != "latest" ]; then
      BASE_URL="$(build_release_base_url "$TAG_CAND")"
      DOWNLOADED_ASSET="volo-${TAG_CAND}-${TARGET_OS}-${TARGET_ARCH}.tar.gz"
      DL_ARC="${TMP_DOWNLOAD_DIR}/${DOWNLOADED_ASSET}"
      if fetch_url "${BASE_URL}/${DOWNLOADED_ASSET}" "$DL_ARC" "0"; then
        FALLBACK_OK="1"
      fi
    fi
    if [ "$FALLBACK_OK" = "0" ]; then
      log_err "Failed to download release archive for ${TARGET_OS}/${TARGET_ARCH} from ${GITHUB_REPO} (${PRIMARY_TARGET})."
      exit 1
    fi
  fi

  if [ ! -s "$DL_ARC" ]; then
    log_err "Downloaded archive is empty: ${DL_ARC}"
    exit 1
  fi

  if [ "$SKIP_VERIFY" != "1" ]; then
    SUM_URL="${BASE_URL}/SHA256SUMS.txt"
    if ! fetch_url "$SUM_URL" "$DL_SUM" "1" || [ ! -s "$DL_SUM" ]; then
      log_err "Failed to fetch SHA256SUMS.txt from ${SUM_URL} (refusing to install unverified archive; pass --no-verify only for custom mirrors without checksums)."
      exit 1
    fi
    EXPECTED_SHA="$(awk -v f="$DOWNLOADED_ASSET" '$2 == f || $2 == ("*" f) { print tolower($1); exit }' "$DL_SUM")"
    if [ -z "$EXPECTED_SHA" ]; then
      log_err "Entry for ${DOWNLOADED_ASSET} not found in SHA256SUMS.txt (pass --no-verify only if intentional)."
      exit 1
    fi
    ACTUAL_SHA="$(compute_sha256 "$DL_ARC")"
    if [ -z "$ACTUAL_SHA" ]; then
      log_err "No SHA-256 utility (sha256sum, shasum, or openssl) found on host; install one or pass --no-verify."
      exit 1
    fi
    if [ "$ACTUAL_SHA" != "$EXPECTED_SHA" ]; then
      log_err "SHA-256 mismatch for ${DOWNLOADED_ASSET}! Expected ${EXPECTED_SHA}, got ${ACTUAL_SHA}"
      exit 1
    fi
    log_ok "Verified SHA-256 checksum (${ACTUAL_SHA})"
  else
    log_warn "Skipping SHA-256 checksum verification (--no-verify specified)."
  fi

  extract_tar_bundle "$DL_ARC" "${TMP_DOWNLOAD_DIR}/extracted"
  smoke_test_binary "$RESOLVED_BIN_PATH"
}

resolve_source_binary() {
  if [ -n "$BIN_SRC" ]; then
    if [ "$DRY_RUN" = "0" ] && [ ! -f "$BIN_SRC" ]; then
      log_err "Specified binary not found: $BIN_SRC"
      exit 1
    fi
    RESOLVED_BIN_PATH="$BIN_SRC"
    CAND_ROOT="$(CDPATH= cd -- "$(dirname "$BIN_SRC")" 2>/dev/null && pwd || pwd)"
    if [ -d "${CAND_ROOT}/themes" ] || [ -d "${CAND_ROOT}/plugins" ] || [ -d "${CAND_ROOT}/locales" ]; then
      RESOLVED_BUNDLE_DIR="$CAND_ROOT"
    elif [ -d "${SCRIPT_DIR}/../themes" ]; then
      RESOLVED_BUNDLE_DIR="$(CDPATH= cd -- "${SCRIPT_DIR}/.." && pwd)"
    fi
    return
  fi

  if [ -n "$ARCHIVE_SRC" ]; then
    if [ "$DRY_RUN" = "1" ] && [ ! -f "$ARCHIVE_SRC" ]; then
      printf "[dry-run] extract archive '%s'\n" "$ARCHIVE_SRC"
      RESOLVED_BIN_PATH="/tmp/volo-dry-run-arc-bin"
      return
    fi
    if [ ! -f "$ARCHIVE_SRC" ]; then
      log_err "Specified archive not found: $ARCHIVE_SRC"
      exit 1
    fi
    TMP_DOWNLOAD_DIR="$(mktemp -d "${TMPDIR:-/tmp}/volo-arc.XXXXXX")"
    extract_tar_bundle "$ARCHIVE_SRC" "${TMP_DOWNLOAD_DIR}/extracted"
    return
  fi

  if [ "$FORCE_DOWNLOAD" = "1" ] || [ -n "$RELEASE_VERSION" ] || [ "$ACTION" = "upgrade" ]; then
    download_release_bundle
    return
  fi

  CANDIDATE_NAME="volo-${TARGET_OS}-${TARGET_ARCH}"
  for cand in \
    "${SCRIPT_DIR}/${CANDIDATE_NAME}" \
    "${SCRIPT_DIR}/volo" \
    "${SCRIPT_DIR}/../volo" \
    "${SCRIPT_DIR}/../dist/${CANDIDATE_NAME}" \
    "./${CANDIDATE_NAME}" \
    "./volo" \
    "./dist/${CANDIDATE_NAME}"
  do
    if [ -n "$cand" ] && [ -f "$cand" ]; then
      log_info "Using local binary: ${cand}"
      RESOLVED_BIN_PATH="$cand"
      CAND_ROOT="$(CDPATH= cd -- "$(dirname "$cand")" 2>/dev/null && pwd || pwd)"
      if [ -d "${CAND_ROOT}/themes" ] || [ -d "${CAND_ROOT}/plugins" ] || [ -d "${CAND_ROOT}/locales" ]; then
        RESOLVED_BUNDLE_DIR="$CAND_ROOT"
      elif [ -d "${SCRIPT_DIR}/../themes" ]; then
        RESOLVED_BUNDLE_DIR="$(CDPATH= cd -- "${SCRIPT_DIR}/.." && pwd)"
      fi
      return
    fi
  done

  if [ "$DRY_RUN" = "1" ] && [ -d "${SCRIPT_DIR}/../cmd/volo" ]; then
    RESOLVED_BIN_PATH="${SCRIPT_DIR}/../volo"
    RESOLVED_BUNDLE_DIR="$(CDPATH= cd -- "${SCRIPT_DIR}/.." && pwd)"
    return
  fi

  log_info "No local binary found; auto-downloading from GitHub Releases..."
  download_release_bundle
}

systemd_unescape_arg() {
  RAW_ARG="$1"
  case "$RAW_ARG" in
    \"*\")
      INNER="${RAW_ARG#\"}"
      INNER="${INNER%\"}"
      printf "%s" "$INNER" | sed 's/\\"/"/g; s/\\\\/\\/g'
      ;;
    \'*\')
      INNER="${RAW_ARG#\'}"
      printf "%s" "${INNER%\'}"
      ;;
    *)
      printf "%s" "$RAW_ARG"
      ;;
  esac
}

extract_systemd_exec_bin() {
  U_FILE="$1"
  if [ -f "$U_FILE" ]; then
    RAW_BIN="$(awk '
      /^ExecStart=/ {
        line = substr($0, 11)
        sub(/^[ \t@+-]+/, "", line)
        if (substr(line, 1, 1) == "\"") {
          out = ""
          esc = 0
          for (i = 2; i <= length(line); i++) {
            ch = substr(line, i, 1)
            if (esc) {
              out = out ch
              esc = 0
            } else if (ch == "\\") {
              esc = 1
            } else if (ch == "\"") {
              break
            } else {
              out = out ch
            }
          }
          print out
          exit
        } else if (substr(line, 1, 1) == "\047") {
          sub(/^\047/, "", line)
          sub(/\047.*$/, "", line)
          print line
          exit
        } else {
          sub(/[ \t].*$/, "", line)
          print line
          exit
        }
      }
    ' "$U_FILE")"
    printf "%s" "$RAW_BIN"
  fi
}

extract_systemd_field() {
  U_FILE="$1"
  F_KEY="$2"
  [ ! -f "$U_FILE" ] && return 0
  RAW_VAL="$(sed -n "s/^${F_KEY}=//p" "$U_FILE" | head -n 1)"
  [ -z "$RAW_VAL" ] && return 0
  if [ "$F_KEY" = "EnvironmentFile" ]; then
    RAW_VAL="${RAW_VAL#-}"
  fi
  systemd_unescape_arg "$RAW_VAL"
}

recover_systemd_unit_config() {
  U_FILE="$1"
  [ ! -f "$U_FILE" ] && return 0
  if [ "$INSTALL_BIN_EXPLICIT" = "0" ]; then
    EXISTING_BIN="$(extract_systemd_exec_bin "$U_FILE")"
    [ -n "$EXISTING_BIN" ] && INSTALL_BIN="$EXISTING_BIN"
  fi
  if [ "$DATA_DIR_EXPLICIT" = "0" ]; then
    EXISTING_WD="$(extract_systemd_field "$U_FILE" "WorkingDirectory")"
    [ -n "$EXISTING_WD" ] && DATA_DIR="$EXISTING_WD"
  fi
  if [ "$ENV_FILE_EXPLICIT" = "0" ]; then
    EXISTING_EF="$(extract_systemd_field "$U_FILE" "EnvironmentFile")"
    [ -n "$EXISTING_EF" ] && ENV_FILE="$EXISTING_EF"
  fi
  if [ "$USER_MODE" = "0" ]; then
    if [ "$USER_EXPLICIT" = "0" ]; then
      EXISTING_U="$(extract_systemd_field "$U_FILE" "User")"
      [ -n "$EXISTING_U" ] && SVC_USER="$EXISTING_U"
    fi
    if [ "$GROUP_EXPLICIT" = "0" ]; then
      EXISTING_G="$(extract_systemd_field "$U_FILE" "Group")"
      if [ -n "$EXISTING_G" ]; then SVC_GROUP="$EXISTING_G"; fi
    fi
  fi
  return 0
}

extract_openrc_env_file() {
  INIT_F="$1"
  [ ! -f "$INIT_F" ] && return 0
  awk '
    /^command_args="/ {
      line = substr($0, 15)
      sub(/"[ \t]*$/, "", line)
      gsub(/\\"/, "\"", line)
      idx = index(line, "--env-file ")
      if (idx > 0) {
        rest = substr(line, idx + 11)
        sub(/^[ \t]+/, "", rest)
        if (substr(rest, 1, 1) == "\"") {
          sub(/^"/, "", rest)
          sub(/".*$/, "", rest)
        } else {
          sub(/[ \t].*$/, "", rest)
        }
        print rest
        exit
      }
    }
  ' "$INIT_F"
}

recover_openrc_config() {
  INIT_F="$1"
  [ ! -f "$INIT_F" ] && return 0
  if [ "$INSTALL_BIN_EXPLICIT" = "0" ]; then
    EXISTING_BIN="$(sed -n 's/^command="\([^"]*\)".*/\1/p' "$INIT_F" | head -n 1)"
    [ -n "$EXISTING_BIN" ] && INSTALL_BIN="$EXISTING_BIN"
  fi
  if [ "$DATA_DIR_EXPLICIT" = "0" ]; then
    EXISTING_WD="$(sed -n 's/^directory="\([^"]*\)".*/\1/p' "$INIT_F" | head -n 1)"
    [ -n "$EXISTING_WD" ] && DATA_DIR="$EXISTING_WD"
  fi
  if [ "$ENV_FILE_EXPLICIT" = "0" ]; then
    EXISTING_EF="$(extract_openrc_env_file "$INIT_F")"
    [ -n "$EXISTING_EF" ] && ENV_FILE="$EXISTING_EF"
  fi
  EXISTING_CU="$(sed -n 's/^command_user="\([^"]*\)".*/\1/p' "$INIT_F" | head -n 1)"
  if [ -n "$EXISTING_CU" ]; then
    if [ "$USER_EXPLICIT" = "0" ]; then
      PARSED_U="${EXISTING_CU%%:*}"
      [ -n "$PARSED_U" ] && SVC_USER="$PARSED_U"
    fi
    case "$EXISTING_CU" in
      *:*)
        if [ "$GROUP_EXPLICIT" = "0" ]; then
          PARSED_G="${EXISTING_CU#*:}"
          if [ -n "$PARSED_G" ]; then SVC_GROUP="$PARSED_G"; fi
        fi
        ;;
    esac
  fi
  return 0
}

xml_unescape() {
  printf "%s" "$1" | sed \
    -e "s/&apos;/'/g" \
    -e 's/&quot;/"/g' \
    -e 's/&gt;/>/g' \
    -e 's/&lt;/</g' \
    -e 's/&amp;/\&/g'
}

read_plist_xml() {
  P_FILE="$1"
  [ ! -f "$P_FILE" ] && return 0
  PLIST_XML=""
  if command -v plutil >/dev/null 2>&1; then
    PLIST_XML="$(plutil -convert xml1 -o - "$P_FILE" 2>/dev/null || true)"
  fi
  [ -z "$PLIST_XML" ] && PLIST_XML="$(cat "$P_FILE" 2>/dev/null || true)"
  printf "%s\n" "$PLIST_XML"
}

extract_macos_plist_prog_arg() {
  P_FILE="$1"
  MODE_SEL="$2"
  [ ! -f "$P_FILE" ] && return 0
  RAW_OUT="$(read_plist_xml "$P_FILE" | awk -v mode="$MODE_SEL" '
    {
      line = $0
      if (!in_prog && match(line, /<key>[ \t]*ProgramArguments[ \t]*<\/key>/)) {
        in_prog = 1
        line = substr(line, RSTART + RLENGTH)
      }
      if (in_prog && !in_arr && match(line, /<array>/)) {
        in_arr = 1
        line = substr(line, RSTART + RLENGTH)
      }
      while (in_arr && match(line, /<string>[^<]*<\/string>/)) {
        val = substr(line, RSTART + 8, RLENGTH - 17)
        idx++
        if (mode == "bin" && idx == 1) {
          print val
          exit
        }
        if (mode == "env" && prev == "--env-file") {
          print val
          exit
        }
        prev = val
        line = substr(line, RSTART + RLENGTH)
      }
      if (in_arr && /<\/array>/) { exit }
    }
  ')"
  if [ -n "$RAW_OUT" ]; then xml_unescape "$RAW_OUT"; fi
  return 0
}

extract_macos_plist_key_string() {
  P_FILE="$1"
  K_NAME="$2"
  [ ! -f "$P_FILE" ] && return 0
  RAW_OUT="$(read_plist_xml "$P_FILE" | awk -v k="$K_NAME" '
    {
      line = $0
      pat = "<key>[ \t]*" k "[ \t]*</key>"
      if (!found_k && match(line, pat)) {
        found_k = 1
        line = substr(line, RSTART + RLENGTH)
      }
      if (found_k && match(line, /<string>[^<]*<\/string>/)) {
        print substr(line, RSTART + 8, RLENGTH - 17)
        exit
      }
    }
  ')"
  if [ -n "$RAW_OUT" ]; then xml_unescape "$RAW_OUT"; fi
  return 0
}

recover_macos_plist_config() {
  P_FILE="$1"
  [ ! -f "$P_FILE" ] && return 0
  if [ "$INSTALL_BIN_EXPLICIT" = "0" ]; then
    EXISTING_BIN="$(extract_macos_plist_prog_arg "$P_FILE" "bin")"
    [ -n "$EXISTING_BIN" ] && INSTALL_BIN="$EXISTING_BIN"
  fi
  if [ "$DATA_DIR_EXPLICIT" = "0" ]; then
    EXISTING_WD="$(extract_macos_plist_key_string "$P_FILE" "WorkingDirectory")"
    [ -n "$EXISTING_WD" ] && DATA_DIR="$EXISTING_WD"
  fi
  if [ "$ENV_FILE_EXPLICIT" = "0" ]; then
    EXISTING_EF="$(extract_macos_plist_prog_arg "$P_FILE" "env")"
    [ -n "$EXISTING_EF" ] && ENV_FILE="$EXISTING_EF"
  fi
  if [ "$USER_MODE" = "0" ]; then
    if [ "$USER_EXPLICIT" = "0" ]; then
      EXISTING_U="$(extract_macos_plist_key_string "$P_FILE" "UserName")"
      [ -n "$EXISTING_U" ] && SVC_USER="$EXISTING_U"
    fi
    if [ "$GROUP_EXPLICIT" = "0" ]; then
      EXISTING_G="$(extract_macos_plist_key_string "$P_FILE" "GroupName")"
      if [ -n "$EXISTING_G" ]; then SVC_GROUP="$EXISTING_G"; fi
    fi
  fi
  return 0
}

systemd_escape_dq() {
  printf "%s" "$1" | sed 's/\\/\\\\/g; s/"/\\"/g'
}

format_systemd_arg() {
  case "$1" in
    *[[:space:]\"]*) printf '"%s"' "$(systemd_escape_dq "$1")" ;;
    *) printf '%s' "$1" ;;
  esac
}

write_volo_env_if_missing() {
  TARGET_ENV="$1"
  TARGET_ROOT="$2"
  ENV_MODE="$3"
  ENV_OWNER="$4"

  if [ -f "$TARGET_ENV" ]; then
    log_info "Preserving existing configuration file: ${TARGET_ENV}"
    return 0
  fi

  if [ -n "$UNIX_SOCKET" ]; then
    SOCKET_ENV_LINE="VOLO_LISTEN_SOCKET=${UNIX_SOCKET},${UNIX_SOCKET_PERM}
VOLO_SOCKET_MODE=${UNIX_SOCKET_PERM}"
    if [ -n "$SOCKET_GROUP" ]; then
      SOCKET_ENV_LINE="${SOCKET_ENV_LINE}
VOLO_SOCKET_GROUP=${SOCKET_GROUP}"
    fi
  else
    SOCKET_ENV_LINE="# VOLO_LISTEN_SOCKET=/run/volo/volo.sock
# VOLO_SOCKET_MODE=0660
# VOLO_SOCKET_GROUP=www-data"
  fi

  ENV_BODY="# Generated by Volo-go install-service.sh
VOLO_LISTEN_ADDR=${LISTEN_ADDR}
${SOCKET_ENV_LINE}
VOLO_PUBLIC_URL=${PUBLIC_URL}
VOLO_TRUSTED_PROXIES=${TRUSTED_PROXIES}
VOLO_ADMIN_PATH=${ADMIN_PATH}
VOLO_SECRET_KEY=<generated-256-bit-secret>
VOLO_GEO_ACCESS_ENABLED=true

# Database defaults to SQLite (<VOLO_DATA_DIR>/volo.db) or Setup Wizard data/db.json.
# Uncomment only to force-override via environment variables:
# VOLO_DB_DRIVER=sqlite
# VOLO_DB_DSN=${TARGET_ROOT}/data/volo.db

VOLO_DATA_DIR=${TARGET_ROOT}/data
VOLO_UPLOAD_DIR=${TARGET_ROOT}/data/uploads
VOLO_BACKUP_DIR=${TARGET_ROOT}/data/backups
VOLO_GEOIP_DIR=${TARGET_ROOT}/data/geoip
VOLO_PLUGINS_DIR=${TARGET_ROOT}/plugins
VOLO_THEMES_DIR=${TARGET_ROOT}/themes
VOLO_LOCALES_DIR=${TARGET_ROOT}/locales"

  if [ "$DRY_RUN" = "1" ]; then
    printf "[dry-run] write %s (mode %s%s)\n%s\n" "$TARGET_ENV" "$ENV_MODE" "${ENV_OWNER:+, owner $ENV_OWNER}" "$ENV_BODY"
    return 0
  fi

  SECRET_HEX="$(generate_secret_hex)"
  mkdir -p "$(dirname "$TARGET_ENV")"
  if [ -L "$TARGET_ENV" ]; then
    log_err "Refusing to write secret env file through symlink: ${TARGET_ENV}"
    exit 1
  fi
  OLD_UMASK="$(umask)"
  umask 077
  printf "%s\n" "$ENV_BODY" | sed "s|<generated-256-bit-secret>|${SECRET_HEX}|" > "$TARGET_ENV"
  umask "$OLD_UMASK"
  chmod "$ENV_MODE" "$TARGET_ENV"
  if [ -n "$ENV_OWNER" ]; then
    chown -h "$ENV_OWNER" "$TARGET_ENV" 2>/dev/null || chown "$ENV_OWNER" "$TARGET_ENV" 2>/dev/null || true
  fi
}

ensure_linux_role_and_group() {
  TARGET_U="$1"
  TARGET_G="$2"
  SOCK_G="${3:-}"
  if [ "$DRY_RUN" = "1" ]; then
    printf "[dry-run] ensure system group '%s' and user '%s'%s\n" "$TARGET_G" "$TARGET_U" "${SOCK_G:+ (socket group: $SOCK_G)}"
    return 0
  fi
  if ! grep -q "^${TARGET_G}:" /etc/group 2>/dev/null; then
    log_info "Creating system permission group: ${TARGET_G}"
    if command -v groupadd >/dev/null 2>&1; then
      groupadd --system "$TARGET_G" || groupadd "$TARGET_G"
    elif command -v addgroup >/dev/null 2>&1; then
      addgroup -S "$TARGET_G" || addgroup "$TARGET_G"
    fi
  fi
  if ! id -u "$TARGET_U" >/dev/null 2>&1; then
    log_info "Creating dedicated service account: ${TARGET_U} (group: ${TARGET_G})"
    if command -v useradd >/dev/null 2>&1; then
      useradd --system --gid "$TARGET_G" --no-create-home --shell /usr/sbin/nologin "$TARGET_U" \
        || useradd -r -g "$TARGET_G" -s /bin/false "$TARGET_U"
    elif command -v adduser >/dev/null 2>&1; then
      adduser -S -D -H -G "$TARGET_G" -s /sbin/nologin "$TARGET_U"
    fi
  fi
  if [ -n "$SOCK_G" ] && [ "$SOCK_G" != "$TARGET_G" ]; then
    if grep -q "^${SOCK_G}:" /etc/group 2>/dev/null; then
      if command -v usermod >/dev/null 2>&1; then
        usermod -aG "$SOCK_G" "$TARGET_U" 2>/dev/null || true
      elif command -v addgroup >/dev/null 2>&1; then
        addgroup "$TARGET_U" "$SOCK_G" 2>/dev/null || true
      fi
    else
      log_warn "Socket group '${SOCK_G}' does not exist in /etc/group yet; ensure '${SOCK_G}' exists before starting."
    fi
  fi
}

prepare_work_dirs() {
  ROOT_DIR="$1"
  if [ "$DRY_RUN" = "1" ]; then
    printf "[dry-run] mkdir -p %s/{data/uploads,data/backups,data/geoip,plugins,themes,locales}\n" "$ROOT_DIR"
    return 0
  fi
  mkdir -p \
    "${ROOT_DIR}/data/uploads" \
    "${ROOT_DIR}/data/backups" \
    "${ROOT_DIR}/data/geoip" \
    "${ROOT_DIR}/plugins" \
    "${ROOT_DIR}/themes" \
    "${ROOT_DIR}/locales"
}

# ==============================================================================
# Linux Implementation (systemd System / User & OpenRC)
# ==============================================================================
run_linux() {
  [ -z "$SVC_USER" ] && SVC_USER="volo"
  [ -z "$SVC_GROUP" ] && SVC_GROUP="$SVC_USER"

  if [ "$INIT_OVERRIDE" != "auto" ]; then
    INIT_SYS="$INIT_OVERRIDE"
  elif command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
    INIT_SYS="systemd"
  elif command -v rc-service >/dev/null 2>&1; then
    INIT_SYS="openrc"
  elif [ "$DRY_RUN" = "1" ]; then
    INIT_SYS="systemd"
  else
    log_err "Neither systemd nor OpenRC detected on this Linux host."
    exit 1
  fi

  LISTEN_DISPLAY="http://${LISTEN_ADDR}"
  PRIVATE_TMP_VAL="true"
  if [ -n "$UNIX_SOCKET" ]; then
    LISTEN_DISPLAY="unix:${UNIX_SOCKET} (perm ${UNIX_SOCKET_PERM}${SOCKET_GROUP:+, group $SOCKET_GROUP})"
    case "$UNIX_SOCKET" in
      /tmp/*|/var/tmp/*) PRIVATE_TMP_VAL="false" ;;
    esac
  fi

  if [ "$USER_MODE" = "1" ]; then
    if [ -n "$INSTALL_BIN_OVERRIDE" ]; then
      INSTALL_BIN="$INSTALL_BIN_OVERRIDE"
    elif [ -n "$INSTALL_DIR" ]; then
      INSTALL_DIR="$(normalize_dir_path "$INSTALL_DIR")"
      INSTALL_BIN="${INSTALL_DIR}/volo"
    else
      INSTALL_BIN="${HOME}/.local/bin/volo"
    fi
    [ -z "$DATA_DIR" ] && DATA_DIR="${XDG_DATA_HOME:-${HOME}/.local/share}/volo"
    DATA_DIR="$(normalize_dir_path "$DATA_DIR")"
    ENV_FILE="${ENV_FILE_OVERRIDE:-${DATA_DIR}/volo.env}"
    UNIT_DIR="${HOME}/.config/systemd/user"
    UNIT_FILE="${UNIT_DIR}/${SERVICE_NAME}.service"

    case "$ACTION" in
      install)
        resolve_source_binary
        smoke_test_binary "$RESOLVED_BIN_PATH"
        prepare_work_dirs "$DATA_DIR"
        sync_extensions "$DATA_DIR" "$ENV_FILE"
        copy_binary_safe "$RESOLVED_BIN_PATH" "$INSTALL_BIN"
        write_volo_env_if_missing "$ENV_FILE" "$DATA_DIR" "0600" ""

        UNIT_CONTENT="[Unit]
Description=Volo-go Pure-Go Sovereign Blog Engine (User Service)
Documentation=https://github.com/Willh92/Volo-go
After=network-online.target

[Service]
Type=simple
WorkingDirectory=$(format_systemd_arg "$DATA_DIR")
EnvironmentFile=-$(format_systemd_arg "$ENV_FILE")
ExecStart=$(format_systemd_arg "$INSTALL_BIN") serve --env-file $(format_systemd_arg "$ENV_FILE")
KillSignal=SIGTERM
TimeoutStopSec=20
Restart=on-failure
RestartSec=3
LimitNOFILE=65535

[Install]
WantedBy=default.target"
        if [ "$DRY_RUN" = "1" ]; then
          printf "[dry-run] write %s:\n%s\n" "$UNIT_FILE" "$UNIT_CONTENT"
          if [ "$NO_START" = "1" ]; then
            printf "[dry-run] systemctl --user daemon-reload && systemctl --user enable %s.service (--no-start)\n" "$SERVICE_NAME"
          else
            printf "[dry-run] systemctl --user daemon-reload && systemctl --user enable --now %s.service\n" "$SERVICE_NAME"
          fi
          return 0
        fi
        mkdir -p "$UNIT_DIR"
        chmod "$DATA_DIR_MODE" "$DATA_DIR" 2>/dev/null || true
        printf "%s\n" "$UNIT_CONTENT" > "$UNIT_FILE"
        systemctl --user daemon-reload
        systemctl --user enable "${SERVICE_NAME}.service"
        if [ "$NO_START" = "1" ]; then
          log_ok "Installed user systemd service: ${SERVICE_NAME}.service (--no-start set)"
        else
          systemctl --user restart "${SERVICE_NAME}.service"
          log_ok "Installed & started user systemd service: ${SERVICE_NAME}.service"
        fi
        log_info "Binary   : ${INSTALL_BIN}"
        log_info "Config   : ${ENV_FILE}"
        log_info "Data Dir : ${DATA_DIR}"
        log_info "Listen   : ${LISTEN_DISPLAY}"
        ;;
      upgrade)
        recover_systemd_unit_config "$UNIT_FILE"
        resolve_source_binary
        smoke_test_binary "$RESOLVED_BIN_PATH"
        perform_pre_upgrade_backup "$INSTALL_BIN" "$DATA_DIR" "$ENV_FILE" ""
        backup_live_binary "$INSTALL_BIN"
        sync_extensions "$DATA_DIR" "$ENV_FILE"
        copy_binary_safe "$RESOLVED_BIN_PATH" "$INSTALL_BIN"
        if [ "$DRY_RUN" = "1" ]; then
          if [ "$NO_START" = "1" ]; then
            printf "[dry-run] systemctl --user daemon-reload (--no-start)\n"
          else
            printf "[dry-run] systemctl --user daemon-reload && systemctl --user restart %s.service\n" "$SERVICE_NAME"
            poll_service_healthcheck "$INSTALL_BIN" "$ENV_FILE"
          fi
          return 0
        fi
        if [ -f "$UNIT_FILE" ]; then
          systemctl --user daemon-reload
          if [ "$NO_START" = "1" ]; then
            log_ok "Upgraded user binary at ${INSTALL_BIN} (--no-start set; service restart skipped)."
          else
            RESTART_OK="1"
            systemctl --user restart "${SERVICE_NAME}.service" || RESTART_OK="0"
            if [ "$RESTART_OK" = "0" ] || ! poll_service_healthcheck "$INSTALL_BIN" "$ENV_FILE"; then
              log_err "Upgrade healthcheck failed for user service ${SERVICE_NAME}.service!"
              journalctl --user -u "${SERVICE_NAME}.service" -n 20 --no-pager >&2 2>/dev/null || true
              if [ "$HAD_PREV_BIN" = "1" ] && [ -f "${INSTALL_BIN}.bak" ]; then
                log_warn "Rolling back ${INSTALL_BIN}.bak -> ${INSTALL_BIN} and restarting previous version..."
                copy_binary_safe "${INSTALL_BIN}.bak" "$INSTALL_BIN"
                systemctl --user restart "${SERVICE_NAME}.service" >/dev/null 2>&1 || true
                log_err "Rolled back ${INSTALL_BIN} to previous version after failed upgrade healthcheck."
              fi
              exit 1
            fi
            log_ok "Upgraded, verified /healthz & restarted user service (${INSTALL_BIN}): $("$INSTALL_BIN" version 2>/dev/null || printf "OK")"
          fi
        else
          log_ok "Upgraded binary at ${INSTALL_BIN}"
        fi
        ;;
      uninstall)
        recover_systemd_unit_config "$UNIT_FILE"
        if [ "$PURGE_DATA" = "1" ]; then
          validate_safe_purge_dir "$DATA_DIR"
        fi
        if [ "$DRY_RUN" = "1" ]; then
          printf "[dry-run] systemctl --user disable --now %s.service && rm -f '%s' '%s' '%s.bak'\n" "$SERVICE_NAME" "$UNIT_FILE" "$INSTALL_BIN" "$INSTALL_BIN"
          if [ "$PURGE_DATA" = "1" ]; then
            printf "[dry-run] rm -rf '%s' && rm -f '%s'\n" "$DATA_DIR" "$ENV_FILE"
          fi
          return 0
        fi
        systemctl --user disable --now "${SERVICE_NAME}.service" >/dev/null 2>&1 || true
        rm -f "$UNIT_FILE" "$INSTALL_BIN" "${INSTALL_BIN}.bak"
        systemctl --user daemon-reload >/dev/null 2>&1 || true
        if [ "$PURGE_DATA" = "1" ]; then
          rm -rf "$DATA_DIR"
          safe_remove_env_file "$ENV_FILE"
          log_ok "Uninstalled user service ${SERVICE_NAME} and purged ${DATA_DIR}."
        else
          log_ok "Uninstalled user service ${SERVICE_NAME} (data preserved at ${DATA_DIR})."
        fi
        ;;
      start|stop|restart|status)
        if [ "$DRY_RUN" = "1" ]; then
          printf "[dry-run] systemctl --user %s %s.service\n" "$ACTION" "$SERVICE_NAME"
          return 0
        fi
        systemctl --user "$ACTION" "${SERVICE_NAME}.service"
        ;;
      logs)
        if [ "$DRY_RUN" = "1" ]; then
          printf "[dry-run] journalctl --user -u %s.service -f -n 100\n" "$SERVICE_NAME"
          return 0
        fi
        journalctl --user -u "${SERVICE_NAME}.service" -f -n 100
        ;;
    esac
    return
  fi

  # System-wide Linux mode
  if [ -n "$INSTALL_BIN_OVERRIDE" ]; then
    INSTALL_BIN="$INSTALL_BIN_OVERRIDE"
  elif [ -n "$INSTALL_DIR" ]; then
    INSTALL_DIR="$(normalize_dir_path "$INSTALL_DIR")"
    INSTALL_BIN="${INSTALL_DIR}/volo"
    if [ -z "$DATA_DIR" ] && ! is_standard_bin_dir "$INSTALL_DIR"; then
      DATA_DIR="$INSTALL_DIR"
    fi
  else
    INSTALL_BIN="/usr/local/bin/volo"
  fi
  [ -z "$DATA_DIR" ] && DATA_DIR="/var/lib/volo"
  DATA_DIR="$(normalize_dir_path "$DATA_DIR")"
  ENV_FILE="${ENV_FILE_OVERRIDE:-/etc/volo/volo.env}"

  if [ "$INIT_SYS" = "systemd" ]; then
    UNIT_FILE="/etc/systemd/system/${SERVICE_NAME}.service"
    case "$ACTION" in
      install)
        resolve_source_binary
        smoke_test_binary "$RESOLVED_BIN_PATH"
        ensure_linux_role_and_group "$SVC_USER" "$SVC_GROUP" "$SOCKET_GROUP"
        prepare_work_dirs "$DATA_DIR"
        sync_extensions "$DATA_DIR" "$ENV_FILE"
        copy_binary_safe "$RESOLVED_BIN_PATH" "$INSTALL_BIN"
        write_volo_env_if_missing "$ENV_FILE" "$DATA_DIR" "0640" "root:${SVC_GROUP}"

        SUPP_LIST=""
        if [ -n "$SUPP_GROUPS" ]; then
          SUPP_LIST="$(printf "%s" "$SUPP_GROUPS" | tr ',' ' ')"
        fi
        if [ -n "$SOCKET_GROUP" ] && [ "$SOCKET_GROUP" != "$SVC_GROUP" ]; then
          case " ${SUPP_LIST} " in
            *" ${SOCKET_GROUP} "*) ;;
            *) SUPP_LIST="${SUPP_LIST:+${SUPP_LIST} }${SOCKET_GROUP}" ;;
          esac
        fi
        SUPP_BLOCK=""
        if [ -n "$SUPP_LIST" ]; then
          SUPP_BLOCK="SupplementaryGroups=${SUPP_LIST}
"
        fi

        PROTECT_HOME_VAL="true"
        case "$DATA_DIR" in
          /home/*|/root/*) PROTECT_HOME_VAL="read-only" ;;
        esac

        RW_PATHS="$(format_systemd_arg "$DATA_DIR") /run/volo"
        EXEC_PRE_BLOCK=""
        if [ -n "$UNIX_SOCKET" ]; then
          SOCK_PARENT="$(normalize_dir_path "$(dirname "$UNIX_SOCKET")")"
          if [ "$SOCK_PARENT" != "/run/volo" ] && [ "$SOCK_PARENT" != "$DATA_DIR" ]; then
            RW_PATHS="${RW_PATHS} $(format_systemd_arg "$SOCK_PARENT")"
            EXEC_PRE_BLOCK="ExecStartPre=+install -d -m 0755 -o ${SVC_USER} -g ${SOCKET_GROUP:-$SVC_GROUP} $(format_systemd_arg "$SOCK_PARENT")
"
          fi
        fi

        UNIT_CONTENT="[Unit]
Description=Volo-go Pure-Go Sovereign Blog Engine
Documentation=https://github.com/Willh92/Volo-go
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${SVC_USER}
Group=${SVC_GROUP}
${SUPP_BLOCK}WorkingDirectory=$(format_systemd_arg "$DATA_DIR")
EnvironmentFile=-$(format_systemd_arg "$ENV_FILE")
${EXEC_PRE_BLOCK}ExecStart=$(format_systemd_arg "$INSTALL_BIN") serve --env-file $(format_systemd_arg "$ENV_FILE")
KillSignal=SIGTERM
TimeoutStopSec=20
Restart=on-failure
RestartSec=3
RuntimeDirectory=volo
RuntimeDirectoryMode=0755
LimitNOFILE=65536
NoNewPrivileges=true
PrivateTmp=${PRIVATE_TMP_VAL}
ProtectSystem=strict
ProtectHome=${PROTECT_HOME_VAL}
ProtectControlGroups=true
ProtectKernelModules=true
ProtectKernelTunables=true
RestrictSUIDSGID=true
LockPersonality=true
UMask=0027
ReadWritePaths=${RW_PATHS}

[Install]
WantedBy=multi-user.target"

        if [ "$DRY_RUN" = "1" ]; then
          printf "[dry-run] chown -R %s:%s %s && chmod %s %s\n" "$SVC_USER" "$SVC_GROUP" "$DATA_DIR" "$DATA_DIR_MODE" "$DATA_DIR"
          printf "[dry-run] write %s:\n%s\n" "$UNIT_FILE" "$UNIT_CONTENT"
          if [ "$NO_START" = "1" ]; then
            printf "[dry-run] systemctl daemon-reload && systemctl enable %s.service (--no-start)\n" "$SERVICE_NAME"
          else
            printf "[dry-run] systemctl daemon-reload && systemctl enable --now %s.service\n" "$SERVICE_NAME"
          fi
          return 0
        fi

        if [ -n "$UNIX_SOCKET" ]; then
          SOCK_PARENT="$(dirname "$UNIX_SOCKET")"
          mkdir -p "$SOCK_PARENT"
          [ ! -L "$SOCK_PARENT" ] && chown "${SVC_USER}:${SOCKET_GROUP:-$SVC_GROUP}" "$SOCK_PARENT" 2>/dev/null || true
          safe_chmod_dir 0755 "$SOCK_PARENT" 2>/dev/null || true
        fi
        safe_chown_tree "${SVC_USER}:${SVC_GROUP}" "$DATA_DIR"
        safe_chmod_dir "$DATA_DIR_MODE" "$DATA_DIR" "${DATA_DIR}/data"
        printf "%s\n" "$UNIT_CONTENT" > "$UNIT_FILE"
        chmod 0644 "$UNIT_FILE"
        systemctl daemon-reload
        systemctl enable "${SERVICE_NAME}.service"
        if [ "$NO_START" = "1" ]; then
          log_ok "Volo-go systemd service installed (--no-start set; not started)!"
        else
          systemctl restart "${SERVICE_NAME}.service"
          log_ok "Volo-go systemd service installed and started!"
        fi
        log_info "Binary   : ${INSTALL_BIN}"
        log_info "Config   : ${ENV_FILE}"
        log_info "Role     : User=${SVC_USER}, Group=${SVC_GROUP}${SOCKET_GROUP:+, SocketGroup=${SOCKET_GROUP}}"
        log_info "Data Dir : ${DATA_DIR} (mode ${DATA_DIR_MODE})"
        log_info "Listen   : ${LISTEN_DISPLAY}"
        ;;
      upgrade)
        recover_systemd_unit_config "$UNIT_FILE"
        resolve_source_binary
        smoke_test_binary "$RESOLVED_BIN_PATH"
        perform_pre_upgrade_backup "$INSTALL_BIN" "$DATA_DIR" "$ENV_FILE" "${SVC_USER}:${SVC_GROUP}"
        backup_live_binary "$INSTALL_BIN"
        resolve_extension_dirs "$DATA_DIR" "$ENV_FILE"
        sync_extensions "$DATA_DIR" "$ENV_FILE"
        if [ "$DRY_RUN" = "0" ]; then
          safe_chown_tree "${SVC_USER}:${SVC_GROUP}" "$RESOLVED_PLUGINS_DIR" "$RESOLVED_THEMES_DIR" "$RESOLVED_LOCALES_DIR" 2>/dev/null || true
        fi
        copy_binary_safe "$RESOLVED_BIN_PATH" "$INSTALL_BIN"
        if [ "$DRY_RUN" = "1" ]; then
          if [ "$NO_START" = "1" ]; then
            printf "[dry-run] systemctl daemon-reload (--no-start)\n"
          else
            printf "[dry-run] systemctl daemon-reload && systemctl restart %s.service\n" "$SERVICE_NAME"
            poll_service_healthcheck "$INSTALL_BIN" "$ENV_FILE"
          fi
          return 0
        fi
        if [ -f "$UNIT_FILE" ]; then
          systemctl daemon-reload
          if [ "$NO_START" = "1" ]; then
            log_ok "Upgraded binary at ${INSTALL_BIN} (--no-start set; service restart skipped)."
          else
            RESTART_OK="1"
            systemctl restart "${SERVICE_NAME}.service" || RESTART_OK="0"
            if [ "$RESTART_OK" = "0" ] || ! poll_service_healthcheck "$INSTALL_BIN" "$ENV_FILE"; then
              log_err "Upgrade verification failed: ${SERVICE_NAME}.service failed to start or pass /healthz within 15s!"
              journalctl -u "${SERVICE_NAME}.service" -n 25 --no-pager >&2 2>/dev/null || true
              if [ "$HAD_PREV_BIN" = "1" ] && [ -f "${INSTALL_BIN}.bak" ]; then
                log_warn "Rolling back ${INSTALL_BIN}.bak -> ${INSTALL_BIN} and restarting previous version..."
                copy_binary_safe "${INSTALL_BIN}.bak" "$INSTALL_BIN"
                systemctl restart "${SERVICE_NAME}.service" >/dev/null 2>&1 || true
                log_err "Rolled back ${INSTALL_BIN} to previous version and restarted ${SERVICE_NAME}.service."
              fi
              exit 1
            fi
            log_ok "Upgraded, verified /healthz & restarted ${SERVICE_NAME}.service (${INSTALL_BIN}): $("$INSTALL_BIN" version 2>/dev/null || printf "OK")"
          fi
        else
          log_ok "Upgraded binary at ${INSTALL_BIN} (service unit not yet registered; run 'install' to register)."
        fi
        ;;
      uninstall)
        recover_systemd_unit_config "$UNIT_FILE"
        if [ "$PURGE_DATA" = "1" ]; then
          validate_safe_purge_dir "$DATA_DIR"
        fi
        if [ "$DRY_RUN" = "1" ]; then
          printf "[dry-run] systemctl disable --now %s.service && rm -f '%s' '%s' '%s.bak'\n" "$SERVICE_NAME" "$UNIT_FILE" "$INSTALL_BIN" "$INSTALL_BIN"
          if [ "$PURGE_DATA" = "1" ]; then
            printf "[dry-run] rm -rf '%s' && safe_remove_env_file '%s'\n" "$DATA_DIR" "$ENV_FILE"
          fi
          return 0
        fi
        systemctl disable --now "${SERVICE_NAME}.service" >/dev/null 2>&1 || true
        rm -f "$UNIT_FILE" "$INSTALL_BIN" "${INSTALL_BIN}.bak"
        systemctl daemon-reload >/dev/null 2>&1 || true
        if [ "$PURGE_DATA" = "1" ]; then
          rm -rf "$DATA_DIR"
          safe_remove_env_file "$ENV_FILE"
          log_ok "Removed ${SERVICE_NAME}.service and purged ${DATA_DIR}."
        else
          log_ok "Removed systemd unit ${UNIT_FILE} (config & data preserved in ${ENV_FILE} and ${DATA_DIR})."
        fi
        ;;
      start|stop|restart|status)
        if [ "$DRY_RUN" = "1" ]; then
          printf "[dry-run] systemctl %s %s.service\n" "$ACTION" "$SERVICE_NAME"
          return 0
        fi
        systemctl "$ACTION" "${SERVICE_NAME}.service"
        ;;
      logs)
        if [ "$DRY_RUN" = "1" ]; then
          printf "[dry-run] journalctl -u %s.service -f -n 100\n" "$SERVICE_NAME"
          return 0
        fi
        journalctl -u "${SERVICE_NAME}.service" -f -n 100
        ;;
    esac
  else
    INIT_SCRIPT="/etc/init.d/${SERVICE_NAME}"
    LOG_FILE="/var/log/volo/volo.log"
    case "$ACTION" in
      install)
        resolve_source_binary
        smoke_test_binary "$RESOLVED_BIN_PATH"
        ensure_linux_role_and_group "$SVC_USER" "$SVC_GROUP" "$SOCKET_GROUP"
        prepare_work_dirs "$DATA_DIR"
        sync_extensions "$DATA_DIR" "$ENV_FILE"
        copy_binary_safe "$RESOLVED_BIN_PATH" "$INSTALL_BIN"
        write_volo_env_if_missing "$ENV_FILE" "$DATA_DIR" "0640" "root:${SVC_GROUP}"

        ESC_INSTALL_BIN="$(openrc_escape_dq "$INSTALL_BIN")"
        ESC_ENV_FILE="$(openrc_escape_dq "$ENV_FILE")"
        ESC_SVC_USER="$(openrc_escape_dq "$SVC_USER")"
        ESC_SVC_GROUP="$(openrc_escape_dq "$SVC_GROUP")"
        ESC_DATA_DIR="$(openrc_escape_dq "$DATA_DIR")"
        ESC_LOG_FILE="$(openrc_escape_dq "$LOG_FILE")"

        OPENRC_SOCK_CHECK=""
        if [ -n "$UNIX_SOCKET" ]; then
          SOCK_PARENT="$(normalize_dir_path "$(dirname "$UNIX_SOCKET")")"
          if [ "$SOCK_PARENT" != "/run/volo" ] && [ "$SOCK_PARENT" != "$DATA_DIR" ]; then
            ESC_SOCK_GROUP="$(openrc_escape_dq "${SOCKET_GROUP:-$SVC_GROUP}")"
            ESC_SOCK_PARENT="$(openrc_escape_dq "$SOCK_PARENT")"
            OPENRC_SOCK_CHECK="
    checkpath -d -m 0755 -o \"${ESC_SVC_USER}:${ESC_SOCK_GROUP}\" \"${ESC_SOCK_PARENT}\""
          fi
        fi

        OPENRC_CONTENT="#!/sbin/openrc-run
name=\"Volo-go\"
description=\"Volo-go Pure-Go Sovereign Blog Engine\"

set -a
if [ -f \"${ESC_ENV_FILE}\" ]; then . \"${ESC_ENV_FILE}\"; fi
set +a

command=\"${ESC_INSTALL_BIN}\"
command_args=\"serve --env-file \\\"${ESC_ENV_FILE}\\\"\"
command_user=\"${ESC_SVC_USER}:${ESC_SVC_GROUP}\"
directory=\"${ESC_DATA_DIR}\"
command_background=\"yes\"
pidfile=\"/run/volo/volo.pid\"
stopsig=\"SIGTERM\"
retry=\"SIGTERM/20/SIGKILL/5\"
output_log=\"${ESC_LOG_FILE}\"
error_log=\"${ESC_LOG_FILE}\"
rc_ulimit=\"-n 65536\"

depend() {
    need net
    use dns logger
}

start_pre() {
    checkpath -d -m 0755 -o \"\${command_user}\" /run/volo /var/log/volo
    checkpath -d -m ${DATA_DIR_MODE} -o \"\${command_user}\" \"${ESC_DATA_DIR}\"${OPENRC_SOCK_CHECK}
}"
        if [ "$DRY_RUN" = "1" ]; then
          printf "[dry-run] write %s:\n%s\n" "$INIT_SCRIPT" "$OPENRC_CONTENT"
          if [ "$NO_START" = "1" ]; then
            printf "[dry-run] rc-update add %s default (--no-start)\n" "$SERVICE_NAME"
          else
            printf "[dry-run] rc-update add %s default && rc-service %s restart\n" "$SERVICE_NAME" "$SERVICE_NAME"
          fi
          return 0
        fi
        if [ -n "$UNIX_SOCKET" ]; then
          SOCK_PARENT="$(dirname "$UNIX_SOCKET")"
          mkdir -p "$SOCK_PARENT"
          [ ! -L "$SOCK_PARENT" ] && chown "${SVC_USER}:${SOCKET_GROUP:-$SVC_GROUP}" "$SOCK_PARENT" 2>/dev/null || true
          safe_chmod_dir 0755 "$SOCK_PARENT" 2>/dev/null || true
        fi
        safe_chown_tree "${SVC_USER}:${SVC_GROUP}" "$DATA_DIR"
        safe_chmod_dir "$DATA_DIR_MODE" "$DATA_DIR" "${DATA_DIR}/data"
        mkdir -p /var/log/volo
        [ ! -L /var/log/volo ] && chown "${SVC_USER}:${SVC_GROUP}" /var/log/volo 2>/dev/null || true
        printf "%s\n" "$OPENRC_CONTENT" > "$INIT_SCRIPT"
        chmod 0755 "$INIT_SCRIPT"
        rc-update add "$SERVICE_NAME" default
        if [ "$NO_START" = "1" ]; then
          log_ok "Volo-go OpenRC service installed (--no-start set; not started)!"
        else
          rc-service "$SERVICE_NAME" restart || rc-service "$SERVICE_NAME" start
          log_ok "Volo-go OpenRC service installed and started!"
        fi
        log_info "Binary   : ${INSTALL_BIN}"
        log_info "Config   : ${ENV_FILE}"
        log_info "Listen   : ${LISTEN_DISPLAY}"
        ;;
      upgrade)
        recover_openrc_config "$INIT_SCRIPT"
        resolve_source_binary
        smoke_test_binary "$RESOLVED_BIN_PATH"
        perform_pre_upgrade_backup "$INSTALL_BIN" "$DATA_DIR" "$ENV_FILE" "${SVC_USER}:${SVC_GROUP}"
        backup_live_binary "$INSTALL_BIN"
        resolve_extension_dirs "$DATA_DIR" "$ENV_FILE"
        sync_extensions "$DATA_DIR" "$ENV_FILE"
        if [ "$DRY_RUN" = "0" ]; then
          safe_chown_tree "${SVC_USER}:${SVC_GROUP}" "$RESOLVED_PLUGINS_DIR" "$RESOLVED_THEMES_DIR" "$RESOLVED_LOCALES_DIR" 2>/dev/null || true
        fi
        copy_binary_safe "$RESOLVED_BIN_PATH" "$INSTALL_BIN"
        if [ "$DRY_RUN" = "1" ]; then
          if [ "$NO_START" = "1" ]; then
            printf "[dry-run] skip OpenRC restart (--no-start)\n"
          else
            printf "[dry-run] rc-service %s restart\n" "$SERVICE_NAME"
            poll_service_healthcheck "$INSTALL_BIN" "$ENV_FILE"
          fi
          return 0
        fi
        if [ -f "$INIT_SCRIPT" ]; then
          if [ "$NO_START" = "1" ]; then
            log_ok "Upgraded binary at ${INSTALL_BIN} (--no-start set; service restart skipped)."
          else
            RESTART_OK="1"
            (rc-service "$SERVICE_NAME" restart || rc-service "$SERVICE_NAME" start) || RESTART_OK="0"
            if [ "$RESTART_OK" = "0" ] || ! poll_service_healthcheck "$INSTALL_BIN" "$ENV_FILE"; then
              log_err "Upgrade verification failed: OpenRC service ${SERVICE_NAME} failed to start or pass /healthz within 15s!"
              [ -f "$LOG_FILE" ] && tail -n 25 "$LOG_FILE" >&2 2>/dev/null || true
              if [ "$HAD_PREV_BIN" = "1" ] && [ -f "${INSTALL_BIN}.bak" ]; then
                log_warn "Rolling back ${INSTALL_BIN}.bak -> ${INSTALL_BIN} and restarting previous version..."
                copy_binary_safe "${INSTALL_BIN}.bak" "$INSTALL_BIN"
                (rc-service "$SERVICE_NAME" restart || rc-service "$SERVICE_NAME" start) >/dev/null 2>&1 || true
                log_err "Rolled back ${INSTALL_BIN} to previous version and restarted OpenRC service."
              fi
              exit 1
            fi
            log_ok "Upgraded, verified /healthz & restarted OpenRC service (${INSTALL_BIN}): $("$INSTALL_BIN" version 2>/dev/null || printf "OK")"
          fi
        else
          log_ok "Upgraded binary at ${INSTALL_BIN}"
        fi
        ;;
      uninstall)
        recover_openrc_config "$INIT_SCRIPT"
        if [ "$PURGE_DATA" = "1" ]; then
          validate_safe_purge_dir "$DATA_DIR"
        fi
        if [ "$DRY_RUN" = "1" ]; then
          printf "[dry-run] rc-service %s stop && rc-update del %s default && rm -f '%s' '%s' '%s.bak'\n" "$SERVICE_NAME" "$SERVICE_NAME" "$INIT_SCRIPT" "$INSTALL_BIN" "$INSTALL_BIN"
          if [ "$PURGE_DATA" = "1" ]; then
            printf "[dry-run] rm -rf '%s' && safe_remove_env_file '%s'\n" "$DATA_DIR" "$ENV_FILE"
          fi
          return 0
        fi
        rc-service "$SERVICE_NAME" stop >/dev/null 2>&1 || true
        rc-update del "$SERVICE_NAME" default >/dev/null 2>&1 || true
        rm -f "$INIT_SCRIPT" "$INSTALL_BIN" "${INSTALL_BIN}.bak"
        if [ "$PURGE_DATA" = "1" ]; then
          rm -rf "$DATA_DIR"
          safe_remove_env_file "$ENV_FILE"
          log_ok "Removed OpenRC service ${SERVICE_NAME} and purged ${DATA_DIR}."
        else
          log_ok "Removed OpenRC service ${SERVICE_NAME} (data preserved in ${DATA_DIR})."
        fi
        ;;
      start|stop|restart|status)
        if [ "$DRY_RUN" = "1" ]; then
          printf "[dry-run] rc-service %s %s\n" "$SERVICE_NAME" "$ACTION"
          return 0
        fi
        rc-service "$SERVICE_NAME" "$ACTION"
        ;;
      logs)
        if [ "$DRY_RUN" = "1" ]; then
          printf "[dry-run] tail -n 100 -f '%s'\n" "$LOG_FILE"
          return 0
        fi
        tail -n 100 -f "$LOG_FILE"
        ;;
    esac
  fi
}

# ==============================================================================
# macOS Implementation (launchd LaunchDaemons & LaunchAgents)
# ==============================================================================
compute_macos_owner() {
  if [ "$USER_MODE" = "0" ] && [ -n "$SVC_USER" ]; then
    if [ -n "$SVC_GROUP" ]; then
      printf "%s:%s" "$SVC_USER" "$SVC_GROUP"
    else
      printf "%s" "$SVC_USER"
    fi
  else
    printf ""
  fi
}

restart_macos_launchd() {
  DOM="$1"
  LBL="$2"
  PF="$3"
  if launchctl kickstart -k "${DOM}/${LBL}" >/dev/null 2>&1; then
    return 0
  fi
  if launchctl bootstrap "$DOM" "$PF" >/dev/null 2>&1 || launchctl load -w "$PF" >/dev/null 2>&1; then
    launchctl kickstart -k "${DOM}/${LBL}" >/dev/null 2>&1 || true
    return 0
  fi
  return 1
}

run_macos() {
  if [ "$USER_MODE" = "1" ]; then
    if [ -n "$INSTALL_BIN_OVERRIDE" ]; then
      INSTALL_BIN="$INSTALL_BIN_OVERRIDE"
    elif [ -n "$INSTALL_DIR" ]; then
      INSTALL_DIR="$(normalize_dir_path "$INSTALL_DIR")"
      INSTALL_BIN="${INSTALL_DIR}/volo"
    else
      INSTALL_BIN="${HOME}/.local/bin/volo"
    fi
    [ -z "$DATA_DIR" ] && DATA_DIR="${HOME}/Library/Application Support/Volo"
    LOG_DIR="${HOME}/Library/Logs/Volo"
    PLIST_DIR="${HOME}/Library/LaunchAgents"
    DOMAIN_TARGET="gui/$(id -u)"
  else
    if [ -n "$INSTALL_BIN_OVERRIDE" ]; then
      INSTALL_BIN="$INSTALL_BIN_OVERRIDE"
    elif [ -n "$INSTALL_DIR" ]; then
      INSTALL_DIR="$(normalize_dir_path "$INSTALL_DIR")"
      INSTALL_BIN="${INSTALL_DIR}/volo"
    else
      INSTALL_BIN="/usr/local/bin/volo"
    fi
    [ -z "$DATA_DIR" ] && DATA_DIR="/Library/Application Support/Volo"
    LOG_DIR="/Library/Logs/Volo"
    PLIST_DIR="/Library/LaunchDaemons"
    DOMAIN_TARGET="system"
  fi
  DATA_DIR="$(normalize_dir_path "$DATA_DIR")"
  ENV_FILE="${ENV_FILE_OVERRIDE:-${DATA_DIR}/volo.env}"
  PLIST_FILE="${PLIST_DIR}/${MACOS_LABEL}.plist"
  OUT_LOG="${LOG_DIR}/volo.out.log"
  ERR_LOG="${LOG_DIR}/volo.err.log"

  case "$ACTION" in
    install)
      resolve_source_binary
      smoke_test_binary "$RESOLVED_BIN_PATH"
      prepare_work_dirs "$DATA_DIR"
      sync_extensions "$DATA_DIR" "$ENV_FILE"
      copy_binary_safe "$RESOLVED_BIN_PATH" "$INSTALL_BIN"
      MACOS_OWNER="$(compute_macos_owner)"
      write_volo_env_if_missing "$ENV_FILE" "$DATA_DIR" "0600" "$MACOS_OWNER"

      ROLE_PLIST_BLOCK=""
      if [ "$USER_MODE" = "0" ]; then
        if [ -n "$SVC_USER" ]; then
          ESC_U="$(xml_escape "$SVC_USER")"
          ROLE_PLIST_BLOCK="  <key>UserName</key>
  <string>${ESC_U}</string>
"
        fi
        if [ -n "$SVC_GROUP" ]; then
          ESC_G="$(xml_escape "$SVC_GROUP")"
          ROLE_PLIST_BLOCK="${ROLE_PLIST_BLOCK}  <key>GroupName</key>
  <string>${ESC_G}</string>
"
        fi
      fi

      ESC_LABEL="$(xml_escape "$MACOS_LABEL")"
      ESC_BIN="$(xml_escape "$INSTALL_BIN")"
      ESC_ENV="$(xml_escape "$ENV_FILE")"
      ESC_DATA="$(xml_escape "$DATA_DIR")"
      ESC_OUT="$(xml_escape "$OUT_LOG")"
      ESC_ERR="$(xml_escape "$ERR_LOG")"

      PLIST_CONTENT="<?xml version=\"1.0\" encoding=\"UTF-8\"?>
<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">
<plist version=\"1.0\">
<dict>
  <key>Label</key>
  <string>${ESC_LABEL}</string>
${ROLE_PLIST_BLOCK}  <key>ProgramArguments</key>
  <array>
    <string>${ESC_BIN}</string>
    <string>serve</string>
    <string>--env-file</string>
    <string>${ESC_ENV}</string>
  </array>
  <key>WorkingDirectory</key>
  <string>${ESC_DATA}</string>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <dict>
    <key>SuccessfulExit</key>
    <false/>
  </dict>
  <key>ExitTimeOut</key>
  <integer>20</integer>
  <key>SoftResourceLimits</key>
  <dict>
    <key>NumberOfFiles</key>
    <integer>65535</integer>
  </dict>
  <key>StandardOutPath</key>
  <string>${ESC_OUT}</string>
  <key>StandardErrorPath</key>
  <string>${ESC_ERR}</string>
</dict>
</plist>"

      if [ "$DRY_RUN" = "1" ]; then
        if [ -n "$MACOS_OWNER" ]; then
          printf "[dry-run] chown -R %s '%s' '%s'\n" "$MACOS_OWNER" "$DATA_DIR" "$LOG_DIR"
        elif [ "$USER_MODE" = "0" ] && [ -n "$SVC_GROUP" ]; then
          printf "[dry-run] chgrp -R %s '%s' '%s' '%s'\n" "$SVC_GROUP" "$DATA_DIR" "$LOG_DIR" "$ENV_FILE"
        fi
        printf "[dry-run] write %s:\n%s\n" "$PLIST_FILE" "$PLIST_CONTENT"
        if [ "$NO_START" = "1" ]; then
          printf "[dry-run] skip launchctl bootstrap (--no-start)\n"
        else
          printf "[dry-run] launchctl bootstrap %s '%s'\n" "$DOMAIN_TARGET" "$PLIST_FILE"
        fi
        return 0
      fi

      mkdir -p "$LOG_DIR" "$PLIST_DIR"
      if [ -n "$UNIX_SOCKET" ]; then
        SOCK_PARENT="$(dirname "$UNIX_SOCKET")"
        mkdir -p "$SOCK_PARENT"
        [ -n "$MACOS_OWNER" ] && [ ! -L "$SOCK_PARENT" ] && chown "$MACOS_OWNER" "$SOCK_PARENT" 2>/dev/null || true
        safe_chmod_dir 0755 "$SOCK_PARENT" 2>/dev/null || true
      fi
      if [ -n "$MACOS_OWNER" ]; then
        safe_chown_tree "$MACOS_OWNER" "$DATA_DIR" "$LOG_DIR"
      elif [ "$USER_MODE" = "0" ] && [ -n "$SVC_GROUP" ]; then
        safe_chgrp_tree "$SVC_GROUP" "$DATA_DIR" "$LOG_DIR" "$ENV_FILE" 2>/dev/null || true
      fi
      xattr -d com.apple.quarantine "$INSTALL_BIN" >/dev/null 2>&1 || true
      safe_chmod_dir "$DATA_DIR_MODE" "$DATA_DIR" "${DATA_DIR}/data" 2>/dev/null || true
      printf "%s\n" "$PLIST_CONTENT" > "$PLIST_FILE"
      chmod 0644 "$PLIST_FILE"
      if [ "$NO_START" = "1" ]; then
        log_ok "Volo-go macOS launchd plist written (${MACOS_LABEL}; --no-start set)!"
      else
        launchctl bootout "${DOMAIN_TARGET}/${MACOS_LABEL}" >/dev/null 2>&1 || launchctl unload "$PLIST_FILE" >/dev/null 2>&1 || true
        if ! launchctl bootstrap "$DOMAIN_TARGET" "$PLIST_FILE" >/dev/null 2>&1; then
          launchctl load -w "$PLIST_FILE"
        fi
        launchctl kickstart -k "${DOMAIN_TARGET}/${MACOS_LABEL}" >/dev/null 2>&1 || true
        log_ok "Volo-go macOS launchd service (${MACOS_LABEL}) installed & started!"
      fi
      log_info "Plist    : ${PLIST_FILE}"
      log_info "Binary   : ${INSTALL_BIN}"
      log_info "Config   : ${ENV_FILE}"
      log_info "Data Dir : ${DATA_DIR}"
      log_info "Logs     : ${OUT_LOG}"
      ;;
    upgrade)
      recover_macos_plist_config "$PLIST_FILE"
      MACOS_OWNER="$(compute_macos_owner)"
      resolve_source_binary
      smoke_test_binary "$RESOLVED_BIN_PATH"
      perform_pre_upgrade_backup "$INSTALL_BIN" "$DATA_DIR" "$ENV_FILE" "$MACOS_OWNER"
      backup_live_binary "$INSTALL_BIN"
      resolve_extension_dirs "$DATA_DIR" "$ENV_FILE"
      sync_extensions "$DATA_DIR" "$ENV_FILE"
      if [ "$DRY_RUN" = "0" ] && [ -n "$MACOS_OWNER" ]; then
        safe_chown_tree "$MACOS_OWNER" "$RESOLVED_PLUGINS_DIR" "$RESOLVED_THEMES_DIR" "$RESOLVED_LOCALES_DIR" 2>/dev/null || true
      fi
      copy_binary_safe "$RESOLVED_BIN_PATH" "$INSTALL_BIN"
      if [ "$DRY_RUN" = "1" ]; then
        if [ "$NO_START" = "1" ]; then
          printf "[dry-run] skip launchctl kickstart (--no-start)\n"
        else
          printf "[dry-run] launchctl kickstart -k %s/%s\n" "$DOMAIN_TARGET" "$MACOS_LABEL"
          poll_service_healthcheck "$INSTALL_BIN" "$ENV_FILE"
        fi
        return 0
      fi
      xattr -d com.apple.quarantine "$INSTALL_BIN" >/dev/null 2>&1 || true
      if [ -f "$PLIST_FILE" ]; then
        if [ "$NO_START" = "1" ]; then
          log_ok "Upgraded binary at ${INSTALL_BIN} (--no-start set; service restart skipped)."
        else
          RESTART_OK="1"
          restart_macos_launchd "$DOMAIN_TARGET" "$MACOS_LABEL" "$PLIST_FILE" || RESTART_OK="0"
          if [ "$RESTART_OK" = "0" ] || ! poll_service_healthcheck "$INSTALL_BIN" "$ENV_FILE"; then
            log_err "Upgrade verification failed: ${MACOS_LABEL} failed to start or pass /healthz within 15s!"
            [ -f "$ERR_LOG" ] && tail -n 25 "$ERR_LOG" >&2 2>/dev/null || true
            if [ "$HAD_PREV_BIN" = "1" ] && [ -f "${INSTALL_BIN}.bak" ]; then
              log_warn "Rolling back ${INSTALL_BIN}.bak -> ${INSTALL_BIN} and restarting previous version..."
              copy_binary_safe "${INSTALL_BIN}.bak" "$INSTALL_BIN"
              xattr -d com.apple.quarantine "$INSTALL_BIN" >/dev/null 2>&1 || true
              restart_macos_launchd "$DOMAIN_TARGET" "$MACOS_LABEL" "$PLIST_FILE" || true
              log_err "Rolled back ${INSTALL_BIN} to previous version and restarted ${MACOS_LABEL}."
            fi
            exit 1
          fi
          log_ok "Upgraded, verified /healthz & restarted ${MACOS_LABEL} (${INSTALL_BIN}): $("$INSTALL_BIN" version 2>/dev/null || printf "OK")"
        fi
      else
        log_ok "Upgraded binary at ${INSTALL_BIN}"
      fi
      ;;
    uninstall)
      recover_macos_plist_config "$PLIST_FILE"
      if [ "$PURGE_DATA" = "1" ]; then
        validate_safe_purge_dir "$DATA_DIR"
        validate_safe_purge_dir "$LOG_DIR"
      fi
      if [ "$DRY_RUN" = "1" ]; then
        printf "[dry-run] launchctl bootout %s/%s && rm -f '%s' '%s' '%s.bak'\n" "$DOMAIN_TARGET" "$MACOS_LABEL" "$PLIST_FILE" "$INSTALL_BIN" "$INSTALL_BIN"
        if [ "$PURGE_DATA" = "1" ]; then
          printf "[dry-run] rm -rf '%s' '%s' && safe_remove_env_file '%s'\n" "$DATA_DIR" "$LOG_DIR" "$ENV_FILE"
        fi
        return 0
      fi
      launchctl bootout "${DOMAIN_TARGET}/${MACOS_LABEL}" >/dev/null 2>&1 || launchctl unload -w "$PLIST_FILE" >/dev/null 2>&1 || true
      rm -f "$PLIST_FILE" "$INSTALL_BIN" "${INSTALL_BIN}.bak"
      if [ "$PURGE_DATA" = "1" ]; then
        rm -rf "$DATA_DIR" "$LOG_DIR"
        safe_remove_env_file "$ENV_FILE"
        log_ok "Uninstalled ${MACOS_LABEL} and purged ${DATA_DIR}."
      else
        log_ok "Uninstalled ${MACOS_LABEL} (data preserved in ${DATA_DIR})."
      fi
      ;;
    start)
      if [ "$DRY_RUN" = "1" ]; then
        printf "[dry-run] launchctl kickstart %s/%s\n" "$DOMAIN_TARGET" "$MACOS_LABEL"
        return 0
      fi
      launchctl kickstart "${DOMAIN_TARGET}/${MACOS_LABEL}" >/dev/null 2>&1 || launchctl start "$MACOS_LABEL"
      log_ok "Started ${MACOS_LABEL}"
      ;;
    stop)
      if [ "$DRY_RUN" = "1" ]; then
        printf "[dry-run] launchctl kill SIGTERM %s/%s\n" "$DOMAIN_TARGET" "$MACOS_LABEL"
        return 0
      fi
      launchctl kill SIGTERM "${DOMAIN_TARGET}/${MACOS_LABEL}" >/dev/null 2>&1 || launchctl stop "$MACOS_LABEL"
      log_ok "Stopped ${MACOS_LABEL}"
      ;;
    restart)
      if [ "$DRY_RUN" = "1" ]; then
        printf "[dry-run] launchctl kickstart -k %s/%s\n" "$DOMAIN_TARGET" "$MACOS_LABEL"
        return 0
      fi
      launchctl kickstart -k "${DOMAIN_TARGET}/${MACOS_LABEL}"
      log_ok "Restarted ${MACOS_LABEL}"
      ;;
    status)
      if [ "$DRY_RUN" = "1" ]; then
        printf "[dry-run] launchctl print %s/%s\n" "$DOMAIN_TARGET" "$MACOS_LABEL"
        return 0
      fi
      launchctl print "${DOMAIN_TARGET}/${MACOS_LABEL}" || launchctl list | grep "$MACOS_LABEL" || log_warn "Service ${MACOS_LABEL} is not loaded."
      ;;
    logs)
      if [ "$DRY_RUN" = "1" ]; then
        printf "[dry-run] tail -n 100 -f '%s' '%s'\n" "$OUT_LOG" "$ERR_LOG"
        return 0
      fi
      mkdir -p "$LOG_DIR"
      touch "$OUT_LOG" "$ERR_LOG"
      tail -n 100 -f "$OUT_LOG" "$ERR_LOG"
      ;;
  esac
}

if [ "$TARGET_OS" = "linux" ]; then
  run_linux
else
  run_macos
fi
