#!/usr/bin/env bash
set -euo pipefail

# ==============================================================================
# Volo-go Binary Size (< 20 MiB), Static Linkage & Idle RSS (< 35 MiB) Verifier
# ==============================================================================

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/volo-budget-XXXXXX")"
trap 'if [[ -n "${SERVER_PID:-}" ]] && kill -0 "${SERVER_PID}" 2>/dev/null; then kill -TERM "${SERVER_PID}" 2>/dev/null || true; wait "${SERVER_PID}" 2>/dev/null || true; fi; rm -rf "${WORK_DIR}"' EXIT

BIN_PATH="${WORK_DIR}/volo"
MAX_BIN_BYTES=$((20 * 1024 * 1024)) # 20 MiB
MAX_RSS_KB=$((35 * 1024))           # 35 MiB (35840 KiB)
PORT="${VOLO_BUDGET_PORT:-18991}"

echo "[1/4] Building pure-Go static binary (CGO_ENABLED=0)..."
(
  cd "${ROOT_DIR}"
  CGO_ENABLED=0 go build -trimpath \
    -gcflags="all=-l" \
    -gcflags="github.com/Willh92/Volo-go/...=" \
    -ldflags="-s -w -X main.Version=v1.0.0-verify" \
    -o "${BIN_PATH}" ./cmd/volo
)

BIN_BYTES="$(wc -c < "${BIN_PATH}" | tr -d ' ')"
BIN_MB="$(awk -v b="${BIN_BYTES}" 'BEGIN { printf "%.2f", b / (1024 * 1024) }')"
echo "      Binary size: ${BIN_BYTES} bytes (${BIN_MB} MiB) [budget < 20.00 MiB]"
if (( BIN_BYTES >= MAX_BIN_BYTES )); then
  echo "FAIL: binary size ${BIN_BYTES} exceeds 20 MiB budget (${MAX_BIN_BYTES})" >&2
  exit 1
fi

echo "[2/4] Verifying static linkage..."
if command -v ldd >/dev/null 2>&1; then
  LDD_OUT="$(ldd "${BIN_PATH}" 2>&1 || true)"
  echo "      ldd: ${LDD_OUT}"
  if ! grep -Eq "not a dynamic executable|statically linked" <<<"${LDD_OUT}"; then
    echo "FAIL: binary is dynamically linked: ${LDD_OUT}" >&2
    exit 1
  fi
fi

echo "[3/4] Booting isolated Volo-go server & probing /healthz..."
"${BIN_PATH}" serve \
  --listen-addr "127.0.0.1:${PORT}" \
  --data-dir "${WORK_DIR}/data" \
  --upload-dir "${WORK_DIR}/data/uploads" \
  --backup-dir "${WORK_DIR}/data/backups" \
  --geoip-dir "${WORK_DIR}/data/geoip" \
  --plugins-dir "${ROOT_DIR}/plugins" \
  --themes-dir "${ROOT_DIR}/themes" \
  >"${WORK_DIR}/server.log" 2>&1 &
SERVER_PID=$!

READY=0
for _ in $(seq 1 40); do
  if "${BIN_PATH}" healthcheck --url "http://127.0.0.1:${PORT}/healthz" >/dev/null 2>&1; then
    READY=1
    break
  fi
  sleep 0.1
done

if (( READY != 1 )); then
  echo "FAIL: server did not pass healthcheck within 4s. Logs:" >&2
  cat "${WORK_DIR}/server.log" >&2
  exit 1
fi

# Warm a public request and /healthz before sampling RSS
curl --noproxy '*' -fsS "http://127.0.0.1:${PORT}/healthz" >/dev/null
curl --noproxy '*' -fsS "http://127.0.0.1:${PORT}/" >/dev/null

if [[ -r "/proc/${SERVER_PID}/status" ]]; then
  RSS_KB="$(awk '/^VmRSS:/ { print $2 }' "/proc/${SERVER_PID}/status")"
else
  RSS_KB="$(ps -o rss= -p "${SERVER_PID}" | tr -d ' ')"
fi
RSS_MB="$(awk -v kb="${RSS_KB}" 'BEGIN { printf "%.2f", kb / 1024 }')"
echo "      Live idle VmRSS: ${RSS_KB} KiB (${RSS_MB} MiB) [budget < 35.00 MiB]"
if (( RSS_KB >= MAX_RSS_KB )); then
  echo "FAIL: idle VmRSS ${RSS_KB} KiB exceeds 35 MiB budget (${MAX_RSS_KB} KiB)" >&2
  exit 1
fi

echo "[4/4] Verifying graceful SIGTERM shutdown..."
kill -TERM "${SERVER_PID}"
wait "${SERVER_PID}"
EXIT_CODE=$?
SERVER_PID=""
if (( EXIT_CODE != 0 )); then
  echo "FAIL: server exited with non-zero code ${EXIT_CODE} on SIGTERM" >&2
  exit 1
fi

echo "PASS: Binary=${BIN_MB} MiB (< 20 MiB), Idle RSS=${RSS_MB} MiB (< 35 MiB), Static=OK, GracefulShutdown=OK"
