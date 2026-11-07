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
BIN_SRC=""
ARCHIVE_SRC=""
INSTALL_DIR=""
INSTALL_BIN_OVERRIDE=""
HOST_ADDR="127.0.0.1"
PORT="8080"
LISTEN_ADDR_EXPLICIT=""
UNIX_SOCKET=""
UNIX_SOCKET_PERM="0660"
SOCKET_GROUP=""
DATA_DIR=""
DATA_DIR_MODE="0750"
ENV_FILE_OVERRIDE=""
PUBLIC_URL="http://localhost:8080"
ADMIN_PATH="/console"
TRUSTED_PROXIES="127.0.0.1/32,::1/128"
SVC_USER=""
SVC_GROUP=""
SUPP_GROUPS=""
USER_MODE="0"
FORCE_SYSTEM="0"
INIT_OVERRIDE="auto"
TARGET_OS_OVERRIDE="auto"
DRY_RUN="0"
PURGE_DATA="0"

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
  upgrade      Download latest/specified release from GitHub, verify SHA-256, and restart service (alias: update)
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
  --host <addr>                   TCP listen host address (default: 127.0.0.1)
  --port <port>                   TCP listen HTTP port (default: 8080)
  --listen-addr <host:port>       Explicit TCP listen address (overrides --host/--port)
  --unix-socket <path>            Listen on Unix domain socket instead of TCP (e.g. /run/volo/volo.sock)
  --unix-socket-perm <octal>      Octal permissions for Unix domain socket (default: 0660)
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

if [ "$#" -gt 0 ]; then
  case "$1" in
    install|upgrade|update|uninstall|start|stop|restart|status|logs)
      ACTION="$1"
      [ "$ACTION" = "update" ] && ACTION="upgrade"
      shift
      ;;
    -h|--help|help)
      usage
      exit 0
      ;;
  esac
fi

while [ "$#" -gt 0 ]; do
  case "$1" in
    --bin|--binary) BIN_SRC="$2"; shift 2 ;;
    --bin=*|--binary=*) BIN_SRC="${1#*=}"; shift 1 ;;
    --archive) ARCHIVE_SRC="$2"; shift 2 ;;
    --archive=*) ARCHIVE_SRC="${1#*=}"; shift 1 ;;
    --install-dir) INSTALL_DIR="$2"; shift 2 ;;
    --install-dir=*) INSTALL_DIR="${1#*=}"; shift 1 ;;
    --install-bin) INSTALL_BIN_OVERRIDE="$2"; shift 2 ;;
    --install-bin=*) INSTALL_BIN_OVERRIDE="${1#*=}"; shift 1 ;;
    --download|--from-github) FORCE_DOWNLOAD="1"; shift 1 ;;
    --version) RELEASE_VERSION="$2"; shift 2 ;;
    --version=*) RELEASE_VERSION="${1#*=}"; shift 1 ;;
    --channel) RELEASE_CHANNEL="$2"; shift 2 ;;
    --channel=*) RELEASE_CHANNEL="${1#*=}"; shift 1 ;;
    --mirror|--github-proxy) GITHUB_MIRROR="$2"; shift 2 ;;
    --mirror=*|--github-proxy=*) GITHUB_MIRROR="${1#*=}"; shift 1 ;;
    --repo) GITHUB_REPO="$2"; shift 2 ;;
    --repo=*) GITHUB_REPO="${1#*=}"; shift 1 ;;
    --no-verify|--skip-verify) SKIP_VERIFY="1"; shift 1 ;;
    --host) HOST_ADDR="$2"; shift 2 ;;
    --host=*) HOST_ADDR="${1#*=}"; shift 1 ;;
    --port) PORT="$2"; shift 2 ;;
    --port=*) PORT="${1#*=}"; shift 1 ;;
    --listen-addr) LISTEN_ADDR_EXPLICIT="$2"; shift 2 ;;
    --listen-addr=*) LISTEN_ADDR_EXPLICIT="${1#*=}"; shift 1 ;;
    --unix-socket|--listen-socket) UNIX_SOCKET="$2"; shift 2 ;;
    --unix-socket=*|--listen-socket=*) UNIX_SOCKET="${1#*=}"; shift 1 ;;
    --unix-socket-perm|--socket-mode) UNIX_SOCKET_PERM="$2"; shift 2 ;;
    --unix-socket-perm=*|--socket-mode=*) UNIX_SOCKET_PERM="${1#*=}"; shift 1 ;;
    --socket-group) SOCKET_GROUP="$2"; shift 2 ;;
    --socket-group=*) SOCKET_GROUP="${1#*=}"; shift 1 ;;
    --public-url) PUBLIC_URL="$2"; shift 2 ;;
    --public-url=*) PUBLIC_URL="${1#*=}"; shift 1 ;;
    --admin-path) ADMIN_PATH="$2"; shift 2 ;;
    --admin-path=*) ADMIN_PATH="${1#*=}"; shift 1 ;;
    --trusted-proxies) TRUSTED_PROXIES="$2"; shift 2 ;;
    --trusted-proxies=*) TRUSTED_PROXIES="${1#*=}"; shift 1 ;;
    --data-dir|--work-dir) DATA_DIR="$2"; shift 2 ;;
    --data-dir=*|--work-dir=*) DATA_DIR="${1#*=}"; shift 1 ;;
    --data-dir-mode) DATA_DIR_MODE="$2"; shift 2 ;;
    --data-dir-mode=*) DATA_DIR_MODE="${1#*=}"; shift 1 ;;
    --env-file) ENV_FILE_OVERRIDE="$2"; shift 2 ;;
    --env-file=*) ENV_FILE_OVERRIDE="${1#*=}"; shift 1 ;;
    --user) SVC_USER="$2"; shift 2 ;;
    --user=*) SVC_USER="${1#*=}"; shift 1 ;;
    --group) SVC_GROUP="$2"; shift 2 ;;
    --group=*) SVC_GROUP="${1#*=}"; shift 1 ;;
    --supplementary-groups) SUPP_GROUPS="$2"; shift 2 ;;
    --supplementary-groups=*) SUPP_GROUPS="${1#*=}"; shift 1 ;;
    --user-mode) USER_MODE="1"; shift 1 ;;
    --system) FORCE_SYSTEM="1"; USER_MODE="0"; shift 1 ;;
    --init) INIT_OVERRIDE="$2"; shift 2 ;;
    --init=*) INIT_OVERRIDE="${1#*=}"; shift 1 ;;
    --target-os) TARGET_OS_OVERRIDE="$2"; shift 2 ;;
    --target-os=*) TARGET_OS_OVERRIDE="${1#*=}"; shift 1 ;;
    --purge-data) PURGE_DATA="1"; shift 1 ;;
    --dry-run) DRY_RUN="1"; shift 1 ;;
    -h|--help) usage; exit 0 ;;
    *)
      log_err "Unknown argument: $1"
      usage
      exit 1
      ;;
  esac
done

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

sync_bundled_extensions() {
  DEST_ROOT="$1"
  [ -z "$RESOLVED_BUNDLE_DIR" ] && return 0
  [ ! -d "$RESOLVED_BUNDLE_DIR" ] && return 0

  for kind in plugins themes; do
    if [ -d "${RESOLVED_BUNDLE_DIR}/${kind}" ]; then
      for item in "${RESOLVED_BUNDLE_DIR}/${kind}"/*; do
        [ -e "$item" ] || continue
        name="$(basename "$item")"
        if [ ! -e "${DEST_ROOT}/${kind}/${name}" ]; then
          if [ "$DRY_RUN" = "1" ]; then
            printf "[dry-run] cp -R %s %s/%s/%s\n" "$item" "$DEST_ROOT" "$kind" "$name"
          else
            mkdir -p "${DEST_ROOT}/${kind}"
            cp -R "$item" "${DEST_ROOT}/${kind}/${name}"
          fi
        fi
      done
    fi
  done

  if [ -d "${RESOLVED_BUNDLE_DIR}/locales" ]; then
    for loc in "${RESOLVED_BUNDLE_DIR}/locales"/*.json; do
      [ -f "$loc" ] || continue
      name="$(basename "$loc")"
      if [ ! -f "${DEST_ROOT}/locales/${name}" ]; then
        if [ "$DRY_RUN" = "1" ]; then
          printf "[dry-run] cp %s %s/locales/%s\n" "$loc" "$DEST_ROOT" "$name"
        else
          mkdir -p "${DEST_ROOT}/locales"
          cp "$loc" "${DEST_ROOT}/locales/${name}"
        fi
      fi
    done
  fi
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
  tar -xzf "$TAR_FILE" -C "$DEST_STAGE"
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
    EFF_URL="$(curl -fsSL -o /dev/null -w '%{url_effective}' "$TAG_URL" 2>/dev/null || true)"
    case "$EFF_URL" in
      */releases/tag/*)
        printf "%s" "${EFF_URL##*/releases/tag/}" | sed 's/[?#].*$//'
        return 0
        ;;
    esac
  fi
  printf ""
}

download_release_bundle() {
  PINNED="0"
  if [ -n "$RELEASE_VERSION" ]; then
    case "$RELEASE_VERSION" in
      latest)  PRIMARY_TARGET="latest" ;;
      preview) PRIMARY_TARGET="preview"; PINNED="1" ;;
      v*|V*)   PRIMARY_TARGET="v${RELEASE_VERSION#[vV]}"; PINNED="1" ;;
      *)       PRIMARY_TARGET="v${RELEASE_VERSION}"; PINNED="1" ;;
    esac
  else
    case "$RELEASE_CHANNEL" in
      preview)     PRIMARY_TARGET="preview"; PINNED="1" ;;
      release|"")  PRIMARY_TARGET="latest" ;;
      *)
        log_err "Invalid --channel '${RELEASE_CHANNEL}' (expected 'release' or 'preview')"
        exit 1
        ;;
    esac
  fi

  STABLE_ASSET="volo-${TARGET_OS}-${TARGET_ARCH}.tar.gz"
  if [ "$DRY_RUN" = "1" ]; then
    printf "[dry-run] download %s from %s (%s) + verify SHA256SUMS.txt\n" "$STABLE_ASSET" "$GITHUB_REPO" "$PRIMARY_TARGET"
    RESOLVED_BIN_PATH="/tmp/volo-dry-run-bin"
    return 0
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
    # Fallback 2: rolling preview release when latest is not yet published
    if [ "$FALLBACK_OK" = "0" ] && [ "$PRIMARY_TARGET" = "latest" ] && [ "$PINNED" = "0" ]; then
      log_warn "No stable 'latest' release found; falling back to rolling 'preview' release..."
      PRIMARY_TARGET="preview"
      BASE_URL="$(build_release_base_url "preview")"
      DOWNLOADED_ASSET="$STABLE_ASSET"
      DL_ARC="${TMP_DOWNLOAD_DIR}/${DOWNLOADED_ASSET}"
      if fetch_url "${BASE_URL}/${DOWNLOADED_ASSET}" "$DL_ARC" "0"; then
        FALLBACK_OK="1"
      fi
    fi
    if [ "$FALLBACK_OK" = "0" ]; then
      log_err "Failed to download release archive for ${TARGET_OS}/${TARGET_ARCH} from ${GITHUB_REPO}."
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

  if [ "$(uname -s | tr '[:upper:]' '[:lower:]')" = "$TARGET_OS" ]; then
    if ! "$RESOLVED_BIN_PATH" version >/dev/null 2>&1; then
      log_err "Downloaded binary failed preflight check ('${RESOLVED_BIN_PATH} version'); refusing to overwrite running service."
      exit 1
    fi
  fi
}

resolve_source_binary() {
  if [ -n "$BIN_SRC" ]; then
    if [ "$DRY_RUN" = "0" ] && [ ! -f "$BIN_SRC" ]; then
      log_err "Specified binary not found: $BIN_SRC"
      exit 1
    fi
    RESOLVED_BIN_PATH="$BIN_SRC"
    CAND_ROOT="$(CDPATH= cd -- "$(dirname "$BIN_SRC")" 2>/dev/null && pwd || pwd)"
    if [ -d "${CAND_ROOT}/themes" ] || [ -d "${CAND_ROOT}/locales" ]; then
      RESOLVED_BUNDLE_DIR="$CAND_ROOT"
    elif [ -d "${SCRIPT_DIR}/../themes" ]; then
      RESOLVED_BUNDLE_DIR="$(CDPATH= cd -- "${SCRIPT_DIR}/.." && pwd)"
    fi
    return
  fi

  if [ -n "$ARCHIVE_SRC" ]; then
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
      if [ -d "${CAND_ROOT}/themes" ] || [ -d "${CAND_ROOT}/locales" ]; then
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

extract_systemd_exec_bin() {
  U_FILE="$1"
  if [ -f "$U_FILE" ]; then
    awk '
      /^ExecStart=/ {
        line = substr($0, 11)
        sub(/^[ \t@+-]+/, "", line)
        if (substr(line, 1, 1) == "\"") {
          sub(/^"/, "", line)
          sub(/".*$/, "", line)
          print line
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
    ' "$U_FILE"
  fi
}

extract_systemd_workdir() {
  U_FILE="$1"
  if [ -f "$U_FILE" ]; then
    sed -n 's/^WorkingDirectory=//p' "$U_FILE" | head -n 1 | sed 's/^"//; s/"$//'
  fi
}

extract_macos_plist_bin() {
  P_FILE="$1"
  [ ! -f "$P_FILE" ] && return 0
  PLIST_XML=""
  if command -v plutil >/dev/null 2>&1; then
    PLIST_XML="$(plutil -convert xml1 -o - "$P_FILE" 2>/dev/null || true)"
  fi
  [ -z "$PLIST_XML" ] && PLIST_XML="$(cat "$P_FILE" 2>/dev/null || true)"
  printf "%s\n" "$PLIST_XML" | awk '
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
      if (in_arr && match(line, /<string>[^<]+<\/string>/)) {
        val = substr(line, RSTART + 8, RLENGTH - 17)
        print val
        exit
      }
      if (in_arr && /<\/array>/) { exit }
    }
  '
}

systemd_escape_dq() {
  printf "%s" "$1" | sed 's/\\/\\\\/g; s/"/\\"/g'
}

format_systemd_arg() {
  case "$1" in
    *[[:space:]]*) printf '"%s"' "$(systemd_escape_dq "$1")" ;;
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

  ENV_BODY="# Generated by Volo-go install-service.sh
VOLO_LISTEN_ADDR=${LISTEN_ADDR}
VOLO_LISTEN_SOCKET=${UNIX_SOCKET}
VOLO_LISTEN_SOCKET_MODE=${UNIX_SOCKET_PERM}
VOLO_PUBLIC_URL=${PUBLIC_URL}
VOLO_TRUSTED_PROXIES=${TRUSTED_PROXIES}
VOLO_ADMIN_PATH=${ADMIN_PATH}
VOLO_SECRET_KEY=<generated-256-bit-secret>
VOLO_GEO_ACCESS_ENABLED=true

VOLO_DB_DRIVER=sqlite
VOLO_DB_DSN=${TARGET_ROOT}/data/volo.db

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
  OLD_UMASK="$(umask)"
  umask 077
  printf "%s\n" "$ENV_BODY" | sed "s|<generated-256-bit-secret>|${SECRET_HEX}|" > "$TARGET_ENV"
  umask "$OLD_UMASK"
  chmod "$ENV_MODE" "$TARGET_ENV"
  if [ -n "$ENV_OWNER" ]; then
    chown "$ENV_OWNER" "$TARGET_ENV" 2>/dev/null || true
  fi
}

ensure_linux_role_and_group() {
  TARGET_U="$1"
  TARGET_G="$2"
  if [ "$DRY_RUN" = "1" ]; then
    printf "[dry-run] ensure system group '%s' and user '%s'\n" "$TARGET_G" "$TARGET_U"
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
  [ -z "$SVC_GROUP" ] && SVC_GROUP="${SOCKET_GROUP:-$SVC_USER}"

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
    LISTEN_DISPLAY="unix:${UNIX_SOCKET} (perm ${UNIX_SOCKET_PERM})"
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
        prepare_work_dirs "$DATA_DIR"
        sync_bundled_extensions "$DATA_DIR"
        copy_binary_safe "$RESOLVED_BIN_PATH" "$INSTALL_BIN"
        write_volo_env_if_missing "$ENV_FILE" "$DATA_DIR" "0600" ""

        UNIT_CONTENT="[Unit]
Description=Volo-go Pure-Go Sovereign Blog Engine (User Service)
Documentation=https://github.com/Willh92/Volo-go
After=network-online.target

[Service]
Type=simple
WorkingDirectory=${DATA_DIR}
EnvironmentFile=-${ENV_FILE}
ExecStart=$(format_systemd_arg "$INSTALL_BIN") serve
KillSignal=SIGTERM
TimeoutStopSec=20
Restart=on-failure
RestartSec=3
LimitNOFILE=65535

[Install]
WantedBy=default.target"
        if [ "$DRY_RUN" = "1" ]; then
          printf "[dry-run] write %s:\n%s\n" "$UNIT_FILE" "$UNIT_CONTENT"
          return 0
        fi
        mkdir -p "$UNIT_DIR"
        chmod "$DATA_DIR_MODE" "$DATA_DIR" 2>/dev/null || true
        printf "%s\n" "$UNIT_CONTENT" > "$UNIT_FILE"
        systemctl --user daemon-reload
        systemctl --user enable "${SERVICE_NAME}.service"
        systemctl --user restart "${SERVICE_NAME}.service"
        log_ok "Installed & started user systemd service: ${SERVICE_NAME}.service"
        log_info "Binary   : ${INSTALL_BIN}"
        log_info "Config   : ${ENV_FILE}"
        log_info "Data Dir : ${DATA_DIR}"
        log_info "Listen   : ${LISTEN_DISPLAY}"
        ;;
      upgrade)
        if [ -z "$INSTALL_DIR" ] && [ -z "$INSTALL_BIN_OVERRIDE" ] && [ -f "$UNIT_FILE" ]; then
          EXISTING_BIN="$(extract_systemd_exec_bin "$UNIT_FILE")"
          [ -n "$EXISTING_BIN" ] && INSTALL_BIN="$EXISTING_BIN"
          EXISTING_WD="$(extract_systemd_workdir "$UNIT_FILE")"
          [ -n "$EXISTING_WD" ] && DATA_DIR="$EXISTING_WD"
        fi
        resolve_source_binary
        [ -d "$DATA_DIR" ] && sync_bundled_extensions "$DATA_DIR"
        copy_binary_safe "$RESOLVED_BIN_PATH" "$INSTALL_BIN"
        if [ "$DRY_RUN" = "1" ]; then
          printf "[dry-run] systemctl --user daemon-reload && systemctl --user restart %s.service\n" "$SERVICE_NAME"
          return 0
        fi
        if [ -f "$UNIT_FILE" ]; then
          systemctl --user daemon-reload
          systemctl --user restart "${SERVICE_NAME}.service"
          log_ok "Upgraded & restarted user service (${INSTALL_BIN}): $("$INSTALL_BIN" version 2>/dev/null || printf "OK")"
        else
          log_ok "Upgraded binary at ${INSTALL_BIN}"
        fi
        ;;
      uninstall)
        systemctl --user disable --now "${SERVICE_NAME}.service" >/dev/null 2>&1 || true
        rm -f "$UNIT_FILE"
        systemctl --user daemon-reload || true
        if [ "$PURGE_DATA" = "1" ]; then
          rm -rf "$DATA_DIR" "$ENV_FILE"
          log_ok "Uninstalled user service ${SERVICE_NAME} and purged ${DATA_DIR}."
        else
          log_ok "Uninstalled user service ${SERVICE_NAME} (data preserved at ${DATA_DIR})."
        fi
        ;;
      start|stop|restart|status)
        systemctl --user "$ACTION" "${SERVICE_NAME}.service"
        ;;
      logs)
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
        ensure_linux_role_and_group "$SVC_USER" "$SVC_GROUP"
        prepare_work_dirs "$DATA_DIR"
        sync_bundled_extensions "$DATA_DIR"
        copy_binary_safe "$RESOLVED_BIN_PATH" "$INSTALL_BIN"
        write_volo_env_if_missing "$ENV_FILE" "$DATA_DIR" "0640" "root:${SVC_GROUP}"

        SUPP_LINE=""
        if [ -n "$SUPP_GROUPS" ]; then
          SUPP_SPACED="$(printf "%s" "$SUPP_GROUPS" | tr ',' ' ')"
          SUPP_LINE="SupplementaryGroups=${SUPP_SPACED}"
        elif [ -n "$SOCKET_GROUP" ] && [ "$SOCKET_GROUP" != "$SVC_GROUP" ]; then
          SUPP_LINE="SupplementaryGroups=${SOCKET_GROUP}"
        fi

        RW_PATHS="${DATA_DIR} /run/volo"
        if [ -n "$UNIX_SOCKET" ]; then
          SOCK_PARENT="$(dirname "$UNIX_SOCKET")"
          case " $RW_PATHS " in
            *" $SOCK_PARENT "*) ;;
            *) RW_PATHS="${RW_PATHS} ${SOCK_PARENT}" ;;
          esac
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
${SUPP_LINE}
WorkingDirectory=${DATA_DIR}
EnvironmentFile=-${ENV_FILE}
ExecStart=$(format_systemd_arg "$INSTALL_BIN") serve
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
ProtectHome=true
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
          printf "[dry-run] systemctl daemon-reload && systemctl enable --now %s.service\n" "$SERVICE_NAME"
          return 0
        fi

        if [ -n "$UNIX_SOCKET" ]; then
          SOCK_PARENT="$(dirname "$UNIX_SOCKET")"
          mkdir -p "$SOCK_PARENT"
          chown "${SVC_USER}:${SOCKET_GROUP:-$SVC_GROUP}" "$SOCK_PARENT" 2>/dev/null || true
          chmod 0755 "$SOCK_PARENT" 2>/dev/null || true
        fi
        chown -R "${SVC_USER}:${SVC_GROUP}" "$DATA_DIR"
        chmod "$DATA_DIR_MODE" "$DATA_DIR" "${DATA_DIR}/data"
        printf "%s\n" "$UNIT_CONTENT" > "$UNIT_FILE"
        chmod 0644 "$UNIT_FILE"
        systemctl daemon-reload
        systemctl enable "${SERVICE_NAME}.service"
        systemctl restart "${SERVICE_NAME}.service"
        log_ok "Volo-go systemd service installed and started!"
        log_info "Binary   : ${INSTALL_BIN}"
        log_info "Config   : ${ENV_FILE}"
        log_info "Role     : User=${SVC_USER}, Group=${SVC_GROUP}"
        log_info "Data Dir : ${DATA_DIR} (mode ${DATA_DIR_MODE})"
        log_info "Listen   : ${LISTEN_DISPLAY}"
        ;;
      upgrade)
        if [ -z "$INSTALL_DIR" ] && [ -z "$INSTALL_BIN_OVERRIDE" ] && [ -f "$UNIT_FILE" ]; then
          EXISTING_BIN="$(extract_systemd_exec_bin "$UNIT_FILE")"
          [ -n "$EXISTING_BIN" ] && INSTALL_BIN="$EXISTING_BIN"
          EXISTING_WD="$(extract_systemd_workdir "$UNIT_FILE")"
          [ -n "$EXISTING_WD" ] && DATA_DIR="$EXISTING_WD"
        fi
        resolve_source_binary
        if [ -d "$DATA_DIR" ]; then
          sync_bundled_extensions "$DATA_DIR"
          [ "$DRY_RUN" = "0" ] && chown -R "${SVC_USER}:${SVC_GROUP}" "${DATA_DIR}/plugins" "${DATA_DIR}/themes" "${DATA_DIR}/locales" 2>/dev/null || true
        fi
        copy_binary_safe "$RESOLVED_BIN_PATH" "$INSTALL_BIN"
        if [ "$DRY_RUN" = "1" ]; then
          printf "[dry-run] systemctl daemon-reload && systemctl restart %s.service\n" "$SERVICE_NAME"
          return 0
        fi
        if [ -f "$UNIT_FILE" ]; then
          systemctl daemon-reload
          systemctl restart "${SERVICE_NAME}.service"
          log_ok "Upgraded & restarted ${SERVICE_NAME}.service (${INSTALL_BIN}): $("$INSTALL_BIN" version 2>/dev/null || printf "OK")"
        else
          log_ok "Upgraded binary at ${INSTALL_BIN} (service unit not yet registered; run 'install' to register)."
        fi
        ;;
      uninstall)
        if [ "$DRY_RUN" = "1" ]; then
          printf "[dry-run] systemctl disable --now %s.service && rm -f %s\n" "$SERVICE_NAME" "$UNIT_FILE"
          return 0
        fi
        systemctl disable --now "${SERVICE_NAME}.service" >/dev/null 2>&1 || true
        rm -f "$UNIT_FILE" "$INSTALL_BIN"
        systemctl daemon-reload
        if [ "$PURGE_DATA" = "1" ]; then
          rm -rf "$DATA_DIR" "$(dirname "$ENV_FILE")"
          log_ok "Removed ${SERVICE_NAME}.service and purged ${DATA_DIR}."
        else
          log_ok "Removed systemd unit ${UNIT_FILE} (config & data preserved in ${ENV_FILE} and ${DATA_DIR})."
        fi
        ;;
      start|stop|restart|status)
        systemctl "$ACTION" "${SERVICE_NAME}.service"
        ;;
      logs)
        journalctl -u "${SERVICE_NAME}.service" -f -n 100
        ;;
    esac
  else
    INIT_SCRIPT="/etc/init.d/${SERVICE_NAME}"
    LOG_FILE="/var/log/volo/volo.log"
    case "$ACTION" in
      install)
        resolve_source_binary
        ensure_linux_role_and_group "$SVC_USER" "$SVC_GROUP"
        prepare_work_dirs "$DATA_DIR"
        sync_bundled_extensions "$DATA_DIR"
        copy_binary_safe "$RESOLVED_BIN_PATH" "$INSTALL_BIN"
        write_volo_env_if_missing "$ENV_FILE" "$DATA_DIR" "0640" "root:${SVC_GROUP}"

        OPENRC_CONTENT="#!/sbin/openrc-run
name=\"Volo-go\"
description=\"Volo-go Pure-Go Sovereign Blog Engine\"
command=\"${INSTALL_BIN}\"
command_args=\"serve --env-file ${ENV_FILE}\"
command_user=\"${SVC_USER}:${SVC_GROUP}\"
directory=\"${DATA_DIR}\"
command_background=\"yes\"
pidfile=\"/run/volo/volo.pid\"
stopsig=\"SIGTERM\"
retry=\"SIGTERM/20/SIGKILL/5\"
output_log=\"${LOG_FILE}\"
error_log=\"${LOG_FILE}\"
rc_ulimit=\"-n 65536\"

depend() {
    need net
    use dns logger
}

start_pre() {
    checkpath --directory --owner \"\${command_user}\" --mode 0755 /run/volo /var/log/volo
    checkpath --directory --owner \"\${command_user}\" --mode ${DATA_DIR_MODE} \"${DATA_DIR}\"
}"
        if [ "$DRY_RUN" = "1" ]; then
          printf "[dry-run] write %s:\n%s\n" "$INIT_SCRIPT" "$OPENRC_CONTENT"
          printf "[dry-run] rc-update add %s default && rc-service %s restart\n" "$SERVICE_NAME" "$SERVICE_NAME"
          return 0
        fi
        chown -R "${SVC_USER}:${SVC_GROUP}" "$DATA_DIR"
        chmod "$DATA_DIR_MODE" "$DATA_DIR" "${DATA_DIR}/data"
        mkdir -p /var/log/volo
        chown "${SVC_USER}:${SVC_GROUP}" /var/log/volo 2>/dev/null || true
        printf "%s\n" "$OPENRC_CONTENT" > "$INIT_SCRIPT"
        chmod 0755 "$INIT_SCRIPT"
        rc-update add "$SERVICE_NAME" default
        rc-service "$SERVICE_NAME" restart || rc-service "$SERVICE_NAME" start
        log_ok "Volo-go OpenRC service installed and started!"
        log_info "Binary   : ${INSTALL_BIN}"
        log_info "Config   : ${ENV_FILE}"
        log_info "Listen   : ${LISTEN_DISPLAY}"
        ;;
      upgrade)
        if [ -z "$INSTALL_DIR" ] && [ -z "$INSTALL_BIN_OVERRIDE" ] && [ -f "$INIT_SCRIPT" ]; then
          EXISTING_BIN="$(sed -n 's/^command="\([^"]*\)".*/\1/p' "$INIT_SCRIPT" | head -n 1)"
          [ -n "$EXISTING_BIN" ] && INSTALL_BIN="$EXISTING_BIN"
          EXISTING_WD="$(sed -n 's/^directory="\([^"]*\)".*/\1/p' "$INIT_SCRIPT" | head -n 1)"
          [ -n "$EXISTING_WD" ] && DATA_DIR="$EXISTING_WD"
        fi
        resolve_source_binary
        if [ -d "$DATA_DIR" ]; then
          sync_bundled_extensions "$DATA_DIR"
          [ "$DRY_RUN" = "0" ] && chown -R "${SVC_USER}:${SVC_GROUP}" "${DATA_DIR}/plugins" "${DATA_DIR}/themes" "${DATA_DIR}/locales" 2>/dev/null || true
        fi
        copy_binary_safe "$RESOLVED_BIN_PATH" "$INSTALL_BIN"
        if [ "$DRY_RUN" = "1" ]; then
          printf "[dry-run] rc-service %s restart\n" "$SERVICE_NAME"
          return 0
        fi
        if [ -f "$INIT_SCRIPT" ]; then
          rc-service "$SERVICE_NAME" restart || rc-service "$SERVICE_NAME" start
          log_ok "Upgraded & restarted OpenRC service (${INSTALL_BIN}): $("$INSTALL_BIN" version 2>/dev/null || printf "OK")"
        else
          log_ok "Upgraded binary at ${INSTALL_BIN}"
        fi
        ;;
      uninstall)
        rc-service "$SERVICE_NAME" stop >/dev/null 2>&1 || true
        rc-update del "$SERVICE_NAME" default >/dev/null 2>&1 || true
        rm -f "$INIT_SCRIPT" "$INSTALL_BIN"
        if [ "$PURGE_DATA" = "1" ]; then
          rm -rf "$DATA_DIR" "$(dirname "$ENV_FILE")"
          log_ok "Removed OpenRC service ${SERVICE_NAME} and purged ${DATA_DIR}."
        else
          log_ok "Removed OpenRC service ${SERVICE_NAME} (data preserved in ${DATA_DIR})."
        fi
        ;;
      start|stop|restart|status)
        rc-service "$SERVICE_NAME" "$ACTION"
        ;;
      logs)
        tail -n 100 -f "$LOG_FILE"
        ;;
    esac
  fi
}

# ==============================================================================
# macOS Implementation (launchd LaunchDaemons & LaunchAgents)
# ==============================================================================
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
      prepare_work_dirs "$DATA_DIR"
      sync_bundled_extensions "$DATA_DIR"
      copy_binary_safe "$RESOLVED_BIN_PATH" "$INSTALL_BIN"
      write_volo_env_if_missing "$ENV_FILE" "$DATA_DIR" "0600" ""

      ROLE_PLIST_KEYS=""
      if [ "$USER_MODE" = "0" ]; then
        [ -n "$SVC_USER" ] && ROLE_PLIST_KEYS="  <key>UserName</key>
  <string>${SVC_USER}</string>"
        [ -n "$SVC_GROUP" ] && ROLE_PLIST_KEYS="${ROLE_PLIST_KEYS}
  <key>GroupName</key>
  <string>${SVC_GROUP}</string>"
      fi

      PLIST_CONTENT="<?xml version=\"1.0\" encoding=\"UTF-8\"?>
<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">
<plist version=\"1.0\">
<dict>
  <key>Label</key>
  <string>${MACOS_LABEL}</string>
${ROLE_PLIST_KEYS}
  <key>ProgramArguments</key>
  <array>
    <string>${INSTALL_BIN}</string>
    <string>serve</string>
    <string>--env-file</string>
    <string>${ENV_FILE}</string>
  </array>
  <key>WorkingDirectory</key>
  <string>${DATA_DIR}</string>
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
  <string>${OUT_LOG}</string>
  <key>StandardErrorPath</key>
  <string>${ERR_LOG}</string>
</dict>
</plist>"

      if [ "$DRY_RUN" = "1" ]; then
        printf "[dry-run] write %s:\n%s\n" "$PLIST_FILE" "$PLIST_CONTENT"
        printf "[dry-run] launchctl bootstrap %s '%s'\n" "$DOMAIN_TARGET" "$PLIST_FILE"
        return 0
      fi

      mkdir -p "$LOG_DIR" "$PLIST_DIR"
      xattr -d com.apple.quarantine "$INSTALL_BIN" >/dev/null 2>&1 || true
      chmod "$DATA_DIR_MODE" "$DATA_DIR" "${DATA_DIR}/data" 2>/dev/null || true
      printf "%s\n" "$PLIST_CONTENT" > "$PLIST_FILE"
      chmod 0644 "$PLIST_FILE"
      launchctl bootout "${DOMAIN_TARGET}/${MACOS_LABEL}" >/dev/null 2>&1 || launchctl unload "$PLIST_FILE" >/dev/null 2>&1 || true
      if ! launchctl bootstrap "$DOMAIN_TARGET" "$PLIST_FILE" >/dev/null 2>&1; then
        launchctl load -w "$PLIST_FILE"
      fi
      launchctl kickstart -k "${DOMAIN_TARGET}/${MACOS_LABEL}" >/dev/null 2>&1 || true
      log_ok "Volo-go macOS launchd service (${MACOS_LABEL}) installed & started!"
      log_info "Plist    : ${PLIST_FILE}"
      log_info "Binary   : ${INSTALL_BIN}"
      log_info "Config   : ${ENV_FILE}"
      log_info "Data Dir : ${DATA_DIR}"
      log_info "Logs     : ${OUT_LOG}"
      ;;
    upgrade)
      if [ -z "$INSTALL_DIR" ] && [ -z "$INSTALL_BIN_OVERRIDE" ] && [ -f "$PLIST_FILE" ]; then
        EXISTING_BIN="$(extract_macos_plist_bin "$PLIST_FILE")"
        [ -n "$EXISTING_BIN" ] && INSTALL_BIN="$EXISTING_BIN"
      fi
      resolve_source_binary
      [ -d "$DATA_DIR" ] && sync_bundled_extensions "$DATA_DIR"
      copy_binary_safe "$RESOLVED_BIN_PATH" "$INSTALL_BIN"
      if [ "$DRY_RUN" = "1" ]; then
        printf "[dry-run] launchctl kickstart -k %s/%s\n" "$DOMAIN_TARGET" "$MACOS_LABEL"
        return 0
      fi
      xattr -d com.apple.quarantine "$INSTALL_BIN" >/dev/null 2>&1 || true
      if [ -f "$PLIST_FILE" ]; then
        launchctl kickstart -k "${DOMAIN_TARGET}/${MACOS_LABEL}" >/dev/null 2>&1 || true
        log_ok "Upgraded & restarted ${MACOS_LABEL} (${INSTALL_BIN}): $("$INSTALL_BIN" version 2>/dev/null || printf "OK")"
      else
        log_ok "Upgraded binary at ${INSTALL_BIN}"
      fi
      ;;
    uninstall)
      if [ "$DRY_RUN" = "1" ]; then
        printf "[dry-run] launchctl bootout %s/%s && rm -f '%s'\n" "$DOMAIN_TARGET" "$MACOS_LABEL" "$PLIST_FILE"
        return 0
      fi
      launchctl bootout "${DOMAIN_TARGET}/${MACOS_LABEL}" >/dev/null 2>&1 || launchctl unload -w "$PLIST_FILE" >/dev/null 2>&1 || true
      rm -f "$PLIST_FILE" "$INSTALL_BIN"
      if [ "$PURGE_DATA" = "1" ]; then
        rm -rf "$DATA_DIR" "$ENV_FILE" "$LOG_DIR"
        log_ok "Uninstalled ${MACOS_LABEL} and purged ${DATA_DIR}."
      else
        log_ok "Uninstalled ${MACOS_LABEL} (data preserved in ${DATA_DIR})."
      fi
      ;;
    start)
      launchctl kickstart "${DOMAIN_TARGET}/${MACOS_LABEL}" >/dev/null 2>&1 || launchctl start "$MACOS_LABEL"
      log_ok "Started ${MACOS_LABEL}"
      ;;
    stop)
      launchctl kill SIGTERM "${DOMAIN_TARGET}/${MACOS_LABEL}" >/dev/null 2>&1 || launchctl stop "$MACOS_LABEL"
      log_ok "Stopped ${MACOS_LABEL}"
      ;;
    restart)
      launchctl kickstart -k "${DOMAIN_TARGET}/${MACOS_LABEL}"
      log_ok "Restarted ${MACOS_LABEL}"
      ;;
    status)
      launchctl print "${DOMAIN_TARGET}/${MACOS_LABEL}" || launchctl list | grep "$MACOS_LABEL" || log_warn "Service ${MACOS_LABEL} is not loaded."
      ;;
    logs)
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
