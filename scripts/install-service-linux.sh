#!/usr/bin/env sh
# Compatibility wrapper forwarding to unified cross-platform install-service.sh
SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")" 2>/dev/null && pwd || pwd)"
exec sh "${SCRIPT_DIR}/install-service.sh" --target-os linux "$@"
