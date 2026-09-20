#!/usr/bin/env bash
# monitor-control.sh — convenient runner for monitor-control.py
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

# Machine-local overrides (gitignored): the deployment host and the key-file
# path are per-machine facts, so they live in scripts/monitor-control.local.env
# rather than in this script. Real environment variables always win.
LOCAL_ENV="$SCRIPT_DIR/monitor-control.local.env"
if [ -f "$LOCAL_ENV" ]; then
  set -a
  # shellcheck disable=SC1090
  . "$LOCAL_ENV"
  set +a
fi

MONITOR_URL="${MONITOR_URL:-http://127.0.0.1:3457}"
MONITOR_KEY_FILE="${MONITOR_KEY_FILE:-api-keys.local}"

PYTHON_BIN="python3"
if ! command -v python3 >/dev/null 2>&1; then
  if command -v python >/dev/null 2>&1; then
    PYTHON_BIN="python"
  else
    echo "ERROR: python3 or python required" >&2
    exit 1
  fi
fi

exec "$PYTHON_BIN" "$SCRIPT_DIR/monitor-control.py" \
  --url "$MONITOR_URL" \
  --key-file "$MONITOR_KEY_FILE" \
  "$@"
